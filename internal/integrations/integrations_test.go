package integrations_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/integrations"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// TestIntegrations_UpsertListSaveRoundTrip writes definition to integrations.yaml
// and lists it back through the real service API.
func TestIntegrations_UpsertListSaveRoundTrip(t *testing.T) {
	_, _, _, svc := openStack(t)
	ctx := context.Background()
	d := integrations.Definition{
		Name:    "line-echo",
		Type:    "stream",
		Command: "echo hello",
		Format:  "line",
	}
	if err := svc.Upsert(ctx, d); err != nil {
		t.Fatal(err)
	}
	// File must exist
	if _, err := os.Stat(home.IntegrationsPath()); err != nil {
		t.Fatal(err)
	}
	list := svc.List()
	if len(list) != 1 || list[0]["name"] != "line-echo" {
		t.Fatalf("list=%v", list)
	}
	got, err := svc.Get("line-echo")
	if err != nil {
		t.Fatal(err)
	}
	if got["command"] != "echo hello" {
		t.Fatalf("%v", got)
	}
	if err := svc.Delete(ctx, "line-echo"); err != nil {
		t.Fatal(err)
	}
	if len(svc.List()) != 0 {
		t.Fatalf("after delete: %v", svc.List())
	}
}

// TestIntegrations_StartStopLifecycle runs a short-lived stream process and
// observes listening → stopped status transitions.
func TestIntegrations_StartStopLifecycle(t *testing.T) {
	_, _, _, svc := openStack(t)
	ctx := context.Background()
	// Keepalive stream: print a line every 200ms until killed.
	d := integrations.Definition{
		Name:    "tick",
		Type:    "stream",
		Command: `while true; do echo '{"n":1}'; sleep 0.2; done`,
		Format:  "json",
	}
	if err := svc.Upsert(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "tick"); err != nil {
		t.Fatal(err)
	}
	// Wait until status is listening
	deadline := time.Now().Add(3 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		row, _ := svc.Get("tick")
		status, _ = row["status"].(string)
		if status == "listening" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != "listening" {
		t.Fatalf("status=%s want listening", status)
	}
	// Events must accrue — poll until at least one arrives.
	deadline = time.Now().Add(3 * time.Second)
	gotEvents := false
	for time.Now().Before(deadline) {
		row, _ := svc.Get("tick")
		if eventCount(row["events"]) >= 1 {
			gotEvents = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gotEvents {
		row, _ := svc.Get("tick")
		t.Fatalf("no events observed: %v", row)
	}
	if err := svc.Stop("tick"); err != nil {
		t.Fatal(err)
	}
	// An intentional Stop must end as "stopped", never "crashed".
	deadline = time.Now().Add(2 * time.Second)
	var st string
	for time.Now().Before(deadline) {
		row, _ := svc.Get("tick")
		st, _ = row["status"].(string)
		if st == "stopped" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st != "stopped" {
		t.Fatalf("after stop: status=%s want stopped", st)
	}
}

// TestIntegrations_ConcurrentStartSingleProcess proves the slot reservation:
// two concurrent Starts spawn exactly one subprocess.
func TestIntegrations_ConcurrentStartSingleProcess(t *testing.T) {
	_, _, _, svc := openStack(t)
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "pids")
	d := integrations.Definition{
		Name:    "once",
		Type:    "stream",
		Command: `echo started >> "$OUT"; sleep 3`,
		Format:  "line",
		Env:     map[string]string{"OUT": out},
	}
	if err := svc.Upsert(ctx, d); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.Start(ctx, "once"); err != nil {
				t.Errorf("start: %v", err)
			}
		}()
	}
	wg.Wait()
	t.Cleanup(func() { _ = svc.Stop("once") })
	// Wait for at least one launch line, then a grace window for a second.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Fatalf("subprocess launched %d times; want 1 (%q)", n, b)
	}
}

// TestIntegrations_NonZeroExitCrashes: a self-exit with a non-zero code must
// mark the integration crashed and emit integration.updated on the bus.
func TestIntegrations_NonZeroExitCrashes(t *testing.T) {
	st, _, _, svc := openStack(t)
	ctx := context.Background()
	d := integrations.Definition{Name: "boom", Type: "stream", Command: "echo one; exit 1", Format: "line"}
	if err := svc.Upsert(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "boom"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, "boom", "crashed")
	row, _ := svc.Get("boom")
	if le, _ := row["last_err"].(string); le == "" {
		t.Fatalf("want non-empty last_err: %v", row)
	}
	// Bus must carry the terminal status (the append follows the status flip).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		evs, err := st.ListBus(ctx, 0, []string{"integration"}, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			if ev.Topic == "integration.updated" && strings.Contains(string(ev.Attrs), "crashed") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no integration.updated with status crashed on bus")
}

