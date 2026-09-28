package runs

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/pricing"
)

// Run status — trebi's taxonomy, verbatim (B2).
// LIVE = {running}. `resumed` is SUPERSEDED-terminal (B17). `parked` is
// terminal: the run waits for the answer to a request.
type Status string

const (
	StatusRunning  Status = "running"
	StatusComplete Status = "complete"
	StatusFailed   Status = "failed"
	StatusExited   Status = "exited"
	StatusSkipped  Status = "skipped"
	StatusTimedOut Status = "timed_out"
	StatusResumed  Status = "resumed"
	StatusParked   Status = "parked"
)

// IsLive reports whether status counts as an active run.
func (s Status) IsLive() bool { return s == StatusRunning }

// IsTerminal reports a finished run (including superseded resumed).
func (s Status) IsTerminal() bool {
	switch s {
	case StatusComplete, StatusFailed, StatusExited, StatusSkipped, StatusTimedOut, StatusResumed, StatusParked:
		return true
	}
	return false
}

// SessionState — separate from Status (B1).
type SessionState string

const (
	SessionRunning SessionState = "running"
	SessionWaiting SessionState = "waiting"
	SessionIdle    SessionState = "idle"
)

// EndReason — D7 failure classifier.
type EndReason string

const (
	EndSentinel      EndReason = "sentinel"
	EndResult        EndReason = "result"
	EndKeepAlive     EndReason = "keep_alive"
	EndKeepAliveIdle EndReason = "keep_alive_idle"
	EndIdleTimeout   EndReason = "idle_timeout"
	EndMaxDuration   EndReason = "max_duration"
	EndAuth          EndReason = "auth"
	EndLimit         EndReason = "limit"
	EndKill          EndReason = "kill"
	EndReplace       EndReason = "replace"
	EndSkip          EndReason = "skip"
	EndLaunch        EndReason = "launch"
	EndFailed        EndReason = "failed"
	EndSessionGone   EndReason = "session_gone"
	EndUpdate        EndReason = "update"
	// EndRequest parks a run that waits for the answer to a request.
	EndRequest EndReason = "request"
	// EndRequestDismissed completes a parked run whose request was dismissed.
	EndRequestDismissed EndReason = "request_dismissed"
)

// RunReason — launch producer.
type RunReason string

const (
	ReasonManual     RunReason = "manual"
	ReasonSchedule   RunReason = "schedule"
	ReasonTrigger    RunReason = "trigger"
	ReasonResume     RunReason = "resume"
	ReasonReplay     RunReason = "replay"
	ReasonEscalation RunReason = "escalation"
	ReasonReport     RunReason = "report"
	// ReasonRequest continues a parked session with the answer to a request.
	ReasonRequest RunReason = "request"
)

// Kind for runs: job | agent. Declared in dispatch, the lowest
// consumer, so event_queue.kind and runs.kind share one spelling.
const (
	KindJob   = dispatch.KindJob
	KindAgent = dispatch.KindAgent
)

// JobDisplayStatus — derived for lists (not run Status).
type JobDisplayStatus string

const (
	JobDisplayRunning JobDisplayStatus = "running"
	JobDisplayWaiting JobDisplayStatus = "waiting"
	JobDisplayOK      JobDisplayStatus = "ok"
	JobDisplayFailed  JobDisplayStatus = "failed"
	JobDisplayIdle    JobDisplayStatus = "idle"
	JobDisplayPaused  JobDisplayStatus = "paused"
)

// DeriveJobDisplayStatus applies the §3.2 derivation: paused if inactive;
// waiting if a live run waits on a human; running if any live run; else map
// the last run (complete → ok, failed|timed_out → failed); idle otherwise.
func DeriveJobDisplayStatus(active bool, liveCount int, liveWaiting bool, last Status) JobDisplayStatus {
	switch {
	case !active:
		return JobDisplayPaused
	case liveWaiting:
		return JobDisplayWaiting
	case liveCount > 0:
		return JobDisplayRunning
	case last == StatusComplete:
		return JobDisplayOK
	case last == StatusFailed || last == StatusTimedOut:
		return JobDisplayFailed
	}
	return JobDisplayIdle
}

