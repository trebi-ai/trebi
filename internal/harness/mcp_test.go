package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
)

func TestDiscoverMCP_EmptyHome(t *testing.T) {
	if got := DiscoverMCP("", nil); got != nil {
		t.Fatalf("empty home: %v", got)
	}
}

func TestDiscoverMCP_UnionAndSkipTrebi(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude.json"), `{
		"mcpServers": {
			"sling": {"command": "sling", "args": ["serve", "mcp"]},
			"pencil": {
				"command": "/Applications/Pen.app/mcp",
				"args": ["--app", "desktop", "--agent", "claudeCodeCLI"]
			},
			"browseros": {"type": "http", "url": "http://127.0.0.1:9000/mcp"},
			"trebi": {"command": "trebi", "args": ["mcp", "serve"]}
		}
	}`)
	write(t, filepath.Join(home, ".claude", "mcp.json"), `{"mcpServers": {}}`)
	write(t, filepath.Join(home, ".cursor", "mcp.json"), `{
		"mcpServers": {
			"sling": {"command": "sling", "args": ["serve", "mcp"]},
			"browsermcp": {"command": "npx", "args": ["@browsermcp/mcp@latest"]}
		}
	}`)
	write(t, filepath.Join(home, ".pi", "agent", "mcp.json"), `{
		"mcpServers": {
			"sling": {"command": "sling", "args": ["serve", "mcp"]}
		}
	}`)

	got := DiscoverMCP(home, &config.Config{})
	byName := map[string]MCPServer{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if _, ok := byName["trebi"]; ok {
		t.Fatal("trebi MCP must be excluded")
	}
	sling := byName["sling"]
	if sling.Transport != "stdio" || sling.Command != "sling serve mcp" {
		t.Fatalf("sling %+v", sling)
	}
	if len(sling.Origins) != 3 {
		t.Fatalf("sling origins %d %+v", len(sling.Origins), sling.Origins)
	}
	if !sling.Injected {
		t.Fatal("sling should be injected by default inherit")
	}
	if len(sling.Harnesses) != 3 {
		t.Fatalf("harnesses %v", sling.Harnesses)
	}
	pencil := byName["pencil"]
	if pencil.Command != "/Applications/Pen.app/mcp --app desktop" {
		t.Fatalf("pencil command (agent stripped) %q", pencil.Command)
	}
	if byName["browsermcp"].Name == "" || byName["browseros"].Transport != "http" {
		t.Fatalf("missing cursor/http rows: %+v", byName)
	}
}

func TestDiscoverMCP_InheritOff(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".cursor", "mcp.json"), `{
		"mcpServers": {"x": {"command": "echo"}}
	}`)
	cfg := &config.Config{
		Tools: config.ToolsConfig{
			Bridge: &config.BridgeConfig{Never: []string{"mcp"}},
		},
	}
	got := DiscoverMCP(home, cfg)
	if len(got) != 1 || got[0].Injected {
		t.Fatalf("want declared not injected: %+v", got)
	}
}

func TestMCPFingerprint_StripsQuery(t *testing.T) {
	d := mcpDecl{transport: "http", rawURL: "https://example.com/mcp?api_key=secret"}
	if fp := mcpFingerprint(d); fp != "http:https://example.com/mcp" {
		t.Fatalf("fp %q", fp)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
