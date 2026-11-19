package api_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPauseRoutes(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/demo")
	wsID := addWorkspace(t, base, wsDir, "pz-ws")

	code, p, _ := doJSON(t, "POST", base+"/v1/pauses", map[string]any{"scope": "workspace", "target": "pz-ws", "for": "2h"})
	if code != 201 || p["target"] != wsID || p["label"] != "pz-ws" || p["until"] == nil || p["active"] != true {
		t.Fatalf("set: %d %v", code, p)
	}
	id := p["id"].(string)

	code, jobs, _ := doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID, nil)
	list, _ := jobs["jobs"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("jobs: %d %v", code, jobs)
	}
	job := list[0].(map[string]any)
	held, _ := job["held_by"].(map[string]any)
	if job["status"] != "held" || held["pause_id"] != id {
		t.Fatalf("job not held: %v", job)
	}
	code, agents, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents", nil)
	alist, _ := agents["agents"].([]any)
	if code != 200 || len(alist) != 1 || alist[0].(map[string]any)["status"] != "held" {
		t.Fatalf("agents: %d %v", code, agents)
	}

	// One open pause per scope: a second set changes it.
	code, p, _ = doJSON(t, "POST", base+"/v1/pauses", map[string]any{"scope": "workspace", "target": wsID, "until": ""})
	if code != 200 || p["id"] != id || p["until"] != nil {
		t.Fatalf("second set: %d %v", code, p)
	}
	code, p, _ = doJSON(t, "PATCH", base+"/v1/pauses/"+id, map[string]any{"missed": "once", "note": "tokens"})
	if code != 200 || p["missed"] != "once" || p["note"] != "tokens" || p["until"] != nil {
		t.Fatalf("update: %d %v", code, p)
	}

	// A manual launch runs, with a warning.
	code, out, _ := doJSON(t, "POST", base+"/v1/jobs/ops%2Fdemo/run", map[string]any{"workspace": wsID, "harness": "fake"})
	if code != 201 || !strings.Contains(out["warning"].(string), "paused") {
		t.Fatalf("manual launch: %d %v", code, out)
	}

	for _, c := range []struct {
		body map[string]any
		code int
	}{
		{map[string]any{"scope": "harness", "target": "fake", "until": "reset"}, 400},
		{map[string]any{"scope": "harness", "target": "nope"}, 404},
		{map[string]any{"scope": "agent", "target": "ops"}, 400},
		{map[string]any{"scope": "all", "until": "2001-01-01T00:00:00Z"}, 400},
		{map[string]any{"scope": "all", "until": "soon"}, 400},
		{map[string]any{"scope": "all", "for": "3h", "until": "reset"}, 400},
		{map[string]any{"scope": "bad"}, 400},
	} {
		if code, out, _ := doJSON(t, "POST", base+"/v1/pauses", c.body); code != c.code {
			t.Errorf("%v: %d %v", c.body, code, out)
		}
	}
	code, ap, _ := doJSON(t, "POST", base+"/v1/pauses", map[string]any{"scope": "agent", "target": "ops", "workspace": "pz-ws"})
	if code != 201 || ap["label"] != "ops" {
		t.Fatalf("agent pause: %d %v", code, ap)
	}

	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	reset := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	if _, err := st.DB().Exec(`INSERT INTO harness_usage (harness, window_key, scope, minutes, used_percent, resets_at, plan, source, observed_at)
		VALUES ('fake','five_hour','session',300,80,?,'max','test',?)`, reset, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	code, hp, _ := doJSON(t, "POST", base+"/v1/pauses", map[string]any{"scope": "harness", "target": "fake", "until": "reset"})
	if code != 201 || hp["until"] != reset {
		t.Fatalf("until reset: %d %v want %s", code, hp, reset)
	}
	token := "pause-token-0000000000000000000000"
	insertAgentCaller(t, st, wsID, "ops", token, "running")
	if code, out := doScoped(t, "POST", base+"/v1/pauses/"+id+"/resume", token, nil); code != 403 {
		t.Fatalf("agent resume: %d %v", code, out)
	}

	code, p, _ = doJSON(t, "POST", base+"/v1/pauses/"+id+"/resume", nil)
	if code != 200 || p["active"] != false || p["ended_by"] != "user" {
		t.Fatalf("resume: %d %v", code, p)
	}
	if code, _, _ := doJSON(t, "PATCH", base+"/v1/pauses/"+id, map[string]any{"note": "x"}); code != 409 {
		t.Fatalf("update ended: %d", code)
	}
	if code, _, _ := doJSON(t, "GET", base+"/v1/pauses/pse_missing", nil); code != 404 {
		t.Fatalf("missing: %d", code)
	}
	_, open, _ := doJSON(t, "GET", base+"/v1/pauses", nil)
	_, all, _ := doJSON(t, "GET", base+"/v1/pauses?all=true", nil)
	if len(open["pauses"].([]any)) != 2 || len(all["pauses"].([]any)) != 3 {
		t.Fatalf("list: open=%v all=%v", open, all)
	}
}
