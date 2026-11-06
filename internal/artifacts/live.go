package artifacts

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/email"
)

// Source is one fixed read-only data source of a live version (D6).
type Source struct {
	Name       string         `json:"name" yaml:"name"`
	Connection string         `json:"connection" yaml:"connection"`
	Operation  string         `json:"operation" yaml:"operation"`
	Input      map[string]any `json:"input,omitempty" yaml:"input"`
}

// Param is one value that the user can change on a live artifact.
type Param struct {
	Name    string            `json:"name" yaml:"name"`
	Label   string            `json:"label" yaml:"label"`
	Type    string            `json:"type" yaml:"type"`                           // enum | date | number
	Options []string          `json:"options,omitempty" yaml:"options,omitempty"` // enum
	Values  map[string]string `json:"values,omitempty" yaml:"values,omitempty"`   // enum option → source value; default the option
	Default string            `json:"default" yaml:"default"`
}

// Param types.
const (
	ParamEnum   = "enum"
	ParamDate   = "date"
	ParamNumber = "number"
)

// Live is the data part of a live version: the parameters and the sources.
type Live struct {
	Params  []Param  `json:"params,omitempty" yaml:"params"`
	Sources []Source `json:"sources" yaml:"sources"`
}

const maxSources = 8

var (
	sourceNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	paramRefRE   = regexp.MustCompile(`^\{param\.([a-z][a-z0-9_]{0,31})\}$`)
)

// ErrPendingApproval is a refresh of a live artifact with no approved version.
var ErrPendingApproval = errors.New("the sources of this artifact wait for the approval of the user")

// RateLimitError is a refresh that comes too soon after the last one.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("refresh again in %s", e.RetryAfter.Round(time.Second))
}

// CanonicalSources is the stable text of s, for the D7 comparison. The
// order of the sources and of the keys does not change it.
func CanonicalSources(s []Source) string {
	sorted := slices.Clone(s)
	slices.SortFunc(sorted, func(a, b Source) int { return strings.Compare(a.Name, b.Name) })
	b, _ := json.Marshal(sorted) // maps marshal with sorted keys
	return string(b)
}

