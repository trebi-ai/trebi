package conversations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

const msgCols = `id, conversation_id, role, COALESCE(channel_type,''), COALESCE(channel_ref,''),
	text, COALESCE(run_id,''), turn_index, COALESCE(idempotency_key,''), created_at`

// ListMessages returns messages in ascending ULID order.
func (s *Service) ListMessages(ctx context.Context, convID string, opts MessageListOpts) ([]Message, error) {
	if _, err := s.Get(ctx, convID); err != nil {
		return nil, err
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	q := `SELECT ` + msgCols + ` FROM conversation_messages WHERE conversation_id=?`
	args := []any{convID}
	if opts.Cursor != "" {
		q += ` AND id > ?`
		args = append(args, opts.Cursor)
	}
	q += ` ORDER BY id ASC LIMIT ?`
	args = append(args, opts.Limit)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// askPoll is the interval between reply reads inside Ask.
const askPoll = 500 * time.Millisecond

// Ask resolves the channel conversation, sends one message, and when Wait is
// set returns the assistant replies that arrive before the deadline.
func (s *Service) Ask(ctx context.Context, opts AskOpts) (*AskResult, error) {
	if strings.TrimSpace(opts.Workspace) == "" {
		return nil, fmt.Errorf("%w: workspace is required", ErrValidation)
	}
	if opts.ChannelType == "" {
		opts.ChannelType = ChannelCLI
	}
	if opts.ChannelRef == "" {
		opts.ChannelRef = opts.Workspace
	}
	convID, err := s.EnsureForChannel(ctx, opts.Workspace, opts.Operator, opts.ChannelType, opts.ChannelRef)
	if err != nil {
		return nil, err
	}
	if opts.Operator == "" {
		// The workspace assistant is a session (plan 2026-09-26 Part E).
		if s.assistant == nil {
			return nil, fmt.Errorf("%w: assistant session manager is not bound", ErrValidation)
		}
		res, err := s.assistant.Ask(ctx, convID, wire.PromptInput{
			Text: opts.Text, ChannelType: opts.ChannelType, ChannelRef: opts.ChannelRef,
			IdempotencyKey: opts.IdempotencyKey, Headless: opts.Headless,
		}, opts.Wait)
		if err != nil {
			return nil, err
		}
		return &AskResult{
			SendResult:     SendResult{Message: convFromWire(res.Message), Delivery: "prompted", Status: res.Status},
			ConversationID: convID,
			Replies:        convFromWireAll(res.Replies),
		}, nil
	}
	before, err := s.lastMessageID(ctx, convID)
	if err != nil {
		return nil, err
	}
	sent, err := s.Send(ctx, convID, SendOpts{
		Text: opts.Text, ChannelType: opts.ChannelType,
		ChannelRef: opts.ChannelRef, IdempotencyKey: opts.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	res := &AskResult{SendResult: *sent, ConversationID: convID}
	if opts.Wait <= 0 {
		return res, nil
	}
	res.Replies = s.waitReplies(ctx, convID, before, opts.Wait)
	return res, nil
}

// EnsureForChannel returns the conversation the channel points at, creating one
// when the pointer is absent.
func (s *Service) EnsureForChannel(ctx context.Context, workspace, operator, channelType, channelRef string) (string, error) {
	p, err := s.GetPointer(ctx, workspace, channelType, channelRef)
	if err == nil && p.ConversationID != "" {
		conv, err := s.Get(ctx, p.ConversationID)
		if err != nil {
			return "", err
		}
		if conv.Operator != operator {
			return "", ErrOperatorConflict
		}
		return conv.ID, nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	conv, err := s.Create(ctx, CreateOpts{
		Workspace:     workspace,
		Operator:      operator,
		OriginChannel: channelType,
		Activate:      &Activate{ChannelType: channelType, ChannelRef: channelRef},
	})
	if err != nil {
		return "", err
	}
	return conv.ID, nil
}

// waitReplies polls for assistant messages newer than after until the deadline.
// It returns what arrived; an empty slice means the assistant is still working.
func (s *Service) waitReplies(ctx context.Context, convID, after string, wait time.Duration) []Message {
	deadline := time.Now().Add(wait)
	for {
		msgs, err := s.ListMessages(ctx, convID, MessageListOpts{Cursor: after, Limit: 50})
		if err != nil {
			return nil
		}
		var replies []Message
		for _, m := range msgs {
			if m.Role == RoleAssistant {
				replies = append(replies, m)
			}
		}
		if len(replies) > 0 || !time.Now().Before(deadline) {
			return replies
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(min(askPoll, time.Until(deadline))):
		}
	}
}

// lastMessageID returns the newest message id, or "" for an empty conversation.
func (s *Service) lastMessageID(ctx context.Context, convID string) (string, error) {
	var id sql.NullString
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT MAX(id) FROM conversation_messages WHERE conversation_id=?`, convID).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return id.String, nil
}

// Send inserts a user message and injects or enqueues a wake.
func (s *Service) Send(ctx context.Context, convID string, opts SendOpts) (*SendResult, error) {
	text := strings.TrimSpace(opts.Text)
	if text == "" {
		return nil, fmt.Errorf("%w: text is required", ErrValidation)
	}
	conv, err := s.Get(ctx, convID)
	if err != nil {
		return nil, err
	}
	if opts.IdempotencyKey != "" {
		if existing, ok, err := s.messageByIdem(ctx, convID, opts.IdempotencyKey); err != nil {
			return nil, err
		} else if ok {
			return &SendResult{Message: existing}, nil
		}
	}
	if conv.Operator == "" {
		// The workspace assistant is a session (plan 2026-09-26 Part E).
		if s.assistant == nil {
			return nil, fmt.Errorf("%w: assistant session manager is not bound", ErrValidation)
		}
		res, err := s.assistant.Prompt(ctx, convID, wire.PromptInput{
			Text: opts.Text, ChannelType: opts.ChannelType, ChannelRef: opts.ChannelRef,
			IdempotencyKey: opts.IdempotencyKey,
		})
		if err != nil {
			return nil, err
		}
		if res.Duplicate {
			return &SendResult{Message: convFromWire(res.Message)}, nil
		}
		s.maybeAutoTitle(ctx, convID)
		return &SendResult{Message: convFromWire(res.Message), Delivery: "prompted", Status: res.Status}, nil
	}
	msg, err := s.insertUserMessage(ctx, conv, opts, text)
	if err != nil {
		if uniqueViolation(err) && opts.IdempotencyKey != "" {
			existing, ok, gerr := s.messageByIdem(ctx, convID, opts.IdempotencyKey)
			if gerr == nil && ok {
				return &SendResult{Message: existing}, nil
			}
		}
		return nil, err
	}
	delivery, runID, queueID, err := s.deliver(ctx, conv, text)
	if err != nil {
		return nil, err
	}
	return &SendResult{Message: msg, Delivery: delivery, RunID: runID, QueueID: queueID}, nil
}

// AppendUser stores one user row for the assistant session manager. The
// bool reports a duplicate idempotency key (plan 2026-09-26 D.2).
func (s *Service) AppendUser(ctx context.Context, convID string, in UserTurn) (Message, bool, error) {
	conv, err := s.Get(ctx, convID)
	if err != nil {
		return Message{}, false, err
	}
	if in.IdempotencyKey != "" {
		if existing, ok, err := s.messageByIdem(ctx, convID, in.IdempotencyKey); err != nil {
			return Message{}, false, err
		} else if ok {
			return existing, true, nil
		}
	}
	msg, err := s.insertUserMessage(ctx, conv, SendOpts{
		Text: in.Text, ChannelType: in.ChannelType,
		ChannelRef: in.ChannelRef, IdempotencyKey: in.IdempotencyKey,
	}, in.Text)
	if err != nil {
		if uniqueViolation(err) && in.IdempotencyKey != "" {
			existing, ok, gerr := s.messageByIdem(ctx, convID, in.IdempotencyKey)
			if gerr == nil && ok {
				return existing, true, nil
			}
		}
		return Message{}, false, err
	}
	s.maybeAutoTitle(ctx, convID)
	return msg, false, nil
}

// AppendAssistant stores one assistant reply row of a session turn
// (plan 2026-09-26 D.6).
func (s *Service) AppendAssistant(ctx context.Context, convID string, turn int, text string) (Message, error) {
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	id := ulid.Make().String()
	_, err := s.st.DB().ExecContext(ctx, `INSERT INTO conversation_messages
		(id, conversation_id, role, text, turn_index, created_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT DO NOTHING`,
		id, convID, RoleAssistant, text, turn, nowStr)
	if err != nil {
		return Message{}, err
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE conversations SET last_activity_at=?, updated_at=?, revision=revision+1 WHERE id=?`,
			nowStr, nowStr, convID)
		return err
	}, store.BusEvent{
		Topic: "conversation.updated", Entity: "conversation", EntityID: convID, Action: "updated",
	})
	if err != nil {
		return Message{}, err
	}
	return s.messageByID(ctx, id)
}

// LastAssistantRow returns the newest assistant row up to turn.
func (s *Service) LastAssistantRow(ctx context.Context, convID string, turn int) (Message, error) {
	row := s.st.DB().QueryRowContext(ctx, `SELECT `+msgCols+`
		FROM conversation_messages
		WHERE conversation_id=? AND role=? AND turn_index IS NOT NULL AND turn_index <= ?
		ORDER BY id DESC LIMIT 1`, convID, RoleAssistant, turn)
	m, err := scanMsg(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return m, err
}

// MessagesAfter returns messages newer than afterID, ascending.
func (s *Service) MessagesAfter(ctx context.Context, convID, afterID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+msgCols+`
		FROM conversation_messages
		WHERE conversation_id=? AND id > ? ORDER BY id LIMIT ?`, convID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// messageByID reads one row.
func (s *Service) messageByID(ctx context.Context, id string) (Message, error) {
	row := s.st.DB().QueryRowContext(ctx, `SELECT `+msgCols+` FROM conversation_messages WHERE id=?`, id)
	m, err := scanMsg(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return m, err
}

// Delete removes one conversation, its messages, and its channels
// (plan 2026-09-26 F.2). The caller removes the session and event rows.
func (s *Service) Delete(ctx context.Context, convID string) error {
	conv, err := s.Get(ctx, convID)
	if err != nil {
		return err
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM conversation_messages WHERE conversation_id=?`, convID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM conversation_channels WHERE conversation_id=?`, convID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM channel_pointers WHERE conversation_id=?`, convID); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM conversations WHERE id=?`, convID)
		return err
	}, store.BusEvent{
		Topic: "conversation.deleted", Entity: "conversation", EntityID: convID,
		Action: "deleted", Attrs: map[string]string{"workspace": conv.Workspace},
	})
	return err
}

func convFromWire(m wire.Message) Message {
	return Message{
		ID: m.ID, ConversationID: m.ConversationID, Role: m.Role,
		ChannelType: m.ChannelType, ChannelRef: m.ChannelRef, Text: m.Text,
		TurnIndex: m.TurnIndex, IdempotencyKey: m.IdempotencyKey, CreatedAt: m.CreatedAt,
	}
}

func convFromWireAll(in []wire.Message) []Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]Message, 0, len(in))
	for _, m := range in {
		out = append(out, convFromWire(m))
	}
	return out
}

// IngestAssistantTurns persists new assistant turns. Unique (run_id, turn_index) is idempotent.
func (s *Service) IngestAssistantTurns(ctx context.Context, runID, convID string, turns []IncomingTurn) (int, error) {
	return s.IngestAssistantTurnsAt(ctx, runID, convID, 0, turns)
}

// IngestAssistantTurnsAt is IngestAssistantTurns with turn_index = from+i.
// from is the count of previously consumed transcript turns (all roles).
func (s *Service) IngestAssistantTurnsAt(ctx context.Context, runID, convID string, from int, turns []IncomingTurn) (int, error) {
	if runID == "" || convID == "" {
		return 0, nil
	}
	if _, err := s.Get(ctx, convID); err != nil {
		return 0, err
	}
	maxIdx := -1
	_ = s.st.DB().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(turn_index), -1) FROM conversation_messages WHERE run_id=? AND turn_index IS NOT NULL`,
		runID).Scan(&maxIdx)
	n := 0
	for i, t := range turns {
		idx := from + i
		if idx <= maxIdx {
			continue
		}
		if t.Role != RoleAssistant || strings.TrimSpace(t.Text) == "" {
			continue
		}
		ok, err := s.insertAssistantTurn(ctx, convID, runID, idx, t.Text)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	if n > 0 {
		s.maybeAutoTitle(ctx, convID)
	}
	return n, nil
}

// AppendSystem persists a system recap row (native compaction).
func (s *Service) AppendSystem(ctx context.Context, convID, text string) error {
	if convID == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	if _, err := s.Get(ctx, convID); err != nil {
		return err
	}
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	id := ulid.Make().String()
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO conversation_messages
			(id, conversation_id, role, text, created_at) VALUES (?,?,?,?,?)`,
			id, convID, RoleSystem, text, nowStr); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE conversations SET updated_at=?, revision=revision+1 WHERE id=?`,
			nowStr, convID)
		return err
	}, store.BusEvent{
		Topic: "conversation.updated", Entity: "conversation", EntityID: convID, Action: "updated",
	})
	return err
}

func (s *Service) insertUserMessage(ctx context.Context, conv *Conversation, opts SendOpts, text string) (Message, error) {
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	msg := Message{
		ID: ulid.Make().String(), ConversationID: conv.ID, Role: RoleUser,
		ChannelType: opts.ChannelType, ChannelRef: opts.ChannelRef, Text: text,
		IdempotencyKey: opts.IdempotencyKey, CreatedAt: now,
	}
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO conversation_messages
			(id, conversation_id, role, channel_type, channel_ref, text, idempotency_key, created_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			msg.ID, conv.ID, RoleUser, opts.ChannelType, opts.ChannelRef, text,
			store.NullStr(opts.IdempotencyKey), nowStr); err != nil {
			return err
		}
		if opts.ChannelType != "" && opts.ChannelRef != "" {
			if err := writePointer(tx, conv.Workspace, opts.ChannelType, opts.ChannelRef, conv.ID, nowStr); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE conversations SET last_activity_at=?, updated_at=?, revision=revision+1 WHERE id=?`,
			nowStr, nowStr, conv.ID)
		return err
	}, store.BusEvent{
		Topic: "conversation.updated", Entity: "conversation", EntityID: conv.ID, Action: "updated",
	})
	return msg, err
}

