package alerts

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/proposals"
	"github.com/trebi-ai/trebi/internal/requests"
	"github.com/trebi-ai/trebi/internal/state"
	"github.com/trebi-ai/trebi/internal/store"
)

const (
	busCursorFlag    = "alerts.bus_seq"
	usageWarnedFlag  = "alerts.usage_warned."
	memoryStreakFlag = "alerts.memory_streak."
	// memoryAlertAfter is the count of failed runs in a row that raises
	// memory_invalid. A shorter streak is a fault that the next run fixes.
	memoryAlertAfter = 3
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
	"memory.valid",
	"connector.unhealthy",
	"connector.recovered",
	"connector.auth.required",
	"connector.auth.completed",
	"connector.status",
	"connector.subscriptions.synced",
	"auth.probe.failed",
	"harness.updated",
	"harness.limit_exceeded",
	"harness.limit_cleared",
	"approval.created",
	"approval.decided",
	"proposal.created",
	"proposal.decided",
	"skill.created",
	"skill.updated",
	"skill.moved",
	"skill.deleted",
	"request.created",
	"request.updated",
	"request.answered",
	"request.expired",
	"request.dismissed",
	"request.cancelled",
	"request.delivery_failed",
	"outbox.failed",
	"outbox.sent",
	"job.updated",
	"agent.updated",
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

	s.resolveHarnessesOff(ctx)
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
// items, applies the job health rules, syncs doctor, and prunes old Done items.
func (s *Service) Tick(ctx context.Context) {
	s.Consume(ctx)
	s.sweepEscalations(ctx)
	s.raiseDweltWaiting(ctx)
	s.sweepHealth(ctx)
	s.checkUsage(ctx)
	s.prune(ctx)
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
	title := attrs["agent_name"]
	body := attrs["note"]
	var keys []string
	if raw := attrs["protected_keys"]; raw != "" {
		var parsed []string
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			keys = parsed
		}
	}
	stats := fmt.Sprintf("%s lines added, %s lines removed.", cmp.Or(attrs["added"], "0"), cmp.Or(attrs["removed"], "0"))
	if len(keys) > 0 {
		stats += " It also changes protected settings: " + strings.Join(keys, ", ") + "."
	}
	opts := RaiseOpts{
		Kind: KindAgentProposal, Subject: "proposal/" + id, WorkspaceID: attrs["workspace"],
		JobID: attrs["agent_id"], RunID: attrs["run_id"], TraceID: ev.TraceID,
		Hint: "Reply approve or deny to this email. Or review it in Trebi.",
	}
	if err := s.named(ctx, &opts); err != nil {
		s.log.Warn("raise proposal", "proposal", id, "error", err)
		return
	}
	who := cmp.Or(opts.names.Owner(), title, "An agent")
	opts.Body = who + " wants to change its instructions."
	if body != "" {
		opts.Body += " It says why: " + body
	}
	opts.Body += " " + stats
	_, err := s.Raise(ctx, opts)
	if err != nil {
		s.log.Warn("raise proposal", "proposal", id, "error", err)
	}
}

// onSkillCreated raises the inbox item of a skill that a run made. A skill
// that a person or the assistant made raises nothing.
func (s *Service) onSkillCreated(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	if attrs["actor_kind"] != "agent" && attrs["actor_kind"] != "job" {
		return
	}
	parts := strings.Split(ev.ID, "/")
	name := parts[len(parts)-1]
	pending := attrs["status"] == "pending"
	hint := "Open it, or change its scope to share it wider."
	if pending {
		hint = "Runs do not get it until you approve it."
	}
	opts := RaiseOpts{
		Kind: KindSkillCreated, Subject: "skill/" + ev.ID, WorkspaceID: attrs["workspace"],
		JobName: attrs["job"], RunID: attrs["run_id"], TraceID: ev.TraceID, Hint: hint, Pending: pending,
	}
	if err := s.named(ctx, &opts); err != nil {
		s.log.Warn("raise skill", "skill", ev.ID, "error", err)
		return
	}
	who := cmp.Or(opts.names.Owner(), "A run")
	opts.Title = who + " made a new skill: " + name
	opts.Body = strings.TrimSpace(who + " made the skill " + name + ". " + attrs["note"] + " " + skillScopeText(attrs["scope"], opts.names))
	_, err := s.Raise(ctx, opts)
	if err != nil {
		s.log.Warn("raise skill", "skill", ev.ID, "error", err)
	}
}

// skillScopeText tells who gets a skill, in plain words.
func skillScopeText(scope string, n Names) string {
	switch scope {
	case "workspace":
		if n.Workspace != "" {
			return "It is for all agents in workspace " + n.Workspace + "."
		}
		return "It is for all agents in the workspace."
	case "agent":
		return "It is for " + cmp.Or(n.Agent, "this agent") + " only."
	case "job":
		return "It is for " + cmp.Or(n.Job, "this job") + " only."
	case "global", "user":
		return "It is for all agents on this machine."
	}
	return ""
}

// onRequestCreated raises the inbox item of a new request. It sits in the
// tray first and mails after ApprovalAfter, as an approval does.
func (s *Service) onRequestCreated(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	opts := RaiseOpts{
		Kind: KindRequest, RequestID: ev.ID, Title: attrs["title"], RunID: attrs["run_id"],
		WorkspaceID: attrs["workspace_id"], JobID: attrs["job_id"], JobName: attrs["job_name"], TraceID: ev.TraceID,
		TrayOnly: s.opts.ApprovalDisabled,
	}
	if !opts.TrayOnly {
		opts.EscalateAt = s.opts.Now().Add(s.opts.ApprovalAfter)
	}
	if r, ok := s.request(ctx, ev.ID); ok {
		opts.Title, opts.Body = r.Title, requestBody(r)
	}
	if err := s.named(ctx, &opts); err != nil {
		s.log.Warn("raise request", "request", ev.ID, "error", err)
		return
	}
	opts.Title = requestTitle(opts.names, opts.Title)
	if _, err := s.Raise(ctx, opts); err != nil {
		s.log.Warn("raise request", "request", ev.ID, "error", err)
	}
}

