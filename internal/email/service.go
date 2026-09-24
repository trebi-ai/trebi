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
	"github.com/trebi-ai/trebi/internal/store"
)

// ErrNotConfigured means the email block is absent: nothing to act on.
var ErrNotConfigured = errors.New("email is not configured")

// ErrRecipient means one send recipient is neither an operator nor on the
// workspace allowlist (plan I.3: an agent cannot mail a third party).
var ErrRecipient = errors.New("email recipient is not an operator or allowlisted address")

// WorkspaceRef is the (id, name) pair address minting needs. The caller
// owns the workspace list; email never reads the workspaces table.
type WorkspaceRef struct {
	ID   string
	Name string
}

// Service owns addresses, the Worker settings, and the envelope log.
// Long-running work (the poll loop) belongs to the daemon, not here.
type Service struct {
	st    *store.Store
	convs *conversations.Service
	log   *slog.Logger

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
		st:  st,
		log: slog.Default().With("source", "email"),
	}
}

// WithConversations installs the conversation reader reply-by-mail needs
// (plan I.2). Plain wiring: the daemon calls it once, before Start.
func (s *Service) WithConversations(c *conversations.Service) *Service {
	s.convs = c
	return s
}

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
	Operators      []string `json:"operators,omitempty"`
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
		out.Operators = cfg.Operators
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

// Test sends one fixed message to the operators and logs the envelope.
// It is the "does the pipe work" action for the route and the CLI.
func (s *Service) Test(ctx context.Context) (string, error) {
	cfg, err := s.Settings()
	if err != nil {
		return "", err
	}
	tr, err := s.Transport()
	if err != nil {
		return "", err
	}
	domain, err := s.domain(ctx)
	if err != nil {
		return "", err
	}
	msgID := MintMessageID(domain)
	req := SendRequest{
		V:         ContractV,
		From:      "trebi@" + domain,
		FromName:  cfg.FromName,
		To:        cfg.Operators,
		Subject:   "trebi email test",
		Text:      "This is a test message from trebi. The email channel works.",
		MessageID: msgID,
	}
	var sentErr error
	if _, err := tr.Send(ctx, req); err != nil {
		sentErr = err
	}
	rowID, err := s.logTestEnvelope(ctx, cfg, msgID, sentErr)
	if sentErr != nil {
		return "", sentErr
	}
	if err != nil {
		return "", err // mail went out; the envelope write failed
	}
	s.appendSent(ctx, rowID, msgID)
	return msgID, nil
}

// SendOpts is POST /v1/email/send (plan I.3): one explicit message from
// the workspace address, or the agent subaddress when Agent is set.
type SendOpts struct {
	WorkspaceID    string
	Agent          string
	To             []string
	Subject        string
	Text           string
	InReplyTo      string
	RunID          string
	ConversationID string
	FromName       string
	Recipients     *AcceptRule
	Purpose        string
}

