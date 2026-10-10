package email

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/store"
)

// DispositionHeld is a reply that waits for the user.
const DispositionHeld = "held"

// Release scopes.
const (
	ReleaseOnce   = "once"
	ReleaseThread = "thread"
	ReleaseSender = "sender"
	ReleaseDomain = "domain"
)

var (
	ErrNotHeld    = errors.New("the mail is not held")
	errBadRelease = errors.New("scope must be once, thread, sender, or domain")
)

// ReplyCheck is the result of the safety check of one participant reply.
type ReplyCheck struct {
	DecisionID string
	Hold       bool
	Failed     bool     // the check did not finish; the mail is held
	Reasons    []string // the labels of the answers that hold the mail
}

// ReplyChecker runs the safety check of a participant reply. The daemon
// implements it with the decisions service.
type ReplyChecker interface {
	CheckReply(ctx context.Context, d RouteDecision, msg IngressMessage) (ReplyCheck, error)
}

// WithReplyChecker installs the safety check. With none, every participant
// reply is held. Plain wiring: the daemon calls it once, before Start.
func (s *Service) WithReplyChecker(c ReplyChecker) *Service {
	s.checker = c
	return s
}

// participant turns a drop for an agent tag into a participant or a held
// mail when the mail replies to a thread that the agent started. A mail
// that fails DMARC is never a participant.
func (g *Ingress) participant(ctx context.Context, msg *IngressMessage, d *RouteDecision) error {
	if d.Class != RouteDropped || d.Reason != ReasonNotAccepted || msg.Tag == "" || msg.Dmarc != "pass" {
		return nil
	}
	c := *d
	c.Agent = msg.Tag
	if err := g.svc.resolveAgent(ctx, &c); err != nil || c.AgentID == "" {
		return err
	}
	thread, err := g.svc.agentThread(ctx, c, *msg)
	if err != nil || thread == "" {
		return err
	}
	msg.Thread = thread
	c.Reason = ""
	if !g.svc.inThread(ctx, MailboxRoom(c.WorkspaceID, c.Agent), thread, msg.From) {
		c.Class, c.Reason = RouteHeld, ReasonNewSenderInThread
		*d = c
		return nil
	}
	c.Class = RouteParticipant
	check := ReplyCheck{Failed: true}
	if g.svc.checker != nil {
		if check, err = g.svc.checker.CheckReply(ctx, c, *msg); err != nil {
			g.log.Warn("email.check", "id", msg.ID, "error", err)
			check = ReplyCheck{DecisionID: check.DecisionID, Failed: true}
		}
	}
	c.DecisionID, c.HoldReasons = check.DecisionID, check.Reasons
	switch {
	case check.Failed:
		c.Class, c.Reason = RouteHeld, ReasonDecisionError
	case check.Hold:
		c.Class, c.Reason = RouteHeld, ReasonDecisionHold
	}
	*d = c
	return nil
}

// agentThread returns the thread root of an outbound mail of the agent that
// msg replies to, or "".
func (s *Service) agentThread(ctx context.Context, d RouteDecision, msg IngressMessage) (string, error) {
	cands := replyCandidates(msg)
	root := ThreadRoot(msg)
	q := `SELECT thread_root FROM email_messages WHERE direction='out' AND workspace_id=? AND agent_id=?
  AND (thread_root=?`
	args := []any{d.WorkspaceID, d.AgentID, root}
	if len(cands) > 0 {
		q += ` OR message_id IN (?` + strings.Repeat(",?", len(cands)-1) + `)`
		for _, c := range cands {
			args = append(args, c)
		}
	}
	q += `) ORDER BY created_at DESC LIMIT 1`
	var thread string
	err := s.st.DB().QueryRowContext(ctx, q, args...).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("agent thread: %w", err)
	}
	return thread, nil
}

// inThread reports whether addr is a participant of the thread.
func (s *Service) inThread(ctx context.Context, room, thread, addr string) bool {
	var parts string
	err := s.st.DB().QueryRowContext(ctx, `SELECT participants_json FROM channel_threads WHERE instance=? AND room=? AND thread=?`,
		Instance, room, thread).Scan(&parts)
	if err != nil {
		return false
	}
	return slices.Contains(decodeParticipants(parts), strings.ToLower(strings.TrimSpace(addr)))
}

// emitHeld appends email.held. A held mail fires no trigger and no stream.
func (s *Service) emitHeld(ctx context.Context, id string, d RouteDecision) {
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "email.held", Entity: "email", EntityID: id, Action: "created", TraceID: d.TraceID,
		Attrs: map[string]string{"workspace": d.WorkspaceID, "agent": d.Agent, "reason": d.Reason, "decision_id": d.DecisionID},
	}); err != nil {
		s.log.Warn("bus append", "topic", "email.held", "error", err)
	}
}

