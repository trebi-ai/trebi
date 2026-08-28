package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

const (
	busCursorFlag        = "alerts.bus_seq"
	defaultNotifyTimeout = 5 * time.Second
	deliverQueueSize     = 16
	maxAlertText         = 512
)

type deliverJob struct {
	opts RaiseOpts
	id   string
}

// Run polls the bus until ctx is cancelled. Tick interval comes from Options.
// A delivery goroutine drains the notify queue and exits when ctx is done, so
// it does not outlive Stop().
func (s *Service) Run(ctx context.Context) {
	iv := s.opts.TickInterval
	if iv <= 0 {
		iv = 2 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.deliverLoop(ctx)
	}()
	s.running.Store(true)
	defer func() {
		s.running.Store(false)
		wg.Wait()
	}()

	s.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

func (s *Service) deliverLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.deliverCh:
			s.deliver(ctx, job.opts, job.id)
		}
	}
}

// Tick consumes new bus events, raises dwelt waiting items, and syncs doctor.
func (s *Service) Tick(ctx context.Context) {
	s.Consume(ctx)
	s.raiseDweltWaiting(ctx)
	s.checkUsage(ctx)
	if s.opts.Doctor != nil {
		s.SyncDoctor(ctx, s.opts.Doctor())
	}
}

// Consume reads event_log after the persisted cursor and raises/resolves.
func (s *Service) Consume(ctx context.Context) {
	after := s.loadCursor()
	events, err := s.st.ListBus(ctx, after, consumeTopics(), 200)
	if err != nil {
		s.log.Warn("alerts consume", "error", err)
		return
	}
	var last int64
	for _, ev := range events {
		s.handleEvent(ctx, ev)
		last = ev.Seq
	}
	if last > 0 {
		s.saveCursor(last)
	}
}

// SyncDoctor upserts health inbox items for findings and auto-resolves stale ones.
func (s *Service) SyncDoctor(ctx context.Context, findings []DoctorFinding) {
	want := map[string]struct{}{}
	for _, f := range findings {
		want[f.ID] = struct{}{}
		s.upsertDoctor(ctx, f)
	}
	s.resolveStaleDoctor(ctx, want)
}

func (s *Service) handleEvent(ctx context.Context, ev store.StreamEvent) {
	attrs := attrMap(ev.Attrs)
	switch ev.Topic {
	case "agent.state":
		s.onAgentState(ctx, attrs)
	case "run.failed":
		s.onRunFailed(ctx, ev, attrs)
	case "run.updated":
		s.onRunUpdated(ctx, ev, attrs)
	case "run.resume_exhausted":
		s.onResumeExhausted(ctx, ev, attrs)
	case "integration.unhealthy":
		s.onIntegrationUnhealthy(ctx, ev, attrs)
	case "integration.recovered":
		s.autoResolve(ctx, KindIntegrationUnhealthy, "integration/"+ev.ID, "auto")
	case "auth.probe.failed":
		s.onAuthMissing(ctx, ev, attrs)
	case "harness.limit_exceeded":
		s.onLimitExceeded(ctx, ev, attrs)
	case "harness.limit_cleared":
		name := firstNonEmpty(attrs["harness"], ev.ID)
		s.autoResolve(ctx, KindLimitExceeded, "harness/"+name, "auto")
	}
}

func (s *Service) onAgentState(ctx context.Context, attrs map[string]string) {
	runID := attrs["run_id"]
	if runID == "" {
		return
	}
	if attrs["state"] == "waiting" {
		s.noteWaiting(runID, attrs)
		return
	}
	s.mu.Lock()
	delete(s.waiting, runID)
	s.mu.Unlock()
	s.autoResolve(ctx, KindWaiting, "run/"+runID, "auto")
}

