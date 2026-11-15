package agentcli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	agentwire "github.com/trebi-ai/agent-wire"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

// Supported harness names (binary / Detect identity).
const (
	Claude    = "claude"
	Codex     = "codex"
	Copilot   = "copilot"
	Cursor    = "cursor" // CLI binary is typically "cursor-agent"
	Gemini    = "gemini"
	OpenCode  = "opencode"
	OpenCode2 = "opencode2"
	Grok      = "grok"
	OMP       = "omp"
	Pi        = "pi"
	// Fake is the eval stand-in harness (cmd/fake). It is off the default
	// harness list, so only a job or login that names it reaches it.
	Fake = "fake"
)

// ErrTranscriptUnsupported: harness has no known on-disk transcript layout.
var ErrTranscriptUnsupported = errors.New("transcript not supported for this tool")

// TranscriptWorkingDir is the working-dir argument for Tool.TranscriptPath.
// Codex finds rollouts by cwd + start time. Everyone else passes the run cwd
// through (Claude slugs it into projects/<slug>/<session>.jsonl).
func TranscriptWorkingDir(harness, workingDir string, startedAt time.Time) string {
	if harness == Codex {
		return CodexWorkingDir(workingDir, startedAt)
	}
	return workingDir
}

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

// MCPInjector is implemented by tools whose pane launch can take extra MCP
// servers. A tool without it cannot serve a connector action (decision 8).
type MCPInjector interface {
	InjectMCP(plan *LaunchPlan, servers []agentwire.MCPServer, files RunFiles) error
}

// RunFiles names the private files of one run. They live outside the job
// directory, and the runner removes them at run end.
type RunFiles struct {
	Dir   string // home.RunsLogDir()
	RunID string
}

// Path returns Dir/<RunID>-<suffix>.
func (f RunFiles) Path(suffix string) string {
	return filepath.Join(f.Dir, f.RunID+"-"+suffix)
}

// Remove deletes every private file of the run.
func (f RunFiles) Remove() error {
	if f.Dir == "" || f.RunID == "" {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(f.Dir, f.RunID+"-*"))
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ShellQuote quotes one argument for the shell command line that the pane
// runs. The command is typed into a shell, so every injected argument needs
// it.
func ShellQuote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
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
	// HandleStartup returns auto-answer actions for TUI dialogs (trust, bypass).
	// Applied during WaitReady and again on live panes by the reaper.
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
	Role  string     `json:"role"` // user|assistant|thought|tools|request
	At    time.Time  `json:"at"`
	Text  string     `json:"text,omitempty"`
	Tools []ToolCall `json:"tools,omitempty"`
	// RunTurn is the run turn (harness launch) the entry belongs to.
	RunTurn int `json:"run_turn,omitempty"`
	// Origin tells why trebi wrote a user prompt: launch|resume|message|
	// request|ask|memory_fix|coalesce. Empty for a vendor user turn.
	Origin string `json:"origin,omitempty"`
	// Delivery tells how a prompt reached the session: steered|prompted|held.
	Delivery string `json:"delivery,omitempty"`
	// Attachments are the upload ids of the images of a user prompt.
	Attachments []string `json:"attachments,omitempty"`
	// Request is set on a request turn.
	Request *TurnRequest `json:"request,omitempty"`
	// Synthetic is set when the vendor CLI wrote the entry itself: not the
	// model, not a prompt. Not part of the /v1 shape.
	Synthetic bool `json:"-"`
}

// TurnRequest is the request of one request turn: its latest state.
type TurnRequest struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Title  string `json:"title"`
	Forms  int    `json:"forms"`
}

// ToolCall is one tool invocation on a turn.
type ToolCall struct {
	Name       string `json:"name"`
	Input      string `json:"input"`
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	// Label is the short words of the step; Kind is the agentwire tool kind.
	// Both are empty when unknown.
	Label string `json:"label,omitempty"`
	Kind  string `json:"kind,omitempty"`
	// ID pairs a call with its result inside one transcript file.
	ID string `json:"-"`
	// AtIsStart is set when the At of the turn is the start of the call, not
	// its end. A vendor file turn has the start; a wire event turn has the end.
	AtIsStart bool `json:"-"`
}

