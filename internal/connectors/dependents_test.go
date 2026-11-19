package connectors_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
)

// TestTriggerDependents lists the routes of a connector: agents, jobs of
// agents, the assistant, inactive targets, routes that get nothing, and a
// broken route. It proves that dispatch still skips the inactive target and
// the route that gets nothing.
func TestTriggerDependents(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "deps")
	files := map[string]string{
		"agents/live/AGENT.md":       "---\nharness: fake\n---\n\nRun.\n",
		"agents/off/AGENT.md":        "---\nharness: fake\ndisabled: true\n---\n\nRun.\n",
		"agents/paused/AGENT.md":     "---\nharness: fake\ntriggers:\n  - cron: \"0 9 * * *\"\n---\n\nRun.\n",
		"agents/triage/AGENT.md":     "---\ndescription: Test agent.\nharness: fake\n---\n\nTriage.\n",
		"agents/triage/reply/JOB.md": "---\nharness: fake\n---\n\nReply.\n",
		"agents/other/AGENT.md":      "---\nharness: fake\n---\n\nRun.\n",
	}
	for name, body := range files {
		p := filepath.Join(wsPath, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	every := routing.GetsEveryMessage
	routingtest.Write(t, wsPath, "email", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Agent: "live", Gets: every, Filter: job.TriggerYAML{Match: "true", Rooms: []string{"r1"}, Allowlist: []string{"a@x.io", "b@x.io"}}},
		{Agent: "off", Gets: every, Filter: job.TriggerYAML{Match: "true"}},
		{Agent: "paused", Gets: routing.GetsOff},
		{Job: "triage/reply", Gets: routing.GetsByHand},
		{Agent: "triage", Gets: every, Filter: job.TriggerYAML{Match: "true"}},
		{Assistant: true, Gets: every},
		{Agent: "ghost", Gets: every},
	}})
	routingtest.Write(t, wsPath, "whatsapp", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Agent: "other", Gets: every},
	}})
	w, err := ws.Add(ctx, wsPath, "deps-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}

	got := map[string]connectors.TriggerDependent{}
	for _, d := range svc.TriggerDependents(ctx, "email") {
		if d.Workspace != w.ID || !routing.ValidID(d.RouteID) {
			t.Fatalf("dependent = %+v", d)
		}
		got[d.Target] = d
	}
	want := map[string]struct {
		kind, name, gets string
		active, broken   bool
	}{
		"live":         {"agent", "live", every, true, false},
		"off":          {"agent", "off", every, false, false},
		"paused":       {"agent", "paused", routing.GetsOff, true, false},
		"triage/reply": {"job", "triage/reply", routing.GetsByHand, true, false},
		"triage":       {"agent", "triage", every, true, false},
		"assistant":    {"assistant", "assistant", every, true, false},
		"ghost":        {"agent", "ghost", every, true, true},
	}
	if len(got) != len(want) {
		t.Fatalf("dependents = %+v", got)
	}
	for target, w := range want {
		d, ok := got[target]
		if !ok || d.Kind != w.kind || d.Name != w.name || d.Gets != w.gets || d.Active != w.active || (d.Broken != "") != w.broken {
			t.Fatalf("%s = %+v (ok=%v)", target, d, ok)
		}
	}
	live := got["live"]
	if !slices.Equal(live.Rooms, []string{"r1"}) || live.Senders != 2 {
		t.Fatalf("live = %+v", live)
	}
	if a := got["triage"]; a.Rooms == nil || len(a.Rooms) != 0 {
		t.Fatalf("agent rooms = %#v", a.Rooms)
	}

	// The inactive agent and the route that gets nothing never enqueue.
	if _, err := svc.InjectNative(ctx, "email", map[string]any{"n": 1}, connectors.NativeMeta{SourceID: "d1", Room: "r9"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes WHERE job_id IN (SELECT id FROM job_index WHERE name IN ('off','paused')) AND outcome != 'no_match'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("skipped routes fired %d", n)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes WHERE job_id=(SELECT id FROM job_index WHERE name='off')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("inactive agent has %d outcomes", n)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='triage')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("live agent enqueued %d", n)
	}
	if svc.TriggerDependents(ctx, "nope") == nil {
		t.Fatal("unknown slug gives nil")
	}
}
