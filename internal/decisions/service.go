package decisions

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/store"
)

// Status of a decision.
const (
	StatusOK      = "ok"
	StatusInvalid = "invalid"
	StatusError   = "error"
	StatusTimeout = "timeout"
	StatusOff     = "off"
)

// Feedback values.
const (
	FeedbackAgree    = "agree"
	FeedbackDisagree = "disagree"
)

const (
	idPrefix = "dec_"
	// KeepFor is how long a decision with no feedback stays.
	KeepFor = 180 * 24 * time.Hour
)

var (
	ErrNotFound    = errors.New("decision not found")
	ErrUnknownKind = errors.New("unknown decision kind")
)

// Blobs keeps the raw decider output.
type Blobs interface {
	Put(r io.Reader) (string, int64, error)
	Open(sha string) (io.ReadCloser, int64, error)
}

// Availability is an optional Decider method: false makes auto skip it.
type Availability interface {
	Available() bool
}

// Decision is one stored decision.
type Decision struct {
	ID             string              `json:"id"`
	Workspace      string              `json:"workspace"`
	Kind           string              `json:"kind"`
	KindVersion    int                 `json:"kind_version"`
	SubjectKind    string              `json:"subject_kind"`
	SubjectID      string              `json:"subject_id"`
	Scope          string              `json:"scope,omitempty"`
	Facts          map[string]string   `json:"facts,omitempty"`
	TraceID        string              `json:"trace_id,omitempty"`
	Decider        string              `json:"decider"`
	Method         Method              `json:"method"`
	Status         string              `json:"status"`
	Outcome        Outcome             `json:"outcome"`
	Confidence     *float64            `json:"confidence,omitempty"`
	Questions      []Question          `json:"questions"`
	Answers        []Answer            `json:"answers"`
	Reasons        []string            `json:"reasons"`
	RawSHA256      string              `json:"raw_sha256,omitempty"`
	Error          string              `json:"error,omitempty"`
	Attempts       int                 `json:"attempts"`
	TookMS         int64               `json:"took_ms"`
	CostUSD        float64             `json:"cost_usd"`
	Tokens         pricing.TokenTotals `json:"tokens"`
	Feedback       string              `json:"feedback,omitempty"`
	FeedbackSource string              `json:"feedback_source,omitempty"`
	FeedbackAt     *time.Time          `json:"feedback_at,omitempty"`
	CreatedAt      time.Time           `json:"created_at"`
}

// Held reports whether the outcome is the safe outcome of its policy.
func (d Decision) Held(p Policy) bool { return d.Outcome == p.Safe }

// Service runs and stores decisions.
type Service struct {
	st       *store.Store
	blobs    Blobs
	cfg      func() config.DecisionsConfig
	mu       sync.RWMutex
	deciders map[string]Decider
	policies map[string]Policy
	now      func() time.Time
	log      *slog.Logger
}

// NewService builds the service. cfg is read on each decision, so a config
// change applies with no restart.
func NewService(st *store.Store, blobs Blobs, cfg func() config.DecisionsConfig) *Service {
	if cfg == nil {
		cfg = func() config.DecisionsConfig { return config.DecisionsConfig{} }
	}
	return &Service{st: st, blobs: blobs, cfg: cfg, deciders: map[string]Decider{}, policies: map[string]Policy{},
		now: func() time.Time { return time.Now().UTC() }, log: slog.Default().With("source", "decisions")}
}

