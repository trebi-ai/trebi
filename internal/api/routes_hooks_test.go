package api_test

import (
	"net/http"
	"testing"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestHooksLifecycle walks the hosted webhook surface: create returns the
// URL once and in full, the list masks it, reveal brings it back with an
// audit entry, and delete is confirm-gated and empties the row (plan
// 2026-09-22 Part E and G).
func TestHooksLifecycle(t *testing.T) {
	s, base, _ := startAPI(t)
	f := newFakeCloud(t)
	s.Cloud().Configure(cloudlink.Settings{APIURL: f.srv.URL, Enabled: false})
	if _, err := s.Cloud().Link(t.Context(), "abc", "mbp"); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/hooks",
		map[string]string{"workspace_id": "ws_1", "label": "deploy"})
	if code != http.StatusCreated {
		t.Fatalf("create %d %v", code, body)
	}
	if body["url"] != "https://in.example.com/hook/tok-1" || body["masked"] != false {
		t.Fatalf("create returns the URL once, in full: %v", body)
	}
	if body["label"] != "deploy" || body["workspace_id"] != "ws_1" || body["id"] == "" {
		t.Fatalf("row shape: %v", body)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/hooks", map[string]string{"label": "x"})
	if code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("missing workspace: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/hooks", nil)
	if code != http.StatusOK {
		t.Fatalf("list %d %v", code, body)
	}
	rows, _ := body["hooks"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list: %v", body)
	}
	row, _ := rows[0].(map[string]any)
	if row["masked"] != true || row["url"] == "https://in.example.com/hook/tok-1" {
		t.Fatalf("masked URL hides the token: %v", row)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/hooks?reveal=1", nil)
	if code != http.StatusOK {
		t.Fatalf("reveal %d %v", code, body)
	}
	rows, _ = body["hooks"].([]any)
	row, _ = rows[0].(map[string]any)
	if row["masked"] != false || row["url"] != "https://in.example.com/hook/tok-1" {
		t.Fatalf("revealed URL: %v", row)
	}

	id, _ := row["id"].(string)
	code, body, _ = doJSON(t, "DELETE", base+"/v1/hooks/"+id, nil)
	if code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("unconfirmed delete: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "DELETE", base+"/v1/hooks/"+id+"?confirm=1", nil)
	if code != http.StatusOK || body["deleted"] != true {
		t.Fatalf("delete %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/hooks", nil)
	rows, _ = body["hooks"].([]any)
	if code != http.StatusOK || len(rows) != 0 {
		t.Fatalf("row survived delete: %d %v", code, body)
	}

	st := mustOpenStore(t, home.DBPath())
	defer func() { _ = st.Close() }()
	if n := busCount(t, st, "hook.created"); n != 1 {
		t.Errorf("hook.created frames = %d, want 1", n)
	}
	if n := busCount(t, st, "hook.deleted"); n != 1 {
		t.Errorf("hook.deleted frames = %d, want 1", n)
	}
}

// busCount counts one topic in the event log.
func busCount(t *testing.T, st *store.Store, topic string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM event_log WHERE topic=?`, topic).Scan(&n); err != nil {
		t.Fatalf("bus count %s: %v", topic, err)
	}
	return n
}
