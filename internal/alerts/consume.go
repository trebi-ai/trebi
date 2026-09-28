package alerts

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/proposals"
	"github.com/trebi-ai/trebi/internal/store"
)

const (
	busCursorFlag    = "alerts.bus_seq"
	deliverQueueSize = 16
	maxAlertText     = 512
)

// deliverJob is one route delivery. A nil route resolves a fresh one; a set
// route continues that frozen route at step from.
type deliverJob struct {
	opts  RaiseOpts
	id    string
	route []notify.Step
	from  int
}

//nolint:gochecknoglobals // static table
var consumeTopics = []string{
	"session.state",
	"run.failed",
	"run.updated",
	"run.resume_exhausted",
	"memory.invalid",
	"connector.unhealthy",
	"connector.recovered",
	"auth.probe.failed",
	"harness.updated",
	"harness.limit_exceeded",
	"harness.limit_cleared",
	"approval.created",
	"approval.decided",
	"proposal.created",
	"proposal.decided",
	"outbox.failed",
	"outbox.sent",
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
			s.runDeliver(ctx, job)
		}
	}
}

// Tick consumes new bus events, mails due escalations, raises dwelt waiting
// items, and syncs doctor.
func (s *Service) Tick(ctx context.Context) {
	s.Consume(ctx)
	s.sweepEscalations(ctx)
	s.raiseDweltWaiting(ctx)
	s.checkUsage(ctx)
	if s.opts.Doctor != nil {
		s.SyncDoctor(ctx, s.opts.Doctor())
	}
}

// Consume reads event_log after the persisted cursor and raises/resolves.
func (s *Service) Consume(ctx context.Context) {
	after := s.loadCursor()
	events, err := s.st.ListBus(ctx, after, consumeTopics, 200)
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

func (s *Service) onProposalCreated(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	id := firstNonEmpty(attrs["proposal_id"], ev.ID)
	if id == "" {
		return
	}
	agent := strings.TrimPrefix(attrs["agent"], "agents/")
	title := attrs["agent"]
	if agent != "" {
		title = agent
	}
	body := attrs["note"]
	var keys []string
	if raw := attrs["protected_keys"]; raw != "" {
		var parsed []string
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			keys = parsed
		}
	}
	stats := fmt.Sprintf("+%s −%s lines", func() string {
		if attrs["added"] != "" {
			return attrs["added"]
		}
		return "0"
	}(), func() string {
		if attrs["removed"] != "" {
			return attrs["removed"]
		}
		return "0"
	}())
	if len(keys) > 0 {
		stats += "; protected: " + strings.Join(keys, ", ")
	}
	body = strings.TrimSpace(strings.Join([]string{body, stats}, "\n"))
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindAgentProposal, Subject: "proposal/" + id, WorkspaceID: attrs["workspace"],
		JobName: attrs["agent"], RunID: attrs["run_id"], TraceID: ev.TraceID,
		Title: title + " proposes an AGENT.md change", Body: body,
		Hint: "Review the diff and approve or deny. Reply approve or deny by mail.",
	})
	if err != nil {
		s.log.Warn("raise proposal", "proposal", id, "error", err)
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
	case "session.state":
		s.onSessionState(ctx, attrs)
	case "run.failed":
		s.onRunFailed(ctx, ev, attrs)
	case "run.updated":
		s.onRunUpdated(ctx, ev, attrs)
	case "run.resume_exhausted":
		s.onResumeExhausted(ctx, ev, attrs)
	case "memory.invalid":
		s.onMemoryInvalid(ctx, ev, attrs)
	case "connector.unhealthy":
		s.onConnectorUnhealthy(ctx, ev, attrs)
	case "connector.recovered":
		s.autoResolve(ctx, KindConnectorUnhealthy, "connector/"+ev.ID, "auto")
	case "auth.probe.failed":
		s.onAuthMissing(ctx, ev, attrs)
	case "harness.updated":
		s.onHarnessUpdated(ctx, ev, attrs)
	case "harness.limit_exceeded":
		s.onLimitExceeded(ctx, ev, attrs)
	case "harness.limit_cleared":
		name := firstNonEmpty(attrs["harness"], ev.ID)
		s.autoResolve(ctx, KindLimitExceeded, "harness/"+name, "auto")
	case "approval.created":
		s.onApprovalCreated(ctx, ev, attrs)
	case "approval.decided":
		s.autoResolve(ctx, KindWaiting, "run/"+firstNonEmpty(attrs["run_id"], ev.ID), "auto")
	case "proposal.created":
		s.onProposalCreated(ctx, ev, attrs)
	case "proposal.decided":
		s.autoResolve(ctx, KindAgentProposal, "proposal/"+firstNonEmpty(attrs["proposal_id"], ev.ID), attrs["decision"])
	case "outbox.failed":
		if attrs["purpose"] == string(outbox.PurposeAlert) {
			s.onAlertFailed(ctx, attrs)
			return
		}
		s.onSendFailed(ctx, ev, attrs)
	case "outbox.sent":
		if attrs["purpose"] == string(outbox.PurposeAlert) {
			s.onAlertSent(ctx, attrs)
		}
		if attrs["row_id"] != "" {
			s.autoResolve(ctx, KindSendFailed, "outbox/"+attrs["row_id"], "auto")
		}
	}
}

