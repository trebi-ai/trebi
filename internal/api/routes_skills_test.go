package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestSkillScopes covers D3 to D7: who writes which scope, the run default
// scope, the overwrite order, ambiguous names, move, and review.
func TestSkillScopes(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	wsDir := t.TempDir()
	writeFiles(t, map[string]string{
		filepath.Join(wsDir, "agents", "cmo", "AGENT.md"):       "---\ndescription: Test agent.\nharness: fake\n---\n\nRun growth.\n",
		filepath.Join(wsDir, "agents", "cmo", "duty", "JOB.md"): "---\nharness: fake\n---\n\nDo the duty.\n",
		filepath.Join(wsDir, "agents", "beta", "AGENT.md"):      "---\ndescription: Review agent.\nharness: fake\nkit:\n  skill_policy:\n    review: true\n---\n\nBeta.\n",
		filepath.Join(wsDir, "agents", "gamma", "AGENT.md"):     "---\ndescription: Locked agent.\nharness: fake\nkit:\n  skill_policy:\n    create: false\n---\n\nGamma.\n",
		filepath.Join(wsDir, "alpha", "JOB.md"):                 "---\nharness: fake\n---\n\nDo alpha.\n",
	})
	wsID := addWorkspace(t, base, wsDir, "skills")

	cmo := "cmo-token-00000000000000000000000000"
	duty := "duty-token-0000000000000000000000000"
	alpha := "alpha-token-000000000000000000000000"
	beta := "beta-token-00000000000000000000000000"
	gamma := "gamma-token-0000000000000000000000000"
	insertRunCaller(t, st, wsID, "agents/cmo", "agent", cmo)
	insertRunCaller(t, st, wsID, "agents/cmo/duty", "job", duty)
	insertRunCaller(t, st, wsID, "alpha", "job", alpha)
	insertRunCaller(t, st, wsID, "agents/beta", "agent", beta)
	insertRunCaller(t, st, wsID, "agents/gamma", "agent", gamma)
	cmoID := indexedJobID(t, st, wsID, "agents/cmo")

	md := func(s string) string { return "---\nname: x\ndescription: " + s + "\n---\n\n" + s + "\n" }
	create := func(token string, body map[string]any) (int, map[string]any) {
		t.Helper()
		return doScoped(t, "POST", base+"/v1/skills", token, body)
	}

	cases := []struct {
		name  string
		token string
		body  map[string]any
		want  int
	}{
		{"person global", "", map[string]any{"name": "shared", "skill_md": md("g")}, http.StatusCreated},
		{"person workspace", "", map[string]any{"name": "tone", "skill_md": md("w"), "scope": "workspace", "owner": wsID}, http.StatusCreated},
		{"agent default scope", cmo, map[string]any{"name": "shared", "skill_md": md("a")}, http.StatusCreated},
		{"agent owned job", cmo, map[string]any{"name": "dutyskill", "skill_md": md("j"), "scope": "job", "owner": "agents/cmo/duty"}, http.StatusCreated},
		{"agent other agent", cmo, map[string]any{"name": "x1", "skill_md": md("x"), "scope": "agent", "owner": "beta"}, http.StatusForbidden},
		{"agent global", cmo, map[string]any{"name": "x2", "skill_md": md("x"), "scope": "global"}, http.StatusForbidden},
		{"agent workspace", cmo, map[string]any{"name": "x3", "skill_md": md("x"), "scope": "workspace", "owner": wsID}, http.StatusForbidden},
		{"job default scope", duty, map[string]any{"name": "mine", "skill_md": md("d")}, http.StatusCreated},
		{"job owner agent", duty, map[string]any{"name": "x4", "skill_md": md("x"), "scope": "agent", "owner": "agents/cmo"}, http.StatusForbidden},
		{"job other job", alpha, map[string]any{"name": "x5", "skill_md": md("x"), "scope": "job", "owner": "agents/cmo/duty"}, http.StatusForbidden},
		{"locked kit", gamma, map[string]any{"name": "x6", "skill_md": md("x")}, http.StatusForbidden},
		{"long note", cmo, map[string]any{"name": "x7", "skill_md": md("x"), "note": string(make([]byte, 501))}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		code, body := create(tc.token, tc.body)
		if code != tc.want {
			t.Fatalf("%s: %d %v", tc.name, code, body)
		}
		if tc.name == "agent default scope" {
			owner, _ := body["owner"].(map[string]any)
			by, _ := body["created_by"].(map[string]any)
			if body["scope"] != "agent" || owner["id"] != cmoID || body["provenance"] != "agent" || by["actor_kind"] != "agent" {
				t.Fatalf("run default: %v", body)
			}
		}
	}

	// A read of a name in two scopes gets the global skill; a write is ambiguous.
	code, body := doScoped(t, "GET", base+"/v1/skills/shared", "", nil)
	if code != http.StatusOK || body["scope"] != "global" {
		t.Fatalf("ambiguous read: %d %v", code, body)
	}
	code, body = doScoped(t, "PUT", base+"/v1/skills/shared", "", map[string]any{"skill_md": md("z")})
	if code != http.StatusConflict || errCode(body) != "ambiguous" {
		t.Fatalf("ambiguous write: %d %v", code, body)
	}
	// The agent cannot change the global skill, but it can change its own.
	code, body = doScoped(t, "PUT", base+"/v1/skills/shared?scope=global", cmo, map[string]any{"skill_md": md("z")})
	if code != http.StatusForbidden {
		t.Fatalf("agent global write: %d %v", code, body)
	}
	code, body = doScoped(t, "PUT", base+"/v1/skills/shared/files/run.sh?scope=agent&owner="+cmoID, cmo, map[string]any{"content": "echo\n"})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("agent file write: %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo", "skills", "shared", "run.sh")); err != nil {
		t.Fatalf("agent skill folder: %v", err)
	}

	// The agent skill overwrites the global one for runs of the agent and its jobs.
	code, body = doScoped(t, "GET", base+"/v1/skills?effective_for=cmo&workspace="+wsID, "", nil)
	if code != http.StatusOK {
		t.Fatalf("effective: %d %v", code, body)
	}
	byRef := map[string]map[string]any{}
	for _, raw := range body["skills"].([]any) {
		row := raw.(map[string]any)
		byRef[row["ref"].(string)] = row
	}
	agentRef := "agent/" + cmoID + "/shared"
	if byRef[agentRef]["used"] != true || byRef["global/shared"]["overwritten_by"] != agentRef || byRef["workspace/"+wsID+"/tone"]["used"] != true {
		t.Fatalf("effective rows: %v", byRef)
	}

	// Move: a name at the target conflicts unless replace is set.
	code, body = doScoped(t, "POST", base+"/v1/skills/shared/move?scope=agent&owner="+cmoID, "", map[string]any{"to_scope": "global"})
	if code != http.StatusConflict {
		t.Fatalf("move conflict: %d %v", code, body)
	}
	code, body = doScoped(t, "POST", base+"/v1/skills/shared/move?scope=agent&owner="+cmoID, cmo, map[string]any{"to_scope": "global", "replace": true})
	if code != http.StatusForbidden {
		t.Fatalf("agent move wider: %d %v", code, body)
	}
	code, body = doScoped(t, "POST", base+"/v1/skills/shared/move?scope=agent&owner="+cmoID, "", map[string]any{"to_scope": "global", "replace": true})
	if code != http.StatusOK || body["ref"] != "global/shared" {
		t.Fatalf("move replace: %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", "shared", "run.sh")); err != nil {
		t.Fatalf("moved files: %v", err)
	}

	// Review: the kit keeps a new skill pending until a person approves it.
	code, body = create(beta, map[string]any{"name": "draft", "skill_md": md("p")})
	if code != http.StatusCreated || body["status"] != "pending" {
		t.Fatalf("review create: %d %v", code, body)
	}
	code, body = doScoped(t, "POST", base+"/v1/skills/draft/approve", beta, nil)
	if code != http.StatusForbidden {
		t.Fatalf("run approve: %d %v", code, body)
	}
	code, body = doScoped(t, "POST", base+"/v1/skills/draft/approve", "", nil)
	if code != http.StatusOK || body["status"] != "active" {
		t.Fatalf("approve: %d %v", code, body)
	}
}

func insertRunCaller(t *testing.T, st *store.Store, wsID, job, kind, token string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`INSERT INTO runs
		(id,workspace_id,job_id,kind,status,trace_id,started_at,updated_at,actor_token_hash)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		"run-"+token[:8], wsID, indexedJobID(t, st, wsID, job), kind, "running", "trace", now, now,
		runs.HashActorToken(token)); err != nil {
		t.Fatal(err)
	}
}

func writeFiles(t *testing.T, files map[string]string) {
	t.Helper()
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
