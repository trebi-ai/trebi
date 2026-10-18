package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateRoutes(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	files := map[string]string{
		filepath.Join(wsDir, "agents", "beta", "AGENT.md"): "---\ndescription: Test agent.\n---\n\nBeta.\n",
		filepath.Join(wsDir, "digest", "JOB.md"): "---\nharness: fake\nstate:\n  topics:\n    schema:\n      type: object\n" +
			"      properties:\n        topic: {enum: [alz, bone]}\n---\n\nDigest.\n",
	}
	for path, md := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wsID := addWorkspace(t, base, wsDir, "stated")
	token := "state-token-00000000000000000000000"
	insertAgentCaller(t, st, wsID, "cmo", token, "running")
	beta := indexedJobID(t, st, wsID, "agents/beta")
	row := map[string]any{"rows": []map[string]any{{"key": "doi:10/1", "value": map[string]any{"n": 1}}}}

	// A run writes its own state through self, with no confirm.
	code, body := doScoped(t, "POST", base+"/v1/state/self/seen/rows", token, row)
	if code != http.StatusOK || body["inserted"] != float64(1) {
		t.Fatalf("self put: %d %v", code, body)
	}
	code, body = doScoped(t, "POST", base+"/v1/state/self/seen/delete", token, map[string]any{"keys": []string{"doi:10/1"}})
	if code != http.StatusOK || body["deleted"] != float64(1) {
		t.Fatalf("self delete in run: %d %v", code, body)
	}

	// A data field at the top of a row names the fix.
	flat := map[string]any{"rows": []map[string]any{{"key": "run", "last_run_at": "2026-10-12"}}}
	code, body = doScoped(t, "POST", base+"/v1/state/self/seen/rows", token, flat)
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if code != http.StatusBadRequest || !strings.Contains(msg, `"last_run_at" is not a row field`) || !strings.Contains(msg, "Put the data in value") {
		t.Fatalf("flat row: %d %v", code, body)
	}

	// self needs a run.
	code, body = doScoped(t, "GET", base+"/v1/state/self", "", nil)
	if code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("self with no run: %d %v", code, body)
	}

	// A run cannot touch the state of another owner.
	code, body = doScoped(t, "POST", base+"/v1/state/"+beta+"/seen/query", token, map[string]any{})
	if code != http.StatusForbidden || errCode(body) != "state_scope" {
		t.Fatalf("other owner: %d %v", code, body)
	}

	// A bad batch writes nothing and names each row.
	bad := map[string]any{"rows": []map[string]any{
		{"key": "a", "value": map[string]any{"topic": "alz"}},
		{"key": "b", "value": map[string]any{"topic": "hairloss"}},
	}}
	code, body = doScoped(t, "POST", base+"/v1/state/digest/topics/rows?workspace="+wsID, "", bad)
	if code != http.StatusUnprocessableEntity || errCode(body) != "state_invalid" {
		t.Fatalf("bad batch: %d %v", code, body)
	}
	details, _ := body["error"].(map[string]any)["details"].(map[string]any)
	rows, _ := details["errors"].([]any)
	if len(rows) != 1 {
		t.Fatalf("errors %v", rows)
	}
	first, _ := rows[0].(map[string]any)
	if first["line"] != float64(2) || first["path"] != "value.topic" || first["msg"] == "" {
		t.Fatalf("errors %v", rows)
	}
	code, body = doScoped(t, "POST", base+"/v1/state/digest/topics/rows?workspace="+wsID, "", map[string]any{"rows": bad["rows"].([]map[string]any)[:1]})
	if code != http.StatusOK {
		t.Fatalf("good batch: %d %v", code, body)
	}

	// Outside a run, a delete needs confirm.
	code, body = doScoped(t, "POST", base+"/v1/state/digest/topics/delete?workspace="+wsID, "", map[string]any{"keys": []string{"a"}})
	if code != http.StatusBadRequest {
		t.Fatalf("delete with no confirm: %d %v", code, body)
	}
	if d, _ := body["error"].(map[string]any)["details"].(map[string]any); d["confirm_required"] == nil {
		t.Fatalf("no confirm_required: %v", body)
	}
	code, body = doScoped(t, "POST", base+"/v1/state/digest/topics/delete?confirm=1&workspace="+wsID, "", map[string]any{"keys": []string{"a"}})
	if code != http.StatusOK || body["deleted"] != float64(1) {
		t.Fatalf("delete with confirm: %d %v", code, body)
	}

	// A bare agent name resolves to agents/<name>.
	code, body = doScoped(t, "GET", base+"/v1/state/cmo?workspace="+wsID, "", nil)
	if code != http.StatusOK || body["owner_name"] != "cmo" || body["kind"] != "agent" {
		t.Fatalf("summary: %d %v", code, body)
	}

	frames := collectBus(t, base, "state.changed", time.Second)
	if n := countTopic(frames, "state.changed"); n != 4 {
		t.Fatalf("state.changed frames = %d, want 4", n)
	}
}
