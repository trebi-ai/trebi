package tmux

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
)

func TestDetect_AllTools(t *testing.T) {
	t.Parallel()
	cases := []struct {
		command string
		want    string
	}{
		{"claude", Claude},
		{"claude --dangerously-skip-permissions", Claude},
		{"/usr/local/bin/claude", Claude},
		{"codex", Codex},
		{"/opt/homebrew/bin/codex exec", Codex},
		{"copilot", Copilot},
		{"copilot-cli chat", Copilot},
		{"gemini", Gemini},
		{"opencode", OpenCode},
		{"opencode --auto", OpenCode},
		{"/usr/bin/opencode run", OpenCode},
		{"grok", Grok},
		{"grok --resume abc", Grok},
		{"/usr/local/bin/grok", Grok},
		{"pi", Pi},
		{"pi --approve", Pi},
		{"/usr/local/bin/pi", Pi},
		{"", ""},
		{"unknown-tool", ""},
		{"grok-cli", ""}, // community binary — not official Grok Build
		{"pixel", ""},    // must not match pi
	}
	for _, c := range cases {
		got := detectTool(c.command)
		if got != c.want {
			t.Errorf("detectTool(%q) = %q, want %q", c.command, got, c.want)
		}
		tool := Detect(c.command)
		if c.want == "" {
			if !IsUnknown(tool) {
				t.Errorf("Detect(%q) should be unknown, got %s", c.command, tool.Name())
			}
			continue
		}
		if tool.Name() != c.want {
			t.Errorf("Detect(%q).Name() = %q, want %q", c.command, tool.Name(), c.want)
		}
	}
}

func TestByName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{Claude, Codex, Copilot, Gemini, OpenCode, Grok, Pi} {
		tool, ok := ByName(name)
		if !ok || tool.Name() != name {
			t.Errorf("ByName(%q) failed", name)
		}
	}
	if _, ok := ByName("nope"); ok {
		t.Error("ByName(nope) should fail")
	}
}

func TestPrepareLaunch_KnownSessionID(t *testing.T) {
	t.Parallel()
	for _, name := range []string{Claude, Grok, Pi} {
		tool, _ := ByName(name)
		plan := tool.PrepareLaunch(name)
		if plan.SessionID == "" {
			t.Errorf("%s: expected session id", name)
		}
		if !strings.Contains(plan.Command, "--session-id") {
			t.Errorf("%s: command %q missing --session-id", name, plan.Command)
		}
		if !strings.Contains(plan.Command, plan.SessionID) {
			t.Errorf("%s: command %q missing id %s", name, plan.Command, plan.SessionID)
		}
	}
}

func TestPrepareLaunch_PiIsolationEnv(t *testing.T) {
	home.SetBase(t.TempDir())
	defer home.SetBase("")

	// Seed a user Pi MCP server so harness merge can detect it.
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	userAgent := filepath.Join(userHome, ".pi", "agent")
	if err := os.MkdirAll(userAgent, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userAgent, "mcp.json"),
		[]byte(`{"mcpServers":{"fixture-sling":{"command":"sling","args":["serve","mcp"]}}}`), 0644); err != nil {
		t.Fatal(err)
	}

	tool, _ := ByName(Pi)
	plan := tool.PrepareLaunch("pi")
	if plan.Env["PI_CODING_AGENT_DIR"] == "" {
		t.Fatal("expected PI_CODING_AGENT_DIR")
	}
	dir := plan.Env["PI_CODING_AGENT_DIR"]
	if !strings.Contains(dir, "harness") || !strings.HasSuffix(dir, "pi") {
		t.Errorf("PI_CODING_AGENT_DIR = %q", dir)
	}
	if !strings.Contains(plan.Command, "--approve") {
		t.Errorf("command missing --approve: %q", plan.Command)
	}
	ext := filepath.Join(dir, "extensions", "trebi-lifecycle.ts")
	data, err := os.ReadFile(ext)
	if err != nil {
		t.Fatalf("lifecycle extension: %v", err)
	}
	if !strings.Contains(string(data), "trebi agent-hook") {
		t.Error("pi extension should call trebi agent-hook")
	}
	if !strings.Contains(string(data), "agent_settled") {
		t.Error("pi extension should map agent_settled → stop")
	}
	if !strings.Contains(string(data), "TREBI_RUN_ID") {
		t.Error("pi extension should check TREBI_RUN_ID")
	}

	// settings.packages must list pi-mcp-adapter by default.
	sdata, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if !strings.Contains(string(sdata), "npm:pi-mcp-adapter") {
		t.Errorf("settings missing pi-mcp-adapter package: %s", sdata)
	}

	// mcp.json merges user servers + host discovery.
	mdata, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if err != nil {
		t.Fatalf("mcp.json: %v", err)
	}
	ms := string(mdata)
	if !strings.Contains(ms, "fixture-sling") {
		t.Errorf("mcp.json should merge user server fixture-sling: %s", ms)
	}
	if !strings.Contains(ms, "hostConfigDiscovery") {
		t.Errorf("mcp.json should enable hostConfigDiscovery: %s", ms)
	}
	if !strings.Contains(ms, "claude-code") {
		t.Errorf("mcp.json should list host imports: %s", ms)
	}
}

