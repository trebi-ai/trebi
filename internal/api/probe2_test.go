package api_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/assistant"
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
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rec, err2 := s.Assistant().Session(context.Background(), id)
		t.Logf("rec %+v %v", rec.Status, err2)
		if err2 == nil && rec.Status == assistant.StatusClosed {
			_, c, _ := doJSON(t, "GET", base+"/v1/conversations/"+id, nil)
			t.Logf("GET session map: %v", c["session"])
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	evs, _ := s.Assistant().Events(context.Background(), id, 0, 50)
	t.Logf("events: %s", fmt.Sprint(evs))
	t.Fatal("never closed")
}
