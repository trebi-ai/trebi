// Package runs owns run CRUD, timeline, launch path, and agent state.
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
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/store"
)

// Service is the runs subsystem.
type Service struct {
	st  *store.Store
	log *slog.Logger

	mu     sync.RWMutex
	agents map[string]agentSnap // run_id → state
}

type agentSnap struct {
	State AgentState
	Since time.Time
}

var (
	// ErrInvalidDecision reports an approval decision other than allow|deny.
	ErrInvalidDecision = errors.New("decision must be allow or deny")
	// ErrNotRunning reports InjectMessage against a run that is not live.
	ErrNotRunning = errors.New("run is not running")
)

// New creates a runs service.
func New(st *store.Store) *Service {
	return &Service{st: st, log: slog.Default().With("source", "runs"), agents: make(map[string]agentSnap)}
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
	Trigger         *TriggerCapture
	Inputs          map[string]string
	MaxDurationMS   int64
	IdleTimeoutMS   int64
	KeepAliveMS     int64
	NudgeMS         int64
	ResumedFrom     string
	ResumeAttempts  int
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
	if o.TraceID == "" {
		o.TraceID = ulid.Make().String()
	}
	if o.NudgeMS == 0 {
		o.NudgeMS = -1
	}
}

// triggerJSON flattens the trigger capture into insert columns.
func (o *CreateOpts) triggerJSON() (eventID, integration, capture, vars *string) {
	if o.Trigger == nil {
		return nil, nil, nil, nil
	}
	if o.Trigger.EventID != "" {
		eventID = &o.Trigger.EventID
	}
	if o.Trigger.Integration != "" {
		integration = &o.Trigger.Integration
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
	return eventID, integration, capture, vars
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
		Attrs:    map[string]string{"job": opts.Job, "workspace": opts.WorkspaceID},
	})
	if err != nil {
		return nil, err
	}

	s.SetAgentState(ctx, id, AgentRunning, "launch")
	return run, nil
}

