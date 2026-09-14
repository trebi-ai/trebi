// Package runs owns run CRUD, timeline, launch path, and session state.
package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/llm"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
)

// WakeOpts is the material one caller supplies for a composed wake prompt:
// the var-expanded operator body plus the trigger-side text.
type WakeOpts struct {
	Body        string // operator body; when empty the composer reads the file
	ExtraPrompt string
	Recap       string
	Message     string
	// Memory carries the launch memory seam to the composer. Nil = off.
	Memory *MemoryLaunch
}

// WakeScope names the operator one wake composes for.
type WakeScope struct {
	WorkspaceID string
	Kind        string // assistant | agent
	Name        string // agent index name (agents/<name>); ignored for assistant
}

// WakeComposer is the one seam from runs to workspace wake composition
// (Part F.3). *workspaces.Service satisfies it; nil falls back to the raw body.
type WakeComposer interface {
	ComposeWakePrompt(ctx context.Context, scope WakeScope, opts WakeOpts) (string, error)
}

// MemoryResolver resolves the MEMORY.md seam for a stored run at gate time
// (B22 §3.3). *workspaces.Service satisfies it; nil disables the gate. Runs
// cannot import workspaces, so the seam is injected like WakeComposer.
type MemoryResolver interface {
	MemoryLaunchFor(ctx context.Context, run *Run) *MemoryLaunch
}

// Service is the runs subsystem.
type Service struct {
	st  *store.Store
	log *slog.Logger
	fs  *files.Service // MEMORY.md seed/migrate writes; nil skips memory prep

	mu       sync.RWMutex
	sessions map[string]sessionSnap // run_id → state
	native   map[string]*nativeHandle
	life     context.Context
	ingest   func(ctx context.Context, runID, convID string, texts []string) error
	recap    func(ctx context.Context, convID, text string) error
	prov     llm.Provider // test seam; nil → llm.New from config
	dispatch tools.Dispatcher
	composer WakeComposer
	memRes   MemoryResolver
}

type nativeHandle struct {
	cancel context.CancelFunc
	inbox  chan string
}

type sessionSnap struct {
	State SessionState
	Since time.Time
}

var (
	// ErrInvalidDecision reports an approval decision other than allow|deny.
	ErrInvalidDecision = errors.New("decision must be allow or deny")
	// ErrApprovalRunMismatch reports an approval_id that belongs to another run.
	ErrApprovalRunMismatch = errors.New("approval belongs to a different run")
	// ErrNotRunning reports InjectMessage against a run that is not live.
	ErrNotRunning = errors.New("run is not running")
	// ErrSentinelInMessage reports a message carrying the run's own completion
	// sentinel. Only the agent may report that it finished.
	ErrSentinelInMessage = errors.New("message must not contain the run completion sentinel")
)

// New creates a runs service. A nil composer skips wake composition; a nil
// fs skips MEMORY.md launch prep.
func New(st *store.Store, composer WakeComposer, fs *files.Service) *Service {
	return &Service{
		st: st, log: slog.Default().With("source", "runs"), fs: fs,
		sessions: make(map[string]sessionSnap), native: make(map[string]*nativeHandle),
		life:     context.Background(),
		composer: composer,
	}
}

// SetMemoryResolver wires the gate-time memory seam. Both daemon and api hold
// the workspaces service at their runs.New call sites.
func (s *Service) SetMemoryResolver(r MemoryResolver) {
	s.memRes = r
}

// NativeHooks wires the in-process assistant loop.
type NativeHooks struct {
	Lifetime context.Context
	Ingest   func(ctx context.Context, runID, convID string, texts []string) error
	Recap    func(ctx context.Context, convID, text string) error
	Provider llm.Provider
	Dispatch tools.Dispatcher
}

// SetNativeHooks installs native-assistant dependencies. Nil fields are left as-is.
func (s *Service) SetNativeHooks(h NativeHooks) {
	if h.Lifetime != nil {
		s.life = h.Lifetime
	}
	if h.Ingest != nil {
		s.ingest = h.Ingest
	}
	if h.Recap != nil {
		s.recap = h.Recap
	}
	if h.Provider != nil {
		s.prov = h.Provider
	}
	if h.Dispatch != nil {
		s.dispatch = h.Dispatch
	}
}

// CreateOpts for launching a run.
type CreateOpts struct {
	WorkspaceID     string
	Job             string
	Kind            string
	Reason          RunReason
	Harness         string
	PromptPreview   string
	SessionID       string
	TmuxSession     string
	QueueID         string
	TraceID         string
	ConversationKey string
	ConversationID  string
	// WorkingDir is the directory the session launched in. Persisted because a
	// harness that mints its own session id finds its transcript by path.
	WorkingDir     string
	Runtime        string
	Trigger        *TriggerCapture
	Inputs         map[string]string
	MaxDurationMS  int64
	IdleTimeoutMS  int64
	KeepAliveMS    int64
	NudgeMS        int64
	ResumedFrom    string
	ResumeAttempts int
	// MemoryHash is the launch-time MEMORY.md hash; empty when memory is off.
	MemoryHash string
	// DryRun evaluates without persisting (when gate only).
	DryRun bool
}

