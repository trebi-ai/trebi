// Package channels owns channel replies. One outbox row carries one reply
// per assistant turn. The outbox worker calls Deliver for each due row. The
// send, the seen mark, and the typing pulse are trebi-connector/1 requests
// to the live session of the connector instance.
package channels

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/store"
)

const (
	ackTimeout    = 10 * time.Second
	typingTimeout = 10 * time.Second
	// typingInterval and typingMax pace the typing pulse (Part D.5).
	typingInterval = 8 * time.Second
	typingMax      = 15 * time.Minute
)

// Sessions is the channel view of the connectors service. The channels
// package owns it.
type Sessions interface {
	// Call sends one request to the live session of an instance.
	Call(ctx context.Context, instance, method string, params, result any) error
	// Features lists the features the instance delivers.
	Features(instance string) []string
	// Limits returns the message limits of the instance.
	Limits(instance string) manifest.Limits
	// ConnectionFor returns the id of a slug or id ref, or "" when no
	// connection has it or the workspace turned it off.
	ConnectionFor(ref, workspaceID string) string
	// IsChannel reports whether a connector holds conversations.
	IsChannel(name string) bool
}

// LiveTurns tells which conversations have a live turn. The channels
// package owns it.
type LiveTurns interface {
	// Busy is true while the conversation has a turn that is thinking or acting.
	Busy(ctx context.Context, convID string) bool
}

// Service sends channel replies for one store.
type Service struct {
	st    *store.Store
	ob    *outbox.Service
	conn  Sessions
	convs *conversations.Service
	turns LiveTurns
	log   *slog.Logger

	mu     sync.Mutex
	typing map[string]*typingState
}

// typingState tracks one live conversation's typing pulse. expired stays
// set after typing_max until the conversation stops being busy.
type typingState struct {
	startedAt time.Time
	lastSent  time.Time
	failures  int
	expired   bool
}

// New creates a channels service. Replies go to ob.
func New(st *store.Store, ob *outbox.Service, conn Sessions, convs *conversations.Service) *Service {
	return &Service{
		st: st, ob: ob, conn: conn, convs: convs,
		log:    slog.Default().With("source", "channels"),
		typing: map[string]*typingState{},
	}
}

// WithLiveTurns sets the busy check that the typing pulse uses. Without it,
// every conversation with a live session or run counts as busy.
func (s *Service) WithLiveTurns(t LiveTurns) *Service {
	s.turns = t
	return s
}