func (l *Live) Check() []FieldError {
	var errs []FieldError
	add := func(field, format string, args ...any) {
		errs = append(errs, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
	}
	params := map[string]bool{}
	for i, p := range l.Params {
		f := fmt.Sprintf("params[%d]", i)
		if !sourceNameRE.MatchString(p.Name) || params[p.Name] {
			add(f+".name", "must match %s and be unique", sourceNameRE)
		}
		params[p.Name] = true
		errs = append(errs, p.check(f)...)
	}
	if len(l.Sources) == 0 || len(l.Sources) > maxSources {
		add("sources", "needs 1 to %d sources", maxSources)
	}
	names := map[string]bool{}
	for i, s := range l.Sources {
		f := fmt.Sprintf("sources[%d]", i)
		if !sourceNameRE.MatchString(s.Name) || names[s.Name] {
			add(f+".name", "must match %s and be unique", sourceNameRE)
		}
		names[s.Name] = true
		if strings.TrimSpace(s.Connection) == "" {
			add(f+".connection", "is required")
		}
		if strings.TrimSpace(s.Operation) == "" {
			add(f+".operation", "is required")
		}
		if msg := checkRefs(s.Input, params); msg != "" {
			add(f+".input", "%s", msg)
		}
	}
	return errs
}

func (p Param) check(f string) []FieldError {
	var errs []FieldError
	switch p.Type {
	case ParamEnum:
		if len(p.Options) == 0 {
			errs = append(errs, FieldError{Field: f + ".options", Message: "an enum needs at least one option"})
		}
		for k := range p.Values {
			if !slices.Contains(p.Options, k) {
				errs = append(errs, FieldError{Field: f + ".values", Message: fmt.Sprintf("%q is not an option", k)})
			}
		}
	case ParamDate, ParamNumber:
	default:
		errs = append(errs, FieldError{Field: f + ".type", Message: "must be enum, date, or number"})
	}
	if _, err := p.value(p.Default); err != nil {
		errs = append(errs, FieldError{Field: f + ".default", Message: err.Error()})
	}
	return errs
}

// checkRefs finds a {param.x} that is not a whole value or names no parameter.
func checkRefs(v any, params map[string]bool) string {
	switch t := v.(type) {
	case string:
		if m := paramRefRE.FindStringSubmatch(t); m != nil {
			if !params[m[1]] {
				return "unknown parameter " + m[1]
			}
			return ""
		}
		if strings.Contains(t, "{param.") {
			return "a parameter must be a whole value, such as \"{param.period}\""
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			if msg := checkRefs(t[k], params); msg != "" {
				return msg
			}
		}
	case []any:
		for _, x := range t {
			if msg := checkRefs(x, params); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// value checks one input of p and returns the text that goes to a source.
func (p Param) value(in string) (string, error) {
	switch p.Type {
	case ParamEnum:
		if !slices.Contains(p.Options, in) {
			return "", fmt.Errorf("%q is not one of %s", in, strings.Join(p.Options, ", "))
		}
		if v, ok := p.Values[in]; ok {
			return v, nil
		}
		return in, nil
	case ParamDate:
		if _, err := time.Parse(time.DateOnly, in); err != nil {
			return "", fmt.Errorf("%q is not a date such as 2026-10-10", in)
		}
	case ParamNumber:
		if _, err := strconv.ParseFloat(in, 64); err != nil {
			return "", fmt.Errorf("%q is not a number", in)
		}
	}
	return in, nil
}

// Inputs merges the inputs of the user over the defaults and checks them.
// The result maps each parameter to its source value.
func (l *Live) Inputs(in map[string]string) (given, values map[string]string, err error) {
	given, values = map[string]string{}, map[string]string{}
	for k := range in {
		if !slices.ContainsFunc(l.Params, func(p Param) bool { return p.Name == k }) {
			return nil, nil, invalid("inputs", "unknown parameter %s", k)
		}
	}
	for _, p := range l.Params {
		v := p.Default
		if x, ok := in[p.Name]; ok {
			v = x
		}
		sv, err := p.value(v)
		if err != nil {
			return nil, nil, invalid("inputs."+p.Name, "%v", err)
		}
		given[p.Name], values[p.Name] = v, sv
	}
	return given, values, nil
}

// canonical compares the sources and the parameters: a new enum value also
// changes what a source can read.
func (l *Live) canonical() string {
	if l == nil {
		return ""
	}
	p := slices.Clone(l.Params)
	slices.SortFunc(p, func(a, b Param) int { return strings.Compare(a.Name, b.Name) })
	b, _ := json.Marshal(p)
	return CanonicalSources(l.Sources) + string(b)
}

// Check validates the parameters and the sources. Each error names its field.

// bindInput returns a copy of input with each {param.x} replaced.
func bindInput(v any, values map[string]string) any {
	switch t := v.(type) {
	case string:
		if m := paramRefRE.FindStringSubmatch(t); m != nil {
			return values[m[1]]
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = bindInput(x, values)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = bindInput(x, values)
		}
		return out
	}
	return v
}

// Sources runs and lists connection operations.
type Sources interface {
	Call(ctx context.Context, c connectors.CallOpts) (connectors.CallResult, error)
	List(ctx context.Context, ws, ref string) ([]connectors.OperationView, error)
}

// WithSources wires the connection operations that a live artifact reads.
func (s *Service) WithSources(src Sources) *Service {
	s.src = src
	return s
}

// WithAlerts wires the inbox item of a version that waits for approval.
func (s *Service) WithAlerts(a Alerter) *Service {
	s.alerts = a
	return s
}

// checkSources checks that each source names a read-only operation.
func (s *Service) checkSources(ctx context.Context, ws string, l *Live) error {
	if errs := l.Check(); len(errs) > 0 {
		return &InvalidError{Fields: errs}
	}
	if s.src == nil {
		return nil
	}
	var errs []FieldError
	for i, src := range l.Sources {
		ops, err := s.src.List(ctx, ws, src.Connection)
		if err != nil {
			errs = append(errs, FieldError{Field: fmt.Sprintf("sources[%d].connection", i), Message: err.Error()})
			continue
		}
		at := slices.IndexFunc(ops, func(o connectors.OperationView) bool { return o.Name == src.Operation })
		switch {
		case at < 0:
			errs = append(errs, FieldError{Field: fmt.Sprintf("sources[%d].operation", i),
				Message: fmt.Sprintf("%s has no operation %s", src.Connection, src.Operation)})
		case !ops[at].ReadOnly:
			errs = append(errs, FieldError{Field: fmt.Sprintf("sources[%d].operation", i),
				Message: fmt.Sprintf("%s of %s is not read-only", src.Operation, src.Connection)})
		}
	}
	if len(errs) > 0 {
		return &InvalidError{Fields: errs}
	}
	return nil
}

// sourceLabels names each source as "connection label: operation label".
func (s *Service) sourceLabels(ctx context.Context, ws string, list []Source) []string {
	out := make([]string, 0, len(list))
	for _, src := range list {
		label := src.Connection + ": " + src.Operation
		if s.src != nil {
			if ops, err := s.src.List(ctx, ws, src.Connection); err == nil {
				if at := slices.IndexFunc(ops, func(o connectors.OperationView) bool { return o.Name == src.Operation }); at >= 0 {
					label = src.Connection + ": " + ops[at].Label
				}
			}
		}
		out = append(out, label)
	}
	return out
}

// changedSources lists the sources of next that are new or differ from prev.
func changedSources(prev, next *Live) []Source {
	old := map[string]string{}
	if prev != nil {
		for _, s := range prev.Sources {
			old[s.Name] = CanonicalSources([]Source{s})
		}
	}
	var out []Source
	for _, s := range next.Sources {
		if old[s.Name] != CanonicalSources([]Source{s}) {
			out = append(out, s)
		}
	}
	return out
}

// askApproval raises the inbox item of a pending version (E2).
func (s *Service) askApproval(ctx context.Context, a *Artifact, n int, prev, next *Live) {
	if s.alerts == nil {
		return
	}
	changed := changedSources(prev, next)
	if len(changed) == 0 {
		changed = next.Sources // only the parameters changed
	}
	body := fmt.Sprintf("Version %d of %s reads data that you did not allow before: %s.",
		n, a.Title, strings.Join(s.sourceLabels(ctx, a.Workspace, changed), ", "))
	opts := alerts.RaiseOpts{Kind: alerts.KindArtifactSources, Subject: approvalSubject(a.ID, n),
		Title: a.Title + " wants to read new data", Body: body, WorkspaceID: a.Workspace}
	if n > 1 {
		opts.Hint = fmt.Sprintf("Allow it, or keep version %d.", n-1)
	}
	if a.OwnerKind != OwnerAssistant {
		opts.JobID = a.OwnerID
	}
	if _, err := s.alerts.Raise(ctx, opts); err != nil {
		s.log.Warn("artifact approval item", "artifact_id", a.ID, "err", err)
	}
}

// liveSpec is the live spec of the next version: the given one, else the
// spec of the newest version of a live artifact.
func liveSpec(ctx context.Context, tx *sql.Tx, cur *Artifact, given *Live, kind Kind) (*Live, error) {
	spec := given
	if spec == nil && cur != nil && cur.Live {
		l, err := versionLive(ctx, tx, cur.ID, cur.LatestVersion)
		if err != nil {
			return nil, err
		}
		spec = l
	}
	if spec != nil && kind != KindMarkdown && kind != KindHTML {
		return nil, invalid("kind", "a live template must be markdown or html")
	}
	return spec, nil
}

// approve applies D7 to the next version of a. A save of the user, or a
// spec equal to the spec of the live version, is approved and becomes the
// live version. It returns the spec of the live version before the save.
func approve(ctx context.Context, tx *sql.Tx, a *Artifact, spec *Live, o Origin) (string, *Live, error) {
	if spec == nil {
		return ApprovalNone, nil, nil
	}
	a.Live = true
	var prev *Live
	if a.LiveVersion != nil {
		p, err := versionLive(ctx, tx, a.ID, *a.LiveVersion)
		if err != nil {
			return "", nil, err
		}
		prev = p
	}
	if o.Kind != OriginUser && (prev == nil || prev.canonical() != spec.canonical()) {
		return ApprovalPending, prev, nil
	}
	n := a.LatestVersion
	a.LiveVersion = &n
	return ApprovalApproved, prev, nil
}

func versionLive(ctx context.Context, tx *sql.Tx, id string, n int) (*Live, error) {
	var sources, params string
	err := tx.QueryRowContext(ctx, `SELECT sources_json, params_json FROM artifact_versions WHERE artifact_id=? AND n=?`, id, n).
		Scan(&sources, &params)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeLive(sources, params), nil
}

func encodeLive(l *Live) (sources, params string) {
	if l == nil {
		return "", ""
	}
	s, _ := json.Marshal(l.Sources)
	if len(l.Params) == 0 {
		return string(s), ""
	}
	p, _ := json.Marshal(l.Params)
	return string(s), string(p)
}

func decodeLive(sources, params string) *Live {
	if sources == "" {
		return nil
	}
	l := &Live{}
	_ = json.Unmarshal([]byte(sources), &l.Sources) // the service writes this JSON
	if params != "" {
		_ = json.Unmarshal([]byte(params), &l.Params)
	}
	return l
}

func approvalSubject(id string, n int) string { return fmt.Sprintf("artifact/%s/%d", id, n) }

// Approval states of a version.
const (
	ApprovalNone     = "none"
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
)

// Decide approves or denies the sources of version n. An approved version
// becomes the live version when it is newer than the current one.
func (s *Service) Decide(ctx context.Context, id string, n int, approve bool, by string) (Artifact, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return Artifact{}, err
	}
	v, err := s.Version(ctx, id, n)
	if err != nil {
		return Artifact{}, err
	}
	if v.Approval == ApprovalNone {
		return Artifact{}, invalid("version", "version %d has no data sources", n)
	}
	state := ApprovalDenied
	if approve {
		state = ApprovalApproved
	}
	live := a.LiveVersion
	if approve && (live == nil || n > *live) {
		live = &n
	}
	a.Revision++
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE artifact_versions SET approval=?, decided_by=?, decided_at=? WHERE artifact_id=? AND n=?`,
			state, by, fmtTime(s.now()), id, n); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE artifacts SET live_version=?, revision=? WHERE id=?`, nullInt(live), a.Revision, id)
		return err
	}, s.event("artifact.updated", "updated", &a, n))
	if err != nil {
		return Artifact{}, err
	}
	if s.alerts != nil {
		if err := s.alerts.ResolveSubject(ctx, approvalSubject(id, n), "decided"); err != nil {
			s.log.Warn("artifact approval resolve", "artifact_id", id, "err", err)
		}
	}
	return s.Get(ctx, id)
}

