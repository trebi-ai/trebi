package alerts_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestResolveAllByKind(t *testing.T) {
	t.Parallel()
	svc, _ := testSvc(t, alerts.Options{})
	ctx := context.Background()
	health, _ := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindAuthMissing, Harness: "claude", Title: "Auth missing"})
	down, _ := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindMachineDown, Title: "machine down"})

	n, err := svc.ResolveAll(ctx, string(alerts.InboxHealth))
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

	if n, err = svc.ResolveAll(ctx, ""); err != nil || n != 1 {
		t.Fatalf("resolve rest %d %v", n, err)
	}
	if n, err = svc.ResolveAll(ctx, ""); err != nil || n != 0 {
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
			if attrs["channel"] != "email" || attrs["index"] != "0" {
				t.Fatalf("attrs %v", attrs)
			}
		}
	}
	if !found {
		t.Fatal("missing alert.delivered")
	}
}

func TestNotifyNoShellInjection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	pwned := filepath.Join(dir, "pwned")
	cmd := fmt.Sprintf("echo {{body}} {{message}} {{hint}} > %q", marker)
	status := fmt.Sprintf("ok`; touch %s; echo $(touch %s); echo \"; touch %s #\nboom", pwned, pwned, pwned)

	svc, st := testSvc(t, alerts.Options{
		Notify: []alerts.NotifyChannel{{Name: "email", Command: cmd}},
	})
	ctx := context.Background()
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "connector.unhealthy", Entity: "connector", EntityID: "mail", Action: "updated",
		Attrs: map[string]string{"connector": "mail", "status": status},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("payload executed")
	}
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

func TestNotifyChainStopsAtFirstSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	svc, st := testSvc(t, alerts.Options{
		Notify: []alerts.NotifyChannel{
			{Name: "email", Command: fmt.Sprintf("echo 1 > %q", first)},
			{Name: "whatsapp", Command: fmt.Sprintf("echo 2 > %q", second)},
		},
	})
	ctx := context.Background()
	if _, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, RunID: "r1", Title: "wait"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatal("first channel did not run")
	}
	if _, err := os.Stat(second); err == nil {
		t.Fatal("second channel ran after success")
	}
	events, _ := st.ListBus(ctx, 0, []string{"alert.delivered"}, 10)
	n := 0
	for _, ev := range events {
		if ev.Topic == "alert.delivered" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("delivered %d want 1", n)
	}
}

func TestNotifyChainFallsThrough(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	second := filepath.Join(dir, "second")
	svc, st := testSvc(t, alerts.Options{
		Notify: []alerts.NotifyChannel{
			{Name: "email", Command: "false"},
			{Name: "whatsapp", Command: fmt.Sprintf("echo 2 > %q", second)},
		},
	})
	ctx := context.Background()
	if _, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, RunID: "r1", Title: "wait"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatal("fallback did not run")
	}
	events, _ := st.ListBus(ctx, 0, []string{"alert.delivered"}, 10)
	var indexes []string
	for _, ev := range events {
		if ev.Topic != "alert.delivered" {
			continue
		}
		var attrs map[string]string
		_ = json.Unmarshal(ev.Attrs, &attrs)
		indexes = append(indexes, attrs["index"])
	}
	if len(indexes) != 2 || indexes[0] != "0" || indexes[1] != "1" {
		t.Fatalf("indexes %v", indexes)
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
			tc.opts.Notify = []alerts.NotifyChannel{{Name: "email", Command: "true"}}
			svc, st := testSvc(t, tc.opts)
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
			events, _ := st.ListBus(ctx, 0, []string{"alert.delivered"}, 10)
			for _, ev := range events {
				if ev.Topic == "alert.delivered" {
					t.Fatal("must not deliver")
				}
			}
		})
	}
}

func TestDeliverDoesNotBlockConsume(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{
		TickInterval:  15 * time.Millisecond,
		NotifyTimeout: 2 * time.Second,
		Notify:        []alerts.NotifyChannel{{Name: "slow", Command: "sleep 30"}},
	})
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
	got, err := svc.ListAlerts(ctx, time.Time{}, 2)
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
	filtered, err := svc.ListAlerts(ctx, since, 2)
	if err != nil || len(filtered) != 2 {
		t.Fatalf("since %d %v", len(filtered), err)
	}
	if filtered[0].Seq != all[4].Seq || filtered[1].Seq != all[3].Seq {
		t.Fatalf("since truncated: got %d,%d", filtered[0].Seq, filtered[1].Seq)
	}
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
