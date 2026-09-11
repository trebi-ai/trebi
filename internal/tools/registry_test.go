package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/request"
)

type fakeDisp struct {
	status int
	body   []byte
	err    error
	last   request.HTTPRequest
	n      int
}

func (f *fakeDisp) Dispatch(_ context.Context, req request.HTTPRequest) (int, []byte, error) {
	f.n++
	f.last = req
	return f.status, f.body, f.err
}

func TestRegistryUnknownToolAndAction(t *testing.T) {
	t.Parallel()
	d := &fakeDisp{status: 200, body: []byte(`{}`)}
	r := NewRegistry(d)
	res := r.Call(context.Background(), "nope", "list", nil)
	if res.Status != 400 || d.n != 0 {
		t.Fatalf("unknown tool: status=%d n=%d", res.Status, d.n)
	}
	if !strings.Contains(string(res.Body), "unknown action") {
		t.Fatalf("body=%s", res.Body)
	}
	res = r.Call(context.Background(), "run", "nope", nil)
	if res.Status != 400 || d.n != 0 {
		t.Fatalf("unknown action: status=%d n=%d", res.Status, d.n)
	}
}

func TestRegistryDocsOnEveryTool(t *testing.T) {
	t.Parallel()
	r := NewRegistry(&fakeDisp{})
	for tool := range NewCatalog() {
		res := r.Call(context.Background(), tool, "docs", nil)
		if res.Status != 200 {
			t.Errorf("%s docs: status %d %s", tool, res.Status, res.Body)
			continue
		}
		if !strings.Contains(string(res.Body), "## Signatures") || !strings.Contains(string(res.Body), "docs()") {
			t.Errorf("%s docs missing signature block", tool)
		}
	}
}

func TestRegistryBoundedBody(t *testing.T) {
	t.Parallel()
	raw := bytes.Repeat([]byte("a"), defaultMaxBytes+50)
	d := &fakeDisp{status: 200, body: raw}
	res := NewRegistry(d).Call(context.Background(), "system", "logs", nil)
	if res.Status != 200 {
		t.Fatalf("status %d %s", res.Status, res.Body)
	}
	var wrap struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(res.Body, &wrap); err != nil {
		t.Fatal(err)
	}
	if !wrap.Truncated || len(wrap.Content) != defaultMaxBytes {
		t.Fatalf("truncated=%v len=%d", wrap.Truncated, len(wrap.Content))
	}
}

func TestRegistryTransformSuccessOnly(t *testing.T) {
	t.Parallel()
	dump := []byte("data: {\"seq\":3}\n\n")
	d := &fakeDisp{status: 200, body: dump}
	res := NewRegistry(d).Call(context.Background(), "system", "bus_poll", nil)
	if res.Status != 200 {
		t.Fatalf("status %d", res.Status)
	}
	if !strings.Contains(string(res.Body), `"next_cursor"`) {
		t.Fatalf("transform not applied: %s", res.Body)
	}
	errBody := []byte(`{"error":{"code":"validation","message":"no"}}`)
	d.status, d.body = 400, errBody
	res = NewRegistry(d).Call(context.Background(), "system", "bus_poll", nil)
	if res.Status != 400 || string(res.Body) != string(errBody) {
		t.Fatalf("error envelope mutated: status=%d body=%s", res.Status, res.Body)
	}
}

func TestRegistryNativeDenyShutdown(t *testing.T) {
	t.Parallel()
	d := &fakeDisp{status: 200, body: []byte(`{}`)}
	r := NewNativeRegistry(d)
	for _, def := range r.Tools() {
		if def.Name != "system" {
			continue
		}
		for _, a := range def.Actions {
			if a == "shutdown" {
				t.Fatal("system.shutdown must not appear in the native tool set")
			}
		}
	}
	res := r.Call(context.Background(), "system", "shutdown", Input{"confirm": true})
	if res.Status != 400 || d.n != 0 {
		t.Fatalf("shutdown dispatched: status=%d n=%d body=%s", res.Status, d.n, res.Body)
	}
	if !strings.Contains(string(res.Body), "unknown action") {
		t.Fatalf("want unknown-action, got %s", res.Body)
	}
}

func TestRegistryNativeDenyAssistantConfig(t *testing.T) {
	t.Parallel()
	d := &fakeDisp{status: 200, body: []byte(`{}`)}
	r := NewNativeRegistry(d)
	res := r.Call(context.Background(), "config", "set", Input{"key": "assistant.model", "value": "x"})
	if res.Status != 400 || d.n != 0 {
		t.Fatalf("assistant.model set dispatched: status=%d n=%d", res.Status, d.n)
	}
	res = NewRegistry(d).Call(context.Background(), "config", "set", Input{"key": "assistant.model", "value": "x"})
	if d.n == 0 {
		t.Fatal("full catalog must still dispatch config.set assistant.*")
	}
	_ = res
}

func TestRegistryValueFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "api-key.txt")
	if err := os.WriteFile(path, []byte("sk-do-not-leak\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &fakeDisp{status: 200, body: []byte(`{}`)}
	res := NewNativeRegistry(d).Call(context.Background(), "secret", "set", Input{
		"name": "API_KEY", "value_file": path,
	})
	if res.Err != nil || d.n != 1 {
		t.Fatalf("dispatch: n=%d err=%v status=%d body=%s", d.n, res.Err, res.Status, res.Body)
	}
	body, _ := json.Marshal(d.last.Body)
	if !strings.Contains(string(body), "sk-do-not-leak") {
		t.Fatalf("value not in request: %s", body)
	}
	if strings.Contains(string(body), "value_file") || strings.Contains(d.last.Path, "value_file") {
		t.Fatalf("value_file leaked to dispatch: path=%s body=%s", d.last.Path, body)
	}
}

func TestRegistryToolPayloadSize(t *testing.T) {
	t.Parallel()
	total := 0
	for _, def := range NewRegistry(nil).Tools() {
		b, err := json.Marshal(def.Schema)
		if err != nil {
			t.Fatal(err)
		}
		total += len(b) + len(def.Name) + len(def.Description)
	}
	t.Logf("catalog tool payload: %d bytes", total)
	if total > 20*1024 {
		t.Fatalf("catalog tool payload %d bytes; MCP budget is 20 KiB", total)
	}
}
