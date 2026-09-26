package runs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/flight"
	config "github.com/trebi-ai/trebi/internal/config"
)

// Finding is one doctor row. The API maps these onto the alerts finding
// shape; the transport is intentionally the same as auth_detail hints.
type Finding struct {
	ID       string
	Severity string
	Code     string
	Message  string
}

// runtimeTool is one probed harness.
type runtimeTool struct {
	name string
	bin  string
	// flags are the protocol-surface flags the help probe must expose. Empty
	// means no subprocess wire exists; the doctor reports TUI-only.
	flags []string
	// status is the vendor status page shown in the posture line.
	status string
}

// runtimeTools are the harnesses the doctor probes. Grok stays listed without
// a wire so TUI-only posture is visible instead of silent (Part J).
var runtimeTools = []runtimeTool{
	{"claude", "claude", []string{"--output-format", "--permission-prompt-tool"}, "https://status.anthropic.com"},
	{"codex", "codex", []string{"app-server"}, "https://status.openai.com"},
	{"opencode", "opencode", []string{"serve"}, ""},
	{"opencode2", "opencode2", []string{"serve"}, ""},
	{"pi", "pi", []string{"rpc"}, ""},
	{"copilot", "copilot", []string{"--acp"}, ""},
	{"cursor", "cursor-agent", []string{"acp"}, ""},
	{"grok", "grok", nil, ""},
}

// doctorCacheTTL keeps repeated doctor calls (UI/CLI/tests) from re-probing
// every vendor binary; the flag/version surface changes far slower than this.
const doctorCacheTTL = 5 * time.Minute

var doctorCache struct {
	mu       sync.Mutex
	key      string
	at       time.Time
	findings []Finding
	// flt collapses concurrent misses into one probe round.
	flt flight.Group[string, []Finding]
}

// doctorProbeKey invalidates the cache when the environment under test (PATH,
// TREBI_HOME) changes — tests switch both between cases.
func doctorProbeKey() string {
	return os.Getenv("PATH") + "\x00" + os.Getenv("TREBI_HOME")
}

// HarnessFindings reports per-harness runtime posture (Part J): runtime kind,
// binary pin + version, protocol-flag probe, and the entitlement posture line.
// No cost probe and no pricing assumptions. Probes run in parallel and the
// result is cached briefly so doctor stays responsive.
func (s *Service) HarnessFindings(ctx context.Context) []Finding {
	key := doctorProbeKey()
	doctorCache.mu.Lock()
	if doctorCache.key == key && time.Since(doctorCache.at) < doctorCacheTTL && doctorCache.findings != nil {
		out := append([]Finding(nil), doctorCache.findings...)
		doctorCache.mu.Unlock()
		return out
	}
	doctorCache.mu.Unlock()

	// One probe round at a time: concurrent misses share the result instead
	// of stacking vendor process spawns (plan 2026-09-26 H).
	out, err := doctorCache.flt.Do(key, func() ([]Finding, error) {
		doctorCache.mu.Lock()
		if doctorCache.key == key && time.Since(doctorCache.at) < doctorCacheTTL && doctorCache.findings != nil {
			cached := append([]Finding(nil), doctorCache.findings...)
			doctorCache.mu.Unlock()
			return cached, nil
		}
		doctorCache.mu.Unlock()
		findings, err := s.probeHarnessFindings(ctx)
		if err != nil {
			return nil, err
		}
		doctorCache.mu.Lock()
		doctorCache.key = key
		doctorCache.at = time.Now()
		doctorCache.findings = findings
		doctorCache.mu.Unlock()
		return findings, nil
	})
	if err != nil {
		return nil
	}
	return append([]Finding(nil), out...)
}

// probeHarnessFindings runs one parallel probe round over every tool.
func (s *Service) probeHarnessFindings(ctx context.Context) ([]Finding, error) {
	cfg, _ := config.Load()
	results := make([][]Finding, len(runtimeTools))
	var wg sync.WaitGroup
	for i := range runtimeTools {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			killSwitch := cfg != nil && strings.EqualFold(cfg.HarnessRuntimeFor(runtimeTools[i].name), "tui")
			results[i] = s.collectToolFindings(ctx, runtimeTools[i], killSwitch)
		}()
	}
	wg.Wait()
	var out []Finding
	for _, r := range results {
		out = append(out, r...)
	}
	return out, nil
}