// onRequestUpdated sets the body of the open item after a form joins the
// request. The item keeps its escalation time.
func (s *Service) onRequestUpdated(ctx context.Context, ev store.StreamEvent) {
	r, ok := s.request(ctx, ev.ID)
	if !ok {
		return
	}
	item, err := s.findActive(ctx, KindRequest, "request/"+ev.ID)
	if err != nil || item == nil {
		return
	}
	opts := raiseOptsFromItem(*item)
	if err := s.named(ctx, &opts); err != nil {
		s.log.Warn("update request item", "request", ev.ID, "error", err)
	}
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE inbox_items SET title=?, body=?, revision=revision+1, updated_at=? WHERE id=?`,
			sanitizeText(requestTitle(opts.names, r.Title)), sanitizeText(requestBody(r)), nowStr, item.ID)
		return err
	}, inboxUpdated(OpUpdate, []string{item.ID}, ""))
	if err != nil {
		s.log.Warn("update request item", "request", ev.ID, "error", err)
	}
}

// requestTitle is "<owner> asks: <title>".
func requestTitle(n Names, title string) string {
	if title == "" {
		return cmp.Or(n.Owner(), "A job") + " asks you to decide"
	}
	return cmp.Or(n.Owner(), "A job") + " asks: " + title
}

// requestBody is the one-line inbox body of a request: the summary, else
// the summary of its one form, else the form titles.
func requestBody(r requests.Request) string {
	switch {
	case r.Summary != "" || len(r.Forms) == 0:
		return r.Summary
	case len(r.Forms) == 1:
		return r.Forms[0].Summary
	}
	titles := make([]string, len(r.Forms))
	for i, f := range r.Forms {
		titles[i] = f.Title
	}
	return strings.Join(titles, "; ")
}

// onRequestUndelivered raises a failure item when an answer cannot reach
// the job.
func (s *Service) onRequestUndelivered(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	opts := RaiseOpts{
		Kind: KindRequestUndelivered, RequestID: ev.ID, RunID: attrs["run_id"],
		WorkspaceID: attrs["workspace_id"], JobID: attrs["job_id"], JobName: attrs["job_name"], TraceID: ev.TraceID,
	}
	if err := s.named(ctx, &opts); err != nil {
		s.log.Warn("raise request delivery", "request", ev.ID, "error", err)
		return
	}
	opts.Body = "Trebi could not give your answer to " + cmp.Or(opts.names.Owner(), "the job") + "."
	if r, ok := s.request(ctx, ev.ID); ok && r.DeliveryError != "" {
		opts.Body += " Details: " + r.DeliveryError
	}
	opts.Hint = "Open the question to send the answer again."
	if _, err := s.Raise(ctx, opts); err != nil {
		s.log.Warn("raise request delivery", "request", ev.ID, "error", err)
	}
}

func (s *Service) request(ctx context.Context, id string) (requests.Request, bool) {
	if s.opts.Request == nil {
		return requests.Request{}, false
	}
	r, err := s.opts.Request(ctx, id)
	if err != nil {
		s.log.Warn("request lookup", "request", id, "error", err)
		return requests.Request{}, false
	}
	return r, true
}

// requestMail is the mail text of a request: who asks, what to decide,
// then how to reply. Only a request with one form takes an answer by mail.
func requestMail(r requests.Request, who string) string {
	who = cmp.Or(who, "A job")
	var b strings.Builder
	b.WriteString(who + " asks you to decide.\n\n")
	b.WriteString(r.Title + "\n")
	if r.Summary != "" {
		b.WriteString(r.Summary + "\n")
	}
	b.WriteString("\n")
	for i, f := range r.Forms {
		if len(r.Forms) > 1 {
			fmt.Fprintf(&b, "%d. %s\n\n", i+1, f.Title)
			if f.Summary != "" {
				b.WriteString(f.Summary + "\n\n")
			}
		}
		formMail(&b, f)
	}
	if len(r.Forms) != 1 {
		b.WriteString("Answer in Trebi.")
		return b.String()
	}
	f := r.Forms[0]
	switch {
	case len(f.Payload.Options) > 0:
		b.WriteString("Reply to this email with a number or a name on the first line.")
	case f.Payload.Draft != nil:
		b.WriteString("Reply approve or reject on the first line. To change the draft, open it in Trebi.")
	default:
		b.WriteString("Reply approve or reject on the first line.")
	}
	b.WriteString(" Any other reply goes to " + who + " as a message.")
	return b.String()
}

// formMail writes the draft, the options, or the fields of one form.
func formMail(b *strings.Builder, f requests.Form) {
	switch {
	case f.Payload.Draft != nil:
		b.WriteString("Draft:\n\n" + f.Payload.Draft.Text + "\n\n")
	case len(f.Payload.Options) > 0:
		for i, o := range f.Payload.Options {
			fmt.Fprintf(b, "%d. %s\n", i+1, o.Label)
		}
		b.WriteString("\n")
	case len(f.Payload.Schema) > 0:
		if sc, err := requests.ParseSchema(f.Payload.Schema); err == nil {
			b.WriteString("Fields: " + strings.Join(sc.Names(), ", ") + "\n\n")
		}
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
	case "job.updated", "agent.updated":
		if attrs["archived"] == "true" {
			if _, err := s.CloseForJobs(ctx, []string{ev.ID}, "archived"); err != nil {
				s.log.Warn("close items of archived job", "job", ev.ID, "error", err)
			}
		}
	case "session.state":
		s.onSessionState(ctx, attrs)
	case "run.failed":
		s.onRunFailed(ctx, ev, attrs)
		s.onRunEnded(ctx, ev, attrs)
	case "run.updated":
		s.onRunUpdated(ctx, ev, attrs)
		s.onRunEnded(ctx, ev, attrs)
	case "run.resume_exhausted":
		s.onResumeExhausted(ctx, ev, attrs)
	case "memory.invalid":
		s.onMemoryInvalid(ctx, ev, attrs)
	case "memory.valid":
		s.onMemoryValid(ctx, attrs)
	case "connector.unhealthy":
		s.onConnectorUnhealthy(ctx, ev, attrs)
	case "connector.recovered":
		s.autoResolve(ctx, KindConnectorUnhealthy, "connector/"+ev.ID, "auto")
	case "connector.auth.required":
		s.onConnectorAuthRequired(ctx, ev, attrs)
	case "connector.update.available":
		name := ev.ID
		if _, err := s.Raise(ctx, RaiseOpts{
			Kind: KindConnectorUpdate, Connector: name, TraceID: ev.TraceID,
			Body: fmt.Sprintf("Version %s is available. The connector uses %s.", attrs["version"], attrs["current"]),
		}); err != nil {
			s.log.Warn("raise connector_update_available", "connector", name, "error", err)
		}
	case "connector.updated.version":
		if attrs["workspace"] == "" {
			s.autoResolve(ctx, KindConnectorUpdate, "connector/"+ev.ID, "auto")
		}
	case "connector.status":
		if attrs["state"] == "connected" {
			s.autoResolve(ctx, KindConnectorAuthRequired, "connector/"+firstNonEmpty(attrs["instance"], ev.ID), "auto")
		}
	case "connector.auth.completed":
		s.autoResolve(ctx, KindConnectorAuthRequired, "connector/"+firstNonEmpty(attrs["instance"], ev.ID), "auto")
	case "connector.subscriptions.synced":
		s.onSubscriptionsSynced(ctx, ev, attrs)
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
	case "skill.created":
		s.onSkillCreated(ctx, ev, attrs)
	case "skill.updated":
		if attrs["status"] == "active" {
			s.autoResolve(ctx, KindSkillCreated, "skill/"+ev.ID, "approved")
		}
	case "skill.moved":
		s.autoResolve(ctx, KindSkillCreated, "skill/"+attrs["from"], "moved")
	case "skill.deleted":
		s.autoResolve(ctx, KindSkillCreated, "skill/"+ev.ID, "deleted")
	case "proposal.created":
		s.onProposalCreated(ctx, ev, attrs)
	case "proposal.decided":
		s.autoResolve(ctx, KindAgentProposal, "proposal/"+firstNonEmpty(attrs["proposal_id"], ev.ID), attrs["decision"])
	case "request.created":
		s.onRequestCreated(ctx, ev, attrs)
	case "request.updated":
		s.onRequestUpdated(ctx, ev)
	case "request.answered", "request.expired", "request.dismissed", "request.cancelled":
		s.autoResolve(ctx, KindRequest, "request/"+ev.ID, strings.TrimPrefix(ev.Topic, "request."))
		s.autoResolve(ctx, KindRequestReminder, "request-reminder/"+ev.ID, strings.TrimPrefix(ev.Topic, "request."))
	case "request.delivery_failed":
		s.onRequestUndelivered(ctx, ev, attrs)
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
		ws: attrs["workspace"], jobID: attrs["job_id"], jobName: attrs["job_name"], trace: attrs["trace_id"],
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
			Kind: KindWaiting, RunID: ep.runID, WorkspaceID: ep.ws, JobID: ep.jobID, JobName: ep.jobName,
			TraceID: ep.trace, Hint: "Open the session to see what it needs.",
		}
		if err := s.named(ctx, &opts); err != nil {
			s.log.Warn("raise waiting", "run", ep.runID, "error", err)
			continue
		}
		who := opts.names.Owner()
		opts.Body = capFirst(s.runOf(ctx, who, ep.runID)) + " waits for you since " + s.when(ep.since) + "."
		// A pending approval turns the wait into an answerable item: the
		// captured question rides the body (D3) and the sweep mails it.
		if q := s.approvalQuestion(ctx, ep.runID); q != "" {
			opts.Title, opts.Body = s.approvalText(who, q)
			opts.Hint = approvalHint(opts.Title)
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
	reason := s.reportedReason(ctx, ev.ID)
	if attrs["owner_id"] != "" && attrs["kind"] == "job" && s.failureRouter != nil {
		handled, err := s.failureRouter.RouteFailure(ctx, RunFailure{
			RunID: ev.ID, WorkspaceID: attrs["workspace"], JobID: attrs["job_id"], JobName: attrs["job_name"],
			OwnerID: attrs["owner_id"], EndReason: attrs["end_reason"], Reason: reason,
		})
		if err != nil {
			s.log.Warn("failure route", "run", ev.ID, "error", err)
		}
		if handled {
			return
		}
	}
	opts := RaiseOpts{
		Kind: KindRunFailed, RunID: ev.ID, WorkspaceID: attrs["workspace"],
		JobID: attrs["job_id"], JobName: attrs["job_name"], EndReason: attrs["end_reason"], TraceID: ev.TraceID,
		Harness: attrs["harness"],
	}
	if err := s.named(ctx, &opts); err != nil {
		s.log.Warn("raise run_failed", "run", ev.ID, "error", err)
		return
	}
	why := endReasonText(attrs["end_reason"])
	if reason != "" {
		why = "The agent said: " + reason
	}
	opts.Body = strings.TrimSpace(capFirst(s.runOf(ctx, opts.names.Owner(), ev.ID)) + " failed. " + why)
	_, err := s.Raise(ctx, opts)
	if err != nil {
		s.log.Warn("raise run_failed", "run", ev.ID, "error", err)
	}
}

// reportedReason is the reason of a run that reported it cannot do its task
// (runs.ClassReported), else "".
func (s *Service) reportedReason(ctx context.Context, runID string) string {
	var detail sql.NullString
	if s.st.DB().QueryRowContext(ctx, `SELECT detail_json FROM run_events WHERE run_id=? AND kind='error' ORDER BY id DESC LIMIT 1`,
		runID).Scan(&detail) != nil || !detail.Valid {
		return ""
	}
	var d struct {
		Message string `json:"message"`
		Class   string `json:"class"`
	}
	if json.Unmarshal([]byte(detail.String), &d) != nil || d.Class != "reported" {
		return ""
	}
	return d.Message
}

func (s *Service) onRunUpdated(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	// run.failed is only status failed. A timeout and a lost session end
	// with run.updated, so they take the same failure path here.
	if attrs["status"] == "timed_out" || (attrs["status"] == "exited" && attrs["end_reason"] == "session_gone") {
		s.onRunFailed(ctx, ev, attrs)
		return
	}
	// A retry that succeeded, or a human marking a failed run complete,
	// clears the run_failed item for this job.
	if attrs["status"] != "complete" && attrs["outcome"] != "complete" {
		return
	}
	subj := "job/" + attrs["workspace"] + "/" + attrs["job_id"]
	if attrs["workspace"] != "" && attrs["job_id"] != "" {
		s.autoResolve(ctx, KindRunFailed, subj, "auto")
	}
	s.autoResolve(ctx, KindResumeExhausted, "run/"+ev.ID, "auto")
}

func (s *Service) onResumeExhausted(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	opts := RaiseOpts{
		Kind: KindResumeExhausted, RunID: firstNonEmpty(attrs["run_id"], ev.ID),
		SessionID: attrs["session_id"], WorkspaceID: attrs["workspace"],
		JobID: attrs["job_id"], JobName: attrs["job_name"], TraceID: ev.TraceID,
		Hint: "Start it again if you still need it.",
	}
	if err := s.named(ctx, &opts); err != nil {
		s.log.Warn("raise resume_exhausted", "run", ev.ID, "error", err)
		return
	}
	opts.Body = "Trebi tried to continue " + s.runOf(ctx, opts.names.Owner(), opts.RunID) + ", but it could not. The run stopped."
	_, err := s.Raise(ctx, opts)
	if err != nil {
		s.log.Warn("raise resume_exhausted", "run", ev.ID, "error", err)
	}
}

// onMemoryInvalid raises one item per damaged memory, when the damage stays
// for memoryAlertAfter runs in a row. The body is for a person; the issue
// lines follow it as detail.
func (s *Service) onMemoryInvalid(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	if n := s.addMemoryFailure(attrs); n < memoryAlertAfter {
		s.log.Info("memory.invalid", "run", ev.ID, "job", firstNonEmpty(attrs["job_name"], attrs["job_id"]), "streak", n)
		return
	}
	noun, name := memoryOwner(firstNonEmpty(attrs["job_name"], attrs["job_id"]))
	opts := RaiseOpts{
		Kind: KindMemoryInvalid, RunID: firstNonEmpty(attrs["run_id"], ev.ID),
		WorkspaceID: attrs["workspace"], JobID: attrs["job_id"], JobName: attrs["job_name"], TraceID: ev.TraceID,
		Hint: "The assistant can repair it for you.",
	}
	if err := s.named(ctx, &opts); err != nil {
		s.log.Warn("raise memory_invalid", "run", ev.ID, "error", err)
		return
	}
	who := cmp.Or(opts.names.Owner(), path.Base(name))
	opts.Body = fmt.Sprintf("The memory of %s is damaged. Trebi cannot save what the %s learns until you repair it.", who, noun)
	if attrs["detail"] != "" {
		opts.Body += " Details: " + attrs["detail"]
	}
	_, err := s.Raise(ctx, opts)
	if err != nil {
		s.log.Warn("raise memory_invalid", "run", ev.ID, "error", err)
	}
}

// onMemoryValid closes the memory item of the job or agent that the run
// belongs to.
func (s *Service) onMemoryValid(ctx context.Context, attrs map[string]string) {
	o := RaiseOpts{Kind: KindMemoryInvalid, WorkspaceID: attrs["workspace"], JobID: attrs["job_id"], JobName: attrs["job_name"]}
	if o.WorkspaceID == "" || o.jobRef() == "" {
		return
	}
	if err := s.st.DeleteFlag(memoryStreakFlag + o.defaultSubject()); err != nil {
		s.log.Warn("memory streak", "subject", o.defaultSubject(), "error", err)
	}
	s.autoResolve(ctx, o.Kind, o.defaultSubject(), "auto")
}

// addMemoryFailure adds one failed run to the streak of the memory owner and
// returns the new count. A store error counts as a full streak, so that the
// alert is not lost.
func (s *Service) addMemoryFailure(attrs map[string]string) int {
	o := RaiseOpts{Kind: KindMemoryInvalid, WorkspaceID: attrs["workspace"], JobID: attrs["job_id"], JobName: attrs["job_name"]}
	key := memoryStreakFlag + o.defaultSubject()
	v, _, err := s.st.GetFlag(key)
	if err != nil {
		s.log.Warn("memory streak", "key", key, "error", err)
		return memoryAlertAfter
	}
	n, _ := strconv.Atoi(v) // a missing or bad value starts a new streak
	n++
	if err := s.st.SetFlag(key, strconv.Itoa(n)); err != nil {
		s.log.Warn("memory streak", "key", key, "error", err)
		return memoryAlertAfter
	}
	return n
}

// memoryOwner names the owner of a memory: "agent" and the name for an
// agent, or "job" and the job name. <agent>/<job> is a job.
func memoryOwner(jobName string) (noun, name string) {
	if job.IsAgentName(jobName) {
		return "agent", jobName
	}
	return "job", jobName
}

// onConnectorUnhealthy raises one item per outage: a crash loop, or an
// adapter status that stays bad for 10 minutes.
func (s *Service) onConnectorUnhealthy(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	name := firstNonEmpty(attrs["instance"], ev.ID)
	conn := cmp.Or(s.connectionTitle(name), attrs["title"], "The connection")
	var body string
	switch attrs["status"] {
	case "crashed":
		body = conn + " stopped without warning. Trebi starts it again. Jobs do not get its events until it works."
	case "connecting", "rate_limited", "error":
		body = unhealthyBody(attrs["status"], attrs["message"])
	default:
		body = conn + " does not work. Jobs do not get its events until it works again."
		if attrs["status"] != "" {
			body += " Details: " + attrs["status"]
		}
	}
	_, err := s.Raise(ctx, RaiseOpts{Kind: KindConnectorUnhealthy, Connector: name, TraceID: ev.TraceID, Body: body})
	if err != nil {
		s.log.Warn("raise connector_unhealthy", "connector", name, "error", err)
	}
}

// unhealthyBody says in simple words why a connection status stays bad.
func unhealthyBody(status, message string) string {
	lead := "It reports an error for 10 minutes."
	switch status {
	case "connecting":
		lead = "Trebi could not connect it for 10 minutes."
	case "rate_limited":
		lead = "The service limits it for 10 minutes or more."
	case "error":
		if message != "" {
			return message
		}
	}
	return strings.TrimSpace(lead + " " + message)
}

// onConnectorAuthRequired raises one item per episode. The subject is the
// connection id, so each connection has its own item. The body is the
// adapter message, with the accounts that need a login.
func (s *Service) onConnectorAuthRequired(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	instance := firstNonEmpty(attrs["instance"], ev.ID)
	title := s.connectionTitle(instance)
	body := "Trebi lost the login to " + cmp.Or(title, "the connection") + ". Until you log in again, jobs cannot use it."
	if attrs["accounts"] != "" {
		body += " Log in again to: " + attrs["accounts"] + "."
	}
	if detail := firstNonEmpty(attrs["message"], attrs["reason"]); detail != "" {
		from := "Details"
		if title != "" {
			from += " from " + title
		}
		body += " " + from + ": " + detail
	}
	_, err := s.Raise(ctx, RaiseOpts{Kind: KindConnectorAuthRequired, Connector: instance, TraceID: ev.TraceID, Body: body})
	if err != nil {
		s.log.Warn("raise connector_auth_required", "connector", instance, "error", err)
	}
}

// onSubscriptionsSynced raises or resolves each watch kind from its count.
func (s *Service) onSubscriptionsSynced(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	conn := cmp.Or(s.connectionTitle(ev.ID), "The connection")
	failed := conn + " cannot watch one of the items that you chose. Jobs do not get events from that item."
	if attrs["error"] != "" {
		failed += " Details: " + attrs["error"]
	}
	action := strings.TrimSpace(conn + " needs one step from you to watch an item: " + attrs["action"])
	for _, k := range []struct {
		kind  Kind
		count string
		body  string
	}{
		{KindConnectorSubscriptionFailed, attrs["errors"], failed},
		{KindConnectorSubscriptionAction, attrs["action_required"], action},
	} {
		if k.count == "" || k.count == "0" {
			s.autoResolve(ctx, k.kind, "connector/"+ev.ID, "auto")
			continue
		}
		if _, err := s.Raise(ctx, RaiseOpts{Kind: k.kind, Connector: ev.ID, TraceID: ev.TraceID, Body: k.body}); err != nil {
			s.log.Warn("raise "+string(k.kind), "connector", ev.ID, "error", err)
		}
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
	opts := RaiseOpts{
		Kind: KindSendFailed, Connector: attrs["connector"], WorkspaceID: attrs["workspace"],
		Subject: "outbox/" + rowID, TraceID: ev.TraceID,
	}
	var row outbox.Row
	if s.opts.Outbox != nil {
		row, _ = s.opts.Outbox.Get(ctx, rowID) // a missing row keeps the short text
	}
	if row.RunID != "" {
		var jobID sql.NullString
		if s.st.DB().QueryRowContext(ctx, `SELECT job_id FROM runs WHERE id=?`, row.RunID).Scan(&jobID) == nil {
			opts.JobID = jobID.String
		}
	}
	if err := s.named(ctx, &opts); err != nil {
		s.log.Warn("raise send_failed", "row", rowID, "error", err)
		return
	}
	opts.Title, opts.Body = sendFailedText(attrs["kind"] == "channel", row, opts.names, attrs["error"])
	_, err := s.Raise(ctx, opts)
	if err != nil {
		s.log.Warn("raise send_failed", "row", rowID, "error", err)
	}
}

// sendFailedText is the title and the body of a message that did not send.
func sendFailedText(channel bool, row outbox.Row, n Names, errText string) (title, body string) {
	what := "the mail"
	if channel {
		what = "the message"
		if n.Connection != "" {
			what += " through " + n.Connection
		}
	}
	if row.Subject != "" {
		what += " “" + row.Subject + "”"
	}
	title = "A message did not send"
	if who := n.Owner(); who != "" {
		title = who + " could not send " + what
		what += " from " + who
	}
	body = "Trebi could not send " + what
	if row.Recipients != "" && !channel {
		body += " to " + row.Recipients
	}
	body += ". Trebi does not try again."
	if errText != "" {
		from := "the mail server"
		if channel {
			from = cmp.Or(n.Connection, "the connection")
		}
		body += " Details from " + from + ": " + errText
	}
	return title, body
}

func (s *Service) onAuthMissing(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	if attrs["source"] == harness.SourceLogout || attrs["status"] == harness.AuthUnknown {
		return
	}
	name := firstNonEmpty(attrs["harness"], ev.ID)
	if !s.harnessOn(name) {
		return
	}
	label := harness.Label(name)
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindAuthMissing, Harness: name, RunID: attrs["run_id"], TraceID: ev.TraceID,
		Title: label + " needs you to log in",
		Body:  authMissingBody(label, attrs["status"], s.jobsOnHarness(ctx, name)),
	})
	if err != nil {
		s.log.Warn("raise auth_missing", "error", err)
	}
}

// authMissingBody is the plain reason of a failed login check and the
// number of jobs that wait for the login.
func authMissingBody(label, status string, jobs int) string {
	reason := "Trebi cannot find a login for " + label + "."
	if status == harness.AuthExpired {
		reason = "The login for " + label + " expired."
	}
	switch jobs {
	case 0:
		return reason
	case 1:
		return reason + " 1 job uses " + label + " and waits for the login."
	}
	return fmt.Sprintf("%s %d jobs use %s and wait for the login.", reason, jobs, label)
}

// jobsOnHarness counts the active jobs that run on name, set in the job or
// through the default harness.
func (s *Service) jobsOnHarness(ctx context.Context, name string) int {
	def := ""
	if cfg, err := config.Load(); err == nil {
		def = cfg.Defaults.Command
	}
	var n int
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM job_index
		WHERE active=1 AND retired_at IS NULL AND id NOT IN (`+store.ArchivedJobIDsSQL+`) AND (harness=? OR (COALESCE(harness,'')='' AND ?=?))`,
		name, def, name).Scan(&n); err != nil {
		return 0
	}
	return n
}

