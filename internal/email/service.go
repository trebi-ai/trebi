package email

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/store"
)

// ErrNotConfigured means the email block is absent: nothing to act on.
var ErrNotConfigured = errors.New("email is not configured")

// ErrRecipient means one send recipient is neither an operator nor on the
// workspace allowlist (plan I.3: an agent cannot mail a third party).
var ErrRecipient = errors.New("email recipient is not an operator or allowlisted address")

// ErrContent means the HTML part or a file breaks the send contract limits.
var ErrContent = errors.New("email content is not valid")

// WorkspaceRef is the (id, handle) pair address minting needs. The caller
// owns the workspace list; email never reads the workspaces table.
type WorkspaceRef struct {
	ID     string
	Handle string
}

// Service owns addresses, the Worker settings, and the envelope log.
// Long-running work (the poll loop) belongs to the daemon, not here.
type Service struct {
	st     *store.Store
	convs  *conversations.Service
	ob     *outbox.Service
	picker WorkspacePicker
	styles StyleSource
	render *Renderer
	log    *slog.Logger

	mu          sync.RWMutex
	settings    *Settings
	transport   Transport
	client      *Client // the bearer transport, when that is the one in use
	hosted      HostedMint
	state       PollState
	ingress     *Ingress // set by the daemon; manual polls reach it
	sendFailure string   // last permanent send failure; empty when healthy
}

// PollState is what the status route reports about the poll loop. The
// daemon worker updates it; it starts empty.
type PollState struct {
	Position  int64  `json:"position"`
	LastPoll  string `json:"last_poll,omitempty"`
	LastError string `json:"last_error,omitempty"`
	Failures  int    `json:"failures"` // consecutive failed polls; 0 when the loop is healthy
}

// New builds the service. The cloud plan gates hosted mail at transport
// selection, never here (plan 2026-09-22 Part C).
func New(st *store.Store) *Service {
	return &Service{
		st:     st,
		log:    slog.Default().With("source", "email"),
		render: NewRenderer(),
	}
}

// WithConversations installs the conversation reader reply-by-mail needs
// (plan I.2). Plain wiring: the daemon calls it once, before Start.
func (s *Service) WithConversations(c *conversations.Service) *Service {
	s.convs = c
	return s
}

// WithOutbox installs the outbox that carries every outbound mail (plan
// 2026-09-28 B.2). Plain wiring: the daemon calls it once, before Start.
func (s *Service) WithOutbox(ob *outbox.Service) *Service {
	s.ob = ob
	return s
}

// errNoOutbox is a send with no outbox wired.
var errNoOutbox = errors.New("email outbox is not wired")

// Configure installs the resolved settings and the transport that moves
// mail: the bearer Worker client or the hosted cloudlink client. Call it
// whenever the config reloads; nil clears the channel. A transport that
// also satisfies HostedMint mints addresses in the cloud instead of
// locally.
func (s *Service) Configure(cfg *Settings, tr Transport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg == nil {
		s.settings, s.transport, s.client, s.hosted = nil, nil, nil, nil
		return
	}
	s.settings = cfg
	s.transport = tr
	s.client, _ = tr.(*Client)
	s.hosted, _ = tr.(HostedMint)
}

// Configured reports whether the email block is present.
func (s *Service) Configured() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings != nil
}

// Client returns the bearer Worker client, or ErrNotConfigured. Only the
// bearer transport polls a spool; the hosted transport pushes.
func (s *Service) Client() (*Client, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil {
		return nil, ErrNotConfigured
	}
	return s.client, nil
}

// Transport returns the mail transport, or ErrNotConfigured.
func (s *Service) Transport() (Transport, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.transport == nil {
		return nil, ErrNotConfigured
	}
	return s.transport, nil
}

// Settings copies the resolved settings, or ErrNotConfigured.
func (s *Service) Settings() (Settings, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.settings == nil {
		return Settings{}, ErrNotConfigured
	}
	return *s.settings, nil
}

// SetPollState records loop facts for the status route.
func (s *Service) SetPollState(st PollState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
}

// Status is the one-call channel view. It works with the block absent.
type Status struct {
	Configured     bool     `json:"configured"`
	Worker         string   `json:"worker,omitempty"`
	Domain         string   `json:"domain,omitempty"`
	Admins         []string `json:"admins,omitempty"`
	PollIntervalMS int64    `json:"poll_interval_ms,omitempty"`
	RequireDMARC   bool     `json:"require_dmarc,omitempty"`
	Entitlement    string   `json:"entitlement"` // ok | missing
	Addresses      int      `json:"addresses"`
	WorkerHealth   *Health  `json:"worker_health,omitempty"`
	WorkerError    string   `json:"worker_error,omitempty"`
	Position       int64    `json:"position"`
	LastPoll       string   `json:"last_poll,omitempty"`
	LastError      string   `json:"last_error,omitempty"`
	Failures       int      `json:"failures"`
	SendError      string   `json:"send_error,omitempty"` // last permanent send failure
}

// Status collects settings, entitlement, address count, a live transport
// probe, and the poll loop facts. A probe failure is reported, not
// returned: the route answers 200 with worker_error set. Entitlement is
// "ok" here; the API reports "missing" when the channel is off because
// the cloud plan lacks hosted_ingress (plan 2026-09-22 Part D).
func (s *Service) Status(ctx context.Context) Status {
	s.mu.RLock()
	cfg, tr, state, sendFailure := s.settings, s.transport, s.state, s.sendFailure
	s.mu.RUnlock()

	out := Status{
		Configured:  cfg != nil,
		Entitlement: "ok",
		Position:    state.Position,
		LastPoll:    state.LastPoll,
		LastError:   state.LastError,
		Failures:    state.Failures,
		SendError:   sendFailure,
	}
	if cfg != nil {
		out.Worker = cfg.Worker
		out.Domain = cfg.Domain
		out.Admins = cfg.Admins
		out.PollIntervalMS = cfg.PollInterval.Milliseconds()
		out.RequireDMARC = cfg.RequireDMARC
	}
	if n, err := s.AddressCount(ctx); err == nil {
		out.Addresses = n
	} else {
		s.log.ErrorContext(ctx, "email.address_count", "err", err.Error())
	}
	if tr != nil {
		h, err := tr.Health(ctx)
		if err != nil {
			out.WorkerError = err.Error()
		} else {
			out.WorkerHealth = &h
		}
	}
	return out
}

