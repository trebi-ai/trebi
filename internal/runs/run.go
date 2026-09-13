package runs

import (
	"encoding/json"
	"time"

	"github.com/trebi-ai/trebi/internal/pricing"
)

// Run status — trebi's taxonomy, verbatim (B2).
// LIVE = {running}. `resumed` is SUPERSEDED-terminal (B17).
type Status string

const (
	StatusRunning  Status = "running"
	StatusComplete Status = "complete"
	StatusFailed   Status = "failed"
	StatusExited   Status = "exited"
	StatusSkipped  Status = "skipped"
	StatusTimedOut Status = "timed_out"
	StatusResumed  Status = "resumed"
)

// IsLive reports whether status counts as an active run.
func (s Status) IsLive() bool { return s == StatusRunning }

// IsTerminal reports a finished run (including superseded resumed).
func (s Status) IsTerminal() bool {
	switch s {
	case StatusComplete, StatusFailed, StatusExited, StatusSkipped, StatusTimedOut, StatusResumed:
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
)

// RunReason — launch producer.
type RunReason string

const (
	ReasonManual   RunReason = "manual"
	ReasonSchedule RunReason = "schedule"
	ReasonTrigger  RunReason = "trigger"
	ReasonWhen     RunReason = "when"
	ReasonResume   RunReason = "resume"
	ReasonReplay   RunReason = "replay"
)

// IfRunning overlap policy.
type IfRunning string

const (
	IfRunningSkip     IfRunning = "skip"
	IfRunningReplace  IfRunning = "replace"
	IfRunningCoalesce IfRunning = "coalesce"
	IfRunningAllow    IfRunning = "allow"
)

// Kind for runs: job | assistant (B27).
const (
	KindJob       = "job"
	KindAssistant = "assistant"
	KindAgent     = "agent"
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
	case StatusRunning, StatusComplete, StatusFailed, StatusExited, StatusSkipped, StatusTimedOut, StatusResumed:
		return true
	}
	return false
}

// ValidEndReason reports whether r is a known end reason.
func ValidEndReason(r string) bool {
	switch EndReason(r) {
	case EndSentinel, EndKeepAlive, EndKeepAliveIdle, EndIdleTimeout, EndMaxDuration,
		EndAuth, EndLimit, EndKill, EndReplace, EndSkip, EndLaunch, EndFailed, EndSessionGone, EndUpdate:
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
	Kind  string `json:"kind"` // tmux|web|none
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
	QueueID           string             `json:"queue_id,omitempty"`
	TraceID           string             `json:"trace_id"`
	Revision          int64              `json:"revision"`
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
