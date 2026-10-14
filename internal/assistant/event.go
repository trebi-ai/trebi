package assistant

import (
	"time"

	"github.com/trebi-ai/agent-wire"
)

// Event is the wire form of one assistant event (plan 2026-09-26 D.7). It is
// our own type with JSON tags: agentwire.Event carries no tags and can change
// shape. Stored events hold the full body; live deltas carry Seq = 0 and are
// subscriber-only.
type Event struct {
	Seq         int64       `json:"seq,omitempty"` // 0 on a live-only delta
	Type        string      `json:"type"`          // init|user|assistant|tool|permission|answer|usage|result|error|status|exit|system
	Turn        int         `json:"turn,omitempty"`
	At          time.Time   `json:"at"`
	Text        string      `json:"text,omitempty"`
	Delta       bool        `json:"delta,omitempty"`
	Prompt      string      `json:"prompt,omitempty"`
	Context     []string    `json:"context,omitempty"`
	Attachments []string    `json:"attachments,omitempty"` // upload ids of a user event
	Channel     string      `json:"channel,omitempty"`     // channel_type of a user event
	Delivery    string      `json:"delivery,omitempty"`    // user event: prompted|steered|held
	Tool        *Tool       `json:"tool,omitempty"`
	Perm        *Permission `json:"permission,omitempty"`
	Answer      *Answer     `json:"answer,omitempty"`
	Usage       *Usage      `json:"usage,omitempty"`
	Result      *Result     `json:"result,omitempty"`
	Error       *Error      `json:"error,omitempty"`
	Status      string      `json:"status,omitempty"`
	ExitCode    *int        `json:"exit_code,omitempty"`
}

// Tool is one tool invocation.
type Tool struct {
	ID       string   `json:"id,omitempty"`
	Name     string   `json:"name,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Input    string   `json:"input,omitempty"`
	Output   string   `json:"output,omitempty"`
	Status   string   `json:"status,omitempty"` // started|completed|failed
	Paths    []string `json:"paths,omitempty"`
	ExitCode *int     `json:"exit_code,omitempty"`
}

// Permission is a permission request awaiting a decision.
type Permission struct {
	ID       string   `json:"id"`
	Tool     string   `json:"tool,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Question string   `json:"question,omitempty"`
	Input    string   `json:"input,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// Answer closes one permission request. By is user|timeout|allow_list.
type Answer struct {
	PermissionID string `json:"permission_id"`
	Decision     string `json:"decision"` // allow_once|allow_session|deny
	By           string `json:"by,omitempty"`
}

// Usage is token and cost accounting.
type Usage struct {
	Input         int64   `json:"input,omitempty"`
	Output        int64   `json:"output,omitempty"`
	CacheRead     int64   `json:"cache_read,omitempty"`
	CacheCreation int64   `json:"cache_creation,omitempty"`
	Model         string  `json:"model,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
}

// Result is the terminal payload of one turn.
type Result struct {
	Subtype   string `json:"subtype,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	EndReason string `json:"end_reason,omitempty"`
}

// Error is a classified failure.
type Error struct {
	Class   string `json:"class,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// eventFrom maps an agent-wire event to the wire type.
func eventFrom(e agentwire.Event) Event {
	out := Event{
		Type:     string(e.Type),
		Turn:     e.Turn,
		At:       e.At.UTC(),
		Text:     e.Text,
		Delta:    e.Delta,
		Status:   string(e.Status),
		ExitCode: e.ExitCode,
	}
	if out.At.IsZero() {
		out.At = time.Now().UTC()
	}
	if e.Tool != nil {
		out.Tool = &Tool{
			ID:       e.Tool.ID,
			Name:     e.Tool.Name,
			Kind:     string(e.Tool.Kind),
			Input:    e.Tool.Input,
			Output:   e.Tool.Output,
			Status:   e.Tool.Status,
			Paths:    e.Tool.Paths,
			ExitCode: e.Tool.ExitCode,
		}
	}
	if e.Permission != nil {
		out.Perm = &Permission{
			ID:       e.Permission.ID,
			Tool:     e.Permission.Tool,
			Kind:     string(e.Permission.Kind),
			Question: e.Permission.Question,
			Input:    e.Permission.Input,
			Options:  e.Permission.Options,
		}
	}
	if e.Usage != nil {
		out.Usage = &Usage{
			Input:         e.Usage.Input,
			Output:        e.Usage.Output,
			CacheRead:     e.Usage.CacheRead,
			CacheCreation: e.Usage.CacheCreation,
			Model:         e.Usage.Model,
			CostUSD:       e.Usage.CostUSD,
		}
	}
	if e.Result != nil {
		out.Result = &Result{
			Subtype:   e.Result.Subtype,
			IsError:   e.Result.IsError,
			EndReason: e.Result.EndReason,
		}
	}
	if e.Type == agentwire.EventError || e.Type == agentwire.EventExit {
		if e.Error != "" || e.Code != "" {
			out.Error = &Error{Class: e.EndReason, Code: e.Code, Message: e.Error}
		}
	}
	return out
}