// EventModeCapability describes how a tool reports agent lifecycle to trebi.
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
		claudeTool{harnessHome{tool: Claude}},
		grokTool{harnessHome{tool: Grok}},
		// OpenCode 2.x is the same terminal application as 1.x: one policy,
		// two names. It is matched first so a path that contains "opencode2"
		// cannot fall through to the 1.x entry.
		opencodeTool{harnessHome: harnessHome{tool: OpenCode2}, name: OpenCode2},
		opencodeTool{harnessHome: harnessHome{tool: OpenCode}, name: OpenCode},
		cursorTool{harnessHome{tool: Cursor}},
		codexTool{harnessHome{tool: Codex}},
		copilotTool{},
		geminiTool{},
		piTool{harnessHome{tool: Pi}},
		ompTool{harnessHome{tool: OMP}},
		fakeTool{},
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

// ByName gives the tool of a harness. A variant gives the tool of its base
// with the home of the variant.
func ByName(name string) (Tool, bool) {
	for _, t := range AllTools() {
		if t.Name() == name {
			return t, true
		}
	}
	if name == "" || name == "unknown" {
		return unknownTool{}, true
	}
	if cfg, err := config.Load(); err == nil && harness.IsVariant(cfg, name) && cfg.HarnessBase(name) == Claude {
		return claudeTool{harnessHome{tool: name, kind: Claude}}, true
	}
	return nil, false
}

// DetectTool returns the tool name for command, or "" if unknown.
// Kept for callers that only need the string identity.
func detectTool(command string) string {
	t := Detect(command)
	if _, ok := t.(unknownTool); ok {
		return ""
	}
	return t.Name()
}

// InstallLifecycleAssets installs shim/hook/plugin assets for every tool that
// provides a LifecycleAdapter. trebiBase is typically config.BaseDir() (~/.trebi).
func installLifecycleAssets(trebiBase, binDir string) error {
	for _, t := range AllTools() {
		if lc := t.Lifecycle(); lc != nil {
			if err := lc.Install(trebiBase, binDir); err != nil {
				return fmt.Errorf("installing lifecycle for %s: %w", t.Name(), err)
			}
		}
	}
	return nil
}

// UninstallLifecycleAssets removes lifecycle assets for every tool (best-effort).
func uninstallLifecycleAssets(trebiBase, binDir string) error {
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

func unsupportedParse(io.Reader) ([]Turn, error) {
	return nil, ErrTranscriptUnsupported
}

// ParseTranscriptBytes parses complete lines in data. An incomplete trailing
// line is not consumed so a tail-parser can resume from the returned offset.
func ParseTranscriptBytes(t Tool, data []byte) (turns []Turn, consumed int64, err error) {
	if len(data) == 0 {
		return nil, 0, nil
	}
	cut := len(data)
	if data[len(data)-1] != '\n' {
		i := bytes.LastIndexByte(data, '\n')
		if i < 0 {
			return nil, 0, nil
		}
		cut = i + 1
	}
	turns, err = t.ParseTranscript(bytes.NewReader(data[:cut]))
	return turns, int64(cut), err
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

// harnessHome is the harness folder of a tool. The visor links the user
// files into it; trebi writes only its own lifecycle files. Tool is an
// interface, so this lives on an embedded base rather than on Tool.
// harnessHome is the home of tool. kind is the program of a variant; empty
// is tool.
type harnessHome struct{ tool, kind string }

func (h harnessHome) prepare() (*config.Config, string) {
	dir := config.HarnessDir(h.tool)
	h.materialize(dir)
	var cfg *config.Config
	if c, err := config.Load(); err == nil {
		cfg = c
	}
	return cfg, dir
}

func (h harnessHome) materialize(dir string) {
	switch cmp.Or(h.kind, h.tool) {
	case Claude:
		_ = (claudeLifecycle{}).materialize(dir)
	case Grok:
		_ = (grokLifecycle{}).materialize(dir)
	case Cursor:
		_ = (cursorLifecycle{}).materialize(dir)
	case OpenCode:
		_ = (opencodeLifecycle{}).materialize(dir)
	case Pi:
		_ = newPiLifecycle().materialize(dir)
	default:
		_ = harness.EnsureDir(dir)
	}
}

// autoUpdateOff is the env that stops name from updating itself in a run.
// The visor owns the updates.
func autoUpdateOff(name string) map[string]string {
	r, ok := agentwire.Recipe(agentwire.Harness(name))
	if !ok {
		return nil
	}
	return maps.Clone(r.UpdateOffEnv)
}