// Test sends one fixed message to the admins and logs the envelope.
// It is the "does the pipe work" action for the route and the CLI. The row
// gets one attempt, made now.
func (s *Service) Test(ctx context.Context) (string, error) {
	cfg, err := s.Settings()
	if err != nil {
		return "", err
	}
	domain, err := s.domain(ctx)
	if err != nil {
		return "", err
	}
	msgID := MintMessageID(domain)
	const body = "This is a test message from trebi. The email channel works."
	parts, err := s.htmlParts(ctx, "", "", body, Page{Layout: LayoutMinimal, Title: "trebi email test"})
	if err != nil {
		return "", err
	}
	row, err := s.enqueue(ctx, envelope{
		messageID: msgID, recipients: cfg.Admins, subject: "trebi email test",
	}, outbox.Row{
		Purpose: outbox.PurposeTest, MaxAttempts: 1, Body: body, Parts: parts,
		Env: fromEnv("trebi@"+domain, cfg.FromName),
	})
	if err != nil {
		return "", err
	}
	row, err = s.ob.SendNow(ctx, row.ID)
	if err != nil {
		return "", err
	}
	if row.Status != outbox.StatusSent {
		return "", errors.New(cmp.Or(row.ErrorMessage, "email test did not send"))
	}
	return msgID, nil
}

// SendTest writes one test mail of a notify route to the given addresses,
// from the alert sender of the workspace. The row gets one attempt; the
// caller calls SendNow.
func (s *Service) SendTest(ctx context.Context, workspaceID string, to []string) (outbox.Row, error) {
	if len(to) == 0 {
		return outbox.Row{}, errors.New("test mail has no recipient")
	}
	cfg, err := s.Settings()
	if err != nil {
		return outbox.Row{}, err
	}
	dom, err := s.domain(ctx)
	if err != nil {
		return outbox.Row{}, err
	}
	const body = "This is a test message from trebi. This step of the notify route works."
	parts, err := s.htmlParts(ctx, workspaceID, "", body, Page{Layout: LayoutMinimal, Title: "trebi notify test"})
	if err != nil {
		return outbox.Row{}, err
	}
	return s.enqueue(ctx, envelope{
		workspaceID: workspaceID, messageID: MintMessageID(dom), recipients: to, subject: "trebi notify test",
	}, outbox.Row{
		Purpose: outbox.PurposeTest, MaxAttempts: 1, Body: body, Parts: parts,
		Env: fromEnv(s.alertFrom(ctx, workspaceID), cfg.FromName),
	})
}

// WorkspacePicker names the workspace that sends a system alert: the
// configured default, else the workspace with the most active jobs.
type WorkspacePicker interface {
	Default() string
	MostActive(ctx context.Context) string
}

// StyleSource reads the mail style of one sender: the workspace, or an
// agent of it when agent is set.
type StyleSource interface {
	EmailStyle(ctx context.Context, workspaceID, agent string) (job.StyleYAML, error)
}

// WithStyles sets the style seam. Nil keeps the house style for all mail.
func (s *Service) WithStyles(src StyleSource) *Service {
	s.styles = src
	return s
}

// WithWorkspacePicker sets the system sender seam. Nil keeps trebi@<domain>.
func (s *Service) WithWorkspacePicker(p WorkspacePicker) *Service {
	s.picker = p
	return s
}

// SystemWorkspace is the workspace id that sends an alert with no
// workspace, or "".
func (s *Service) SystemWorkspace(ctx context.Context) string {
	if s.picker == nil {
		return ""
	}
	return cmp.Or(s.picker.Default(), s.picker.MostActive(ctx))
}

// SendOpts is POST /v1/email/send (plan I.3): one explicit message from
// the workspace address, or the agent subaddress when Agent is set.
type SendOpts struct {
	WorkspaceID    string
	Agent          string
	To             []string
	Subject        string
	Text           string
	HTML           string
	Attachments    []SendAttachment
	InReplyTo      string
	RunID          string
	ConversationID string
	FromName       string
	Recipients     *AcceptRule
	Purpose        string
}

// sendParts is the outbox Parts value of an email row.
type sendParts struct {
	HTML        string           `json:"html,omitempty"`
	Attachments []SendAttachment `json:"attachments,omitempty"`
}

// parts checks the HTML part and the files against the send contract and
// encodes them for the outbox row. A file with no type gets one from its
// name. nil means a plain text mail.
func (o *SendOpts) parts() ([]byte, error) {
	if o.HTML == "" && len(o.Attachments) == 0 {
		return nil, nil
	}
	if len(o.Attachments) > MaxAttachments {
		return nil, fmt.Errorf("%w: at most %d files", ErrContent, MaxAttachments)
	}
	size := len(o.Text) + len(o.HTML)
	for i := range o.Attachments {
		a := &o.Attachments[i]
		if err := a.normalize(); err != nil {
			return nil, err
		}
		size += len(a.Content)
	}
	if size > MaxSendBytes {
		return nil, fmt.Errorf("%w: the message is %d bytes, the limit is %d", ErrContent, size, MaxSendBytes)
	}
	return json.Marshal(sendParts{HTML: o.HTML, Attachments: o.Attachments})
}

// Queued is the outcome of one enqueued send.
type Queued struct {
	MessageID string
	OutboxID  string
}

