// Package chains owns agent-to-agent work: asks, hand-offs, replies, the
// chain limits, and the links between the runs of one chain (backend §3.27).
package chains

import (
	"time"

	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Kind is the kind of a hop.
type Kind string

const (
	KindAsk     Kind = "ask"
	KindHandoff Kind = "handoff"
	KindReply   Kind = "reply"
)

// HopStatus is the state of a hop.
type HopStatus string

const (
	HopHeld     HopStatus = "held"
	HopQueued   HopStatus = "queued"
	HopRunning  HopStatus = "running"
	HopAnswered HopStatus = "answered"
	HopFailed   HopStatus = "failed"
	HopRefused  HopStatus = "refused"
	HopStopped  HopStatus = "stopped"
	HopDone     HopStatus = "done"
)

// RefuseReason tells why the daemon refused or held a hop.
type RefuseReason string

const (
	RefuseHops     RefuseReason = "hops"
	RefuseTurns    RefuseReason = "turns"
	RefuseUSD      RefuseReason = "usd"
	RefuseCycle    RefuseReason = "cycle"
	RefuseRate     RefuseReason = "rate"
	RefuseTeam     RefuseReason = "team"
	RefuseDisabled RefuseReason = "disabled"
	RefuseBudget   RefuseReason = "budget"
	RefuseStopped  RefuseReason = "stopped"
	RefuseArchived RefuseReason = "archived"
)

// Text is the reason in plain words, for the inbox.
func (r RefuseReason) Text() string {
	switch r {
	case RefuseHops:
		return "the agents already passed the work too many times in this chain"
	case RefuseTurns:
		return "the agents already took too many turns in this chain"
	case RefuseUSD, RefuseBudget:
		return "the chain reached its spend limit"
	case RefuseCycle:
		return "the request would go back to an agent that already works on it"
	case RefuseRate:
		return "the agents asked each other too often in a short time"
	case RefuseTeam:
		return "the other agent is not in its team"
	case RefuseDisabled:
		return "asking other agents is off"
	case RefuseStopped:
		return "someone stopped the chain"
	case RefuseArchived:
		return "the other agent is archived"
	}
	return "the chain reached a limit"
}

// Chain status.
const (
	StatusOpen    = "open"
	StatusHeld    = "held"
	StatusStopped = "stopped"
)

// End kinds.
const (
	EndAgent     = "agent"
	EndAssistant = "assistant"
	EndHuman     = "human"
)

// Delivery states of a reply hop.
const (
	DeliveryNone      = "none"
	DeliveryClaimed   = "claimed"
	DeliveryDelivered = "delivered"
	DeliveryDropped   = "dropped"
)

// Limits are the limits of one chain.
type Limits = workspaces.ChainLimits

// Usage is what a chain used so far.
type Usage struct {
	Depth   int                 `json:"depth"`
	Turns   int                 `json:"turns"`
	CostUSD float64             `json:"cost_usd"`
	Tokens  pricing.TokenTotals `json:"tokens"`
	Runs    int                 `json:"runs"`
}

// Chain is all work that one start causes. One chain has one trace ID.
type Chain struct {
	TraceID            string    `json:"trace_id"`
	Workspace          string    `json:"workspace"`
	RootKind           string    `json:"root_kind"`
	RootRunID          string    `json:"root_run_id,omitempty"`
	RootConversationID string    `json:"root_conversation_id,omitempty"`
	RootAgent          string    `json:"root_agent,omitempty"`
	Status             string    `json:"status"`
	Live               bool      `json:"live"`
	Limits             Limits    `json:"limits"`
	Usage              Usage     `json:"usage"`
	StoppedBy          string    `json:"stopped_by,omitempty"`
	StopReason         string    `json:"stop_reason,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	Revision           int64     `json:"revision"`

	rootAgentID string
}

// Hop is one message from one operator to an agent inside a chain.
type Hop struct {
	ID             string       `json:"id"`
	TraceID        string       `json:"trace_id"`
	Workspace      string       `json:"workspace"`
	Kind           Kind         `json:"kind"`
	Status         HopStatus    `json:"status"`
	RefuseReason   RefuseReason `json:"refuse_reason,omitempty"`
	Depth          int          `json:"depth"`
	Path           []string     `json:"path"`
	From           End          `json:"from"`
	To             End          `json:"to"`
	ReplyOf        string       `json:"reply_of,omitempty"`
	ConversationID string       `json:"conversation_id,omitempty"`
	MessageID      string       `json:"message_id,omitempty"`
	QueueID        string       `json:"queue_id,omitempty"`
	RequestID      string       `json:"request_id,omitempty"`
	DeliveryState  string       `json:"delivery_state,omitempty"`
	Preview        string       `json:"preview"`
	Text           string       `json:"text,omitempty"`
	Context        string       `json:"context,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
	Revision       int64        `json:"revision"`

	pathIDs []string
	idem    string
}

// End is one side of a hop. RunID is empty until the target run launches,
// and always empty for a human or the assistant.
type End struct {
	Kind           string `json:"kind"`
	AgentID        string `json:"agent_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	Turn           int    `json:"turn,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
}

// HopRef is the short form of a hop on runs and turns.
type HopRef struct {
	ID      string    `json:"id"`
	Kind    Kind      `json:"kind"`
	Status  HopStatus `json:"status"`
	From    End       `json:"from"`
	To      End       `json:"to"`
	Preview string    `json:"preview"`
	At      time.Time `json:"at"`
}

// Ref returns the short form of h.
func (h *Hop) Ref() HopRef {
	return HopRef{ID: h.ID, Kind: h.Kind, Status: h.Status, From: h.From, To: h.To, Preview: h.Preview, At: h.CreatedAt}
}

// RunChain is the chain block of GET /v1/runs/{id}.
type RunChain struct {
	TraceID string   `json:"trace_id"`
	Status  string   `json:"status"`
	Depth   int      `json:"depth"`
	Path    []string `json:"path"`
	Limits  Limits   `json:"limits"`
	Usage   Usage    `json:"usage"`
	In      []HopRef `json:"in"`
	Out     []HopRef `json:"out"`
}

// ChainRun is one run of a chain on GET /v1/chains/{trace_id}.
type ChainRun struct {
	runs.RunSummary
	Agent string `json:"agent"`
	Kind  string `json:"kind"`
	Depth int    `json:"depth"`
	Turns []int  `json:"turns"`
}
