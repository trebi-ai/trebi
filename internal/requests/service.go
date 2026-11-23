package requests

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
)

type Runs interface {
	Get(ctx context.Context, id string) (*runs.Run, error)
	Unpark(ctx context.Context, runID string, end runs.EndReason) error
	Deliver(ctx context.Context, runID string, extra ...runs.ParkDelivery) (runs.DeliverResult, error)
	AddEvent(ctx context.Context, runID, kind, source string, detail any)
}

// Grant answers the cloud plan grant. A Pro Cloud account grants requests.
type Grant interface{ Entitlement() entitle.State }

// Options are the config knobs of requests.
type Options struct {
	ExpireAfter time.Duration // requests.expire_after; 0 → 168h
	Retain      time.Duration // requests.retain; 0 → 720h
}

const (
	defaultExpireAfter = 168 * time.Hour
	defaultRetain      = 720 * time.Hour
)

var _ runs.ParkGate = (*Service)(nil)

type Service struct {
	st      *store.Store
	runs    Runs
	grant   Grant
	reports *ReportStore
	decider Decider
	opts    Options
	now     func() time.Time
	log     *slog.Logger
}

func NewService(st *store.Store, rs Runs, g Grant, reports *ReportStore, o Options) *Service {
	if o.ExpireAfter <= 0 {
		o.ExpireAfter = defaultExpireAfter
	}
	if o.Retain <= 0 {
		o.Retain = defaultRetain
	}
	return &Service{
		st: st, runs: rs, grant: g, reports: reports, opts: o,
		now: func() time.Time { return time.Now().UTC() },
		log: slog.Default().With("source", "requests"),
	}
}

// WithClock replaces the clock. Tests use it.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Reports returns the snapshot store.
func (s *Service) Reports() *ReportStore { return s.reports }

// CreateOpts opens a request, or adds its forms to the open request of the
// run. Title, Summary, and ExpiresIn apply only to a new request.
type CreateOpts struct {
	Title     string
	Summary   string
	ExpiresIn time.Duration
	Forms     []FormOpts
	// Source, HopID, and TraceID mark a request that the daemon raises.
	Source  string
	HopID   string
	TraceID string
}

// FormOpts is one form of a create call. Report is a file in the working dir.
type FormOpts struct {
	Type    Type
	Title   string
	Summary string
	Target  string
	Payload Payload
	Allow   []Decision
	Report  string
}

// AnswerIn is the answer of a person to every form of one request.
type AnswerIn struct {
	Forms []FormAnswerIn
	By    string
	Via   string
}

// SourceChain marks a chain limit request.
const SourceChain = "chain"

// Decider gets the decision of a request that the daemon raised.
type Decider interface {
	RequestDecided(ctx context.Context, r Request) error
}

// WithDecider sets the receiver of chain request decisions.
func (s *Service) WithDecider(d Decider) { s.decider = d }

// Create opens a request for the run of actor. Only a run can ask, and only
// when the cloud plan grants requests.
func (s *Service) Create(ctx context.Context, actor tools.Actor, o CreateOpts) (Request, error) {
	if actor.RunID == "" {
		return Request{}, ErrNotRun
	}
	o.Source, o.HopID, o.TraceID = "", "", ""
	return s.create(ctx, actor.RunID, o)
}

// CreateSystem creates a request that the daemon raises for a run, with no
// run token. Source and HopID mark its origin.
func (s *Service) CreateSystem(ctx context.Context, runID string, o CreateOpts) (Request, error) {
	return s.create(ctx, runID, o)
}

// event is the thin bus frame of one request change.
func (r Request) event(topic, action string) store.BusEvent {
	return store.BusEvent{
		Topic: topic, Entity: "request", EntityID: r.ID, Action: action, Revision: int64(r.Revision),
		Attrs: map[string]string{
			"run_id": r.RunID, "job_id": r.JobID, "job_name": r.JobName, "workspace_id": r.WorkspaceID,
			"status": string(r.Status), "title": r.Title, "delivery_state": string(r.DeliveryState),
			"forms": strconv.Itoa(len(r.Forms)), "turn": strconv.Itoa(r.Turn),
		},
	}
}