func (s *Service) onHarnessUpdated(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	name := firstNonEmpty(attrs["harness"], ev.ID)
	if name == "" {
		return
	}
	if attrs["enabled"] == "false" {
		s.resolveHarness(ctx, name)
		return
	}
	switch attrs["update_ok"] {
	case "false":
		app := appLabel(name)
		body := fmt.Sprintf("Trebi could not update %s to %s. It uses %s again, so your jobs still run.", app, attrs["update_to"], attrs["update_from"])
		if attrs["update_error"] != "" {
			body += " Details: " + attrs["update_error"]
		}
		_, err := s.Raise(ctx, RaiseOpts{
			Kind: KindHarnessUpdateFailed, Harness: name, TraceID: ev.TraceID,
			Title: app + " did not update to " + attrs["update_to"], Body: body,
		})
		if err != nil {
			s.log.Warn("raise harness_update_failed", "error", err)
		}
	case "true":
		s.autoResolve(ctx, KindHarnessUpdateFailed, "harness-update/"+name, "auto")
	}
	if attrs["auth_ok"] != "true" && attrs["auth_ok"] != "1" {
		return
	}
	s.autoResolve(ctx, KindAuthMissing, "harness/"+name, "auto")
}

func (s *Service) harnessOn(name string) bool {
	return s.opts.HarnessOn == nil || s.opts.HarnessOn(name)
}

