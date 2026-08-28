package alerts_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
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
		{alerts.KindIntegrationUnhealthy, alerts.SeverityBlocker, alerts.InboxHealth, 8 * time.Hour, "", true},
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
		Topic: "agent.state",
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
		Topic: "agent.state",
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
	n, err := svc.ReadAll(ctx)
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

func TestRunLoop(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{TickInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "integration.unhealthy", Entity: "integration", EntityID: "mail", Action: "updated",
		Attrs: map[string]string{"integration": "mail", "status": "crashed"},
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

func TestNotifyDelivered(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{
		Notify: []alerts.NotifyChannel{{Name: "email", Command: "true"}},
	})
	ctx := context.Background()
	if _, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, RunID: "r1", Title: "wait"}); err != nil {
		t.Fatal(err)
	}
	events, _ := st.ListBus(ctx, 0, []string{"alert.delivered"}, 10)
	found := false
	for _, ev := range events {
		if ev.Topic == "alert.delivered" {
			found = true
			var attrs map[string]string
			_ = json.Unmarshal(ev.Attrs, &attrs)
			if attrs["channel"] != "email" {
				t.Fatalf("attrs %v", attrs)
			}
		}
	}
	if !found {
		t.Fatal("missing alert.delivered")
	}
}

func testSvc(t *testing.T, opts alerts.Options) (*alerts.Service, *store.Store) {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC) }
	}
	return alerts.New(st, opts), st
}