func (s *Service) noteWaiting(runID string, attrs map[string]string) {
	since := s.opts.Now().UTC()
	if t := parseTime(attrs["since"]); !t.IsZero() {
		since = t
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.waiting[runID]; ok {
		return
	}
	s.waiting[runID] = waitingEp{
		since: since, runID: runID,
		ws: attrs["workspace"], job: attrs["job"], trace: attrs["trace_id"],
	}
}

func (s *Service) raiseDweltWaiting(ctx context.Context) {
	if s.opts.WaitingDisabled {
		return
	}
	after := s.opts.WaitingAfter
	if after <= 0 {
		after = 5 * time.Minute
	}
	now := s.opts.Now()
	s.mu.Lock()
	var due []waitingEp
	for id, ep := range s.waiting {
		if ep.raised || now.Sub(ep.since) < after {
			continue
		}
		ep.raised = true
		s.waiting[id] = ep
		due = append(due, ep)
	}
	s.mu.Unlock()
	for _, ep := range due {
		_, err := s.Raise(ctx, RaiseOpts{
			Kind: KindWaiting, RunID: ep.runID, WorkspaceID: ep.ws, JobName: ep.job,
			TraceID: ep.trace, Title: "Agent waiting", Hint: "open attach",
		})
		if err != nil {
			s.log.Warn("raise waiting", "run", ep.runID, "error", err)
		}
	}
}

func (s *Service) onRunFailed(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindRunFailed, RunID: ev.ID, WorkspaceID: attrs["workspace"],
		JobName: attrs["job"], EndReason: attrs["end_reason"], TraceID: ev.TraceID,
		Harness: attrs["harness"],
		Title:   "Run failed", Body: "end_reason=" + attrs["end_reason"],
	})
	if err != nil {
		s.log.Warn("raise run_failed", "run", ev.ID, "error", err)
	}
}

func (s *Service) onRunUpdated(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	if attrs["status"] != "complete" {
		return
	}
	// Retry succeeded: clear a run_failed item for this job.
	subj := "job/" + attrs["workspace"] + "/" + attrs["job"]
	if attrs["workspace"] != "" && attrs["job"] != "" {
		s.autoResolve(ctx, KindRunFailed, subj, "auto")
	}
	s.autoResolve(ctx, KindResumeExhausted, "run/"+ev.ID, "auto")
}

func (s *Service) onResumeExhausted(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindResumeExhausted, RunID: firstNonEmpty(attrs["run_id"], ev.ID),
		SessionID: attrs["session_id"], WorkspaceID: attrs["workspace"],
		JobName: attrs["job"], TraceID: ev.TraceID,
		Title: "Resume exhausted",
	})
	if err != nil {
		s.log.Warn("raise resume_exhausted", "run", ev.ID, "error", err)
	}
}

func (s *Service) onIntegrationUnhealthy(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	name := firstNonEmpty(attrs["integration"], ev.ID)
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindIntegrationUnhealthy, Integration: name, TraceID: ev.TraceID,
		Title: "Integration unhealthy", Body: attrs["status"],
	})
	if err != nil {
		s.log.Warn("raise integration_unhealthy", "integration", name, "error", err)
	}
}

func (s *Service) onAuthMissing(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindAuthMissing, RunID: attrs["run_id"], Harness: attrs["harness"],
		TraceID: ev.TraceID, Title: "Auth missing",
	})
	if err != nil {
		s.log.Warn("raise auth_missing", "error", err)
	}
}

func (s *Service) onLimitExceeded(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	name := firstNonEmpty(attrs["harness"], ev.ID)
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindLimitExceeded, Harness: name, RunID: attrs["run_id"],
		TraceID: ev.TraceID, Title: "Harness limit exceeded",
	})
	if err != nil {
		s.log.Warn("raise limit_exceeded", "harness", name, "error", err)
	}
}