// Send mails one explicit message and returns the minted Message-ID
// (plan I.3). Every recipient must be an operator or on the workspace
// allowlist. The envelope row is written before the send, so the log
// holds the attempt even if the process dies mid-send.
func (s *Service) Send(ctx context.Context, opts SendOpts) (string, error) {
	cfg, err := s.Settings()
	if err != nil {
		return "", err
	}
	tr, err := s.Transport()
	if err != nil {
		return "", err
	}
	addr, err := s.Address(ctx, opts.WorkspaceID)
	if err != nil {
		return "", err
	}
	if err := s.checkRecipients(ctx, cfg, opts); err != nil {
		return "", err
	}
	from := addr.Full()
	if opts.Agent != "" {
		lp, err := tagAddress(addr, opts.Agent)
		if err != nil {
			return "", err
		}
		from = lp + "@" + addr.Domain
	}
	msgID := MintMessageID(addr.Domain)
	root, refs := msgID, []string(nil)
	if opts.InReplyTo != "" {
		root, refs = s.threadOnto(ctx, opts.InReplyTo, msgID)
	}
	rowID := ulid.Make().String()
	if _, err := s.st.DB().ExecContext(ctx, `
 INSERT INTO email_messages
  (id, direction, workspace_id, agent, message_id, in_reply_to, thread_root, recipients,
   subject, disposition, conversation_id, run_id, purpose, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rowID, "out", opts.WorkspaceID, opts.Agent, msgID,
		opts.InReplyTo, root,
		strings.Join(opts.To, ","), opts.Subject, "sent", store.NullStr(opts.ConversationID), store.NullStr(opts.RunID), opts.Purpose,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		return "", fmt.Errorf("send envelope: %w", err)
	}
	req := SendRequest{
		V: ContractV, From: from, FromName: cmp.Or(opts.FromName, cfg.FromName),
		To: opts.To, Subject: opts.Subject, Text: opts.Text,
		MessageID: msgID, InReplyTo: opts.InReplyTo, References: refs,
	}
	if err := s.sendWithBackoff(ctx, tr, req); err != nil {
		s.noteSendFailure(err)
		if _, uerr := s.st.DB().ExecContext(ctx,
			`UPDATE email_messages SET disposition='send_failed' WHERE id=?`, rowID); uerr != nil {
			s.log.ErrorContext(ctx, "email send_failed mark", "row", rowID, "error", uerr.Error())
		}
		return msgID, err
	}
	s.noteSendFailure(nil)
	s.appendSent(ctx, rowID, msgID)
	return msgID, nil
}

// SendAlert mails one alert on its thread and returns the minted
// Message-ID. The envelope row and the inbox link are written before the
// send, so a reply resolves to the item even if this process dies between
// the two. subject doubles as the thread key: a repeat alert with the same
// subject carries In-Reply-To for the previous one.
func (s *Service) SendAlert(ctx context.Context, inboxID, subject, text string) (string, error) {
	cfg, err := s.Settings()
	if err != nil {
		return "", err
	}
	tr, err := s.Transport()
	if err != nil {
		return "", err
	}
	var workspaceID string
	var ws sql.NullString
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT workspace_id FROM inbox_items WHERE id=?`, inboxID).Scan(&ws); err != nil {
		return "", fmt.Errorf("alert inbox item %s: %w", inboxID, err)
	}
	workspaceID = ws.String
	prev, err := s.lastSentOnThread(ctx, workspaceID, subject)
	if err != nil {
		return "", err
	}
	dom, err := s.domain(ctx)
	if err != nil {
		return "", err
	}
	msgID := MintMessageID(dom)
	rowID := ulid.Make().String()
	root, inReplyTo, refs := msgID, "", []string(nil)
	if prev.messageID != "" {
		root = prev.threadRoot
		inReplyTo = prev.messageID
		refs = []string{prev.threadRoot}
		if prev.messageID != prev.threadRoot {
			refs = append(refs, prev.messageID)
		}
	}
	if err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO email_messages
  (id, direction, workspace_id, message_id, in_reply_to, thread_root, recipients,
   subject, disposition, inbox_id, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			rowID, "out", ws.String, msgID, inReplyTo, root, strings.Join(cfg.Operators, ","),
			subject, "sent", store.NullStr(inboxID), time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("alert envelope: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE inbox_items SET email_message_id=? WHERE id=?`, msgID, inboxID); err != nil {
			return fmt.Errorf("alert inbox link: %w", err)
		}
		return nil
	}); err != nil {
		return "", err
	}
	req := SendRequest{
		V: ContractV, From: s.alertFrom(ctx, workspaceID), FromName: cfg.FromName,
		To: cfg.Operators, Subject: subject, Text: text,
		MessageID: msgID, InReplyTo: inReplyTo, References: refs,
	}
	if err := s.sendWithBackoff(ctx, tr, req); err != nil {
		s.noteSendFailure(err)
		if _, uerr := s.st.DB().ExecContext(ctx,
			`UPDATE email_messages SET disposition='send_failed' WHERE id=?`, rowID); uerr != nil {
			s.log.ErrorContext(ctx, "email send_failed mark", "row", rowID, "error", uerr.Error())
		}
		return msgID, err
	}
	s.noteSendFailure(nil)
	s.appendSent(ctx, rowID, msgID)
	return msgID, nil
}

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
	a, err := s.Address(ctx, workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	full, err := tagAddress(a, agent)
	if err != nil {
		return "", err
	}
	return maskLocalPart(full) + "@" + a.Domain, nil
}