// Get returns a run by id.
func (s *Service) Get(ctx context.Context, id string) (*Run, error) {
	row := s.st.DB().QueryRowContext(ctx, `
		SELECT id, workspace_id, job, kind, COALESCE(reason,''), status, COALESCE(end_reason,''),
			COALESCE(prompt_preview,''), COALESCE(session_id,''), COALESCE(harness,''),
			COALESCE(runtime,'tui'), COALESCE(tmux_session,''),
			COALESCE(tokens_input,0), COALESCE(tokens_output,0), COALESCE(tokens_cache_read,0),
			COALESCE(tokens_cache_creation,0), COALESCE(tokens_model,''),
			started_at, ended_at, max_duration_ms, idle_timeout_ms, keep_alive_ms, nudge_ms,
			COALESCE(resumed_from,''), resume_attempts, COALESCE(queue_id,''), trace_id, revision,
			COALESCE(conversation_key,''), COALESCE(conversation_id,''),
			trigger_event_id, trigger_integration, trigger_capture_json, trigger_vars_json, inputs_json
		FROM runs WHERE id = ?`, id)
	return scanRun(row, s)
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

// ApprovalListOpts filters for ListApprovals.
type ApprovalListOpts struct {
	Limit  int
	Cursor string
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
	Limit           int
	Cursor          string
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
		COALESCE(conversation_key,''), COALESCE(conversation_id,''),
		trigger_event_id, trigger_integration, trigger_capture_json, trigger_vars_json, inputs_json
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
	q, args := opts.query()
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows, s)
		if err != nil {
			return nil, err
		}
		if opts.pseudoStatus() && string(r.AgentState) != opts.Status {
			continue
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
	q := `SELECT id, workspace_id, job, kind, COALESCE(reason,''), status, COALESCE(end_reason,''),
		COALESCE(prompt_preview,''), COALESCE(session_id,''), COALESCE(harness,''),
		COALESCE(runtime,'tui'), COALESCE(tmux_session,''),
		COALESCE(tokens_input,0), COALESCE(tokens_output,0), COALESCE(tokens_cache_read,0),
		COALESCE(tokens_cache_creation,0), COALESCE(tokens_model,''),
		started_at, ended_at, max_duration_ms, idle_timeout_ms, keep_alive_ms, nudge_ms,
		COALESCE(resumed_from,''), resume_attempts, COALESCE(queue_id,''), trace_id, revision,
		COALESCE(conversation_key,''), COALESCE(conversation_id,''),
		trigger_event_id, trigger_integration, trigger_capture_json, trigger_vars_json, inputs_json
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
		r, err := scanRun(rows, s)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DecideApproval records a allow|deny decision for runID in one transaction
// with the approval.decided bus frame. Revision starts at 1 (insert-as-decided).
func (s *Service) DecideApproval(ctx context.Context, runID, decision string) (*Approval, error) {
	if decision == "" {
		decision = "allow"
	}
	if decision != "allow" && decision != "deny" {
		return nil, ErrInvalidDecision
	}
	if _, err := s.Get(ctx, runID); err != nil {
		return nil, err
	}
	id := ulid.Make().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	a := &Approval{
		ID: id, RunID: runID, Status: "decided", Decision: decision,
		Revision: 1, CreatedAt: now, DecidedAt: now,
	}
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO approvals (id, run_id, question, status, decision, created_at, decided_at, revision)
			VALUES (?,?,?,?,?,?,?,1)`, id, runID, "", "decided", decision, now, now)
		return err
	}, store.BusEvent{
		Topic: "approval.decided", Entity: "approval", EntityID: id, Action: "updated",
		Revision: 1,
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
	q := `SELECT id, run_id, status, COALESCE(question,''), COALESCE(decision,''), revision, created_at, COALESCE(decided_at,'')
		FROM approvals WHERE 1=1`
	var args []any
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
		tokens.FillDerived()
		pricing.Apply(tokens)
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
		for _, layer := range []string{"task", "agent"} {
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
			VALUES (?, 'task', ?, ?, ?, 0, ?, 'reaper')`, id, string(status), nowStr, nowStr, string(end))
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
	delete(s.agents, id)
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

// SetAgentState updates in-memory agent state and opens/closes agent segments.
// source is free-form (hook|screen|launch).
func (s *Service) SetAgentState(ctx context.Context, runID string, state AgentState, source string) {
	if ctx == nil {
		ctx = context.Background()
	}
	since := time.Now().UTC()
	s.mu.Lock()
	prev, had := s.agents[runID]
	changed := !had || prev.State != state
	if changed {
		s.agents[runID] = agentSnap{State: state, Since: since}
	}
	s.mu.Unlock()
	// No transition: no segment write, no bus append (reaper sweeps call this every tick).
	if !changed {
		return
	}

	nowStr := since.Format(time.RFC3339Nano)
	if source == "" {
		source = "agent-bus"
	}
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if had {
			// Duration computed in Go — SQLite julianday mishandles RFC3339Nano.
			var segStart string
			_ = tx.QueryRow(`SELECT started_at FROM run_segments WHERE run_id=? AND layer='agent' AND ended_at IS NULL ORDER BY id DESC LIMIT 1`,
				runID).Scan(&segStart)
			var durMS int64
			if t0, perr := time.Parse(time.RFC3339Nano, segStart); perr == nil {
				durMS = since.Sub(t0).Milliseconds()
			}
			if _, err := tx.Exec(`UPDATE run_segments SET ended_at=?, duration_ms=?
				WHERE run_id=? AND layer='agent' AND ended_at IS NULL`, nowStr, durMS, runID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO run_segments (run_id, layer, state, started_at, reason, source)
			VALUES (?, 'agent', ?, ?, ?, 'agent-bus')`, runID, string(state), nowStr, source); err != nil {
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
		Topic: "agent.state",
		Attrs: map[string]any{
			"run_id": runID, "state": string(state), "since": nowStr, "source": source,
			"workspace": ws, "job": job,
		},
	}); err != nil {
		s.log.Warn("agent.state append", "run", runID, "error", err)
	}
}

// AgentState returns the current agent state for a run (empty if unknown).
func (s *Service) AgentState(runID string) AgentState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a, ok := s.agents[runID]; ok {
		return a.State
	}
	return ""
}

