package api_test

import (
	"fmt"
	"time"

	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// writeAssistant pins the fake harness in the workspace trebi.yaml, so a
// prompt resolves without machine probes.
func writeAssistant(t *testing.T, wsDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wsDir, "trebi.yaml"), []byte("assistant:\n  harness: fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestConversationSendPromptsSession covers the F.2 send path: the reply
// delivery is "prompted", the session row fills in, and one assistant row
// lands in the message log.
func TestConversationSendPromptsSession(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "prompt-ws")
	writeAssistant(t, wsDir)
	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "origin_channel": "cli",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, conv)
	}
	id, _ := conv["id"].(string)

	code, sent, _ := doJSON(t, "POST", base+"/v1/conversations/"+id+"/messages", map[string]any{
		"text": "hi", "connector": "cli", "room": wsID,
	})
	if code != 201 {
		t.Fatalf("send %d %v", code, sent)
	}
	if sent["delivery"] != "prompted" {
		t.Fatalf("delivery %v", sent)
	}
	if sent["status"] != assistant.StatusRunning && sent["status"] != assistant.StatusIdle {
		t.Fatalf("status %v", sent["status"])
	}

	waitFor(t, 5*time.Second, "session idle", func() bool {
		code, c, _ := doJSON(t, "GET", base+"/v1/conversations/"+id, nil)
		if code != 200 {
			return false
		}
		sess, _ := c["session"].(map[string]any)
		return sess != nil && sess["status"] == assistant.StatusClosed
	})

	code, body, _ := doJSON(t, "GET", base+"/v1/conversations/"+id+"/messages?limit=10", nil)
	if code != 200 {
		t.Fatalf("messages %d %v", code, body)
	}
	msgs, _ := body["messages"].([]any)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, fmt.Sprint(m.(map[string]any)["role"]))
	}
	if len(roles) < 2 {
		t.Fatalf("want user+assistant rows, got %v", roles)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/conversations/"+id+"/messages?cursor=@@bad@@", nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("bad cursor %d %v", code, body)
	}
	_ = s
}

// TestConversationCreateRejectsAgentPins: harness and mode apply only to
// assistant conversations (plan F.2).
func TestConversationCreateRejectsAgentPins(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "pin-ws")
	code, body, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "operator": "agents/cmo", "harness": "fake",
	})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("agent pin %d %v", code, body)
	}
	_ = s
}

// TestConversationParallelSendsStoreRows: parallel sends each store one user
// row and none queues a wake (sessions replace the wake lane, plan E).
func TestConversationParallelSendsStoreRows(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "par-ws")
	writeAssistant(t, wsDir)
	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "origin_channel": "cli",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, conv)
	}
	id, _ := conv["id"].(string)
	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			doJSON(t, "POST", base+"/v1/conversations/"+id+"/messages", map[string]any{
				"text": "hi", "connector": "cli", "room": wsID,
				"idempotency_key": ulid.Make().String(),
			})
		}()
	}
	wg.Wait()
	code, body, _ := doJSON(t, "GET", base+"/v1/conversations/"+id+"/messages?limit=50", nil)
	if code != 200 {
		t.Fatalf("messages %d", code)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) < n {
		t.Fatalf("messages %d want >= %d", len(msgs), n)
	}
	code, qbody, _ := doJSON(t, "GET", base+"/v1/queue?limit=50", nil)
	if code != 200 {
		t.Fatalf("queue %d %v", code, qbody)
	}
	if items, _ := qbody["items"].([]any); len(items) != 0 {
		t.Fatalf("session sends must not queue rows: %v", qbody)
	}
}

// TestConversationSendWhileDraining: drain holds queue items, not session
// prompts (plan E).
func TestConversationSendWhileDraining(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	defer st.Close()
	if err := st.SetFlag("drain", "1"); err != nil {
		t.Fatal(err)
	}
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "drain-ws")
	writeAssistant(t, wsDir)
	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "origin_channel": "cli",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, conv)
	}
	id, _ := conv["id"].(string)
	code, sent, _ := doJSON(t, "POST", base+"/v1/conversations/"+id+"/messages", map[string]any{
		"text": "hi", "connector": "cli", "room": wsID,
	})
	if code != 201 {
		t.Fatalf("send while draining %d %v (want 201 not 503)", code, sent)
	}
	if sent["delivery"] != "prompted" {
		t.Fatalf("delivery under drain %v", sent)
	}
}

// TestConversationOutboxRoutes covers the support read and the retry of one
// failed channel reply (plan 2026-09-27 G.4).
func TestConversationOutboxRoutes(t *testing.T) {
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

	wsID := addWorkspace(t, base, t.TempDir(), "outbox-ws")
	code, body, _ := doJSON(t, "POST", base+"/v1/conversations",
		map[string]any{"workspace": wsID, "origin_channel": "cli"})
	if code != 201 {
		t.Fatalf("create conversation %d %v", code, body)
	}
	convID, _ := body["id"].(string)
	if convID == "" {
		t.Fatalf("conversation id missing: %v", body)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`INSERT INTO outbox
		(id, kind, purpose, instance, workspace_id, conversation_id, room, dedupe_key, body, status, attempts, error_message, created_at)
		VALUES ('01ROW1','channel','reply','telegram',?,?,'chat1','01MSG1','hello','failed',5,'boom',?)`,
		wsID, convID, now); err != nil {
		t.Fatal(err)
	}

	// An agent that does not operate the conversation cannot read or retry.
	agentToken := "outbox-agent-token-0000000000000000"
	insertAgentCaller(t, st, wsID, "cmo", agentToken, "running")
	if code, body := doScoped(t, "GET", base+"/v1/conversations/"+convID+"/outbox", agentToken, nil); code != 403 {
		t.Fatalf("agent outbox read = %d %v, want 403", code, body)
	}
	if code, body := doScoped(t, "POST", base+"/v1/conversations/"+convID+"/outbox/01ROW1/retry", agentToken, nil); code != 403 {
		t.Fatalf("agent outbox retry = %d %v, want 403", code, body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/conversations/"+convID+"/outbox", nil)
	if code != 200 {
		t.Fatalf("outbox read %d %v", code, body)
	}
	rows, _ := body["outbox"].([]any)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %v", rows)
	}
	row, _ := rows[0].(map[string]any)
	if row["status"] != "failed" || row["instance"] != "telegram" || row["dedupe_key"] != "01MSG1" {
		t.Fatalf("outbox row %v", row)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/conversations/"+convID+"/outbox/01ROW1/retry", nil)
	if code != 200 || body["status"] != "pending" || body["attempts"] != float64(0) {
		t.Fatalf("retry %d %v", code, body)
	}
	// A pending row cannot retry again: conflict.
	if code, _, _ = doJSON(t, "POST", base+"/v1/conversations/"+convID+"/outbox/01ROW1/retry", nil); code != 409 {
		t.Fatalf("retry of a pending row = %d, want 409", code)
	}
	// A missing row is not found.
	if code, _, _ = doJSON(t, "POST", base+"/v1/conversations/"+convID+"/outbox/missing/retry", nil); code != 404 {
		t.Fatalf("retry of a missing row = %d, want 404", code)
	}
	// A missing conversation is not found.
	if code, _, _ = doJSON(t, "GET", base+"/v1/conversations/missing/outbox", nil); code != 404 {
		t.Fatalf("outbox of a missing conversation = %d, want 404", code)
	}
}