// applyDefaults fills zero-valued launch fields.
func (o *CreateOpts) applyDefaults() {
	if o.Kind == "" {
		o.Kind = KindJob
	}
	if o.Reason == "" {
		o.Reason = ReasonManual
	}
	if o.Harness == "" {
		o.Harness = "claude"
	}
	if o.Runtime == "" {
		o.Runtime = "tui"
	}
	if o.TraceID == "" {
		o.TraceID = ulid.Make().String()
	}
	if o.NudgeMS == 0 {
		o.NudgeMS = -1
	}
}

// triggerJSON flattens the trigger capture into insert columns.
func (o *CreateOpts) triggerJSON() (eventID, connector, capture, vars *string) {
	if o.Trigger == nil {
		return nil, nil, nil, nil
	}
	if o.Trigger.EventID != "" {
		eventID = &o.Trigger.EventID
	}
	if o.Trigger.Connector != "" {
		connector = &o.Trigger.Connector
	}
	if o.Trigger.Event != nil {
		s := string(o.Trigger.Event)
		capture = &s
	}
	if o.Trigger.Vars != nil {
		b, _ := json.Marshal(o.Trigger.Vars)
		s := string(b)
		vars = &s
	}
	return eventID, connector, capture, vars
}

// Create inserts a running row + launch timeline + bus event.
func (s *Service) Create(ctx context.Context, opts CreateOpts) (*Run, error) {
	opts.applyDefaults()
	if opts.DryRun {
		return &Run{
			ID:        "dry-run",
			Workspace: opts.WorkspaceID,
			Job:       opts.Job,
			Kind:      opts.Kind,
			Reason:    opts.Reason,
			Status:    StatusRunning,
			Harness:   opts.Harness,
			TraceID:   opts.TraceID,
		}, nil
	}

	id := ulid.Make().String()
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	run := newRun(id, now, opts)

	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		return insertRunRow(tx, run, opts, nowStr)
	}, store.BusEvent{
		Topic:    "run.created",
		TraceID:  opts.TraceID,
		Entity:   "run",
		EntityID: id,
		Action:   "created",
		Revision: 1,
		Attrs: map[string]string{
			"job": opts.Job, "workspace": opts.WorkspaceID,
			"kind": opts.Kind, "conversation_key": opts.ConversationKey,
			"conversation_id": opts.ConversationID,
		},
	})
	if err != nil {
		return nil, err
	}

	s.SetSessionState(ctx, id, SessionRunning, "launch")
	return run, nil
}

// Get returns a run by id.
// MaxWait caps one long-poll. A client that needs longer calls again.
const MaxWait = 120 * time.Second

// waitPoll is the interval between status reads inside WaitFor.
const waitPoll = 250 * time.Millisecond

// WaitFor returns the run when it reaches a terminal status, or the current row
// when wait elapses. A run that is already terminal returns at once.
func (s *Service) WaitFor(ctx context.Context, id string, wait time.Duration) (*Run, error) {
	if wait > MaxWait {
		wait = MaxWait
	}
	deadline := time.Now().Add(wait)
	for {
		run, err := s.Get(ctx, id)
		if err != nil || run.Status.IsTerminal() || !time.Now().Before(deadline) {
			return run, err
		}
		select {
		case <-ctx.Done():
			return run, nil
		case <-time.After(min(waitPoll, time.Until(deadline))):
		}
	}
}

func (s *Service) Get(ctx context.Context, id string) (*Run, error) {
	row := s.st.DB().QueryRowContext(ctx, `
		SELECT id, workspace_id, job, kind, COALESCE(reason,''), status, COALESCE(end_reason,''),
			COALESCE(prompt_preview,''), COALESCE(session_id,''), COALESCE(harness,''),
			COALESCE(runtime,'tui'), COALESCE(tmux_session,''),
			COALESCE(tokens_input,0), COALESCE(tokens_output,0), COALESCE(tokens_cache_read,0),
			COALESCE(tokens_cache_creation,0), COALESCE(tokens_model,''),
			started_at, ended_at, max_duration_ms, idle_timeout_ms, keep_alive_ms, nudge_ms,
			COALESCE(resumed_from,''), resume_attempts, COALESCE(queue_id,''), trace_id, revision,
			COALESCE(conversation_key,''), COALESCE(conversation_id,''), COALESCE(working_dir,''),
			trigger_event_id, trigger_connector, trigger_capture_json, trigger_vars_json, inputs_json,
			COALESCE(memory_hash,'')
		FROM runs WHERE id = ?`, id)
	return s.scanRun(row)
}

// Approval is a decided permission prompt for a run.
type Approval struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	Prompt    string `json:"prompt"`
	Decision  string `json:"decision,omitempty"`
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at"`
	DecidedAt string `json:"decided_at,omitempty"`
}

const approvalCols = `SELECT id, run_id, status, COALESCE(question,''), COALESCE(decision,''),
	revision, created_at, COALESCE(decided_at,'')`

// ApprovalListOpts filters for ListApprovals.
type ApprovalListOpts struct {
	Limit  int
	Cursor string
	State  string // pending|decided; empty returns both
}

// ListOpts filters for List.
type ListOpts struct {
	Workspace string
	Job       string
	Status    string
	Kind      string
	// Harness filters runs.harness (claude, grok, cursor, …).
	Harness string
	// Model matches tokens_model (exact, case-insensitive).
	Model           string
	ConversationKey string
	// Conversation matches conversation_id.
	Conversation string
	EndReason    string
	// Q matches the prompt preview, the job, and the run id.
	Q string
	// Since and Until bound started_at; both are optional.
	Since  string
	Until  string
	Limit  int
	Cursor string
}

