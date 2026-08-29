package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

// Supported harness names (binary / Detect identity).
const (
	Claude   = "claude"
	Codex    = "codex"
	Copilot  = "copilot"
	Cursor   = "cursor" // CLI binary is typically "cursor-agent"
	Gemini   = "gemini"
	OpenCode = "opencode"
	Grok     = "grok"
	Pi       = "pi"
)

// ErrTranscriptUnsupported: harness has no known on-disk transcript layout.
var ErrTranscriptUnsupported = errors.New("transcript not supported for this tool")

// ErrResumeUnsupported: tool cannot build a resume command.
var ErrResumeUnsupported = errors.New("session resume not supported for this tool")

type Action struct {
	Key  string // e.g. "enter"
	Text string // literal keys via Send
}

// LaunchPlan is rewritten argv, optional known session id, and pane-only isolation env.
type LaunchPlan struct {
	Command   string
	SessionID string            // known upfront, or ""
	Env       map[string]string // GROK_HOME, OPENCODE_CONFIG, …
}

// LifecycleAdapter installs hook/shim/plugin assets for AgentBus (via agent-hook).
// Writes only under trebiBase; hooks no-op when TREBI_RUN_ID is unset.
// agent-hook always prints "{}" (Claude requires JSON hook stdout; others discard it).
type LifecycleAdapter interface {
	Install(trebiBase, binDir string) error
	Uninstall(trebiBase, binDir string) error
}

// Tool is harness policy: launch/resume, screen protocol, transcripts, optional hooks.
// Session ids never come from the screen: claude/grok via launch flag, opencode via hooks;
// codex/copilot/gemini keep them on disk only — PrepareResume fails loudly for those.
type Tool interface {
	Name() string
	Match(command string) bool
	PrepareLaunch(command string) LaunchPlan
	PrepareResume(command, sessionID string) (LaunchPlan, error)
	IsReady(screen string) bool
	// HandleStartup returns first-run dialog actions (trust, bypass); applied once per WaitReady loop.
	HandleStartup(screen string) []Action
	PostReadyDelay() time.Duration
	TranscriptPath(home, workingDir, sessionID string) (string, error)
	// ParseTranscript maps a harness session file into the §3.6 Turn shape.
	ParseTranscript(r io.Reader) ([]Turn, error)
	Lifecycle() LifecycleAdapter // nil if no hooks
	ProbeAuth(ctx context.Context) harness.AuthStatus
	AuthCapabilities() harness.AuthCaps
}

// Turn is one §3.6 transcript turn.
type Turn struct {
	Role  string     `json:"role"` // user|assistant|tools
	At    time.Time  `json:"at"`
	Text  string     `json:"text,omitempty"`
	Tools []ToolCall `json:"tools,omitempty"`
}

// ToolCall is one tool invocation on a turn.
type ToolCall struct {
	Name       string `json:"name"`
	Input      string `json:"input"`
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	ExitCode   *int   `json:"exit_code,omitempty"`
}

// EventModeCapability describes how a tool reports agent lifecycle to jobi.
type EventModeCapability string

const (
	EventModeFull       EventModeCapability = "event"       // full hook promotion
	EventModeSoft       EventModeCapability = "soft-event"  // soft idle without promotion
	EventModeScreenOnly EventModeCapability = "screen-only" // no hooks
	EventModeDeprecated EventModeCapability = "deprecated"  // sunset (Gemini)
)

// EventMode reports the tool's lifecycle capability (for doctor / tool list).
func EventMode(name string) EventModeCapability {
	switch name {
	case Claude, Grok, OpenCode, Cursor, Pi:
		return EventModeFull
	case Codex:
		return EventModeSoft
	case Gemini:
		return EventModeDeprecated
	default:
		return EventModeScreenOnly
	}
}

// AllTools returns registered harnesses in Detect priority order.
// Claude is checked first so paths containing "claude" win over weaker matches.
func AllTools() []Tool {
	return []Tool{
		claudeTool{},
		grokTool{},
		opencodeTool{},
		cursorTool{},
		codexTool{},
		copilotTool{},
		geminiTool{},
		piTool{},
	}
}

// Detect returns the Tool for command, or UnknownTool if unrecognized.
// Never returns nil.
func Detect(command string) Tool {
	if command == "" {
		return unknownTool{}
	}
	// Prefer full-command match for tools that appear in flags paths (claude).
	for _, t := range AllTools() {
		if t.Match(command) {
			return t
		}
	}
	return unknownTool{}
}

func ByName(name string) (Tool, bool) {
	for _, t := range AllTools() {
		if t.Name() == name {
			return t, true
		}
	}
	if name == "" || name == "unknown" {
		return unknownTool{}, true
	}
	return nil, false
}

// DetectTool returns the tool name for command, or "" if unknown.
// Kept for callers that only need the string identity.
func DetectTool(command string) string {
	t := Detect(command)
	if _, ok := t.(unknownTool); ok {
		return ""
	}
	return t.Name()
}

