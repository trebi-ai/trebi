package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// scopeFixture starts a daemon API with two registered workspaces (alpha,
// beta) and returns the store plus a JSON GET helper.
func scopeFixture(t *testing.T) (*store.Store, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	for _, name := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr

	post := func(path string, body any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if resp.StatusCode != 201 {
			t.Fatalf("POST %s: %d %s", path, resp.StatusCode, raw)
		}
		return m
	}
	wsA := post("/v1/workspaces", map[string]any{"path": filepath.Join(dir, "alpha"), "name": "alpha"})["id"].(string)
	wsB := post("/v1/workspaces", map[string]any{"path": filepath.Join(dir, "beta"), "name": "beta"})["id"].(string)
	return st, base, wsA, wsB
}

// scopeGet GETs path and returns the decoded JSON object.
func scopeGet(t *testing.T, base, path string) map[string]any {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
	}
	return m
}

// scopeArr pulls the JSON array under key, tolerating a null.
func scopeArr(t *testing.T, m map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := m[key].([]any)
	if !ok {
		if m[key] == nil {
			return nil
		}
		t.Fatalf("%s: %v", key, m[key])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(map[string]any))
	}
	return out
}

// scopePOST issues an empty-body POST and returns the decoded JSON object.
func scopePOST(t *testing.T, base, path string) map[string]any {
	t.Helper()
	resp, err := http.Post(base+path, "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if resp.StatusCode != 200 {
		t.Fatalf("POST %s: %d %s", path, resp.StatusCode, raw)
	}
	return m
}

// scopeStr reads a string field that may be absent (omitempty).
func scopeStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func TestQueueWorkspaceScope(t *testing.T) {
	st, base, wsA, wsB := scopeFixture(t)
	q := dispatch.NewQueue(st)
	ctx := t.Context()
	for _, ws := range []string{wsA, wsB, ""} {
		if err := q.Enqueue(ctx, &dispatch.QueueItem{Connector: "webhook", WorkspaceID: ws, JobID: "j1", JobName: "j1"}); err != nil {
			t.Fatal(err)
		}
	}

	scoped := scopeArr(t, scopeGet(t, base, "/v1/queue?workspace=alpha"), "items")
	if len(scoped) != 2 {
		t.Fatalf("workspace=alpha items %d, want 2 (alpha + global)", len(scoped))
	}
	for _, it := range scoped {
		if ws := it["workspace_id"].(string); ws != wsA && ws != "" {
			t.Fatalf("beta row leaked into alpha scope: %v", it["workspace_id"])
		}
	}
	all := scopeArr(t, scopeGet(t, base, "/v1/queue"), "items")
	if len(all) != 3 {
		t.Fatalf("unscoped items %d, want 3", len(all))
	}
}

func TestEventsWorkspaceAndOutcomeScope(t *testing.T) {
	st, base, wsA, wsB := scopeFixture(t)
	db := st.DB()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, ev := range []struct{ id string }{{"ev_a"}, {"ev_b"}, {"ev_g"}} {
		if _, err := db.Exec(`INSERT INTO events (id, connector, received_at, trace_id) VALUES (?,?,?,?)`,
			ev.id, "webhook", now, "t"); err != nil {
			t.Fatal(err)
		}
	}
	for _, oc := range []struct{ ev, ws, job, outcome string }{
		{"ev_a", wsA, "j1", "queued"},
		{"ev_a", wsA, "j2", "error"},
		{"ev_b", wsB, "j1", "queued"},
		// ev_g has no outcome row: global.
	} {
		if _, err := db.Exec(`INSERT INTO event_outcomes (event_id, workspace_id, job_id, on_index, outcome, created_at)
			VALUES (?,?,?,?,?,?)`, oc.ev, oc.ws, oc.job, 0, oc.outcome, now); err != nil {
			t.Fatal(err)
		}
	}

	rows := func(path string) []map[string]any {
		return scopeArr(t, scopeGet(t, base, path), "events")
	}
	if got := rows("/v1/events"); len(got) != 4 {
		t.Fatalf("unscoped rows %d, want 4 (ev_a x2, ev_b, ev_g)", len(got))
	}
	if got := rows("/v1/events?workspace=alpha"); len(got) != 2 {
		t.Fatalf("workspace=alpha rows %d, want 2 (global ev_g has no outcome row)", len(got))
	} else {
		for _, r := range got {
			if r["id"] != "ev_a" {
				t.Fatalf("wrong event in alpha scope: %v", r["id"])
			}
		}
	}
	if got := rows("/v1/events?workspace=beta"); len(got) != 1 || got[0]["id"] != "ev_b" {
		t.Fatalf("workspace=beta rows %v", got)
	}
	if got := rows("/v1/events?outcome=queued"); len(got) != 2 {
		t.Fatalf("outcome=queued rows %d, want 2", len(got))
	}
	if got := rows("/v1/events?workspace=alpha&outcome=error"); len(got) != 1 || got[0]["id"] != "ev_a" {
		t.Fatalf("workspace+outcome rows %v", got)
	}
	if got := rows("/v1/events?workspace=beta&outcome=error"); len(got) != 0 {
		t.Fatalf("beta/error rows %v, want none", got)
	}
}