func (s *Service) deliver(ctx context.Context, conv *Conversation, text string) (delivery, runID, queueID string, err error) {
	live := s.liveRun(ctx, conv)
	if live != nil {
		ierr := s.runs.InjectMessage(ctx, live.ID, text)
		if ierr == nil {
			return DeliveryInjected, live.ID, "", nil
		}
		if live.TmuxSession != "" && live.Harness != "fake" && !errors.Is(ierr, runs.ErrNotRunning) {
			s.log.Info("inject failed", "run_id", live.ID, "err", ierr)
			return "", "", "", ierr
		}
	}
	if s.queue == nil || s.ws == nil {
		return DeliveryQueued, "", "", nil
	}
	qid, err := s.enqueueWake(ctx, conv, text)
	if err != nil {
		return "", "", "", err
	}
	return DeliveryQueued, "", qid, nil
}

func (s *Service) liveRun(ctx context.Context, conv *Conversation) *runs.Run {
	if s.runs == nil {
		return nil
	}
	live, err := s.runs.FindLive(ctx, conv.Workspace, runs.KindAgent, conv.Key)
	if err != nil {
		return nil
	}
	return live
}

func (s *Service) enqueueWake(ctx context.Context, conv *Conversation, text string) (string, error) {
	if s.queue == nil {
		return "", fmt.Errorf("%w: queue not bound", ErrValidation)
	}
	jobName := conv.Operator
	kind := runs.KindAgent
	// The raw text rides the queue; dispatch composes the wake with a fresh
	// recap (Part F.3). item.Prompt is the wake message.
	item := &dispatch.QueueItem{
		Connector:       "conversation",
		WorkspaceID:     conv.Workspace,
		Job:             jobName,
		Kind:            kind,
		ConversationKey: conv.Key,
		Prompt:          text,
		Status:          dispatch.QueuePending,
	}
	if err := s.queue.Enqueue(ctx, item); err != nil {
		return "", err
	}
	return item.ID, nil
}

