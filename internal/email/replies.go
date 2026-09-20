// Reply handling (plan 2026-09-19 Part I): resolve one reply to the item
// or run it answers, act on it, and answer on the thread when an exchange
// ends. ingress.go routes; service.go keeps settings and sending.
package email

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/conversations"
)

// replyHops caps how deep one reply walks the References chain (plan I.1).
const replyHops = 8

// replyTarget is what one reply resolved to. isItem picks the dispatch
// path: an item target can carry a decision; a row target with inbox_id
// still goes to the assistant lane.
type replyTarget struct {
	conversationID string
	runID          string
	inboxID        string
	alertKind      string
	resolved       bool
	isItem         bool
}

// replyInbound is the newest inbound mail on one thread.
type replyInbound struct {
	sender      string
	subject     string
	workspaceID string
	messageID   string
}

// ReplyOnThread mails the run's last assistant turn on the mail thread
// that started the exchange (plan I.2). One outbound mail per exchange:
// covered_turn makes the send restart-safe. Missing wiring, thread, or
// turn is a silent no-op.
func (s *Service) ReplyOnThread(ctx context.Context, runID, conversationID string) error {
	if s.convs == nil {
		return nil
	}
	root, ok, err := s.replyThreadRoot(ctx, conversationID)
	if err != nil || !ok {
		return err
	}
	turn, covered, err := s.replyDueTurn(ctx, conversationID, runID)
	if err != nil || turn == nil || *turn.TurnIndex <= covered {
		return err
	}
	in, ok, err := s.replyInboundOn(ctx, root)
	if err != nil || !ok {
		return err
	}
	return s.sendReply(ctx, conversationID, root, runID, turn, in)
}

// replyThreadRoot finds the thread the exchange began on: the newest
// user message must have arrived by email, and its channel ref is the
// thread root (A.13).
func (s *Service) replyThreadRoot(ctx context.Context, conversationID string) (string, bool, error) {
	m, err := s.convs.LastUserMessage(ctx, conversationID)
	if err != nil || m == nil {
		return "", false, err
	}
	if m.ChannelType != conversations.ChannelEmail {
		return "", false, nil
	}
	return m.ChannelRef, true, nil
}

// replyDueTurn reports the run's newest assistant turn and the turn one
// delivered reply already covered. Only sent rows cover: a failed send
// never reached the operator (plan I.2).
func (s *Service) replyDueTurn(ctx context.Context, conversationID, runID string) (*conversations.Message, int, error) {
	turn, err := s.convs.LastAssistantTurn(ctx, conversationID, runID)
	if err != nil || turn == nil {
		return nil, 0, err
	}
	var covered sql.NullInt64
	if err := s.st.DB().QueryRowContext(ctx, `
SELECT MAX(covered_turn) FROM email_messages
WHERE direction='out' AND disposition='sent' AND conversation_id=? AND run_id=?`,
		conversationID, runID).Scan(&covered); err != nil {
		return nil, 0, fmt.Errorf("reply covered %s/%s: %w", conversationID, runID, err)
	}
	if covered.Valid {
		return turn, int(covered.Int64), nil
	}
	return turn, -1, nil
}

// replyInboundOn finds the newest inbound mail on one thread; the reply
// goes to its sender with In-Reply-To set (plan I.2).
func (s *Service) replyInboundOn(ctx context.Context, root string) (replyInbound, bool, error) {
	var in replyInbound
	err := s.st.DB().QueryRowContext(ctx, `
SELECT COALESCE(sender,''), COALESCE(subject,''), COALESCE(workspace_id,''), COALESCE(message_id,'')
FROM email_messages WHERE direction='in' AND thread_root=? ORDER BY id DESC LIMIT 1`, root).
		Scan(&in.sender, &in.subject, &in.workspaceID, &in.messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return in, false, nil
	}
	if err != nil {
		return in, false, fmt.Errorf("reply inbound %s: %w", root, err)
	}
	return in, true, nil
}

