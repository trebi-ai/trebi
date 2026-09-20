// Package email owns the trebid side of the mail channel: the Worker
// client, workspace addresses, and the envelope log (plan 2026-09-19).
package email

import "time"

// ContractV is the wire version both shapes carry.
const ContractV = 1

// Settings is the resolved email block handed to the service at wiring
// time. The config package owns parsing; this is what the service acts on.
type Settings struct {
	Worker       string
	Token        string
	Domain       string
	Operators    []string
	PollInterval time.Duration
	RequireDMARC bool
	FromName     string
}

// Attachment is one inbound attachment's metadata. Bytes stay in the
// Worker's object store until someone reads them.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Type string `json:"type,omitempty"`
}

// IngressMessage is one spooled inbound message, the fixture shape
// testdata/contract/ingress-message.v1.json defines.
type IngressMessage struct {
	V           int               `json:"v"`
	Seq         int64             `json:"seq"`
	ID          string            `json:"id"`
	LocalPart   string            `json:"local_part"`
	Tag         string            `json:"tag"`
	From        string            `json:"from"`
	To          []string          `json:"to"`
	Cc          []string          `json:"cc"`
	Subject     string            `json:"subject"`
	MessageID   string            `json:"message_id"`
	InReplyTo   string            `json:"in_reply_to"`
	References  []string          `json:"references"`
	Date        string            `json:"date"`
	Text        string            `json:"text"`
	HTML        string            `json:"html"`
	Attachments []Attachment      `json:"attachments"`
	Headers     map[string]string `json:"headers"`
	Spf         string            `json:"spf"`
	Dkim        string            `json:"dkim"`
	Dmarc       string            `json:"dmarc"`
	ReceivedAt  string            `json:"received_at"`
}

// SendRequest is the outbound shape testdata/contract/send-request.v1.json
// defines. The caller always supplies Message-ID so it can store the
// envelope before the send.
type SendRequest struct {
	V          int      `json:"v"`
	From       string   `json:"from"`
	FromName   string   `json:"from_name"`
	To         []string `json:"to"`
	Cc         []string `json:"cc"`
	Subject    string   `json:"subject"`
	Text       string   `json:"text"`
	MessageID  string   `json:"message_id"`
	InReplyTo  string   `json:"in_reply_to,omitempty"`
	References []string `json:"references,omitempty"`
}

// Health is the Worker fact sheet. Spool depth and oldest-entry age are
// the signal that trebid has stopped pulling.
type Health struct {
	Version          string           `json:"version"`
	SpoolDepth       int64            `json:"spool_depth"`
	SpoolOldestAgeMS int64            `json:"spool_oldest_age_ms"`
	KnownCount       int              `json:"known_count"`
	Counters         map[string]int64 `json:"counters"`
}

// Address is one workspace's permanent mail address row.
type Address struct {
	WorkspaceID string `json:"workspace_id"`
	LocalPart   string `json:"local_part"`
	Domain      string `json:"domain"`
	CreatedAt   string `json:"created_at,omitempty"`
	RotatedAt   string `json:"rotated_at,omitempty"`
	Revision    int    `json:"revision"`
}

// Full returns local@domain.
func (a Address) Full() string { return a.LocalPart + "@" + a.Domain }