func (s *Service) insertAssistantTurn(ctx context.Context, convID, runID string, turnIndex int, text string) (bool, error) {
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	id := ulid.Make().String()
	res, err := s.st.DB().ExecContext(ctx, `INSERT INTO conversation_messages
		(id, conversation_id, role, text, run_id, turn_index, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT DO NOTHING`,
		id, convID, RoleAssistant, text, runID, turnIndex, nowStr)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE conversations SET last_activity_at=?, updated_at=?, revision=revision+1 WHERE id=?`,
			nowStr, nowStr, convID)
		return err
	}, store.BusEvent{
		Topic: "conversation.updated", Entity: "conversation", EntityID: convID, Action: "updated",
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// maybeAutoTitle sets an empty title from the first user message
// (plan 2026-09-26 E). It runs as soon as that message exists, before any
// assistant reply, and it never overwrites a title the user set.
func (s *Service) maybeAutoTitle(ctx context.Context, convID string) {
	conv, err := s.Get(ctx, convID)
	if err != nil || strings.TrimSpace(conv.Title) != "" {
		return
	}
	text, err := s.firstUserText(ctx, convID)
	if err != nil || strings.TrimSpace(text) == "" {
		return
	}
	title := deriveTitle(text)
	_, _ = s.Patch(ctx, convID, PatchOpts{Title: &title, Revision: conv.Revision})
}

// firstUserText returns the text of the oldest user row. An empty result
// with a nil error means the conversation has no user row yet.
func (s *Service) firstUserText(ctx context.Context, convID string) (string, error) {
	var text string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT text FROM conversation_messages WHERE conversation_id=? AND role=? ORDER BY id LIMIT 1`,
		convID, RoleUser).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return text, nil
}