// pseudoStatus reports an agent-map status (idle|waiting) filtered in memory.
func (o ListOpts) pseudoStatus() bool {
	return o.Status == "idle" || o.Status == "waiting"
}

// query builds the SQL and args for List.
func (o ListOpts) query() (string, []any) {
	q := `SELECT id, workspace_id, job, kind, COALESCE(reason,''), status, COALESCE(end_reason,''),
		COALESCE(prompt_preview,''), COALESCE(session_id,''), COALESCE(harness,''),
		COALESCE(runtime,'tui'), COALESCE(tmux_session,''),
		COALESCE(tokens_input,0), COALESCE(tokens_output,0), COALESCE(tokens_cache_read,0),
		COALESCE(tokens_cache_creation,0), COALESCE(tokens_model,''),
		started_at, ended_at, max_duration_ms, idle_timeout_ms, keep_alive_ms, nudge_ms,
		COALESCE(resumed_from,''), resume_attempts, COALESCE(queue_id,''), trace_id, revision,
		COALESCE(conversation_key,''), COALESCE(conversation_id,''), COALESCE(working_dir,''),
		trigger_event_id, trigger_connector, trigger_capture_json, trigger_vars_json, inputs_json,
		COALESCE(memory_hash,'')
		FROM runs WHERE 1=1`
	var args []any
	if o.Workspace != "" {
		q += ` AND workspace_id = ?`
		args = append(args, o.Workspace)
	}
	if o.Job != "" {
		q += ` AND job = ?`
		args = append(args, o.Job)
	}
	if o.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, o.Kind)
	}
	if o.Harness != "" {
		q += ` AND LOWER(harness) = LOWER(?)`
		args = append(args, strings.TrimSpace(o.Harness))
	}
	if o.Model != "" {
		q += ` AND LOWER(COALESCE(tokens_model,'')) = LOWER(?)`
		args = append(args, strings.TrimSpace(o.Model))
	}
	if o.pseudoStatus() {
		// Filter in memory after fetch of running
		q += ` AND status = 'running'`
	} else if o.Status != "" {
		q += ` AND status = ?`
		args = append(args, o.Status)
	}
	if o.ConversationKey != "" {
		q += ` AND conversation_key = ?`
		args = append(args, o.ConversationKey)
	}
	if o.Conversation != "" {
		q += ` AND conversation_id = ?`
		args = append(args, o.Conversation)
	}
	if o.EndReason != "" {
		q += ` AND end_reason = ?`
		args = append(args, o.EndReason)
	}
	if o.Since != "" {
		q += ` AND started_at >= ?`
		args = append(args, o.Since)
	}
	if o.Until != "" {
		q += ` AND started_at <= ?`
		args = append(args, o.Until)
	}
	if o.Q != "" {
		like := "%" + strings.ToLower(strings.TrimSpace(o.Q)) + "%"
		q += ` AND (LOWER(COALESCE(prompt_preview,'')) LIKE ? OR LOWER(job) LIKE ? OR LOWER(id) LIKE ?)`
		args = append(args, like, like, like)
	}
	if o.Cursor != "" {
		q += ` AND id < ?`
		args = append(args, o.Cursor)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, o.Limit)
	return q, args
}

// List returns runs newest first.
func (s *Service) List(ctx context.Context, opts ListOpts) ([]*Run, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	if !opts.pseudoStatus() {
		return s.listPage(ctx, opts)
	}
	want := opts.Limit
	var out []*Run
	cursor := opts.Cursor
	for len(out) < want {
		pageOpts := opts
		pageOpts.Limit = want * 4
		pageOpts.Cursor = cursor
		page, err := s.listPage(ctx, pageOpts)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			if string(r.SessionState) == opts.Status {
				out = append(out, r)
				if len(out) == want {
					break
				}
			}
		}
		cursor = page[len(page)-1].ID
		if len(page) < pageOpts.Limit {
			break
		}
	}
	return out, nil
}

func (s *Service) listPage(ctx context.Context, opts ListOpts) ([]*Run, error) {
	q, args := opts.query()
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := s.scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FindLive returns the running run on a conversation lane, or sql.ErrNoRows.
func (s *Service) FindLive(ctx context.Context, workspaceID, kind, conversationKey string) (*Run, error) {
	list, err := s.List(ctx, ListOpts{
		Workspace:       workspaceID,
		Kind:            kind,
		Status:          string(StatusRunning),
		ConversationKey: conversationKey,
		Limit:           1,
	})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, sql.ErrNoRows
	}
	return list[0], nil
}