// Held lists the held mail, newest first. An empty workspace lists all.
func (s *Service) Held(ctx context.Context, workspace string) ([]Message, error) {
	q := `SELECT em.id, em.direction, ` + messageCols + messageFrom + ` WHERE em.direction='in' AND em.disposition=?`
	args := []any{DispositionHeld}
	if workspace != "" {
		q += ` AND em.workspace_id=?`
		args = append(args, workspace)
	}
	rows, err := s.st.DB().QueryContext(ctx, q+` ORDER BY upper(em.id) DESC LIMIT 200`, args...)
	if err != nil {
		return nil, fmt.Errorf("held mail: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("held mail: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// HeldMessage returns one held mail, or ErrNotHeld.
func (s *Service) HeldMessage(ctx context.Context, id string) (Message, error) {
	m, err := s.GetMessage(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, err
	}
	if err != nil {
		return Message{}, err
	}
	if m.Direction != "in" || m.Disposition != DispositionHeld || m.AgentName == "" {
		return Message{}, ErrNotHeld
	}
	return m, nil
}

// AcceptEntry is the email.accept entry of a sender or domain release.
func AcceptEntry(scope, sender string) (string, error) {
	addr := strings.ToLower(strings.TrimSpace(sender))
	switch scope {
	case ReleaseSender:
		return addr, nil
	case ReleaseDomain:
		_, domain, ok := strings.Cut(addr, "@")
		if !ok || domain == "" {
			return "", fmt.Errorf("sender %q has no domain", sender)
		}
		return "@" + domain, nil
	case ReleaseOnce, ReleaseThread:
		return "", nil
	}
	return "", errBadRelease
}

// Release gives a held mail to its agent as a participant reply. The
// thread scope and wider also add the sender to the thread. The caller
// writes email.accept for the sender and domain scopes.
func (s *Service) Release(ctx context.Context, id, scope string) (Message, error) {
	if _, err := AcceptEntry(scope, "a@b"); err != nil {
		return Message{}, err
	}
	m, err := s.HeldMessage(ctx, id)
	if err != nil {
		return Message{}, err
	}
	s.mu.RLock()
	g := s.ingress
	s.mu.RUnlock()
	if g == nil {
		return Message{}, ErrNoIngress
	}
	if err := s.setDisposition(ctx, m, "routed"); err != nil {
		return Message{}, err
	}
	room := MailboxRoom(m.WorkspaceID, m.AgentName)
	if scope != ReleaseOnce {
		s.touchThread(ctx, room, m.ThreadRoot, m.Subject, []string{m.Sender})
	}
	msg := IngressMessage{ID: m.ID, Tag: m.AgentName, From: m.Sender, Subject: m.Subject, MessageID: m.MessageID,
		InReplyTo: m.InReplyTo, Thread: m.ThreadRoot, Attachments: m.Attachments, Dmarc: m.DMARC}
	if m.Content != nil {
		msg.Text = m.Content.Text
	}
	d := RouteDecision{Class: RouteParticipant, WorkspaceID: m.WorkspaceID, Agent: m.AgentName, AgentID: m.AgentID,
		DMARC: m.DMARC, TraceID: m.TraceID, DecisionID: m.DecisionID}
	g.dispatch(ctx, msg, d, untrustedFor(msg))
	g.inject(ctx, msg, room)
	s.emitReceived(ctx, m.ID)
	return s.GetMessage(ctx, id)
}

// Ignore drops a held mail.
func (s *Service) Ignore(ctx context.Context, id string) (Message, error) {
	m, err := s.HeldMessage(ctx, id)
	if err != nil {
		return Message{}, err
	}
	if err := s.setDisposition(ctx, m, "dropped"); err != nil {
		return Message{}, err
	}
	return s.GetMessage(ctx, id)
}

func (s *Service) setDisposition(ctx context.Context, m Message, disposition string) error {
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE email_messages SET disposition=?, revision=revision+1 WHERE id=? AND disposition=?`,
			disposition, m.ID, DispositionHeld)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotHeld // a second release in the same moment
		}
		return nil
	}, store.BusEvent{Topic: "email.updated", Entity: "email", EntityID: m.ID, Action: "updated", Revision: m.Revision + 1,
		Attrs: map[string]string{"workspace": m.WorkspaceID, "disposition": disposition}})
	if err != nil {
		return fmt.Errorf("email %s: %w", m.ID, err)
	}
	return nil
}

// heldTitle is the inbox title of a held mail.
func heldTitle(msg IngressMessage) string {
	subject := msg.Subject
	for next := replyPrefixRE.ReplaceAllString(subject, ""); next != subject; next = replyPrefixRE.ReplaceAllString(subject, "") {
		subject = next
	}
	if subject = strings.TrimSpace(subject); subject == "" {
		subject = "(no subject)"
	}
	return fmt.Sprintf("%s replied to '%s'", msg.From, subject)
}

// HeldItem is the inbox text of a held mail. It never holds the body.
func (d RouteDecision) HeldItem(msg IngressMessage) (title, body string) {
	title = heldTitle(msg)
	switch d.Reason {
	case ReasonNewSenderInThread:
		body = msg.From + " is new in this thread. The agent does not read the mail until you release it."
	case ReasonDecisionError:
		body = "The safety check did not finish, so the mail waits for you."
	default:
		body = "The safety check found: " + strings.Join(d.HoldReasons, ", ") + "."
	}
	return title, body
}