// Snapshot is the data of one refresh of a live artifact. It never changes.
type Snapshot struct {
	ID         string            `json:"id"`
	ArtifactID string            `json:"artifact_id"`
	VersionN   int               `json:"version"`
	DataSHA256 string            `json:"data_sha256,omitempty"`
	Inputs     map[string]string `json:"inputs,omitempty"`
	Reason     string            `json:"reason"` // refresh | schedule | send
	Actor      string            `json:"actor,omitempty"`
	Errors     map[string]string `json:"errors,omitempty"`
	TookMS     int64             `json:"took_ms"`
	Pinned     bool              `json:"pinned"`
	CreatedAt  time.Time         `json:"created_at"`
}

// Refresh reasons.
const (
	ReasonRefresh  = "refresh"
	ReasonSchedule = "schedule"
	ReasonSend     = "send"
)

// SourceData is the result of one source in a snapshot.
type SourceData struct {
	Rows  *connectors.Rows `json:"rows,omitempty"`
	JSON  json.RawMessage  `json:"json,omitempty"`
	Text  string           `json:"text,omitempty"`
	Error string           `json:"error,omitempty"`
}

// RefreshOpts is one refresh. Agent marks a refresh that an agent asked for.
type RefreshOpts struct {
	ID     string
	Inputs map[string]string
	Reason string
	Actor  string
	Agent  bool
}

