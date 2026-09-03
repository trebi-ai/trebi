// Package conversations owns conversation CRUD, messages, and channel pointers.
package conversations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

const (
	ChannelUI  = "ui"
	ChannelCLI = "cli"

	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"

	DeliveryInjected = "injected"
	DeliveryQueued   = "queued"

	defaultRecapN = 10
	titleMaxRunes = 60
)

var (
	ErrNotFound    = errors.New("not found")
	ErrConflict    = errors.New("conflict")
	ErrValidation  = errors.New("validation")
	errNoAssistant = errors.New("no assistant in workspace")
)

// Conversation is the §3.11 wire struct plus workspace/key for the lane.
type Conversation struct {
	ID             string                `json:"id"`
	Workspace      string                `json:"workspace"`
	Key            string                `json:"key"`
	Title          string                `json:"title"`
	OriginChannel  string                `json:"origin_channel"`
	Summary        string                `json:"summary,omitempty"`
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`
	LastActivityAt *time.Time            `json:"last_activity_at,omitempty"`
	Channels       []ConversationChannel `json:"channels,omitempty"`
	RunCount       int                   `json:"run_count,omitempty"`
	Revision       int64                 `json:"revision"`
}

// ConversationChannel is one window onto a conversation.
type ConversationChannel struct {
	ChannelType string    `json:"channel_type"`
	ChannelRef  string    `json:"channel_ref"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
}

// Message is one durable chat row.
type Message struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Role           string    `json:"role"`
	ChannelType    string    `json:"channel_type,omitempty"`
	ChannelRef     string    `json:"channel_ref,omitempty"`
	Text           string    `json:"text"`
	RunID          string    `json:"run_id,omitempty"`
	TurnIndex      *int      `json:"turn_index,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Pointer is a channel's active conversation.
type Pointer struct {
	ChannelType    string    `json:"channel_type"`
	ChannelRef     string    `json:"channel_ref"`
	ConversationID string    `json:"conversation_id"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Activate points a channel at a newly created conversation.
type Activate struct {
	ChannelType string `json:"channel_type"`
	ChannelRef  string `json:"channel_ref"`
}

// CreateOpts is POST /v1/conversations.
type CreateOpts struct {
	Workspace     string
	Title         string
	OriginChannel string
	Activate      *Activate
}

// PatchOpts is PATCH /v1/conversations/{id}. Nil fields are left unchanged.
type PatchOpts struct {
	Title    *string
	Summary  *string
	Revision int64 // 0 skips the conflict check
}

// ListOpts filters conversation lists.
type ListOpts struct {
	Workspace string
	Channel   string
	Limit     int
	Cursor    string
}

// MessageListOpts pages messages in ascending ULID order.
type MessageListOpts struct {
	Limit  int
	Cursor string
}

// SendOpts is POST /v1/conversations/{id}/messages.
type SendOpts struct {
	Text           string
	ChannelType    string
	ChannelRef     string
	IdempotencyKey string
}

// SendResult is the send-path response.
type SendResult struct {
	Message  Message `json:"message"`
	Delivery string  `json:"delivery"`
	RunID    string  `json:"run_id,omitempty"`
	QueueID  string  `json:"queue_id,omitempty"`
}

// AskOpts is POST /v1/workspaces/{id}/assistant/messages. The daemon resolves
// or creates the channel conversation, so a client sends no pointer calls.
type AskOpts struct {
	Workspace      string
	Text           string
	ChannelType    string // defaults to cli
	ChannelRef     string // defaults to the workspace id
	IdempotencyKey string
	Wait           time.Duration // 0 returns as soon as the message is queued
}

// AskResult carries the send outcome plus the assistant replies that arrived
// inside the wait window.
type AskResult struct {
	SendResult
	ConversationID string    `json:"conversation_id"`
	Replies        []Message `json:"replies,omitempty"`
}

// IncomingTurn is one parsed transcript turn for the watcher to persist.
type IncomingTurn struct {
	Role string
	Text string
	At   time.Time
}

const convCols = `id, workspace_id, COALESCE(key,''), COALESCE(title,''), COALESCE(origin_channel,''),
	COALESCE(summary,''), created_at, updated_at, last_activity_at, revision`

// Service owns conversations, messages, and channel pointers.
type Service struct {
	st    *store.Store
	log   *slog.Logger
	runs  *runs.Service
	queue *dispatch.Queue
	ws    *workspaces.Service
}

// New builds a conversations service. Bind runs/queue/workspaces before Send.
func New(st *store.Store) *Service {
	return &Service{st: st, log: slog.Default().With("source", "conversations")}
}

// WithRuns injects the runs service used by the send path.
func (s *Service) WithRuns(r *runs.Service) *Service {
	s.runs = r
	return s
}