// ListForTurnWatch returns conversation-linked runs that are live or just ended.
func (s *Service) ListForTurnWatch(ctx context.Context, endedAfter time.Time) ([]*Run, error) {
	q := `SELECT id, COALESCE(session_id,''), COALESCE(harness,''), COALESCE(conversation_id,''),
		COALESCE(working_dir,''), started_at
		FROM runs WHERE COALESCE(conversation_id,'') != ''
		AND (status = ? OR (ended_at IS NOT NULL AND ended_at >= ?))
		ORDER BY id DESC LIMIT 200`
	rows, err := s.st.DB().QueryContext(ctx, q, string(StatusRunning), endedAfter.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r := &Run{}
		var started sql.NullString
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Harness, &r.ConversationID, &r.WorkingDir, &started); err != nil {
			return nil, err
		}
		if started.Valid {
			r.StartedAt = store.ParseTS(started.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OpenApproval records that a run is waiting on an answer. It is a no-op when
// the run already has a pending row, so a repeated hook does not stack rows.
func (s *Service) OpenApproval(ctx context.Context, runID, question string) (*Approval, error) {
	if existing, err := s.pendingApproval(ctx, runID); err != nil || existing != nil {
		return existing, err
	}
	id := ulid.Make().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	a := &Approval{ID: id, RunID: runID, Status: "pending", Prompt: question, Revision: 1, CreatedAt: now}
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO approvals (id, run_id, question, status, created_at, revision)
			VALUES (?,?,?,'pending',?,1)`, id, runID, store.NullStr(question), now)
		return err
	}, store.BusEvent{
		Topic: "approval.created", Entity: "approval", EntityID: id, Action: "created",
		Revision: 1,
		Attrs:    map[string]string{"run_id": runID},
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// DecideApproval records a allow|deny decision for runID in one transaction with
// the approval.decided bus frame. It answers the pending row when there is one,
// and otherwise inserts an already-decided row.
func (s *Service) DecideApproval(ctx context.Context, runID, approvalID, decision string) (*Approval, error) {
	if decision != "allow" && decision != "deny" {
		return nil, ErrInvalidDecision
	}
	if _, err := s.Get(ctx, runID); err != nil {
		return nil, err
	}
	a, err := s.approvalToDecide(ctx, runID, approvalID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	a.Status, a.Decision, a.DecidedAt = "decided", decision, now
	a.Revision++
	insert := a.CreatedAt == ""
	if insert {
		a.CreatedAt = now
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if insert {
			_, err := tx.Exec(`INSERT INTO approvals (id, run_id, question, status, decision, created_at, decided_at, revision)
				VALUES (?,?,'','decided',?,?,?,?)`, a.ID, runID, decision, now, now, a.Revision)
			return err
		}
		_, err := tx.Exec(`UPDATE approvals SET status='decided', decision=?, decided_at=?, revision=? WHERE id=?`,
			decision, now, a.Revision, a.ID)
		return err
	}, store.BusEvent{
		Topic: "approval.decided", Entity: "approval", EntityID: a.ID, Action: "updated",
		Revision: a.Revision,
		Attrs:    map[string]string{"run_id": runID, "decision": decision},
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// ListApprovals returns approval rows newest-id first.
func (s *Service) ListApprovals(ctx context.Context, opts ApprovalListOpts) ([]Approval, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	q := approvalCols + ` FROM approvals WHERE 1=1`
	var args []any
	if opts.State != "" {
		q += ` AND status=?`
		args = append(args, opts.State)
	}
	if opts.Cursor != "" {
		q += ` AND id < ?`
		args = append(args, opts.Cursor)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, opts.Limit)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.RunID, &a.Status, &a.Prompt, &a.Decision, &a.Revision, &a.CreatedAt, &a.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Terminate marks a run terminal with end reason and freezes tokens.
func (s *Service) Terminate(ctx context.Context, id string, status Status, end EndReason, tokens *pricing.TokenUsage) error {
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)

	var ti, to, tcr, tcc int64
	var modelName string
	if tokens != nil {
		tokens.ApplyRates()
		ti, to, tcr, tcc = tokens.Input, tokens.Output, tokens.CacheRead, tokens.CacheCreation
		modelName = tokens.Model
	}

	// Lock order everywhere: LockBus, then open the tx (see store.AppendEntity).
	s.st.LockBus()
	defer s.st.UnlockBus()

	var rev int64
	alreadyTerminal := false
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		var cur, jobName, wsID, harness string
		err := tx.QueryRow(`SELECT revision, status, job, workspace_id, COALESCE(harness,'') FROM runs WHERE id = ?`, id).
			Scan(&rev, &cur, &jobName, &wsID, &harness)
		if err != nil {
			return err
		}
		// Idempotent: a terminal run stays as its first terminal write left it.
		if Status(cur).IsTerminal() {
			alreadyTerminal = true
			return nil
		}
		rev++
		_, err = tx.Exec(`UPDATE runs SET status=?, end_reason=?, ended_at=?, tokens_input=?, tokens_output=?,
			tokens_cache_read=?, tokens_cache_creation=?, tokens_model=?, revision=?, updated_at=?
			WHERE id=?`, string(status), string(end), nowStr, ti, to, tcr, tcc, modelName, rev, nowStr, id)
		if err != nil {
			return err
		}
		// Close open task + agent segments (duration computed in Go — SQLite julianday mishandles RFC3339Nano).
		for _, layer := range []string{"run", "session"} {
			var segStart string
			_ = tx.QueryRow(`SELECT started_at FROM run_segments WHERE run_id=? AND layer=? AND ended_at IS NULL ORDER BY id DESC LIMIT 1`,
				id, layer).Scan(&segStart)
			var durMS int64
			if t0, err := time.Parse(time.RFC3339Nano, segStart); err == nil {
				durMS = now.Sub(t0).Milliseconds()
			}
			if _, err := tx.Exec(`UPDATE run_segments SET ended_at=?, duration_ms=?, reason=?
				WHERE run_id=? AND layer=? AND ended_at IS NULL`, nowStr, durMS, string(end), id, layer); err != nil {
				return err
			}
		}
		// Terminal segment.
		_, err = tx.Exec(`INSERT INTO run_segments (run_id, layer, state, started_at, ended_at, duration_ms, reason, source)
			VALUES (?, 'run', ?, ?, ?, 0, ?, 'reaper')`, id, string(status), nowStr, nowStr, string(end))
		if err != nil {
			return err
		}
		topic := "run.updated"
		if status == StatusFailed {
			topic = "run.failed"
		}
		_, err = store.AppendInTx(tx, store.BusEvent{
			Topic: topic, Entity: "run", EntityID: id, Action: "updated", Revision: rev,
			Attrs: map[string]string{
				"end_reason": string(end), "status": string(status),
				"job": jobName, "workspace": wsID, "harness": harness,
			},
		})
		return err
	})
	if err != nil {
		return err
	}
	if alreadyTerminal {
		return nil
	}
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
	return nil
}

// Timeline returns segments + events for a run.
func (s *Service) Timeline(ctx context.Context, id string) (*Timeline, error) {
	tl := &Timeline{RunID: id}
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT layer, state, started_at, ended_at, duration_ms, COALESCE(reason,''), COALESCE(source,''), detail_json
		FROM run_segments WHERE run_id=? ORDER BY started_at, id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var seg TimelineSegment
		var start, end sql.NullString
		var dur sql.NullInt64
		var detail sql.NullString
		if err := rows.Scan(&seg.Layer, &seg.State, &start, &end, &dur, &seg.Reason, &seg.Source, &detail); err != nil {
			rows.Close()
			return nil, err
		}
		seg.Start, _ = time.Parse(time.RFC3339Nano, start.String)
		if end.Valid {
			t, _ := time.Parse(time.RFC3339Nano, end.String)
			seg.End = &t
		}
		if dur.Valid {
			d := dur.Int64
			seg.DurationMS = &d
		}
		if detail.Valid {
			seg.Detail = json.RawMessage(detail.String)
		}
		tl.Segments = append(tl.Segments, seg)
	}
	rows.Close()

	erows, err := s.st.DB().QueryContext(ctx, `
		SELECT at, kind, COALESCE(source,''), detail_json FROM run_events WHERE run_id=? ORDER BY at, id`, id)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		var pt TimelinePoint
		var at string
		var detail sql.NullString
		if err := erows.Scan(&at, &pt.Kind, &pt.Source, &detail); err != nil {
			return nil, err
		}
		pt.At, _ = time.Parse(time.RFC3339Nano, at)
		if detail.Valid {
			pt.Detail = json.RawMessage(detail.String)
		}
		tl.Events = append(tl.Events, pt)
	}
	return tl, erows.Err()
}

// SetSessionState updates in-memory agent state and opens/closes agent segments.
// source is free-form (hook|screen|launch).
func (s *Service) SetSessionState(ctx context.Context, runID string, state SessionState, source string) {
	if ctx == nil {
		ctx = context.Background()
	}
	since := time.Now().UTC()
	s.mu.Lock()
	prev, had := s.sessions[runID]
	changed := !had || prev.State != state
	if changed {
		s.sessions[runID] = sessionSnap{State: state, Since: since}
	}
	s.mu.Unlock()
	// No transition: no segment write, no bus append (reaper sweeps call this every tick).
	if !changed {
		return
	}

	nowStr := since.Format(time.RFC3339Nano)
	if source == "" {
		source = "session"
	}
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if had {
			// Duration computed in Go — SQLite julianday mishandles RFC3339Nano.
			var segStart string
			_ = tx.QueryRow(`SELECT started_at FROM run_segments WHERE run_id=? AND layer='session' AND ended_at IS NULL ORDER BY id DESC LIMIT 1`,
				runID).Scan(&segStart)
			var durMS int64
			if t0, perr := time.Parse(time.RFC3339Nano, segStart); perr == nil {
				durMS = since.Sub(t0).Milliseconds()
			}
			if _, err := tx.Exec(`UPDATE run_segments SET ended_at=?, duration_ms=?
				WHERE run_id=? AND layer='session' AND ended_at IS NULL`, nowStr, durMS, runID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO run_segments (run_id, layer, state, started_at, reason, source)
			VALUES (?, 'session', ?, ?, ?, 'session')`, runID, string(state), nowStr, source); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		s.log.Warn("agent segment write", "run", runID, "error", err)
	}
	var ws, job string
	_ = s.st.DB().QueryRowContext(ctx, `SELECT workspace_id, job FROM runs WHERE id=?`, runID).Scan(&ws, &job)
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic:    "session.state",
		Entity:   "run",
		EntityID: runID,
		Attrs: map[string]any{
			"run_id": runID, "state": string(state), "since": nowStr, "source": source,
			"workspace": ws, "job": job,
		},
	}); err != nil {
		s.log.Warn("session.state append", "run", runID, "error", err)
	}
}