// HandleEvent is a test-friendly seam for a single already-read bus frame.
// Production consumers use Consume and the persisted cursor.
func (s *Service) HandleEvent(ctx context.Context, ev store.StreamEvent) {
	s.handleEvent(ctx, ev)
}

func (s *Service) onSessionState(ctx context.Context, attrs map[string]string) {
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
		opts := RaiseOpts{
			Kind: KindWaiting, RunID: ep.runID, WorkspaceID: ep.ws, JobName: ep.job,
			TraceID: ep.trace, Title: "Agent waiting", Hint: "open attach",
		}
		// A pending approval turns the wait into an answerable item: the
		// captured question rides the body (D3) and the sweep mails it.
		if q := s.approvalQuestion(ctx, ep.runID); q != "" {
			opts.Body, opts.Hint = q, "Reply approve or deny by mail"
			if !s.opts.ApprovalDisabled {
				opts.EscalateAt = now.Add(s.opts.ApprovalAfter)
			}
		}
		if _, err := s.Raise(ctx, opts); err != nil {
			s.log.Warn("raise waiting", "run", ep.runID, "error", err)
		}
	}
}

// approvalQuestion returns the pending approval's question for runID, or "".
func (s *Service) approvalQuestion(ctx context.Context, runID string) string {
	if s.opts.ApprovalQuestion == nil {
		return ""
	}
	return s.opts.ApprovalQuestion(ctx, runID)
}

func (s *Service) onRunFailed(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	if attrs["owner"] != "" && attrs["kind"] == "job" && s.failureRouter != nil {
		handled, err := s.failureRouter.RouteFailure(ctx, RunFailure{
			RunID: ev.ID, WorkspaceID: attrs["workspace"], Job: attrs["job"],
			Owner: attrs["owner"], EndReason: attrs["end_reason"],
		})
		if err != nil {
			s.log.Warn("failure route", "run", ev.ID, "error", err)
		}
		if handled {
			return
		}
	}
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
	// A retry that succeeded, or a human marking a failed run complete,
	// clears the run_failed item for this job.
	if attrs["status"] != "complete" && attrs["outcome"] != "complete" {
		return
	}
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

func (s *Service) onMemoryInvalid(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindMemoryInvalid, RunID: firstNonEmpty(attrs["run_id"], ev.ID),
		WorkspaceID: attrs["workspace"], JobName: attrs["job"], TraceID: ev.TraceID,
		Body: attrs["issue"],
	})
	if err != nil {
		s.log.Warn("raise memory_invalid", "run", ev.ID, "error", err)
	}
}

func (s *Service) onConnectorUnhealthy(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	name := firstNonEmpty(attrs["connector"], ev.ID)
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindConnectorUnhealthy, Connector: name, TraceID: ev.TraceID,
		Title: "Connector unhealthy", Body: attrs["status"],
	})
	if err != nil {
		s.log.Warn("raise connector_unhealthy", "connector", name, "error", err)
	}
}

