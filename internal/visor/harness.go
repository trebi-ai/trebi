package visor

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/platform"
)

// fakeHarness is the trebi test harness (cmd/fake). agent-wire does not know
// its program, its login, or its marker file.
const fakeHarness = agentwire.Harness("fake")

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
	if o.Harness.Variants == nil {
		o.Harness.Variants = func() map[string]string {
			c, err := config.Load()
			if err != nil {
				return nil
			}
			return c.VariantBases()
		}
	}
	o.Harness.Policy = harnessPolicy(cfg, exe)
	if o.Harness.Path == "" {
		userHome, _ := os.UserHomeDir()
		o.Harness.Path = platform.UserPath(userHome, os.Getenv("PATH"))
	}
	if len(o.Harness.Names) == 0 {
		o.Harness.Names = harness.Names()
	}
	o.Harness.Facts = harnessFacts(o.Harness.Facts)
}

// harnessFacts adds the facts of the fake harness to f. A nil field of f
// uses agent-wire for the other harnesses.
func harnessFacts(f agentvisor.HarnessFacts) agentvisor.HarnessFacts {
	if f.Recipe == nil {
		f.Recipe = agentwire.Recipe
	}
	if f.LoginCommand == nil {
		f.LoginCommand = agentwire.LoginCommandFor
	}
	if f.LogoutCommand == nil {
		f.LogoutCommand = agentwire.LogoutCommand
	}
	if f.LoginEnv == nil {
		f.LoginEnv = agentwire.LoginEnv
	}
	if f.AuthStatus == nil {
		f.AuthStatus = agentwire.AuthStatus
	}
	recipe := f.Recipe
	f.Recipe = func(h agentwire.Harness) (agentwire.InstallRecipe, bool) {
		if h == fakeHarness {
			return agentwire.InstallRecipe{}, false
		}
		return recipe(h)
	}
	login := f.LoginCommand
	f.LoginCommand = func(h agentwire.Harness, o agentwire.LoginOptions) ([]string, error) {
		if h == fakeHarness {
			return []string{"fake", "login"}, nil
		}
		return login(h, o)
	}
	logout := f.LogoutCommand
	f.LogoutCommand = func(h agentwire.Harness) []string {
		if h == fakeHarness {
			return []string{"fake", "logout"}
		}
		return logout(h)
	}
	env := f.LoginEnv
	f.LoginEnv = func(h agentwire.Harness, dir string) map[string]string {
		if h == fakeHarness {
			// The visor env has no TREBI_HOME.
			return map[string]string{"TREBI_HOME": home.Base()}
		}
		return env(h, dir)
	}
	status := f.AuthStatus
	f.AuthStatus = func(ctx context.Context, h agentwire.Harness, dir string) (agentwire.AuthState, error) {
		if h != fakeHarness {
			return status(ctx, h, dir)
		}
		if _, err := os.Stat(filepath.Join(home.HarnessDir(string(h)), "auth.json")); err != nil {
			return agentwire.AuthMissing, nil //nolint:nilerr // no marker is no login
		}
		return agentwire.AuthOK, nil
	}
	binary := f.Binary
	f.Binary = func(h agentwire.Harness) string {
		if h == fakeHarness {
			p, _ := exec.LookPath(string(h))
			return p
		}
		if binary != nil {
			return binary(h)
		}
		return ""
	}
	return f
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