// SessionState returns the current agent state for a run (empty if unknown).
func (s *Service) SessionState(runID string) SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a, ok := s.sessions[runID]; ok {
		return a.State
	}
	return ""
}

// SessionSnapshot returns all live session states.
func (s *Service) SessionSnapshot() map[string]SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]SessionState, len(s.sessions))
	for id, a := range s.sessions {
		out[id] = a.State
	}
	return out
}

// WaitingCount returns how many agents are in the waiting state. It reads the
// in-memory snapshot, so it costs no query.
func (s *Service) WaitingCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, a := range s.sessions {
		if a.State == SessionWaiting {
			n++
		}
	}
	return n
}

// RunningCount returns number of running rows.
func (s *Service) RunningCount(ctx context.Context) (int, error) {
	var n int
	err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE status='running'`).Scan(&n)
	return n, err
}

// Counts returns total and running run counts.
func (s *Service) Counts(ctx context.Context) (total, running int, err error) {
	err = s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&total)
	if err != nil {
		return 0, 0, err
	}
	running, err = s.RunningCount(ctx)
	return total, running, err
}

// StatusByJob returns the newest run status and live run ids per "workspace/job" lane.
func (s *Service) StatusByJob(ctx context.Context) (last map[string]Status, live map[string][]string, err error) {
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT workspace_id, job, id, status FROM runs WHERE kind='job' AND (
			status='running' OR id IN (SELECT MAX(id) FROM runs WHERE kind='job' GROUP BY workspace_id, job)
		)`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	last = map[string]Status{}
	live = map[string][]string{}
	for rows.Next() {
		var ws, jb, id, st string
		if err := rows.Scan(&ws, &jb, &id, &st); err != nil {
			return nil, nil, err
		}
		key := ws + "/" + jb
		last[key] = Status(st)
		if Status(st).IsLive() {
			live[key] = append(live[key], id)
		}
	}
	return last, live, rows.Err()
}

