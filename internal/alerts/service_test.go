package alerts_test

import (
	"os"

	"github.com/trebi-ai/trebi/internal/hometest"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/requests"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/visor"
)

// TestMain lets the test binary run as the holder of a visor session.
func TestMain(m *testing.M) {
	if code, ok := visor.Hold(os.Args); ok {
		os.Exit(code)
	}
	hometest.Main(m)
}

func TestRaiseInbox(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()

	id, err := svc.Raise(ctx, alerts.RaiseOpts{
		Kind: alerts.KindWaiting, Subject: "run/1", Title: "Agent waiting",
		Body: "approve?", Hint: "open attach", RunID: "run1", WorkspaceID: "ws", JobName: "job",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("empty id")
	}
	items, err := listAll(ctx, svc, alerts.ListOpts{Limit: 10})
	if err != nil || len(items) != 1 {
		t.Fatalf("%v %v", items, err)
	}
	if items[0].Revision != 1 {
		t.Fatalf("revision %d", items[0].Revision)
	}
	if len(items[0].Actions) != 1 || items[0].Actions[0].Kind != alerts.ActionOpenSession {
		t.Fatalf("actions: %+v", items[0].Actions)
	}
	got, err := svc.Get(ctx, id)
	if err != nil || got.Body != "approve?" {
		t.Fatalf("get: %+v %v", got, err)
	}
	events, err := st.ListBus(ctx, 0, []string{"alert"}, 10)
	if err != nil || len(events) < 1 {
		t.Fatalf("bus: %v %v", events, err)
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind     alerts.Kind
		severity alerts.Severity
		inbox    alerts.InboxKind
		cooldown time.Duration
		reason   string
		allow    bool
	}{
		{alerts.KindWaiting, alerts.SeverityAttention, alerts.InboxApproval, 0, "", true},
		{alerts.KindAuthMissing, alerts.SeverityBlocker, alerts.InboxHealth, 5 * time.Minute, "", true},
		{alerts.KindLimitExceeded, alerts.SeverityBlocker, alerts.InboxHealth, 15 * time.Minute, "", true},
		{alerts.KindUsageWarning, alerts.SeverityAttention, alerts.InboxHealth, 0, "", true},
		{alerts.KindConnectorUnhealthy, alerts.SeverityBlocker, alerts.InboxHealth, 8 * time.Hour, "", true},
		{alerts.KindRunFailed, alerts.SeverityError, alerts.InboxFailure, time.Hour, "idle_timeout", true},
		{alerts.KindRunFailed, alerts.SeverityError, alerts.InboxFailure, time.Hour, "kill", false},
		{alerts.KindRunFailed, alerts.SeverityError, alerts.InboxFailure, time.Hour, "auth", false},
		{alerts.KindResumeExhausted, alerts.SeverityError, alerts.InboxFailure, 0, "", true},
		{alerts.KindMachineDown, alerts.SeverityCritical, alerts.InboxWatchdog, 0, "", true},
	}
	for _, tc := range cases {
		spec, ok := alerts.Lookup(tc.kind)
		if !ok {
			t.Fatalf("missing %s", tc.kind)
		}
		if spec.Severity != tc.severity || spec.InboxKind != tc.inbox || spec.Cooldown != tc.cooldown {
			t.Fatalf("%s: %+v", tc.kind, spec)
		}
		if tc.reason != "" {
			svc, _ := testSvc(t, alerts.Options{})
			id, err := svc.Raise(context.Background(), alerts.RaiseOpts{
				Kind: tc.kind, Subject: "x", Title: "t", EndReason: tc.reason,
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.allow && id == "" {
				t.Fatalf("%s reason %s: want item", tc.kind, tc.reason)
			}
			if !tc.allow && id != "" {
				t.Fatalf("%s reason %s: B20 should gate", tc.kind, tc.reason)
			}
		}
	}
}

func TestDedupeAndCooldown(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	opts := alerts.RaiseOpts{Kind: alerts.KindRunFailed, Subject: "job/ws/demo", Title: "Run failed", EndReason: "failed"}
	id1, err := svc.Raise(ctx, opts)
	if err != nil || id1 == "" {
		t.Fatalf("%q %v", id1, err)
	}
	id2, err := svc.Raise(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Fatalf("dedupe want %s got %s", id1, id2)
	}
	items, _ := listAll(ctx, svc, alerts.ListOpts{})
	if len(items) != 1 {
		t.Fatalf("items %d", len(items))
	}
	events, _ := st.ListBus(ctx, 0, []string{"alert.suppressed"}, 20)
	n := 0
	for _, ev := range events {
		if ev.Topic == "alert.suppressed" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("suppressed %d want 1 (events %v)", n, events)
	}
}

func TestAuthMissingAutoResolve(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	id, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindAuthMissing, Harness: "claude", Title: "Auth missing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "harness.updated", Entity: "harness", EntityID: "claude", Action: "updated",
		Attrs: map[string]string{"harness": "claude", "auth_ok": "true", "auth_status": "ok"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	got, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.DoneBy != alerts.DoneBySystem {
		t.Fatalf("status %s", got.DoneBy)
	}
}

func TestAutoResolve(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	id, err := svc.Raise(ctx, alerts.RaiseOpts{
		Kind: alerts.KindWaiting, Subject: "run/r1", Title: "Agent waiting", RunID: "r1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "session.state",
		Attrs: map[string]string{"run_id": "r1", "state": "running"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	got, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.DoneBy != alerts.DoneBySystem {
		t.Fatalf("status %s", got.DoneBy)
	}
}

func TestConsumeRunFailed(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "run.failed", Entity: "run", EntityID: "run1", Action: "updated",
		Attrs: map[string]string{"end_reason": "failed", "status": "failed", "job_id": "jb_00000000d1", "job_name": "demo", "workspace": "ws"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	items, err := listAll(ctx, svc, alerts.ListOpts{})
	if err != nil || len(items) != 1 {
		t.Fatalf("%v %v", items, err)
	}
	if items[0].AlertKind != string(alerts.KindRunFailed) {
		t.Fatalf("kind %s", items[0].AlertKind)
	}
	// kill never pages
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "run.failed", Entity: "run", EntityID: "run2", Action: "updated",
		Attrs: map[string]string{"end_reason": "kill", "status": "failed", "job_id": "jb_00000000d1", "job_name": "demo", "workspace": "ws"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	items, _ = listAll(ctx, svc, alerts.ListOpts{})
	if len(items) != 1 {
		t.Fatalf("kill should not raise, items=%d", len(items))
	}
}

// TestConsumeRunFailedReportedReason puts the reason of a reported failure in
// the alert body.
func TestConsumeRunFailedReportedReason(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO run_events (run_id, at, kind, source, detail_json) VALUES
		('run1', '2026-10-27T00:00:00Z', 'error', 'wire', '{"message":"LunchMoney is not reachable","class":"reported"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "run.failed", Entity: "run", EntityID: "run1", Action: "updated",
		Attrs: map[string]string{"end_reason": "failed", "status": "failed", "job_id": "jb_00000000d1", "job_name": "demo", "workspace": "ws"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	items, err := listAll(ctx, svc, alerts.ListOpts{})
	if err != nil || len(items) != 1 {
		t.Fatalf("%v %v", items, err)
	}
	got, err := svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "The run of demo failed. The agent said: LunchMoney is not reachable" {
		t.Fatalf("body %q", got.Body)
	}
}

// TestConsumeRunMarkedCompleteResolves: a human marking a failed run complete
// clears the run_failed item, same as a successful retry.
func TestConsumeRunMarkedCompleteResolves(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "run.failed", Entity: "run", EntityID: "run1", Action: "updated",
		Attrs: map[string]string{"end_reason": "idle_timeout", "status": "failed", "job_id": "jb_00000000d1", "job_name": "demo", "workspace": "ws"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	items, err := listAll(ctx, svc, alerts.ListOpts{})
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	// The machine status stays failed; the outcome is what resolves the item.
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "run.updated", Entity: "run", EntityID: "run1", Action: "updated",
		Attrs: map[string]string{"status": "failed", "outcome": "complete", "job_id": "jb_00000000d1", "job_name": "demo", "workspace": "ws"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	got, err := svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DoneBy != alerts.DoneBySystem {
		t.Fatalf("status %s want resolved", got.DoneBy)
	}
}

// TestConsumeMemoryInvalid: a memory that stays damaged for three runs in a
// row raises an item with a body for a person and two actions. memory.valid of the same job closes it; that of
// another job does not.
func TestConsumeMemoryInvalid(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	send := func(topic, run, jobID, jobName string, extra map[string]string) {
		t.Helper()
		attrs := map[string]string{"run_id": run, "kind": "job", "workspace": "ws", "job_id": jobID, "job_name": jobName}
		for k, v := range extra {
			attrs[k] = v
		}
		if _, err := st.Append(ctx, store.BusEvent{Topic: topic, Entity: "run", EntityID: run, Action: "updated", Attrs: attrs}); err != nil {
			t.Fatal(err)
		}
		svc.Consume(ctx)
	}
	invalid := func(run, jobID, jobName, detail string) {
		t.Helper()
		send("memory.invalid", run, jobID, jobName, map[string]string{"detail": detail})
	}
	// Two failed runs in a row are not an alert yet; a valid run resets the streak.
	invalid("run0", "jb_00000000d1", "devto-monitor", "x.")
	invalid("run0", "jb_00000000d1", "devto-monitor", "x.")
	send("memory.valid", "run0", "jb_00000000d1", "devto-monitor", nil)
	invalid("run0", "jb_00000000d1", "devto-monitor", "x.")
	invalid("run0", "jb_00000000d1", "devto-monitor", "x.")
	if items, _ := listAll(ctx, svc, alerts.ListOpts{}); len(items) != 0 {
		t.Fatalf("short streak raised items: %v", items)
	}
	invalid("run1", "jb_00000000d1", "devto-monitor", "Line 9: missing section '## Lessons'.")
	for range 3 {
		invalid("run2", "agt_00000000a1", "agents/ellis", "file is missing.")
		invalid("run5", "jb_00000000d3", "agents/ellis/scan", "file is missing.")
	}
	items, err := listAll(ctx, svc, alerts.ListOpts{})
	if err != nil || len(items) != 3 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	byJob := map[string]alerts.InboxItem{}
	for _, it := range items {
		full, err := svc.Get(ctx, it.ID) // a list row has no body
		if err != nil {
			t.Fatal(err)
		}
		byJob[it.JobID] = *full
	}
	job, agent := byJob["jb_00000000d1"], byJob["agt_00000000a1"]
	if job.Title != "devto-monitor cannot save its memory" || !strings.HasPrefix(job.Body, "The memory of devto-monitor is damaged. Trebi cannot save what the job learns") ||
		!strings.Contains(job.Body, " Details: Line 9: missing section '## Lessons'.") {
		t.Fatalf("job item title=%q body=%q", job.Title, job.Body)
	}
	if !strings.HasPrefix(agent.Body, "The memory of ellis is damaged. Trebi cannot save what the agent learns") {
		t.Fatalf("agent body=%q", agent.Body)
	}
	for _, it := range byJob {
		if strings.Contains(it.Title+it.Body, "MEMORY.md") || strings.Contains(it.Title+it.Body, "Log entry") {
			t.Fatalf("developer words in %q / %q", it.Title, it.Body)
		}
	}
	wantJob := []alerts.InboxAction{
		{Kind: alerts.ActionReview, Route: "/jobs/ws/devto-monitor?tab=memory", Label: "Open memory"},
		{Kind: alerts.ActionFix, Label: "Repair with assistant"},
	}
	if len(job.Actions) != 2 || job.Actions[0] != wantJob[0] || job.Actions[1].Kind != wantJob[1].Kind || job.Actions[1].Label != wantJob[1].Label {
		t.Fatalf("job actions %+v", job.Actions)
	}
	if !strings.HasPrefix(job.Actions[1].Route, "/assistant?prompt=") || !strings.Contains(job.Actions[1].Route, "devto-monitor") {
		t.Fatalf("repair route %q", job.Actions[1].Route)
	}
	if len(agent.Actions) != 2 || agent.Actions[0].Route != "/agents/ws/ellis?tab=memory" {
		t.Fatalf("agent actions %+v", agent.Actions)
	}
	owned := byJob["jb_00000000d3"]
	if !strings.HasPrefix(owned.Body, "The memory of scan is damaged. Trebi cannot save what the job learns") ||
		len(owned.Actions) != 2 || owned.Actions[0].Route != "/jobs/ws/agents%2Fellis%2Fscan?tab=memory" {
		t.Fatalf("agent job item body=%q actions=%+v", owned.Body, owned.Actions)
	}

	send("memory.valid", "run3", "jb_00000000d2", "other", nil)
	if open, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{View: alerts.ViewInbox}}); len(open) != 3 {
		t.Fatalf("another job closed an item: open=%d", len(open))
	}
	send("memory.valid", "run4", "jb_00000000d1", "devto-monitor", nil)
	got, err := svc.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DoneBy != alerts.DoneBySystem {
		t.Fatalf("done_by %q want system", got.DoneBy)
	}
	if got, _ := svc.Get(ctx, agent.ID); got.DoneBy != "" {
		t.Fatalf("agent item closed: %q", got.DoneBy)
	}
}

func TestWaitingDwell(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	svc, st := testSvc(t, alerts.Options{
		WaitingAfter: 5 * time.Minute,
		Now:          func() time.Time { return now },
	})
	ctx := context.Background()
	since := now.Add(-6 * time.Minute).Format(time.RFC3339Nano)
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "session.state",
		Attrs: map[string]string{"run_id": "r1", "state": "waiting", "since": since, "workspace": "ws", "job_id": "jb_00000000b2", "job_name": "j"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Tick(ctx)
	items, _ := listAll(ctx, svc, alerts.ListOpts{})
	if len(items) != 1 {
		t.Fatalf("want waiting item, got %d", len(items))
	}
}

// TestWaitingApprovalEscalation pins the D3 and M5 path: a dwelt waiting
// raise with a pending approval carries the question body, mails only after
// the approval interval, and clears the deadline once mailed.
func TestWaitingApprovalEscalation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	svc, st := routeSvc(t, alerts.Options{
		WaitingAfter: 5 * time.Minute, ApprovalAfter: time.Minute,
		Now: func() time.Time { return now },
		ApprovalQuestion: func(context.Context, string) string {
			return "Allow Bash?"
		},
	}, nil, &sendWorker{token: "test-token-0000"}, nil)
	ctx := context.Background()
	since := now.Add(-6 * time.Minute).Format(time.RFC3339Nano)
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "session.state",
		Attrs: map[string]string{"run_id": "r1", "state": "waiting", "since": since},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Tick(ctx)
	items, _ := listAll(ctx, svc, alerts.ListOpts{})
	if len(items) != 1 {
		t.Fatalf("want waiting item, got %d", len(items))
	}
	got, err := svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "An agent needs your OK to run a command" ||
		got.Body != "An agent wants to run a command on this machine and waits for your OK." ||
		got.Hint != "Reply approve or deny to this email. Or answer in Trebi." {
		t.Fatalf("item body/hint = %q / %q", got.Body, got.Hint)
	}
	if got.EscalatesAt == nil || !got.EscalatesAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("escalates_at = %v", got.EscalatesAt)
	}
	if events, _ := st.ListBus(ctx, 0, []string{"alert.delivered"}, 10); len(events) != 0 {
		t.Fatalf("delivered before the interval: %v", events)
	}

	now = now.Add(2 * time.Minute)
	svc.Tick(ctx)
	svc.Consume(ctx)
	events, _ := st.ListBus(ctx, 0, []string{"alert.delivered"}, 10)
	if len(events) != 1 {
		t.Fatalf("want one delivery, got %v", events)
	}
	got, err = svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EscalatesAt != nil {
		t.Fatalf("escalates_at not cleared: %v", got.EscalatesAt)
	}
}

// TestApprovalCreatedNotesWaiting covers the advisory path: approval.created
// without a session.state frame still starts the dwell clock, and the dwelt
// raise carries the question.
func TestApprovalCreatedNotesWaiting(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	svc, st := testSvc(t, alerts.Options{
		WaitingAfter: 5 * time.Minute,
		Now:          func() time.Time { return now },
		ApprovalQuestion: func(context.Context, string) string {
			return "Overwrite the nightly report?"
		},
	})
	ctx := context.Background()
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "approval.created", Attrs: map[string]string{"run_id": "r1"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Tick(ctx)
	if items, _ := listAll(ctx, svc, alerts.ListOpts{}); len(items) != 0 {
		t.Fatalf("raised before dwell: %v", items)
	}

	now = now.Add(6 * time.Minute)
	svc.Tick(ctx)
	items, _ := listAll(ctx, svc, alerts.ListOpts{})
	if len(items) != 1 {
		t.Fatalf("want waiting item, got %d", len(items))
	}
	got, err := svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AlertKind != "waiting" || got.Body != "An agent waits for your OK: Overwrite the nightly report?" {
		t.Fatalf("item = %+v", got)
	}
}

// TestApprovalCreatedAttachesRaisedItem covers the late question: the dwelt
// item exists first, then approval.created writes the body and arms the mail.
func TestApprovalCreatedAttachesRaisedItem(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	question := ""
	svc, st := testSvc(t, alerts.Options{
		WaitingAfter: 5 * time.Minute, ApprovalAfter: time.Minute,
		Now:              func() time.Time { return now },
		ApprovalQuestion: func(context.Context, string) string { return question },
	})
	ctx := context.Background()
	since := now.Add(-6 * time.Minute).Format(time.RFC3339Nano)
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "session.state",
		Attrs: map[string]string{"run_id": "r1", "state": "waiting", "since": since},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Tick(ctx)
	items, _ := listAll(ctx, svc, alerts.ListOpts{})
	if len(items) != 1 {
		t.Fatalf("want waiting item, got %d", len(items))
	}

	question = "Deploy to production?"
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "approval.created", Attrs: map[string]string{"run_id": "r1"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Tick(ctx)
	got, err := svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "An agent needs your OK" || got.Body != "An agent waits for your OK: Deploy to production?" {
		t.Fatalf("title = %q, body = %q", got.Title, got.Body)
	}
	if got.EscalatesAt == nil {
		t.Fatal("escalates_at not armed")
	}
}

// TestApprovalDecidedResolvesWaiting pins the decided frame onto the waiting
// subject, so a reply-by-mail decision drains the inbox item.
func TestApprovalDecidedResolvesWaiting(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	id, err := svc.Raise(ctx, alerts.RaiseOpts{
		Kind: alerts.KindWaiting, Subject: "run/r1", RunID: "r1", Title: "Agent waiting",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "approval.decided", Attrs: map[string]string{"run_id": "r1"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Tick(ctx)
	got, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.DoneBy != alerts.DoneBySystem {
		t.Fatalf("status = %q", got.DoneBy)
	}
}

func TestRunLoop(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{TickInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "connector.unhealthy", Entity: "connector", EntityID: "mail", Action: "updated",
		Attrs: map[string]string{"connector": "mail", "status": "crashed"},
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		items, _ := listAll(ctx, svc, alerts.ListOpts{})
		if len(items) >= 1 {
			cancel()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("consumer never raised")
}

func TestListCursor(t *testing.T) {
	t.Parallel()
	svc, _ := testSvc(t, alerts.Options{})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := svc.Raise(ctx, alerts.RaiseOpts{
			Kind: alerts.KindUsageWarning, Subject: "harness/h" + string(rune('a'+i)), Title: "warn",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	page, err := svc.List(ctx, alerts.ListOpts{Limit: 2})
	if err != nil || len(page.Items) != 2 || page.NextCursor != page.Items[1].ID {
		t.Fatalf("%+v %v", page, err)
	}
	rest, err := svc.List(ctx, alerts.ListOpts{Limit: 2, Cursor: page.NextCursor})
	if err != nil || len(rest.Items) != 1 || rest.NextCursor != "" {
		t.Fatalf("rest %+v %v", rest, err)
	}
}

func TestDoctorSync(t *testing.T) {
	t.Parallel()
	svc, _ := testSvc(t, alerts.Options{})
	ctx := context.Background()
	svc.SyncDoctor(ctx, []alerts.DoctorFinding{
		{ID: "config-missing", Severity: "warn", Code: "config_missing", Message: "no config", Fix: alerts.FixWriteConfig},
	})
	items, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Kind: alerts.InboxHealth}})
	if len(items) != 1 || items[0].Subject != "doctor/config-missing" {
		t.Fatalf("%+v", items)
	}
	if len(items[0].Actions) != 1 || items[0].Actions[0].Kind != alerts.ActionFix {
		t.Fatalf("actions %+v", items[0].Actions)
	}
	svc.SyncDoctor(ctx, nil)
	got, _ := svc.Get(ctx, items[0].ID)
	if got.DoneBy != alerts.DoneBySystem {
		t.Fatalf("stale doctor %s", got.DoneBy)
	}
}

// TestAlertTextSanitized: a hostile connector status lands in the item
// body without quotes, backticks, or newlines.
func TestAlertTextSanitized(t *testing.T) {
	t.Parallel()
	status := "ok`; touch /tmp/x; echo $(id); echo \"; boom #\nboom"
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "connector.unhealthy", Entity: "connector", EntityID: "mail", Action: "updated",
		Attrs: map[string]string{"connector": "mail", "status": status},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	items, err := listAll(ctx, svc, alerts.ListOpts{})
	if err != nil || len(items) != 1 {
		t.Fatalf("items %v %v", items, err)
	}
	got, err := svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got.Body, "`\"\n") {
		t.Fatalf("body not sanitized: %q", got.Body)
	}
}

func TestListAlertsNewestFirst(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := svc.Raise(ctx, alerts.RaiseOpts{
			Kind: alerts.KindUsageWarning, Subject: fmt.Sprintf("harness/h%d", i), Title: "warn",
		}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := st.ListBus(ctx, 0, []string{"alert.raised"}, 20)
	if err != nil || len(all) != 5 {
		t.Fatalf("raised %d %v", len(all), err)
	}
	got, err := svc.ListAlerts(ctx, time.Time{}, 2, "")
	if err != nil || len(got) != 2 {
		t.Fatalf("list %d %v", len(got), err)
	}
	if got[0].Seq < got[1].Seq {
		t.Fatalf("not newest-first: %d then %d", got[0].Seq, got[1].Seq)
	}
	if got[0].Seq != all[4].Seq || got[1].Seq != all[3].Seq {
		t.Fatalf("limit kept oldest: got %d,%d want %d,%d", got[0].Seq, got[1].Seq, all[4].Seq, all[3].Seq)
	}
	since := all[2].TS
	filtered, err := svc.ListAlerts(ctx, since, 2, "")
	if err != nil || len(filtered) != 2 {
		t.Fatalf("since %d %v", len(filtered), err)
	}
	if filtered[0].Seq != all[4].Seq || filtered[1].Seq != all[3].Seq {
		t.Fatalf("since truncated: got %d,%d", filtered[0].Seq, filtered[1].Seq)
	}
}

// TestRouteDefaultEmail: with no route set, the alert mails the admins.
func TestRouteDefaultEmail(t *testing.T) {
	t.Parallel()
	w := &sendWorker{token: "test-token-0000"}
	svc, st := routeSvc(t, alerts.Options{}, nil, w, nil)
	ctx := context.Background()
	id, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, RunID: "r1", Title: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	sends := w.sent()
	if len(sends) != 1 {
		t.Fatalf("native sends = %d, want 1", len(sends))
	}
	if want := "[Trebi] wait"; sends[0].Subject != want {
		t.Errorf("subject = %q, want %q", sends[0].Subject, want)
	}
	if strings.Contains(sends[0].Text, id) || strings.Contains(sends[0].Text, "r1") {
		t.Errorf("text = %q", sends[0].Text)
	}
	item, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if item.EmailMessageID != sends[0].MessageID || item.Delivery != alerts.DeliveryDelivered {
		t.Errorf("item message id %q delivery %q", item.EmailMessageID, item.Delivery)
	}
	if len(item.Deliveries) != 1 || item.Deliveries[0].Via != "email" || item.Deliveries[0].Status != "sent" {
		t.Errorf("deliveries %+v", item.Deliveries)
	}
	got := deliveredAttrs(t, st)
	if len(got) != 1 || got[0]["channel"] != "email" || got[0]["index"] != "0" || got[0]["status"] != "ok" ||
		got[0]["message_id"] != sends[0].MessageID {
		t.Errorf("delivered attrs = %v", got)
	}
}

// TestRouteConnectorFirst: a working connector step stops the route.
func TestRouteConnectorFirst(t *testing.T) {
	t.Parallel()
	w := &sendWorker{token: "test-token-0000"}
	ch := &chanSender{}
	route := []notify.Step{{Via: "whatsapp", To: "+1555"}, {Via: "email"}}
	svc, st := routeSvc(t, alerts.Options{}, route, w, ch)
	ctx := context.Background()
	id, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, RunID: "r1", Title: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	rows := ch.sent()
	if len(rows) != 1 || rows[0].Room != "+1555" || rows[0].Env["TREBI_ALERT_RESUME"] != id {
		t.Fatalf("channel rows %+v", rows)
	}
	if len(w.sent()) != 0 {
		t.Fatal("email ran after the connector succeeded")
	}
	got := deliveredAttrs(t, st)
	if len(got) != 1 || got[0]["channel"] != "whatsapp" || got[0]["status"] != "ok" {
		t.Fatalf("delivered %v", got)
	}
}

// TestRouteAdvancesOnFailure: a failed connector step moves to email, and
// a problem step is skipped.
func TestRouteAdvancesOnFailure(t *testing.T) {
	t.Parallel()
	w := &sendWorker{token: "test-token-0000"}
	ch := &chanSender{fail: true}
	route := []notify.Step{{Via: "whatsapp", To: "+1555"}, {Via: "nope"}, {Via: "email"}}
	svc, st := routeSvc(t, alerts.Options{}, route, w, ch)
	ctx := context.Background()
	id, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, RunID: "r1", Title: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	settle(ctx, svc)
	if len(w.sent()) != 1 {
		t.Fatalf("email sends = %d, want 1", len(w.sent()))
	}
	var seq []string
	for _, a := range deliveredAttrs(t, st) {
		seq = append(seq, a["channel"]+"/"+a["index"]+"/"+a["status"])
	}
	want := "whatsapp/0/failed nope/1/skipped:unknown_connector email/2/ok"
	if strings.Join(seq, " ") != want {
		t.Fatalf("delivered %v, want %s", seq, want)
	}
	item, _ := svc.Get(ctx, id)
	if item.Delivery != alerts.DeliveryDelivered || len(item.Deliveries) != 2 {
		t.Fatalf("item %q %+v", item.Delivery, item.Deliveries)
	}
}

// TestRouteAllFail: every step fails; the item shows view_outbox and no
// second alert is raised.
func TestRouteAllFail(t *testing.T) {
	t.Parallel()
	w := &sendWorker{token: "test-token-0000", failAll: true}
	ch := &chanSender{fail: true}
	route := []notify.Step{{Via: "whatsapp", To: "+1555"}, {Via: "email"}}
	svc, st := routeSvc(t, alerts.Options{}, route, w, ch)
	ctx := context.Background()
	id, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, RunID: "r1", Title: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	settle(ctx, svc)
	item, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if item.Delivery != alerts.DeliveryFailed {
		t.Fatalf("delivery %q", item.Delivery)
	}
	n := len(item.Actions)
	if n == 0 || item.Actions[n-1].Kind != alerts.ActionViewOutbox || item.Actions[n-1].Route != "/history/outbox?inbox="+id {
		t.Fatalf("actions %+v", item.Actions)
	}
	items, _ := listAll(ctx, svc, alerts.ListOpts{})
	if len(items) != 1 {
		t.Fatalf("items %d, want 1", len(items))
	}
	got := deliveredAttrs(t, st)
	if last := got[len(got)-1]; last["channel"] != "inbox" || last["status"] != alerts.DeliveryFailed {
		t.Fatalf("delivered %v", got)
	}
}

// TestRouteEmptyStaysInInbox: an empty route delivers nothing.
func TestRouteEmptyStaysInInbox(t *testing.T) {
	t.Parallel()
	w := &sendWorker{token: "test-token-0000"}
	svc, _ := routeSvc(t, alerts.Options{}, []notify.Step{}, w, nil)
	ctx := context.Background()
	id, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, RunID: "r1", Title: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.sent()) != 0 {
		t.Fatal("empty route sent mail")
	}
	if item, _ := svc.Get(ctx, id); item.Delivery != alerts.DeliveryInboxOnly {
		t.Fatalf("delivery %q", item.Delivery)
	}
}

func TestNotifySuppressedInShadowAndDrain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts alerts.Options
	}{
		{"shadow", alerts.Options{Shadow: true}},
		{"drain", alerts.Options{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := &sendWorker{token: "test-token-0000"}
			svc, st := routeSvc(t, tc.opts, nil, w, nil)
			if tc.name == "drain" {
				if err := st.SetFlag("drain", "1"); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			id, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, RunID: "r1", Title: "wait"})
			if err != nil || id == "" {
				t.Fatalf("%q %v", id, err)
			}
			if len(deliveredAttrs(t, st)) != 0 || len(w.sent()) != 0 {
				t.Fatal("must not deliver")
			}
		})
	}
}

func TestDeliverDoesNotBlockConsume(t *testing.T) {
	t.Parallel()
	ch := &chanSender{delay: 30 * time.Second}
	svc, st := routeSvc(t, alerts.Options{TickInterval: 15 * time.Millisecond},
		[]notify.Step{{Via: "whatsapp", To: "+1555"}}, nil, ch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "connector.unhealthy", Entity: "connector", EntityID: "mail", Action: "updated",
		Attrs: map[string]string{"connector": "mail", "status": "down"},
	}); err != nil {
		t.Fatal(err)
	}
	waitInbox(t, svc, 1)

	start := time.Now()
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "connector.unhealthy", Entity: "connector", EntityID: "chat", Action: "updated",
		Attrs: map[string]string{"connector": "chat", "status": "down"},
	}); err != nil {
		t.Fatal(err)
	}
	waitInbox(t, svc, 2)
	if time.Since(start) > time.Second {
		t.Fatal("consume blocked by slow notify")
	}
	cancel()
}

// TestDeliverSkipsResolvedItem: an alert that resolves while it waits in the
// delivery queue sends nothing.
func TestDeliverSkipsResolvedItem(t *testing.T) {
	t.Parallel()
	ch := &chanSender{gate: make(chan struct{})}
	svc, st := routeSvc(t, alerts.Options{TickInterval: 10 * time.Millisecond},
		[]notify.Step{{Via: "whatsapp", To: "+1555"}}, nil, ch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	unhealthy := func(name string) {
		t.Helper()
		if _, err := st.Append(ctx, store.BusEvent{
			Topic: "connector.unhealthy", Entity: "connector", EntityID: name, Action: "updated",
			Attrs: map[string]string{"connector": name, "status": "down"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	unhealthy("mail") // blocks the delivery goroutine at the gate
	waitInbox(t, svc, 1)
	for _, ev := range []store.BusEvent{
		{Topic: "auth.probe.failed", Entity: "harness", EntityID: "grok", Action: "updated",
			Attrs: map[string]string{"harness": "grok", "source": "probe", "status": "expired"}},
		{Topic: "harness.updated", Entity: "harness", EntityID: "grok", Action: "updated",
			Attrs: map[string]string{"harness": "grok", "auth_ok": "true", "auth_status": "ok"}},
	} {
		if _, err := st.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	grokDone := func() bool {
		var done bool
		err := st.DB().QueryRowContext(ctx, `SELECT done_at IS NOT NULL FROM inbox_items WHERE subject='harness/grok'`).Scan(&done)
		return err == nil && done
	}
	deadline := time.Now().Add(10 * time.Second)
	for !grokDone() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	unhealthy("chat") // queued after the auth alert
	waitInbox(t, svc, 3)
	close(ch.gate)
	for len(ch.sent()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var subjects []string
	for _, r := range ch.sent() {
		item, err := svc.Get(ctx, r.InboxID)
		if err != nil {
			t.Fatal(err)
		}
		subjects = append(subjects, item.Subject)
	}
	if len(subjects) != 2 || slices.Contains(subjects, "harness/grok") {
		t.Fatalf("sent %v, want the two connector alerts only", subjects)
	}
}

// testLayers sets only the global layer.
type testLayers struct{ route []notify.Step }

func (testLayers) JobLayer(context.Context, string, string) ([]notify.Step, bool, []string, string) {
	return nil, false, nil, ""
}

func (testLayers) AgentLayer(context.Context, string, string) ([]notify.Step, bool, []string) {
	return nil, false, nil
}

func (testLayers) WorkspaceLayer(context.Context, string) ([]notify.Step, bool, []string) {
	return nil, false, nil
}

func (l testLayers) Global() ([]notify.Step, bool, []string) {
	return l.route, l.route != nil, []string{"ops@example.com"}
}

// chanSender is a connector send command that knows only whatsapp.
type chanSender struct {
	mu    sync.Mutex
	fail  bool
	delay time.Duration
	gate  chan struct{} // nil sends at once; else each send waits for close
	rows  []outbox.Row
}

func (c *chanSender) ChannelProblem(name, _ string) string {
	if name != "whatsapp" {
		return notify.ProblemNoConn
	}
	return ""
}

func (c *chanSender) Deliver(ctx context.Context, r outbox.Row) error {
	if c.gate != nil {
		select {
		case <-ctx.Done():
		case <-c.gate:
		}
	}
	if c.delay > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(c.delay):
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, r)
	if c.fail {
		return outbox.Permanent{Err: errors.New("send failed")}
	}
	return nil
}

func (c *chanSender) sent() []outbox.Row {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]outbox.Row(nil), c.rows...)
}

func TestUsageWarningOncePerCrossing(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	setUsage := func(pct float64) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO harness_usage
			(harness, window_key, used_percent, source, observed_at) VALUES ('claude', 'weekly', ?, 'test', '')
			ON CONFLICT (harness, window_key, scope, minutes) DO UPDATE SET used_percent=excluded.used_percent`, pct); err != nil {
			t.Fatal(err)
		}
	}
	unread := func() []alerts.InboxItem {
		t.Helper()
		got, err := svc.List(ctx, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Unread: true}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return got.Items
	}

	setUsage(85)
	for range 5 {
		svc.Tick(ctx)
	}
	open := unread()
	if len(open) != 1 || open[0].Title != "Claude Code used 85% of its weekly limit" {
		t.Fatalf("want one plain warning, got %+v", open)
	}

	// A user archive while usage stays high does not raise again.
	if _, err := svc.Apply(ctx, alerts.OpArchive, alerts.InboxSelector{IDs: []string{open[0].ID}}); err != nil {
		t.Fatal(err)
	}
	svc.Tick(ctx)
	if got := unread(); len(got) != 0 {
		t.Fatalf("re-raised while still high: %+v", got)
	}

	// A drop below the threshold re-arms the warning.
	setUsage(10)
	svc.Tick(ctx)
	setUsage(90)
	svc.Tick(ctx)
	if got := unread(); len(got) != 1 {
		t.Fatalf("want a new warning after re-arm, got %+v", got)
	}
}

func delivered(t *testing.T, st *store.Store) []store.StreamEvent {
	t.Helper()
	events, err := st.ListBus(context.Background(), 0, []string{"alert.delivered"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func waitInbox(t *testing.T, svc *alerts.Service, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		items, _ := listAll(context.Background(), svc, alerts.ListOpts{})
		if len(items) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("inbox never reached %d", n)
}

// sendWorker is the smallest Worker the native channel tests need: it
// records send bodies and can fail every send.
type sendWorker struct {
	mu      sync.Mutex
	token   string
	failAll bool
	sends   []email.SendRequest
}

func (w *sendWorker) handler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+w.token {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req email.SendRequest
		_ = json.Unmarshal(raw, &req)
		w.mu.Lock()
		w.sends = append(w.sends, req)
		fail := w.failAll
		w.mu.Unlock()
		if fail {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"message_id": req.MessageID})
	})
}

func (w *sendWorker) sent() []email.SendRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]email.SendRequest(nil), w.sends...)
}

func openAlertsStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testSvc(t *testing.T, opts alerts.Options) (*alerts.Service, *store.Store) {
	t.Helper()
	st := openAlertsStore(t)
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC) }
	}
	return alerts.New(st, opts), st
}

func TestNotifyDoctorFindings(t *testing.T) {
	t.Parallel()
	w := &sendWorker{token: "test-token-0000"}
	svc, _ := routeSvc(t, alerts.Options{}, []notify.Step{{Via: "nope"}, {Via: "email"}}, w, nil)
	var codes []string
	for _, f := range svc.CollectDoctorFindings() {
		if strings.HasPrefix(f.Code, "notify_") {
			codes = append(codes, f.Code+":"+f.Message)
		}
	}
	if len(codes) != 1 || !strings.Contains(codes[0], "notify_route_invalid:notify route of global: step 0 (nope)") {
		t.Fatalf("findings %v", codes)
	}
}

// routeSvc builds a service whose global route is route (nil = unset). A
// nil worker leaves email off; a nil channel sender knows no connector.
func routeSvc(t *testing.T, opts alerts.Options, route []notify.Step, w *sendWorker, ch *chanSender) (*alerts.Service, *store.Store) {
	t.Helper()
	st := openAlertsStore(t)
	ob := outbox.New(st)
	one := outbox.Policy{Base: time.Second, Max: time.Second, Attempts: 1}
	var em *email.Service
	if w != nil {
		em = mailService(t, st, ob, w, one)
	}
	if ch != nil {
		ob.Register(outbox.KindChannel, ch, one)
	}
	opts.Email, opts.Outbox = em, ob
	opts.Router = notify.NewResolver(testLayers{route: route}, ch, func() bool { return em != nil && em.Configured() })
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return alerts.New(st, opts), st
}

// settle consumes the bus until each route step frame is handled.
func settle(ctx context.Context, svc *alerts.Service) {
	for range 4 {
		svc.Consume(ctx)
	}
}

func deliveredAttrs(t *testing.T, st *store.Store) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, ev := range delivered(t, st) {
		var attrs map[string]string
		_ = json.Unmarshal(ev.Attrs, &attrs)
		out = append(out, attrs)
	}
	return out
}

func mailService(t *testing.T, st *store.Store, ob *outbox.Service, w *sendWorker, p outbox.Policy) *email.Service {
	t.Helper()
	srv := httptest.NewServer(w.handler())
	t.Cleanup(srv.Close)
	s := email.New(st)
	ob.Register(outbox.KindEmail, s.Outbound(), p)
	s.WithOutbox(ob)
	s.Configure(&email.Settings{
		Worker: srv.URL, Token: w.token, Domain: "mail.example.com",
		Admins: []string{"ops@example.com"}, FromName: "Trebi",
	}, email.NewClient(srv.URL, w.token))
	return s
}

// TestConnectorAuthRequired proves a login alert per connection with a Log
// in action on its page, and that a connected status resolves it.
func TestConnectorAuthRequired(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	const id = "con_01k6a0000000000000000000ab"
	handle := func(ev store.BusEvent) {
		t.Helper()
		if _, err := st.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
		frames, err := st.ListBusWith(ctx, store.ListBusOpts{Topics: []string{ev.Topic}, Limit: 100})
		if err != nil || len(frames) == 0 {
			t.Fatalf("frames %v %v", frames, err)
		}
		svc.HandleEvent(ctx, frames[len(frames)-1])
	}
	handle(store.BusEvent{Topic: "connector.auth.required", Entity: "connector", EntityID: id, Action: "updated",
		Attrs: map[string]string{"instance": id, "reason": "logged_out"}})
	items, _ := listAll(ctx, svc, alerts.ListOpts{})
	if len(items) != 1 || items[0].Subject != "connector/"+id || items[0].AlertKind != string(alerts.KindConnectorAuthRequired) {
		t.Fatalf("items %+v", items)
	}
	if acts := items[0].Actions; len(acts) != 1 || acts[0].Kind != alerts.ActionLogIn || acts[0].Route != "/connectors/"+id+"?tab=login" {
		t.Fatalf("actions %+v", acts)
	}
	handle(store.BusEvent{Topic: "connector.status", Entity: "connector", EntityID: id, Action: "updated",
		Attrs: map[string]string{"instance": id, "state": "connected"}})
	if open, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Unread: true}}); len(open) != 0 {
		t.Fatalf("open after connected: %+v", open)
	}

	// An OAuth login that ends: one item until the next login.
	const msg = "Granola ended the login. Log in again to use it in jobs."
	ended := store.BusEvent{Topic: "connector.auth.required", Entity: "connector", EntityID: id, Action: "updated",
		Attrs: map[string]string{"instance": id, "reason": "login_ended", "message": msg}}
	handle(ended)
	handle(ended)
	open, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Unread: true}})
	if len(open) != 1 {
		t.Fatalf("open after the login ended: %+v", open)
	}
	if it, err := svc.Get(ctx, open[0].ID); err != nil || !strings.HasSuffix(it.Body, " Details: "+msg) ||
		strings.Contains(it.Title+it.Body, "con_") {
		t.Fatalf("item %+v, %v", it, err)
	}
	handle(store.BusEvent{Topic: "connector.auth.completed", Entity: "connector", EntityID: id, Action: "updated",
		Attrs: map[string]string{"instance": id}})
	if open, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Unread: true}}); len(open) != 0 {
		t.Fatalf("open after a new login: %+v", open)
	}
}

// TestConnectorUpdateAvailable proves an update item with an Update action,
// and that a global version change resolves it.
func TestConnectorUpdateAvailable(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	handle := func(ev store.BusEvent) {
		t.Helper()
		if _, err := st.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
		frames, err := st.ListBusWith(ctx, store.ListBusOpts{Topics: []string{ev.Topic}, Limit: 100})
		if err != nil || len(frames) == 0 {
			t.Fatalf("frames %v %v", frames, err)
		}
		svc.HandleEvent(ctx, frames[len(frames)-1])
	}
	handle(store.BusEvent{Topic: "connector.update.available", Entity: "connector", EntityID: "whatsapp", Action: "update.available",
		Attrs: map[string]string{"version": "0.7.0", "current": "0.6.0"}})
	items, _ := listAll(ctx, svc, alerts.ListOpts{})
	if len(items) != 1 || items[0].Subject != "connector/whatsapp" || items[0].AlertKind != string(alerts.KindConnectorUpdate) {
		t.Fatalf("items %+v", items)
	}
	if acts := items[0].Actions; len(acts) != 1 || acts[0].Kind != alerts.ActionUpdate || acts[0].Route != "/v1/connectors/whatsapp/update" {
		t.Fatalf("actions %+v", acts)
	}
	handle(store.BusEvent{Topic: "connector.updated.version", Entity: "connector", EntityID: "whatsapp", Action: "updated.version",
		Attrs: map[string]string{"from": "0.6.0", "to": "0.7.0", "workspace": "ws1"}})
	if open, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Unread: true}}); len(open) != 1 {
		t.Fatalf("a workspace update must not resolve: %+v", open)
	}
	handle(store.BusEvent{Topic: "connector.updated.version", Entity: "connector", EntityID: "whatsapp", Action: "updated.version",
		Attrs: map[string]string{"from": "0.6.0", "to": "0.7.0"}})
	if open, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Unread: true}}); len(open) != 0 {
		t.Fatalf("open after update: %+v", open)
	}
}

func requestEvent(topic string) store.BusEvent {
	return store.BusEvent{
		Topic: topic, Entity: "request", EntityID: "R1", Action: "updated",
		Attrs: map[string]string{"run_id": "r1", "job_id": "jb_00000000c3", "job_name": "mailer", "workspace_id": "ws", "title": "Send the reply"},
	}
}

func TestRequestItemLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	w := &sendWorker{token: "test-token-0000"}
	forms := []requests.Form{{Type: requests.TypeReview, Title: "Send the reply", Payload: requests.Payload{Draft: &requests.Draft{Format: "text", Text: "Hi Ana, yes."}}}}
	summary := "Ana asks for a refund."
	svc, st := routeSvc(t, alerts.Options{
		ApprovalAfter: time.Minute,
		PublicURL:     "https://trebi.example.com",
		Now:           func() time.Time { return now },
		Request: func(_ context.Context, id string) (requests.Request, error) {
			return requests.Request{ID: id, Title: "Send the reply", Summary: summary, DeliveryError: "no session", Forms: forms}, nil
		},
	}, nil, w, nil)
	ctx := context.Background()
	if _, err := st.Append(ctx, requestEvent("request.created")); err != nil {
		t.Fatal(err)
	}
	svc.Tick(ctx)
	items, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Kind: alerts.InboxRequest}})
	if len(items) != 1 {
		t.Fatalf("want one request item, got %d", len(items))
	}
	item, err := svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.RequestID != "R1" || item.Body != "Ana asks for a refund." || item.Subject != "request/R1" {
		t.Fatalf("item %+v", item)
	}
	route := "/run/r1?tab=session&request=R1"
	if len(item.Actions) != 1 || item.Actions[0].Route != route {
		t.Fatalf("actions %+v", item.Actions)
	}
	if item.EscalatesAt == nil || !item.EscalatesAt.Equal(now.Add(time.Minute)) || len(w.sent()) != 0 {
		t.Fatalf("escalates_at %v, sends %d", item.EscalatesAt, len(w.sent()))
	}

	now = now.Add(2 * time.Minute)
	svc.Tick(ctx)
	settle(ctx, svc)
	sends := w.sent()
	if len(sends) != 1 {
		t.Fatalf("sends %d", len(sends))
	}
	for _, want := range []string{"Hi Ana, yes.", "Ana asks for a refund.", "Reply approve or reject on the first line.", "Answer: https://trebi.example.com" + route} {
		if !strings.Contains(sends[0].Text, want) {
			t.Errorf("mail lacks %q:\n%s", want, sends[0].Text)
		}
	}
	if !strings.Contains(sends[0].HTML, `href="https://trebi.example.com/run/r1?tab=session&amp;request=R1"`) {
		t.Errorf("html lacks the button:\n%s", sends[0].HTML)
	}

	forms = append(forms, requests.Form{Idx: 1, Type: requests.TypeConfirm, Title: "Close the ticket"})
	summary = ""
	if _, err := st.Append(ctx, requestEvent("request.updated")); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	if item, _ = svc.Get(ctx, item.ID); item.Body != "Send the reply; Close the ticket" {
		t.Fatalf("updated item %+v", item)
	}

	if _, err := st.Append(ctx, requestEvent("request.answered")); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	if item, _ = svc.Get(ctx, item.ID); item.DoneBy != alerts.DoneBySystem {
		t.Fatalf("status %s", item.DoneBy)
	}

	if _, err := st.Append(ctx, requestEvent("request.delivery_failed")); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	items, _ = listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Kind: alerts.InboxFailure}})
	if len(items) != 1 || items[0].Title != "mailer did not get your answer" || items[0].RequestID != "R1" {
		t.Fatalf("failure items %+v", items)
	}
}

// TestAlertMailLink: a mail of an item with no UI route links to the
// inbox item.
func TestAlertMailLink(t *testing.T) {
	t.Parallel()
	w := &sendWorker{token: "test-token-0000"}
	svc, _ := routeSvc(t, alerts.Options{PublicURL: "https://trebi.example.com/"}, nil, w, nil)
	ctx := context.Background()
	id, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindMachineDown, Title: "down"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, RunID: "r1", Title: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	sends := w.sent()
	if len(sends) != 2 {
		t.Fatalf("sends %+v", sends)
	}
	links := map[string]string{
		"[Trebi] down": "Open in Trebi: https://trebi.example.com/inbox/" + id + "\n",
		"[Trebi] wait": "Open session: https://trebi.example.com/run/r1?tab=session\n",
	}
	for _, m := range sends {
		if want := links[m.Subject]; want == "" || !strings.HasSuffix(m.Text, want) {
			t.Errorf("%s: text %q, want suffix %q", m.Subject, m.Text, want)
		}
	}
	if !strings.Contains(sends[0].HTML+sends[1].HTML, ">Open session</a>") {
		t.Errorf("html lacks the button label")
	}
	_ = run
}

func TestRequestItemApprovalDisabled(t *testing.T) {
	t.Parallel()
	w := &sendWorker{token: "test-token-0000"}
	svc, st := routeSvc(t, alerts.Options{ApprovalDisabled: true}, nil, w, nil)
	ctx := context.Background()
	if _, err := st.Append(ctx, requestEvent("request.created")); err != nil {
		t.Fatal(err)
	}
	svc.Tick(ctx)
	settle(ctx, svc)
	items, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Kind: alerts.InboxRequest}})
	if len(items) != 1 || items[0].EscalatesAt != nil || len(w.sent()) != 0 {
		t.Fatalf("items %+v, sends %d", items, len(w.sent()))
	}
}

// TestConnectorSubscriptionAlerts proves one item for each watch kind, and
// that a clean sync resolves both.
func TestConnectorSubscriptionAlerts(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	synced := func(attrs map[string]string) {
		t.Helper()
		if _, err := st.Append(ctx, store.BusEvent{Topic: "connector.subscriptions.synced", Entity: "connector", EntityID: "con_1", Action: "subscriptions.synced", Attrs: attrs}); err != nil {
			t.Fatal(err)
		}
		frames, err := st.ListBusWith(ctx, store.ListBusOpts{Topics: []string{"connector.subscriptions.synced"}, Limit: 100})
		if err != nil || len(frames) == 0 {
			t.Fatalf("frames %v %v", frames, err)
		}
		svc.HandleEvent(ctx, frames[len(frames)-1])
	}
	bad := map[string]string{"active": "0", "errors": "1", "error": "no access", "action_required": "1", "action": "Paste the code"}
	synced(bad)
	synced(bad)
	items, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Unread: true}})
	if len(items) != 2 {
		t.Fatalf("items %+v", items)
	}
	for _, it := range items {
		if it.Subject != "connector/con_1" || len(it.Actions) != 1 || it.Actions[0].Route != "/connectors/con_1?tab=events" {
			t.Fatalf("item %+v", it)
		}
	}
	synced(map[string]string{"active": "2", "errors": "0", "action_required": "0"})
	if open, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Unread: true}}); len(open) != 0 {
		t.Fatalf("open after clean sync: %+v", open)
	}
}

type pushSpy struct {
	mu   sync.Mutex
	sent []cloudlink.Alert
}

func (p *pushSpy) SendAlert(_ context.Context, a cloudlink.Alert) error {
	p.mu.Lock()
	p.sent = append(p.sent, a)
	p.mu.Unlock()
	return nil
}

// TestRaisePushesApproval sends one push frame for an approval item, with the
// inbox item id, and none for a health item (plan 2026-10-09 B12).
func TestRaisePushesApproval(t *testing.T) {
	t.Parallel()
	spy := &pushSpy{}
	svc, _ := testSvc(t, alerts.Options{Push: spy})
	ctx := context.Background()
	id, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, Subject: "run/1", Title: "Agent waiting", RunID: "run1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindAuthMissing, Subject: "harness/claude", Title: "Log in"}); err != nil {
		t.Fatal(err)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.sent) != 1 {
		t.Fatalf("pushes: %+v", spy.sent)
	}
	a := spy.sent[0]
	if a.MessageID != id || a.Kind != "approval" || a.Summary != "Agent waiting" || a.Cursor == "" {
		t.Fatalf("push frame: %+v", a)
	}
}

// listAll lists open and Done items, newest first. A set View or Unread
// lists that one view only.
func listAll(ctx context.Context, svc *alerts.Service, opts alerts.ListOpts) ([]alerts.InboxItem, error) {
	page, err := svc.List(ctx, opts)
	if err != nil || opts.View != "" || opts.Unread {
		return page.Items, err
	}
	opts.View = alerts.ViewDone
	done, err := svc.List(ctx, opts)
	if err != nil {
		return nil, err
	}
	items := append(page.Items, done.Items...)
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID > items[j].ID
	})
	return items, nil
}

// TestConnectorStatusOutage proves that a status outage raises one item with
// the adapter message, that connector.recovered
// resolves it, and that a second outage after the auto-resolve raises a new
// item in spite of the cooldown.
func TestConnectorStatusOutage(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	emit := func(topic string, attrs map[string]string) {
		t.Helper()
		attrs["instance"] = "con_1"
		attrs["slug"] = "whatsapp-ellis"
		if _, err := st.Append(ctx, store.BusEvent{
			Topic: topic, Entity: "connector", EntityID: "con_1", Action: "updated", Attrs: attrs,
		}); err != nil {
			t.Fatal(err)
		}
		svc.Consume(ctx)
	}
	open := func() []alerts.InboxItem {
		t.Helper()
		items, err := listAll(ctx, svc, alerts.ListOpts{})
		if err != nil {
			t.Fatal(err)
		}
		var live []alerts.InboxItem
		for _, it := range items {
			got, err := svc.Get(ctx, it.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Done {
				live = append(live, *got)
			}
		}
		return live
	}
	msg := "another client opened this WhatsApp session; restart the connector"
	emit("connector.unhealthy", map[string]string{"status": "error", "message": msg, "title": "WhatsApp Ellis"})
	items := open()
	if len(items) != 1 || items[0].Title != "A connection stopped working" || items[0].Body != msg {
		t.Fatalf("items %+v", items)
	}
	first := items[0].ID
	emit("connector.recovered", map[string]string{"status": "listening"})
	if got, err := svc.Get(ctx, first); err != nil || got.DoneBy != alerts.DoneBySystem {
		t.Fatalf("not resolved: %+v %v", got, err)
	}
	emit("connector.unhealthy", map[string]string{"status": "connecting", "message": "no network"})
	items = open()
	if len(items) != 1 || items[0].ID == first {
		t.Fatalf("second outage: %+v", items)
	}
	if items[0].Title != "A connection stopped working" || items[0].Body != "Trebi could not connect it for 10 minutes. no network" {
		t.Fatalf("second item %q %q", items[0].Title, items[0].Body)
	}
}