// sendReply writes the envelope, then mails the turn text on the thread
// (plan I.2). A send failure marks the row send_failed; the next tick
// retries, because only sent rows count as covering.
func (s *Service) sendReply(ctx context.Context, conversationID, root, runID string, turn *conversations.Message, in replyInbound) error {
	if err := s.gate(); err != nil {
		return err
	}
	cfg, err := s.Settings()
	if err != nil {
		return err
	}
	client, err := s.Client()
	if err != nil {
		return err
	}
	addr, err := s.Address(ctx, in.workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // workspace lost its address; nothing to send from
	}
	if err != nil {
		return err
	}
	msgID := MintMessageID(addr.Domain)
	rowID := ulid.Make().String()
	subject := in.subject
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	refs := []string{root}
	if in.messageID != "" && in.messageID != root {
		refs = append(refs, in.messageID)
	}
	if _, err := s.st.DB().ExecContext(ctx, `
INSERT INTO email_messages
  (id, direction, workspace_id, message_id, in_reply_to, thread_root, recipients,
   subject, disposition, conversation_id, run_id, covered_turn, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rowID, "out", in.workspaceID, msgID, in.messageID, root, in.sender,
		subject, "sent", conversationID, runID, *turn.TurnIndex,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("reply envelope: %w", err)
	}
	req := SendRequest{
		V: ContractV, From: addr.Full(), FromName: cfg.FromName,
		To: []string{in.sender}, Subject: subject,
		Text:      turn.Text + "\n\n--\ntrebi conversation " + conversationID,
		MessageID: msgID, InReplyTo: in.messageID, References: refs,
	}
	if err := s.sendWithBackoff(ctx, client, req); err != nil {
		s.noteSendFailure(err)
		if _, uerr := s.st.DB().ExecContext(ctx,
			`UPDATE email_messages SET disposition='send_failed' WHERE id=?`, rowID); uerr != nil {
			s.log.ErrorContext(ctx, "email send_failed mark", "row", rowID, "error", uerr.Error())
		}
		return err
	}
	s.noteSendFailure(nil)
	s.appendSent(ctx, rowID, msgID)
	return nil
}

// resolveReply walks In-Reply-To then References (plan I.1): first the
// inbox items one message id answered, then the envelope rows. Nil means
// the reply starts a new exchange.
func (s *Service) resolveReply(ctx context.Context, msg IngressMessage) (*replyTarget, error) {
	for _, mid := range replyCandidates(msg) {
		t, err := s.itemByMessageID(ctx, mid)
		if err != nil {
			return nil, err
		}
		if t != nil {
			return t, nil
		}
	}
	for _, mid := range replyCandidates(msg) {
		t, err := s.rowByMessageID(ctx, mid)
		if err != nil {
			return nil, err
		}
		if t != nil {
			return t, nil
		}
	}
	return nil, nil
}

// itemByMessageID finds the inbox item one outbound message id answered.
func (s *Service) itemByMessageID(ctx context.Context, messageID string) (*replyTarget, error) {
	var id, runID, kind, status string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id, COALESCE(run_id,''), COALESCE(alert_kind,''), status FROM inbox_items WHERE email_message_id=?`,
		messageID).Scan(&id, &runID, &kind, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reply item %s: %w", messageID, err)
	}
	return &replyTarget{inboxID: id, runID: runID, alertKind: kind,
		resolved: status == "resolved", isItem: true}, nil
}

