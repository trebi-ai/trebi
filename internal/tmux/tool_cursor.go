package tmux

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

// CursorAgentBinary is the primary PATH name for the Cursor Agent CLI.
// Trebi tool identity is Cursor ("cursor") — defined in tool.go.
const CursorAgentBinary = "cursor-agent"

type cursorTool struct{ harnessHome }

func (cursorTool) Name() string { return Cursor }

func (cursorTool) Match(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	base := fields[0]
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".exe")
	// Match cursor-agent (vendor) and bare "cursor" (trebi store / pin name).
	// Do not match bare "agent" — too ambiguous with other tools.
	return base == CursorAgentBinary || base == Cursor
}

func (t cursorTool) PrepareLaunch(command string) LaunchPlan {
	cmd := injectCursorFlags(normalizeCursorCommand(command))
	sid := createCursorChat()
	if sid != "" {
		cmd = StripFlags(cmd, "--resume", "--continue")
		cmd = AppendFlags(cmd, []string{"--resume", sid})
	}
	return planWithSession(cmd, sid, t.launchEnv())
}

func (t cursorTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	if sessionID == "" {
		return LaunchPlan{}, fmt.Errorf("no session ID available for cursor resume")
	}
	base := StripFlags(normalizeCursorCommand(command), "--resume", "--continue")
	base = injectCursorFlags(base)
	cmd := AppendFlags(base, []string{"--resume", sessionID})
	return LaunchPlan{Command: cmd, SessionID: sessionID, Env: t.launchEnv()}, nil
}

// normalizeCursorCommand rewrites trebi/tool-id "cursor" to the vendor agent CLI.
// macOS Cursor.app also installs /usr/local/bin/cursor (VS Code `code` shim); using
// that for resume/launch opens the editor, not the agent. Prefer an absolute
// agent path from findCursorBinary; else bare "cursor-agent".
func normalizeCursorCommand(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return command
	}
	base := fields[0]
	name := base
	if i := strings.LastIndex(base, "/"); i >= 0 {
		name = base[i+1:]
	}
	name = strings.TrimSuffix(name, ".exe")
	if name != Cursor {
		return command
	}
	// Absolute store pin named "cursor" (trebi copies the agent there) — keep.
	if strings.Contains(base, string(os.PathSeparator)) && !isCursorIDEBinary(base) {
		return command
	}
	if bin := findCursorBinary(); bin != "" {
		fields[0] = bin
		return strings.Join(fields, " ")
	}
	fields[0] = CursorAgentBinary
	return strings.Join(fields, " ")
}

// injectCursorFlags adds --trust when missing so unattended launches skip the
// workspace-trust prompt. Approval policy lives in harness cli-config.json.
func injectCursorFlags(command string) string {
	if !strings.Contains(command, "--trust") {
		command = AppendFlags(command, []string{"--trust"})
	}
	return command
}

// createCursorChatTimeout bounds create-chat; the vendor CLI has been observed
// to hang when CURSOR_CONFIG_DIR is a fresh harness tree.
const createCursorChatTimeout = 8 * time.Second

// createCursorChat runs `cursor-agent create-chat` for a known-upfront session id.
// Best-effort with a hard timeout: empty string on any failure (sessionStart /
// conversation_id hooks still deliver the id for resume).
//
// Note: create-chat hangs under a fresh CURSOR_CONFIG_DIR, so this deliberately
// runs against the default profile; the chat id is server-side and --resume
// still works when the agent later launches with harness isolation.
func createCursorChat() string {
	bin := findCursorBinary()
	if bin == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), createCursorChatTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "create-chat")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(out))
	// create-chat prints a bare UUID.
	if len(id) < 8 || strings.ContainsAny(id, " \n\t") {
		// Take first line if multi-line noise.
		if i := strings.IndexAny(id, "\n\r"); i >= 0 {
			id = strings.TrimSpace(id[:i])
		}
	}
	if len(id) < 8 {
		return ""
	}
	return id
}

// findCursorBinary returns an absolute path to the Cursor Agent CLI (not the IDE).
func findCursorBinary() string {
	if p, err := exec.LookPath(CursorAgentBinary); err == nil {
		return p
	}
	// Store pin path (trebi ensure); prefer cursor-agent name, then store "cursor" copy.
	current := config.ToolCurrentLink(Cursor)
	for _, c := range []string{
		filepath.Join(current, "bin", CursorAgentBinary),
		filepath.Join(current, "bin", Cursor),
		filepath.Join(current, CursorAgentBinary),
		filepath.Join(current, Cursor),
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			if abs, err := filepath.Abs(c); err == nil {
				return abs
			}
			return c
		}
	}
	// Last resort: PATH "cursor" only when it is not Cursor.app's editor shim.
	if p, err := exec.LookPath(Cursor); err == nil && !isCursorIDEBinary(p) {
		return p
	}
	return ""
}