func TestEnsurePiMcpConfig_MergeAndDiscovery(t *testing.T) {
	dir := t.TempDir()
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	if err := os.MkdirAll(filepath.Join(userHome, ".pi", "agent"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userHome, ".pi", "agent", "mcp.json"),
		[]byte(`{"mcpServers":{"a":{"command":"echo"}},"imports":["cursor"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ensurePiMcpConfig(dir, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if _, ok := servers["a"]; !ok {
		t.Fatalf("want server a: %v", m)
	}
	settings, _ := m["settings"].(map[string]any)
	if settings["hostConfigDiscovery"] != "on" {
		t.Fatalf("hostConfigDiscovery: %v", settings)
	}
}

func TestEnsurePiPackageInSettings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ensurePiPackageInSettings(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "npm:pi-mcp-adapter") {
		t.Fatalf("packages not merged: %s", raw)
	}
	if err := ensurePiPackageInSettings(path); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(path)
	if strings.Count(string(raw2), "npm:pi-mcp-adapter") != 1 {
		t.Fatalf("duplicate package entry: %s", raw2)
	}
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 3, "hel…"},
		{"café", 3, "caf…"},
		{"日本語です", 3, "日本語…"},
		{"emoji😀end", 6, "emoji😀…"},
		{"", 4, ""},
		{"ab", 0, ""},
	}
	for _, tc := range cases {
		if got := truncateRunes(tc.s, tc.n); got != tc.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}

func TestPrepareLaunch_CodexNotifyQuoted(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })

	plan := (codexTool{}).PrepareLaunch("codex")
	if plan.Command != "codex -c check_for_update_on_startup=false" {
		t.Fatalf("no shim: command %q want only the update switch", plan.Command)
	}
	if plan.Env["CODEX_HOME"] != "" {
		t.Fatalf("CODEX_HOME=%q want empty (no cold harness home)", plan.Env["CODEX_HOME"])
	}

	notify := filepath.Join(config.LibexecDir(), "trebi-codex-notify")
	if err := os.MkdirAll(filepath.Dir(notify), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notify, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan = (codexTool{}).PrepareLaunch("codex exec")
	want := `-c 'notify=["` + notify + `"]'`
	if !strings.Contains(plan.Command, want) {
		t.Fatalf("command %q want substring %q", plan.Command, want)
	}
}

func TestPrepareLaunch_PassThrough(t *testing.T) {
	// Codex injects -c notify when shim exists; allow extra flags.
	for _, name := range []string{Copilot, Gemini} {
		tool, _ := ByName(name)
		plan := tool.PrepareLaunch(name + " --foo")
		if plan.SessionID != "" {
			t.Errorf("%s: expected empty session id, got %s", name, plan.SessionID)
		}
		if plan.Command != name+" --foo" {
			t.Errorf("%s: command mutated: %q", name, plan.Command)
		}
	}
	// OpenCode keeps command, adds OPENCODE_CONFIG and OPENCODE_CONFIG_DIR env.
	home.SetBase(t.TempDir())
	defer home.SetBase("")
	tool, _ := ByName(OpenCode)
	plan := tool.PrepareLaunch("opencode --foo")
	if plan.SessionID != "" {
		t.Errorf("opencode: expected empty session id")
	}
	if plan.Command != "opencode --foo" {
		t.Errorf("opencode: command mutated: %q", plan.Command)
	}
	if plan.Env["OPENCODE_CONFIG"] == "" {
		t.Error("opencode: expected OPENCODE_CONFIG env")
	}
	// The config dir is what makes OpenCode scan the bridged skills and
	// plugins; the merged config file alone is not a directory.
	if dir := plan.Env["OPENCODE_CONFIG_DIR"]; dir != config.HarnessDir(OpenCode) {
		t.Errorf("opencode: OPENCODE_CONFIG_DIR=%q want harness home %q", dir, config.HarnessDir(OpenCode))
	}
}

func TestPrepareLaunch_GrokIsolationEnv(t *testing.T) {
	// Isolate BaseDir so we don't write into the real ~/.trebi.
	home.SetBase(t.TempDir())
	defer home.SetBase("")

	tool, _ := ByName(Grok)
	plan := tool.PrepareLaunch("grok")
	if plan.Env["GROK_HOME"] == "" {
		t.Fatal("expected GROK_HOME")
	}
	if !strings.Contains(plan.Env["GROK_HOME"], "harness") || !strings.HasSuffix(plan.Env["GROK_HOME"], "grok") {
		t.Errorf("GROK_HOME = %q", plan.Env["GROK_HOME"])
	}
	// Hooks should be materialized under harness.
	hooks := filepath.Join(plan.Env["GROK_HOME"], "hooks", "trebi.json")
	data, err := os.ReadFile(hooks)
	if err != nil {
		t.Fatalf("hooks: %v", err)
	}
	if !strings.Contains(string(data), "permission_prompt") {
		t.Error("grok hooks should matcher-filter Notification to permission_prompt")
	}
	if strings.Contains(string(data), `"command": "trebi agent-hook waiting"`) &&
		!strings.Contains(string(data), "permission_prompt") {
		t.Error("waiting without matcher would spam idle_prompt")
	}
}

func TestPrepareLaunch_ClaudeEnsureBridges(t *testing.T) {
	trebiBase := t.TempDir()
	home.SetBase(trebiBase)
	t.Cleanup(func() { home.SetBase("") })
	// User skills under real home would be used; we only assert harness dir is created
	// and EnsureBridges is reachable without error via PrepareLaunch.
	plan := claudeTool{}.PrepareLaunch("claude")
	if plan.Env["CLAUDE_CONFIG_DIR"] == "" {
		t.Fatal("expected CLAUDE_CONFIG_DIR")
	}
	if _, ok := plan.Env["CLAUDE_SECURESTORAGE_CONFIG_DIR"]; !ok {
		t.Fatal("shared launch must pin CLAUDE_SECURESTORAGE_CONFIG_DIR")
	}
	// harness dir materialized
	if st, err := os.Stat(plan.Env["CLAUDE_CONFIG_DIR"]); err != nil || !st.IsDir() {
		t.Fatalf("harness dir: %v", err)
	}
}

func TestPrepareLaunch_ClaudeSettingsMatchHumanAttention(t *testing.T) {
	home.SetBase(t.TempDir())
	defer home.SetBase("")

	tool, _ := ByName(Claude)
	plan := tool.PrepareLaunch("claude --dangerously-skip-permissions")
	if plan.Env["CLAUDE_CONFIG_DIR"] == "" {
		t.Fatal("expected CLAUDE_CONFIG_DIR")
	}
	settings := filepath.Join(plan.Env["CLAUDE_CONFIG_DIR"], "trebi-settings.json")
	data, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	s := string(data)
	for _, want := range []string{
		"permission_prompt",
		"elicitation_dialog",
		"AskUserQuestion",
		"PermissionRequest",
		"trebi agent-hook waiting",
		"trebi agent-hook stop",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("claude settings missing %q", want)
		}
	}
	if strings.Contains(s, "idle_prompt") {
		t.Error("idle_prompt must not map to waiting (turn-complete ≠ needs human)")
	}
	// Inline --settings fallback uses the same embedded payload.
	if !strings.Contains(claudeSettingsJSON, "permission_prompt") {
		t.Error("claudeSettingsJSON embed should restrict Notification matchers")
	}
	if strings.Contains(claudeSettingsJSON, "idle_prompt") {
		t.Error("claudeSettingsJSON must not wire idle_prompt")
	}
}

func TestPrepareResume(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tool   string
		base   string
		sid    string
		wantIn []string
	}{
		{Claude, "claude --session-id oldid --dangerously-skip-permissions", "new-sid", []string{"--resume", "new-sid"}},
		{Grok, "grok --session-id oldid", "gsid", []string{"--resume", "gsid"}},
		{OpenCode, "opencode --auto", "osid", []string{"--session", "osid"}},
		{Codex, "codex -C /work", "csid", []string{"codex resume -C /work -c check_for_update_on_startup=false csid"}},
		{Copilot, "copilot", "psid", []string{"--resume", "psid"}},
		{Gemini, "gemini", "msid", []string{"--resume", "msid"}},
		{Pi, "pi --session-id oldid", "psid-pi", []string{"--session", "psid-pi"}},
	}
	for _, c := range cases {
		tool, _ := ByName(c.tool)
		plan, err := tool.PrepareResume(c.base, c.sid)
		if err != nil {
			t.Errorf("%s: %v", c.tool, err)
			continue
		}
		for _, w := range c.wantIn {
			if !strings.Contains(plan.Command, w) {
				t.Errorf("%s: resume %q missing %q", c.tool, plan.Command, w)
			}
		}
		// Launch-only session-id should be stripped for claude/grok/pi.
		if c.tool == Claude || c.tool == Grok || c.tool == Pi {
			if strings.Contains(plan.Command, "oldid") {
				t.Errorf("%s: launch session id leaked into resume: %q", c.tool, plan.Command)
			}
		}
	}

	if got := CodexRolloutID("/x/rollout-2026-09-30T12-00-00-0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b.jsonl"); got != "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b" {
		t.Errorf("rollout id %q", got)
	}
	if got := CodexRolloutID("/x/transcript.jsonl"); got != "" {
		t.Errorf("rollout id of other file %q", got)
	}

	// Empty session id errors.
	for _, name := range []string{Claude, Grok, OpenCode, Codex, Copilot, Gemini, Cursor, Pi} {
		tool, _ := ByName(name)
		if _, err := tool.PrepareResume(name, ""); err == nil {
			t.Errorf("%s: expected error for empty session id", name)
		}
	}

	if _, err := (unknownTool{}).PrepareResume("x", "y"); !errors.Is(err, ErrResumeUnsupported) {
		t.Errorf("unknown resume: %v", err)
	}
}

func TestEnsureCursorUserHooks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")

	// Seed user hooks that must survive.
	seed := `{
  "version": 1,
  "hooks": {
    "stop": [
      {"command": "./my-audit.sh", "timeout": 3}
    ],
    "sessionStart": [
      {"command": "trebi agent-hook session-start", "timeout": 5}
    ]
  }
}
`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ensureCursorUserHooksAt(path, cursorHooksJSON); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got cursorHooksFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// User audit preserved.
	if len(got.Hooks["stop"]) < 2 {
		t.Fatalf("stop hooks: want user+trebi, got %s", raw)
	}
	foundUser := false
	trebiStops := 0
	for _, e := range got.Hooks["stop"] {
		var c struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(e, &c)
		if c.Command == "./my-audit.sh" {
			foundUser = true
		}
		if strings.Contains(c.Command, trebiCursorHookMarker) {
			trebiStops++
		}
	}
	if !foundUser {
		t.Fatal("user stop hook lost")
	}
	if trebiStops != 1 {
		t.Fatalf("want exactly one trebi stop hook, got %d", trebiStops)
	}
	// sessionStart should be trebi only (old trebi replaced, no duplicate).
	if n := countTrebiHooks(got.Hooks["sessionStart"]); n != 1 {
		t.Fatalf("sessionStart trebi hooks: %d", n)
	}
	if n := countTrebiHooks(got.Hooks["preToolUse"]); n != 1 {
		t.Fatalf("preToolUse missing: %d", n)
	}

	// Idempotent re-merge: still one trebi stop.
	if err := ensureCursorUserHooksAt(path, cursorHooksJSON); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw2, &got)
	if n := countTrebiHooks(got.Hooks["stop"]); n != 1 {
		t.Fatalf("after re-merge stop trebi count=%d", n)
	}

	if err := stripCursorUserHooksAt(path); err != nil {
		t.Fatal(err)
	}
	raw3, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stripped cursorHooksFile
	if err := json.Unmarshal(raw3, &stripped); err != nil {
		t.Fatal(err)
	}
	if countTrebiHooks(stripped.Hooks["stop"]) != 0 || countTrebiHooks(stripped.Hooks["sessionStart"]) != 0 {
		t.Fatalf("strip left trebi hooks: %s", raw3)
	}
	if len(stripped.Hooks["stop"]) != 1 {
		t.Fatalf("user stop should remain: %s", raw3)
	}
}

func TestCursorMaterialize_BridgesUserHooks(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	home.SetBase(t.TempDir())
	defer home.SetBase("")

	dir := config.HarnessDir(Cursor)
	if err := (cursorLifecycle{}).materialize(dir); err != nil {
		t.Fatal(err)
	}
	userHooks := filepath.Join(userHome, ".cursor", "hooks.json")
	raw, err := os.ReadFile(userHooks)
	if err != nil {
		t.Fatalf("user hooks not written: %v", err)
	}
	if !strings.Contains(string(raw), trebiCursorHookMarker) {
		t.Fatalf("missing trebi hooks: %s", raw)
	}
	// Harness copy still present.
	if _, err := os.Stat(filepath.Join(dir, "hooks.json")); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeCursorCommand(t *testing.T) {
	home.SetBase(t.TempDir())
	defer home.SetBase("")

	// Bare "cursor" must not stay as "cursor" (IDE landmine on macOS).
	got := normalizeCursorCommand("cursor --trust --resume deadbeef")
	fields := strings.Fields(got)
	if len(fields) == 0 {
		t.Fatal("empty command")
	}
	base := filepath.Base(fields[0])
	if base == Cursor {
		t.Fatalf("bare cursor not rewritten: %q", got)
	}
	// Bare cursor-agent, or absolute path whose base is cursor-agent.
	if base != CursorAgentBinary {
		t.Fatalf("want cursor-agent binary, got %q", got)
	}
	if !strings.Contains(got, "--trust") || !strings.Contains(got, "deadbeef") {
		t.Fatalf("flags lost: %q", got)
	}

	// Vendor name unchanged.
	keep := normalizeCursorCommand("cursor-agent --force")
	if !strings.HasPrefix(keep, "cursor-agent") {
		t.Fatalf("cursor-agent rewritten unexpectedly: %q", keep)
	}

	// Absolute non-IDE .../cursor (store pin) kept.
	pin := filepath.Join(t.TempDir(), "cursor")
	if err := os.WriteFile(pin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	kept := normalizeCursorCommand(pin + " --trust")
	if !strings.HasPrefix(kept, pin) {
		t.Fatalf("store pin rewritten: %q", kept)
	}
}

func TestPrepareResume_CursorRewritesBareCursor(t *testing.T) {
	home.SetBase(t.TempDir())
	defer home.SetBase("")
	t.Setenv("HOME", t.TempDir()) // materialize bridges ~/.cursor/hooks.json

	plan, err := (cursorTool{}).PrepareResume("cursor --trust --resume old-sid", "new-sid")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(plan.Command)
	if len(fields) == 0 {
		t.Fatal("empty resume command")
	}
	base := filepath.Base(fields[0])
	if base == Cursor {
		t.Fatalf("resume still uses IDE name cursor: %q", plan.Command)
	}
	if !strings.Contains(plan.Command, "--resume") || !strings.Contains(plan.Command, "new-sid") {
		t.Fatalf("resume flags: %q", plan.Command)
	}
	if strings.Contains(plan.Command, "old-sid") {
		t.Fatalf("old session id leaked: %q", plan.Command)
	}
	if plan.Env["CURSOR_CONFIG_DIR"] == "" {
		t.Fatal("expected CURSOR_CONFIG_DIR")
	}
}

func TestIsCursorIDEBinary(t *testing.T) {
	t.Parallel()
	if isCursorIDEBinary("") {
		t.Fatal("empty")
	}
	if isCursorIDEBinary("/usr/local/bin/cursor-agent") {
		t.Fatal("agent path should not be IDE")
	}
	if !isCursorIDEBinary("/Applications/Cursor.app/Contents/Resources/app/bin/code") {
		t.Fatal("Cursor.app code binary should be IDE")
	}
	// Symlink to Cursor.app (matches macOS /usr/local/bin/cursor layout).
	dir := t.TempDir()
	appBin := filepath.Join(dir, "Cursor.app", "Contents", "Resources", "app", "bin")
	if err := os.MkdirAll(appBin, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(appBin, "code")
	if err := os.WriteFile(target, []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "cursor")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if !isCursorIDEBinary(link) {
		t.Fatalf("symlink to Cursor.app not detected: %s", link)
	}
}

func TestIsReady(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tool   string
		screen string
		want   bool
	}{
		{Claude, "❯ Try \"fix typecheck errors\"\n  Fable 5 | 0.5 min | 0K (0%) | /tmp/x", true},
		{Claude, "❯ \n  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents", true},
		{Claude, "❯ only", false},
		{Claude, "Fable 5 | 0.5 min | 0K (0%) without glyph", false},
		{Claude, "❯ 1. Yes, I trust this folder\n  2. No, exit", false},
		{Codex, "› Implement {feature}\n\n  gpt-5.5 medium · /tmp/x", true},
		{Codex, "Do you trust the contents of this directory? …\n› 1. Yes, continue\n  Press enter to continue", false},
		{Codex, "loading...", false},
		{Copilot, "❯\n────\n v1.0.71 available · run /update · / commands · ? help", true},
		{Copilot, "╻▄▄▄\n┃\n╹▀▀▀\n / commands · ? help · → next tab", true},
		{Copilot, "Do you trust the files in this folder?\n❯ 1. Yes\n  2. Yes, and remember this folder", false},
		{Copilot, "Would you like to add this key binding to your terminal configuration?\n┃ ❯ 1. Yes", false},
		{Copilot, ">", false},
		{Gemini, "│ >   Type your message or @path/to/file │\n /tmp/x  no sandbox  auto", true},
		{Gemini, "│ ⠹ Waiting for auth... (Press ESC or CTRL+C to cancel) │", false},
		{Gemini, "gemini >", false},
		{OpenCode, "┃  Ask anything... \"Fix broken tests\"\n┃  Build · Grok 4.3 xAI", true},
		{OpenCode, "something > here", false},
		{OpenCode, "┃ typed text, placeholder gone", false},
		{Grok, "│ ❯ \n Grok 4.5 (high) · always-approve", true},
		{Grok, "┌─ Resume session ─┐\n│ ❯", false},
		{Grok, "idle spinner", false},
	}
	for _, c := range cases {
		tool, _ := ByName(c.tool)
		if got := tool.IsReady(c.screen); got != c.want {
			t.Errorf("%s IsReady(%q) = %v, want %v", c.tool, c.screen, got, c.want)
		}
	}
}

func TestHandleStartup_Claude(t *testing.T) {
	t.Parallel()
	tool := claudeTool{}
	trust := "Is this a project you created or one you trust?\n❯ 1. Yes, I trust this folder\n  2. No, exit"
	acts := tool.HandleStartup(trust)
	if len(acts) != 1 || acts[0].Text != "1" {
		t.Fatalf("trust actions: %+v, want Text=1", acts)
	}
	bypass := "Bypass Permissions mode\nYes, I accept"
	acts = tool.HandleStartup(bypass)
	if len(acts) != 1 || acts[0].Text != "2" {
		t.Fatalf("bypass actions: %+v", acts)
	}
	// Resume-of-large-session prompt: select "2. Resume full session as-is".
	resume := "This session is 3h 3m old and 235k tokens.\n" +
		"❯ 1. Resume from summary (recommended)\n" +
		"  2. Resume full session as-is\n" +
		"  3. Don't ask me again"
	acts = tool.HandleStartup(resume)
	if len(acts) != 1 || acts[0].Text != "2" {
		t.Fatalf("resume-summary actions: %+v", acts)
	}
	// First-run theme picker (fresh CLAUDE_CONFIG_DIR).
	theme := "Let's get started.\n\n Choose the text style that looks best with your terminal\n" +
		" To change this later, run /theme\n\n" +
		"   1. Auto (match terminal)\n ❯ 2. Dark mode ✔\n   3. Light mode\n"
	acts = tool.HandleStartup(theme)
	if len(acts) != 1 || acts[0].Key != "enter" {
		t.Fatalf("theme picker actions: %+v", acts)
	}
	if tool.IsReady(theme) {
		t.Error("IsReady = true for theme picker; want false")
	}
	if acts := tool.HandleStartup("normal screen"); len(acts) != 0 {
		t.Fatalf("unexpected actions: %+v", acts)
	}
	// Claude-in-Chrome opt-in: always select 2 (keep browser tools off).
	chrome := "Claude in Chrome extension detected\n\n" +
		"Claude will use your Chrome browser by default\n\n" +
		"  ❯ 1. Yes, use my browser\n" +
		"    2. No, keep browser tools off\n\n" +
		"  Enter to confirm · Esc to keep browser tools off\n"
	acts = tool.HandleStartup(chrome)
	if len(acts) != 1 || acts[0].Text != "2" {
		t.Fatalf("chrome dialog actions: %+v; want Text=2", acts)
	}
	if tool.IsReady(chrome) {
		t.Error("IsReady = true for chrome dialog; want false")
	}
}

func TestSeedClaudeOnboarding(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := seedClaudeOnboarding(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["hasCompletedOnboarding"] != true {
		t.Fatalf("hasCompletedOnboarding: %v", m["hasCompletedOnboarding"])
	}
	if m["claudeInChromeDefaultEnabled"] != false {
		t.Fatalf("claudeInChromeDefaultEnabled: %v", m["claudeInChromeDefaultEnabled"])
	}
	if m["hasCompletedClaudeInChromeOnboarding"] != true {
		t.Fatalf("hasCompletedClaudeInChromeOnboarding: %v", m["hasCompletedClaudeInChromeOnboarding"])
	}
	// Idempotent + re-asserts chrome-off even if file had chrome on.
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"),
		[]byte(`{"hasCompletedOnboarding":true,"claudeInChromeDefaultEnabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedClaudeOnboarding(dir); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["claudeInChromeDefaultEnabled"] != false {
		t.Fatalf("seed should force chrome default off: %v", m["claudeInChromeDefaultEnabled"])
	}
}

// TestClaudeResumeSummaryFixture asserts the real captured dialog is NOT ready
// and yields the "resume full" action, so the runner auto-answers it on resume.
func TestClaudeResumeSummaryFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "screens", "claude", "resume-summary-prompt.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	screen := string(data)
	tool := claudeTool{}
	if tool.IsReady(screen) {
		t.Error("IsReady = true for the resume-summary dialog; want false (must not proceed until answered)")
	}
	acts := tool.HandleStartup(screen)
	if len(acts) != 1 || acts[0].Text != "2" {
		t.Fatalf("resume dialog actions: %+v; want single Text=2", acts)
	}
}

func TestPostReadyDelay(t *testing.T) {
	t.Parallel()
	if (claudeTool{}).PostReadyDelay() != 3*time.Second {
		t.Error("claude delay")
	}
	if (codexTool{}).PostReadyDelay() != 1*time.Second {
		t.Error("codex delay")
	}
	if (unknownTool{}).PostReadyDelay() != 3*time.Second {
		t.Error("unknown delay")
	}
}

func TestTranscriptWorkingDirPassesClaudeCwd(t *testing.T) {
	t.Parallel()
	wd := "/Users/fritz/trebi/ocral/agents/watchtower/fl-housing-tracker"
	if got := TranscriptWorkingDir(Claude, wd, time.Time{}); got != wd {
		t.Fatalf("claude cwd=%q want %q", got, wd)
	}
}

func TestClaudeTranscriptPathFindsProjectSlug(t *testing.T) {
	root := t.TempDir()
	home.SetBase(root)
	t.Cleanup(func() { home.SetBase("") })
	wd := "/Users/me/proj"
	sid := "sess-slug"
	slug := ClaudeProjectDir(wd)
	p := filepath.Join(config.HarnessDir(Claude), "projects", slug, sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := (claudeTool{}).TranscriptPath("", wd, sid)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("got %s want %s", got, p)
	}
	// Empty cwd still finds the file under projects/.
	got, err = (claudeTool{}).TranscriptPath("", "", sid)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("walk got %s want %s", got, p)
	}
}

