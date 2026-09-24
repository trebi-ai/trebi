package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureClaudeTrebiMCPReplacesHTTP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	initial := map[string]any{"mcpServers": map[string]any{
		"trebi": map[string]any{"type": "http", "url": "http://127.0.0.1:1/mcp"},
		"keep":  map[string]any{"type": "stdio", "command": "keep"},
	}}
	b, _ := json.Marshal(initial)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureClaudeTrebiMCP(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	servers := got["mcpServers"].(map[string]any)
	trebi := servers["trebi"].(map[string]any)
	if trebi["type"] != "stdio" || trebi["command"] == "" {
		t.Fatalf("trebi=%v", trebi)
	}
	if _, ok := servers["keep"]; !ok {
		t.Fatal("other server removed")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v", st.Mode().Perm())
	}
}