// Send enqueues one explicit message and returns the minted Message-ID and
// the outbox row id (plan I.3). Every recipient must be an operator or on
// the workspace allowlist. The envelope row and the outbox row are written
// in one transaction; the outbox worker sends.
func (s *Service) Send(ctx context.Context, opts SendOpts) (Queued, error) {
	cfg, err := s.Settings()
	if err != nil {
		return Queued{}, err
	}
	addr, err := s.Address(ctx, opts.WorkspaceID)
	if err != nil {
		return Queued{}, err
	}
	if err := s.checkRecipients(ctx, cfg, opts); err != nil {
		return Queued{}, err
	}
	switch {
	case strings.TrimSpace(opts.Text) != "" && strings.TrimSpace(opts.HTML) != "":
		return Queued{}, fmt.Errorf("%w: send text or html, not both", ErrContent)
	case strings.TrimSpace(opts.HTML) != "":
		// The text part is the HTML as text, so it never drifts from it.
		opts.Text = cmp.Or(PlainText(opts.HTML), opts.Subject)
	case strings.TrimSpace(opts.Text) == "":
		return Queued{}, fmt.Errorf("%w: text or html is required", ErrContent)
	default:
		// Text is Markdown: the HTML part is Text in the sender style.
		r, err := s.htmlPart(ctx, opts.WorkspaceID, opts.Agent, opts.Text, Page{
			Layout: LayoutReport, Title: opts.Subject, Brand: cmp.Or(opts.FromName, cfg.FromName),
		})
		if err != nil {
			return Queued{}, err
		}
		opts.HTML, opts.Attachments = r.attach(opts.Attachments)
	}
	parts, err := opts.parts()
	if err != nil {
		return Queued{}, err
	}
	from := addr.Full()
	if opts.Agent != "" {
		lp, err := tagAddress(addr, opts.Agent)
		if err != nil {
			return Queued{}, err
		}
		from = lp + "@" + addr.Domain
	}
	msgID := MintMessageID(addr.Domain)
	root := msgID
	if opts.InReplyTo != "" {
		root, _ = s.threadOnto(ctx, opts.InReplyTo, msgID)
	}
	purpose := outbox.PurposeSend
	if opts.Purpose == string(outbox.PurposeReport) {
		purpose = outbox.PurposeReport
	}
	row, err := s.enqueue(ctx, envelope{
		workspaceID: opts.WorkspaceID, agent: opts.Agent, messageID: msgID,
		inReplyTo: opts.InReplyTo, threadRoot: root, recipients: opts.To, subject: opts.Subject,
		conversationID: opts.ConversationID, runID: opts.RunID, purpose: opts.Purpose,
	}, outbox.Row{
		Purpose: purpose, Body: opts.Text, Parts: parts,
		Env: fromEnv(from, cmp.Or(opts.FromName, cfg.FromName)),
	})
	if err != nil {
		return Queued{}, err
	}
	return Queued{MessageID: msgID, OutboxID: row.ID}, nil
}

// AlertMail is the email step of an alert route.
type AlertMail struct {
	InboxID, Subject, Text string
	To                     []string
	RouteStep              int
	Route                  []notify.Step
}

