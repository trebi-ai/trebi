package requests

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
)

type Runs interface {
	Get(ctx context.Context, id string) (*runs.Run, error)
	Unpark(ctx context.Context, runID string, end runs.EndReason) error
	TurnAfter(ctx context.Context, runID string, reason runs.RunReason, since time.Time) (int, error)
}

type Launcher interface {
	Resume(ctx context.Context, prevID string, o runs.ResumeOpts) (*runs.Run, error)
	Fresh(ctx context.Context, prevID, prompt string, reason runs.RunReason) (*runs.Run, error)
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
	maxWait            = 60 * time.Second
)

var _ runs.RequestGate = (*Service)(nil)

type Service struct {
	st      *store.Store
	runs    Runs
	launch  Launcher
	grant   Grant
	reports *ReportStore
	opts    Options
	now     func() time.Time
	log     *slog.Logger
}

func NewService(st *store.Store, rs Runs, l Launcher, g Grant, reports *ReportStore, o Options) *Service {
	if o.ExpireAfter <= 0 {
		o.ExpireAfter = defaultExpireAfter
	}
	if o.Retain <= 0 {
		o.Retain = defaultRetain
	}
	return &Service{
		st: st, runs: rs, launch: l, grant: g, reports: reports, opts: o,
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

type CreateOpts struct {
	Type      Type          `json:"type"`
	Title     string        `json:"title"`
	Summary   string        `json:"summary,omitempty"`
	Target    string        `json:"target,omitempty"`
	Payload   Payload       `json:"-"`
	Allow     []Decision    `json:"allow,omitempty"`
	Report    string        `json:"report,omitempty"`
	Delivery  DeliveryMode  `json:"delivery,omitempty"`
	ExpiresIn time.Duration `json:"-"`
}

// Create opens a request for the run of actor. Only a run can ask, and only
// when the cloud plan grants requests.
func (s *Service) Create(ctx context.Context, actor tools.Actor, o CreateOpts) (Request, error) {
	if actor.RunID == "" {
		return Request{}, ErrNotRun
	}
	if !s.granted() {
		return Request{}, ErrPlan
	}
	run, err := s.runs.Get(ctx, actor.RunID)
	if err != nil {
		return Request{}, fmt.Errorf("run %s: %w", actor.RunID, err)
	}
	now := s.now()
	r := Request{
		ID: ulid.Make().String(), RunID: run.ID, WorkspaceID: run.Workspace, JobID: run.JobID, JobName: run.JobName,
		Kind: run.Kind, SessionID: run.SessionID, Harness: run.Harness, Runtime: run.Runtime,
		WorkingDir: run.WorkingDir, ConversationKey: run.ConversationKey, ConversationID: run.ConversationID,
		Type: o.Type, Title: strings.TrimSpace(o.Title), Summary: o.Summary, Target: o.Target,
		Payload: o.Payload, Allow: o.Allow, Delivery: o.Delivery,
		Status: StatusPending, DeliveryState: DeliveryNone,
		CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	if err := r.Validate(); err != nil {
		return Request{}, err
	}
	if o.ExpiresIn < 0 {
		return Request{}, invalid("expires_in", "must not be negative")
	}
	exp := o.ExpiresIn
	if exp == 0 {
		exp = s.opts.ExpireAfter
	}
	at := now.Add(exp)
	r.ExpiresAt = &at
	if o.Report != "" {
		if r.ReportKind, r.ReportSHA256, err = s.reports.Snapshot(ctx, r.ID, r.WorkingDir, o.Report); err != nil {
			return Request{}, err
		}
	}
	payload, _ := json.Marshal(r.Payload)
	allow, _ := json.Marshal(r.Allow)
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO requests (id, run_id, workspace_id, job_id, kind,
  session_id, harness, runtime, working_dir, conversation_key, conversation_id, type, title,
  summary, target, payload_json, allow_json, report_kind, report_sha256, delivery, status,
  expires_at, delivery_state, created_at, updated_at, revision)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.RunID, r.WorkspaceID, r.JobID, r.Kind, r.SessionID, r.Harness, r.Runtime,
			r.WorkingDir, r.ConversationKey, r.ConversationID, string(r.Type), r.Title, r.Summary,
			r.Target, string(payload), string(allow), r.ReportKind, r.ReportSHA256,
			string(r.Delivery), string(r.Status), fmtTime(at), string(r.DeliveryState),
			fmtTime(now), fmtTime(now), r.Revision)
		return err
	}, r.event("request.created", "created"))
	if err != nil {
		if r.ReportKind != "" {
			_ = s.reports.Remove(r.ID) // best effort; the row is not there
		}
		return Request{}, err
	}
	return r, nil
}

