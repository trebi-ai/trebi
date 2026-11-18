package api_test

import (
	"context"
	"testing"

	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/waittest"
)

func TestProbeDirectManager(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "probe2-ws")
	writeFakeHarness(t, wsDir)
	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "origin_channel": "cli",
	})
	t.Logf("create %d", code)
	id := conv["id"].(string)
	code2, sent, _ := doJSON(t, "POST", base+"/v1/conversations/"+id+"/messages", map[string]any{
		"text": "hi", "channel_type": "cli", "channel_ref": wsID,
	})
	t.Logf("send %d %v", code2, sent)
	waittest.Until(t, "the session to close", func() bool {
		rec, err2 := s.Assistant().Session(context.Background(), id)
		return err2 == nil && rec.Status == assistant.StatusClosed
	})
	_, c, _ := doJSON(t, "GET", base+"/v1/conversations/"+id, nil)
	t.Logf("GET session map: %v", c["session"])
}