// resolveHarness closes the open alerts of a harness that is off.
func (s *Service) resolveHarness(ctx context.Context, name string) {
	for _, k := range []Kind{KindAuthMissing, KindLimitExceeded, KindUsageWarning} {
		s.autoResolve(ctx, k, "harness/"+name, "auto")
	}
	s.autoResolve(ctx, KindHarnessUpdateFailed, "harness-update/"+name, "auto")
}

// resolveHarnessesOff closes the alerts of each harness that is off. It
// covers a config change made while the daemon was down.
func (s *Service) resolveHarnessesOff(ctx context.Context) {
	if s.opts.HarnessOn == nil {
		return
	}
	for _, name := range config.FirstClassTools() {
		if !s.opts.HarnessOn(name) {
			s.resolveHarness(ctx, name)
		}
	}
}

func (s *Service) onLimitExceeded(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	name := firstNonEmpty(attrs["harness"], ev.ID)
	app := appLabel(name)
	body := app + " reached its usage limit. Jobs that use " + app + " wait until the limit resets"
	if u := s.usage(ctx, name); !u.resets.IsZero() {
		body += " " + s.at(u.resets)
	}
	_, err := s.Raise(ctx, RaiseOpts{
		Kind: KindLimitExceeded, Harness: name, RunID: attrs["run_id"], TraceID: ev.TraceID, Body: body + ".",
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
	opts := raiseOptsFromItem(*item)
	if err := s.named(ctx, &opts); err != nil {
		s.log.Warn("approval attach names", "id", item.ID, "error", err)
	}
	title, body := s.approvalText(opts.names.Owner(), question)
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE inbox_items SET title=?, body=?, hint=?,
			escalates_at=COALESCE(escalates_at, ?), revision=revision+1, updated_at=? WHERE id=?`,
			sanitizeText(title), sanitizeText(body), approvalHint(title), esc, nowStr, item.ID)
		return err
	}, store.BusEvent{
		Topic: "inbox.updated", Entity: "inbox", EntityID: item.ID, Action: "updated",
		TraceID: item.TraceID, Revision: item.Revision + 1,
		Attrs: InboxUpdate{Op: OpUpdate, IDs: []string{item.ID}, Count: 1},
	})
	if err != nil {
		s.log.Warn("approval attach", "id", item.ID, "error", err)
	}
}

func (s *Service) checkUsage(ctx context.Context) {
	// The highest 5-hour or weekly window of each harness. A window past
	// its reset time counts as 0, so it re-arms the warning.
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT harness, MAX(CASE WHEN resets_at IS NULL OR resets_at > ? THEN used_percent ELSE 0 END)
		FROM harness_usage WHERE window_key IN ('five_hour','weekly')
		GROUP BY harness`, now)
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
		_, warned, _ := s.st.GetFlag(usageWarnedFlag + name)
		if pct < float64(s.opts.UsageWarnAt) {
			s.autoResolve(ctx, KindUsageWarning, subj, "auto")
			if warned {
				if err := s.st.DeleteFlag(usageWarnedFlag + name); err != nil {
					s.log.Warn("usage_warning flag", "harness", name, "error", err)
				}
			}
			continue
		}
		// One warning per crossing; usage must drop below the threshold to re-arm.
		if warned {
			continue
		}
		if err := s.st.SetFlag(usageWarnedFlag+name, "1"); err != nil {
			s.log.Warn("usage_warning flag", "harness", name, "error", err)
			continue
		}
		_, err := s.Raise(ctx, s.usageWarning(ctx, name, pct))
		if err != nil {
			s.log.Warn("raise usage_warning", "harness", name, "error", err)
		}
	}
}