// event is the thin bus frame of one request change.
func (r Request) event(topic, action string) store.BusEvent {
	return store.BusEvent{
		Topic: topic, Entity: "request", EntityID: r.ID, Action: action, Revision: int64(r.Revision),
		Attrs: map[string]string{
			"run_id": r.RunID, "job_id": r.JobID, "job_name": r.JobName, "workspace_id": r.WorkspaceID,
			"type": string(r.Type), "status": string(r.Status), "title": r.Title,
			"delivery_state": string(r.DeliveryState),
		},
	}
}

func (s *Service) Get(ctx context.Context, id string) (Request, error) {
	row := s.st.DB().QueryRowContext(ctx, `SELECT `+requestColumns+` FROM requests WHERE id=?`, id)
	r, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return r, err
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

// Answer closes a pending request with a person's decision. ifRevision 0
// skips the revision check. A closed request returns itself with ErrClosed.
func (s *Service) Answer(ctx context.Context, id string, a Answer, ifRevision int) (Request, error) {
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
	if err := a.ValidateFor(r); err != nil {
		return Request{}, err
	}
	a.At = s.now()
	if r, err = s.close(ctx, r, StatusAnswered, &a); err != nil {
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
	if r, err = s.close(ctx, r, StatusDismissed, nil); err != nil {
		return r, err
	}
	s.log.Info("request.dismissed", "request_id", r.ID, "by", by)
	s.afterClose(ctx, r)
	return r, nil
}

// Wait returns once the request is not pending, or after d (at most 60s).
func (s *Service) Wait(ctx context.Context, id string, d time.Duration) (Request, error) {
	d = min(d, maxWait)
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		r, err := s.Get(ctx, id)
		if err != nil || !r.Open() {
			return r, err
		}
		select {
		case <-ctx.Done():
			return r, nil
		case <-deadline.C:
			return r, nil
		case <-tick.C:
		}
	}
}

// PromptHint is the launch prompt text that tells a run how to ask. It is
// empty when the cloud plan does not grant requests.
func (s *Service) PromptHint() string {
	if !s.granted() {
		return ""
	}
	return launchHint
}

// HasOpen reports whether the session of runID must wait: a resume request
// is pending, or closed with an answer that is not delivered yet.
func (s *Service) HasOpen(ctx context.Context, runID string) (bool, error) {
	var n int
	err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE run_id=? AND delivery=?
  AND (status=? OR (status IN (?,?) AND delivery_state IN (?,?)))`,
		runID, string(DeliveryResume), string(StatusPending), string(StatusAnswered), string(StatusExpired),
		string(DeliveryNone), string(DeliveryClaimed)).Scan(&n)
	return n > 0, err
}

// RunEnded cancels the poll requests of the run and delivers the answers
// that waited for the session to end.
func (s *Service) RunEnded(ctx context.Context, runID string, _ runs.Status) {
	polls, err := s.query(ctx, `WHERE run_id=? AND delivery=? AND status=?`,
		runID, string(DeliveryPoll), string(StatusPending))
	if err != nil {
		s.log.Warn("request run ended", "run_id", runID, "err", err)
	}
	for _, r := range polls {
		if _, err := s.close(ctx, r, StatusCancelled, nil); err != nil && !errors.Is(err, ErrClosed) {
			s.log.Warn("cancel poll request", "request_id", r.ID, "err", err)
		}
	}
	if err := s.deliver(ctx, runID); err != nil {
		s.log.Warn("request deliver", "run_id", runID, "err", err)
	}
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
	return out, rows.Err()
}

// close moves a pending request to status with one compare-and-set.
func (s *Service) close(ctx context.Context, r Request, status Status, a *Answer) (Request, error) {
	var answerJSON any
	if a != nil {
		b, _ := json.Marshal(a)
		answerJSON = string(b)
	}
	now := s.now()
	next := r
	next.Status, next.Answer, next.UpdatedAt, next.Revision = status, a, now, r.Revision+1
	action := strings.TrimPrefix(topicFor(status), "request.")
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE requests SET status=?, answer_json=?, answered_at=?,
  updated_at=?, revision=revision+1 WHERE id=? AND status=? AND revision=?`,
			string(status), answerJSON, fmtTime(now), fmtTime(now), r.ID, string(StatusPending), r.Revision)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrClosed
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
	if r.Delivery != DeliveryResume {
		return
	}
	if err := s.deliver(ctx, r.RunID); err != nil {
		s.log.Warn("request deliver", "run_id", r.RunID, "err", err)
	}
	if r.Status != StatusDismissed {
		return
	}
	open, err := s.HasOpen(ctx, r.RunID)
	if err != nil || open {
		return
	}
	if err := s.runs.Unpark(ctx, r.RunID, runs.EndRequestDismissed); err != nil && !errors.Is(err, runs.ErrNotParked) {
		s.log.Warn("unpark", "run_id", r.RunID, "err", err)
	}
}
