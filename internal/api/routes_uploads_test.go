package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/waittest"
)

var tinyPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00") //nolint:gochecknoglobals // test fixture

func TestUploadRoutes(t *testing.T) {
	base := startUploadServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "up-ws")
	otherID := addWorkspace(t, base, t.TempDir(), "up-other")
	url := base + "/v1/workspaces/" + wsID + "/uploads"

	code, u := postUpload(t, url, tinyPNG)
	if code != 201 || u["mime"] != "image/png" || !strings.HasPrefix(u["id"].(string), "upl_") {
		t.Fatalf("put %d %v", code, u)
	}
	id := u["id"].(string)

	resp, err := http.Get(url + "/" + id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(b, tinyPNG) {
		t.Fatalf("get %d %s %q", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}

	if code, body := postUpload(t, url, []byte("just some text")); code != 415 {
		t.Fatalf("text %d %v", code, body)
	}
	big := append(append([]byte{}, tinyPNG...), make([]byte, 100)...)
	if code, body := postUpload(t, url, big); code != 413 {
		t.Fatalf("large %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "GET", base+"/v1/workspaces/"+otherID+"/uploads/"+id, nil); code != 404 {
		t.Fatalf("other workspace %d %v", code, body)
	}

	if err := os.WriteFile(filepath.Join(wsDir, "shot.png"), tinyPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, body := postUpload(t, url+"?path=shot.png", nil); code != 201 || body["mime"] != "image/png" {
		t.Fatalf("path %d %v", code, body)
	}
	if code, body := postUpload(t, url+"?path=../x.png", nil); code != 400 {
		t.Fatalf("outside path %d %v", code, body)
	}
}

// TestConversationSendAttachments: an image with no text reaches the
// assistant, and the message and the user event carry the upload id.
func TestConversationSendAttachments(t *testing.T) {
	base := startUploadServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "att-ws")
	otherID := addWorkspace(t, base, t.TempDir(), "att-other")
	writeAssistant(t, wsDir)
	_, u := postUpload(t, base+"/v1/workspaces/"+wsID+"/uploads", tinyPNG)
	id := u["id"].(string)
	_, foreign := postUpload(t, base+"/v1/workspaces/"+otherID+"/uploads", tinyPNG)

	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{"workspace": wsID, "origin_channel": "ui"})
	if code != 201 {
		t.Fatalf("create %d %v", code, conv)
	}
	cid := conv["id"].(string)
	msgs := base + "/v1/conversations/" + cid + "/messages"

	for _, bad := range []string{"upl_missing", foreign["id"].(string), "../etc/passwd"} {
		if code, body, _ := doJSON(t, "POST", msgs, map[string]any{"attachments": []string{bad}}); code != 400 {
			t.Fatalf("attachment %q: %d %v", bad, code, body)
		}
	}
	code, sent, _ := doJSON(t, "POST", msgs, map[string]any{"attachments": []string{id}})
	if code != 201 || sent["delivery"] != "prompted" {
		t.Fatalf("send %d %v", code, sent)
	}
	waittest.Until(t, "session done", func() bool {
		_, c, _ := doJSON(t, "GET", base+"/v1/conversations/"+cid, nil)
		sess, _ := c["session"].(map[string]any)
		return sess != nil && (sess["status"] == assistant.StatusIdle || sess["status"] == assistant.StatusClosed)
	})

	_, body, _ := doJSON(t, "GET", msgs+"?limit=10", nil)
	if !strings.Contains(mustJSONString(t, body), `"attachments":["`+id+`"]`) {
		t.Fatalf("messages lack the attachment: %v", body)
	}
	_, body, _ = doJSON(t, "GET", base+"/v1/conversations/"+cid+"/events", nil)
	if !strings.Contains(mustJSONString(t, body), `"attachments":["`+id+`"]`) {
		t.Fatalf("user event lacks the attachment: %v", body)
	}
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// startUploadServer starts a server with a 64-byte upload limit.
func startUploadServer(t *testing.T) string {
	t.Helper()
	_, base := startTestServerSeeded(t, api.Options{}, func(*store.Store) {
		f, err := os.OpenFile(filepath.Join(home.Base(), "config.yaml"), os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString("uploads:\n  max_bytes: 64\n"); err != nil {
			t.Fatal(err)
		}
	})
	return base
}

func postUpload(t *testing.T, url string, body []byte) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}