func TestInboxWorkspaceScope(t *testing.T) {
	st, base, wsA, wsB := scopeFixture(t)
	svc := alerts.New(st, alerts.Options{})
	ctx := t.Context()
	for _, o := range []alerts.RaiseOpts{
		{Kind: alerts.KindUsageWarning, Subject: "inbox/alpha", Title: "alpha", WorkspaceID: wsA},
		{Kind: alerts.KindUsageWarning, Subject: "inbox/beta", Title: "beta", WorkspaceID: wsB},
		{Kind: alerts.KindUsageWarning, Subject: "inbox/global", Title: "global"},
	} {
		if _, err := svc.Raise(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	scoped := scopeArr(t, scopeGet(t, base, "/v1/inbox?workspace=alpha"), "items")
	if len(scoped) != 2 {
		t.Fatalf("workspace=alpha items %d, want 2 (alpha + global)", len(scoped))
	}
	for _, it := range scoped {
		if ws := scopeStr(it, "workspace"); ws != wsA && ws != "" {
			t.Fatalf("beta row leaked into alpha scope: %v", it["workspace"])
		}
	}
	all := scopeArr(t, scopeGet(t, base, "/v1/inbox"), "items")
	if len(all) != 3 {
		t.Fatalf("unscoped items %d, want 3", len(all))
	}

	// read-all scoped to beta covers the beta item plus the global row only.
	if n := scopePOST(t, base, "/v1/inbox/read-all?workspace=beta")["read"].(float64); n != 2 {
		t.Fatalf("scoped read-all %v, want 2", n)
	}
	// The alpha item stays unread; the global row is now read for everyone.
	if unread := scopeArr(t, scopeGet(t, base, "/v1/inbox?workspace=alpha&status=unread"), "items"); len(unread) != 1 || unread[0]["title"] != "alpha" {
		t.Fatalf("alpha unread after beta read-all %v, want only the alpha item", unread)
	}

	// resolve-all scoped to alpha resolves alpha plus global; beta stays
	// unread-untouched (still read from the earlier read-all, never resolved).
	if n := scopePOST(t, base, "/v1/inbox/resolve-all?workspace=alpha")["resolved"].(float64); n != 2 {
		t.Fatalf("scoped resolve-all %v, want 2", n)
	}
	if read := scopeArr(t, scopeGet(t, base, "/v1/inbox?workspace=beta&status=read"), "items"); len(read) != 1 || read[0]["title"] != "beta" {
		t.Fatalf("beta items after alpha resolve-all %v, want the untouched read beta item", read)
	}
}

func TestAlertsLogWorkspaceScope(t *testing.T) {
	st, base, wsA, wsB := scopeFixture(t)
	svc := alerts.New(st, alerts.Options{})
	ctx := t.Context()
	for _, o := range []alerts.RaiseOpts{
		{Kind: alerts.KindUsageWarning, Subject: "warn/alpha", Title: "alpha", WorkspaceID: wsA},
		{Kind: alerts.KindUsageWarning, Subject: "warn/beta", Title: "beta", WorkspaceID: wsB},
		{Kind: alerts.KindUsageWarning, Subject: "warn/global", Title: "global"},
	} {
		if _, err := svc.Raise(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	scoped := scopeArr(t, scopeGet(t, base, "/v1/alerts?workspace=alpha"), "alerts")
	if len(scoped) != 2 {
		t.Fatalf("workspace=alpha alerts %d, want 2 (alpha + global)", len(scoped))
	}
	all := scopeArr(t, scopeGet(t, base, "/v1/alerts"), "alerts")
	if len(all) != 3 {
		t.Fatalf("unscoped alerts %d, want 3", len(all))
	}
}

func TestApprovalsWorkspaceScope(t *testing.T) {
	st, base, wsA, wsB := scopeFixture(t)
	db := st.DB()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, run := range []struct{ id, ws string }{
		{"run_a", wsA}, {"run_b", wsB}, {"run_g", ""}, // run_g: global
	} {
		if _, err := db.Exec(`INSERT INTO runs (id, workspace_id, job_id, status, started_at, trace_id, updated_at)
			VALUES (?,?,?,?,?,?,?)`, run.id, run.ws, "j1", "waiting", now, "t", now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO approvals (id, run_id, question, status, revision, created_at)
			VALUES (?,?,?,?,1,?)`, "ap_"+run.id, run.id, "may I?", "pending", now); err != nil {
			t.Fatal(err)
		}
	}

	scoped := scopeArr(t, scopeGet(t, base, "/v1/approvals?workspace=alpha"), "approvals")
	if len(scoped) != 2 {
		t.Fatalf("workspace=alpha approvals %d, want 2 (alpha + global)", len(scoped))
	}
	for _, a := range scoped {
		if id := a["run_id"].(string); id != "run_a" && id != "run_g" {
			t.Fatalf("beta approval leaked into alpha scope: %v", id)
		}
	}
	all := scopeArr(t, scopeGet(t, base, "/v1/approvals"), "approvals")
	if len(all) != 3 {
		t.Fatalf("unscoped approvals %d, want 3", len(all))
	}
}