func (s *Service) checkUsage(ctx context.Context) {
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT name, usage_percent FROM harness_state
		WHERE usage_percent IS NOT NULL`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var pct float64
		if rows.Scan(&name, &pct) != nil {
			continue
		}
		subj := "harness/" + name
		if pct < float64(s.opts.UsageWarnAt) {
			s.autoResolve(ctx, KindUsageWarning, subj, "auto")
			continue
		}
		_, err := s.Raise(ctx, RaiseOpts{
			Kind: KindUsageWarning, Harness: name,
			Title: "Harness usage warning",
			Body:  fmt.Sprintf("%.0f%% ≥ %d%%", pct, s.opts.UsageWarnAt),
		})
		if err != nil {
			s.log.Warn("raise usage_warning", "harness", name, "error", err)
		}
	}
}

func (s *Service) loadCursor() int64 {
	v, ok, err := s.st.GetFlag(busCursorFlag)
	if err != nil || !ok {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

func (s *Service) saveCursor(seq int64) {
	if err := s.st.SetFlag(busCursorFlag, strconv.FormatInt(seq, 10)); err != nil {
		s.log.Warn("alerts cursor", "error", err)
	}
}

func (s *Service) enqueueDeliver(ctx context.Context, opts RaiseOpts, id string) {
	if s.skipNotify() || len(s.opts.Notify) == 0 {
		return
	}
	if !s.running.Load() {
		s.deliver(ctx, opts, id)
		return
	}
	select {
	case s.deliverCh <- deliverJob{opts: opts, id: id}:
	default:
		s.log.Warn("alert delivery queue full", "id", id)
	}
}

func (s *Service) skipNotify() bool {
	if s.opts.Shadow || s.opts.Drain {
		return true
	}
	v, ok, err := s.st.GetFlag("drain")
	return err == nil && ok && v == "1"
}

// deliver tries channels in order and stops at the first success.
func (s *Service) deliver(ctx context.Context, opts RaiseOpts, id string) {
	timeout := s.opts.NotifyTimeout
	if timeout <= 0 {
		timeout = defaultNotifyTimeout
	}
	for i, ch := range s.opts.Notify {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		cmd := exec.CommandContext(cctx, "sh", "-c", expandNotify(ch.Command))
		cmd.Env = notifyEnv(opts, id)
		err := cmd.Run()
		cancel()
		status := "ok"
		if err != nil {
			status = err.Error()
			s.log.Warn("alert.delivered", "channel", ch.Name, "index", i, "error", err)
		}
		if _, aerr := s.st.Append(ctx, store.BusEvent{
			Topic: "alert.delivered", Entity: "alert", EntityID: id, Action: "updated",
			Attrs: map[string]string{
				"kind": string(opts.Kind), "channel": ch.Name,
				"index": strconv.Itoa(i), "status": status,
			},
		}); aerr != nil {
			s.log.Warn("bus append", "topic", "alert.delivered", "error", aerr)
		}
		if err == nil {
			return
		}
	}
}

func (s *Service) upsertDoctor(ctx context.Context, f DoctorFinding) {
	subject := "doctor/" + f.ID
	item, err := s.findActiveBySubject(ctx, subject)
	if err != nil {
		s.log.Warn("doctor lookup", "id", f.ID, "error", err)
		return
	}
	actJSON, _ := json.Marshal([]InboxAction{{
		Kind: ActionFix, Route: "/v1/system/doctor/" + f.ID + "/fix", Label: "Fix",
	}})
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	if item != nil {
		_, _ = s.st.DB().ExecContext(ctx, `UPDATE inbox_items SET title=?, body=?, actions=?,
			revision=revision+1, updated_at=? WHERE id=?`,
			f.Message, f.Message, string(actJSON), nowStr, item.ID)
		return
	}
	id := ulid.Make().String()
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_items (
			id, kind, status, subject, title, body, actions, alert_kind, revision, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,1,?,?)`,
			id, string(InboxHealth), string(InboxUnread), subject, f.Message, f.Message,
			string(actJSON), "doctor", nowStr, nowStr)
		return err
	}, store.BusEvent{
		Topic: "inbox.created", Entity: "inbox", EntityID: id, Action: "created",
		Attrs: map[string]string{"kind": string(InboxHealth), "status": "unread"},
	})
	if err != nil {
		s.log.Warn("doctor inbox", "id", f.ID, "error", err)
	}
}

func (s *Service) findActiveBySubject(ctx context.Context, subject string) (*InboxItem, error) {
	row := s.st.DB().QueryRowContext(ctx, `
		SELECT `+inboxCols+`
		FROM inbox_items WHERE subject=? AND status != 'resolved'
		ORDER BY created_at DESC LIMIT 1`, subject)
	item, err := scanInbox(row, false)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return item, err
}

