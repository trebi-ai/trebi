// Package email owns the trebid side of the mail channel: the Worker
// client, workspace addresses, and the envelope log (plan 2026-09-19).
package email

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
)

// ContractV is the wire version both shapes carry.
const ContractV = 1

// Settings is the resolved email block handed to the service at wiring
// time. The config package owns parsing; this is what the service acts on.
type Settings struct {
	Worker       string
	Token        string
	Domain       string
	Admins       []string
	PollInterval time.Duration
	RequireDMARC bool
	FromName     string
	// ContentRetention is how long message text stays in the store.
	// Zero means the daemon does not keep it.
	ContentRetention time.Duration
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
	// Thread is the thread the daemon found; it wins over the headers.
	Thread string `json:"-"`
}

// Addressed reports whether To or Cc names the address. A signed mail sent
// to another person and replayed here does not.
func (m IngressMessage) Addressed() bool {
	want := strings.ToLower(m.LocalPart)
	for _, a := range slices.Concat(m.To, m.Cc) {
		local, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(a)), "@")
		base, _, _ := strings.Cut(local, "+")
		if ok && base == want {
			return true
		}
	}
	return false
}

// SingleFrom reports whether the From header names one address, so the
// DMARC verdict and the sender are about the same address.
func (m IngressMessage) SingleFrom() bool {
	raw, ok := m.Headers["from"]
	if !ok {
		return true
	}
	if strings.Contains(raw, "\n") {
		return false // two From headers
	}
	p := mail.AddressParser{WordDecoder: &mime.WordDecoder{
		CharsetReader: func(_ string, in io.Reader) (io.Reader, error) { return in, nil },
	}}
	list, err := p.ParseList(raw)
	if err != nil {
		return strings.Count(raw, "@") == 1
	}
	return len(list) == 1
}

// SendRequest is the outbound shape testdata/contract/send-request.v1.json
// defines. The caller always supplies Message-ID so it can store the
// envelope before the send.
type SendRequest struct {
	V           int              `json:"v"`
	From        string           `json:"from"`
	FromName    string           `json:"from_name"`
	To          []string         `json:"to"`
	Cc          []string         `json:"cc,omitempty"`
	Subject     string           `json:"subject"`
	Text        string           `json:"text"`
	HTML        string           `json:"html,omitempty"`
	Attachments []SendAttachment `json:"attachments,omitempty"`
	MessageID   string           `json:"message_id"`
	InReplyTo   string           `json:"in_reply_to,omitempty"`
	References  []string         `json:"references,omitempty"`
}

// SendAttachment is one outbound file. Content is base64 on the wire.
type SendAttachment struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content []byte `json:"content"`
}

// normalize checks the name and sets the media type. An empty type comes
// from the file extension.
func (a *SendAttachment) normalize() error {
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" || len(a.Name) > 255 || strings.ContainsAny(a.Name, "/\\") ||
		strings.ContainsFunc(a.Name, unicode.IsControl) {
		return fmt.Errorf("%w: file name %q must be a plain name of 1 to 255 bytes", ErrContent, a.Name)
	}
	if a.Type == "" {
		a.Type = cmp.Or(mime.TypeByExtension(path.Ext(a.Name)), "application/octet-stream")
	}
	mt, _, err := mime.ParseMediaType(a.Type)
	if err != nil || !strings.Contains(mt, "/") {
		return fmt.Errorf("%w: file %q has no valid media type", ErrContent, a.Name)
	}
	a.Type = mt
	return nil
}

// Outbound limits of the send contract. Size counts text, html, and the
// decoded attachment bytes.
const (
	MaxAttachments = 10
	MaxSendBytes   = 5 << 20
)

// SendResult is the accepted reply from one send.
type SendResult struct {
	MessageID string `json:"message_id"`
}

// Transport moves mail: one send, one attachment read, one health probe.
// The bearer Worker client and the hosted cloudlink client both satisfy
// it, and the two never run at once (plan 2026-09-22 Part D).
type Transport interface {
	Send(ctx context.Context, req SendRequest) (SendResult, error)
	Attachment(ctx context.Context, key string) ([]byte, string, error)
	Health(ctx context.Context) (Health, error)
}

// HostedMint mints one hosted address in the cloud and returns it as
// local@domain. The hosted transport satisfies it; the bearer transport
// mints locally with crypto/rand instead. A second mint for the same
// workspace replaces the hosted address.
type HostedMint interface {
	MintAddress(ctx context.Context, workspaceID, handle string) (string, error)
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