// SendAlert writes the envelope, the inbox link, and the outbox row of one
// alert mail in one transaction, so a reply resolves to the item even if the
// process dies before the send. The subject is the thread key: a repeat
// alert with the same subject carries In-Reply-To. A repeat of the same
// inbox item and step returns the stored row. The caller may call SendNow.
func (s *Service) SendAlert(ctx context.Context, a AlertMail) (outbox.Row, string, error) {
	if len(a.To) == 0 {
		return outbox.Row{}, "", errors.New("alert mail has no recipient")
	}
	cfg, err := s.Settings()
	if err != nil {
		return outbox.Row{}, "", err
	}
	var ws sql.NullString
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT workspace_id FROM inbox_items WHERE id=?`, a.InboxID).Scan(&ws); err != nil {
		return outbox.Row{}, "", fmt.Errorf("alert inbox item %s: %w", a.InboxID, err)
	}
	workspaceID := ws.String
	prev, err := s.lastSentOnThread(ctx, workspaceID, a.Subject)
	if err != nil {
		return outbox.Row{}, "", err
	}
	dom, err := s.domain(ctx)
	if err != nil {
		return outbox.Row{}, "", err
	}
	msgID := MintMessageID(dom)
	root, inReplyTo := msgID, ""
	if prev.messageID != "" {
		root, inReplyTo = prev.threadRoot, prev.messageID
	}
	step := a.RouteStep
	parts, err := s.htmlParts(ctx, workspaceID, "", a.Text, Page{Layout: LayoutMinimal, Title: a.Subject})
	if err != nil {
		return outbox.Row{}, "", err
	}
	row, err := s.enqueue(ctx, envelope{
		workspaceID: workspaceID, messageID: msgID, inReplyTo: inReplyTo, threadRoot: root,
		recipients: a.To, subject: a.Subject, inboxID: a.InboxID,
	}, outbox.Row{
		Purpose: outbox.PurposeAlert, Body: a.Text, Parts: parts, InboxID: a.InboxID,
		DedupeKey: fmt.Sprintf("alert/%s/%d", a.InboxID, step), RouteStep: &step, Route: a.Route,
		Env: fromEnv(s.alertFrom(ctx, workspaceID), cfg.FromName),
	})
	return row, msgID, err
}

// envelope is the email_messages row of one outbound mail.
type envelope struct {
	workspaceID, agent, messageID, inReplyTo, threadRoot string
	recipients                                           []string
	subject, conversationID, runID, inboxID, purpose     string
	coveredTurn                                          *int
}

// Keys of the From header fields on an email outbox row.
const (
	envFrom     = "from"
	envFromName = "from_name"
)

// Outbound dispositions. The outbox row holds the in-flight state.
const (
	dispositionQueued     = "queued"
	dispositionSent       = "sent"
	dispositionSendFailed = "send_failed"
)

// Outbound is the outbox Sender of the email Kind. Service.Deliver is
// already the inbound path, so the outbound methods live on this type.
func (s *Service) Outbound() outbox.Sender { return outbound{s} }

type outbound struct{ s *Service }

// Deliver sends one email outbox row: one Transport.Send, with the Worker's
// retry_after_ms honoured inside the attempt. A refused recipient is
// permanent. The outbox retries every other failure.
func (o outbound) Deliver(ctx context.Context, r outbox.Row) error {
	s := o.s
	tr, err := s.Transport()
	if err != nil {
		return err
	}
	var messageID, inReplyTo, root string
	if err := s.st.DB().QueryRowContext(ctx, `
SELECT COALESCE(message_id,''), COALESCE(in_reply_to,''), COALESCE(thread_root,'')
FROM email_messages WHERE id=?`, r.EmailRowID).Scan(&messageID, &inReplyTo, &root); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return outbox.Permanent{Err: fmt.Errorf("email envelope %s is gone", r.EmailRowID)}
		}
		return fmt.Errorf("email envelope %s: %w", r.EmailRowID, err)
	}
	var refs []string
	if root != "" && root != messageID {
		refs = append(refs, root)
	}
	if inReplyTo != "" && inReplyTo != root {
		refs = append(refs, inReplyTo)
	}
	var parts sendParts
	if len(r.Parts) > 0 {
		if err := json.Unmarshal(r.Parts, &parts); err != nil {
			return outbox.Permanent{Err: fmt.Errorf("email parts of %s: %w", r.ID, err)}
		}
	}
	req := SendRequest{
		V: ContractV, From: r.Env[envFrom], FromName: r.Env[envFromName],
		To: splitRecipients(r.Recipients), Subject: r.Subject, Text: r.Body,
		HTML: parts.HTML, Attachments: parts.Attachments,
		MessageID: messageID, InReplyTo: inReplyTo, References: refs,
	}
	if err := s.sendWithBackoff(ctx, tr, req); err != nil {
		s.noteSendFailure(err)
		if errors.Is(err, ErrRecipient) {
			return outbox.Permanent{Err: err}
		}
		return err
	}
	s.noteSendFailure(nil)
	return nil
}

// Changed keeps the envelope disposition in step with its outbox row, and
// publishes email.sent when the row is sent.
func (o outbound) Changed(ctx context.Context, r outbox.Row) {
	s := o.s
	if r.EmailRowID == "" {
		return
	}
	disposition := dispositionQueued
	switch r.Status {
	case outbox.StatusSent:
		disposition = dispositionSent
	case outbox.StatusFailed, outbox.StatusCanceled:
		disposition = dispositionSendFailed
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE email_messages SET disposition=?, revision=revision+1 WHERE id=?`,
		disposition, r.EmailRowID); err != nil {
		s.log.ErrorContext(ctx, "email disposition", "row", r.EmailRowID, "error", err.Error())
		return
	}
	if r.Status == outbox.StatusSent {
		var msgID string
		// A missing row leaves the frame without a message id.
		_ = s.st.DB().QueryRowContext(ctx,
			`SELECT COALESCE(message_id,'') FROM email_messages WHERE id=?`, r.EmailRowID).Scan(&msgID)
		s.appendSent(ctx, r.EmailRowID, msgID)
	}
}

var _ outbox.Watcher = outbound{}

// MintMessageID returns an RFC Message-ID trebid controls, so the
// envelope is storable before the send.
func MintMessageID(domain string) string {
	return "<" + ulid.Make().String() + "@" + domain + ">"
}

// Address returns one workspace's stored address.
func (s *Service) Address(ctx context.Context, workspaceID string) (Address, error) {
	row := s.st.DB().QueryRowContext(ctx,
		`SELECT workspace_id, local_part, domain, created_at, rotated_at, revision
		 FROM email_addresses WHERE workspace_id = ?`, workspaceID)
	return scanAddress(row.Scan)
}

// Masked returns the address with the token hidden.
func (s *Service) Masked(ctx context.Context, workspaceID string) (Address, error) {
	a, err := s.Address(ctx, workspaceID)
	if err != nil {
		return Address{}, err
	}
	a.LocalPart = maskLocalPart(a.LocalPart)
	return a, nil
}

