// Package wire holds the assistant prompt types that both the conversations
// service and the assistant manager use (plan 2026-09-26 Part E). It is a
// leaf: neither parent may import the other through it.
package wire

import (
	"time"

	"github.com/trebi-ai/trebi/internal/uploads"
)

// Message is one conversation message row, reduced to the fields the prompt
// path and its callers use.
type Message struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Role           string    `json:"role"`
	Connector      string    `json:"connector,omitempty"`
	Instance       string    `json:"instance,omitempty"`
	Room           string    `json:"room,omitempty"`
	Thread         string    `json:"thread,omitempty"`
	Text           string    `json:"text"`
	TurnIndex      *int      `json:"turn_index,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	Attachments    []string  `json:"attachments,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// PromptInput is one user turn for the assistant session manager.
type PromptInput struct {
	// Text is the full prompt, context blocks included.
	Text string
	// Display is the draft without the context blocks. Empty means Text.
	Display string
	// Context holds the chip labels of the context blocks.
	Context []string
	// Connector, Instance, Room, and Thread mark where the turn came from.
	Connector string
	Instance  string
	Room      string
	Thread    string
	// IdempotencyKey deduplicates a resent prompt.
	IdempotencyKey string
	// Harness and Mode apply on the first prompt of a conversation only.
	Harness string
	Mode    string
	// Headless marks a prompt with no viewer: email, trigger, escalation.
	// The manager denies a pending permission after the permission timeout.
	Headless bool
	// Attachments are the images of the turn.
	Attachments uploads.Attachments
}

// PromptResult is the outcome of one Prompt.
type PromptResult struct {
	Message   Message `json:"message"`
	Duplicate bool    `json:"duplicate"`
	Status    string  `json:"status"`
	Delivery  string  `json:"delivery,omitempty"` // prompted|steered|held
}

// AskResult is the outcome of one Ask: the stored user row plus the
// assistant replies that arrived inside the wait window.
type AskResult struct {
	Message  Message   `json:"message"`
	Replies  []Message `json:"replies"`
	Status   string    `json:"status"`
	Delivery string    `json:"delivery,omitempty"`
}