// onSendFailed raises one item per outbox row, so two failed sends stay two
// items. A failed alert row raises nothing: the route moves on, and an
// alert about an alert would loop (plan 2026-09-28 B.5).
func (s *Service) onSendFailed(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	if attrs["purpose"] == "alert" {
		return
	}
	rowID := firstNonEmpty(attrs["row_id"], ev.ID)
	title := "Email send failed"
	if attrs["kind"] == "channel" {
		title = "Channel send failed"
		if attrs["connector"] != "" {
			title = "Send through " + attrs["connector"] + " failed"
		}
	}
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindSendFailed, Connector: attrs["connector"], WorkspaceID: attrs["workspace"],
		Subject: "outbox/" + rowID, TraceID: ev.TraceID,
		Title: title, Body: attrs["error"],
	})
	if err != nil {
		s.log.Warn("raise send_failed", "row", rowID, "error", err)
	}
}

func (s *Service) onAuthMissing(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	if attrs["source"] == "logout" {
		return
	}
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindAuthMissing, Harness: firstNonEmpty(attrs["harness"], ev.ID),
		RunID: attrs["run_id"], TraceID: ev.TraceID, Title: "Auth missing",
	})
	if err != nil {
		s.log.Warn("raise auth_missing", "error", err)
	}
}

func (s *Service) onHarnessUpdated(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	if attrs["auth_ok"] != "true" && attrs["auth_ok"] != "1" {
		return
	}
	name := firstNonEmpty(attrs["harness"], ev.ID)
	if name == "" {
		return
	}
	s.autoResolve(ctx, KindAuthMissing, "harness/"+name, "auto")
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

// onApprovalCreated puts the captured question on the run's waiting item
// (D3): an already raised item gets the body and the mail deadline; a run
// with no item yet gets its dwell clock started, so the dwelt raise carries
// the question. The item itself stays the waiting kind; the UI joins it.
func (s *Service) onApprovalCreated(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	runID := firstNonEmpty(attrs["run_id"], ev.ID)
	if runID == "" {
		return
	}
	question := s.approvalQuestion(ctx, runID)
	if question == "" {
		return
	}
	item, err := s.findActive(ctx, KindWaiting, "run/"+runID)
	if err != nil {
		s.log.Warn("approval attach lookup", "run", runID, "error", err)
	}
	if item != nil {
		s.attachApproval(ctx, item, question)
		return
	}
	s.noteWaiting(runID, attrs)
}

// attachApproval writes the question onto a raised waiting item and arms the
// escalation sweep (plan M5): tray first, mail after the interval.
func (s *Service) attachApproval(ctx context.Context, item *InboxItem, question string) {
	esc := store.NullStr("")
	if !s.opts.ApprovalDisabled {
		esc = store.NullStr(s.opts.Now().Add(s.opts.ApprovalAfter).UTC().Format(time.RFC3339Nano))
	}
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE inbox_items SET body=?, hint=?,
			escalates_at=COALESCE(escalates_at, ?), revision=revision+1, updated_at=? WHERE id=?`,
			question, "Reply approve or deny by mail", esc, nowStr, item.ID)
		return err
	}, store.BusEvent{
		Topic: "inbox.updated", Entity: "inbox", EntityID: item.ID, Action: "updated",
		TraceID: item.TraceID, Revision: item.Revision + 1,
		Attrs: map[string]string{"kind": string(item.Kind), "status": string(item.Status)},
	})
	if err != nil {
		s.log.Warn("approval attach", "id", item.ID, "error", err)
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
	s.queueDeliver(ctx, deliverJob{opts: opts, id: id})
}

// queueDeliver runs a delivery on the delivery goroutine, or inline when the
// service does not run (tests, one-shot CLI).
func (s *Service) queueDeliver(ctx context.Context, job deliverJob) {
	if s.skipNotify() || !s.routeOn() {
		return
	}
	if !s.running.Load() {
		s.runDeliver(ctx, job)
		return
	}
	select {
	case s.deliverCh <- job:
	default:
		s.log.Warn("alert delivery queue full", "id", job.id)
	}
}

func (s *Service) skipNotify() bool {
	if s.opts.Shadow {
		return true
	}
	return s.st.IsDraining()
}

// sweepEscalations mails approval items whose tray interval elapsed (plan
// M5). Clearing escalates_at after the send is the durable mailed marker.
func (s *Service) sweepEscalations(ctx context.Context) {
	if s.skipNotify() {
		return
	}
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT `+inboxCols+` FROM inbox_items
		WHERE status != 'resolved' AND escalates_at IS NOT NULL AND escalates_at <= ?`, nowStr)
	if err != nil {
		s.log.Warn("escalation sweep", "error", err)
		return
	}
	defer rows.Close()
	var due []InboxItem
	for rows.Next() {
		item, err := scanInbox(rows, true)
		if err != nil {
			s.log.Warn("escalation scan", "error", err)
			return
		}
		due = append(due, *item)
	}
	if err := rows.Err(); err != nil {
		s.log.Warn("escalation sweep", "error", err)
		return
	}
	for _, item := range due {
		if s.routeOn() {
			s.deliver(ctx, raiseOptsFromItem(item), item.ID)
		}
		if _, err := s.st.DB().ExecContext(ctx,
			`UPDATE inbox_items SET escalates_at=NULL WHERE id=?`, item.ID); err != nil {
			s.log.Warn("escalation clear", "id", item.ID, "error", err)
		}
	}
}