// AgentAddress returns the agent's masked subaddress, or "" when the
// workspace has no address yet.
func (s *Service) AgentAddress(ctx context.Context, workspaceID, agent string) (string, error) {
	a, err := s.AgentAddressMasked(ctx, workspaceID, agent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return a.Full(), nil
}

// AgentAddressFull returns the agent's subaddress with the token visible,
// as a full Address row so one payload builder serves the masked and
// reveal shapes (plan 2026-09-24 B.3). sql.ErrNoRows when the workspace
// has no address yet.
func (s *Service) AgentAddressFull(ctx context.Context, workspaceID, agent string) (Address, error) {
	a, err := s.Address(ctx, workspaceID)
	if err != nil {
		return Address{}, err
	}
	lp, err := tagAddress(a, agent)
	if err != nil {
		return Address{}, err
	}
	a.LocalPart = lp
	return a, nil
}

// AgentAddressMasked is the reveal-safe view of AgentAddressFull: the tag
// stays readable and the token is hidden.
func (s *Service) AgentAddressMasked(ctx context.Context, workspaceID, agent string) (Address, error) {
	a, err := s.AgentAddressFull(ctx, workspaceID, agent)
	if err != nil {
		return Address{}, err
	}
	a.LocalPart = maskLocalPart(a.LocalPart)
	return a, nil
}

// EnsureAddress mints the workspace address once: <handle>-<token>. The
// hosted transport mints in the cloud instead, and the row keeps the
// address the worker assigned.
func (s *Service) EnsureAddress(ctx context.Context, workspaceID, handle string) (Address, error) {
	if a, err := s.Address(ctx, workspaceID); err == nil {
		return a, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Address{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for range 5 {
		lp, domain, hosted, err := s.mintCandidate(ctx, workspaceID, handle)
		if err != nil {
			return Address{}, err
		}
		_, err = s.st.DB().ExecContext(ctx, `
INSERT INTO email_addresses (workspace_id, local_part, domain, created_at)
VALUES (?,?,?,?) ON CONFLICT(workspace_id) DO NOTHING`,
			workspaceID, lp, domain, now)
		if err == nil {
			return s.Address(ctx, workspaceID)
		}
		if isUniqueViolation(err) && !hosted {
			continue // local-part collision with another workspace; re-mint
		}
		return Address{}, fmt.Errorf("mint address for %s: %w", workspaceID, err)
	}
	return Address{}, fmt.Errorf("mint address for %s: no free token", workspaceID)
}

// Rotate mints a new address with the given handle and a new token. The
// old local part is retired from the Worker known set on the next ack.
// The hosted transport replaces the hosted address instead.
func (s *Service) Rotate(ctx context.Context, workspaceID, handle string) (Address, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	for range 5 {
		lp, domain, hosted, err := s.mintCandidate(ctx, workspaceID, handle)
		if err != nil {
			return Address{}, err
		}
		_, err = s.st.DB().ExecContext(ctx, `
INSERT INTO email_addresses (workspace_id, local_part, domain, created_at, rotated_at, revision)
VALUES (?,?,?,?,?,2)
ON CONFLICT(workspace_id) DO UPDATE SET
  local_part=excluded.local_part, domain=excluded.domain,
  rotated_at=excluded.rotated_at, revision=email_addresses.revision+1`,
			workspaceID, lp, domain, now, now)
		if err == nil {
			return s.Address(ctx, workspaceID)
		}
		if isUniqueViolation(err) && !hosted {
			continue
		}
		return Address{}, fmt.Errorf("rotate address for %s: %w", workspaceID, err)
	}
	return Address{}, fmt.Errorf("rotate address for %s: no free token", workspaceID)
}

// SyncAddresses mints for every registered workspace that lacks one.
// The daemon calls it when the channel is configured at start.
func (s *Service) SyncAddresses(ctx context.Context, refs []WorkspaceRef) error {
	if !s.Configured() {
		return nil
	}
	for _, ref := range refs {
		if _, err := s.EnsureAddress(ctx, ref.ID, ref.Handle); err != nil {
			s.log.ErrorContext(ctx, "email.sync_address", "workspace_id", ref.ID, "err", err.Error())
		}
	}
	return nil
}

// Attachment fetches one attachment's bytes and content type from the
// transport. The key is "<messageID>/<attachmentID>".
func (s *Service) Attachment(ctx context.Context, key string) ([]byte, string, error) {
	tr, err := s.Transport()
	if err != nil {
		return nil, "", err
	}
	return tr.Attachment(ctx, key)
}

// KnownLocalParts lists stored bases for the Worker known-set filter.
// Phase 4 sends it with every ack.
func (s *Service) KnownLocalParts(ctx context.Context) ([]string, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT local_part FROM email_addresses ORDER BY local_part`)
	if err != nil {
		return nil, fmt.Errorf("known local parts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var lp string
		if err := rows.Scan(&lp); err != nil {
			return nil, err
		}
		out = append(out, lp)
	}
	return out, rows.Err()
}

// AddressCount counts stored addresses.
func (s *Service) AddressCount(ctx context.Context) (int, error) {
	var n int
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM email_addresses`).Scan(&n); err != nil {
		return 0, fmt.Errorf("address count: %w", err)
	}
	return n, nil
}

// Message is one envelope-log row (plan G), the wire shape for the log
// routes. Both directions share it; absent columns are omitted. Only
// GetMessage fills Content, and only while the content prune keeps it.
type Message struct {
	ID             string       `json:"id"`
	Direction      string       `json:"direction"` // in | out
	WorkspaceID    string       `json:"workspace_id,omitempty"`
	Agent          string       `json:"agent,omitempty"`
	MessageID      string       `json:"message_id,omitempty"`
	InReplyTo      string       `json:"in_reply_to,omitempty"`
	ThreadRoot     string       `json:"thread_root,omitempty"`
	Sender         string       `json:"sender,omitempty"`
	Recipients     string       `json:"recipients,omitempty"`
	Subject        string       `json:"subject,omitempty"`
	DMARC          string       `json:"dmarc,omitempty"`
	Disposition    string       `json:"disposition,omitempty"`
	InboxID        string       `json:"inbox_id,omitempty"`
	RunID          string       `json:"run_id,omitempty"`
	ConversationID string       `json:"conversation_id,omitempty"`
	Attachments    []Attachment `json:"attachments"`
	TraceID        string       `json:"trace_id,omitempty"`
	CreatedAt      string       `json:"created_at"`
	Revision       int64        `json:"revision"`
	// Outbound delivery, from the outbox row (plan 2026-09-28 B.3).
	OutboxID      string `json:"outbox_id,omitempty"`
	Status        string `json:"status,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
	NextAttemptAt string `json:"next_attempt_at,omitempty"`
	Error         string `json:"error,omitempty"`
	// Content is nil when the daemon does not keep the text.
	Content *Content `json:"content,omitempty"`
}

// Content is the kept text of one message. Inbound HTML is sanitized
// when it is read; outbound HTML is the daemon's own render.
type Content struct {
	Text string `json:"text"`
	HTML string `json:"html,omitempty"`
}

// PruneContent clears message text older than keep. A zero keep clears
// all of it. The envelope rows stay.
func (s *Service) PruneContent(ctx context.Context, now time.Time, keep time.Duration) (int64, error) {
	cutoff := now.Add(-keep).UTC().Format(time.RFC3339)
	res, err := s.st.DB().ExecContext(ctx, `UPDATE email_messages SET body_text=NULL, body_html=NULL
WHERE created_at < ? AND (body_text IS NOT NULL OR body_html IS NOT NULL)`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("email content prune: %w", err)
	}
	return res.RowsAffected()
}

// MessageListOpts filters the envelope log (plan K). Empty filters match
// every row; Cursor is a row id, so pages walk strictly older.
type MessageListOpts struct {
	Direction string // "" | in | out
	Workspace string
	Thread    string // matches thread_root
	Limit     int
	Cursor    string
}

// Null columns map to "" so one Scan serves both directions: outbound rows
// carry recipients and no sender, inbound rows the reverse.
const messageCols = `COALESCE(em.workspace_id,''), COALESCE(em.agent,''), COALESCE(em.message_id,''),
COALESCE(em.in_reply_to,''), COALESCE(em.thread_root,''), COALESCE(em.sender,''), COALESCE(em.recipients,''),
COALESCE(em.subject,''), COALESCE(em.dmarc,''), COALESCE(em.disposition,''), COALESCE(em.inbox_id,''),
COALESCE(em.run_id,''), COALESCE(em.conversation_id,''), COALESCE(em.attachments,'[]'),
COALESCE(em.trace_id,''), em.created_at, em.revision, COALESCE(em.outbox_id,''), COALESCE(o.status,''),
COALESCE(o.attempts,0), COALESCE(o.next_attempt_at,''), COALESCE(o.error_message,'')`

// messageFrom joins each envelope to its outbox row.
const messageFrom = ` FROM email_messages em LEFT JOIN outbox o ON o.id = em.outbox_id`

// ListMessages reads the envelope log newest-first. The row id is a ULID,
// so it doubles as the cursor.
func (s *Service) ListMessages(ctx context.Context, opts MessageListOpts) ([]Message, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	q := `SELECT em.id, em.direction, ` + messageCols + messageFrom + ` WHERE 1=1`
	var args []any
	if opts.Direction != "" {
		q += ` AND em.direction=?`
		args = append(args, opts.Direction)
	}
	if opts.Workspace != "" {
		q += ` AND em.workspace_id=?`
		args = append(args, opts.Workspace)
	}
	if opts.Thread != "" {
		q += ` AND em.thread_root=?`
		args = append(args, opts.Thread)
	}
	// Inbound ids come from the Worker in lowercase; outbound ids are minted
	// in uppercase. Compare without case so both sort by time.
	if opts.Cursor != "" {
		q += ` AND upper(em.id) < upper(?)`
		args = append(args, opts.Cursor)
	}
	q += ` ORDER BY upper(em.id) DESC LIMIT ?`
	args = append(args, opts.Limit)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list email messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list email messages: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMessage returns one envelope row with its kept content, or
// sql.ErrNoRows.
func (s *Service) GetMessage(ctx context.Context, id string) (Message, error) {
	var text, html sql.NullString
	row := s.st.DB().QueryRowContext(ctx,
		`SELECT em.id, em.direction, `+messageCols+`, em.body_text, em.body_html`+messageFrom+` WHERE em.id=?`, id)
	m, err := scanMessage(func(dest ...any) error {
		return row.Scan(append(dest, &text, &html)...)
	})
	if err != nil {
		return Message{}, err
	}
	if text.Valid || html.Valid {
		m.Content = &Content{Text: text.String, HTML: html.String}
		if m.Direction == "in" && m.Content.HTML != "" {
			m.Content.HTML = SanitizeHTML(m.Content.HTML)
		}
	}
	return m, nil
}

func scanMessage(scan func(dest ...any) error) (Message, error) {
	var m Message
	var att string
	if err := scan(&m.ID, &m.Direction, &m.WorkspaceID, &m.Agent, &m.MessageID,
		&m.InReplyTo, &m.ThreadRoot, &m.Sender, &m.Recipients, &m.Subject,
		&m.DMARC, &m.Disposition, &m.InboxID, &m.RunID, &m.ConversationID,
		&att, &m.TraceID, &m.CreatedAt, &m.Revision, &m.OutboxID, &m.Status, &m.Attempts,
		&m.NextAttemptAt, &m.Error); err != nil {
		return Message{}, err
	}
	m.Attachments = []Attachment{}
	if att != "" {
		// The column is always json.Marshal output; a failed parse degrades to none.
		_ = json.Unmarshal([]byte(att), &m.Attachments)
	}
	return m, nil
}

// ConnectorRow is the synthetic connectors-list row for the email channel
// (plan K). It appears only when the block is configured and carries only
// cached loop facts: the list path never probes the Worker. The row
// supports restart (a forced poll) and nothing else.
func (s *Service) ConnectorRow() map[string]any {
	if !s.Configured() {
		return nil
	}
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()
	status := "listening"
	if state.Failures > 0 {
		status = "crashed"
	}
	metrics := map[string]any{"position": state.Position, "failures": state.Failures}
	if state.LastPoll != "" {
		metrics["last_poll"] = state.LastPoll
	}
	if state.LastError != "" {
		metrics["last_error"] = state.LastError
	}
	return map[string]any{
		"id": "email", "type": "email", "slug": "email", "title": "Email", "status": status,
		"provenance": "native", "managed": false,
		"capabilities": []string{"events", "channel"},
		"events": map[string]any{
			"status":  status,
			"metrics": metrics,
		},
	}
}

// alertFrom picks the sender for one alert. A workspace address makes the
// alert answerable: the local part is in the known set, so the operator's
// reply reaches the spool and resolves by Message-ID (plan I.1). An alert
// with no workspace uses the system workspace. The bare instance sender is
// the last fallback; mail to it never comes back.
func (s *Service) alertFrom(ctx context.Context, workspaceID string) string {
	if workspaceID == "" {
		workspaceID = s.SystemWorkspace(ctx)
	}
	if workspaceID != "" {
		if addr, err := s.Address(ctx, workspaceID); err == nil {
			return addr.Full()
		} else if !errors.Is(err, sql.ErrNoRows) {
			s.log.WarnContext(ctx, "alert from address", "workspace", workspaceID, "error", err)
		}
	}
	dom, err := s.domain(ctx)
	if err != nil {
		return ""
	}
	return "trebi@" + dom
}

// RouteEnvFor exposes the resolved routing rules for explicit sends. It is
// available only when ingress actions are bound; a missing binding closes the
// agent and assistant rules.
func (s *Service) RouteEnvFor(ctx context.Context, workspaceID string) (RouteEnv, error) {
	if s.ingress == nil {
		return RouteEnv{}, ErrNotConfigured
	}
	return s.ingress.act.RouteEnvFor(ctx, workspaceID)
}

// checkRecipients applies the v1 trust dial (plan I.3): every recipient
// must be an operator or on the workspace allowlist. The allowlist comes
// from the routing environment; without an ingress only admins pass.
func (s *Service) checkRecipients(ctx context.Context, cfg Settings, opts SendOpts) error {
	if opts.Recipients != nil {
		for _, to := range opts.To {
			if !opts.Recipients.Accepts(to, cfg.Admins, s.workspaceAllowlist(ctx, opts.WorkspaceID)) {
				return fmt.Errorf("%w: %s", ErrRecipient, to)
			}
		}
		return nil
	}
	allowed := map[string]bool{}
	for _, o := range append([]string(nil), cfg.Admins...) {
		allowed[strings.ToLower(strings.TrimSpace(o))] = true
	}
	if s.ingress != nil {
		if env, err := s.ingress.act.RouteEnvFor(ctx, opts.WorkspaceID); err == nil {
			for _, a := range env.Allowlist {
				allowed[strings.ToLower(strings.TrimSpace(a))] = true
			}
		}
	}
	for _, to := range opts.To {
		if !allowed[strings.ToLower(strings.TrimSpace(to))] {
			return fmt.Errorf("%w: %s", ErrRecipient, to)
		}
	}
	return nil
}

func (s *Service) workspaceAllowlist(ctx context.Context, workspaceID string) []string {
	if s.ingress == nil {
		return nil
	}
	env, err := s.ingress.act.RouteEnvFor(ctx, workspaceID)
	if err != nil {
		return nil
	}
	return env.Allowlist
}

// threadOnto resolves the thread an explicit reply joins: the root of the
// row its In-Reply-To names, with both ids as References. An unknown id
// starts a fresh thread anchored on the new message.
func (s *Service) threadOnto(ctx context.Context, inReplyTo, msgID string) (string, []string) {
	var root string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT COALESCE(thread_root,'') FROM email_messages WHERE message_id=? ORDER BY id DESC LIMIT 1`,
		inReplyTo).Scan(&root)
	if err != nil || root == "" {
		return msgID, []string{inReplyTo}
	}
	return root, []string{root, inReplyTo}
}

// Send backoff bounds (plan 2026-09-19 Part I.1): honour retry_after_ms,
// but never let one alert stall the delivery loop for long.
const (
	maxSendAttempts = 3
	maxSendBackoff  = 30 * time.Second
)

// sendWithBackoff honours the Worker's retry_after_ms: up to three
// attempts, each delayed by the asked time, capped. Any other error is
// permanent for the caller.
func (s *Service) sendWithBackoff(ctx context.Context, tr Transport, req SendRequest) error {
	var re *RetryAfterError
	for attempt := 0; ; attempt++ {
		_, err := tr.Send(ctx, req)
		if err == nil {
			return nil
		}
		if !errors.As(err, &re) || attempt >= maxSendAttempts-1 {
			return err
		}
		wait := min(re.After, maxSendBackoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// noteSendFailure keeps the last permanent send failure for the doctor.
// nil clears it.
func (s *Service) noteSendFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.sendFailure = ""
		return
	}
	s.sendFailure = err.Error()
}

type threadPrev struct {
	messageID  string
	threadRoot string
}

// alertThreadWindow bounds how long a repeat alert threads onto the
// previous one. Past it, the same fault starts a fresh thread rather than
// appending to one the operator has long since filed away.
const alertThreadWindow = 30 * 24 * time.Hour

// lastSentOnThread finds the newest delivered outbound message with this
// subject, in this workspace, inside the thread window. Only
// sent and queued rows thread: a failed send never reached the operator's
// client. The workspace scope keeps two workspaces with an
// identically titled fault on separate threads. Rows order by id (a minted
// ULID), not created_at: two sends in one second must still pick the later
// row.
func (s *Service) lastSentOnThread(ctx context.Context, workspaceID, subject string) (threadPrev, error) {
	var p threadPrev
	since := time.Now().UTC().Add(-alertThreadWindow).Format(time.RFC3339)
	err := s.st.DB().QueryRowContext(ctx, `
SELECT message_id, thread_root FROM email_messages
WHERE direction='out' AND subject=? AND disposition IN ('sent','queued')
  AND workspace_id=? AND created_at >= ?
ORDER BY id DESC LIMIT 1`, subject, workspaceID, since).Scan(&p.messageID, &p.threadRoot)
	if errors.Is(err, sql.ErrNoRows) {
		return threadPrev{}, nil
	}
	return p, err
}

// appendSent publishes the outbound bus frame. The frame stays thin; the
// email log refetches the table.
func (s *Service) appendSent(ctx context.Context, rowID, msgID string) {
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "email.sent", Entity: "email", EntityID: rowID, Action: "sent",
		Attrs: map[string]string{"message_id": msgID},
	}); err != nil {
		s.log.Warn("bus append", "topic", "email.sent", "error", err)
	}
}