// AgentSnapshot returns all live agent states.
func (s *Service) AgentSnapshot() map[string]AgentState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]AgentState, len(s.agents))
	for id, a := range s.agents {
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
	for _, a := range s.agents {
		if a.State == AgentWaiting {
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
		Runtime:         "tui",
		SessionID:       opts.SessionID,
		PromptPreview:   trunc(opts.PromptPreview, 80),
		Inputs:          opts.Inputs,
		Trigger:         opts.Trigger,
		ConversationKey: opts.ConversationKey,
		ConversationID:  opts.ConversationID,
		QueueID:         opts.QueueID,
		TraceID:         opts.TraceID,
		Revision:        1,
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
		trigger_event_id, trigger_integration, trigger_capture_json, trigger_vars_json,
		conversation_key, conversation_id, session_id, runtime, harness, tmux_session,
		started_at, max_duration_ms, idle_timeout_ms, keep_alive_ms, nudge_ms,
		resumed_from, resume_attempts, queue_id, trace_id, revision, updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, opts.WorkspaceID, opts.Job, opts.Kind, string(opts.Reason), string(StatusRunning),
		run.PromptPreview, inputsJSON, trigEventID, trigInteg, capJSON, varsJSON,
		store.NullStr(opts.ConversationKey), store.NullStr(opts.ConversationID), store.NullStr(opts.SessionID), "tui", opts.Harness, store.NullStr(opts.TmuxSession),
		nowStr, opts.MaxDurationMS, opts.IdleTimeoutMS, opts.KeepAliveMS, opts.NudgeMS,
		store.NullStr(opts.ResumedFrom), opts.ResumeAttempts, queueID, opts.TraceID, 1, nowStr,
	)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO run_segments (run_id, layer, state, started_at, reason, source)
		VALUES (?, 'task', 'running', ?, 'launch', 'runner')`, run.ID, nowStr)
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

func scanRun(row rowScanner, s *Service) (*Run, error) {
	r := &Run{}
	var reason, endReason, started, ended sql.NullString
	var trigEvent, trigInteg, capJSON, varsJSON, inputsJSON sql.NullString
	var ti, to, tcr, tcc, maxD, idle, keep, nudge, resumeAtt, rev int64
	var modelName, sess, harness, runtime, tmux, qid, trace, convKey, convID, resumedFrom string
	var prompt, ws, job, kind, status string
	err := row.Scan(
		&r.ID, &ws, &job, &kind, &reason, &status, &endReason,
		&prompt, &sess, &harness, &runtime, &tmux,
		&ti, &to, &tcr, &tcc, &modelName,
		&started, &ended, &maxD, &idle, &keep, &nudge,
		&resumedFrom, &resumeAtt, &qid, &trace, &rev,
		&convKey, &convID, &trigEvent, &trigInteg, &capJSON, &varsJSON, &inputsJSON,
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
	r.StartedAt, _ = time.Parse(time.RFC3339Nano, started.String)
	if r.StartedAt.IsZero() {
		r.StartedAt, _ = time.Parse(time.RFC3339, started.String)
	}
	if ended.Valid {
		t, err := time.Parse(time.RFC3339Nano, ended.String)
		if err != nil {
			t, _ = time.Parse(time.RFC3339, ended.String)
		}
		r.EndedAt = &t
		r.DurationMS = t.Sub(r.StartedAt).Milliseconds()
	} else {
		r.DurationMS = time.Since(r.StartedAt).Milliseconds()
	}
	r.Tokens = pricing.TokenUsage{Input: ti, Output: to, CacheRead: tcr, CacheCreation: tcc, Model: modelName}
	r.Tokens.FillDerived()
	pricing.Apply(&r.Tokens)
	r.Session = SessionSpec{MaxDurationMS: maxD, IdleTimeoutMS: idle, KeepAliveMS: keep, NudgeMS: nudge}
	r.TmuxSession = tmux
	if tmux != "" {
		r.Attach = &AttachInfo{Kind: "tmux", Tmux: tmux}
	}
	if trigInteg.Valid || capJSON.Valid {
		r.Trigger = &TriggerCapture{Integration: trigInteg.String, EventID: trigEvent.String}
		if capJSON.Valid {
			r.Trigger.Event = json.RawMessage(capJSON.String)
		}
		if varsJSON.Valid {
			_ = json.Unmarshal([]byte(varsJSON.String), &r.Trigger.Vars)
		}
	}
	if r.Status.IsLive() {
		s.mu.RLock()
		if a, ok := s.agents[r.ID]; ok {
			r.AgentState = a.State
			st := a.Since
			r.AgentStateSince = &st
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