func (s *Service) lastMessages(ctx context.Context, convID string, n int) ([]Message, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT `+msgCols+` FROM conversation_messages WHERE conversation_id=? ORDER BY id DESC LIMIT ?`,
		convID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rev []Message
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		rev = append(rev, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

func (s *Service) messageByIdem(ctx context.Context, convID, key string) (Message, bool, error) {
	row := s.st.DB().QueryRowContext(ctx,
		`SELECT `+msgCols+` FROM conversation_messages WHERE conversation_id=? AND idempotency_key=?`,
		convID, key)
	m, err := scanMsg(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, err
	}
	return m, true, nil
}

// LastUserMessage returns the newest user message on one conversation,
// or nil when it has none. Reply-by-mail reads the channel the exchange
// began on (plan I.2).
func (s *Service) LastUserMessage(ctx context.Context, conversationID string) (*Message, error) {
	row := s.st.DB().QueryRowContext(ctx,
		`SELECT `+msgCols+` FROM conversation_messages WHERE conversation_id=? AND role='user' ORDER BY id DESC LIMIT 1`,
		conversationID)
	m, err := scanMsg(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ReplyText returns the assistant text written after userMsgID in one
// conversation, joined in id order. runID picks the run rows; an empty runID
// picks the assistant session rows. msg is the last assistant row: its id
// keys the channel outbox and its turn index is the email covered_turn.
// ok is false when no text follows the user message.
func (s *Service) ReplyText(ctx context.Context, convID, runID, userMsgID string) (msg Message, text string, ok bool, err error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT `+msgCols+` FROM conversation_messages
		WHERE conversation_id=? AND role='assistant' AND (run_id = ? OR (? = '' AND run_id IS NULL))
		AND turn_index IS NOT NULL AND id > ?
		ORDER BY id`,
		convID, runID, runID, userMsgID)
	if err != nil {
		return Message{}, "", false, err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return Message{}, "", false, err
		}
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		parts = append(parts, m.Text)
		msg = m
	}
	if err := rows.Err(); err != nil {
		return Message{}, "", false, err
	}
	if len(parts) == 0 {
		return Message{}, "", false, nil
	}
	return msg, strings.Join(parts, "\n\n"), true, nil
}