// collectToolFindings probes one harness. The library owns the binary, version
// and login probes; trebi owns the kill switch, the required-flag list and the
// wording.
func (s *Service) collectToolFindings(ctx context.Context, tool runtimeTool, killSwitch bool) []Finding {
	bin := installedBinary(tool.name, tool.bin)
	if bin == "" {
		return nil
	}
	if killSwitch {
		return []Finding{{
			ID: "harness-runtime-" + tool.name, Severity: "warn", Code: "harness_runtime_tui",
			Message: fmt.Sprintf("%s: harness.runtime is tui (incident kill switch); every vendor run opens a pane until it is cleared", tool.name),
		}}
	}
	rt := s.agentwireRuntime()
	driver, ok := rt.Driver(agentwire.Harness(tool.name))
	if !ok {
		// No proven wire: the pane is the only runtime. Say so (Part J).
		msg := fmt.Sprintf("%s: runtime tui only (no proven subprocess wire); runs open a tmux pane", tool.name)
		if version := rt.Version(ctx, bin); version != "" {
			msg += ", " + version
		}
		return []Finding{{
			ID: "harness-runtime-" + tool.name, Severity: "info", Code: "harness_runtime_tui_only", Message: msg,
		}}
	}
	// Detect owns the binary, version-floor and login probes. The pin is what
	// actually runs, so its own version wins when Detect probed another path.
	det, _ := rt.Detect(ctx, agentwire.Harness(tool.name))
	version := det.Version
	if det.Path != bin {
		version = rt.Version(ctx, bin)
	}
	probe := "flag not found"
	if len(tool.flags) == 0 {
		probe = ""
	} else {
		help := rt.HelpText(ctx, bin)
		missing := []string{}
		for _, f := range tool.flags {
			if !strings.Contains(help, f) {
				missing = append(missing, f)
			}
		}
		if len(missing) == 0 {
			probe = strings.Join(tool.flags, " ok, ") + " ok"
		} else {
			probe = "missing " + strings.Join(missing, ", ")
		}
	}
	msg := fmt.Sprintf("%s: runtime cli (%s driver) via %s", tool.name, driver.Name(), bin)
	if version != "" {
		msg += ", " + version
	}
	if probe != "" {
		msg += ", " + probe
	}
	msg += "; subscription login via installed binary; vendor may meter or restrict programmatic use — check vendor status"
	if tool.status != "" {
		msg += " (" + tool.status + ")"
	}
	out := []Finding{{
		ID: "harness-runtime-" + tool.name, Severity: "info", Code: "harness_runtime", Message: msg,
	}}
	if tool.name == "codex" {
		if canary, ok := codexSchemaCanary(ctx, bin); ok {
			out = append(out, canary)
		}
	}
	return out
}

// codexSchemaCanary stores a hash of `codex app-server generate-json-schema`
// and reports a change (E.2 drift canary). First run stores silently.
func codexSchemaCanary(ctx context.Context, bin string) (Finding, bool) {
	dir, err := os.MkdirTemp("", "trebi-codex-schema-*")
	if err != nil {
		return Finding{}, false
	}
	defer os.RemoveAll(dir)
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(runCtx, bin, "app-server", "generate-json-schema", "--out", dir).CombinedOutput(); err != nil {
		_ = out
		return Finding{}, false
	}
	hash := hashTree(dir)
	if hash == "" {
		return Finding{}, false
	}
	canaryPath := filepath.Join(config.HarnessDir("codex"), ".trebi-appserver-schema")
	prev, _ := os.ReadFile(canaryPath)
	if string(prev) == hash {
		return Finding{}, false
	}
	_ = os.MkdirAll(filepath.Dir(canaryPath), 0o755)
	_ = os.WriteFile(canaryPath, []byte(hash), 0o644)
	if len(prev) == 0 {
		return Finding{}, false
	}
	return Finding{
		ID: "harness-codex-schema", Severity: "warn", Code: "codex_schema_changed",
		Message: "codex: app-server JSON schema changed (" + hash[:12] + "); re-run the codex live fixtures and check the pin before trusting runs",
	}, true
}

// hashTree hashes file names and contents under dir (stable order).
func hashTree(dir string) string {
	var files []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if len(files) == 0 {
		return ""
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f)
		h.Write([]byte(rel))
		data, err := os.ReadFile(f)
		if err != nil {
			return ""
		}
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// installedBinary prefers the pinned store binary, then PATH.
func installedBinary(tool, bin string) string {
	pin := filepath.Join(config.BinDir(), tool, "current", bin)
	if st, err := statExecutable(pin); err == nil && st {
		return pin
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	return ""
}

func statExecutable(path string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false, err
	}
	return true, nil
}
