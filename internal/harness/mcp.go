package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/config"
)

// MCPOrigin is one file+key a discovered MCP server was declared in.
type MCPOrigin struct {
	Kind string `json:"kind"` // user
	Tool string `json:"tool"`
	Path string `json:"path"`
	Key  string `json:"key"`
}

// MCPServer is one logical MCP server after unioning duplicate declarations.
type MCPServer struct {
	Name        string      `json:"name"`
	Transport   string      `json:"transport"` // stdio|http
	Command     string      `json:"command,omitempty"`
	URL         string      `json:"url,omitempty"`
	Fingerprint string      `json:"-"`
	Origins     []MCPOrigin `json:"origins"`
	Harnesses   []string    `json:"harnesses"`
	Injected    bool        `json:"injected"`
}

type mcpDecl struct {
	tool, path, key, transport, command, rawURL string
	args                                        []string
}

var mcpScanFiles = []struct {
	tool string
	rel  string
}{
	{tool: "claude", rel: ".claude/mcp.json"},
	{tool: "claude", rel: ".claude.json"},
	{tool: "codex", rel: ".codex/mcp.json"},
	{tool: "cursor", rel: ".cursor/mcp.json"},
	{tool: "pi", rel: ".pi/agent/mcp.json"},
}

// DiscoverMCP scans user-global harness MCP configs that EnsureBridges injects.
// userHome empty skips the scan (tests). cfg nil loads trebi config for inherit.
func DiscoverMCP(userHome string, cfg *config.Config) []MCPServer {
	if userHome == "" {
		return nil
	}
	if cfg == nil {
		cfg, _ = config.Load()
	}
	var decls []mcpDecl
	for _, src := range mcpScanFiles {
		path := filepath.Join(userHome, src.rel)
		servers := readMCPServers(path)
		if len(servers) == 0 {
			continue
		}
		for key, raw := range servers {
			d, ok := parseMCPEntry(src.tool, path, key, raw)
			if !ok || isTrebiMCP(d) {
				continue
			}
			decls = append(decls, d)
		}
	}
	return unionMCPDecls(decls, cfg)
}

func readMCPServers(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var root map[string]any
	if json.Unmarshal(b, &root) != nil || root == nil {
		return nil
	}
	m, _ := root["mcpServers"].(map[string]any)
	return m
}

func parseMCPEntry(tool, path, key string, raw any) (mcpDecl, bool) {
	m, ok := raw.(map[string]any)
	if !ok || key == "" {
		return mcpDecl{}, false
	}
	if enabled, ok := m["enabled"].(bool); ok && !enabled {
		return mcpDecl{}, false
	}
	if disabled, ok := m["disabled"].(bool); ok && disabled {
		return mcpDecl{}, false
	}
	d := mcpDecl{tool: tool, path: path, key: key}
	if u := asString(m["url"]); u != "" {
		d.rawURL = u
		d.transport = "http"
		d.command = u
		return d, true
	}
	typ := strings.ToLower(asString(m["type"]))
	if typ == "http" || typ == "sse" {
		d.rawURL = asString(m["url"])
		d.transport = "http"
		d.command = d.rawURL
		return d, d.rawURL != ""
	}
	cmd, args := mcpCommandArgs(m["command"], m["args"])
	if cmd == "" {
		return mcpDecl{}, false
	}
	d.transport = "stdio"
	d.command = cmd
	d.args = args
	return d, true
}

func mcpCommandArgs(command, args any) (string, []string) {
	switch c := command.(type) {
	case string:
		return c, asStringSlice(args)
	case []any:
		if len(c) == 0 {
			return "", nil
		}
		out := asStringSlice(c)
		return out[0], out[1:]
	}
	return "", nil
}

func unionMCPDecls(decls []mcpDecl, cfg *config.Config) []MCPServer {
	byFP := map[string]*MCPServer{}
	var order []string
	for _, d := range decls {
		fp := mcpFingerprint(d)
		srv, ok := byFP[fp]
		if !ok {
			srv = &MCPServer{
				Name:        d.key,
				Transport:   d.transport,
				Command:     displayCommand(d),
				URL:         d.rawURL,
				Fingerprint: fp,
			}
			byFP[fp] = srv
			order = append(order, fp)
		} else if srv.Name != d.key {
			// majority name: first key wins; keep it
		}
		srv.Origins = append(srv.Origins, MCPOrigin{Kind: "user", Tool: d.tool, Path: d.path, Key: d.key})
		srv.Harnesses = appendUnique(srv.Harnesses, d.tool)
		if config.ResolveResourceInherit(d.tool, "mcp", cfg) {
			srv.Injected = true
		}
	}
	out := make([]MCPServer, 0, len(order))
	for _, fp := range order {
		out = append(out, *byFP[fp])
	}
	return out
}

func mcpFingerprint(d mcpDecl) string {
	return MCPFingerprint(d.transport, d.command, d.args, d.rawURL)
}

// MCPFingerprint identifies one MCP server by its transport and target. The
// connectors service reuses it so a managed server and a discovered one
// fingerprint the same way.
func MCPFingerprint(transport, command string, args []string, url string) string {
	if transport == "http" || url != "" {
		return "http:" + canonicalMCPURL(url)
	}
	parts := append([]string{command}, stripAgentArgs(args)...)
	return "stdio:" + strings.Join(parts, " ")
}

func displayCommand(d mcpDecl) string {
	if d.transport == "http" {
		return d.rawURL
	}
	parts := append([]string{d.command}, stripAgentArgs(d.args)...)
	return strings.Join(parts, " ")
}

func stripAgentArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--agent" {
			i++
			continue
		}
		if strings.HasPrefix(a, "--agent=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func canonicalMCPURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return strings.TrimRight(raw, "/")
	}
	u.Fragment = ""
	// Drop credentials; keep host+path. Query may hold tokens — strip it.
	u.User = nil
	u.RawQuery = ""
	s := u.String()
	return strings.TrimRight(s, "/")
}

func isTrebiMCP(d mcpDecl) bool {
	if strings.EqualFold(d.key, "trebi") {
		return true
	}
	base := filepath.Base(d.command)
	if base == "trebi" || base == "trebi.exe" {
		joined := strings.Join(d.args, " ")
		if strings.Contains(joined, "mcp") {
			return true
		}
	}
	return false
}

// MCPFingerprintID is a stable inherited connector id suffix.
func MCPFingerprintID(fp string) string {
	sum := sha256.Sum256([]byte(fp))
	return hex.EncodeToString(sum[:4])
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asStringSlice(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, i := range x {
			if s := asString(i); s != "" {
				out = append(out, s)
				continue
			}
			if s := stringify(i); s != "" && s != "null" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func stringify(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func appendUnique(in []string, v string) []string {
	for _, s := range in {
		if s == v {
			return in
		}
	}
	return append(in, v)
}