// LastAssistantTurn returns the run's newest assistant row on one
// conversation, or the newest assistant session row when runID is empty.
func (s *Service) LastAssistantTurn(ctx context.Context, conversationID, runID string) (*Message, error) {
	row := s.st.DB().QueryRowContext(ctx,
		`SELECT `+msgCols+` FROM conversation_messages
		WHERE conversation_id=? AND role='assistant' AND (run_id = ? OR (? = '' AND run_id IS NULL))
		AND turn_index IS NOT NULL
		ORDER BY turn_index DESC, id DESC LIMIT 1`,
		conversationID, runID, runID)
	m, err := scanMsg(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func scanMsg(row convScanner) (Message, error) {
	var m Message
	var turn sql.NullInt64
	var created string
	err := row.Scan(&m.ID, &m.ConversationID, &m.Role, &m.ChannelType, &m.ChannelRef,
		&m.Text, &m.RunID, &turn, &m.IdempotencyKey, &created)
	if err != nil {
		return m, err
	}
	if turn.Valid {
		i := int(turn.Int64)
		m.TurnIndex = &i
	}
	m.CreatedAt = parseTime(created)
	return m, nil
}

func deriveTitle(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	if utf8.RuneCountInString(text) <= titleMaxRunes {
		return text
	}
	runes := []rune(text)
	return string(runes[:titleMaxRunes]) + "…"
}
