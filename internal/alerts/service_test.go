package alerts_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/store"
)

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
	items, err := svc.List(ctx, alerts.ListOpts{Limit: 10})
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
	items, _ := svc.List(ctx, alerts.ListOpts{})
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
	if got.Status != alerts.InboxResolved {
		t.Fatalf("status %s", got.Status)
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
	if got.Status != alerts.InboxResolved {
		t.Fatalf("status %s", got.Status)
	}
}

func TestConsumeRunFailed(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "run.failed", Entity: "run", EntityID: "run1", Action: "updated",
		Attrs: map[string]string{"end_reason": "failed", "status": "failed", "job": "demo", "workspace": "ws"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	items, err := svc.List(ctx, alerts.ListOpts{})
	if err != nil || len(items) != 1 {
		t.Fatalf("%v %v", items, err)
	}
	if items[0].AlertKind != string(alerts.KindRunFailed) {
		t.Fatalf("kind %s", items[0].AlertKind)
	}
	// kill never pages
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "run.failed", Entity: "run", EntityID: "run2", Action: "updated",
		Attrs: map[string]string{"end_reason": "kill", "status": "failed", "job": "demo", "workspace": "ws"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	items, _ = svc.List(ctx, alerts.ListOpts{})
	if len(items) != 1 {
		t.Fatalf("kill should not raise, items=%d", len(items))
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
		Attrs: map[string]string{"end_reason": "idle_timeout", "status": "failed", "job": "demo", "workspace": "ws"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	items, err := svc.List(ctx, alerts.ListOpts{})
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	// The machine status stays failed; the outcome is what resolves the item.
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "run.updated", Entity: "run", EntityID: "run1", Action: "updated",
		Attrs: map[string]string{"status": "failed", "outcome": "complete", "job": "demo", "workspace": "ws"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	got, err := svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != alerts.InboxResolved {
		t.Fatalf("status %s want resolved", got.Status)
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
		Attrs: map[string]string{"run_id": "r1", "state": "waiting", "since": since, "workspace": "ws", "job": "j"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Tick(ctx)
	items, _ := svc.List(ctx, alerts.ListOpts{})
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
			return "Deploy to production?"
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
	items, _ := svc.List(ctx, alerts.ListOpts{})
	if len(items) != 1 {
		t.Fatalf("want waiting item, got %d", len(items))
	}
	got, err := svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "Deploy to production?" || got.Hint != "Reply approve or deny by mail" {
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
	if items, _ := svc.List(ctx, alerts.ListOpts{}); len(items) != 0 {
		t.Fatalf("raised before dwell: %v", items)
	}

	now = now.Add(6 * time.Minute)
	svc.Tick(ctx)
	items, _ := svc.List(ctx, alerts.ListOpts{})
	if len(items) != 1 {
		t.Fatalf("want waiting item, got %d", len(items))
	}
	got, err := svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AlertKind != "waiting" || got.Body != "Overwrite the nightly report?" {
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
	items, _ := svc.List(ctx, alerts.ListOpts{})
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
	if got.Body != "Deploy to production?" {
		t.Fatalf("body = %q", got.Body)
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
	if got.Status != alerts.InboxResolved {
		t.Fatalf("status = %q", got.Status)
	}
}

func TestSetStatusAndReadAll(t *testing.T) {
	t.Parallel()
	svc, _ := testSvc(t, alerts.Options{})
	ctx := context.Background()
	id, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindAuthMissing, Harness: "claude", Title: "Auth missing"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetStatus(ctx, id, alerts.InboxRead, "manual"); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, id)
	if got.Status != alerts.InboxRead || got.Revision < 2 {
		t.Fatalf("%+v", got)
	}
	id2, _ := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindLimitExceeded, Harness: "grok", Title: "limit"})
	n, err := svc.ReadAll(ctx, "")
	if err != nil || n < 1 {
		t.Fatalf("read-all %d %v", n, err)
	}
	got2, _ := svc.Get(ctx, id2)
	if got2.Status != alerts.InboxRead {
		t.Fatalf("status %s", got2.Status)
	}
	if err := svc.SetStatus(ctx, id, alerts.InboxResolved, "manual"); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Get(ctx, id)
	if got.ResolvedAt == nil {
		t.Fatal("resolved_at")
	}
}

func TestResolveAllByKind(t *testing.T) {
	t.Parallel()
	svc, _ := testSvc(t, alerts.Options{})
	ctx := context.Background()
	health, _ := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindAuthMissing, Harness: "claude", Title: "Auth missing"})
	down, _ := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindMachineDown, Title: "machine down"})

	n, err := svc.ResolveAll(ctx, string(alerts.InboxHealth), "")
	if err != nil || n != 1 {
		t.Fatalf("kind resolve %d %v", n, err)
	}
	got, _ := svc.Get(ctx, health)
	if got.Status != alerts.InboxResolved {
		t.Fatalf("health status %s", got.Status)
	}
	other, _ := svc.Get(ctx, down)
	if other.Status == alerts.InboxResolved {
		t.Fatal("other kind must stay open")
	}

	if n, err = svc.ResolveAll(ctx, "", ""); err != nil || n != 1 {
		t.Fatalf("resolve rest %d %v", n, err)
	}
	if n, err = svc.ResolveAll(ctx, "", ""); err != nil || n != 0 {
		t.Fatalf("second call must be a no-op: %d %v", n, err)
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
		items, _ := svc.List(ctx, alerts.ListOpts{})
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
	if err != nil || len(page) != 2 {
		t.Fatalf("%v %v", page, err)
	}
	rest, err := svc.List(ctx, alerts.ListOpts{Limit: 2, Cursor: page[1].ID})
	if err != nil || len(rest) != 1 {
		t.Fatalf("rest %v %v", rest, err)
	}
}

func TestDoctorSync(t *testing.T) {
	t.Parallel()
	svc, _ := testSvc(t, alerts.Options{})
	ctx := context.Background()
	svc.SyncDoctor(ctx, []alerts.DoctorFinding{
		{ID: "config-missing", Severity: "warn", Code: "config_missing", Message: "no config"},
	})
	items, _ := svc.List(ctx, alerts.ListOpts{Kind: string(alerts.InboxHealth)})
	if len(items) != 1 || items[0].Subject != "doctor/config-missing" {
		t.Fatalf("%+v", items)
	}
	if len(items[0].Actions) != 1 || items[0].Actions[0].Kind != alerts.ActionFix {
		t.Fatalf("actions %+v", items[0].Actions)
	}
	svc.SyncDoctor(ctx, nil)
	got, _ := svc.Get(ctx, items[0].ID)
	if got.Status != alerts.InboxResolved {
		t.Fatalf("stale doctor %s", got.Status)
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
	items, err := svc.List(ctx, alerts.ListOpts{})
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
	if want := "[trebi] wait: run/r1"; sends[0].Subject != want {
		t.Errorf("subject = %q, want %q", sends[0].Subject, want)
	}
	if !strings.Contains(sends[0].Text, "inbox: "+id) {
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
	if n := len(item.Actions); n == 0 || item.Actions[n-1].Kind != alerts.ActionViewOutbox {
		t.Fatalf("actions %+v", item.Actions)
	}
	items, _ := svc.List(ctx, alerts.ListOpts{})
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
	rows  []outbox.Row
}

func (c *chanSender) HasChannel(name, _ string) (bool, bool, bool) {
	return name == "whatsapp", name == "whatsapp", false
}

func (c *chanSender) Deliver(ctx context.Context, r outbox.Row) error {
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
		items, _ := svc.List(context.Background(), alerts.ListOpts{})
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

// TestConnectorAuthRequired proves a login alert per instance with a Log in
// action, and that a connected status resolves it.
func TestConnectorAuthRequired(t *testing.T) {
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
	handle(store.BusEvent{Topic: "connector.auth.required", Entity: "connector", EntityID: "chat", Action: "updated",
		Attrs: map[string]string{"instance": "chat@ws1", "reason": "logged_out"}})
	items, _ := svc.List(ctx, alerts.ListOpts{})
	if len(items) != 1 || items[0].Subject != "connector/chat@ws1" || items[0].AlertKind != string(alerts.KindConnectorAuthRequired) {
		t.Fatalf("items %+v", items)
	}
	if acts := items[0].Actions; len(acts) != 1 || acts[0].Kind != alerts.ActionLogIn || !strings.Contains(acts[0].Route, "instance=chat%40ws1") {
		t.Fatalf("actions %+v", acts)
	}
	handle(store.BusEvent{Topic: "connector.status", Entity: "connector", EntityID: "chat", Action: "updated",
		Attrs: map[string]string{"instance": "chat@ws1", "state": "connected"}})
	if open, _ := svc.List(ctx, alerts.ListOpts{Status: string(alerts.InboxUnread)}); len(open) != 0 {
		t.Fatalf("open after connected: %+v", open)
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
	items, _ := svc.List(ctx, alerts.ListOpts{})
	if len(items) != 1 || items[0].Subject != "connector/whatsapp" || items[0].AlertKind != string(alerts.KindConnectorUpdate) {
		t.Fatalf("items %+v", items)
	}
	if acts := items[0].Actions; len(acts) != 1 || acts[0].Kind != alerts.ActionUpdate || acts[0].Route != "/v1/connectors/whatsapp/update" {
		t.Fatalf("actions %+v", acts)
	}
	handle(store.BusEvent{Topic: "connector.updated.version", Entity: "connector", EntityID: "whatsapp", Action: "updated.version",
		Attrs: map[string]string{"from": "0.6.0", "to": "0.7.0", "workspace": "ws1"}})
	if open, _ := svc.List(ctx, alerts.ListOpts{Status: string(alerts.InboxUnread)}); len(open) != 1 {
		t.Fatalf("a workspace update must not resolve: %+v", open)
	}
	handle(store.BusEvent{Topic: "connector.updated.version", Entity: "connector", EntityID: "whatsapp", Action: "updated.version",
		Attrs: map[string]string{"from": "0.6.0", "to": "0.7.0"}})
	if open, _ := svc.List(ctx, alerts.ListOpts{Status: string(alerts.InboxUnread)}); len(open) != 0 {
		t.Fatalf("open after update: %+v", open)
	}
}