// WithQueue injects the dispatch queue used by the send path.
func (s *Service) WithQueue(q *dispatch.Queue) *Service {
	s.queue = q
	return s
}

// WithWorkspaces injects the workspace service (assistant job + wake compose).
func (s *Service) WithWorkspaces(ws *workspaces.Service) *Service {
	s.ws = ws
	return s
}

// List returns conversations newest-id first.
func (s *Service) List(ctx context.Context, opts ListOpts) ([]Conversation, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	q := `SELECT ` + convCols + ` FROM conversations WHERE 1=1`
	args := []any{}
	if opts.Workspace != "" {
		q += ` AND workspace_id=?`
		args = append(args, opts.Workspace)
	}
	if opts.Channel != "" {
		q += ` AND (origin_channel=? OR EXISTS (
			SELECT 1 FROM conversation_channels ch WHERE ch.conversation_id=conversations.id AND ch.channel_type=?))`
		args = append(args, opts.Channel, opts.Channel)
	}
	if opts.Cursor != "" {
		q += ` AND id < ?`
		args = append(args, opts.Cursor)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, opts.Limit)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		c, err := scanConv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if out == nil {
		out = []Conversation{}
	}
	return out, rows.Err()
}

// Get returns one conversation with channels and run_count.
func (s *Service) Get(ctx context.Context, id string) (*Conversation, error) {
	row := s.st.DB().QueryRowContext(ctx, `SELECT `+convCols+` FROM conversations WHERE id=?`, id)
	c, err := scanConv(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.Channels, err = s.listChannels(ctx, id)
	if err != nil {
		return nil, err
	}
	_ = s.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE conversation_id=?`, id).Scan(&c.RunCount)
	return &c, nil
}

// Create inserts a conversation and optionally points a channel at it.
func (s *Service) Create(ctx context.Context, opts CreateOpts) (*Conversation, error) {
	if strings.TrimSpace(opts.Workspace) == "" {
		return nil, fmt.Errorf("%w: workspace is required", ErrValidation)
	}
	if strings.TrimSpace(opts.OriginChannel) == "" {
		return nil, fmt.Errorf("%w: origin_channel is required", ErrValidation)
	}
	if err := s.ensureWorkspace(ctx, opts.Workspace); err != nil {
		return nil, err
	}
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	id := ulid.Make().String()
	key := ulid.Make().String()
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO conversations
			(id, workspace_id, key, title, origin_channel, summary, created_at, updated_at, last_activity_at, revision)
			VALUES (?,?,?,?,?,'',?,?,?,1)`,
			id, opts.Workspace, key, store.NullStr(opts.Title), opts.OriginChannel, nowStr, nowStr, nowStr); err != nil {
			return err
		}
		if opts.Activate == nil {
			return nil
		}
		return writePointer(tx, opts.Activate.ChannelType, opts.Activate.ChannelRef, id, nowStr)
	}, store.BusEvent{
		Topic: "conversation.created", Entity: "conversation", EntityID: id, Action: "created", Revision: 1,
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Patch updates title/summary with an optional revision check.
func (s *Service) Patch(ctx context.Context, id string, opts PatchOpts) (*Conversation, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	title, summary := cur.Title, cur.Summary
	if opts.Title != nil {
		title = *opts.Title
	}
	if opts.Summary != nil {
		summary = *opts.Summary
	}
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	q := `UPDATE conversations SET title=?, summary=?, revision=revision+1, updated_at=? WHERE id=?`
	args := []any{title, summary, nowStr, id}
	if opts.Revision > 0 {
		q += ` AND revision=?`
		args = append(args, opts.Revision)
	}
	var newRev int64
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(q, args...)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrConflict
		}
		return tx.QueryRow(`SELECT revision FROM conversations WHERE id=?`, id).Scan(&newRev)
	}, store.BusEvent{
		Topic: "conversation.updated", Entity: "conversation", EntityID: id, Action: "updated",
	})
	if err != nil {
		return nil, err
	}
	cur.Title, cur.Summary, cur.Revision = title, summary, newRev
	cur.UpdatedAt, _ = time.Parse(time.RFC3339Nano, nowStr)
	return cur, nil
}