// InstallLifecycleAssets installs shim/hook/plugin assets for every tool that
// provides a LifecycleAdapter. trebiBase is typically config.BaseDir() (~/.trebi).
func InstallLifecycleAssets(trebiBase, binDir string) error {
	for _, t := range AllTools() {
		if lc := t.Lifecycle(); lc != nil {
			if err := lc.Install(trebiBase, binDir); err != nil {
				return fmt.Errorf("installing lifecycle for %s: %w", t.Name(), err)
			}
		}
	}
	// Re-assert resource bridges (skills/MCP/…) after materialize.
	var cfg *config.Config
	if c, err := config.Load(); err == nil {
		cfg = c
	}
	for _, name := range []string{Claude, Grok, Codex, OpenCode, Cursor, Pi} {
		_ = harness.EnsureBridges(name, cfg)
	}
	return nil
}

// UninstallLifecycleAssets removes lifecycle assets for every tool (best-effort).
func UninstallLifecycleAssets(trebiBase, binDir string) error {
	var errs []string
	for _, t := range AllTools() {
		if lc := t.Lifecycle(); lc != nil {
			if err := lc.Uninstall(trebiBase, binDir); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", t.Name(), err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("uninstall lifecycle: %s", strings.Join(errs, "; "))
	}
	return nil
}

// AppendFlags appends extra flags to a command string.
func AppendFlags(command string, flags []string) string {
	if len(flags) == 0 {
		return command
	}
	return command + " " + strings.Join(flags, " ")
}

// StripFlag removes a flag and its following value from a command string.
func StripFlag(command, flag string) string {
	fields := strings.Fields(command)
	var result []string
	for i := 0; i < len(fields); i++ {
		if fields[i] == flag {
			i++ // skip value
			continue
		}
		// Also strip flag=value form.
		if strings.HasPrefix(fields[i], flag+"=") {
			continue
		}
		result = append(result, fields[i])
	}
	return strings.Join(result, " ")
}

// StripFlags removes multiple flag+value pairs.
func StripFlags(command string, flags ...string) string {
	for _, f := range flags {
		command = StripFlag(command, f)
	}
	return command
}

const defaultPostReadyDelay = 1 * time.Second

// claudePostReadyDelay: Claude re-renders input after ❯ and drops early pastes.
const claudePostReadyDelay = 3 * time.Second

// unknownTool is the fallback when Detect cannot identify a harness.
type unknownTool struct{}

func (unknownTool) Name() string              { return "unknown" }
func (unknownTool) Match(command string) bool { return false }
func (unknownTool) PrepareLaunch(command string) LaunchPlan {
	return LaunchPlan{Command: command}
}
func (unknownTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	return LaunchPlan{}, ErrResumeUnsupported
}
func (unknownTool) IsReady(screen string) bool           { return true } // WaitReady uses special-case sleep
func (unknownTool) HandleStartup(screen string) []Action { return nil }
func (unknownTool) PostReadyDelay() time.Duration        { return 3 * time.Second }
func (unknownTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	return "", ErrTranscriptUnsupported
}
func (unknownTool) ParseTranscript(io.Reader) ([]Turn, error) {
	return nil, ErrTranscriptUnsupported
}
func (unknownTool) Lifecycle() LifecycleAdapter { return nil }
func (unknownTool) ProbeAuth(context.Context) harness.AuthStatus {
	return harness.AuthStatus{Tool: "unknown", Status: harness.AuthUnknown}
}
func (unknownTool) AuthCapabilities() harness.AuthCaps { return harness.AuthCaps{} }

// IsUnknown reports whether t is the unknown fallback.
func IsUnknown(t Tool) bool {
	_, ok := t.(unknownTool)
	return ok
}

// binaryMatch reports whether the first field of command contains name
// (path basenames like /usr/bin/claude, or "copilot-cli").
func binaryMatch(command, name string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	first := fields[0]
	// Basename-ish: last path segment
	base := first
	if i := strings.LastIndex(first, "/"); i >= 0 {
		base = first[i+1:]
	}
	return base == name || strings.Contains(base, name)
}

func containsToken(command, name string) bool {
	return strings.Contains(command, name)
}

// injectSessionIDFlag appends --session-id <uuid> (or custom flag) and returns the id.
func injectSessionIDFlag(command, flag string) (cmd, sessionID string) {
	id := uuid.New().String()
	return AppendFlags(command, []string{flag, id}), id
}

func resumeWithFlag(command, sessionID string, launchFlags []string, resumeFlag string, toolName string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("no session ID available for %s resume", toolName)
	}
	base := StripFlags(command, launchFlags...)
	return AppendFlags(base, []string{resumeFlag, sessionID}), nil
}

// planWithSession builds a LaunchPlan with command + session id.
func planWithSession(command, sessionID string, env map[string]string) LaunchPlan {
	return LaunchPlan{Command: command, SessionID: sessionID, Env: env}
}