const (
	refreshTimeout   = 60 * time.Second
	userRefreshGap   = 10 * time.Second
	agentRefreshGap  = 60 * time.Second
	snapshotIDPrefix = "snp_"
)

// refreshLimiter keeps the time of the last refresh of each artifact.
type refreshLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *refreshLimiter) allow(id string, gap time.Duration, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if t, ok := l.last[id]; ok && now.Sub(t) < gap {
		return &RateLimitError{RetryAfter: gap - now.Sub(t)}
	}
	l.last[id] = now
	return nil
}

// Refresh runs the sources of the live version with no LLM and stores the
// result as a snapshot (E3). A failed source keeps its error text; the data
// of the other sources stays.
func (s *Service) Refresh(ctx context.Context, o RefreshOpts) (Snapshot, error) {
	if s.src == nil {
		return Snapshot{}, errors.New("live artifacts are not wired")
	}
	a, err := s.Get(ctx, o.ID)
	if err != nil {
		return Snapshot{}, err
	}
	if !a.Live {
		return Snapshot{}, invalid("artifact", "%s is not a live artifact", a.Name)
	}
	if a.LiveVersion == nil {
		return Snapshot{}, ErrPendingApproval
	}
	v, err := s.Version(ctx, a.ID, *a.LiveVersion)
	if err != nil {
		return Snapshot{}, err
	}
	given, values, err := v.Live.Inputs(o.Inputs)
	if err != nil {
		return Snapshot{}, err
	}
	switch {
	case o.Reason == ReasonSchedule || o.Reason == ReasonSend:
	case o.Agent:
		err = s.limit.allow(a.ID, agentRefreshGap, s.now())
	default:
		err = s.limit.allow(a.ID, userRefreshGap, s.now())
	}
	if err != nil {
		return Snapshot{}, err
	}
	start := time.Now()
	data := s.runSources(ctx, a.Workspace, v.Live, values, o.Agent)
	snap := Snapshot{ID: snapshotIDPrefix + strings.ToLower(ulid.Make().String()), ArtifactID: a.ID, VersionN: v.N,
		Inputs: given, Reason: cmp.Or(o.Reason, ReasonRefresh), Actor: o.Actor, Errors: map[string]string{},
		TookMS: time.Since(start).Milliseconds(), CreatedAt: s.now()}
	ok := false
	for name, d := range data {
		if d.Error != "" {
			snap.Errors[name] = d.Error
		} else {
			ok = true
		}
	}
	if ok {
		raw, _ := json.Marshal(data)
		if snap.DataSHA256, _, err = s.blobs.Put(strings.NewReader(string(raw))); err != nil {
			return Snapshot{}, err
		}
	}
	return snap, s.insertSnapshot(ctx, &a, snap)
}