// isCursorIDEBinary reports whether path is Cursor.app's editor CLI (code shim),
// which must never be used as the agent binary.
func isCursorIDEBinary(path string) bool {
	if path == "" {
		return false
	}
	resolved := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		resolved = r
	}
	if strings.Contains(resolved, "Cursor.app") {
		return true
	}
	// Unresolved or non-app layouts still look like the VS Code `code` binary.
	base := filepath.Base(resolved)
	if base == "code" || base == "code.cmd" {
		return true
	}
	return false
}

func (cursorTool) IsReady(screen string) bool {
	if IsAuthBlocker(screen) || IsLimitBlocker(screen) {
		return false
	}
	// Idle composer placeholders (goldens: testdata/screens/cursor/startup).
	// Fresh session: "→ Plan, search, build anything"
	// After a turn: "→ Add a follow-up"
	if strings.Contains(screen, "→ Plan, search, build anything") ||
		strings.Contains(screen, "→ Add a follow-up") {
		return true
	}
	// Typed-into but not yet submitted, or minor placeholder variants: header +
	// input arrow + model footer.
	if strings.Contains(screen, "Cursor Agent") &&
		strings.Contains(screen, "→ ") &&
		(strings.Contains(screen, "Run Everything") || strings.Contains(screen, "Auto")) {
		return true
	}
	return false
}

func (cursorTool) HandleStartup(screen string) []Action {
	// --trust usually skips this; keep Enter fallback for the real dialog chrome.
	// Avoid matching "--trust" in the launch command line still on-screen.
	flat := strings.ToLower(screen)
	if strings.Contains(flat, "do you trust") ||
		strings.Contains(flat, "trust this workspace") ||
		strings.Contains(flat, "trust this folder") {
		return []Action{{Key: "enter"}}
	}
	return nil
}

func (cursorTool) PostReadyDelay() time.Duration { return defaultPostReadyDelay }

func (cursorTool) ParseTranscript(r io.Reader) ([]Turn, error) { return unsupportedParse(r) }

func (cursorTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("no session id recorded")
	}
	// Prefer harness agent-transcripts: projects/<slug>/agent-transcripts/<sid>/<sid>.jsonl
	// Usage tokens are not embedded (interactive CLI); model comes from chat store.
	for _, base := range []string{
		os.Getenv("CURSOR_DATA_DIR"),
		os.Getenv("CURSOR_CONFIG_DIR"),
		config.HarnessDir(Cursor),
		filepath.Join(home, ".cursor"),
	} {
		if base == "" {
			continue
		}
		projects := filepath.Join(base, "projects")
		entries, err := os.ReadDir(projects)
		if err != nil {
			continue
		}
		target := sessionID + ".jsonl"
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(projects, e.Name(), "agent-transcripts", sessionID, target)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
	}
	return "", ErrTranscriptUnsupported
}

func (cursorTool) Lifecycle() LifecycleAdapter { return cursorLifecycle{} }

// ClassifyScreen: busy when a turn is streaming; idle when the composer shows.
func (cursorTool) ClassifyScreen(screen string) ScreenClass {
	return classifyChrome(screen,
		[]string{"ctrl+c to stop", "Working", "Generating", "Thinking", "Esc to cancel"},
		[]string{"→ Plan, search, build anything", "→ Add a follow-up"},
	)
}

func (t cursorTool) launchEnv() map[string]string {
	t.tool = Cursor
	_, dir := t.prepare()
	return map[string]string{
		// Relocates cli-config.json + chats/projects under harness.
		// Hooks are NOT loaded from CURSOR_CONFIG_DIR — see ensureCursorUserHooks.
		"CURSOR_CONFIG_DIR": dir,
		// Keep data (projects/chats) with config so isolation is complete.
		"CURSOR_DATA_DIR": dir,
	}
}

//go:embed lifecycle/cursor-trebi-hooks.json
var cursorHooksJSON []byte

// Minimal trebi-seeded cli-config for isolated CURSOR_CONFIG_DIR (unattended-friendly).
const cursorCLIConfigJSON = `{
  "version": 1,
  "editor": { "vimMode": false },
  "permissions": {
    "allow": [
      "Shell(*)",
      "Read(**)",
      "Write(**)",
      "WebFetch(*)",
      "Mcp(*:*)"
    ],
    "deny": []
  },
  "approvalMode": "unrestricted",
  "sandbox": {
    "mode": "disabled"
  },
  "notifications": false,
  "hints": false
}
`

type cursorLifecycle struct{}

func (c cursorLifecycle) Install(trebiBase, binDir string) error {
	dir := filepath.Join(trebiBase, "harness", Cursor)
	return c.materialize(dir)
}

func (cursorLifecycle) Uninstall(trebiBase, binDir string) error {
	dir := filepath.Join(trebiBase, "harness", Cursor)
	for _, name := range []string{"hooks.json"} {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing cursor %s: %w", name, err)
		}
	}
	if err := stripCursorUserHooks(); err != nil {
		return fmt.Errorf("removing cursor user hooks: %w", err)
	}
	return nil
}