// mintCandidate produces one candidate local part and domain. The hosted
// transport calls MintAddress on the cloud, where a second mint for one
// workspace replaces the address; the bearer transport mints locally with
// crypto/rand and the handle. hosted reports which path ran.
func (s *Service) mintCandidate(ctx context.Context, workspaceID, handle string) (string, string, bool, error) {
	s.mu.RLock()
	hosted := s.hosted
	s.mu.RUnlock()
	if hosted != nil {
		full, err := hosted.MintAddress(ctx, workspaceID, handle)
		if err != nil {
			return "", "", true, fmt.Errorf("mint hosted address for %s: %w", workspaceID, err)
		}
		lp, domain, ok := strings.Cut(full, "@")
		if !ok || lp == "" || domain == "" {
			return "", "", true, fmt.Errorf("mint hosted address for %s: unexpected address %q", workspaceID, full)
		}
		return lp, domain, true, nil
	}
	token, err := mintToken()
	if err != nil {
		return "", "", false, err
	}
	lp, err := localPart(handle, token, "")
	if err != nil {
		return "", "", false, err
	}
	domain, err := s.domain(ctx)
	if err != nil {
		return "", "", false, err
	}
	return lp, domain, false, nil
}

// domain returns the mail domain: the configured one, else the domain of
// any stored address. The hosted transport learns its domain from the
// address the worker mints, so the config block needs none.
func (s *Service) domain(ctx context.Context) (string, error) {
	cfg, err := s.Settings()
	if err != nil {
		return "", err
	}
	if cfg.Domain != "" {
		return cfg.Domain, nil
	}
	var d string
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT domain FROM email_addresses WHERE domain <> '' LIMIT 1`).Scan(&d); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return d, nil
}

func scanAddress(scan func(dest ...any) error) (Address, error) {
	var a Address
	var rotated sql.NullString
	if err := scan(&a.WorkspaceID, &a.LocalPart, &a.Domain, &a.CreatedAt, &rotated, &a.Revision); err != nil {
		return Address{}, err
	}
	a.RotatedAt = rotated.String
	return a, nil
}

// isUniqueViolation reports a local_part unique conflict. Error text is
// matched because SQLite and Postgres spell constraints differently.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "email_addresses.local_part") ||
		strings.Contains(msg, "duplicate key")
}

func fromEnv(from, name string) map[string]string {
	return map[string]string{envFrom: from, envFromName: name}
}

// enqueue writes the envelope row and its outbox row in one transaction.
// An alert envelope also links its inbox item. A dedupe hit writes nothing
// and returns the stored row.
func (s *Service) enqueue(ctx context.Context, e envelope, r outbox.Row) (outbox.Row, error) {
	if s.ob == nil {
		return outbox.Row{}, errNoOutbox
	}
	if e.threadRoot == "" {
		e.threadRoot = e.messageID
	}
	r.Kind = outbox.KindEmail
	r.WorkspaceID = e.workspaceID
	r.Instance, r.Room, r.Thread = "email", MailboxRoom(e.workspaceID, e.agent), e.threadRoot
	r.Recipients = strings.Join(e.recipients, ",")
	r.Subject = e.subject
	r.ConversationID, r.RunID = e.conversationID, e.runID
	r.EmailRowID = ulid.Make().String()
	var parts sendParts
	if len(r.Parts) > 0 {
		// Parts is always json.Marshal output; a failed parse keeps the text only.
		_ = json.Unmarshal(r.Parts, &parts)
	}
	text, html := s.keptContent(r.Body, parts.HTML)
	var turn any
	if e.coveredTurn != nil {
		turn = *e.coveredTurn
	}
	row, err := s.ob.EnqueueTx(ctx, func(tx *sql.Tx, r outbox.Row) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO email_messages
  (id, direction, workspace_id, agent, message_id, in_reply_to, thread_root, recipients,
   subject, disposition, conversation_id, run_id, inbox_id, purpose, covered_turn, outbox_id, created_at,
   body_text, body_html)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.EmailRowID, "out", e.workspaceID, e.agent, e.messageID, e.inReplyTo, e.threadRoot,
			r.Recipients, e.subject, dispositionQueued, store.NullStr(e.conversationID),
			store.NullStr(e.runID), store.NullStr(e.inboxID), e.purpose, turn, r.ID,
			time.Now().UTC().Format(time.RFC3339), text, html); err != nil {
			return fmt.Errorf("email envelope: %w", err)
		}
		if e.inboxID != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE inbox_items SET email_message_id=? WHERE id=?`, e.messageID, e.inboxID); err != nil {
				return fmt.Errorf("alert inbox link: %w", err)
			}
		}
		return nil
	}, r)
	if err == nil {
		s.touchThread(ctx, r.Room, r.Thread, e.subject, e.recipients)
	}
	return row, err
}