// runSources calls each source in parallel with one total time limit.
func (s *Service) runSources(ctx context.Context, ws string, l *Live, values map[string]string, agent bool) map[string]SourceData {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	out := make(map[string]SourceData, len(l.Sources))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, src := range l.Sources {
		wg.Add(1)
		go func() { // stops when the call returns; the context limits it to refreshTimeout
			defer wg.Done()
			input, _ := bindInput(src.Input, values).(map[string]any)
			res, err := s.src.Call(ctx, connectors.CallOpts{Workspace: ws, Connection: src.Connection, Operation: src.Operation,
				Input: input, Agent: agent, RequireReadOnly: true})
			d := SourceData{Rows: res.Rows, JSON: res.Output, Text: res.Text}
			if err != nil {
				d = SourceData{Error: err.Error()}
			}
			mu.Lock()
			out[src.Name] = d
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func (s *Service) insertSnapshot(ctx context.Context, a *Artifact, snap Snapshot) error {
	inputs, _ := json.Marshal(snap.Inputs)
	errs := ""
	if len(snap.Errors) > 0 {
		b, _ := json.Marshal(snap.Errors)
		errs = string(b)
	}
	ev := s.event("artifact.refreshed", "refreshed", a, snap.VersionN)
	attrs, _ := ev.Attrs.(map[string]any)
	attrs["snapshot_id"], attrs["reason"] = snap.ID, snap.Reason
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO artifact_snapshots (id, artifact_id, version_n, data_sha256, inputs_json, reason,
  actor, errors_json, took_ms, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			snap.ID, snap.ArtifactID, snap.VersionN, snap.DataSHA256, string(inputs), snap.Reason, snap.Actor, errs,
			snap.TookMS, fmtTime(snap.CreatedAt))
		return err
	}, ev)
	return err
}

const snapshotSelect = `SELECT id, artifact_id, version_n, data_sha256, inputs_json, reason, actor, errors_json, took_ms, pinned,
  created_at FROM artifact_snapshots`

func scanSnapshot(row scanner) (Snapshot, error) {
	var sn Snapshot
	var inputs, errs, created string
	if err := row.Scan(&sn.ID, &sn.ArtifactID, &sn.VersionN, &sn.DataSHA256, &inputs, &sn.Reason, &sn.Actor, &errs,
		&sn.TookMS, &sn.Pinned, &created); err != nil {
		return Snapshot{}, err
	}
	if inputs != "" {
		_ = json.Unmarshal([]byte(inputs), &sn.Inputs) // the service writes this JSON
	}
	if errs != "" {
		_ = json.Unmarshal([]byte(errs), &sn.Errors) // the service writes this JSON
	}
	sn.CreatedAt = parseTime(created)
	return sn, nil
}

// Snapshots lists the snapshots of id, newest first.
func (s *Service) Snapshots(ctx context.Context, id string, limit int) ([]Snapshot, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.st.DB().QueryContext(ctx, snapshotSelect+` WHERE artifact_id=? ORDER BY created_at DESC, id DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		sn, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// Snapshot returns one snapshot of id.
func (s *Service) Snapshot(ctx context.Context, id, snapshotID string) (Snapshot, error) {
	sn, err := scanSnapshot(s.st.DB().QueryRowContext(ctx, snapshotSelect+` WHERE id=? AND artifact_id=?`, snapshotID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	return sn, err
}

// LatestSnapshot returns the newest snapshot of id, or ErrNotFound.
func (s *Service) LatestSnapshot(ctx context.Context, id string) (Snapshot, error) {
	list, err := s.Snapshots(ctx, id, 1)
	if err != nil {
		return Snapshot{}, err
	}
	if len(list) == 0 {
		return Snapshot{}, ErrNotFound
	}
	return list[0], nil
}

// SnapshotData returns the data of a snapshot as source name → data.
func (s *Service) SnapshotData(ctx context.Context, sn Snapshot) (map[string]SourceData, error) {
	out := map[string]SourceData{}
	if sn.DataSHA256 != "" {
		rc, _, err := s.blobs.Open(sn.DataSHA256)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		if err := json.NewDecoder(rc).Decode(&out); err != nil {
			return nil, err
		}
	}
	for name, msg := range sn.Errors {
		out[name] = SourceData{Error: msg}
	}
	return out, nil
}

// Bound returns version n of a live artifact with the data of a snapshot:
// Markdown with the data bound, or HTML with window.trebi first. An empty
// snapshotID takes the newest snapshot; with none, the data is empty.
func (s *Service) Bound(ctx context.Context, id string, n int, snapshotID string) ([]byte, Version, *Snapshot, error) {
	data, _, v, err := s.Read(ctx, id, n)
	if err != nil {
		return nil, Version{}, nil, err
	}
	sn, err := s.snapshotFor(ctx, id, snapshotID)
	if err != nil {
		return nil, Version{}, nil, err
	}
	var src map[string]SourceData
	inputs := map[string]string{}
	if sn != nil {
		if src, err = s.SnapshotData(ctx, *sn); err != nil {
			return nil, Version{}, nil, err
		}
		inputs = sn.Inputs
	}
	if v.Live == nil {
		return data, v, sn, nil
	}
	if strings.HasPrefix(v.MIME, "text/html") {
		b, _ := json.Marshal(map[string]any{"data": src, "inputs": inputs})
		script := "<script>window.trebi=" + strings.ReplaceAll(string(b), "</", `<\/`) + ";</script>\n"
		return append([]byte(script), data...), v, sn, nil
	}
	return BindMarkdown(data, BindData(src, inputs)), v, sn, nil
}

// snapshotFor returns the snapshot of id, the newest one for "", or nil
// when the artifact has none.
func (s *Service) snapshotFor(ctx context.Context, id, snapshotID string) (*Snapshot, error) {
	if snapshotID != "" {
		sn, err := s.Snapshot(ctx, id, snapshotID)
		return &sn, err
	}
	sn, err := s.LatestSnapshot(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return &sn, err
}

// liveBody is the mail content of a live artifact: the bound Markdown, or
// for HTML a short summary with one status line per source.
func (s *Service) liveBody(ctx context.Context, b email.ArtifactBody, a Artifact, snapshotID string) (email.ArtifactBody, error) {
	sn, err := s.snapshotFor(ctx, a.ID, snapshotID)
	if err != nil {
		return email.ArtifactBody{}, err
	}
	if sn == nil {
		return email.ArtifactBody{}, invalid("artifact", "%s has no data yet; refresh it first", a.Name)
	}
	if a.Kind == KindMarkdown {
		data, _, _, err := s.Bound(ctx, a.ID, sn.VersionN, sn.ID)
		b.Content = data
		return b, err
	}
	src, err := s.SnapshotData(ctx, *sn)
	if err != nil {
		return email.ArtifactBody{}, err
	}
	var md strings.Builder
	fmt.Fprintf(&md, "%s has new data from %s.\n\n```status\n", a.Title, sn.CreatedAt.Format("Jan 2, 2006 15:04 UTC"))
	for _, name := range slices.Sorted(maps.Keys(src)) {
		d := src[name]
		switch {
		case d.Error != "":
			fmt.Fprintf(&md, "%s: error, %s\n", name, strings.ReplaceAll(d.Error, "\n", " "))
		case d.Rows != nil:
			fmt.Fprintf(&md, "%s: ok, %d rows\n", name, len(d.Rows.Values))
		default:
			fmt.Fprintf(&md, "%s: ok\n", name)
		}
	}
	md.WriteString("```\n")
	b.Kind, b.Content = string(KindMarkdown), []byte(md.String())
	return b, nil
}