func (s *Service) resolveStaleDoctor(ctx context.Context, want map[string]struct{}) {
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT `+inboxCols+` FROM inbox_items
		WHERE subject LIKE 'doctor/%' AND status != 'resolved'`)
	if err != nil {
		return
	}
	defer rows.Close()
	var stale []string
	for rows.Next() {
		item, err := scanInbox(rows, false)
		if err != nil {
			return
		}
		id := strings.TrimPrefix(item.Subject, "doctor/")
		if _, ok := want[id]; !ok {
			stale = append(stale, item.ID)
		}
	}
	for _, id := range stale {
		_ = s.SetStatus(ctx, id, InboxResolved, "auto")
	}
}

// CollectDoctorFindings is the system doctor snapshot (also used by the API).
func CollectDoctorFindings() []DoctorFinding {
	var findings []DoctorFinding
	if _, err := os.Stat(home.DBPath()); err != nil {
		findings = append(findings, DoctorFinding{
			ID: "db-missing", Severity: "error", Code: "db_missing",
			Message: "trebi.db missing — run trebi service migrate-db",
		})
	}
	// The daemon seeds config.yaml at startup, so an absent file means it was
	// removed afterwards. Defaults still apply, hence info and not warn.
	if _, err := os.Stat(home.ConfigPath()); err != nil {
		findings = append(findings, DoctorFinding{
			ID: "config-missing", Severity: "info", Code: "config_missing",
			Message: "config.yaml missing; defaults apply (fix rewrites it)",
		})
	}
	for _, h := range harness.List() {
		if h.Present {
			continue
		}
		findings = append(findings, DoctorFinding{
			ID: "harness-" + h.Name, Severity: "info", Code: "harness_home_missing",
			Message: fmt.Sprintf("harness home for %s not created yet (will appear on first launch)", h.Name),
		})
	}
	if findings == nil {
		return []DoctorFinding{}
	}
	return findings
}

func consumeTopics() []string {
	return []string{
		"agent.state",
		"run.failed",
		"run.updated",
		"run.resume_exhausted",
		"integration.unhealthy",
		"integration.recovered",
		"auth.probe.failed",
		"harness.limit_exceeded",
		"harness.limit_cleared",
	}
}

func attrMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	var anyMap map[string]any
	if json.Unmarshal(raw, &anyMap) != nil {
		return out
	}
	for k, v := range anyMap {
		if v == nil {
			continue
		}
		out[k] = fmt.Sprint(v)
	}
	return out
}

// expandNotify rewrites {{placeholders}} to quoted $TREBI_ALERT_* references.
// Untrusted values never enter the command string; they travel via the env.
func expandNotify(cmd string) string {
	for _, p := range notifyPlaceholders() {
		wrapped := `"$` + p.env + `"`
		cmd = strings.ReplaceAll(cmd, "'{{"+p.ph+"}}'", wrapped)
		cmd = strings.ReplaceAll(cmd, `"{{`+p.ph+`}}"`, wrapped)
		cmd = strings.ReplaceAll(cmd, "{{"+p.ph+"}}", wrapped)
	}
	return cmd
}

func notifyEnv(opts RaiseOpts, id string) []string {
	vals := map[string]string{
		"TREBI_ALERT_KIND":        string(opts.Kind),
		"TREBI_ALERT_TOOL":        opts.Harness,
		"TREBI_ALERT_TASK_ID":     opts.RunID,
		"TREBI_ALERT_JOB":         opts.JobName,
		"TREBI_ALERT_WORKSPACE":   opts.WorkspaceID,
		"TREBI_ALERT_MESSAGE":     opts.Title,
		"TREBI_ALERT_HINT":        opts.Hint,
		"TREBI_ALERT_BODY":        opts.Body,
		"TREBI_ALERT_RESUME":      id,
		"TREBI_ALERT_INTEGRATION": opts.Integration,
	}
	env := os.Environ()
	for k, v := range vals {
		env = append(env, k+"="+v)
	}
	return env
}

func notifyPlaceholders() []struct{ ph, env string } {
	return []struct{ ph, env string }{
		{"kind", "TREBI_ALERT_KIND"},
		{"tool", "TREBI_ALERT_TOOL"},
		{"task_id", "TREBI_ALERT_TASK_ID"},
		{"job", "TREBI_ALERT_JOB"},
		{"workspace", "TREBI_ALERT_WORKSPACE"},
		{"message", "TREBI_ALERT_MESSAGE"},
		{"hint", "TREBI_ALERT_HINT"},
		{"body", "TREBI_ALERT_BODY"},
		{"resume", "TREBI_ALERT_RESUME"},
		{"integration", "TREBI_ALERT_INTEGRATION"},
	}
}

// sanitizeText strips shell-metacharacters and control chars from alert text
// at raise time (backend.md message hygiene).
func sanitizeText(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '`' || r == '"':
			continue
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 32 || r == 127:
			continue
		default:
			b.WriteRune(r)
		}
	}
	s = strings.Join(strings.Fields(b.String()), " ")
	rs := []rune(s)
	if len(rs) > maxAlertText {
		s = string(rs[:maxAlertText])
	}
	return s
}