// GetPointer resolves a channel's active conversation.
func (s *Service) GetPointer(ctx context.Context, channelType, channelRef string) (*Pointer, error) {
	var p Pointer
	var updated string
	err := s.st.DB().QueryRowContext(ctx, `
		SELECT channel_type, channel_ref, conversation_id, updated_at
		FROM channel_pointers WHERE channel_type=? AND channel_ref=?`,
		channelType, channelRef).Scan(&p.ChannelType, &p.ChannelRef, &p.ConversationID, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.UpdatedAt = parseTime(updated)
	return &p, nil
}

// PutPointer is the /switch primitive: point a channel at a conversation.
func (s *Service) PutPointer(ctx context.Context, channelType, channelRef, convID string) (*Pointer, error) {
	if channelType == "" || channelRef == "" {
		return nil, fmt.Errorf("%w: channel_type and channel_ref are required", ErrValidation)
	}
	conv, err := s.Get(ctx, convID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	recap := s.Recap(ctx, convID, defaultRecapN)
	if recap == "" {
		recap = "Switched to this conversation."
	}
	msgID := ulid.Make().String()
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if err := writePointer(tx, channelType, channelRef, convID, nowStr); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO conversation_messages
			(id, conversation_id, role, channel_type, channel_ref, text, created_at)
			VALUES (?,?,?,?,?,?,?)`,
			msgID, convID, RoleSystem, channelType, channelRef, recap, nowStr); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE conversations SET last_activity_at=?, updated_at=?, revision=revision+1 WHERE id=?`,
			nowStr, nowStr, convID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	}, store.BusEvent{
		Topic: "conversation.updated", Entity: "conversation", EntityID: convID, Action: "updated",
	})
	if err != nil {
		return nil, err
	}
	s.log.Info("conversation.switched",
		"channel_type", channelType, "channel_ref", channelRef,
		"from", "", "to", convID, "workspace", conv.Workspace)
	return &Pointer{ChannelType: channelType, ChannelRef: channelRef, ConversationID: convID, UpdatedAt: now}, nil
}

// IDByKey resolves a conversation id from the workspace lane key.
func (s *Service) IDByKey(ctx context.Context, workspaceID, key string) (string, error) {
	var id string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id FROM conversations WHERE workspace_id=? AND key=?`, workspaceID, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// Recap is summary plus the last n messages, for wake composition and switch.
func (s *Service) Recap(ctx context.Context, id string, n int) string {
	conv, err := s.Get(ctx, id)
	if err != nil {
		return ""
	}
	if n <= 0 {
		n = defaultRecapN
	}
	var b strings.Builder
	if conv.Summary != "" {
		b.WriteString(conv.Summary)
		b.WriteByte('\n')
	}
	msgs, err := s.lastMessages(ctx, id, n)
	if err != nil {
		return strings.TrimSpace(b.String())
	}
	for _, m := range msgs {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Text)
	}
	return strings.TrimSpace(b.String())
}

func (s *Service) ensureWorkspace(ctx context.Context, id string) error {
	if s.ws == nil {
		return nil
	}
	if _, err := s.ws.Get(ctx, id); err != nil {
		return fmt.Errorf("%w: workspace not found", ErrNotFound)
	}
	return nil
}

func (s *Service) listChannels(ctx context.Context, convID string) ([]ConversationChannel, error) {
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT channel_type, channel_ref, created_at FROM conversation_channels
		WHERE conversation_id=? ORDER BY created_at`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConversationChannel{}
	for rows.Next() {
		var ch ConversationChannel
		var created string
		if err := rows.Scan(&ch.ChannelType, &ch.ChannelRef, &created); err != nil {
			return nil, err
		}
		ch.CreatedAt = parseTime(created)
		out = append(out, ch)
	}
	return out, rows.Err()
}

func writePointer(tx *sql.Tx, channelType, channelRef, convID, nowStr string) error {
	if channelType == "" || channelRef == "" {
		return fmt.Errorf("%w: activate requires channel_type and channel_ref", ErrValidation)
	}
	_, err := tx.Exec(`INSERT INTO channel_pointers (channel_type, channel_ref, conversation_id, updated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(channel_type, channel_ref) DO UPDATE SET
			conversation_id=excluded.conversation_id, updated_at=excluded.updated_at`,
		channelType, channelRef, convID, nowStr)
	if err != nil {
		return err
	}
	var existing string
	err = tx.QueryRow(`SELECT id FROM conversation_channels
		WHERE conversation_id=? AND channel_type=? AND channel_ref=?`,
		convID, channelType, channelRef).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.Exec(`INSERT INTO conversation_channels (id, conversation_id, channel_type, channel_ref, created_at)
		VALUES (?,?,?,?,?)`, ulid.Make().String(), convID, channelType, channelRef, nowStr)
	return err
}

type convScanner interface {
	Scan(dest ...any) error
}

func scanConv(row convScanner) (Conversation, error) {
	var c Conversation
	var created, updated string
	var last sql.NullString
	err := row.Scan(&c.ID, &c.Workspace, &c.Key, &c.Title, &c.OriginChannel, &c.Summary,
		&created, &updated, &last, &c.Revision)
	if err != nil {
		return c, err
	}
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	if last.Valid && last.String != "" {
		t := parseTime(last.String)
		c.LastActivityAt = &t
	}
	return c, nil
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, s)
	}
	return t.UTC()
}

func uniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "constraint")
}