func (s *Service) Get(ctx context.Context, id string) (Request, error) {
	out, err := s.query(ctx, `WHERE id=?`, id)
	if err != nil {
		return Request{}, err
	}
	if len(out) == 0 {
		return Request{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return out[0], nil
}

type ListOpts struct {
	Status    Status
	RunID     string
	JobID     string
	Workspace string
	After     string // the id cursor: rows older than this id
	Limit     int
}

// List returns requests newest first and the id cursor of the next page.
func (s *Service) List(ctx context.Context, o ListOpts) ([]Request, string, error) {
	limit := o.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	where := []string{"1=1"}
	var args []any
	for _, f := range []struct{ col, v string }{
		{"status", string(o.Status)}, {"run_id", o.RunID}, {"job_id", o.JobID}, {"workspace_id", o.Workspace},
	} {
		if f.v != "" {
			where = append(where, f.col+"=?")
			args = append(args, f.v)
		}
	}
	if o.After != "" {
		where = append(where, "id<?")
		args = append(args, o.After)
	}
	args = append(args, limit)
	out, err := s.query(ctx, `WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) == limit {
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

// Answer closes a pending request with one answer for each form. ifRevision
// 0 skips the revision check. A closed request returns itself with ErrClosed.
func (s *Service) Answer(ctx context.Context, id string, in AnswerIn, ifRevision int) (Request, error) {
	r, err := s.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if !r.Open() {
		return r, ErrClosed
	}
	if ifRevision > 0 && ifRevision != r.Revision {
		return r, ErrConflict
	}
	if err := r.ValidateAnswers(in.Forms); err != nil {
		return Request{}, err
	}
	if r, err = s.close(ctx, r, StatusAnswered, &Answered{By: in.By, Via: in.Via, At: s.now()}, in.Forms); err != nil {
		return r, err
	}
	s.afterClose(ctx, r)
	return r, nil
}

// Dismiss closes a request with no answer. The run does not continue.
func (s *Service) Dismiss(ctx context.Context, id, by string) (Request, error) {
	r, err := s.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if !r.Open() {
		return r, ErrClosed
	}
	if r, err = s.close(ctx, r, StatusDismissed, &Answered{By: by, At: s.now()}, nil); err != nil {
		return r, err
	}
	s.log.Info("request.dismissed", "request_id", r.ID, "by", by)
	s.afterClose(ctx, r)
	return r, nil
}

// Name is the park gate name.
func (s *Service) Name() string { return runs.GateRequest }

// PromptHint is the launch prompt text that tells a run how to ask. It is
// empty when the cloud plan does not grant requests.
func (s *Service) PromptHint(context.Context, runs.LaunchOpts) string {
	if !s.granted() {
		return ""
	}
	return launchHint
}

// Open reports a pending request (waiting) and a closed one with an answer
// that is not delivered yet (ready). A claimed answer goes to the turn
// in flight, so it is not ready.
func (s *Service) Open(ctx context.Context, runID string) (waiting, ready bool, err error) {
	var pending, closed int
	err = s.st.DB().QueryRowContext(ctx, `SELECT
  COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END),0),
  COALESCE(SUM(CASE WHEN status IN (?,?) AND delivery_state=? THEN 1 ELSE 0 END),0)
FROM requests WHERE run_id=?`,
		string(StatusPending), string(StatusAnswered), string(StatusExpired),
		string(DeliveryNone), runID).Scan(&pending, &closed)
	return pending > 0, closed > 0, err
}

// RunEnded implements runs.ParkGate. A request does not change when its run
// ends: the answer resumes the session.
func (*Service) RunEnded(context.Context, string, runs.Status) {}

func (s *Service) create(ctx context.Context, runID string, o CreateOpts) (Request, error) {
	if !s.granted() {
		return Request{}, ErrPlan
	}
	if len(o.Forms) == 0 {
		return Request{}, invalid("forms", "needs at least one form")
	}
	if o.ExpiresIn < 0 {
		return Request{}, invalid("expires_in", "must not be negative")
	}
	run, err := s.runs.Get(ctx, runID)
	if err != nil {
		return Request{}, fmt.Errorf("run %s: %w", runID, err)
	}
	for attempt := 1; ; attempt++ {
		r, err := s.createOnce(ctx, run, o)
		if errors.Is(err, errOpenChanged) && attempt < 3 {
			continue
		}
		return r, err
	}
}

// errOpenChanged reports that the open request of the run changed between
// the read and the write.
var errOpenChanged = errors.New("open request changed")

// createOnce adds the forms of o to the open request of the run and source,
// or opens a new request with them.
func (s *Service) createOnce(ctx context.Context, run *runs.Run, o CreateOpts) (Request, error) {
	now := s.now()
	r, joined, err := s.openOrNew(ctx, run, o, now)
	if err != nil {
		return Request{}, err
	}
	first := len(r.Forms)
	for i, fo := range o.Forms {
		r.Forms = append(r.Forms, Form{
			Idx: first + i, Type: fo.Type, Title: strings.TrimSpace(fo.Title), Summary: fo.Summary,
			Target: fo.Target, Payload: fo.Payload, Allow: fo.Allow,
		})
	}
	if err := r.Validate(); err != nil {
		return Request{}, err
	}
	added := r.Forms[first:]
	if err := s.snapshotReports(ctx, r, added, o.Forms); err != nil {
		return Request{}, err
	}
	topic, action := "request.created", "created"
	if joined {
		topic, action = "request.updated", "updated"
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		return r.write(ctx, tx, added, joined)
	}, r.event(topic, action))
	if err != nil {
		s.removeReports(r.ID, added)
		return Request{}, err
	}
	s.runEvent(ctx, r)
	return r, nil
}

// runEvent records the state of r in the run feed, so the session shows it.
func (s *Service) runEvent(ctx context.Context, r Request) {
	s.runs.AddEvent(ctx, r.RunID, runs.EventRequest, "trebi", agentcli.TurnRequest{
		ID: r.ID, Status: string(r.Status), Title: r.Title, Forms: len(r.Forms),
	})
}

// openOrNew returns the open request of the run and source with a bumped
// revision, or a new request.
func (s *Service) openOrNew(ctx context.Context, run *runs.Run, o CreateOpts, now time.Time) (Request, bool, error) {
	open, err := s.query(ctx, `WHERE run_id=? AND source=? AND status=?`, run.ID, o.Source, string(StatusPending))
	if err != nil {
		return Request{}, false, err
	}
	if len(open) > 0 {
		r := open[0]
		r.UpdatedAt = now
		r.Revision++
		return r, true, nil
	}
	exp := o.ExpiresIn
	if exp == 0 {
		exp = s.opts.ExpireAfter
	}
	at := now.Add(exp)
	r := Request{
		ID: ulid.Make().String(), RunID: run.ID, WorkspaceID: run.Workspace, JobID: run.JobID, JobName: run.JobName,
		Kind: run.Kind, SessionID: run.SessionID, Harness: run.Harness, Runtime: run.Runtime,
		WorkingDir: run.WorkingDir, ConversationKey: run.ConversationKey, ConversationID: run.ConversationID,
		Title: strings.TrimSpace(o.Title), Summary: o.Summary, Turn: run.TurnCount,
		Status: StatusPending, DeliveryState: DeliveryNone, ExpiresAt: &at,
		Source: o.Source, HopID: o.HopID, TraceID: o.TraceID,
		CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	if r.Title == "" {
		r.Title = strings.TrimSpace(o.Forms[0].Title)
	}
	return r, false, nil
}

// snapshotReports copies the report of each added form. On error it removes
// the copies that it made.
func (s *Service) snapshotReports(ctx context.Context, r Request, added []Form, opts []FormOpts) error {
	for i, fo := range opts {
		if fo.Report == "" {
			continue
		}
		var err error
		if added[i].ReportKind, added[i].ReportSHA256, err = s.reports.Snapshot(ctx, r.ID, added[i].Idx, r.WorkingDir, fo.Report); err != nil {
			s.removeReports(r.ID, added)
			return err
		}
	}
	return nil
}

func (s *Service) removeReports(id string, forms []Form) {
	for _, f := range forms {
		if f.ReportKind != "" {
			_ = s.reports.RemoveForm(id, f.Idx) // best effort; the row is not there
		}
	}
}

// write stores r and its added forms. It returns errOpenChanged when the open
// request of the run changed after the read.
func (r Request) write(ctx context.Context, tx *sql.Tx, added []Form, joined bool) error {
	var cur string
	err := tx.QueryRowContext(ctx, `SELECT id FROM requests WHERE run_id=? AND source=? AND status=?`,
		r.RunID, r.Source, string(StatusPending)).Scan(&cur)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if (joined && cur != r.ID) || (!joined && cur != "") {
		return errOpenChanged
	}
	if joined {
		res, err := tx.ExecContext(ctx, `UPDATE requests SET updated_at=?, revision=? WHERE id=? AND revision=?`,
			fmtTime(r.UpdatedAt), r.Revision, r.ID, r.Revision-1)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errOpenChanged
		}
	} else if err := r.insert(ctx, tx); err != nil {
		return err
	}
	for _, f := range added {
		if err := f.insert(ctx, tx, r.ID); err != nil {
			return err
		}
	}
	return nil
}

func (r Request) insert(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO requests (id, run_id, workspace_id, job_id, kind,
  session_id, harness, runtime, working_dir, conversation_key, conversation_id, title, summary, turn,
  status, expires_at, delivery_state, source, hop_id, trace_id, created_at, updated_at, revision)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.RunID, r.WorkspaceID, r.JobID, r.Kind, r.SessionID, r.Harness, r.Runtime,
		r.WorkingDir, r.ConversationKey, r.ConversationID, r.Title, r.Summary, r.Turn,
		string(r.Status), fmtTime(*r.ExpiresAt), string(r.DeliveryState),
		r.Source, r.HopID, r.TraceID, fmtTime(r.CreatedAt), fmtTime(r.UpdatedAt), r.Revision)
	return err
}

func (f Form) insert(ctx context.Context, tx *sql.Tx, requestID string) error {
	payload, _ := json.Marshal(f.Payload) // plain value types always encode
	allow, _ := json.Marshal(f.Allow)
	_, err := tx.ExecContext(ctx, `INSERT INTO request_forms (request_id, idx, type, title, summary, target,
  payload_json, allow_json, report_kind, report_sha256) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		requestID, f.Idx, string(f.Type), f.Title, f.Summary, f.Target, string(payload), string(allow),
		f.ReportKind, f.ReportSHA256)
	return err
}

