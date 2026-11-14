package api_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestOnboardingRoutes(t *testing.T) {
	_, base := startTestServer(t)

	code, st, _ := doJSON(t, "GET", base+"/v1/onboarding", nil)
	if code != 200 || st["step"] != "you" || st["revision"] != float64(0) {
		t.Fatalf("get: %d %v", code, st)
	}
	if hidden, ok := st["hidden"].([]any); !ok || len(hidden) != 0 {
		t.Fatalf("hidden: %v", st["hidden"])
	}

	code, st, _ = doJSON(t, "PATCH", base+"/v1/onboarding", map[string]any{"step": "ai", "hide": []string{"pro"}, "revision": 0})
	if code != 200 || st["step"] != "ai" || st["revision"] != float64(1) {
		t.Fatalf("patch: %d %v", code, st)
	}
	code, st, _ = doJSON(t, "PATCH", base+"/v1/onboarding", map[string]any{"step": "done", "revision": 0})
	if code != 409 || errCode(st) != "conflict" {
		t.Fatalf("stale: %d %v", code, st)
	}
	for _, body := range []map[string]any{{"step": "nope"}, {"hide": []string{"nope"}}} {
		if code, st, _ = doJSON(t, "PATCH", base+"/v1/onboarding", body); code != 400 || errCode(st) != "validation" {
			t.Fatalf("invalid %v: %d %v", body, code, st)
		}
	}
	code, st, _ = doJSON(t, "PATCH", base+"/v1/onboarding", map[string]any{"completed": true})
	if code != 200 || st["step"] != "done" || st["completed_at"] == nil {
		t.Fatalf("complete: %d %v", code, st)
	}
	code, st, _ = doJSON(t, "POST", base+"/v1/onboarding/restart", nil)
	if code != 200 || st["step"] != "you" || st["completed_at"] != nil {
		t.Fatalf("restart: %d %v", code, st)
	}
	if hidden, _ := st["hidden"].([]any); len(hidden) != 1 {
		t.Fatalf("restart lost the hidden card: %v", st)
	}
}

func TestOnboardingDefaults(t *testing.T) {
	root := t.TempDir()
	_, base := startTestServerSeeded(t, api.Options{WorkspacesRoot: root}, seedHarnessLogins())

	code, d, _ := doJSON(t, "POST", base+"/v1/onboarding/defaults", nil)
	ws, _ := d["workspace"].(map[string]any)
	if code != 200 || ws["name"] != "Personal" || d["harness"] != "fake" || d["choose"] != false {
		t.Fatalf("defaults: %d %v", code, d)
	}
	path, _ := ws["path"].(string)
	if filepath.Dir(path) != root || !strings.HasSuffix(path, "Personal") {
		t.Fatalf("path %q is not under %q", path, root)
	}
	code, again, _ := doJSON(t, "POST", base+"/v1/onboarding/defaults", nil)
	if w, _ := again["workspace"].(map[string]any); code != 200 || w["id"] != ws["id"] {
		t.Fatalf("second call: %d %v", code, again)
	}
	code, list, _ := doJSON(t, "GET", base+"/v1/workspaces", nil)
	if rows, _ := list["workspaces"].([]any); code != 200 || len(rows) != 1 {
		t.Fatalf("workspaces: %d %v", code, list)
	}
	if _, st, _ := doJSON(t, "GET", base+"/v1/onboarding", nil); st["workspace_id"] != ws["id"] {
		t.Fatalf("state workspace: %v", st)
	}
}

func TestOnboardingDefaultsChoose(t *testing.T) {
	_, base := startTestServerSeeded(t, api.Options{WorkspacesRoot: t.TempDir()}, seedHarnessLogins("claude"))
	code, d, _ := doJSON(t, "POST", base+"/v1/onboarding/defaults", nil)
	if code != http.StatusOK || d["choose"] != true || d["harness"] != nil {
		t.Fatalf("two logged in: %d %v", code, d)
	}
}

// seedHarnessLogins stores a probe for each visible harness. A name in on is
// logged in.
func seedHarnessLogins(on ...string) func(*store.Store) {
	return func(st *store.Store) {
		for _, n := range []string{"claude", "codex", "grok", "opencode", "copilot", "cursor", "pi", "omp"} {
			ok := 0
			for _, o := range on {
				if o == n {
					ok = 1
				}
			}
			_, _ = st.DB().Exec(`INSERT INTO harness_state (name, auth_ok, updated_at) VALUES (?, ?, 't')`, n, ok)
		}
	}
}
