// Package channels owns channel replies. One outbox row carries one reply
// per assistant turn, a worker sends the due rows, and the queue ack and the
// typing pulse ride the connector's channel commands.
package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/store"
)

// ErrNotFound is a missing outbox row.
var ErrNotFound = errors.New("outbox row not found")

// ErrNotFailed is a retry of a row that did not fail.
var ErrNotFailed = errors.New("outbox row is not failed")

// Outbox statuses.
const (
	StatusPending = "pending"
	StatusSent    = "sent"
	StatusFailed  = "failed"
)

const (
	sendTimeout   = 30 * time.Second
	ackTimeout    = 10 * time.Second
	typingTimeout = 10 * time.Second
	dueBatch      = 20
	maxAttempts   = 5
	baseBackoff   = 5 * time.Second
	maxBackoff    = 5 * time.Minute
)

// Definitions is the channel view of the connectors service. The channels
// package owns it.
type Definitions interface {
	// Channel returns the channel block and the expanded env of a managed
	// connector with a channel.
	Channel(name string) (connectors.Channel, map[string]string, bool)
	// Cwd returns the connector working directory, or "".
	Cwd(name string) string
}

// OutboxRow is one pending or sent channel reply.
type OutboxRow struct {
	ID             string    `json:"id"`
	Connector      string    `json:"connector"`
	Workspace      string    `json:"workspace_id"`
	ConversationID string    `json:"conversation_id"`
	ChannelRef     string    `json:"channel_ref"`
	MessageID      string    `json:"message_id"`
	RunID          string    `json:"run_id,omitempty"`
	Text           string    `json:"text"`
	Status         string    `json:"status"`
	Attempts       int       `json:"attempts"`
	NextAttemptAt  string    `json:"next_attempt_at,omitempty"`
	ErrorMessage   string    `json:"error_message,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	SentAt         string    `json:"sent_at,omitempty"`
}

// Service sends channel replies for one store.
type Service struct {
	st    *store.Store
	conn  Definitions
	convs *conversations.Service
	log   *slog.Logger

	mu     sync.Mutex
	typing map[string]*typingState
}

// typingState tracks one live conversation's typing pulse.
type typingState struct {
	startedAt time.Time
	lastSent  time.Time
	failures  int
}

// New creates a channels service.
func New(st *store.Store, conn Definitions, convs *conversations.Service) *Service {
	return &Service{
		st: st, conn: conn, convs: convs,
		log:    slog.Default().With("source", "channels"),
		typing: map[string]*typingState{},
	}
}

// ReplyOnTurn writes one pending outbox row for the assistant turn of a
// conversation when the exchange began on a connector channel. For a run the
// turn carries runID; for the assistant session runID is empty. The message
// id is the row key, so one turn never sends twice.
func (s *Service) ReplyOnTurn(ctx context.Context, conversationID, runID string) error {
	if s.convs == nil || s.conn == nil || conversationID == "" {
		return nil
	}
	user, err := s.convs.LastUserMessage(ctx, conversationID)
	if err != nil || user == nil {
		return err
	}
	if _, _, ok := s.conn.Channel(user.ChannelType); !ok {
		return nil
	}
	turn, err := s.convs.LastAssistantTurn(ctx, conversationID, runID)
	if err != nil || turn == nil {
		return err
	}
	conv, err := s.convs.Get(ctx, conversationID)
	if err != nil {
		return err
	}
	_, err = s.st.DB().ExecContext(ctx, `
INSERT INTO channel_outbox
  (id, connector, workspace_id, conversation_id, channel_ref, message_id, run_id, text, status, attempts, created_at)
VALUES (?,?,?,?,?,?,?,?,?,0,?)
ON CONFLICT (message_id) DO NOTHING`,
		ulid.Make().String(), user.ChannelType, conv.Workspace, conversationID,
		user.ChannelRef, turn.ID, store.NullStr(runID), turn.Text, StatusPending,
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// SendDue sends the pending rows whose next attempt is due. A success marks
// the row sent; a failure backs off; five attempts fail the row and emit
// channel.send_failed.
func (s *Service) SendDue(ctx context.Context, now time.Time) error {
	if s.conn == nil {
		return nil
	}
	rows, err := s.dueRows(ctx, now)
	if err != nil {
		return err
	}
	for _, row := range rows {
		s.sendRow(ctx, row, now)
	}
	return nil
}

// Ack runs the connector's ack command once for a finished queue item. It is
// best effort: a failure is logged and changes nothing else.
func (s *Service) Ack(ctx context.Context, item dispatch.QueueItem) error {
	if s.conn == nil || item.Connector == "" {
		return nil
	}
	ch, env, ok := s.conn.Channel(item.Connector)
	if !ok || strings.TrimSpace(ch.Ack) == "" {
		return nil
	}
	convID := ""
	if s.convs != nil && item.WorkspaceID != "" && item.ConversationKey != "" {
		if p, err := s.convs.GetPointer(ctx, item.WorkspaceID, item.Connector, item.ConversationKey); err == nil {
			convID = p.ConversationID
		}
	}
	sourceID := ""
	if item.EventID != "" {
		_ = s.st.DB().QueryRowContext(ctx,
			`SELECT COALESCE(source_id,'') FROM events WHERE id=?`, item.EventID).Scan(&sourceID)
	}
	err := s.runCommand(ctx, s.conn.Cwd(item.Connector), env, ch.Ack, "",
		channelEnv(convID, item.ConversationKey, item.EventID, sourceID), ackTimeout)
	if err != nil {
		s.log.WarnContext(ctx, "channel ack", "connector", item.Connector, "error", err.Error())
	}
	return err
}

// PulseTyping runs the typing command of every live conversation at the
// channel's typing interval, until the turn ends or typing_max passes.
// Failures are logged at the first and at each tenth in a row.
func (s *Service) PulseTyping(ctx context.Context, now time.Time) error {
	if s.conn == nil || s.convs == nil {
		return nil
	}
	ids, err := s.liveConversations(ctx)
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(ids))
	for _, id := range ids {
		live[id] = true
	}
	s.mu.Lock()
	for id := range s.typing {
		if !live[id] {
			delete(s.typing, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.pulseOne(ctx, id, now)
	}
	return nil
}

// ListOutbox returns the outbox rows of one conversation, newest first.
func (s *Service) ListOutbox(ctx context.Context, conversationID string) ([]OutboxRow, error) {
	rows, err := s.st.DB().QueryContext(ctx, `
SELECT id, connector, workspace_id, conversation_id, channel_ref, message_id,
       COALESCE(run_id,''), text, status, attempts, COALESCE(next_attempt_at,''),
       COALESCE(error_message,''), created_at, COALESCE(sent_at,'')
FROM channel_outbox WHERE conversation_id=? ORDER BY id DESC`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxRow{}
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// RetryOutbox sets one failed row of a conversation back to pending with
// attempts = 0, so the next send worker tick retries it now.
func (s *Service) RetryOutbox(ctx context.Context, conversationID, rowID string) (*OutboxRow, error) {
	res, err := s.st.DB().ExecContext(ctx, `
UPDATE channel_outbox
SET status=?, attempts=0, next_attempt_at=NULL, error_message=NULL
WHERE id=? AND conversation_id=? AND status=?`,
		StatusPending, rowID, conversationID, StatusFailed)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var status string
		err := s.st.DB().QueryRowContext(ctx,
			`SELECT status FROM channel_outbox WHERE id=? AND conversation_id=?`,
			rowID, conversationID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		return nil, ErrNotFailed
	}
	row, err := s.outboxRow(ctx, rowID)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *Service) outboxRow(ctx context.Context, rowID string) (OutboxRow, error) {
	row := s.st.DB().QueryRowContext(ctx, `
SELECT id, connector, workspace_id, conversation_id, channel_ref, message_id,
       COALESCE(run_id,''), text, status, attempts, COALESCE(next_attempt_at,''),
       COALESCE(error_message,''), created_at, COALESCE(sent_at,'')
FROM channel_outbox WHERE id=?`, rowID)
	return scanOutbox(row)
}

func (s *Service) dueRows(ctx context.Context, now time.Time) ([]OutboxRow, error) {
	due := now.UTC().Format(time.RFC3339)
	rows, err := s.st.DB().QueryContext(ctx, `
SELECT id, connector, workspace_id, conversation_id, channel_ref, message_id,
       COALESCE(run_id,''), text, status, attempts, COALESCE(next_attempt_at,''),
       COALESCE(error_message,''), created_at, COALESCE(sent_at,'')
FROM channel_outbox
WHERE status=? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
ORDER BY id LIMIT ?`, StatusPending, due, dueBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxRow{}
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Service) sendRow(ctx context.Context, row OutboxRow, now time.Time) {
	ch, env, ok := s.conn.Channel(row.Connector)
	if !ok {
		s.markFailed(ctx, row, "connector has no channel")
		return
	}
	err := s.runCommand(ctx, s.conn.Cwd(row.Connector), env, ch.Send, row.Text,
		channelEnv(row.ConversationID, row.ChannelRef, "", ""), sendTimeout)
	if err == nil {
		s.markSent(ctx, row, now)
		return
	}
	s.retryRow(ctx, row, now, err)
}

func (s *Service) markSent(ctx context.Context, row OutboxRow, now time.Time) {
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE channel_outbox SET status=?, sent_at=? WHERE id=?`,
		StatusSent, now.UTC().Format(time.RFC3339Nano), row.ID); err != nil {
		s.log.ErrorContext(ctx, "channel sent mark", "row", row.ID, "error", err.Error())
		return
	}
	s.log.InfoContext(ctx, "channel.sent", "connector", row.Connector,
		"conversation_id", row.ConversationID, "message_id", row.MessageID)
}