// AddEvent appends a timeline point event (best-effort).
func (s *Service) AddEvent(ctx context.Context, runID, kind, source string, detail any) {
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	var d *string
	if detail != nil {
		b, _ := json.Marshal(detail)
		s := string(b)
		d = &s
	}
	_, _ = s.st.DB().ExecContext(ctx, `INSERT INTO run_events (run_id, at, kind, source, detail_json)
		VALUES (?,?,?,?,?)`, runID, nowStr, kind, source, d)
}

// markResumedPredecessor makes the predecessor of a resume terminal `resumed` (B17).
// newRun builds the in-memory run row for Create.
func newRun(id string, now time.Time, opts CreateOpts) *Run {
	run := &Run{
		ID:              id,
		Workspace:       opts.WorkspaceID,
		Job:             opts.Job,
		Kind:            opts.Kind,
		Reason:          opts.Reason,
		Status:          StatusRunning,
		StartedAt:       now,
		Harness:         opts.Harness,
		Runtime:         opts.Runtime,
		SessionID:       opts.SessionID,
		PromptPreview:   trunc(opts.PromptPreview, 80),
		Inputs:          opts.Inputs,
		Trigger:         opts.Trigger,
		ConversationKey: opts.ConversationKey,
		ConversationID:  opts.ConversationID,
		WorkingDir:      opts.WorkingDir,
		QueueID:         opts.QueueID,
		TraceID:         opts.TraceID,
		Revision:        1,
		MemoryHash:      opts.MemoryHash,
		ResumedFrom:     opts.ResumedFrom,
		ResumeAttempts:  opts.ResumeAttempts,
		Session: SessionSpec{
			MaxDurationMS: opts.MaxDurationMS,
			IdleTimeoutMS: opts.IdleTimeoutMS,
			KeepAliveMS:   opts.KeepAliveMS,
			NudgeMS:       opts.NudgeMS,
		},
	}
	if opts.TmuxSession != "" {
		run.Attach = &AttachInfo{Kind: "tmux", Tmux: opts.TmuxSession}
	} else if opts.Runtime == "native" {
		run.Attach = &AttachInfo{Kind: "none"}
	}
	return run
}

// insertRunRow writes the run row, the launch segment, and the launch event.
func insertRunRow(tx *sql.Tx, run *Run, opts CreateOpts, nowStr string) error {
	var inputsJSON *string
	if opts.Inputs != nil {
		b, _ := json.Marshal(opts.Inputs)
		s := string(b)
		inputsJSON = &s
	}
	trigEventID, trigInteg, capJSON, varsJSON := opts.triggerJSON()
	var queueID *string
	if opts.QueueID != "" {
		queueID = &opts.QueueID
	}

	_, err := tx.Exec(`INSERT INTO runs (
		id, workspace_id, job, kind, reason, status, prompt_preview, inputs_json,
		trigger_event_id, trigger_connector, trigger_capture_json, trigger_vars_json,
		conversation_key, conversation_id, working_dir, session_id, runtime, harness, tmux_session,
		started_at, max_duration_ms, idle_timeout_ms, keep_alive_ms, nudge_ms,
		resumed_from, resume_attempts, queue_id, trace_id, memory_hash, revision, updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, opts.WorkspaceID, opts.Job, opts.Kind, string(opts.Reason), string(StatusRunning),
		run.PromptPreview, inputsJSON, trigEventID, trigInteg, capJSON, varsJSON,
		store.NullStr(opts.ConversationKey), store.NullStr(opts.ConversationID), store.NullStr(opts.WorkingDir), store.NullStr(opts.SessionID), opts.Runtime, opts.Harness, store.NullStr(opts.TmuxSession),
		nowStr, opts.MaxDurationMS, opts.IdleTimeoutMS, opts.KeepAliveMS, opts.NudgeMS,
		store.NullStr(opts.ResumedFrom), opts.ResumeAttempts, queueID, opts.TraceID, opts.MemoryHash, 1, nowStr,
	)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO run_segments (run_id, layer, state, started_at, reason, source)
		VALUES (?, 'run', 'running', ?, 'launch', 'runner')`, run.ID, nowStr)
	if err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]string{"reason": string(opts.Reason)})
	_, err = tx.Exec(`INSERT INTO run_events (run_id, at, kind, source, detail_json, trace_id)
		VALUES (?, ?, 'launch', 'runner', ?, ?)`, run.ID, nowStr, string(detail), opts.TraceID)
	if err != nil {
		return err
	}
	// B17: the predecessor of a resume becomes superseded-terminal `resumed`.
	if opts.ResumedFrom != "" {
		return markResumedPredecessor(tx, opts.ResumedFrom, run.ID, nowStr)
	}
	return nil
}

