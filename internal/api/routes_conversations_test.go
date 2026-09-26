package api_test

import (
	"fmt"
	"time"

	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/assistant"
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
		"text": "hi", "channel_type": "cli", "channel_ref": wsID,
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
				"text": "hi", "channel_type": "cli", "channel_ref": wsID,
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
		"text": "hi", "channel_type": "cli", "channel_ref": wsID,
	})
	if code != 201 {
		t.Fatalf("send while draining %d %v (want 201 not 503)", code, sent)
	}
	if sent["delivery"] != "prompted" {
		t.Fatalf("delivery under drain %v", sent)
	}
}