func (s *Service) granted() bool {
	return s.grant != nil && s.grant.Entitlement().Has(entitle.Requests)
}

func (s *Service) query(ctx context.Context, tail string, args ...any) ([]Request, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+requestColumns+` FROM requests `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.loadForms(ctx, out)
}

// loadForms fills the forms of rs with one query.
func (s *Service) loadForms(ctx context.Context, rs []Request) error {
	if len(rs) == 0 {
		return nil
	}
	at := make(map[string]int, len(rs))
	args := make([]any, len(rs))
	for i, r := range rs {
		at[r.ID] = i
		args[i] = r.ID
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+formColumns+` FROM request_forms WHERE request_id IN (?`+
		strings.Repeat(",?", len(rs)-1)+`) ORDER BY request_id, idx`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		id, f, err := scanForm(rows)
		if err != nil {
			return err
		}
		i := at[id]
		rs[i].Forms = append(rs[i].Forms, f)
	}
	return rows.Err()
}

// close moves a pending request to status with one compare-and-set. It
// writes the form answers in the same transaction.
func (s *Service) close(ctx context.Context, r Request, status Status, by *Answered, answers []FormAnswerIn) (Request, error) {
	now := s.now()
	next := r
	next.Status, next.Answered, next.UpdatedAt, next.Revision = status, by, now, r.Revision+1
	next.Forms = slices.Clone(r.Forms)
	byIdx := map[int]FormAnswer{}
	for _, a := range answers {
		byIdx[a.Idx] = a.FormAnswer
	}
	for i := range next.Forms {
		if a, ok := byIdx[next.Forms[i].Idx]; ok {
			next.Forms[i].Answer = &a
		}
	}
	var byArg, viaArg any
	if by != nil {
		byArg, viaArg = by.By, by.Via
	}
	action := strings.TrimPrefix(topicFor(status), "request.")
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE requests SET status=?, answered_by=?, answered_via=?, answered_at=?,
  updated_at=?, revision=revision+1 WHERE id=? AND status=? AND revision=?`,
			string(status), byArg, viaArg, fmtTime(now), fmtTime(now), r.ID, string(StatusPending), r.Revision)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrClosed
		}
		for _, f := range next.Forms {
			if f.Answer == nil {
				continue
			}
			b, _ := json.Marshal(f.Answer) // plain value types always encode
			if _, err := tx.ExecContext(ctx, `UPDATE request_forms SET answer_json=? WHERE request_id=? AND idx=?`,
				string(b), r.ID, f.Idx); err != nil {
				return err
			}
		}
		return nil
	}, next.event(topicFor(status), action))
	if errors.Is(err, ErrClosed) {
		cur, gerr := s.Get(ctx, r.ID)
		if gerr != nil {
			return Request{}, gerr
		}
		if cur.Open() {
			return cur, ErrConflict
		}
		return cur, ErrClosed
	}
	if err != nil {
		return Request{}, err
	}
	s.runEvent(ctx, next)
	return next, nil
}

func topicFor(st Status) string {
	switch st {
	case StatusAnswered:
		return "request.answered"
	case StatusExpired:
		return "request.expired"
	case StatusDismissed:
		return "request.dismissed"
	case StatusCancelled:
		return "request.cancelled"
	}
	return "request.created"
}

// afterClose delivers what the run can get now. A dismiss that leaves the
// run with nothing to wait for completes the parked run.
func (s *Service) afterClose(ctx context.Context, r Request) {
	if r.Source == SourceChain && s.decider != nil && (r.Status == StatusAnswered || r.Status == StatusExpired) {
		if err := s.decider.RequestDecided(ctx, r); err != nil {
			s.log.Warn("chain request decided", "request_id", r.ID, "err", err)
		}
	}
	if _, err := s.runs.Deliver(ctx, r.RunID); err != nil {
		s.log.Warn("request deliver", "run_id", r.RunID, "err", err)
	}
	if r.Status != StatusDismissed {
		return
	}
	if err := s.runs.Unpark(ctx, r.RunID, runs.EndRequestDismissed); err != nil && !errors.Is(err, runs.ErrNotParked) {
		s.log.Warn("unpark", "run_id", r.RunID, "err", err)
	}
}
