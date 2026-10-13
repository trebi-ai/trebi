package connectors

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

// A caller looks up the MCP command in its own PATH, so a command in the bin
// folder gets its full path.
func TestMCPServerBinCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "tool"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]string{
		"tool":       filepath.Join(dir, "bin", "tool"),
		"npx":        "npx",
		"/usr/bin/x": "/usr/bin/x",
	} {
		r := Resolved{Slug: "demo", Dir: dir, Manifest: manifest.Manifest{Actions: &manifest.Actions{MCP: &manifest.MCPAction{Command: cmd}}}}
		srv, _, err := r.MCPServer()
		if err != nil {
			t.Fatal(err)
		}
		if srv.Command != want {
			t.Errorf("%s: command = %q, want %q", cmd, srv.Command, want)
		}
	}
}