func markResumedPredecessor(tx *sql.Tx, predecessorID, newID, nowStr string) error {
	res, err := tx.Exec(`UPDATE runs SET status=?, revision=revision+1, updated_at=?
		WHERE id=? AND status NOT IN (?, ?)`,
		string(StatusResumed), nowStr, predecessorID,
		string(StatusRunning), string(StatusResumed))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, err = store.AppendInTx(tx, store.BusEvent{
			Topic: "run.updated", Entity: "run", EntityID: predecessorID, Action: "updated",
			Attrs: map[string]string{"status": string(StatusResumed), "resumed_by": newID},
		})
		return err
	}
	return nil
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// StatsOpts selects the rows of a run report.
type StatsOpts struct {
	Workspace string
	Job       string
	// Range is 24h, 7d, 30d, or mtd. An empty value means 30d.
	Range string
}

// Stats is the §3.9 run report: the selected range, the month to date, and one
// row per day of the range.
type Stats struct {
	Range  string      `json:"range"`
	Totals StatsTotals `json:"totals"`
	MTD    StatsTotals `json:"mtd"`
	Days   []StatsDay  `json:"days"`
}

// StatsTotals counts runs and money over one period.
type StatsTotals struct {
	Runs         int     `json:"runs"`
	OK           int     `json:"ok"`
	Failed       int     `json:"failed"`
	CostUSD      float64 `json:"cost_usd"`
	TokensBilled int64   `json:"tokens_billed"`
}

// StatsDay is one calendar day of the range, oldest first.
type StatsDay struct {
	Date string `json:"date"`
	StatsTotals
}

// since returns the start of the range and its label.
func (o StatsOpts) since(now time.Time) (time.Time, string) {
	switch o.Range {
	case "24h":
		return now.Add(-24 * time.Hour), "24h"
	case "7d":
		return now.AddDate(0, 0, -7), "7d"
	case "mtd":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), "mtd"
	default:
		return now.AddDate(0, 0, -30), "30d"
	}
}

// ValidRange reports whether r names a stats range.
func ValidRange(r string) bool {
	switch r {
	case "", "24h", "7d", "30d", "mtd":
		return true
	}
	return false
}

// Stats reports run counts and cost for the selected range, plus the month to
// date. Cost is priced per model, the same way a single run row is priced.
func (s *Service) Stats(ctx context.Context, opts StatsOpts) (*Stats, error) {
	now := time.Now().UTC()
	from, label := opts.since(now)
	mtdFrom := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	earliest := from
	if mtdFrom.Before(earliest) {
		earliest = mtdFrom
	}

	rows, err := s.statsRows(ctx, opts, earliest)
	if err != nil {
		return nil, err
	}
	out := &Stats{Range: label, Days: dailyStats(rows, from, now)}
	for _, row := range rows {
		if !row.at.Before(from) {
			out.Totals.add(row)
		}
		if !row.at.Before(mtdFrom) {
			out.MTD.add(row)
		}
	}
	return out, nil
}

// statsRow is one run reduced to what a report needs.
type statsRow struct {
	at     time.Time
	status Status
	usage  pricing.TokenUsage
}

// add folds one run into the totals.
func (t *StatsTotals) add(row statsRow) {
	t.Runs++
	switch row.status {
	case StatusComplete:
		t.OK++
	case StatusFailed, StatusExited, StatusTimedOut:
		t.Failed++
	}
	t.CostUSD += row.usage.CostUSD
	t.TokensBilled += row.usage.Billed
}

// dailyStats buckets rows by UTC day, oldest first, with empty days present.
func dailyStats(rows []statsRow, from, now time.Time) []StatsDay {
	const day = "2006-01-02"
	byDate := map[string]*StatsTotals{}
	var days []StatsDay
	for d := from.UTC().Truncate(24 * time.Hour); !d.After(now); d = d.AddDate(0, 0, 1) {
		days = append(days, StatsDay{Date: d.Format(day)})
		byDate[d.Format(day)] = &days[len(days)-1].StatsTotals
	}
	for _, row := range rows {
		if t := byDate[row.at.UTC().Format(day)]; t != nil {
			t.add(row)
		}
	}
	return days
}

// statsRows reads the runs of the report window and prices each one.
func (s *Service) statsRows(ctx context.Context, opts StatsOpts, from time.Time) ([]statsRow, error) {
	q := `SELECT started_at, status, COALESCE(tokens_input,0), COALESCE(tokens_output,0),
		COALESCE(tokens_cache_read,0), COALESCE(tokens_cache_creation,0), COALESCE(tokens_model,'')
		FROM runs WHERE started_at >= ?`
	args := []any{from.Format(time.RFC3339)}
	if opts.Workspace != "" {
		q += ` AND workspace_id = ?`
		args = append(args, opts.Workspace)
	}
	if opts.Job != "" {
		q += ` AND job = ?`
		args = append(args, opts.Job)
	}
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []statsRow
	for rows.Next() {
		var started, status, model string
		var in, outTok, cacheR, cacheC int64
		if err := rows.Scan(&started, &status, &in, &outTok, &cacheR, &cacheC, &model); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339, started)
		if err != nil {
			continue
		}
		u := pricing.TokenUsage{Input: in, Output: outTok, CacheRead: cacheR, CacheCreation: cacheC, Model: model}
		u.ApplyRates()
		out = append(out, statsRow{at: at.UTC(), status: Status(status), usage: u})
	}
	return out, rows.Err()
}