// Register adds a policy. It panics on an invalid question, because the
// policies are fixed code.
func (s *Service) Register(p Policy) {
	if p.Build != nil && len(p.Questions) > 0 {
		panic(fmt.Sprintf("policy %s: set Build or Questions, not both", p.Kind))
	}
	for _, q := range p.Questions {
		if err := q.Validate(); err != nil {
			panic(fmt.Sprintf("policy %s: %v", p.Kind, err))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[p.Kind] = p
}

// AddDecider adds a decider under its kind: "harness", "native", or "fake".
func (s *Service) AddDecider(kind string, d Decider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deciders[kind] = d
}

// Policy returns a registered policy.
func (s *Service) Policy(kind string) (Policy, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.policies[kind]
	return p, ok
}

// Decide runs the policy kind on state and stores the decision.
func (s *Service) Decide(ctx context.Context, workspace, kind string, subj Subject, state State) (Decision, error) {
	d, raw, err := s.run(ctx, Request{Workspace: workspace, Kind: kind, Subject: subj, State: state})
	if err != nil {
		return Decision{}, err
	}
	if len(raw) > 0 && s.blobs != nil {
		if sha, _, err := s.blobs.Put(bytes.NewReader(raw)); err == nil {
			d.RawSHA256 = sha
		}
	}
	return d, s.insert(ctx, d)
}

// Test runs the policy kind on state with no row and no event.
func (s *Service) Test(ctx context.Context, kind string, state State) (Decision, error) {
	d, _, err := s.run(ctx, Request{Kind: kind, Subject: Subject{Kind: "test"}, State: state})
	d.ID = ""
	return d, err
}

// decideChecked calls dec and returns a checkErr when the answers are not valid.
func decideChecked(ctx context.Context, dec Decider, r Request) (Result, error) {
	res, err := dec.Decide(ctx, r)
	if err != nil {
		return res, err
	}
	if err := checkAnswers(r.Questions, res.Answers); err != nil {
		return res, checkErr{err}
	}
	return res, nil
}

// checkErr marks a reply that is not valid JSON or fails Question.Check.
type checkErr struct{ error }

func isCheckErr(err error) bool {
	var c checkErr
	return errors.As(err, &c)
}

// vote merges the answers of several calls into a distribution.
func vote(qs []Question, results []Result) []Answer {
	out := make([]Answer, 0, len(qs))
	n := float64(len(results))
	for _, q := range qs {
		dist := map[string]float64{}
		for _, res := range results {
			for _, a := range res.Answers {
				switch {
				case a.ID != q.ID:
				case q.Shares:
					for k, p := range a.Distribution {
						dist[k] += p / n
					}
				default:
					dist[a.Value] += 1 / n
				}
			}
		}
		keys := slices.Sorted(maps.Keys(dist))
		sort.SliceStable(keys, func(i, j int) bool { return dist[keys[i]] > dist[keys[j]] })
		a := Answer{ID: q.ID, Value: keys[0], Distribution: dist, Confidence: dist[keys[0]], Method: MethodVote}
		if len(keys) > 1 {
			a.Margin = dist[keys[0]] - dist[keys[1]]
		} else {
			a.Margin = dist[keys[0]]
		}
		out = append(out, a)
	}
	return out
}

func ruleAnswers(p Policy, st State) map[string]Answer {
	out := map[string]Answer{}
	for _, r := range p.Rules {
		if v, hit := r.Check(st); hit {
			out[r.Question] = Answer{ID: r.Question, Value: v, Confidence: 1, Method: MethodRule, Reason: "rule"}
		}
	}
	return out
}

func rulesOnly(p Policy, rules map[string]Answer) []Answer {
	out := []Answer{}
	for _, q := range p.Questions {
		if a, ok := rules[q.ID]; ok {
			out = append(out, a)
		}
	}
	return out
}

// merge puts the rule answers over the model answers, applies hold_below,
// and sets the level of a score.
func merge(p Policy, rules map[string]Answer, model []Answer, holdBelow float64) []Answer {
	by := map[string]Answer{}
	for _, a := range model {
		by[a.ID] = a
	}
	maps.Copy(by, rules)
	out := make([]Answer, 0, len(p.Questions))
	for _, q := range p.Questions {
		a, ok := by[q.ID]
		if !ok {
			continue
		}
		if a.Method == "" {
			a.Method = MethodVerbal
		}
		if a.Method != MethodRule && holdBelow > 0 && a.Confidence < holdBelow && q.unsafe() != "" {
			a.Value = q.unsafe()
		}
		if q.Type == TypeScore {
			a.Level = slices.IndexFunc(q.Options, func(o Option) bool { return o.Key == a.Value })
		}
		out = append(out, a)
	}
	return out
}

func minConfidence(as []Answer) *float64 {
	if len(as) == 0 {
		return nil
	}
	m := 1.0
	for _, a := range as {
		m = min(m, a.Confidence)
	}
	return &m
}

type storedAnswers struct {
	Answers []Answer `json:"answers"`
	Reasons []string `json:"reasons"`
}

const decisionCols = `id, workspace_id, kind, kind_version, subject_kind, subject_id, trace_id, decider, method, status,
  outcome, confidence, questions_json, answers_json, raw_sha256, error, attempts, took_ms, cost_usd, tokens_input, tokens_output,
  tokens_cache_read, tokens_cache_creation, feedback, feedback_source, feedback_at, created_at,
  COALESCE(scope,''), COALESCE(facts_json,'')`

func scanDecision(sc interface{ Scan(...any) error }) (Decision, error) {
	var d Decision
	var method, outcome, qs, as, fbAt, created, facts string
	var conf sql.NullFloat64
	err := sc.Scan(&d.ID, &d.Workspace, &d.Kind, &d.KindVersion, &d.SubjectKind, &d.SubjectID, &d.TraceID, &d.Decider,
		&method, &d.Status, &outcome, &conf, &qs, &as, &d.RawSHA256, &d.Error, &d.Attempts, &d.TookMS, &d.CostUSD,
		&d.Tokens.Input, &d.Tokens.Output, &d.Tokens.CacheRead, &d.Tokens.CacheCreation, &d.Feedback, &d.FeedbackSource, &fbAt, &created, &d.Scope, &facts)
	if err != nil {
		return d, err
	}
	d.Method, d.Outcome = Method(method), Outcome(outcome)
	d.Tokens.Billed = d.Tokens.Input + d.Tokens.Output + d.Tokens.CacheCreation
	d.Tokens.Cached = d.Tokens.CacheRead
	if conf.Valid {
		d.Confidence = &conf.Float64
	}
	_ = json.Unmarshal([]byte(qs), &d.Questions) // the service writes this JSON
	var sa storedAnswers
	_ = json.Unmarshal([]byte(as), &sa)
	d.Answers, d.Reasons = sa.Answers, sa.Reasons
	if facts != "" {
		_ = json.Unmarshal([]byte(facts), &d.Facts) // the service writes this JSON
	}
	if t, err := time.Parse(time.RFC3339Nano, fbAt); err == nil {
		d.FeedbackAt = &t
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return d, nil
}

// Get reads one decision.
func (s *Service) Get(ctx context.Context, id string) (Decision, error) {
	d, err := scanDecision(s.st.DB().QueryRowContext(ctx, `SELECT `+decisionCols+` FROM decisions WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Decision{}, ErrNotFound
	}
	return d, err
}

// ListFilter selects decisions. After is the id of the last row of the
// prior page.
type ListFilter struct {
	Workspace   string
	Kind        string
	Outcome     string
	Status      string
	SubjectKind string
	SubjectID   string
	Scope       string
	// Feedback is agree, disagree, or none (no feedback yet).
	Feedback string
	// IDs keeps only these decisions. An empty slice that is not nil keeps none.
	IDs   []string
	After string
	Limit int
}

// List returns decisions newest first and the id for the next page.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Decision, string, error) {
	q := `SELECT ` + decisionCols + ` FROM decisions WHERE 1=1`
	var args []any
	for col, v := range map[string]string{"workspace_id": f.Workspace, "kind": f.Kind, "outcome": f.Outcome, "status": f.Status,
		"subject_kind": f.SubjectKind, "subject_id": f.SubjectID, "scope": f.Scope} {
		if v != "" {
			q += " AND " + col + "=?"
			args = append(args, v)
		}
	}
	switch f.Feedback {
	case "":
	case "none":
		q += " AND feedback=''"
	default:
		q += " AND feedback=?"
		args = append(args, f.Feedback)
	}
	if f.IDs != nil {
		if len(f.IDs) == 0 {
			return []Decision{}, "", nil
		}
		q += " AND id IN (?" + strings.Repeat(",?", len(f.IDs)-1) + ")"
		for _, id := range f.IDs {
			args = append(args, id)
		}
	}
	if f.After != "" {
		q += " AND id<?"
		args = append(args, f.After)
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []Decision{}
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out, next = out[:limit], out[limit-1].ID
	}
	return out, next, nil
}

// Raw opens the raw decider output of a decision.
func (s *Service) Raw(ctx context.Context, id string) ([]byte, error) {
	d, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.RawSHA256 == "" || s.blobs == nil {
		return []byte{}, nil
	}
	rc, _, err := s.blobs.Open(d.RawSHA256)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Feedback records agree or disagree from source (user or an inbox action).
func (s *Service) Feedback(ctx context.Context, id, value, source string) (Decision, error) {
	if value != FeedbackAgree && value != FeedbackDisagree {
		return Decision{}, fmt.Errorf("feedback must be %s or %s", FeedbackAgree, FeedbackDisagree)
	}
	d, err := s.Get(ctx, id)
	if err != nil {
		return Decision{}, err
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE decisions SET feedback=?, feedback_source=?, feedback_at=? WHERE id=?`,
			value, source, s.now().Format(time.RFC3339Nano), id)
		return err
	}, store.BusEvent{Topic: "decision.made", Entity: "decision", EntityID: id, Action: "updated",
		Attrs: map[string]any{"workspace": d.Workspace, "kind": d.Kind, "outcome": string(d.Outcome), "status": d.Status,
			"subject_kind": d.SubjectKind, "subject_id": d.SubjectID, "feedback": value}})
	if err != nil {
		return Decision{}, err
	}
	return s.Get(ctx, id)
}

// KindInfo describes one registered policy.
type KindInfo struct {
	Kind      string     `json:"kind"`
	Version   int        `json:"version"`
	Label     string     `json:"label"`
	Subject   string     `json:"subject"`
	Safe      Outcome    `json:"safe_outcome"`
	Questions []Question `json:"questions"`
	Decider   string     `json:"decider"` // the effective decider
	Samples   int        `json:"samples"`
	HoldBelow float64    `json:"hold_below,omitempty"`
	// Dynamic is a policy that builds its questions for each call.
	Dynamic bool `json:"dynamic,omitempty"`
}

// Kinds lists the registered policies with the effective decider.
func (s *Service) Kinds() []KindInfo {
	cfg := s.cfg()
	s.mu.RLock()
	ps := slices.Collect(maps.Values(s.policies))
	s.mu.RUnlock()
	slices.SortFunc(ps, func(a, b Policy) int { return strings.Compare(a.Kind, b.Kind) })
	out := make([]KindInfo, 0, len(ps))
	for _, p := range ps {
		kc := cfg.Kinds[p.Kind]
		_, name := s.pick(cmp.Or(kc.Decider, cfg.Decider))
		qs := p.Questions
		if qs == nil {
			qs = []Question{}
		}
		out = append(out, KindInfo{Kind: p.Kind, Version: p.Version, Label: p.Label, Subject: p.Subject, Safe: p.Safe,
			Questions: qs, Decider: name, Samples: cmp.Or(kc.Samples, cfg.Samples, 1), HoldBelow: kc.HoldBelow, Dynamic: p.Build != nil})
	}
	return out
}

// Stats is the summary of one kind in a time window.
type Stats struct {
	Kind      string         `json:"kind,omitempty"`
	Since     time.Time      `json:"since"`
	Total     int            `json:"total"`
	Outcomes  map[string]int `json:"outcomes"`
	Statuses  map[string]int `json:"statuses"`
	Feedback  int            `json:"feedback"`
	Disagree  int            `json:"disagree"`
	AgreeRate *float64       `json:"agree_rate,omitempty"`
	P50MS     int64          `json:"p50_ms"`
	P95MS     int64          `json:"p95_ms"`
	// CostUSD and Tokens add up the decider calls of the window.
	CostUSD float64             `json:"cost_usd"`
	Tokens  pricing.TokenTotals `json:"tokens"`
}

// StatsFilter selects the decisions that Stats counts. Empty fields count all.
type StatsFilter struct {
	Workspace, Kind, Scope string
	Since                  time.Time
}

// Stats counts the decisions that match f.
func (s *Service) Stats(ctx context.Context, f StatsFilter) (Stats, error) {
	st := Stats{Kind: f.Kind, Since: f.Since, Outcomes: map[string]int{}, Statuses: map[string]int{}}
	q := `SELECT outcome, status, feedback, took_ms, cost_usd, tokens_input, tokens_output, tokens_cache_read, tokens_cache_creation
		FROM decisions WHERE created_at >= ?`
	args := []any{f.Since.UTC().Format(time.RFC3339Nano)}
	for col, v := range map[string]string{"workspace_id": f.Workspace, "kind": f.Kind, "scope": f.Scope} {
		if v != "" {
			q += " AND " + col + "=?"
			args = append(args, v)
		}
	}
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	var took []int64
	agree := 0
	for rows.Next() {
		var outcome, status, fb string
		var ms int64
		var cost float64
		var tok pricing.TokenUsage
		if err := rows.Scan(&outcome, &status, &fb, &ms, &cost, &tok.Input, &tok.Output, &tok.CacheRead, &tok.CacheCreation); err != nil {
			return st, err
		}
		st.CostUSD += cost
		st.Tokens.Add(tok)
		st.Total++
		st.Outcomes[outcome]++
		st.Statuses[status]++
		took = append(took, ms)
		if fb != "" {
			st.Feedback++
			if fb == FeedbackAgree {
				agree++
			} else {
				st.Disagree++
			}
		}
	}
	if st.Feedback > 0 {
		r := float64(agree) / float64(st.Feedback)
		st.AgreeRate = &r
	}
	if len(took) > 0 {
		slices.Sort(took)
		st.P50MS, st.P95MS = took[len(took)*50/100], took[min(len(took)-1, len(took)*95/100)]
	}
	return st, rows.Err()
}

// Prune deletes the decisions older than KeepFor that have no feedback.
func (s *Service) Prune(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.st.DB().ExecContext(ctx, `DELETE FROM decisions WHERE feedback='' AND created_at < ?`,
		now.Add(-KeepFor).UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// run applies the decision order: rules, the decider with one repair, the
// hold threshold, then the map.
func (s *Service) run(ctx context.Context, r Request) (Decision, []byte, error) {
	p, ok := s.Policy(r.Kind)
	if !ok {
		return Decision{}, nil, fmt.Errorf("%w: %s", ErrUnknownKind, r.Kind)
	}
	start := time.Now()
	d := Decision{ID: idPrefix + strings.ToLower(ulid.Make().String()), Workspace: r.Workspace, Kind: p.Kind,
		KindVersion: p.Version, SubjectKind: cmp.Or(r.Subject.Kind, p.Subject), SubjectID: r.Subject.ID,
		Scope: r.Subject.Scope, Facts: r.State.Facts, TraceID: r.TraceID, Questions: p.Questions, Status: StatusOK, CreatedAt: s.now()}
	if p.Build != nil {
		qs, err := buildQuestions(p, r.State)
		if err != nil {
			d.Questions, d.Status, d.Error, d.Outcome = []Question{}, StatusInvalid, err.Error(), p.Safe
			d.Answers, d.Reasons = []Answer{}, []string{"The check did not finish (" + d.Status + ")."}
			return d, nil, nil
		}
		p.Questions, d.Questions = qs, qs
	}
	rules := ruleAnswers(p, r.State)
	cfg := s.cfg()
	kc := cfg.Kinds[p.Kind]
	var raw []byte
	var model []Answer
	if len(rules) < len(p.Questions) {
		dec, name := s.pick(cmp.Or(kc.Decider, cfg.Decider))
		d.Decider = name
		if dec == nil {
			d.Status = StatusOff
			if name != StatusOff {
				d.Status, d.Error = StatusError, "no decider is available: "+name
			}
		} else {
			r.Questions = p.Questions
			model, raw = s.callDecider(ctx, &d, dec, r, cmp.Or(kc.Samples, cfg.Samples, 1), cfg.TimeoutDuration())
		}
	} else {
		d.Decider, d.Method = "rules", MethodRule
	}
	d.TookMS = time.Since(start).Milliseconds()
	if d.Status != StatusOK {
		d.Outcome, d.Answers = p.Safe, rulesOnly(p, rules)
		d.Reasons = []string{"The check did not finish (" + d.Status + ")."}
		return d, raw, nil
	}
	d.Answers = merge(p, rules, normalizeShares(p.Questions, model), kc.HoldBelow)
	d.Confidence = minConfidence(d.Answers)
	by := Answers{}
	for _, a := range d.Answers {
		by[a.ID] = a
	}
	d.Outcome, d.Reasons = p.Map(by, r.State)
	if d.Reasons == nil {
		d.Reasons = []string{}
	}
	return d, raw, nil
}

// buildQuestions runs Build and validates the questions.
func buildQuestions(p Policy, st State) ([]Question, error) {
	qs, err := p.Build(st)
	if err != nil {
		return nil, err
	}
	if len(qs) == 0 {
		return nil, errors.New("the policy built no question")
	}
	for _, q := range qs {
		if err := q.Validate(); err != nil {
			return nil, err
		}
	}
	return qs, nil
}

// normalizeShares completes the answers of the shares questions.
func normalizeShares(qs []Question, as []Answer) []Answer {
	out := slices.Clone(as)
	for _, q := range qs {
		if !q.Shares {
			continue
		}
		for i, a := range out {
			if a.ID == q.ID {
				out[i] = q.Normalize(a)
			}
		}
	}
	return out
}

// pick resolves a decider setting. It returns nil and "off" for off.
func (s *Service) pick(setting string) (Decider, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch setting {
	case StatusOff:
		return nil, StatusOff
	case "", "auto":
		if n, ok := s.deciders["native"]; ok {
			if a, ok := n.(Availability); !ok || a.Available() {
				return n, n.Name()
			}
		}
		if h, ok := s.deciders["harness"]; ok {
			return h, h.Name()
		}
		return nil, "auto"
	}
	if d, ok := s.deciders[setting]; ok {
		return d, d.Name()
	}
	return nil, setting
}

// callDecider calls dec samples times in parallel and repairs an invalid
// reply once. It sets the method, attempts, spend, status, and error of d.
func (s *Service) callDecider(ctx context.Context, d *Decision, dec Decider, r Request, samples int, timeout time.Duration) (
	answers []Answer, raw []byte) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	results := make([]Result, samples)
	errs := make([]error, samples)
	calls := make([]int, samples)
	first := make([]Result, samples) // the first reply of a repaired sample; it costs too
	var wg sync.WaitGroup
	for i := range samples {
		wg.Add(1)
		go func() { // stops when Decide returns; ctx limits it to timeout
			defer wg.Done()
			results[i], errs[i] = decideChecked(ctx, dec, r)
			calls[i] = 1
			var c checkErr
			if errors.As(errs[i], &c) && ctx.Err() == nil {
				first[i] = results[i]
				rr := r
				rr.Repair = c.Error()
				results[i], errs[i] = decideChecked(ctx, dec, rr)
				calls[i] = 2
			}
		}()
	}
	wg.Wait()
	for _, c := range calls {
		d.Attempts += c
	}
	var good []Result
	var last error
	for i, res := range results {
		d.CostUSD += first[i].CostUSD + res.CostUSD
		d.Tokens.Add(first[i].Tokens)
		d.Tokens.Add(res.Tokens)
		raw = append(raw, res.Raw...)
		if errs[i] != nil {
			last = errs[i]
			continue
		}
		good = append(good, res)
	}
	switch {
	case len(good) == 0 && ctx.Err() != nil:
		d.Status, d.Error = StatusTimeout, "the decider took longer than "+timeout.String()
		return nil, raw
	case len(good) == 0 && isCheckErr(last):
		d.Status, d.Error = StatusInvalid, last.Error()
		return nil, raw
	case len(good) == 0:
		d.Status, d.Error = StatusError, last.Error()
		return nil, raw
	case len(good) == 1:
		d.Method = cmp.Or(good[0].Method, MethodVerbal)
		return good[0].Answers, raw
	}
	d.Method = MethodVote
	return vote(r.Questions, good), raw
}

func (s *Service) insert(ctx context.Context, d Decision) error {
	qs, _ := json.Marshal(d.Questions)
	as, _ := json.Marshal(storedAnswers{Answers: d.Answers, Reasons: d.Reasons})
	facts := ""
	if len(d.Facts) > 0 {
		b, _ := json.Marshal(d.Facts)
		facts = string(b)
	}
	ev := store.BusEvent{Topic: "decision.made", TraceID: d.TraceID, Entity: "decision", EntityID: d.ID, Action: "created",
		Attrs: map[string]any{"workspace": d.Workspace, "kind": d.Kind, "outcome": string(d.Outcome), "status": d.Status,
			"subject_kind": d.SubjectKind, "subject_id": d.SubjectID, "scope": d.Scope}}
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO decisions (id, workspace_id, kind, kind_version, subject_kind, subject_id,
  trace_id, decider, method, status, outcome, confidence, questions_json, answers_json, raw_sha256, error, attempts, took_ms,
  cost_usd, tokens_input, tokens_output, tokens_cache_read, tokens_cache_creation, created_at, scope, facts_json)
  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			d.ID, d.Workspace, d.Kind, d.KindVersion, d.SubjectKind, d.SubjectID, d.TraceID, d.Decider, string(d.Method),
			d.Status, string(d.Outcome), d.Confidence, string(qs), string(as), d.RawSHA256, d.Error, max(d.Attempts, 1),
			d.TookMS, d.CostUSD, d.Tokens.Input, d.Tokens.Output, d.Tokens.CacheRead, d.Tokens.CacheCreation,
			d.CreatedAt.Format(time.RFC3339Nano), store.NullStr(d.Scope), store.NullStr(facts))
		return err
	}, ev)
	if err != nil {
		return fmt.Errorf("insert decision: %w", err)
	}
	s.log.Info("decision.made", "id", d.ID, "kind", d.Kind, "outcome", d.Outcome, "status", d.Status, "took_ms", d.TookMS)
	return nil
}