func (cursorLifecycle) materialize(dir string) error {
	if err := harness.EnsureDir(dir); err != nil {
		return err
	}
	// Canonical copy under harness (docs / future if Cursor honors config-dir hooks).
	if err := harness.WriteFileIfChanged(
		filepath.Join(dir, "hooks.json"),
		cursorHooksJSON,
		0644,
	); err != nil {
		return fmt.Errorf("writing cursor hooks: %w", err)
	}
	// Cursor Agent CLI only loads ~/.cursor/hooks.json (and project .cursor/hooks.json).
	// CURSOR_CONFIG_DIR does not relocate hooks — merge trebi entries into the user file.
	if err := ensureCursorUserHooks(cursorHooksJSON); err != nil {
		return fmt.Errorf("bridging cursor user hooks: %w", err)
	}
	// Seed cli-config only when missing so we don't clobber bridged/user-tuned prefs.
	cfgPath := filepath.Join(dir, "cli-config.json")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := harness.WriteFileIfChanged(cfgPath, []byte(cursorCLIConfigJSON), 0644); err != nil {
			return err
		}
	}
	return harness.WriteSchemaStamp(dir)
}

// trebiCursorHookMarker identifies trebi-owned entries inside ~/.cursor/hooks.json.
const trebiCursorHookMarker = "trebi agent-hook"

type cursorHooksFile struct {
	Version int                          `json:"version"`
	Hooks   map[string][]json.RawMessage `json:"hooks"`
}

// parseCursorHooks decodes a hooks payload and fills defaults.
func parseCursorHooks(raw []byte) (cursorHooksFile, error) {
	var f cursorHooksFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("parse trebi hooks: %w", err)
	}
	f.fillDefaults()
	return f, nil
}

// loadCursorHooksFile reads the user hooks file. A corrupt file is renamed to
// .bad and an empty file state is returned, so trebi hooks start fresh.
func loadCursorHooksFile(path string) (cursorHooksFile, error) {
	fresh := cursorHooksFile{Version: 1, Hooks: map[string][]json.RawMessage{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fresh, nil
		}
		return fresh, err
	}
	if len(raw) == 0 {
		return fresh, nil
	}
	var f cursorHooksFile
	if json.Unmarshal(raw, &f) != nil {
		_ = os.Rename(path, path+".bad")
		return fresh, nil
	}
	f.fillDefaults()
	return f, nil
}

func (f *cursorHooksFile) fillDefaults() {
	if f.Version == 0 {
		f.Version = 1
	}
	if f.Hooks == nil {
		f.Hooks = map[string][]json.RawMessage{}
	}
}

// hooksWithoutTrebi returns the hooks minus trebi-marked entries; dropped
// reports whether any entry was removed.
func (f *cursorHooksFile) hooksWithoutTrebi() (kept map[string][]json.RawMessage, dropped bool) {
	kept = map[string][]json.RawMessage{}
	for event, entries := range f.Hooks {
		out := make([]json.RawMessage, 0, len(entries))
		for _, e := range entries {
			if isTrebiCursorHookEntry(e) {
				dropped = true
				continue
			}
			out = append(out, e)
		}
		if len(out) > 0 {
			kept[event] = out
		}
	}
	return kept, dropped
}

func (f *cursorHooksFile) writeTo(path string) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return harness.WriteFileIfChanged(path, b, 0644)
}

// ensureCursorUserHooks merges trebi lifecycle hooks into ~/.cursor/hooks.json.
// Existing non-trebi entries are preserved; prior trebi agent-hook entries are replaced.
func ensureCursorUserHooks(trebiJSON []byte) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return fmt.Errorf("resolving home: %w", err)
	}
	return ensureCursorUserHooksAt(filepath.Join(home, ".cursor", "hooks.json"), trebiJSON)
}

func ensureCursorUserHooksAt(path string, trebiJSON []byte) error {
	desired, err := parseCursorHooks(trebiJSON)
	if err != nil {
		return err
	}
	existing, err := loadCursorHooksFile(path)
	if err != nil {
		return err
	}
	// Keep user entries; drop prior trebi markers.
	kept, _ := existing.hooksWithoutTrebi()
	out := cursorHooksFile{Version: existing.Version, Hooks: kept}
	// Append current trebi hooks.
	for event, entries := range desired.Hooks {
		out.Hooks[event] = append(out.Hooks[event], entries...)
	}
	return out.writeTo(path)
}

func stripCursorUserHooks() error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return stripCursorUserHooksAt(filepath.Join(home, ".cursor", "hooks.json"))
}

func stripCursorUserHooksAt(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var existing cursorHooksFile
	if err := json.Unmarshal(raw, &existing); err != nil {
		return nil // leave corrupt file alone on uninstall
	}
	if existing.Hooks == nil {
		return nil
	}
	kept, changed := existing.hooksWithoutTrebi()
	if !changed {
		return nil
	}
	if len(kept) == 0 {
		// File was trebi-only — remove rather than leave an empty husk.
		return os.Remove(path)
	}
	out := cursorHooksFile{Version: existing.Version, Hooks: kept}
	if out.Version == 0 {
		out.Version = 1
	}
	return out.writeTo(path)
}

func isTrebiCursorHookEntry(raw json.RawMessage) bool {
	var e struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return false
	}
	return strings.Contains(e.Command, trebiCursorHookMarker)
}