func (s *Service) retryRow(ctx context.Context, row OutboxRow, now time.Time, cause error) {
	attempts := row.Attempts + 1
	if attempts >= maxAttempts {
		s.markFailed(ctx, row, cause.Error())
		return
	}
	backoff := baseBackoff << attempts
	if backoff > maxBackoff || backoff <= 0 {
		backoff = maxBackoff
	}
	if _, err := s.st.DB().ExecContext(ctx, `
UPDATE channel_outbox SET attempts=?, next_attempt_at=?, error_message=? WHERE id=?`,
		attempts, now.Add(backoff).UTC().Format(time.RFC3339), cause.Error(), row.ID); err != nil {
		s.log.ErrorContext(ctx, "channel retry mark", "row", row.ID, "error", err.Error())
	}
}

func (s *Service) markFailed(ctx context.Context, row OutboxRow, message string) {
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE channel_outbox SET status=?, attempts=?, error_message=? WHERE id=?`,
			StatusFailed, maxAttempts, message, row.ID)
		return err
	}, store.BusEvent{
		Topic: "channel.send_failed", Entity: "conversation", EntityID: row.ConversationID,
		Attrs: map[string]string{
			"connector": row.Connector, "conversation_id": row.ConversationID, "error": message,
		},
	})
	if err != nil {
		s.log.ErrorContext(ctx, "channel failed mark", "row", row.ID, "error", err.Error())
	}
}

func (s *Service) pulseOne(ctx context.Context, conversationID string, now time.Time) {
	user, err := s.convs.LastUserMessage(ctx, conversationID)
	if err != nil || user == nil {
		return
	}
	ch, env, ok := s.conn.Channel(user.ChannelType)
	if !ok || strings.TrimSpace(ch.Typing) == "" {
		return
	}
	interval := ch.TypingInterval.Std()
	if interval <= 0 {
		interval = 8 * time.Second
	}
	maximum := ch.TypingMax.Std()

	s.mu.Lock()
	st, ok := s.typing[conversationID]
	if !ok {
		st = &typingState{startedAt: now}
		s.typing[conversationID] = st
	}
	if maximum > 0 && now.Sub(st.startedAt) >= maximum {
		delete(s.typing, conversationID)
		s.mu.Unlock()
		return
	}
	if !st.lastSent.IsZero() && now.Sub(st.lastSent) < interval {
		s.mu.Unlock()
		return
	}
	st.lastSent = now
	s.mu.Unlock()

	err = s.runCommand(ctx, s.conn.Cwd(user.ChannelType), env, ch.Typing, "",
		channelEnv(conversationID, user.ChannelRef, "", ""), typingTimeout)
	s.mu.Lock()
	if err != nil {
		st.failures++
		if st.failures == 1 || st.failures%10 == 0 {
			s.log.WarnContext(ctx, "channel typing", "conversation_id", conversationID,
				"failures", st.failures, "error", err.Error())
		}
	} else {
		st.failures = 0
	}
	s.mu.Unlock()
}

// liveConversations lists conversations with a live assistant turn or a
// running agent run.
func (s *Service) liveConversations(ctx context.Context) ([]string, error) {
	rows, err := s.st.DB().QueryContext(ctx, `
SELECT DISTINCT conversation_id FROM assistant_sessions WHERE status IN ('starting','running','waiting')
UNION
SELECT DISTINCT conversation_id FROM runs WHERE conversation_id IS NOT NULL AND status='running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// runCommand runs one channel command with sh -c, the connector cwd and env,
// the channel env, the text on stdin, and a timeout.
func (s *Service) runCommand(ctx context.Context, cwd string, env map[string]string,
	cmdline, stdin string, extra map[string]string, timeout time.Duration) error {
	if strings.TrimSpace(cmdline) == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", cmdline)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range extra {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// channelEnv is the fixed channel env of decision 18. Empty values stay out.
func channelEnv(conversationID, channelRef, eventID, sourceID string) map[string]string {
	env := map[string]string{
		"TREBI_CHANNEL_REF":     channelRef,
		"TREBI_CONVERSATION_ID": conversationID,
	}
	if eventID != "" {
		env["TREBI_EVENT_ID"] = eventID
	}
	if sourceID != "" {
		env["TREBI_SOURCE_ID"] = sourceID
	}
	return env
}

type outboxScanner interface {
	Scan(dest ...any) error
}

func scanOutbox(row outboxScanner) (OutboxRow, error) {
	var o OutboxRow
	var created string
	if err := row.Scan(&o.ID, &o.Connector, &o.Workspace, &o.ConversationID, &o.ChannelRef,
		&o.MessageID, &o.RunID, &o.Text, &o.Status, &o.Attempts, &o.NextAttemptAt,
		&o.ErrorMessage, &created, &o.SentAt); err != nil {
		return o, err
	}
	o.CreatedAt = store.ParseTS(created)
	return o, nil
}