// EnsureAddress mints the workspace address once. The local part is
// <slug(name)>-<token>; the slug is frozen at first mint. The hosted
// transport mints in the cloud instead, and the row keeps the address the
// worker assigned (plan 2026-09-22 Part D).
func (s *Service) EnsureAddress(ctx context.Context, workspaceID, workspaceName string) (Address, error) {
	if a, err := s.Address(ctx, workspaceID); err == nil {
		return a, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Address{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	slug := slugify(workspaceName)
	for range 5 {
		lp, domain, hosted, err := s.mintCandidate(ctx, workspaceID, slug)
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

// Rotate mints a new token for the workspace. The slug stays; the old
// local part is retired from the Worker known set on the next ack. The
// hosted transport replaces the hosted address instead.
func (s *Service) Rotate(ctx context.Context, workspaceID, workspaceName string) (Address, error) {
	s.mu.RLock()
	hostedMode := s.hosted != nil
	s.mu.RUnlock()
	slug := ""
	if !hostedMode {
		old, err := s.Address(ctx, workspaceID)
		switch {
		case err == nil:
			var splitErr error
			slug, _, splitErr = splitLocalPart(old.LocalPart)
			if splitErr != nil {
				return Address{}, splitErr
			}
		case errors.Is(err, sql.ErrNoRows):
			slug = slugify(workspaceName)
		default:
			return Address{}, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for range 5 {
		lp, domain, hosted, err := s.mintCandidate(ctx, workspaceID, slug)
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
		if _, err := s.EnsureAddress(ctx, ref.ID, ref.Name); err != nil {
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
// routes. Both directions share it; absent columns are omitted. Bodies
// never live here: they stay in the Worker's object store.
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
const messageCols = `COALESCE(workspace_id,''), COALESCE(agent,''), COALESCE(message_id,''),
COALESCE(in_reply_to,''), COALESCE(thread_root,''), COALESCE(sender,''), COALESCE(recipients,''),
COALESCE(subject,''), COALESCE(dmarc,''), COALESCE(disposition,''), COALESCE(inbox_id,''),
COALESCE(run_id,''), COALESCE(conversation_id,''), COALESCE(attachments,'[]'),
COALESCE(trace_id,''), created_at, revision`

// ListMessages reads the envelope log newest-first. The row id is a ULID,
// so it doubles as the cursor.
func (s *Service) ListMessages(ctx context.Context, opts MessageListOpts) ([]Message, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	q := `SELECT id, direction, ` + messageCols + ` FROM email_messages WHERE 1=1`
	var args []any
	if opts.Direction != "" {
		q += ` AND direction=?`
		args = append(args, opts.Direction)
	}
	if opts.Workspace != "" {
		q += ` AND workspace_id=?`
		args = append(args, opts.Workspace)
	}
	if opts.Thread != "" {
		q += ` AND thread_root=?`
		args = append(args, opts.Thread)
	}
	if opts.Cursor != "" {
		q += ` AND id < ?`
		args = append(args, opts.Cursor)
	}
	q += ` ORDER BY id DESC LIMIT ?`
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

// GetMessage returns one envelope row, or sql.ErrNoRows.
func (s *Service) GetMessage(ctx context.Context, id string) (Message, error) {
	row := s.st.DB().QueryRowContext(ctx,
		`SELECT id, direction, `+messageCols+` FROM email_messages WHERE id=?`, id)
	m, err := scanMessage(row.Scan)
	if err != nil {
		return Message{}, err
	}
	return m, nil
}

func scanMessage(scan func(dest ...any) error) (Message, error) {
	var m Message
	var att string
	if err := scan(&m.ID, &m.Direction, &m.WorkspaceID, &m.Agent, &m.MessageID,
		&m.InReplyTo, &m.ThreadRoot, &m.Sender, &m.Recipients, &m.Subject,
		&m.DMARC, &m.Disposition, &m.InboxID, &m.RunID, &m.ConversationID,
		&att, &m.TraceID, &m.CreatedAt, &m.Revision); err != nil {
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
		status = "error"
	}
	row := map[string]any{
		"id": "email", "name": "email", "type": "native",
		"facets": []string{"email"}, "status": status,
		"provenance": "native", "managed": false,
	}
	metrics := map[string]any{"position": state.Position, "failures": state.Failures}
	if state.LastPoll != "" {
		metrics["last_poll"] = state.LastPoll
	}
	if state.LastError != "" {
		metrics["last_error"] = state.LastError
	}
	row["metrics"] = metrics
	return row
}

// alertFrom picks the sender for one alert. A workspace address makes the
// alert answerable: the local part is in the known set, so the operator's
// reply reaches the spool and resolves by Message-ID (plan I.1). The bare
// instance sender is the fallback for an alert with no workspace; mail to
// it never comes back, because no address row owns that local part.
func (s *Service) alertFrom(ctx context.Context, workspaceID string) string {
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
// from the routing environment; without an ingress only operators pass.
func (s *Service) checkRecipients(ctx context.Context, cfg Settings, opts SendOpts) error {
	if opts.Recipients != nil {
		for _, to := range opts.To {
			if !opts.Recipients.Accepts(to, cfg.Operators, s.workspaceAllowlist(ctx, opts.WorkspaceID)) {
				return fmt.Errorf("%w: %s", ErrRecipient, to)
			}
		}
		return nil
	}
	allowed := map[string]bool{}
	for _, o := range append([]string(nil), cfg.Operators...) {
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
// disposition='sent' rows thread: a failed send never reached the
// operator's client. The workspace scope keeps two workspaces with an
// identically titled fault on separate threads. Rows order by id (a minted
// ULID), not created_at: two sends in one second must still pick the later
// row.
func (s *Service) lastSentOnThread(ctx context.Context, workspaceID, subject string) (threadPrev, error) {
	var p threadPrev
	since := time.Now().UTC().Add(-alertThreadWindow).Format(time.RFC3339)
	err := s.st.DB().QueryRowContext(ctx, `
SELECT message_id, thread_root FROM email_messages
WHERE direction='out' AND subject=? AND disposition='sent'
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

func (s *Service) logTestEnvelope(ctx context.Context, cfg Settings, msgID string, sendErr error) (string, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	disposition := "sent"
	if sendErr != nil {
		disposition = "send_failed"
	}
	rowID := ulid.Make().String()
	_, err := s.st.DB().ExecContext(ctx, `
INSERT INTO email_messages
  (id, direction, workspace_id, message_id, recipients, subject, disposition, created_at)
VALUES (?,?,?,?,?,?,?,?)`,
		rowID, "out", "", msgID, strings.Join(cfg.Operators, ","),
		"trebi email test", disposition, now)
	if err != nil {
		return "", fmt.Errorf("email test envelope: %w", err)
	}
	return rowID, nil
}

// mintCandidate produces one candidate local part and domain. The hosted
// transport calls MintAddress on the cloud, where a second mint for one
// workspace replaces the address; the bearer transport mints locally with
// crypto/rand and the frozen slug. hosted reports which path ran.
func (s *Service) mintCandidate(ctx context.Context, workspaceID, slug string) (string, string, bool, error) {
	s.mu.RLock()
	hosted := s.hosted
	s.mu.RUnlock()
	if hosted != nil {
		full, err := hosted.MintAddress(ctx, workspaceID)
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
	lp, err := localPart(slug, token, "")
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