// TestIntegrations_CleanSelfExitStops: a zero-exit self-termination ends as
// "stopped", not "crashed".
func TestIntegrations_CleanSelfExitStops(t *testing.T) {
	_, _, _, svc := openStack(t)
	ctx := context.Background()
	d := integrations.Definition{Name: "brief", Type: "stream", Command: "echo one; exit 0", Format: "line"}
	if err := svc.Upsert(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "brief"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, "brief", "stopped")
	row, _ := svc.Get("brief")
	if le, _ := row["last_err"].(string); le != "" {
		t.Fatalf("clean exit set last_err=%q", le)
	}
}

// TestIntegrations_InjectEventMatchesJob enqueues via handleLine when a job
// subscribes with match true.
func TestIntegrations_InjectEventMatchesJob(t *testing.T) {
	st, ws, _, svc := openStack(t)
	ctx := context.Background()

	wsPath := filepath.Join(home.Base(), "ws")
	jobDir := filepath.Join(wsPath, "sub")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Event trigger on integration "src"
	md := "---\nharness: fake\ntriggers:\n  - event: src\n    match: \"true\"\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Add(ctx, wsPath, "inj-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}

	// Register integration def (inject works even without Start)
	_ = svc.Upsert(ctx, integrations.Definition{Name: "src", Format: "json", Command: "true"})

	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"n": 42}); err != nil {
		t.Fatal(err)
	}

	// Durable event row
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE integration='src'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatal("expected events row")
	}
	// Queue should have a pending/claimed/done item for job sub
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job='sub'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatal("expected queue row from match")
	}
}

// TestIntegrations_AllowlistGate proves the B3 ingress allowlist: a sender not
// on the list is blocked before match; a listed sender enqueues.
func TestIntegrations_AllowlistGate(t *testing.T) {
	st, ws, _, svc := openStack(t)
	ctx := context.Background()

	wsPath := filepath.Join(home.Base(), "ws")
	jobDir := filepath.Join(wsPath, "guard")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\ntriggers:\n  - event: src\n    match: \"true\"\n    allowlist: [\"alice\"]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Add(ctx, wsPath, "allow-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	_ = svc.Upsert(ctx, integrations.Definition{Name: "src", Format: "json", Command: "true"})

	// Unlisted sender → blocked outcome, no queue row.
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"sender": "mallory", "n": 1}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job='guard'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("blocked sender enqueued %d row(s); want 0", n)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes WHERE job='guard' AND outcome='blocked'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("blocked outcome rows=%d want 1", n)
	}

	// Event with no identity field → also blocked (deny by default).
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"n": 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job='guard'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("identity-less event enqueued %d row(s); want 0", n)
	}

	// Listed sender → queue row.
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"sender": "alice", "n": 3}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job='guard'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("allowed sender queue rows=%d want 1", n)
	}
}

// TestIntegrations_AllowlistScopedAndNumeric proves scoped "key:value"
// entries bind to one field, and numeric identity values match.
func TestIntegrations_AllowlistScopedAndNumeric(t *testing.T) {
	st, ws, _, svc := openStack(t)
	ctx := context.Background()

	wsPath := filepath.Join(home.Base(), "ws")
	jobDir := filepath.Join(wsPath, "scoped")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\ntriggers:\n  - event: src\n    match: \"true\"\n    allowlist: [\"sender:alice\", \"12345\"]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Add(ctx, wsPath, "scoped-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	_ = svc.Upsert(ctx, integrations.Definition{Name: "src", Format: "json", Command: "true"})

	queueRows := func() int {
		t.Helper()
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job='scoped'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Scoped "sender:alice" must NOT pass channel "alice".
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"channel": "alice"}); err != nil {
		t.Fatal(err)
	}
	if n := queueRows(); n != 0 {
		t.Fatalf("channel alice enqueued %d row(s); want 0", n)
	}

	// Scoped "sender:alice" must pass sender "alice".
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"sender": "alice"}); err != nil {
		t.Fatal(err)
	}
	if n := queueRows(); n != 1 {
		t.Fatalf("sender alice queue rows=%d want 1", n)
	}

	// Numeric chat_id 12345 (float64 after JSON decode) must match "12345".
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"chat_id": 12345}); err != nil {
		t.Fatal(err)
	}
	if n := queueRows(); n != 2 {
		t.Fatalf("numeric chat_id queue rows=%d want 2", n)
	}
}

func openStack(t *testing.T) (*store.Store, *workspaces.Service, *runs.Service, *integrations.Service) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := workspaces.New(st)
	r := runs.New(st)
	pol := dispatch.NewPolicy()
	svc := integrations.New(st, ws, r, pol)
	return st, ws, r, svc
}

// waitStatus polls Get until the integration reaches want (or times out).
func waitStatus(t *testing.T, svc *integrations.Service, name, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		row, _ := svc.Get(name)
		status, _ = row["status"].(string)
		if status == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("status=%s want %s", status, want)
}

// eventCount reads a numeric map value across possible int types.
func eventCount(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