// emailOn reports whether the native channel is wired and configured.
func (s *Service) emailOn() bool {
	return s.opts.Email != nil && s.opts.Email.Configured()
}

// routeOn reports whether alert delivery is wired.
func (s *Service) routeOn() bool {
	return s.opts.Router != nil && s.opts.Outbox != nil
}

// deliver resolves the route of the alert and starts it at step 0.
func (s *Service) deliver(ctx context.Context, opts RaiseOpts, id string) {
	s.runDeliver(ctx, deliverJob{opts: opts, id: id})
}

// runDeliver sends the first usable step at or after job.from. A step with a
// problem is skipped. The outbox retries a step; its outbox.failed frame
// moves the route to the next step (onAlertFailed). When no usable step is
// left, the item stays in the inbox.
func (s *Service) runDeliver(ctx context.Context, job deliverJob) {
	if !s.routeOn() {
		return
	}
	sc := job.opts.scope()
	r := s.opts.Router.Resolve(ctx, sc)
	if job.route != nil {
		r = s.opts.Router.ResolveRoute(ctx, sc, job.route)
	}
	plain := r.Plain()
	for i := job.from; i < len(r.Steps); i++ {
		st := r.Steps[i]
		if st.Problem != "" {
			s.appendDelivered(ctx, job.opts, job.id, st.Via, i, "skipped:"+st.Problem, "", "")
			continue
		}
		row, msgID, err := s.sendStep(ctx, job.opts, job.id, st, i, plain)
		if err != nil {
			s.log.Warn("alert.delivered", "channel", st.Via, "index", i, "error", err)
			s.appendDelivered(ctx, job.opts, job.id, st.Via, i, "failed", msgID, err.Error())
			continue
		}
		s.setDelivery(ctx, job.id, DeliveryPending)
		// A first attempt now keeps the latency low; the worker retries.
		if _, err := s.opts.Outbox.SendNow(ctx, row.ID); err != nil {
			s.log.Warn("alert first attempt", "row", row.ID, "error", err)
		}
		return
	}
	state := DeliveryInboxOnly
	if job.from > 0 {
		state = DeliveryFailed
	}
	s.setDelivery(ctx, job.id, state)
	s.appendDelivered(ctx, job.opts, job.id, "inbox", len(r.Steps), state, "", "")
}

// sendStep writes the outbox row of one usable step.
func (s *Service) sendStep(ctx context.Context, opts RaiseOpts, id string, st notify.ResolvedStep, i int, route []notify.Step) (outbox.Row, string, error) {
	if st.Via == notify.ViaEmail {
		if !s.emailOn() {
			return outbox.Row{}, "", fmt.Errorf("email is not configured")
		}
		return s.opts.Email.SendAlert(ctx, email.AlertMail{
			InboxID: id, Subject: opts.emailSubject(), Text: s.mailText(ctx, opts, id),
			To: st.Targets, RouteStep: i, Route: route,
		})
	}
	step := i
	row, err := s.opts.Outbox.Enqueue(ctx, outbox.Row{
		Kind: outbox.KindChannel, Purpose: outbox.PurposeAlert, WorkspaceID: opts.WorkspaceID,
		Connector: st.Via, Target: st.To, Body: opts.pushText(id), Env: opts.notifyEnv(id),
		InboxID: id, DedupeKey: fmt.Sprintf("alert/%s/%d", id, i), RouteStep: &step, Route: route,
	})
	return row, "", err
}