func TestTranscriptPath(t *testing.T) {
	userHome := "/home/u"
	wd := "/Users/me/proj"
	sid := "sess-1"
	wantSlug := ClaudeProjectDir(wd)

	// No harness file and no env → default to harness path (strategy B).
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })

	path, err := claudeTool{}.TranscriptPath(userHome, wd, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, filepath.Join("harness", "claude", "projects", wantSlug, sid+".jsonl")) {
		t.Errorf("claude path (harness default): %s", path)
	}

	// CLAUDE_CONFIG_DIR env wins.
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/isolated-claude")
	path, err = claudeTool{}.TranscriptPath(userHome, wd, sid)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join("/tmp/isolated-claude", "projects", wantSlug, sid+".jsonl") {
		t.Errorf("claude path (env): %s", path)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	path, err = grokTool{}.TranscriptPath(userHome, wd, sid)
	if err != nil {
		t.Fatal(err)
	}
	wantGrok := filepath.Join(".grok", "sessions", url.PathEscape(wd), sid, "chat_history.jsonl")
	if !strings.Contains(path, wantGrok) {
		t.Errorf("grok path: %s want %s", path, wantGrok)
	}

	harnessHome := filepath.Join(t.TempDir(), "harness", Grok)
	path, err = grokTool{}.TranscriptPath(harnessHome, wd, sid)
	if err != nil {
		t.Fatal(err)
	}
	if path != grokChatHistoryPath(harnessHome, wd, sid) {
		t.Errorf("grok harness path: %s", path)
	}

	for _, name := range []string{Copilot, Gemini, OpenCode} {
		tool, _ := ByName(name)
		_, err := tool.TranscriptPath(userHome, wd, sid)
		if !errors.Is(err, ErrTranscriptUnsupported) {
			t.Errorf("%s: want ErrTranscriptUnsupported, got %v", name, err)
		}
	}

	// Codex reads rollouts, but this store holds none for that session.
	if _, err := (codexTool{}).TranscriptPath(userHome, wd, sid); err == nil {
		t.Error("codex: want an error for a session with no rollout")
	}

	if _, err := (claudeTool{}).TranscriptPath(userHome, wd, ""); err == nil {
		t.Error("empty session should error")
	}
}