// usageWindow is the fullest usage window of an AI app.
type usageWindow struct {
	weekly bool
	pct    float64
	resets time.Time
}

// usage is the fullest open usage window of an AI app.
func (s *Service) usage(ctx context.Context, name string) usageWindow {
	var u usageWindow
	var key string
	var resets sql.NullString
	if s.st.DB().QueryRowContext(ctx, `SELECT window_key, used_percent, resets_at FROM harness_usage
		WHERE harness=? AND window_key IN ('five_hour','weekly') ORDER BY used_percent DESC LIMIT 1`, name).
		Scan(&key, &u.pct, &resets) != nil {
		return usageWindow{}
	}
	u.weekly, u.resets = key == "weekly", parseTime(resets.String)
	return u
}

// usageWarning is the alert of an AI app close to its limit.
func (s *Service) usageWarning(ctx context.Context, name string, pct float64) RaiseOpts {
	app, u := appLabel(name), s.usage(ctx, name)
	window := "usage limit"
	switch {
	case u.weekly:
		window = "weekly limit"
	case u.pct > 0:
		window = "5-hour limit"
	}
	title := fmt.Sprintf("%s used %.0f%% of its %s", app, pct, window)
	body := title + "."
	if !u.resets.IsZero() {
		body += " The limit resets " + s.at(u.resets) + "."
	}
	body += " At 100%, jobs that use " + app + " wait for the reset."
	return RaiseOpts{Kind: KindUsageWarning, Harness: name, Title: title, Body: body}
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
		WHERE done_at IS NULL AND escalates_at IS NOT NULL AND escalates_at <= ?`, nowStr)
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
	// The cause can go away while the job waits in the queue.
	var done bool
	if err := s.st.DB().QueryRowContext(ctx, `SELECT done_at IS NOT NULL FROM inbox_items WHERE id=?`, job.id).Scan(&done); err == nil && done {
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
	if err := s.named(ctx, &opts); err != nil {
		return outbox.Row{}, "", err
	}
	link, label := opts.link(id, s.publicURL())
	if st.Via == notify.ViaEmail {
		if !s.emailOn() {
			return outbox.Row{}, "", fmt.Errorf("email is not configured")
		}
		mail := email.AlertMail{
			InboxID: id, Subject: opts.emailSubject(), Text: s.mailText(ctx, opts), Link: link, LinkLabel: label,
			Details: opts.names.Details(), To: st.Targets, RouteStep: i, Route: route,
		}
		if opts.Kind == KindRequestReminder {
			mail.ThreadKind, mail.ThreadKey, mail.Reply = string(KindRequest), "request/"+opts.RequestID, true
		}
		return s.opts.Email.SendAlert(ctx, mail)
	}
	step := i
	row, err := s.opts.Outbox.Enqueue(ctx, outbox.Row{
		Kind: outbox.KindChannel, Purpose: outbox.PurposeAlert, WorkspaceID: opts.WorkspaceID,
		Instance: st.Via, Room: st.To, Body: opts.pushText(link, label), Env: opts.notifyEnv(id),
		InboxID: id, DedupeKey: fmt.Sprintf("alert/%s/%d", id, i), RouteStep: &step, Route: route,
	})
	return row, "", err
}

// mailText is the alert mail body. A proposal mail leads with the diff.
func (s *Service) mailText(ctx context.Context, opts RaiseOpts) string {
	text := opts.emailText()
	if opts.Kind == KindRequest {
		if r, ok := s.request(ctx, opts.RequestID); ok {
			text = requestMail(r, opts.names.Owner())
		}
	}
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
	via := cmp.Or(row.Instance, notify.ViaEmail)
	s.appendDelivered(ctx, opts, item.ID, via, *row.RouteStep, "failed", "", row.ErrorMessage)
	if item.Done {
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
	via, msgID := cmp.Or(row.Instance, notify.ViaEmail), ""
	if via == notify.ViaEmail {
		msgID = item.EmailMessageID
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
	}, inboxUpdated(OpUpdate, []string{id}, "")); err != nil {
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
	actions := []InboxAction{}
	if f.Fix != "" {
		actions = append(actions, InboxAction{Kind: ActionFix, Route: "/v1/system/doctor/" + f.ID + "/fix", Label: "Fix"})
	}
	actJSON, _ := json.Marshal(actions)
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	if item == nil && s.userArchived(ctx, subject) {
		return // the finding stays; the user closed its item
	}
	if item != nil {
		_, _ = s.st.DB().ExecContext(ctx, `UPDATE inbox_items SET title=?, body=?, actions=?,
			revision=revision+1, updated_at=? WHERE id=? AND (title<>? OR body<>? OR actions<>?)`,
			f.Message, f.Message, string(actJSON), nowStr, item.ID, f.Message, f.Message, string(actJSON))
		return
	}
	id := ulid.Make().String()
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_items (
			id, kind, subject, title, body, actions, alert_kind, revision, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,1,?,?)`,
			id, string(InboxHealth), subject, f.Message, f.Message,
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
	if d.Class == email.RouteHeld {
		title, body := d.HeldItem(msg, s.agentTitle(ctx, d.WorkspaceID, d.Agent))
		return s.Raise(ctx, RaiseOpts{Kind: KindEmailHeld, Subject: "email/held/" + msg.ID, Title: sanitizeText(title),
			Body: sanitizeText(body), WorkspaceID: d.WorkspaceID, JobID: d.AgentID, TraceID: d.TraceID})
	}
	var subject, title, body, hint string
	quoted := "\"" + sanitizeText(msg.Subject) + "\""
	switch {
	case d.Reason == email.ReasonUndelivered:
		subject = "email/undelivered/" + msg.ID
		title = "Trebi could not add a reply from " + msg.From + " to the conversation"
		body = "The mail " + quoted + " from " + msg.From + " answers a Trebi conversation, but Trebi could not add it."
		hint = "Send the text again in the conversation."
	case d.Spoof():
		subject = "email/spoof/" + msg.From
		_, domain, _ := strings.Cut(msg.From, "@")
		title = "Trebi dropped mail that claims to be from " + msg.From
		body = "The mail server of " + cmp.Or(domain, "the sender") + " did not prove that it sent the mail " + quoted + ". Trebi dropped it."
		hint = "If the mail is real, ask the sender to fix the mail setup of their domain."
	default:
		subject = "email/misrouted/" + msg.Tag
		title = "Mail to +" + msg.Tag + " did not arrive"
		if who, ok := s.agentByTag(ctx, d.WorkspaceID, msg.Tag); ok {
			body = msg.From + " is not on the list of people that " + who + " takes mail from."
			hint = "Let " + who + " take mail from " + msg.From + "."
		} else {
			body = "No agent takes mail at +" + msg.Tag + ". Trebi did not deliver the mail from " + msg.From + "."
			hint = "Check the address, or make an agent that takes this mail."
		}
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
			id, kind, subject, title, body, hint, workspace_id, alert_kind,
			trace_id, email_message_id, revision, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,1,?,?)`,
			id, string(InboxHealth), subject, title, body, hint,
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
	who := "The agent"
	var jobID sql.NullString
	if s.st.DB().QueryRowContext(ctx, `SELECT job_id FROM runs WHERE id=?`, runID).Scan(&jobID) == nil && jobID.Valid {
		o := RaiseOpts{JobID: jobID.String, WorkspaceID: d.WorkspaceID}
		if s.named(ctx, &o) == nil && o.names.Owner() != "" {
			who = o.names.Owner()
		}
	}
	title := "Your reply says both approve and deny"
	body := who + " got the reply \"" + sanitizeText(msg.Subject) + "\" as a message. Its question still waits."
	hint := "Reply again with one word: approve or deny."
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
			id, kind, subject, title, body, hint, run_id, workspace_id, alert_kind,
			trace_id, email_message_id, revision, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
			id, string(InboxHealth), subject, title, body, hint,
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
		FROM inbox_items WHERE subject=? AND done_at IS NULL
		ORDER BY created_at DESC LIMIT 1`, subject)
	item, err := scanInbox(row, false)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return item, err
}

