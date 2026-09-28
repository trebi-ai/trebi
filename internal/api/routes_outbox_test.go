package api_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestOutboxRoutes covers list, get, retry, cancel, and the agent scope of
// /v1/outbox (plan 2026-09-28 B.4).
func TestOutboxRoutes(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

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
	wsID := addWorkspace(t, base, t.TempDir(), "outbox-routes")

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, q := range []string{
		`INSERT INTO outbox (id, kind, purpose, workspace_id, instance, room, body, status, attempts, error_message, created_at)
		 VALUES ('01ROWA','channel','reply','` + wsID + `','telegram','chat1','hello','failed',5,'boom','` + now + `')`,
		`INSERT INTO outbox (id, kind, purpose, workspace_id, recipients, subject, body, status, next_attempt_at, created_at)
		 VALUES ('01ROWB','email','alert','` + wsID + `','ops@example.com','[trebi] x','text','pending','2999-01-01T00:00:00.000Z','` + now + `')`,
	} {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	code, body, _ := doJSON(t, "GET", base+"/v1/outbox?kind=channel", nil)
	rows, _ := body["outbox"].([]any)
	if code != 200 || len(rows) != 1 {
		t.Fatalf("list kind=channel %d %v", code, body)
	}
	if row, _ := rows[0].(map[string]any); row["id"] != "01ROWA" || row["error"] != "boom" || row["body"] != "hello" {
		t.Fatalf("row %v", row)
	}
	if code, body, _ = doJSON(t, "GET", base+"/v1/outbox?limit=1", nil); code != 200 || body["next_cursor"] != "01ROWB" {
		t.Fatalf("page %d %v", code, body)
	}
	if code, body, _ = doJSON(t, "GET", base+"/v1/outbox/01ROWB", nil); code != 200 || body["purpose"] != "alert" {
		t.Fatalf("get %d %v", code, body)
	}
	if code, _, _ = doJSON(t, "GET", base+"/v1/outbox/missing", nil); code != 404 {
		t.Fatalf("get missing = %d, want 404", code)
	}
	if code, _, _ = doJSON(t, "GET", base+"/v1/outbox?cursor=bad", nil); code != 400 {
		t.Fatalf("bad cursor = %d, want 400", code)
	}

	// Cancel a pending row; a second cancel conflicts; retry brings it back.
	if code, body, _ = doJSON(t, "POST", base+"/v1/outbox/01ROWB/cancel", nil); code != 200 || body["status"] != "canceled" {
		t.Fatalf("cancel %d %v", code, body)
	}
	if code, _, _ = doJSON(t, "POST", base+"/v1/outbox/01ROWB/cancel", nil); code != 409 {
		t.Fatalf("second cancel = %d, want 409", code)
	}
	if code, body, _ = doJSON(t, "POST", base+"/v1/outbox/01ROWA/retry", nil); code != 200 || body["status"] != "pending" || body["attempts"] != float64(0) {
		t.Fatalf("retry %d %v", code, body)
	}
	if code, _, _ = doJSON(t, "POST", base+"/v1/outbox/missing/retry", nil); code != 404 {
		t.Fatalf("retry missing = %d, want 404", code)
	}

	// An agent sees no row it does not own and cannot act on one.
	token := "outbox-routes-agent-token-00000000"
	insertAgentCaller(t, st, wsID, "cmo", token, "running")
	code, body = doScoped(t, "GET", base+"/v1/outbox", token, nil)
	if rows, _ := body["outbox"].([]any); code != 200 || len(rows) != 0 {
		t.Fatalf("agent list %d %v", code, body)
	}
	if code, _ = doScoped(t, "POST", base+"/v1/outbox/01ROWA/cancel", token, nil); code != 403 {
		t.Fatalf("agent cancel = %d, want 403", code)
	}
}
