package alerts_test

import (
	"context"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/store"
)

func onlyOn(names ...string) func(string) bool {
	return func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
}

func appendAuthFailed(t *testing.T, st *store.Store, name, status string) {
	t.Helper()
	if _, err := st.Append(context.Background(), store.BusEvent{
		Topic: "auth.probe.failed", Entity: "harness", EntityID: name, Action: "updated",
		Attrs: map[string]string{"harness": name, "status": status},
	}); err != nil {
		t.Fatal(err)
	}
}

func openSubjects(t *testing.T, svc *alerts.Service) map[string]bool {
	t.Helper()
	items, err := listAll(context.Background(), svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{View: alerts.ViewInbox, Kind: alerts.InboxHealth}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, it := range items {
		out[it.Subject] = true
	}
	return out
}

func TestAuthMissingSkipsHarnessOffAndUnknown(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{HarnessOn: onlyOn("claude", "codex")})
	appendAuthFailed(t, st, "pi", "missing")
	appendAuthFailed(t, st, "claude", "unknown")
	appendAuthFailed(t, st, "codex", "missing")
	svc.Consume(context.Background())
	got := openSubjects(t, svc)
	if got["harness/pi"] || got["harness/claude"] || !got["harness/codex"] || len(got) != 1 {
		t.Fatalf("open subjects %v", got)
	}
}

func TestHarnessOffResolvesAlerts(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	appendAuthFailed(t, st, "pi", "missing")
	appendAuthFailed(t, st, "claude", "missing")
	svc.Consume(ctx)
	if got := openSubjects(t, svc); !got["harness/pi"] || !got["harness/claude"] {
		t.Fatalf("setup %v", got)
	}
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "harness.updated", Entity: "harness", EntityID: "pi", Action: "updated",
		Attrs: map[string]string{"harness": "pi", "enabled": "false"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	if got := openSubjects(t, svc); got["harness/pi"] || !got["harness/claude"] {
		t.Fatalf("after off %v", got)
	}
}

func TestRunResolvesAlertsOfHarnessOff(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	appendAuthFailed(t, st, "pi", "missing")
	appendAuthFailed(t, st, "claude", "missing")
	svc.Consume(ctx)

	// The daemon starts after the user turned pi off.
	svc2 := alerts.New(st, alerts.Options{HarnessOn: onlyOn("claude"), TickInterval: time.Hour})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { svc2.Run(runCtx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && openSubjects(t, svc)["harness/pi"] {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if got := openSubjects(t, svc); got["harness/pi"] || !got["harness/claude"] {
		t.Fatalf("after start %v", got)
	}
}
