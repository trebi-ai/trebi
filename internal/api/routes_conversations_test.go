package api_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/runs"
)

func TestConversationSendQueuedAndInjected(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	writeAssistant(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "chat-ws")

	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "origin_channel": "cli",
		"activate": map[string]any{"channel_type": "cli", "channel_ref": wsID},
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
	if sent["delivery"] != "queued" {
		t.Fatalf("delivery %v", sent)
	}
	if sent["queue_id"] == nil || sent["queue_id"] == "" {
		t.Fatalf("missing queue_id %v", sent)
	}

	key, _ := conv["key"].(string)
	live, err := s.Runs().Create(t.Context(), runs.CreateOpts{
		WorkspaceID: wsID, Job: "TREBI", Kind: runs.KindAssistant,
		ConversationKey: key, ConversationID: id, Harness: "fake",
	})
	if err != nil {
		t.Fatal(err)
	}
	code, inj, _ := doJSON(t, "POST", base+"/v1/conversations/"+id+"/messages", map[string]any{
		"text": "again", "channel_type": "cli", "channel_ref": wsID,
	})
	if code != 201 || inj["delivery"] != "injected" {
		t.Fatalf("injected %d %v", code, inj)
	}
	if inj["run_id"] != live.ID {
		t.Fatalf("run_id %v want %s", inj["run_id"], live.ID)
	}

	code, body, _ := doJSON(t, "GET", base+"/v1/conversations/"+id+"/messages?limit=10", nil)
	if code != 200 {
		t.Fatalf("messages %d %v", code, body)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("want >=2 messages %v", body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/conversations/"+id+"/messages?cursor=@@bad@@", nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("bad cursor %d %v", code, body)
	}
}

func TestConversationInjectFallbackQueued(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	writeAssistant(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "race-ws")
	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "origin_channel": "cli",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, conv)
	}
	id, _ := conv["id"].(string)
	key, _ := conv["key"].(string)
	_, err := s.Runs().Create(t.Context(), runs.CreateOpts{
		WorkspaceID: wsID, Job: "TREBI", Kind: runs.KindAssistant,
		ConversationKey: key, ConversationID: id, Harness: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	code, sent, _ := doJSON(t, "POST", base+"/v1/conversations/"+id+"/messages", map[string]any{
		"text": "hi", "channel_type": "cli", "channel_ref": wsID,
	})
	if code != 201 || sent["delivery"] != "queued" {
		t.Fatalf("fallback %d %v", code, sent)
	}
}

func TestConversationPatchConflictAndPointer(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "ptr-ws")
	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "origin_channel": "ui",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, conv)
	}
	id, _ := conv["id"].(string)
	code, body, _ := doJSON(t, "PATCH", base+"/v1/conversations/"+id, map[string]any{
		"title": "n", "revision": 99,
	})
	if code != 412 || errCode(body) != "precondition_failed" {
		t.Fatalf("conflict %d %v", code, body)
	}
	code, _, _ = doJSON(t, "PATCH", base+"/v1/conversations/"+id, map[string]any{
		"title": "n", "revision": 1,
	})
	if code != 200 {
		t.Fatalf("patch %d", code)
	}
	code, _, _ = doJSON(t, "PUT", base+"/v1/channel-pointers/ui/"+wsID, map[string]any{
		"conversation_id": id,
	})
	if code != 200 {
		t.Fatalf("put pointer %d", code)
	}
	code, ptr, _ := doJSON(t, "GET", base+"/v1/channel-pointers/ui/"+wsID, nil)
	if code != 200 || ptr["conversation_id"] != id {
		t.Fatalf("get pointer %d %v", code, ptr)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/channel-pointers/ui/missing", nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("missing pointer %d %v", code, body)
	}
}

func TestConversationParallelSendsOneWake(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	writeAssistant(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "par-ws")
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
	if len(msgs) != n {
		t.Fatalf("messages %d want %d", len(msgs), n)
	}
	code, qbody, _ := doJSON(t, "GET", base+"/v1/queue?limit=50", nil)
	if code != 200 {
		t.Fatalf("queue %d %v", code, qbody)
	}
	items, _ := qbody["items"].([]any)
	if len(items) < 1 {
		t.Fatalf("expected queue rows %v", qbody)
	}
}

func TestWakeExtraPromptAndConversation(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	writeAssistant(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "wake-ws")
	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "origin_channel": "cli",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, conv)
	}
	id, _ := conv["id"].(string)
	code, run, _ := doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/assistant/wake", map[string]any{
		"extra_prompt": "focus on CI", "conversation_id": id,
	})
	if code != 201 {
		t.Fatalf("wake %d %v", code, run)
	}
	if run["conversation_id"] != id {
		t.Fatalf("conversation_id %v", run)
	}
	if run["kind"] != "assistant" {
		t.Fatalf("kind %v", run)
	}
}

func TestConversationSendWhileDraining(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	defer st.Close()
	if err := st.SetFlag("drain", "1"); err != nil {
		t.Fatal(err)
	}
	wsDir := t.TempDir()
	writeAssistant(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "drain-ws")
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
	if sent["delivery"] != "queued" {
		t.Fatalf("queued under drain %v", sent)
	}
}

func writeAssistant(t *testing.T, wsDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wsDir, "TREBI.md"), []byte("---\nharness: claude\n---\n\nYou are the assistant.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