// resolveStaleDoctor closes the open items of findings that went away. A
// user-archived item of such a finding becomes a system close, so the
// finding shows again when it comes back.
func (s *Service) resolveStaleDoctor(ctx context.Context, want map[string]struct{}) {
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT id, subject, done_at IS NULL FROM inbox_items
		WHERE subject LIKE 'doctor/%' AND (done_at IS NULL OR done_by = ?)`, DoneByUser)
	if err != nil {
		return
	}
	var open, archived []string
	for rows.Next() {
		var id, subject string
		var isOpen bool
		if rows.Scan(&id, &subject, &isOpen) != nil {
			_ = rows.Close() // a bad row stops this round; the next tick retries
			return
		}
		if _, ok := want[strings.TrimPrefix(subject, "doctor/")]; ok {
			continue
		}
		if isOpen {
			open = append(open, id)
		} else {
			archived = append(archived, id)
		}
	}
	_ = rows.Close() // read-only query
	for _, id := range open {
		if err := s.Close(ctx, id, "auto"); err != nil {
			s.log.Warn("doctor resolve", "id", id, "error", err)
		}
	}
	for _, id := range archived {
		if _, err := s.st.DB().ExecContext(ctx, `UPDATE inbox_items SET done_by=? WHERE id=?`, DoneBySystem, id); err != nil {
			s.log.Warn("doctor resolve", "id", id, "error", err)
		}
	}
}

// userArchived reports whether the newest item of subject is closed by the user.
func (s *Service) userArchived(ctx context.Context, subject string) bool {
	var by string
	err := s.st.DB().QueryRowContext(ctx, `SELECT COALESCE(done_by,'') FROM inbox_items WHERE subject=?
		ORDER BY created_at DESC LIMIT 1`, subject).Scan(&by)
	return err == nil && by == DoneByUser
}

// CollectDoctorFindings is the system doctor snapshot from the live store.
func (s *Service) CollectDoctorFindings() []DoctorFinding {
	cfg, _ := config.Load()
	out := append(collectDoctorFindings(s.st, s.opts.Actions, cfg), s.notifyFindings(context.Background())...)
	return append(out, s.harnessVersionFindings(context.Background(), cfg)...)
}

// notifyFindings reports each layer whose route holds a problem step, and a
// configured email channel with no address to mail.
func (s *Service) notifyFindings(ctx context.Context) []DoctorFinding {
	if s.opts.Router == nil {
		return nil
	}
	scopes := []notify.Scope{{}}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT w.id, COALESCE(j.name,'') FROM workspaces w
		LEFT JOIN job_index j ON j.workspace_id=w.id AND j.active=1 AND j.retired_at IS NULL AND j.id NOT IN (`+store.ArchivedJobIDsSQL+`) WHERE w.orphaned=0 ORDER BY w.id, j.name`)
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
	noContact := s.emailOn() && (s.opts.SetupDone == nil || s.opts.SetupDone())
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
			// The tray notifies first. Mail follows after the escalation delay.
			if r.Source == notify.LayerDefault && st.Problem == notify.ProblemEmail {
				continue
			}
			code := "notify_route_invalid"
			if st.Problem == notify.ProblemScope {
				code = "connector_out_of_scope"
			}
			out = append(out, DoctorFinding{
				ID: "notify-route-" + strings.ReplaceAll(key, "/", "-"), Severity: "warn", Code: code,
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
func collectDoctorFindings(st *store.Store, actions ActionsChecker, cfg *config.Config) []DoctorFinding {
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
			Message: "The Trebi database file is missing. Restart Trebi to make it again.",
		})
	}
	// The daemon seeds config.yaml at startup, so an absent file means it was
	// removed afterwards. Defaults still apply, hence info and not warn.
	if _, err := os.Stat(home.ConfigPath()); err != nil {
		findings = append(findings, DoctorFinding{
			ID: "config-missing", Severity: "info", Code: "config_missing",
			Message: "config.yaml missing; defaults apply (fix rewrites it)", Fix: FixWriteConfig,
		})
	}
	for _, h := range harness.List(cfg, nil) {
		if !h.Enabled || h.Hidden {
			continue
		}
		if _, err := os.Stat(h.Home); err == nil {
			continue
		}
		findings = append(findings, DoctorFinding{
			ID: "harness-" + h.Name, Severity: "info", Code: "harness_home_missing",
			Message: fmt.Sprintf("harness home for %s not created yet (will appear on first launch)", h.Name),
			Fix:     FixHarnessHome, Harness: h.Name,
		})
	}
	findings = append(findings, CollectAgentDoctorFindings(st, actions, cfg)...)
	if f, ok := stateRetiredFinding(context.Background(), st.DB()); ok {
		findings = append(findings, f)
	}
	if f, ok := duplicateIDFinding(context.Background(), st); ok {
		findings = append(findings, f)
	}
	findings = append(findings, alertChannelFindings(context.Background(), st.DB())...)
	if findings == nil {
		return []DoctorFinding{}
	}
	return findings
}

