package visor

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
)

// configureHarnesses gives the visor the trebi policy for the harness homes.
// A visor process gets only its own home, so the trebi home is its parent.
func configureHarnesses(o *agentvisor.Options, exe string) {
	if o.Home != "" && filepath.Clean(o.Home) != filepath.Clean(home.VisorDir()) {
		home.SetBase(filepath.Dir(o.Home))
	}
	moved, err := home.MoveHarnessDirs()
	if err != nil {
		slog.Warn("move the harness folders to the visor", "error", err)
	}
	if runtime.GOOS == "darwin" && slices.Contains(moved, "claude") {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		kc := claudeKeychain{security: "/usr/bin/security"}
		if err := kc.move(ctx, home.OldHarnessDir("claude"), home.HarnessDir("claude")); err != nil {
			slog.Warn("move the Claude login to the new folder", "error", err)
		}
		cancel()
	}
	cfg, err := config.Load()
	if err != nil {
		cfg = nil
	}
	if o.Harness.Enabled == nil {
		o.Harness.Enabled = func(name string) bool {
			c, err := config.Load()
			return err == nil && c.HarnessEnabled(name)
		}
	}
	o.Harness.Policy = harnessPolicy(cfg, exe)
}

// harnessPolicy is the trebi MCP server, no user hooks, the share defaults
// of config.yaml, and the grok keys.
func harnessPolicy(cfg *config.Config, exe string) agentvisor.HarnessPolicy {
	p := agentvisor.HarnessPolicy{
		NoUserHooks: true,
		Share:       map[string]map[string]bool{},
		Keys:        map[string][]agentwire.HomeKey{},
	}
	if exe != "" {
		p.MCPServers = []agentwire.MCPServer{{Name: "trebi", Command: exe, Args: []string{"mcp", "serve"}}}
	}
	for tool, keys := range config.KnownBridgeResources() {
		for _, key := range keys {
			if p.Share[tool] == nil {
				p.Share[tool] = map[string]bool{}
			}
			p.Share[tool][key] = config.ResolveResourceInherit(tool, key, cfg) && !config.ResourceOwn(tool, key, cfg)
		}
	}
	grok := string(agentwire.Grok)
	for _, k := range config.CompatKeys(grok) {
		p.Keys[grok] = append(p.Keys[grok], agentwire.HomeKey{File: "config.toml", Key: "compat.claude." + k, Value: cfg.ClaudeCompat(grok, k)})
	}
	p.Keys[grok] = append(p.Keys[grok],
		agentwire.HomeKey{File: "config.toml", Key: "telemetry.trace_upload", Value: false},
		agentwire.HomeKey{File: "config.toml", Key: "harness.disable_codebase_upload", Value: true},
	)
	return p
}

func executable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}
