// Package cli is the subprocess run runtime: it spawns vendor coding-agent
// CLIs and speaks their structured protocols (Claude stream-json, Codex
// app-server, OpenCode serve, ACP, pi RPC).
//
// It replaces tmux TUI scraping as the default vendor runtime
// (docs/plans/2026-09-16-harness-subprocess-runtime.md). The tmux policy
// layer (internal/tmux) still owns launch/resume planning, isolation env,
// auth probes and transcripts; this package turns a prepared command into a
// live Session and normalizes wire events for the runs layer.
//
// The wire is Go-native. Official SDKs are the reference implementation and
// the fixture oracle at build/test time only; they are never a runtime
// dependency (Part P).
package cli

import (
	"context"
	"encoding/json"
	"time"
)

// EventType is the normalized wire vocabulary. One vendor turn emits status,
// progress, permissions, and exactly one terminal result.
type EventType string

const (
	// EventInit reports the vendor session is live (system/init, thread id).
	EventInit EventType = "init"
	// EventAssistant is assistant text; Delta=true means a stream delta.
	EventAssistant EventType = "assistant"
	// EventUser echoes a user turn (prompt write).
	EventUser EventType = "user"
	// EventTool is a tool lifecycle event (started/completed/failed).
	EventTool EventType = "tool"
	// EventPermission is a human decision request (can_use_tool, approvals).
	EventPermission EventType = "permission"
	// EventResult is the terminal turn event. Exactly one per turn.
	EventResult EventType = "result"
	// EventError is a classified failure (wire or process).
	EventError EventType = "error"
	// EventUsage carries token/cost counters.
	EventUsage EventType = "usage"
	// EventStatus is a coarse session status signal (busy/idle/compaction).
	EventStatus EventType = "status"
	// EventExit reports the child process ended (always emitted, after result).
	EventExit EventType = "exit"
)

// SessionStatus is the activity view of a session (G.3).
type SessionStatus string

const (
	StatusUnknown SessionStatus = ""
	StatusRunning SessionStatus = "running"
	StatusWaiting SessionStatus = "waiting"
	StatusIdle    SessionStatus = "idle"
)

// Event is one normalized wire event from a vendor session.
type Event struct {
	Type EventType
	At   time.Time

	// SessionID carries the vendor session/thread id on init and result.
	SessionID string

	// Text is assistant/user text or a short human summary.
	Text string
	// Delta marks Text as a stream delta rather than a complete block.
	Delta bool

	Tool       *ToolEvent
	Permission *Permission
	Result     *Result
	Usage      *Usage
	// Status is the tracker hint of a status event.
	Status SessionStatus

	// Error is a human-readable failure for EventError/EventExit.
	Error string
	// EndReason is the classified failure family (auth|limit|entitlement|...).
	EndReason string
	// Hint is the operator-facing doctor hint for a classified failure.
	Hint string
	// ExitCode is set on EventExit.
	ExitCode *int

	// Raw is the vendor frame, kept for fixtures and the run log.
	Raw []byte
}

// ToolEvent is one tool invocation (start or completion).
type ToolEvent struct {
	ID       string
	Name     string
	Input    string
	Output   string
	Status   string // started|completed|failed
	ExitCode *int
}

// Permission is a vendor permission request awaiting a decision.
type Permission struct {
	ID       string
	Tool     string
	Question string
	Input    string
	Options  []string
}

// Decision answers a Permission. Message is shown to the model on deny.
type Decision struct {
	Allow        bool
	Message      string
	UpdatedInput json.RawMessage
}

// Usage is token/cost accounting (vendor units).
type Usage struct {
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
	Model         string
	CostUSD       float64
}

// Result is the terminal turn payload. Subtype and IsError come from the
// vendor; EndReason is the classified family when the result is an error.
type Result struct {
	Subtype   string
	IsError   bool
	Text      string
	SessionID string
	Usage     Usage
	EndReason string
}

// Prompt is one user turn written to a live session.
type Prompt struct {
	Text string
	// Kind is the origin: job|assistant|agent|memory_fix|followup|wake.
	Kind string
}

// StartRequest carries everything a driver needs to spawn a session. The runs
// layer resolves policy (binary pin, isolation env, session id) first; the
// driver only adds protocol flags.
type StartRequest struct {
	RunID string
	// Tool is the harness identity (claude, codex, opencode, …).
	Tool string
	// BaseCommand is the prepared command string (pinned binary + policy flags)
	// built by tmux.PrepareLaunch / PrepareResume and ApplyModelEffort.
	BaseCommand string
	// ExtraArgs are appended verbatim after the driver's protocol flags.
	ExtraArgs  []string
	WorkingDir string
	// Env is the merged child env (secret env + job env + plan env), already
	// precedence-resolved. It wins over the process environment.
	Env map[string]string
	// SessionID is a known session id (resume or pre-minted).
	SessionID string
	Model     string
	Effort    string

	// Persistent keeps stdin open for follow-up turns; OneShot writes the
	// prompt and closes stdin (process exit terminates the run).
	Persistent bool
	OneShot    bool

	// PermissionMode is the resolved vendor permission mode (default,
	// acceptEdits, bypassPermissions, plan). Empty = tool default.
	PermissionMode string

	// ApprovalPolicy and Sandbox are vendor knobs (Codex) resolved by the
	// runs layer. Empty = driver default (unattended-safe).
	ApprovalPolicy string
	Sandbox        string

	// HandshakeTimeout overrides the protocol handshake wait (tests).
	HandshakeTimeout time.Duration

	// MCPConfigPath is the merged MCP config for the run, when one exists.
	MCPConfigPath string

	// OneShotPrompt is written and stdin closed when OneShot is set.
	OneShotPrompt string

	// LogPath is the run log file, when the caller wants raw frames kept.
	LogPath string
}

// Session is one vendor conversation. Runs holds it for the life of the run.
type Session interface {
	Provider() string
	// ID is the vendor session id; may be empty until init.
	ID() string
	// PID is the child process id (0 for in-process/HTTP sessions).
	PID() int
	Prompt(ctx context.Context, p Prompt) error
	Interrupt(ctx context.Context) error
	Close(ctx context.Context) error
	// Events is closed after the session is finished and terminal events are
	// drained. A consumer must read until it closes.
	Events() <-chan Event
}

// Answerer is implemented by sessions that route permission decisions to the
// vendor (Claude control_response, Codex approvals, ACP).
type Answerer interface {
	AnswerPermission(ctx context.Context, permissionID string, d Decision) error
}

// Interrupter is the optional protocol-level interrupt (e.g. Claude
// control_request subtype interrupt). Falls back to a signal on Close.
type Interrupter interface {
	InterruptTurn(ctx context.Context) error
}

// Driver starts sessions for one vendor protocol.
type Driver interface {
	Name() string
	Start(ctx context.Context, req StartRequest) (Session, error)
	Resume(ctx context.Context, req StartRequest) (Session, error)
}

// DriverFor resolves the built-in driver for a harness name. ok=false means
// the tool has no proven wire and must use the TUI runtime.
func DriverFor(tool string) (Driver, bool) {
	switch tool {
	case "claude":
		return ClaudeDriver{}, true
	case "codex":
		return CodexDriver{}, true
	case "opencode":
		return OpenCodeDriver{}, true
	case "pi":
		return PiDriver{}, true
	case "copilot":
		return CopilotDriver{}, true
	case "cursor":
		return CursorDriver{}, true
	}
	return nil, false
}
