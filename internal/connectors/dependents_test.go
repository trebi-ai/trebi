package connectors_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
)

// TestTriggerDependents lists jobs, agents, and the assistant on a
// connector, with inactive jobs and disabled triggers, and proves that
// dispatch still skips those two.
func TestTriggerDependents(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "deps")
	files := map[string]string{
		"live/JOB.md":            "---\nharness: fake\ntriggers:\n  - event: email\n    match: \"true\"\n    rooms: [\"r1\"]\n    allowlist: [\"a@x.io\", \"b@x.io\"]\n---\n\nRun.\n",
		"off/JOB.md":             "---\nharness: fake\ndisabled: true\ntriggers:\n  - event: email\n    match: \"true\"\n---\n\nRun.\n",
		"paused/JOB.md":          "---\nharness: fake\ntriggers:\n  - cron: \"0 9 * * *\"\n  - event: email\n    match: \"true\"\n    disabled: true\n  - event: email\n    match: \"false\"\n---\n\nRun.\n",
		"other/JOB.md":           "---\nharness: fake\ntriggers:\n  - event: whatsapp\n---\n\nRun.\n",
		"agents/triage/AGENT.md": "---\ndescription: Test agent.\nharness: fake\ntriggers:\n  - event: email\n    match: \"true\"\n---\n\nTriage.\n",
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
	w, err := ws.Add(ctx, wsPath, "deps-ws")
	if err != nil {
		t.Fatal(err)
	}
	yml, err := os.ReadFile(filepath.Join(wsPath, "trebi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	yml = append(yml, "\nassistant:\n  triggers:\n    - event: email\n"...)
	if err := os.WriteFile(filepath.Join(wsPath, "trebi.yaml"), yml, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}

	type key struct {
		kind, name string
		on         int
	}
	got := map[key]connectors.TriggerDependent{}
	for _, d := range svc.TriggerDependents(ctx, "email") {
		if d.Workspace != w.ID {
			t.Fatalf("workspace = %q", d.Workspace)
		}
		got[key{d.Kind, d.Name, d.OnIndex}] = d
	}
	want := map[key][2]bool{ // active, disabled
		{"job", "live", 0}:            {true, false},
		{"job", "off", 0}:             {false, false},
		{"job", "paused", 0}:          {true, true},
		{"job", "paused", 1}:          {true, false},
		{"agent", "agents/triage", 0}: {true, false},
		{"assistant", "assistant", 0}: {true, false},
	}
	if len(got) != len(want) {
		t.Fatalf("dependents = %+v", got)
	}
	for k, flags := range want {
		d, ok := got[k]
		if !ok || d.Active != flags[0] || d.Disabled != flags[1] {
			t.Fatalf("%v = %+v (ok=%v)", k, d, ok)
		}
	}
	live := got[key{"job", "live", 0}]
	if !slices.Equal(live.Rooms, []string{"r1"}) || live.Senders != 2 {
		t.Fatalf("live = %+v", live)
	}
	if a := got[key{"agent", "agents/triage", 0}]; a.Rooms == nil || len(a.Rooms) != 0 {
		t.Fatalf("agent rooms = %#v", a.Rooms)
	}

	// The off job and the disabled trigger never enqueue.
	if _, err := svc.InjectNative(ctx, "email", map[string]any{"n": 1}, connectors.NativeMeta{SourceID: "d1", Room: "r9"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes WHERE job_id IN (SELECT id FROM job_index WHERE name IN ('off','paused')) AND outcome != 'no_match'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("skipped triggers fired %d", n)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes WHERE job_id=(SELECT id FROM job_index WHERE name='off')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("inactive job has %d outcomes", n)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='agents/triage')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("live agent enqueued %d", n)
	}
	if svc.TriggerDependents(ctx, "nope") == nil {
		t.Fatal("unknown slug gives nil")
	}
}