// approvalToDecide picks the row a decision answers: the named one, else the
// run's pending row, else a fresh id for an insert-as-decided row.
func (s *Service) approvalToDecide(ctx context.Context, runID, approvalID string) (*Approval, error) {
	if approvalID != "" {
		a, err := s.approvalByID(ctx, approvalID)
		if err != nil {
			return nil, err
		}
		if a.RunID != runID {
			return nil, ErrApprovalRunMismatch
		}
		return a, nil
	}
	if a, err := s.pendingApproval(ctx, runID); err != nil {
		return nil, err
	} else if a != nil {
		return a, nil
	}
	return &Approval{ID: ulid.Make().String(), RunID: runID}, nil
}

func (s *Service) pendingApproval(ctx context.Context, runID string) (*Approval, error) {
	return s.scanApproval(s.st.DB().QueryRowContext(ctx, approvalCols+
		` FROM approvals WHERE run_id=? AND status='pending' ORDER BY id DESC LIMIT 1`, runID))
}

func (s *Service) approvalByID(ctx context.Context, id string) (*Approval, error) {
	a, err := s.scanApproval(s.st.DB().QueryRowContext(ctx, approvalCols+` FROM approvals WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, sql.ErrNoRows
	}
	return a, nil
}

// scanApproval reads one row; a missing row is (nil, nil).
func (s *Service) scanApproval(row *sql.Row) (*Approval, error) {
	var a Approval
	err := row.Scan(&a.ID, &a.RunID, &a.Status, &a.Prompt, &a.Decision, &a.Revision, &a.CreatedAt, &a.DecidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Service) scanRun(row rowScanner) (*Run, error) {
	r := &Run{}
	var reason, endReason, started, ended sql.NullString
	var trigEvent, trigInteg, capJSON, varsJSON, inputsJSON sql.NullString
	var ti, to, tcr, tcc, maxD, idle, keep, nudge, resumeAtt, rev int64
	var modelName, sess, harness, runtime, tmux, qid, trace, convKey, convID, workingDir, resumedFrom string
	var prompt, ws, job, kind, status, memoryHash string
	err := row.Scan(
		&r.ID, &ws, &job, &kind, &reason, &status, &endReason,
		&prompt, &sess, &harness, &runtime, &tmux,
		&ti, &to, &tcr, &tcc, &modelName,
		&started, &ended, &maxD, &idle, &keep, &nudge,
		&resumedFrom, &resumeAtt, &qid, &trace, &rev,
		&convKey, &convID, &workingDir, &trigEvent, &trigInteg, &capJSON, &varsJSON, &inputsJSON,
		&memoryHash,
	)
	if err != nil {
		return nil, err
	}
	if inputsJSON.Valid && inputsJSON.String != "" {
		_ = json.Unmarshal([]byte(inputsJSON.String), &r.Inputs)
	}
	r.Workspace = ws
	r.Job = job
	r.Kind = kind
	r.Reason = RunReason(reason.String)
	r.Status = Status(status)
	r.EndReason = EndReason(endReason.String)
	r.PromptPreview = prompt
	r.SessionID = sess
	r.Harness = harness
	r.Runtime = runtime
	r.ResumedFrom = resumedFrom
	r.ResumeAttempts = int(resumeAtt)
	r.QueueID = qid
	r.TraceID = trace
	r.Revision = rev
	r.ConversationKey = convKey
	r.ConversationID = convID
	r.WorkingDir = workingDir
	r.MemoryHash = memoryHash
	r.StartedAt = store.ParseTS(started.String)
	if ended.Valid {
		t := store.ParseTS(ended.String)
		r.EndedAt = &t
		r.DurationMS = t.Sub(r.StartedAt).Milliseconds()
	} else {
		r.DurationMS = time.Since(r.StartedAt).Milliseconds()
	}
	r.Tokens = pricing.TokenUsage{Input: ti, Output: to, CacheRead: tcr, CacheCreation: tcc, Model: modelName}
	r.Tokens.ApplyRates()
	r.Session = SessionSpec{MaxDurationMS: maxD, IdleTimeoutMS: idle, KeepAliveMS: keep, NudgeMS: nudge}
	r.TmuxSession = tmux
	if tmux != "" {
		r.Attach = &AttachInfo{Kind: "tmux", Tmux: tmux}
	}
	if trigInteg.Valid || capJSON.Valid {
		r.Trigger = &TriggerCapture{Connector: trigInteg.String, EventID: trigEvent.String}
		if capJSON.Valid {
			r.Trigger.Event = json.RawMessage(capJSON.String)
		}
		if varsJSON.Valid {
			_ = json.Unmarshal([]byte(varsJSON.String), &r.Trigger.Vars)
		}
	}
	if r.Status.IsLive() {
		s.mu.RLock()
		if a, ok := s.sessions[r.ID]; ok {
			r.SessionState = a.State
			st := a.Since
			r.SessionStateSince = &st
		}
		s.mu.RUnlock()
	}
	return r, nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