func TestGrokTranscriptPathFindsChatHistory(t *testing.T) {
	root := t.TempDir()
	sid := "sess-find"
	wd := "/tmp/ops/nightly"
	want := grokChatHistoryPath(root, wd, sid)
	if err := os.MkdirAll(filepath.Dir(want), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", root)
	got, err := grokTool{}.TranscriptPath("", "", sid)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestClaudeProjectDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"/Users/fritz/.trebi/workspace/", "-Users-fritz--trebi-workspace"},
		{"/Users/fritz/.trebi/workspace", "-Users-fritz--trebi-workspace"},
		{"/tmp/work///", "-tmp-work"},
		{"/home/.config", "-home--config"},
	}
	for _, c := range cases {
		if got := ClaudeProjectDir(c.in); got != c.want {
			t.Errorf("ClaudeProjectDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLifecycle_InstallUninstall(t *testing.T) {
	// trebiBase simulates ~/.trebi; userHome is a separate simulated $HOME.
	trebiBase := t.TempDir()
	userHome := t.TempDir()
	binDir := filepath.Join(trebiBase, "bin")
	home.SetBase(trebiBase)
	t.Cleanup(func() { home.SetBase("") })

	// Legacy PATH shim file must be removed so bin/claude/ can hold versions later.
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	legacyShim := filepath.Join(binDir, "claude")
	if err := os.WriteFile(legacyShim, []byte("#!/bin/sh\n# legacy shim\n"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := installLifecycleAssets(trebiBase, binDir); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// No PATH shim; settings under harness/claude.
	if fi, err := os.Lstat(legacyShim); err == nil && !fi.IsDir() {
		t.Fatal("legacy claude shim file should be removed")
	}
	settingsPath := filepath.Join(trebiBase, "harness", "claude", "trebi-settings.json")
	sdata, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("claude settings: %v", err)
	}
	if !strings.Contains(string(sdata), "trebi agent-hook") {
		t.Error("claude settings should reference agent-hook")
	}
	// Claude waiting must be matcher-restricted (not blanket Notification).
	// idle_prompt after Stop would otherwise stick long-lived agents on waiting.
	if !strings.Contains(string(sdata), "permission_prompt") {
		t.Error("claude settings should matcher-filter Notification to permission_prompt")
	}
	if !strings.Contains(string(sdata), "AskUserQuestion") {
		t.Error("claude settings should map AskUserQuestion PreToolUse to waiting")
	}
	if strings.Contains(string(sdata), "idle_prompt") {
		t.Error("claude settings must not wire idle_prompt → waiting")
	}
	// Blanket Notification→waiting without a matcher would spam idle_prompt.
	// Require every waiting hook to sit under a named human-attention matcher
	// or PermissionRequest / AskUserQuestion (checked above).
	if !strings.Contains(string(sdata), "PermissionRequest") {
		t.Error("claude settings should map PermissionRequest to waiting")
	}

	// Grok hooks only under trebi harness — never user home.
	grokHarness := filepath.Join(trebiBase, "harness", "grok", "hooks", "trebi.json")
	if _, err := os.Stat(grokHarness); err != nil {
		t.Fatalf("grok harness hooks missing: %v", err)
	}
	gdata, _ := os.ReadFile(grokHarness)
	if !strings.Contains(string(gdata), "permission_prompt") {
		t.Error("grok hooks should use permission_prompt matcher")
	}
	userGrok := filepath.Join(userHome, ".grok", "hooks", "trebi.json")
	if _, err := os.Stat(userGrok); !os.IsNotExist(err) {
		t.Error("must not write grok hooks under user home")
	}

	// OpenCode plugin under harness only
	ocPath := filepath.Join(trebiBase, "harness", "opencode", "plugins", "trebi-lifecycle.js")
	odata, err := os.ReadFile(ocPath)
	if err != nil {
		t.Fatalf("opencode plugin: %v", err)
	}
	if !strings.Contains(string(odata), "permission.asked") {
		t.Error("opencode plugin should map permission.asked")
	}
	if !strings.Contains(string(odata), "TREBI_RUN_ID") {
		t.Error("opencode plugin should check TREBI_RUN_ID")
	}
	userOC := filepath.Join(userHome, ".config", "opencode", "plugins", "trebi-lifecycle.js")
	if _, err := os.Stat(userOC); !os.IsNotExist(err) {
		t.Error("must not write opencode plugin under user config")
	}

	// Pi lifecycle extension under harness only
	piExt := filepath.Join(trebiBase, "harness", "pi", "extensions", "trebi-lifecycle.ts")
	pdata, err := os.ReadFile(piExt)
	if err != nil {
		t.Fatalf("pi extension: %v", err)
	}
	if !strings.Contains(string(pdata), "TREBI_RUN_ID") {
		t.Error("pi extension should check TREBI_RUN_ID")
	}
	if !strings.Contains(string(pdata), "agent_settled") {
		t.Error("pi extension should map agent_settled → stop")
	}
	if !strings.Contains(string(pdata), "trebi agent-hook") {
		t.Error("pi extension should call trebi agent-hook")
	}

	// Codex notify helper under libexec (not bin/)
	notify := filepath.Join(trebiBase, "libexec", "trebi-codex-notify")
	if st, err := os.Stat(notify); err != nil || st.Mode()&0111 == 0 {
		t.Fatalf("codex notify helper: %v", err)
	}

	// Lifecycle() presence
	if (claudeTool{}).Lifecycle() == nil || (grokTool{}).Lifecycle() == nil ||
		(opencodeTool{}).Lifecycle() == nil || (piTool{}).Lifecycle() == nil {
		t.Error("expected lifecycle adapters")
	}
	if (codexTool{}).Lifecycle() == nil {
		t.Error("codex should have notify lifecycle")
	}

	// PrepareLaunch injects settings + CLAUDE_CONFIG_DIR
	plan := claudeTool{}.PrepareLaunch("claude --dangerously-skip-permissions")
	if !strings.Contains(plan.Command, "--settings") {
		t.Errorf("expected --settings in launch: %s", plan.Command)
	}
	if plan.Env["CLAUDE_CONFIG_DIR"] == "" {
		t.Error("expected CLAUDE_CONFIG_DIR in launch env")
	}

	if err := uninstallLifecycleAssets(trebiBase, binDir); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Error("claude settings should be removed on uninstall")
	}
	if _, err := os.Stat(ocPath); !os.IsNotExist(err) {
		t.Error("opencode plugin should be removed")
	}
	_ = userHome
}

func TestStripFlag(t *testing.T) {
	t.Parallel()
	got := StripFlag("claude --session-id abc --foo", "--session-id")
	if got != "claude --foo" {
		t.Errorf("got %q", got)
	}
	got = StripFlag("grok --session-id=xyz -c", "--session-id")
	if strings.Contains(got, "xyz") {
		t.Errorf("flag=value not stripped: %q", got)
	}
}

func TestAppendFlags(t *testing.T) {
	t.Parallel()
	if AppendFlags("claude", nil) != "claude" {
		t.Error("nil flags")
	}
	if AppendFlags("claude", []string{"--a", "b"}) != "claude --a b" {
		t.Error("append")
	}
}

func TestAllTools_UniqueNames(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, tool := range AllTools() {
		if seen[tool.Name()] {
			t.Errorf("duplicate tool %s", tool.Name())
		}
		seen[tool.Name()] = true
	}
	for _, want := range []string{Claude, Codex, Copilot, Gemini, OpenCode, Grok, Pi} {
		if !seen[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}

func TestFirstClassToolsMatchesAllToolsFilter(t *testing.T) {
	t.Parallel()
	want := map[string]bool{}
	for _, n := range config.FirstClassTools() {
		want[n] = true
	}
	for _, tool := range AllTools() {
		n := tool.Name()
		first := n != Gemini && n != Fake
		if want[n] != first {
			t.Errorf("%s: FirstClassTools=%v filter=%v", n, want[n], first)
		}
		delete(want, n)
	}
	for n := range want {
		t.Errorf("FirstClassTools extra %s not in AllTools", n)
	}
}

func countTrebiHooks(entries []json.RawMessage) int {
	n := 0
	for _, e := range entries {
		if isTrebiCursorHookEntry(e) {
			n++
		}
	}
	return n
}