// SendReplies enqueues one outbox row for each assistant text block written
// after the last user message, when the exchange began on a connector
// channel. For a run the blocks carry runID; for the assistant session runID
// is empty. The message id is the dedupe key, so a second call writes only
// the new blocks. The live hooks and the end-of-turn sweep both call it.
func (s *Service) SendReplies(ctx context.Context, conversationID, runID string) error {
	if s.convs == nil || s.conn == nil || s.ob == nil || conversationID == "" {
		return nil
	}
	user, err := s.convs.LastUserMessage(ctx, conversationID)
	if err != nil || user == nil {
		return err
	}
	instance := cmp.Or(user.Instance, user.Connector)
	if user.Room == "" || !s.conn.IsChannel(instance) {
		return nil
	}
	rows, err := s.convs.AssistantRowsAfter(ctx, conversationID, runID, user.ID)
	if err != nil || len(rows) == 0 {
		return err
	}
	conv, err := s.convs.Get(ctx, conversationID)
	if err != nil {
		return err
	}
	for _, m := range rows {
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		if _, err := s.ob.Enqueue(ctx, outbox.Row{
			Kind: outbox.KindChannel, Purpose: outbox.PurposeReply,
			WorkspaceID: conv.Workspace, Instance: instance,
			Room: user.Room, Thread: user.Thread,
			Body: m.Text, ConversationID: conversationID, RunID: runID, DedupeKey: m.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Deliver sends one outbox row with messages/send. Text over max_text goes
// in parts, each with the key <row_id>:<n>. The error code of the adapter
// selects the outbox result (Part D.6).
func (s *Service) Deliver(ctx context.Context, r outbox.Row) error {
	if s.conn == nil {
		return outbox.Permanent{Err: errors.New("connectors are not available")}
	}
	if r.Purpose == outbox.PurposeReply && r.ConversationID != "" {
		// A late reply must never arrive after a later one.
		later, err := s.ob.LaterSent(ctx, r)
		if err != nil {
			return err
		}
		if later {
			return outbox.Canceled{Reason: "superseded"}
		}
	}
	// A notify row names the slug of its via: step; a reply row holds the id.
	instance := s.conn.ConnectionFor(r.Instance, r.WorkspaceID)
	if instance == "" {
		return outbox.Permanent{Err: fmt.Errorf("connector %q is not found, is turned off, or is outside its scope in workspace %s", r.Instance, r.WorkspaceID)}
	}
	limits := s.conn.Limits(instance)
	format := "text"
	if slices.Contains(limits.Formats, "markdown") {
		format = "markdown"
	} else if len(limits.Formats) > 0 {
		format = limits.Formats[0]
	}
	parts := splitText(r.Body, limits.MaxText)
	for i, part := range parts {
		key := r.ID
		if len(parts) > 1 {
			key = fmt.Sprintf("%s:%d", r.ID, i+1)
		}
		var res protocol.SendResult
		err := s.conn.Call(ctx, instance, protocol.MethodMessagesSend, protocol.SendParams{
			Room: r.Room, Thread: r.Thread, Text: part, Format: format, Key: key,
		}, &res)
		if err != nil {
			return deliverErr(err)
		}
	}
	return nil
}

// deliverErr maps an adapter error to the outbox result.
func deliverErr(err error) error {
	switch protocol.CodeOf(err) {
	case protocol.CodeRateLimited:
		var pe *protocol.Error
		errors.As(err, &pe)
		return outbox.RetryAfter{After: pe.RetryAfter, Err: err}
	case protocol.CodeAuthRequired:
		return outbox.Hold{Reason: outbox.HoldAuthRequired, Err: err}
	case protocol.CodeNotFound, protocol.CodeInvalid, protocol.CodeUnsupported, protocol.CodePermanent:
		return outbox.Permanent{Err: err}
	}
	return err
}

// splitText cuts text into parts of at most max runes: at a paragraph break,
// else at a line break, else at a rune. max <= 0 keeps one part.
func splitText(text string, max int) []string {
	if max <= 0 || utf8.RuneCountInString(text) <= max {
		return []string{text}
	}
	var out []string
	for utf8.RuneCountInString(text) > max {
		cut := byteOffset(text, max)
		at := strings.LastIndex(text[:cut], "\n\n")
		skip := 2
		if at <= 0 {
			at, skip = strings.LastIndex(text[:cut], "\n"), 1
		}
		if at <= 0 {
			at, skip = cut, 0
		}
		out = append(out, text[:at])
		text = strings.TrimLeft(text[at+skip:], "\n")
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// byteOffset returns the byte offset of rune n.
func byteOffset(s string, n int) int {
	i := 0
	for off := range s {
		if i == n {
			return off
		}
		i++
	}
	return len(s)
}

// Ack marks the event of a finished queue item as seen. It is best effort:
// a failure is logged and changes nothing else. An instance without the
// seen feature is skipped.
func (s *Service) Ack(ctx context.Context, item dispatch.QueueItem) error {
	if s.conn == nil || item.Connector == "" || item.EventID == "" || !s.conn.IsChannel(item.Connector) {
		return nil
	}
	var instance, room, sourceID string
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT instance, COALESCE(room,''), COALESCE(source_id,'') FROM events WHERE id=?`,
		item.EventID).Scan(&instance, &room, &sourceID); err != nil || room == "" || sourceID == "" {
		return nil //nolint:nilerr // a missing event has nothing to mark
	}
	instance = cmp.Or(instance, item.Connector)
	if !slices.Contains(s.conn.Features(instance), manifest.FeatureSeen) {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, ackTimeout)
	defer cancel()
	err := s.conn.Call(cctx, instance, protocol.MethodMessagesSeen, protocol.SeenParams{Room: room, MessageID: sourceID}, nil)
	if err != nil {
		s.log.WarnContext(ctx, "channel ack", "connector", instance, "error", err.Error())
	}
	return err
}

// PulseTyping sends typing for every busy conversation each typingInterval,
// until the turn stops or typingMax passes.
// After typing_max the conversation stays silent until it is no longer busy.
// Failures are logged at the first and at each tenth in a row.
func (s *Service) PulseTyping(ctx context.Context, now time.Time) error {
	if s.conn == nil || s.convs == nil {
		return nil
	}
	candidates, err := s.liveConversations(ctx)
	if err != nil {
		return err
	}
	ids := candidates[:0]
	for _, id := range candidates {
		if s.turns == nil || s.turns.Busy(ctx, id) {
			ids = append(ids, id)
		}
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

func (s *Service) pulseOne(ctx context.Context, conversationID string, now time.Time) {
	user, err := s.convs.LastUserMessage(ctx, conversationID)
	if err != nil || user == nil {
		return
	}
	instance := cmp.Or(user.Instance, user.Connector)
	if user.Room == "" || !s.conn.IsChannel(instance) || !slices.Contains(s.conn.Features(instance), manifest.FeatureTyping) {
		return
	}

	s.mu.Lock()
	st, ok := s.typing[conversationID]
	if !ok {
		st = &typingState{startedAt: now}
		s.typing[conversationID] = st
	}
	if st.expired || now.Sub(st.startedAt) >= typingMax {
		st.expired = true
		s.mu.Unlock()
		return
	}
	if !st.lastSent.IsZero() && now.Sub(st.lastSent) < typingInterval {
		s.mu.Unlock()
		return
	}
	st.lastSent = now
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, typingTimeout)
	err = s.conn.Call(cctx, instance, protocol.MethodTyping, protocol.TypingParams{Room: user.Room, Thread: user.Thread}, nil)
	cancel()
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

// liveConversations lists conversations with a live assistant session or a
// running agent run. LiveTurns then keeps the busy ones.
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
