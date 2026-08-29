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
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
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

// IngestAssistantTurns persists new assistant turns. Unique (run_id, turn_index) is idempotent.
func (s *Service) IngestAssistantTurns(ctx context.Context, runID, convID string, turns []IncomingTurn) (int, error) {
	if runID == "" || convID == "" {
		return 0, nil
	}
	if _, err := s.Get(ctx, convID); err != nil {
		return 0, err
	}
	n := 0
	for i, t := range turns {
		if t.Role != RoleAssistant || strings.TrimSpace(t.Text) == "" {
			continue
		}
		ok, err := s.insertAssistantTurn(ctx, convID, runID, i, t.Text)
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
			if err := writePointer(tx, opts.ChannelType, opts.ChannelRef, conv.ID, nowStr); err != nil {
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
	live, err := s.runs.FindLive(ctx, conv.Workspace, runs.KindAssistant, conv.Key)
	if err != nil {
		return nil
	}
	return live
}

func (s *Service) enqueueWake(ctx context.Context, conv *Conversation, text string) (string, error) {
	if s.queue == nil {
		return "", fmt.Errorf("%w: queue not bound", ErrValidation)
	}
	jobName, err := s.assistantJob(ctx, conv.Workspace)
	if err != nil {
		return "", err
	}
	prompt := s.wakePrompt(ctx, conv, text)
	item := &dispatch.QueueItem{
		Integration:     "assistant",
		WorkspaceID:     conv.Workspace,
		Job:             jobName,
		Kind:            runs.KindAssistant,
		ConversationKey: conv.Key,
		Prompt:          prompt,
		Status:          dispatch.QueuePending,
	}
	if err := s.queue.Enqueue(ctx, item); err != nil {
		return "", err
	}
	return item.ID, nil
}

func (s *Service) wakePrompt(ctx context.Context, conv *Conversation, text string) string {
	if s.ws == nil {
		return text
	}
	composed, err := s.ws.ComposeWakePrompt(ctx, conv.Workspace, workspaces.WakeOpts{
		ExtraPrompt: text,
		Recap:       s.Recap(ctx, conv.ID, defaultRecapN),
		Message:     text,
	})
	if err != nil || composed == "" {
		return text
	}
	return composed
}

func (s *Service) assistantJob(ctx context.Context, wsID string) (string, error) {
	if s.ws == nil {
		return "", fmt.Errorf("%w: %v", ErrValidation, errNoAssistant)
	}
	jobs, err := s.ws.ListJobs(ctx, wsID)
	if err != nil {
		return "", err
	}
	for _, row := range jobs {
		if row.Kind == "assistant" {
			return row.Name, nil
		}
	}
	return "", fmt.Errorf("%w: %v", ErrNotFound, errNoAssistant)
}

func (s *Service) insertAssistantTurn(ctx context.Context, convID, runID string, turnIndex int, text string) (bool, error) {
	var existing string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id FROM conversation_messages WHERE run_id=? AND turn_index=?`, runID, turnIndex).Scan(&existing)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	id := ulid.Make().String()
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO conversation_messages
			(id, conversation_id, role, text, run_id, turn_index, created_at)
			VALUES (?,?,?,?,?,?,?)`,
			id, convID, RoleAssistant, text, runID, turnIndex, nowStr); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE conversations SET last_activity_at=?, updated_at=?, revision=revision+1 WHERE id=?`,
			nowStr, nowStr, convID)
		return err
	}, store.BusEvent{
		Topic: "conversation.updated", Entity: "conversation", EntityID: convID, Action: "updated",
	})
	if uniqueViolation(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) maybeAutoTitle(ctx context.Context, convID string) {
	conv, err := s.Get(ctx, convID)
	if err != nil || strings.TrimSpace(conv.Title) != "" {
		return
	}
	msgs, err := s.ListMessages(ctx, convID, MessageListOpts{Limit: 50})
	if err != nil {
		return
	}
	userText := ""
	asst := 0
	for _, m := range msgs {
		if m.Role == RoleUser && userText == "" {
			userText = m.Text
		}
		if m.Role == RoleAssistant {
			asst++
		}
	}
	if asst != 1 || userText == "" {
		return
	}
	title := deriveTitle(userText)
	_, _ = s.Patch(ctx, convID, PatchOpts{Title: &title, Revision: conv.Revision})
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