// mailText is the alert mail body. A proposal mail leads with the diff.
func (s *Service) mailText(ctx context.Context, opts RaiseOpts, id string) string {
	text := opts.emailText(id)
	if opts.Kind == KindAgentProposal && s.opts.ProposalContent != nil && strings.HasPrefix(opts.Subject, "proposal/") {
		if base, proposed, ok := s.opts.ProposalContent(ctx, strings.TrimPrefix(opts.Subject, "proposal/")); ok {
			text = proposals.TruncateDiff(proposals.UnifiedDiff(base, proposed), 200) + "\n\n" + text
		}
	}
	return text
}

// onAlertFailed records the failed step and moves the route to the next
// step. It raises no alert: an alert about an alert would loop.
func (s *Service) onAlertFailed(ctx context.Context, attrs map[string]string) {
	row, item, ok := s.alertRow(ctx, attrs)
	if !ok {
		return
	}
	opts := raiseOptsFromItem(*item)
	via := cmp.Or(row.Connector, notify.ViaEmail)
	s.appendDelivered(ctx, opts, item.ID, via, *row.RouteStep, "failed", "", row.ErrorMessage)
	if item.Status == InboxResolved {
		return
	}
	s.queueDeliver(ctx, deliverJob{opts: opts, id: item.ID, route: row.Route, from: *row.RouteStep + 1})
}

// onAlertSent records the sent step. The route stops here.
func (s *Service) onAlertSent(ctx context.Context, attrs map[string]string) {
	row, item, ok := s.alertRow(ctx, attrs)
	if !ok {
		return
	}
	via, msgID := row.Connector, ""
	if via == "" {
		via, msgID = notify.ViaEmail, item.EmailMessageID
	}
	s.setDelivery(ctx, item.ID, DeliveryDelivered)
	s.appendDelivered(ctx, raiseOptsFromItem(*item), item.ID, via, *row.RouteStep, "ok", msgID, "")
}

// alertRow loads the outbox row and the inbox item of an alert frame.
func (s *Service) alertRow(ctx context.Context, attrs map[string]string) (outbox.Row, *InboxItem, bool) {
	if s.opts.Outbox == nil || attrs["row_id"] == "" {
		return outbox.Row{}, nil, false
	}
	row, err := s.opts.Outbox.Get(ctx, attrs["row_id"])
	if err != nil || row.InboxID == "" || row.RouteStep == nil {
		return outbox.Row{}, nil, false
	}
	item, err := s.Get(ctx, row.InboxID)
	if err != nil || item == nil {
		return outbox.Row{}, nil, false
	}
	return row, item, true
}