// stateRetiredFinding counts the retired jobs and agents that still have
// state rows. The state sweep deletes them state.RetiredGrace after retire.
func stateRetiredFinding(ctx context.Context, db *sql.DB) (DoctorFinding, bool) {
	var n int
	var first sql.NullString
	err := db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(retired_at) FROM job_index
		WHERE retired_at IS NOT NULL AND id IN (SELECT DISTINCT owner_id FROM state_rows)`).Scan(&n, &first)
	if err != nil || n == 0 {
		return DoctorFinding{}, false
	}
	purge := store.ParseTS(first.String).Add(state.RetiredGrace).Format("2006-01-02")
	return DoctorFinding{
		ID: "state-retired", Severity: "info", Code: "state_retired",
		Message: fmt.Sprintf("%d deleted job(s) or agent(s) still have state; the first purge is on %s", n, purge),
	}, true
}

// alertChannelFindings names each alert channel whose newest finished alert
// did not send. The finding goes to the inbox only, never to the channel
// that fails. A later alert that sends on the channel clears it.
func alertChannelFindings(ctx context.Context, db *sql.DB) []DoctorFinding {
	rows, err := db.QueryContext(ctx, `SELECT CASE WHEN kind='email' THEN 'email' ELSE instance END, status,
		COALESCE(error_message,''), created_at FROM outbox
		WHERE purpose=? AND (status=? OR status=? OR (status=? AND COALESCE(error_message,'') <> ''))
		ORDER BY created_at DESC LIMIT 500`,
		string(outbox.PurposeAlert), string(outbox.StatusSent), string(outbox.StatusFailed), string(outbox.StatusCanceled))
	if err != nil {
		return nil
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []DoctorFinding
	for rows.Next() {
		var via, status, msg, at string
		if rows.Scan(&via, &status, &msg, &at) != nil || seen[via] {
			continue
		}
		seen[via] = true
		if status == string(outbox.StatusSent) {
			continue
		}
		out = append(out, DoctorFinding{
			ID: "alert-channel-" + via, Severity: "error", Code: "alert_channel_failed",
			Message: fmt.Sprintf("Alerts by %s do not send: %s (last try %s). Alerts stay in the inbox.",
				via, cmp.Or(msg, "unknown error"), dayText(store.ParseTS(at))),
		})
	}
	return out
}

// duplicateIDFinding names the jobs and agents that got a new ID in the
// bus window because their file had the ID of another one, as a copied
// folder does.
func duplicateIDFinding(ctx context.Context, st *store.Store) (DoctorFinding, bool) {
	evs, err := st.ListBusWith(ctx, store.ListBusOpts{Topics: []string{"job.id_assigned"}, Limit: 1000, Exact: true})
	if err != nil {
		return DoctorFinding{}, false
	}
	var names []string
	for _, ev := range evs {
		if a := attrMap(ev.Attrs); a["reason"] == "duplicate" {
			names = append(names, a["name"])
		}
	}
	if len(names) == 0 {
		return DoctorFinding{}, false
	}
	return DoctorFinding{
		ID: "id-duplicate", Severity: "warn", Code: "id_duplicate",
		Message: fmt.Sprintf("%d job(s) or agent(s) had the ID of another one and got a new ID: %s. A copied folder does this; its runs and state start empty",
			len(names), strings.Join(names, ", ")),
	}, true
}

// raiseOptsFromItem rebuilds the delivery context one stored item carries.
func raiseOptsFromItem(item InboxItem) RaiseOpts {
	return RaiseOpts{
		Kind: Kind(item.AlertKind), Subject: item.Subject, Title: item.Title,
		Body: item.Body, Hint: item.Hint, RunID: item.RunID,
		WorkspaceID: item.Workspace, JobID: item.JobID, JobName: item.JobName, TraceID: item.TraceID,
		RequestID: item.RequestID, Connector: item.Connector, Harness: subjectHarness(item.Subject),
	}
}

// subjectHarness is the AI app of a harness subject, else "".
func subjectHarness(subject string) string {
	for _, p := range []string{"harness/", "harness-update/"} {
		if name, ok := strings.CutPrefix(subject, p); ok {
			return name
		}
	}
	return ""
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