// rowByMessageID finds the envelope row one message id names; a row with
// no link is not a target.
func (s *Service) rowByMessageID(ctx context.Context, messageID string) (*replyTarget, error) {
	var runID, convID, inboxID string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT COALESCE(run_id,''), COALESCE(conversation_id,''), COALESCE(inbox_id,'')
		 FROM email_messages WHERE message_id=? ORDER BY id DESC LIMIT 1`,
		messageID).Scan(&runID, &convID, &inboxID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reply row %s: %w", messageID, err)
	}
	if runID == "" && convID == "" && inboxID == "" {
		return nil, nil
	}
	return &replyTarget{conversationID: convID, runID: runID, inboxID: inboxID}, nil
}

// dispatchReply tries the reply path before the class dispatch (plan
// I.1). False means the normal dispatch must run: no thread headers, an
// untrusted or dropped message, or no target.
func (g *Ingress) dispatchReply(ctx context.Context, msg IngressMessage, d RouteDecision) bool {
	if !d.Trusted || d.Class == RouteDropped {
		return false
	}
	if msg.InReplyTo == "" && len(msg.References) == 0 {
		return false
	}
	t, err := g.svc.resolveReply(ctx, msg)
	if err != nil {
		g.log.Warn("email.reply resolve", "id", msg.ID, "error", err)
		return false
	}
	if t == nil {
		return false
	}
	g.log.Info("email.reply", "id", msg.ID, "item", t.isItem, "run", t.runID, "conversation", t.conversationID)
	g.applyReply(ctx, msg, d, t)
	return true
}

// applyReply acts on one resolved target (plan I.1, Part P). Failures
// log; the envelope row is already durable.
func (g *Ingress) applyReply(ctx context.Context, msg IngressMessage, d RouteDecision, t *replyTarget) {
	if t.isItem {
		g.applyItemReply(ctx, msg, d, t)
		return
	}
	if t.conversationID != "" || t.inboxID != "" {
		g.askReply(ctx, msg, d, t)
		return
	}
	g.runReply(ctx, msg, t.runID, t.inboxID)
}

// applyItemReply dispatches on one inbox item target (plan I.1, Part P).
// A resolved item replays as an assistant message, never a second
// action; a waiting item can take a decision word; everything else
// lands in the conversation.
func (g *Ingress) applyItemReply(ctx context.Context, msg IngressMessage, d RouteDecision, t *replyTarget) {
	switch {
	case t.resolved:
		g.askReply(ctx, msg, d, t)
	case t.alertKind == "waiting" && t.runID != "":
		g.decideReply(ctx, msg, d, t)
	default:
		g.askReply(ctx, msg, d, t)
	}
}

// decideReply reads the decision word and applies it to the pending
// approval (plan I.1, Part P). No word, or an apply with no approval
// waiting, falls back to a run message; both words record the ambiguous
// item.
func (g *Ingress) decideReply(ctx context.Context, msg IngressMessage, d RouteDecision, t *replyTarget) {
	decision, ambiguous := parseDecision(msg.Text)
	if ambiguous {
		g.runReply(ctx, msg, t.runID, "")
		g.ambiguousItem(ctx, msg, d, t.runID)
		return
	}
	if decision == "" {
		g.runReply(ctx, msg, t.runID, t.inboxID)
		return
	}
	applied, err := g.act.DecideApproval(ctx, t.runID, decision)
	if err != nil || !applied {
		g.log.Warn("email.reply decide", "run", t.runID, "id", msg.ID, "applied", applied, "error", err)
		g.runReply(ctx, msg, t.runID, t.inboxID)
		return
	}
	if err := g.act.ResolveInbox(ctx, t.inboxID); err != nil {
		g.log.Warn("email.reply resolve item", "inbox", t.inboxID, "error", err)
	}
	g.svc.LinkInbound(ctx, msg.ID, t.runID, "", t.inboxID)
}

// runReply injects one operator reply into the live run (plan I.1). The
// sender is trusted, so the body rides unfenced, mirroring the wake line.
func (g *Ingress) runReply(ctx context.Context, msg IngressMessage, runID, inboxID string) {
	if err := g.act.RunMessage(ctx, runID, replyText(msg)); err != nil {
		g.log.Warn("email.reply message", "run", runID, "id", msg.ID, "error", err)
	}
	g.svc.LinkInbound(ctx, msg.ID, runID, "", inboxID)
}

// askReply replays one reply as an assistant message (plan J: a reply on
// a resolved item never repeats the action).
func (g *Ingress) askReply(ctx context.Context, msg IngressMessage, d RouteDecision, t *replyTarget) {
	convID, err := g.act.Ask(ctx, d, msg, assistantText(msg, untrustedFor(msg), false))
	if err != nil {
		g.log.Warn("email.reply ask", "id", msg.ID, "error", err)
	}
	g.svc.LinkInbound(ctx, msg.ID, "", convID, t.inboxID)
}

// ambiguousItem records the low-severity item for one approval reply
// that named both decisions (Part P), and links it on the reply row.
func (g *Ingress) ambiguousItem(ctx context.Context, msg IngressMessage, d RouteDecision, runID string) {
	inboxID, err := g.act.AmbiguousReplyItem(ctx, runID, d, msg)
	if err != nil {
		g.log.Warn("email.reply ambiguous", "id", msg.ID, "error", err)
		return
	}
	g.svc.LinkInbound(ctx, msg.ID, runID, "", inboxID)
}

// Decorate fills the delivery marker on each assistant message that went
// out by mail (plan I.2). Rows without covered_turn (alerts, explicit
// sends) never match; only sent rows mark a turn. Errors log; the messages
// still go out undecorated.
func (s *Service) Decorate(ctx context.Context, msgs []conversations.Message) {
	if len(msgs) == 0 {
		return
	}
	rows, err := s.st.DB().QueryContext(ctx, `
SELECT COALESCE(run_id,''), covered_turn, message_id, created_at FROM email_messages
WHERE direction='out' AND disposition='sent' AND covered_turn IS NOT NULL AND conversation_id=?`,
		msgs[0].ConversationID)
	if err != nil {
		s.log.Warn("email.decorate", "conversation", msgs[0].ConversationID, "error", err)
		return
	}
	defer func() { _ = rows.Close() }()
	type sentTurn struct {
		msgID  string
		sentAt string
	}
	sent := map[string]sentTurn{} // runID "\x00" turn -> delivery facts
	for rows.Next() {
		var runID string
		var turn int
		var msgID, sentAt string
		if err := rows.Scan(&runID, &turn, &msgID, &sentAt); err != nil {
			s.log.Warn("email.decorate scan", "error", err)
			return
		}
		sent[runID+"\x00"+strconv.Itoa(turn)] = sentTurn{msgID: msgID, sentAt: sentAt}
	}
	if err := rows.Err(); err != nil {
		s.log.Warn("email.decorate rows", "error", err)
		return
	}
	for i := range msgs {
		m := &msgs[i]
		if m.Role != conversations.RoleAssistant || m.RunID == "" || m.TurnIndex == nil {
			continue
		}
		st, ok := sent[m.RunID+"\x00"+strconv.Itoa(*m.TurnIndex)]
		if !ok {
			continue
		}
		m.Delivery = &conversations.Delivery{Channel: "email", MessageID: st.msgID, SentAt: st.sentAt}
	}
}

// replyCandidates lists the message ids a reply can resolve by,
// In-Reply-To first, at most replyHops, without duplicates.
func replyCandidates(msg IngressMessage) []string {
	out := make([]string, 0, 1+len(msg.References))
	for _, mid := range append([]string{msg.InReplyTo}, msg.References...) {
		mid = strings.TrimSpace(mid)
		if mid == "" || slices.Contains(out, mid) {
			continue
		}
		if len(out) >= replyHops {
			break
		}
		out = append(out, mid)
	}
	return out
}

// replyText composes the run message for one operator reply.
func replyText(msg IngressMessage) string {
	return "Email reply from " + msg.From + ": " + msg.Subject + "\n\n" + msg.Text
}

// parseDecision reads the first non-quoted content line for the decision
// word (Part P): approve, allow, yes; deny, no. Whole words only, so
// "now" never matches "no". Both sets is ambiguous; no word is no
// decision.
func parseDecision(text string) (decision string, ambiguous bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ">") {
			continue
		}
		allow, deny := false, false
		for _, f := range strings.Fields(line) {
			switch trimWord(f) {
			case "approve", "approved", "allow", "yes":
				allow = true
			case "deny", "denied", "no":
				deny = true
			}
		}
		switch {
		case allow && deny:
			return "", true
		case allow:
			return "allow", false
		case deny:
			return "deny", false
		}
		return "", false // the first content line decides
	}
	return "", false
}

// trimWord lowercases one word and strips the punctuation around it.
func trimWord(f string) string {
	return strings.ToLower(strings.Trim(f, `.,;:!?()[]"'`))
}
