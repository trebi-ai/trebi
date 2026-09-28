// Package channels owns channel replies. One outbox row carries one reply
// per assistant turn. The outbox worker calls Deliver for each due row. The
// queue ack and the typing pulse ride the connector's channel commands.
package channels

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/procgroup"
	"github.com/trebi-ai/trebi/internal/store"
)

// triggerRefPrefix marks the channel ref of an assistant item with no
// channel address. Such a conversation gets no reply.
const triggerRefPrefix = "trigger:"

const (
	sendTimeout   = 30 * time.Second
	ackTimeout    = 10 * time.Second
	typingTimeout = 10 * time.Second
	// commandOutputMax caps the output kept for an error message.
	commandOutputMax = 64 * 1024
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
	conn  Definitions
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
func New(st *store.Store, ob *outbox.Service, conn Definitions, convs *conversations.Service) *Service {
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

// ReplyOnTurn enqueues one outbox row for the assistant turn of a
// conversation when the exchange began on a connector channel. For a run the
// turn carries runID; for the assistant session runID is empty. The reply is
// the text written after the last user message. The id of the last text row
// is the dedupe key, so one turn never sends twice.
func (s *Service) ReplyOnTurn(ctx context.Context, conversationID, runID string) error {
	if s.convs == nil || s.conn == nil || s.ob == nil || conversationID == "" {
		return nil
	}
	user, err := s.convs.LastUserMessage(ctx, conversationID)
	if err != nil || user == nil {
		return err
	}
	if user.ChannelRef == "" || strings.HasPrefix(user.ChannelRef, triggerRefPrefix) {
		return nil
	}
	if ch, _, ok := s.conn.Channel(user.ChannelType); !ok || ch.SendOnly() {
		return nil
	}
	last, text, ok, err := s.convs.ReplyText(ctx, conversationID, runID, user.ID)
	if err != nil || !ok {
		return err
	}
	conv, err := s.convs.Get(ctx, conversationID)
	if err != nil {
		return err
	}
	_, err = s.ob.Enqueue(ctx, outbox.Row{
		Kind: outbox.KindChannel, Purpose: outbox.PurposeReply,
		WorkspaceID: conv.Workspace, Connector: user.ChannelType, Target: user.ChannelRef,
		Body: text, ConversationID: conversationID, RunID: runID, DedupeKey: last.ID,
	})
	return err
}

// Deliver runs the connector's send command for one outbox row, with the
// body on stdin. A connector with no channel block is a permanent failure.
func (s *Service) Deliver(ctx context.Context, r outbox.Row) error {
	if s.conn == nil {
		return outbox.Permanent{Err: errors.New("connectors are not available")}
	}
	ch, env, ok := s.conn.Channel(r.Connector)
	if !ok {
		return outbox.Permanent{Err: fmt.Errorf("connector %q has no channel block", r.Connector)}
	}
	if strings.TrimSpace(ch.Send) == "" {
		return outbox.Permanent{Err: fmt.Errorf("connector %q has no channel send command", r.Connector)}
	}
	extra := channelEnv(r.ConversationID, r.Target, "", "")
	extra["TREBI_MESSAGE_ID"] = r.ID
	extra["TREBI_PURPOSE"] = string(r.Purpose)
	maps.Copy(extra, r.Env)
	return s.runCommand(ctx, s.conn.Cwd(r.Connector), env, ch.Send, r.Body, extra, sendTimeout)
}

// Ack runs the connector's ack command once for a finished queue item. It is
// best effort: a failure is logged and changes nothing else.
func (s *Service) Ack(ctx context.Context, item dispatch.QueueItem) error {
	if s.conn == nil || item.Connector == "" {
		return nil
	}
	ch, env, ok := s.conn.Channel(item.Connector)
	if !ok || ch.SendOnly() || strings.TrimSpace(ch.Ack) == "" {
		return nil
	}
	sourceID, channelRef := "", ""
	if item.EventID != "" {
		// A missing event row leaves both values empty.
		_ = s.st.DB().QueryRowContext(ctx,
			`SELECT COALESCE(source_id,''), COALESCE(channel_ref,'') FROM events WHERE id=?`,
			item.EventID).Scan(&sourceID, &channelRef)
	}
	pointerRef := cmp.Or(channelRef, item.ConversationKey)
	convID := ""
	if s.convs != nil && item.WorkspaceID != "" && pointerRef != "" {
		if p, err := s.convs.GetPointer(ctx, item.WorkspaceID, item.Connector, pointerRef); err == nil {
			convID = p.ConversationID
		}
	}
	err := s.runCommand(ctx, s.conn.Cwd(item.Connector), env, ch.Ack, "",
		channelEnv(convID, channelRef, item.EventID, sourceID), ackTimeout)
	if err != nil {
		s.log.WarnContext(ctx, "channel ack", "connector", item.Connector, "error", err.Error())
	}
	return err
}

// PulseTyping runs the typing command of every busy conversation at the
// channel's typing interval, until the turn stops or typing_max passes.
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
	ch, env, ok := s.conn.Channel(user.ChannelType)
	if !ok || ch.SendOnly() || strings.TrimSpace(ch.Typing) == "" {
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
	if st.expired || (maximum > 0 && now.Sub(st.startedAt) >= maximum) {
		st.expired = true
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

// runCommand runs one channel command with sh -c, the connector cwd and env,
// the channel env, the text on stdin, and a timeout.
func (s *Service) runCommand(ctx context.Context, cwd string, env map[string]string,
	cmdline, stdin string, extra map[string]string, timeout time.Duration) error {
	if strings.TrimSpace(cmdline) == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmdEnv := os.Environ()
	for k, v := range env {
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	for k, v := range extra {
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	out, err := procgroup.Command{Script: cmdline, Dir: cwd, Env: cmdEnv, Stdin: strings.NewReader(stdin)}.
		Run(cctx, commandOutputMax)
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
