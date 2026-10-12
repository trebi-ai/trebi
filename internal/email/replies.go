// Reply handling (plan 2026-09-19 Part I): resolve one reply to the item
// or run it answers, act on it, and answer on the thread when an exchange
// ends. ingress.go routes; service.go keeps settings and sending.
package email

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/proposals"
	"github.com/trebi-ai/trebi/internal/requests"
)

// replyHops caps how deep one reply walks the References chain (plan I.1).
const replyHops = 8

// replyTarget is what one reply resolved to. isItem picks the dispatch
// path: an item target can carry a decision; a row target with inbox_id
// still goes to the assistant lane.
type replyTarget struct {
	conversationID string
	operator       string // short agent name; '' = assistant
	runID          string
	inboxID        string
	alertKind      string
	proposalID     string
	requestID      string
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

type ReplyOpts struct {
	RunID          string
	ConversationID string
	Agent          string // short id; '' = workspace address
	FromName       string // agent title; '' = Settings.FromName
}

// ReplyOnThread enqueues the assistant text written after the last user
// message on the mail thread that started the exchange (plan I.2). One
// outbound mail per exchange: covered_turn and the outbox dedupe key make
// the enqueue restart-safe. Missing wiring, thread, or text is a silent no-op.
func (s *Service) ReplyOnThread(ctx context.Context, opts ReplyOpts) error {
	if s.convs == nil {
		return nil
	}
	user, err := s.convs.LastUserMessage(ctx, opts.ConversationID)
	if err != nil || user == nil || user.Connector != conversations.ChannelEmail {
		return err
	}
	// The thread of an email user row is the thread root (A.13).
	root := user.Thread
	due, err := s.replyDueTurn(ctx, opts.ConversationID, opts.RunID, user.ID)
	if err != nil || due == nil {
		return err
	}
	in, ok, err := s.replyInboundOn(ctx, root)
	if err != nil || !ok {
		return err
	}
	return s.sendReply(ctx, opts, root, due, in)
}

// dueReply is the assistant text one reply mail carries and the turn it covers.
type dueReply struct {
	text string
	turn int
}

// replyDueTurn returns the text written after the user row userMsgID, or
// nil when there is none or an enqueued reply already covers its turn. The
// outbox owns the retry of a failed send, so every row covers.
func (s *Service) replyDueTurn(ctx context.Context, conversationID, runID, userMsgID string) (*dueReply, error) {
	last, text, ok, err := s.convs.ReplyText(ctx, conversationID, runID, userMsgID)
	if err != nil || !ok || last.TurnIndex == nil {
		return nil, err
	}
	var covered sql.NullInt64
	if err := s.st.DB().QueryRowContext(ctx, `
SELECT MAX(covered_turn) FROM email_messages
WHERE direction='out' AND conversation_id=? AND COALESCE(run_id,'')=?`,
		conversationID, runID).Scan(&covered); err != nil {
		return nil, fmt.Errorf("reply covered %s/%s: %w", conversationID, runID, err)
	}
	if covered.Valid && int(covered.Int64) >= *last.TurnIndex {
		return nil, nil
	}
	return &dueReply{text: text, turn: *last.TurnIndex}, nil
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

// sendReply enqueues the turn text on the thread (plan I.2). The dedupe
// key is the conversation, the run, and the turn, so a second hook for the
// same turn is a no-op.
func (s *Service) sendReply(ctx context.Context, opts ReplyOpts, root string, due *dueReply, in replyInbound) error {
	cfg, err := s.Settings()
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
	subject := in.subject
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	from := addr.Full()
	if opts.Agent != "" {
		tagged, err := tagAddress(addr, opts.Agent)
		if err != nil {
			return err
		}
		from = tagged + "@" + addr.Domain
	}
	turn := due.turn
	parts, err := s.htmlParts(ctx, in.workspaceID, opts.Agent, due.text, Page{
		Layout: LayoutReply, Title: subject, Footer: "Trebi conversation " + opts.ConversationID,
	})
	if err != nil {
		return err
	}
	_, err = s.enqueue(ctx, envelope{
		workspaceID: in.workspaceID, agent: opts.Agent, messageID: MintMessageID(addr.Domain),
		inReplyTo: in.messageID, threadRoot: root, recipients: []string{in.sender}, subject: subject,
		conversationID: opts.ConversationID, runID: opts.RunID, coveredTurn: &turn,
	}, outbox.Row{
		Purpose:   outbox.PurposeReply,
		Body:      due.text + "\n\n--\ntrebi conversation " + opts.ConversationID,
		Parts:     parts,
		Env:       fromEnv(from, cmp.Or(opts.FromName, cfg.FromName)),
		DedupeKey: "email:" + opts.ConversationID + ":" + opts.RunID + ":" + strconv.Itoa(turn),
	})
	return err
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
	var id, runID, kind, doneBy, subject string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id, COALESCE(run_id,''), COALESCE(alert_kind,''), COALESCE(done_by,''), COALESCE(subject,'') FROM inbox_items WHERE email_message_id=?`,
		messageID).Scan(&id, &runID, &kind, &doneBy, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reply item %s: %w", messageID, err)
	}
	return &replyTarget{inboxID: id, runID: runID, alertKind: kind,
		proposalID: proposalIDFromSubject(subject), requestID: requestIDFromSubject(subject),
		resolved: doneBy == "system", isItem: true}, nil
}

// proposalIDFromSubject extracts the derived proposal link from an alert subject.
func proposalIDFromSubject(subject string) string {
	if id, ok := strings.CutPrefix(subject, "proposal/"); ok {
		return id
	}
	return ""
}

// requestIDFromSubject extracts the request id from a request alert subject.
func requestIDFromSubject(subject string) string {
	if id, ok := strings.CutPrefix(subject, "request/"); ok {
		return id
	}
	return ""
}

// rowByMessageID finds the envelope row one message id names; a row with
// no link is not a target.
func (s *Service) rowByMessageID(ctx context.Context, messageID string) (*replyTarget, error) {
	var runID, convID, inboxID, operator string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT COALESCE(em.run_id,''), COALESCE(em.conversation_id,''), COALESCE(em.inbox_id,''),
			COALESCE((SELECT SUBSTR(ji.name, 8) FROM job_index ji WHERE ji.id = c.operator_id), '')
		 FROM email_messages em
		 LEFT JOIN conversations c ON c.id = em.conversation_id
		 WHERE em.message_id=? ORDER BY em.id DESC LIMIT 1`,
		messageID).Scan(&runID, &convID, &inboxID, &operator)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reply row %s: %w", messageID, err)
	}
	if runID == "" && convID == "" && inboxID == "" {
		return nil, nil
	}
	if inboxID != "" {
		var runID, alertKind, doneBy, subject string
		err := s.st.DB().QueryRowContext(ctx,
			`SELECT COALESCE(run_id,''), COALESCE(alert_kind,''), COALESCE(done_by,''), COALESCE(subject,'') FROM inbox_items WHERE id=?`,
			inboxID).Scan(&runID, &alertKind, &doneBy, &subject)
		if err == nil {
			return &replyTarget{inboxID: inboxID, runID: runID, alertKind: alertKind,
				proposalID: proposalIDFromSubject(subject), requestID: requestIDFromSubject(subject),
				resolved: doneBy == "system", isItem: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return &replyTarget{conversationID: convID, operator: operator, runID: runID, inboxID: inboxID}, nil
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
	if !t.isItem && t.conversationID != "" {
		env, err := g.act.RouteEnvFor(ctx, d.WorkspaceID)
		if err != nil {
			g.log.Warn("email.reply env", "id", msg.ID, "error", err)
			return true
		}
		cfg, err := g.svc.Settings()
		if err != nil {
			g.log.Warn("email.reply settings", "id", msg.ID, "error", err)
			return true
		}
		rule := env.Assistant
		if t.operator != "" {
			found, ok := env.Agents[t.operator]
			if !ok {
				// A deleted or renamed agent closes its tag.
				g.log.Warn("email.reply closed tag", "id", msg.ID, "operator", t.operator, "sender", msg.From)
				return true
			}
			rule = found
		}
		if !rule.Accepts(msg.From, cfg.Admins, env.Allowlist) {
			g.log.Warn("email.reply rejected", "id", msg.ID, "operator", t.operator, "sender", msg.From)
			return true
		}
	}
	g.log.Info("email.reply", "id", msg.ID, "item", t.isItem, "run", t.runID, "conversation", t.conversationID)
	g.applyReply(ctx, msg, d, t)
	return true
}

// applyReply acts on one resolved target (plan I.1, Part P). Failures
// log; the envelope row is already durable.
func (g *Ingress) applyReply(ctx context.Context, msg IngressMessage, d RouteDecision, t *replyTarget) {
	if t.isItem || t.inboxID != "" {
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
	case t.proposalID != "":
		g.decideProposalReply(ctx, msg, d, t)
	case t.alertKind == "request" && t.requestID != "":
		g.decideRequestReply(ctx, msg, d, t)
	case t.alertKind == "waiting" && t.runID != "":
		g.decideReply(ctx, msg, d, t)
	default:
		g.askReply(ctx, msg, d, t)
	}
}

// decideProposalReply applies an emailed AGENT.md proposal decision.
func (g *Ingress) decideProposalReply(ctx context.Context, msg IngressMessage, d RouteDecision, t *replyTarget) {
	decision, ambiguous := parseDecision(msg.Text)
	if ambiguous || decision == "" {
		g.askReply(ctx, msg, d, t)
		return
	}
	if err := g.act.DecideProposal(ctx, t.proposalID, decision == "allow", "email:"+msg.From); err != nil {
		g.log.Warn("email.reply proposal", "proposal", t.proposalID, "id", msg.ID, "error", err)
		if errors.Is(err, proposals.ErrStale) {
			_, _ = g.act.Ask(ctx, d, msg, "", "AGENT.md changed after this proposal. Nothing was applied.")
		}
	}
	if err := g.act.ResolveInbox(ctx, t.inboxID); err != nil {
		g.log.Warn("email.reply resolve proposal item", "inbox", t.inboxID, "error", err)
	}
	g.svc.LinkInbound(ctx, msg.ID, t.runID, "", t.inboxID)
}

// decideRequestReply answers the request of one inbox item. The first line
// picks the decision; any other reply goes to the job as a message.
func (g *Ingress) decideRequestReply(ctx context.Context, msg IngressMessage, d RouteDecision, t *replyTarget) {
	r, err := g.act.Request(ctx, t.requestID)
	if err != nil {
		g.log.Warn("email.reply request", "request", t.requestID, "id", msg.ID, "error", err)
		g.askReply(ctx, msg, d, t)
		return
	}
	if len(r.Forms) != 1 {
		_, _ = g.act.Ask(ctx, d, msg, "", fmt.Sprintf("This request has %d forms. Open the link to answer.", len(r.Forms))) // best effort notice
		g.svc.LinkInbound(ctx, msg.ID, t.runID, "", t.inboxID)
		return
	}
	in := requests.AnswerIn{
		Forms: []requests.FormAnswerIn{{Idx: r.Forms[0].Idx, FormAnswer: formAnswer(r.Forms[0], msg.Text)}},
		By:    "email:" + msg.From,
	}
	if err := g.act.AnswerRequest(ctx, t.requestID, in); err != nil {
		g.log.Warn("email.reply answer", "request", t.requestID, "id", msg.ID, "error", err)
		if errors.Is(err, requests.ErrClosed) {
			_, _ = g.act.Ask(ctx, d, msg, "", "This request is already closed. Nothing changed.") // best effort notice
		}
	}
	g.svc.LinkInbound(ctx, msg.ID, t.runID, "", t.inboxID)
}

// formAnswer maps one mail reply to the answer of a one-form request.
// approve and reject words decide; a first line that names one option
// chooses it; other text is a respond message.
func formAnswer(f requests.Form, text string) requests.FormAnswer {
	body := strings.TrimSpace(unquoted(text))
	line, _, _ := strings.Cut(body, "\n")
	line = strings.TrimSpace(line)
	if f.Type == requests.TypeChoose {
		for _, o := range f.Payload.Options {
			if strings.EqualFold(line, o.ID) || strings.EqualFold(line, o.Label) {
				return requests.FormAnswer{Decision: requests.DecisionChoose, Choices: []string{o.ID}}
			}
		}
	}
	respond := requests.FormAnswer{Decision: requests.DecisionRespond, Message: body}
	decision, ambiguous := parseDecision(text)
	switch {
	case ambiguous || decision == "":
		return respond
	case decision == "deny":
		return requests.FormAnswer{Decision: requests.DecisionReject}
	case slices.Contains(f.Allow, requests.DecisionApprove):
		return requests.FormAnswer{Decision: requests.DecisionApprove}
	case f.Type == requests.TypeForm && !requiresFields(f):
		return requests.FormAnswer{Decision: requests.DecisionSubmit, Values: json.RawMessage(`{}`)}
	}
	return respond
}

// requiresFields reports whether a form has a required field.
func requiresFields(f requests.Form) bool {
	sc, err := requests.ParseSchema(f.Payload.Schema)
	return err != nil || len(sc.Required) > 0
}

// unquoted drops the quoted lines of a mail reply.
func unquoted(text string) string {
	var keep []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), ">") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
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

// askReply replays one reply into the conversation of its operator (plan
// J: a reply on a resolved item never repeats the action). A refused reply
// raises an inbox item, so it never disappears.
func (g *Ingress) askReply(ctx context.Context, msg IngressMessage, d RouteDecision, t *replyTarget) {
	convID, err := g.act.Ask(ctx, d, msg, t.operator, inboundText(msg, untrustedFor(msg), ""))
	if err == nil {
		g.svc.LinkInbound(ctx, msg.ID, "", convID, t.inboxID)
		return
	}
	g.log.Warn("email.reply ask", "id", msg.ID, "operator", t.operator, "error", err)
	d.Reason = ReasonUndelivered
	inboxID, ierr := g.act.InboxItem(ctx, d, msg)
	if ierr != nil {
		g.log.Warn("email.reply undelivered item", "id", msg.ID, "error", ierr)
		inboxID = t.inboxID
	}
	g.svc.LinkInbound(ctx, msg.ID, "", "", inboxID)
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
// word (Part P): approve, allow, yes; deny, no, reject. Whole words only, so
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
			case "deny", "denied", "no", "reject", "rejected":
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