func splitRecipients(target string) []string {
	var out []string
	for _, a := range strings.Split(target, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// htmlPart renders text in the style of the sender. A failed style lookup
// keeps the house style.
func (s *Service) htmlPart(ctx context.Context, workspaceID, agent, text string, p Page) (Rendered, error) {
	p.Theme = DefaultTheme()
	if s.styles != nil && workspaceID != "" {
		st, err := s.styles.EmailStyle(ctx, workspaceID, agent)
		if err != nil {
			s.log.WarnContext(ctx, "email style", "workspace", workspaceID, "agent", agent, "err", err)
		}
		p.Theme, p.Logo = p.Theme.With(st), st.Logo
		if st.Footer != "" && p.Footer != "" {
			p.Footer = st.Footer + " · " + p.Footer
		} else {
			p.Footer = cmp.Or(p.Footer, st.Footer)
		}
	}
	return s.render.Markdown(text, p)
}

// htmlParts is htmlPart as the outbox Parts value of a mail with no files.
func (s *Service) htmlParts(ctx context.Context, workspaceID, agent, text string, p Page) ([]byte, error) {
	r, err := s.htmlPart(ctx, workspaceID, agent, text, p)
	if err != nil {
		return nil, err
	}
	h, files := r.attach(nil)
	return json.Marshal(sendParts{HTML: h, Attachments: files})
}

// keptContent returns the body columns of a new row: NULL when the
// daemon does not keep content.
func (s *Service) keptContent(text, html string) (any, any) {
	cfg, err := s.Settings()
	if err != nil || cfg.ContentRetention <= 0 {
		return nil, nil
	}
	return text, store.NullStr(html)
}