// setDelivery writes the delivery state of an item.
func (s *Service) setDelivery(ctx context.Context, id, state string) {
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE inbox_items SET delivery=?, revision=revision+1, updated_at=? WHERE id=?`,
			state, nowStr, id)
		return err
	}, store.BusEvent{
		Topic: "inbox.updated", Entity: "inbox", EntityID: id, Action: "updated",
		Attrs: map[string]string{"delivery": state},
	}); err != nil {
		s.log.Warn("inbox delivery", "id", id, "error", err)
	}
}

func (s *Service) appendDelivered(ctx context.Context, opts RaiseOpts, id, channel string, index int, status, msgID, errMsg string) {
	attrs := alertAttrs(opts, map[string]string{
		"kind": string(opts.Kind), "channel": channel,
		"index": strconv.Itoa(index), "status": status,
	})
	if msgID != "" {
		attrs["message_id"] = msgID
	}
	if errMsg != "" {
		attrs["error"] = sanitizeText(errMsg)
	}
	if _, aerr := s.st.Append(ctx, store.BusEvent{
		Topic: "alert.delivered", Entity: "alert", EntityID: id, Action: "updated",
		Attrs: attrs,
	}); aerr != nil {
		s.log.Warn("bus append", "topic", "alert.delivered", "error", aerr)
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

// EmailInboxItem records the low-severity item for one inbound-mail
// event: a misrouted tag from a trusted sender, or a spoof suspect
// (plan A.8, A.10). One open item per subject; a repeat bumps the
// revision. The kind registry stays closed: kind is health, alert_kind
// is "email".
func (s *Service) EmailInboxItem(ctx context.Context, d email.RouteDecision, msg email.IngressMessage) (string, error) {
	var subject, title, body, hint string
	if d.Spoof() {
		subject = "email/spoof/" + msg.From
		title = "Unverified mail claims a trusted address"
		body = "Mail from " + msg.From + " with subject \"" + sanitizeText(msg.Subject) +
			"\" failed the DMARC check. The message was dropped."
		hint = "The sender's mail server does not sign for this address. Check the DMARC setup of the sender."
	} else {
		subject = "email/misrouted/" + msg.Tag
		title = "Email misrouted to +" + msg.Tag
		body = "Mail to +" + msg.Tag + " was not delivered. No agent has this ID, or its email.accept list is empty."
		hint = "Create the agent, or add the sender to email.accept."
	}
	item, err := s.findActiveBySubject(ctx, subject)
	if err != nil {
		return "", fmt.Errorf("email inbox lookup %s: %w", subject, err)
	}
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	if item != nil {
		if _, err := s.st.DB().ExecContext(ctx, `UPDATE inbox_items SET title=?, body=?, hint=?,
			email_message_id=?, revision=revision+1, updated_at=? WHERE id=?`,
			title, body, hint, msg.ID, nowStr, item.ID); err != nil {
			return "", fmt.Errorf("email inbox update %s: %w", subject, err)
		}
		return item.ID, nil
	}
	id := ulid.Make().String()
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_items (
			id, kind, status, subject, title, body, hint, workspace_id, alert_kind,
			trace_id, email_message_id, revision, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
			id, string(InboxHealth), string(InboxUnread), subject, title, body, hint,
			store.NullStr(d.WorkspaceID), "email", store.NullStr(d.TraceID), msg.ID, nowStr, nowStr)
		return err
	}, store.BusEvent{
		Topic: "inbox.created", Entity: "inbox", EntityID: id, Action: "created",
		TraceID: d.TraceID,
		Attrs:   map[string]string{"kind": string(InboxHealth), "status": "unread"},
	})
	if err != nil {
		return "", fmt.Errorf("email inbox insert %s: %w", subject, err)
	}
	return id, nil
}

// AmbiguousReplyItem records the low-severity item for one approval reply
// that named both decisions (plan Part P). The reply became a run message;
// the approval still waits. One open item per sender; a repeat bumps the
// revision. Kind is health, alert_kind is "email", like EmailInboxItem.
func (s *Service) AmbiguousReplyItem(ctx context.Context, runID string, d email.RouteDecision, msg email.IngressMessage) (string, error) {
	subject := "email/ambiguous/" + msg.From
	title := "Approval reply names both decisions"
	body := "Mail from " + msg.From + " with subject \"" + sanitizeText(msg.Subject) +
		"\" named both approve and deny. The reply became a run message; the approval still waits."
	hint := "Answer again with one decision word: approve or deny."
	item, err := s.findActiveBySubject(ctx, subject)
	if err != nil {
		return "", fmt.Errorf("ambiguous inbox lookup %s: %w", subject, err)
	}
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	if item != nil {
		if _, err := s.st.DB().ExecContext(ctx, `UPDATE inbox_items SET title=?, body=?, hint=?,
			email_message_id=?, revision=revision+1, updated_at=? WHERE id=?`,
			title, body, hint, msg.ID, nowStr, item.ID); err != nil {
			return "", fmt.Errorf("ambiguous inbox update %s: %w", subject, err)
		}
		return item.ID, nil
	}
	id := ulid.Make().String()
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_items (
			id, kind, status, subject, title, body, hint, run_id, workspace_id, alert_kind,
			trace_id, email_message_id, revision, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
			id, string(InboxHealth), string(InboxUnread), subject, title, body, hint,
			store.NullStr(runID), store.NullStr(d.WorkspaceID), "email",
			store.NullStr(d.TraceID), msg.ID, nowStr, nowStr)
		return err
	}, store.BusEvent{
		Topic: "inbox.created", Entity: "inbox", EntityID: id, Action: "created",
		TraceID: d.TraceID,
		Attrs:   map[string]string{"kind": string(InboxHealth), "status": "unread"},
	})
	if err != nil {
		return "", fmt.Errorf("ambiguous inbox insert %s: %w", subject, err)
	}
	return id, nil
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

// CollectDoctorFindings is the system doctor snapshot from the live store.
func (s *Service) CollectDoctorFindings() []DoctorFinding {
	return append(collectDoctorFindings(s.st, s.opts.Actions), s.notifyFindings(context.Background())...)
}

// notifyFindings reports each layer whose route holds a problem step, and a
// configured email channel with no address to mail.
func (s *Service) notifyFindings(ctx context.Context) []DoctorFinding {
	if s.opts.Router == nil {
		return nil
	}
	scopes := []notify.Scope{{}}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT w.id, COALESCE(j.name,'') FROM workspaces w
		LEFT JOIN job_index j ON j.workspace_id=w.id AND j.active=1 WHERE w.orphaned=0 ORDER BY w.id, j.name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	seenWS := map[string]bool{}
	for rows.Next() {
		var ws, job string
		if rows.Scan(&ws, &job) != nil {
			continue
		}
		if !seenWS[ws] {
			seenWS[ws] = true
			scopes = append(scopes, notify.Scope{WorkspaceID: ws})
		}
		if job != "" {
			scopes = append(scopes, notify.Scope{WorkspaceID: ws, Job: job})
		}
	}
	var out []DoctorFinding
	seen := map[string]bool{}
	noContact := s.emailOn()
	for _, sc := range scopes {
		r := s.opts.Router.Resolve(ctx, sc)
		if len(r.Contact) > 0 {
			noContact = false
		}
		// Report a route once, at the layer that sets it.
		key := string(r.Source)
		switch r.Source {
		case notify.LayerJob, notify.LayerAgent:
			key += "/" + sc.WorkspaceID + "/" + sc.Job
		case notify.LayerWorkspace:
			key += "/" + sc.WorkspaceID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		for i, st := range r.Steps {
			if st.Problem == "" {
				continue
			}
			out = append(out, DoctorFinding{
				ID: "notify-route-" + strings.ReplaceAll(key, "/", "-"), Severity: "warn", Code: "notify_route_invalid",
				Message: fmt.Sprintf("notify route of %s: step %d (%s) is skipped: %s", key, i, st.Via, st.Problem),
			})
			break
		}
	}
	if noContact {
		out = append(out, DoctorFinding{
			ID: "notify-no-contact", Severity: "info", Code: "notify_no_contact",
			Message: "email is on, but no contact and no email.admins are set; alerts stay in the inbox",
		})
	}
	return out
}

// collectDoctorFindings is the store-injected implementation so doctor never
// has to race the daemon with a second migration-capable connection.
func collectDoctorFindings(st *store.Store, actions ActionsChecker) []DoctorFinding {
	var findings []DoctorFinding
	if st == nil {
		findings = append(findings, DoctorFinding{
			ID: "db-missing", Severity: "error", Code: "db_missing",
			Message: "trebi.db unavailable",
		})
		return findings
	}
	if _, err := os.Stat(st.Path()); err != nil {
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
	findings = append(findings, CollectAgentDoctorFindings(st, actions)...)
	if findings == nil {
		return []DoctorFinding{}
	}
	return findings
}

// raiseOptsFromItem rebuilds the delivery context one stored item carries.
func raiseOptsFromItem(item InboxItem) RaiseOpts {
	return RaiseOpts{
		Kind: Kind(item.AlertKind), Subject: item.Subject, Title: item.Title,
		Body: item.Body, Hint: item.Hint, RunID: item.RunID,
		WorkspaceID: item.Workspace, JobName: item.Job, TraceID: item.TraceID,
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

// sanitizeText strips shell-metacharacters and control chars from alert text
// at raise time (runtime.md §6.5).
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