// ValidStatus reports whether s is a known run status.
func ValidStatus(s string) bool {
	switch Status(s) {
	case StatusRunning, StatusComplete, StatusFailed, StatusExited, StatusSkipped, StatusTimedOut, StatusResumed, StatusParked:
		return true
	}
	return false
}

// ValidEndReason reports whether r is a known end reason.
func ValidEndReason(r string) bool {
	switch EndReason(r) {
	case EndSentinel, EndResult, EndKeepAlive, EndKeepAliveIdle, EndIdleTimeout, EndMaxDuration,
		EndAuth, EndLimit, EndKill, EndReplace, EndSkip, EndLaunch, EndFailed, EndSessionGone, EndUpdate,
		EndRequest, EndRequestDismissed:
		return true
	}
	return false
}

// RunSummary embeds in job rows and Home cards.
type RunSummary struct {
	ID         string    `json:"id"`
	Status     Status    `json:"status"`
	EndReason  EndReason `json:"end_reason,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	CostUSD    float64   `json:"cost_usd"`
}

// Outcome is a human verdict over a terminal run's machine status. The
// reaper's status/end_reason stay as recorded; display, job health, and the
// inbox read the outcome instead.
type Outcome struct {
	Status Status    `json:"status"`
	Note   string    `json:"note,omitempty"`
	At     time.Time `json:"at"`
}

// ValidOutcomeStatus reports whether s may be set as a human outcome.
func ValidOutcomeStatus(s Status) bool {
	switch s {
	case StatusComplete, StatusFailed, StatusSkipped:
		return true
	}
	return false
}

// OutcomeStatus normalizes a human outcome word: "success" is the UI word for
// complete, and complete|failed|skipped pass through. Unknown words return "".
func OutcomeStatus(word string) Status {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "success", "complete":
		return StatusComplete
	case "failed":
		return StatusFailed
	case "skipped":
		return StatusSkipped
	}
	return ""
}

// CostSummary — per-job cost display (D6 tier 1).
type CostSummary struct {
	MTDUSD    float64 `json:"mtd_usd"`
	Runs30d   int     `json:"runs_30d"`
	AvgRunMS  int64   `json:"avg_run_ms"`
	AvgRunUSD float64 `json:"avg_run_usd"`
}

// SessionSpec — effective per-run session knobs (wire + frontmatter).
type SessionSpec struct {
	MaxDuration string `json:"max_duration,omitempty" yaml:"max_duration,omitempty"`
	IdleTimeout string `json:"idle_timeout,omitempty" yaml:"idle_timeout,omitempty"`
	KeepAlive   string `json:"keep_alive,omitempty" yaml:"keep_alive,omitempty"`
	Nudge       string `json:"nudge,omitempty" yaml:"nudge,omitempty"`
	Resumable   string `json:"resumable,omitempty" yaml:"resumable,omitempty"`
	IfRunning   string `json:"if_running,omitempty" yaml:"if_running,omitempty"`
	// Duration fields in ms for wire runs (when populated from DB).
	MaxDurationMS int64 `json:"max_duration_ms,omitempty" yaml:"-"`
	IdleTimeoutMS int64 `json:"idle_timeout_ms,omitempty" yaml:"-"`
	KeepAliveMS   int64 `json:"keep_alive_ms,omitempty" yaml:"-"`
	NudgeMS       int64 `json:"nudge_ms,omitempty" yaml:"-"`
	ResumableMS   int64 `json:"resumable_ms,omitempty" yaml:"-"`
}

// AttachInfo — how to attach to a live run.
type AttachInfo struct {
	Kind  string `json:"kind"` // tmux|events|web|none
	Tmux  string `json:"tmux,omitempty"`
	WSURL string `json:"ws_url,omitempty"`
}

// TriggerCapture — replay input (D4).
type TriggerCapture struct {
	EventID   string            `json:"event_id,omitempty"`
	Connector string            `json:"connector,omitempty"`
	Vars      map[string]string `json:"vars,omitempty"`
	Event     json.RawMessage   `json:"event,omitempty"`
}

// Run — one execution (wire shape §3.4).
type Run struct {
	ID                string             `json:"id"`
	Workspace         string             `json:"workspace"`
	Job               string             `json:"job"`
	Kind              string             `json:"kind"`
	Reason            RunReason          `json:"reason,omitempty"`
	Status            Status             `json:"status"`
	EndReason         EndReason          `json:"end_reason,omitempty"`
	SessionState      SessionState       `json:"session_state,omitempty"`
	SessionStateSince *time.Time         `json:"session_state_since,omitempty"`
	StartedAt         time.Time          `json:"started_at"`
	EndedAt           *time.Time         `json:"ended_at,omitempty"`
	DurationMS        int64              `json:"duration_ms"`
	Harness           string             `json:"harness"`
	Runtime           string             `json:"runtime"`
	Model             string             `json:"model,omitempty"`
	SessionID         string             `json:"session_id,omitempty"`
	TmuxSession       string             `json:"tmux_session,omitempty"`
	Attach            *AttachInfo        `json:"attach,omitempty"`
	PromptPreview     string             `json:"prompt_preview"`
	Inputs            map[string]string  `json:"inputs,omitempty"`
	Trigger           *TriggerCapture    `json:"trigger,omitempty"`
	ConversationKey   string             `json:"conversation_key,omitempty"`
	ConversationID    string             `json:"conversation_id,omitempty"`
	WorkingDir        string             `json:"working_dir,omitempty"`
	Tokens            pricing.TokenUsage `json:"tokens"`
	CoalescedCount    int                `json:"coalesced_count,omitempty"`
	Session           SessionSpec        `json:"session"`
	ResumedFrom       string             `json:"resumed_from,omitempty"`
	ResumeAttempts    int                `json:"resume_attempts,omitempty"`
	Outcome           *Outcome           `json:"outcome,omitempty"`
	QueueID           string             `json:"queue_id,omitempty"`
	TraceID           string             `json:"trace_id"`
	// MemoryHash is the launch-time MEMORY.md hash; empty when memory is off.
	MemoryHash string `json:"memory_hash,omitempty"`
	// RuntimePID is the subprocess child pid for runtime=cli runs.
	RuntimePID int   `json:"runtime_pid,omitempty"`
	Revision   int64 `json:"revision"`
}

// attachKindFor derives the persisted attach kind from the launch options.
func attachKindFor(opts CreateOpts) string {
	switch {
	case opts.TmuxSession != "":
		return "tmux"
	case opts.Runtime == "native":
		return "none"
	case opts.Runtime == "cli":
		return "events"
	}
	return ""
}

// EffectiveStatus is the human outcome when one is set, else the machine
// status the reaper recorded.
func (r *Run) EffectiveStatus() Status {
	if r.Outcome != nil {
		return r.Outcome.Status
	}
	return r.Status
}

// Timeline — two-layer segments + point events.
type Timeline struct {
	RunID    string            `json:"run_id"`
	Segments []TimelineSegment `json:"segments"`
	Events   []TimelinePoint   `json:"events"`
}

type TimelineSegment struct {
	Layer      string          `json:"layer"` // run|session
	State      string          `json:"state"`
	Start      time.Time       `json:"start"`
	End        *time.Time      `json:"end,omitempty"`
	DurationMS *int64          `json:"duration_ms,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Source     string          `json:"source,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
}

type TimelinePoint struct {
	Kind   string          `json:"kind"`
	At     time.Time       `json:"at"`
	Source string          `json:"source,omitempty"`
	Detail json.RawMessage `json:"detail,omitempty"`
}
