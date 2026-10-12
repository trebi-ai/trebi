// Inbound pipeline (plan 2026-09-19 Part H): pull the Worker spool,
// record, gate, route, ack. This file is the named inbound concern;
// service.go keeps addresses, settings, and sending.
package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/requests"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// RouteEnv is what the router must know about one workspace: the tags its
// agents answer to and its own allowlist extension (plan F).
type RouteEnv struct {
	Agents    map[string]AcceptRule // short id → rule
	Assistant AcceptRule
	Allowlist []string
}

// RouteClass is the closed set of routing verdicts (plan A.8).
type RouteClass string

const (
	RouteAgent     RouteClass = "agent"
	RouteAssistant RouteClass = "assistant"
	RouteMisrouted RouteClass = "misrouted"
	RouteDropped   RouteClass = "dropped"
	// RouteParticipant is a sender in a thread that an agent started.
	RouteParticipant RouteClass = "participant"
	// RouteHeld is a reply that waits for the user: a new sender in a thread
	// of an agent, or a reply that the safety check held.
	RouteHeld RouteClass = "held"
)

// Drop reasons. One spelling across the log and the inbox item.
const (
	ReasonNoAddress     = "no_address"
	ReasonDMARCFail     = "dmarc_fail"
	ReasonDMARCSpoof    = "dmarc_fail_spoof"
	ReasonUnknownTag    = "unknown_tag_untrusted"
	ReasonNotConfigured = "not_configured"
	ReasonNotAccepted   = "not_accepted"
	ReasonNotAddressed  = "not_addressed"
	ReasonMultipleFrom  = "multiple_from"
	ReasonDuplicate     = "duplicate"
	// Held reasons.
	ReasonNewSenderInThread = "new_sender_in_thread"
	ReasonDecisionHold      = "decision_hold"
	ReasonDecisionError     = "decision_error"
	// ReasonUndelivered marks a trusted reply that its conversation refused.
	ReasonUndelivered = "undelivered"
)

// RouteDecision is the verdict for one inbound message.
type RouteDecision struct {
	Class       RouteClass
	WorkspaceID string
	Agent       string // short agent name; RouteAgent only
	AgentID     string // job_index ID of Agent; set by deliver
	Trusted     bool   // sender is an operator or on the workspace allowlist
	ToAssistant bool   // the assistant accepts the sender; gates a misrouted ask
	DMARC       string
	TraceID     string
	Reason      string
	Accepted    bool
	DecisionID  string   // the safety check of a participant reply
	HoldReasons []string // the labels of the check answers that hold the mail
}

// Disposition is the email_messages spelling of the verdict (plan G).
func (d RouteDecision) Disposition() string {
	switch d.Class {
	case RouteMisrouted:
		return "misrouted"
	case RouteDropped:
		return "dropped"
	case RouteHeld:
		return DispositionHeld
	default:
		return "routed"
	}
}

// Spoof reports a drop that claimed a trusted identity: the one drop that
// still gets a low-severity inbox item.
func (d RouteDecision) Spoof() bool { return d.Reason == ReasonDMARCSpoof }

// Actions is the write side of routing plus the workspace facts it reads.
// daemon.ControlPlane implements it: alerts imports email, so email cannot
// import back, and cross-service orchestration belongs to the daemon.
type Actions interface {
	// RouteEnvFor returns one workspace's agent tags and allowlist.
	RouteEnvFor(ctx context.Context, workspaceID string) (RouteEnv, error)
	// Ask delivers one message to the operator's conversation; agent is the
	// short agent name, or empty for the assistant. It returns the
	// conversation id.
	Ask(ctx context.Context, d RouteDecision, msg IngressMessage, agent, text string) (string, error)
	// InboxItem records the low-severity item; returns the inbox item id.
	InboxItem(ctx context.Context, d RouteDecision, msg IngressMessage) (string, error)
	// DecideApproval applies one reply decision to the run's pending
	// approval; applied is false when no approval waits.
	DecideApproval(ctx context.Context, runID, decision string) (applied bool, err error)
	// RunMessage injects one operator reply into a live run.
	RunMessage(ctx context.Context, runID, text string) error
	// DecideProposal applies one emailed AGENT.md proposal decision.
	DecideProposal(ctx context.Context, id string, allow bool, by string) error
	// Request returns one request for an emailed answer.
	Request(ctx context.Context, id string) (requests.Request, error)
	// AnswerRequest applies one emailed request answer.
	AnswerRequest(ctx context.Context, id string, in requests.AnswerIn) error
	// ResolveInbox marks one inbox item resolved.
	ResolveInbox(ctx context.Context, inboxID string) error
	// AmbiguousReplyItem records the item for one approval reply that named
	// both decisions; returns the inbox item id.
	AmbiguousReplyItem(ctx context.Context, runID string, d RouteDecision, msg IngressMessage) (string, error)
	// AssistantName returns the workspace assistant title, or "".
	AssistantName(ctx context.Context, workspaceID string) string
}

// Draining reports whether the daemon is draining; the loop skips
// automatic polls, the manual route does not.
func (s *Service) Draining() bool { return s.st.IsDraining() }

// WorkspaceByLocalPart resolves the address row's workspace, or
// sql.ErrNoRows when no workspace owns the base.
func (s *Service) WorkspaceByLocalPart(ctx context.Context, localPart string) (string, error) {
	var wsID string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT workspace_id FROM email_addresses WHERE local_part=?`, localPart).Scan(&wsID)
	return wsID, err
}

// Route applies the routing table (plan A.8, A.10) to one inbound
// message. Pure verdict: it writes nothing and wakes nothing.
func (s *Service) Route(msg IngressMessage, wsID string, env RouteEnv) RouteDecision {
	d := RouteDecision{WorkspaceID: wsID, DMARC: msg.Dmarc, TraceID: ulid.Make().String()}
	cfg, err := s.Settings()
	if err != nil {
		d.Class, d.Reason = RouteDropped, ReasonNotConfigured
		return d
	}
	d.Trusted = addrListed(cfg.Admins, msg.From) || addrListed(env.Allowlist, msg.From)
	d.ToAssistant = env.Assistant.Accepts(msg.From, cfg.Admins, env.Allowlist)
	if cfg.RequireDMARC && msg.Dmarc != "pass" {
		d.Class, d.Reason = RouteDropped, ReasonDMARCFail
		if d.Trusted {
			d.Reason = ReasonDMARCSpoof // claimed a trusted identity
		}
		return d
	}
	if !msg.SingleFrom() {
		d.Class, d.Reason = RouteDropped, ReasonMultipleFrom
		return d
	}
	if !msg.Addressed() {
		d.Class, d.Reason = RouteDropped, ReasonNotAddressed // a signed mail replayed from another thread
		return d
	}
	rule, known := env.Agents[msg.Tag]
	switch {
	case msg.Tag == "" && d.ToAssistant:
		d.Class, d.Accepted = RouteAssistant, true
	case msg.Tag == "":
		d.Class, d.Reason = RouteDropped, ReasonNotAccepted
	case known && !rule.Empty() && rule.Accepts(msg.From, cfg.Admins, env.Allowlist):
		d.Class, d.Agent, d.Accepted = RouteAgent, msg.Tag, true
	case known && d.Trusted:
		d.Class, d.Reason = RouteMisrouted, ReasonNotAccepted
	case known:
		d.Class, d.Reason = RouteDropped, ReasonNotAccepted
	case d.Trusted:
		d.Class = RouteMisrouted
	default:
		d.Class, d.Reason = RouteDropped, ReasonUnknownTag
	}
	return d
}

// RecordInbound writes one inbound envelope with its verdict. It returns
// false when the row already exists: a re-pull after a crash between
// record and ack must not route twice (plan H).
func (s *Service) RecordInbound(ctx context.Context, msg IngressMessage, d RouteDecision) (bool, error) {
	att, err := json.Marshal(msg.Attachments)
	if err != nil {
		att = []byte("[]")
	}
	text, html := s.keptContent(msg.Text, msg.HTML, d.Class == RouteHeld) // a release needs the text
	res, err := s.st.DB().ExecContext(ctx, `INSERT INTO email_messages (
		id, direction, workspace_id, agent_id, message_id, in_reply_to, thread_root,
		sender, subject, dmarc, disposition, attachments, trace_id, created_at, revision,
		body_text, body_html, decision_id
	) VALUES (?, 'in', ?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?)
		ON CONFLICT(id) DO NOTHING`,
		msg.ID, d.WorkspaceID, store.NullStr(d.AgentID), msg.MessageID, msg.InReplyTo, ThreadRoot(msg),
		msg.From, msg.Subject, msg.Dmarc, d.Disposition(), string(att), d.TraceID,
		time.Now().UTC().Format(time.RFC3339), text, html, d.DecisionID)
	if err != nil {
		return false, fmt.Errorf("record inbound %s: %w", msg.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record inbound %s: %w", msg.ID, err)
	}
	return n > 0, nil
}

// resolveAgent sets d.AgentID. An agent that went away after Route
// routes like an unknown tag, so its mail never records a NULL agent.
func (s *Service) resolveAgent(ctx context.Context, d *RouteDecision) error {
	err := s.st.DB().QueryRowContext(ctx, `SELECT id FROM job_index WHERE workspace_id=? AND kind='agent' AND name=? AND retired_at IS NULL`,
		d.WorkspaceID, "agents/"+d.Agent).Scan(&d.AgentID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		d.Agent, d.Accepted = "", false
		d.Class, d.Reason = RouteDropped, ReasonUnknownTag
		if d.Trusted {
			d.Class, d.Reason = RouteMisrouted, ""
		}
		return nil
	case err != nil:
		return fmt.Errorf("resolve agent %s: %w", d.Agent, err)
	}
	return nil
}

// seenInbound reports an earlier copy of one Message-ID that reached the
// same operator. A re-sent copy must not act twice.
func (s *Service) seenInbound(ctx context.Context, msg IngressMessage, d RouteDecision) bool {
	if msg.MessageID == "" {
		return false
	}
	var n int
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM email_messages WHERE direction='in' AND message_id=?
		 AND workspace_id=? AND COALESCE(agent_id,'') = ? AND id<>? AND disposition<>'dropped'`,
		msg.MessageID, d.WorkspaceID, d.AgentID, msg.ID).Scan(&n)
	if err != nil {
		s.log.Warn("email.seen", "id", msg.ID, "error", err)
		return false
	}
	return n > 0
}

// LinkInbound records routing-produced ids on the message row. Empty
// arguments are skipped; the call is best-effort.
func (s *Service) LinkInbound(ctx context.Context, id, runID, conversationID, inboxID string) {
	sets := map[string]string{"run_id": runID, "conversation_id": conversationID, "inbox_id": inboxID}
	var cols []string
	var args []any
	for col, val := range sets {
		if val == "" {
			continue
		}
		cols = append(cols, col+"=?")
		args = append(args, val)
	}
	if len(cols) == 0 {
		return
	}
	args = append(args, id)
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE email_messages SET `+strings.Join(cols, ", ")+` WHERE id=?`, args...); err != nil {
		s.log.Warn("link inbound", "id", id, "error", err)
	}
}

// ThreadRoot picks the thread anchor: the thread the daemon found, else
// the first Reference, else In-Reply-To, else the message's own
// Message-ID. One mail thread binds one conversation.
func ThreadRoot(msg IngressMessage) string {
	if msg.Thread != "" {
		return msg.Thread
	}
	if len(msg.References) > 0 && strings.TrimSpace(msg.References[0]) != "" {
		return msg.References[0]
	}
	if msg.InReplyTo != "" {
		return msg.InReplyTo
	}
	return msg.MessageID
}

// emitReceived appends the thin email.received frame (plan H). Drops do
// not emit: they must not fire job triggers or stream noise.
func (s *Service) emitReceived(ctx context.Context, id string) {
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "email.received", Entity: "email", EntityID: id, Action: "created",
	}); err != nil {
		s.log.Warn("bus append", "topic", "email.received", "error", err)
	}
}

// emitConnectorState appends connector.unhealthy / connector.recovered
// for the email channel (plan H). Both topics exist; the alert registry
// does not change.
func (s *Service) emitConnectorState(topic, status string) {
	if _, err := s.st.Append(context.Background(), store.BusEvent{
		Topic: topic, Entity: "connector", EntityID: "email", Action: "updated",
		Attrs: map[string]string{"connector": "email", "status": status},
	}); err != nil {
		s.log.Warn("bus append", "topic", topic, "error", err)
	}
}

const (
	cursorName     = "email"
	pullBatch      = 50
	unhealthyAfter = 3
)

// Ingress polls the Worker spool and routes inbound mail (plan H). The
// daemon starts it in Start under the WaitGroup.
type Ingress struct {
	svc    *Service
	conn   *connectors.Service
	act    Actions
	log    *slog.Logger
	iv     time.Duration
	shadow bool

	mu        sync.Mutex
	pos       int64
	failures  int
	unhealthy bool
	lastError string
}

// NewIngress builds the poller. iv <= 0 means 30s; tests drive it fast.
func NewIngress(svc *Service, conn *connectors.Service, act Actions, iv time.Duration, shadow bool) *Ingress {
	if iv <= 0 {
		iv = 30 * time.Second
	}
	return &Ingress{
		svc: svc, conn: conn, act: act, iv: iv, shadow: shadow,
		log: slog.Default().With("source", "email.ingress"),
	}
}

// AttachIngress installs the loop so other holders of the service (the
// manual poll route) share the daemon's instance. The daemon calls it
// once in Start.
func (s *Service) AttachIngress(g *Ingress) {
	s.mu.Lock()
	s.ingress = g
	s.mu.Unlock()
}

// ErrNoIngress reports that the daemon has not attached the inbound
// pipeline to the service.
var ErrNoIngress = errors.New("email inbound pipeline not started")

// PollNow runs one poll cycle on the attached loop. The manual route
// reaches the same instance the daemon runs; it passes drain by design.
func (s *Service) PollNow(ctx context.Context) (int, error) {
	s.mu.RLock()
	g := s.ingress
	s.mu.RUnlock()
	if g == nil {
		return 0, ErrNoIngress
	}
	return g.PollOnce(ctx)
}

// Run polls until ctx is done. Shadow skips every automatic poll; drain
// skips them too. Manual polls through PollOnce are never blocked. The
// hosted transport has no spool to pull: the socket pushes instead.
func (g *Ingress) Run(ctx context.Context) {
	t := time.NewTicker(g.iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if g.shadow || !g.svc.Configured() || g.svc.Draining() {
				continue
			}
			if _, err := g.svc.Client(); err != nil {
				continue // hosted mail pushes; there is no spool to pull
			}
			if _, err := g.PollOnce(ctx); err != nil {
				g.log.Warn("email.poll", "error", err)
			}
		}
	}
}

// PollOnce runs one pull→record→route→ack cycle and returns how many
// messages it processed. The manual poll route calls it directly; it
// passes drain by design. The position advances only after the batch is
// durably recorded, never after a successful wake (plan H).
func (g *Ingress) PollOnce(ctx context.Context) (int, error) {
	client, err := g.svc.Client()
	if err != nil {
		return 0, err
	}
	cur, _ := g.conn.GetCursor(ctx, cursorName)
	after, _ := strconv.ParseInt(cur.LastEventID, 10, 64)
	msgs, _, err := client.Pull(ctx, after, pullBatch)
	if err != nil {
		g.noteFailure(err)
		return 0, err
	}
	n := 0
	var last int64
	for _, msg := range msgs {
		if err := g.deliver(ctx, msg); err != nil {
			// Not durably recorded: stop here, ack through the last
			// recorded seq, re-pull the rest on the next poll.
			break
		}
		last = msg.Seq
		n++
	}
	if last > 0 {
		// The cursor write is the durable record of position. Acking past
		// it would delete spool rows the next pull can no longer ask for,
		// so a cursor failure stops the cycle here (plan H).
		if err := g.conn.PutCursor(ctx, cursorName, strconv.FormatInt(last, 10)); err != nil {
			g.noteFailure(err)
			return n, fmt.Errorf("email cursor: %w", err)
		}
		g.mu.Lock()
		g.pos = last
		g.mu.Unlock()
	}
	// A failed known-set read must not ack an empty list: that clears the
	// Worker's filter (A.5). Ack the position alone and retry the set next
	// poll.
	known, err := g.svc.KnownLocalParts(ctx)
	if err != nil {
		g.log.Warn("email.known", "error", err)
	} else if err := client.Ack(ctx, last, known); err != nil {
		g.log.Warn("email.ack", "error", err) // re-pull is deduped by the row id
	}
	g.noteSuccess()
	return n, nil
}

// Deliver is the one inbound path for one message: resolve the address,
// route, record the envelope, run the side effects, and emit the thin
// frame. The hosted socket calls it and the bearer poll loop runs the
// same deliver per pulled message (plan 2026-09-22 Part D). An error
// means the envelope row is not durably recorded; a recorded message id
// never routes twice.
func (s *Service) Deliver(ctx context.Context, msg IngressMessage) error {
	s.mu.RLock()
	g := s.ingress
	s.mu.RUnlock()
	if g == nil {
		return ErrNoIngress
	}
	return g.deliver(ctx, msg)
}

// deliver classifies, records, and dispatches one message. An error means
// the envelope row is not durably recorded.
func (g *Ingress) deliver(ctx context.Context, msg IngressMessage) error {
	wsID := ""
	if wp, err := g.svc.WorkspaceByLocalPart(ctx, msg.LocalPart); err == nil {
		wsID = wp
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("resolve %s: %w", msg.LocalPart, err)
	}
	d := RouteDecision{Class: RouteDropped, Reason: ReasonNoAddress, DMARC: msg.Dmarc, TraceID: ulid.Make().String()}
	if wsID != "" {
		env, err := g.act.RouteEnvFor(ctx, wsID)
		if err != nil {
			return fmt.Errorf("route env %s: %w", wsID, err)
		}
		d = g.svc.Route(msg, wsID, env)
	}
	if d.Class == RouteAgent {
		if err := g.svc.resolveAgent(ctx, &d); err != nil {
			return err
		}
	}
	if err := g.participant(ctx, &msg, &d); err != nil {
		return err
	}
	if d.Class != RouteDropped && g.svc.seenInbound(ctx, msg, d) {
		d.Class, d.Reason = RouteDropped, ReasonDuplicate
	}
	room := MailboxRoom(d.WorkspaceID, d.Agent)
	if d.WorkspaceID != "" {
		msg.Thread = g.svc.threadFor(ctx, room, msg)
	}
	fresh, err := g.svc.RecordInbound(ctx, msg, d)
	if err != nil {
		return err
	}
	if !fresh {
		return nil // re-pull after a crash between record and ack
	}
	if d.WorkspaceID != "" && d.Class != RouteDropped && d.Class != RouteHeld {
		people := append(append([]string{msg.From}, msg.To...), msg.Cc...)
		g.svc.touchThread(ctx, room, ThreadRoot(msg), msg.Subject, people)
	}
	g.log.Info("email.received", "id", msg.ID, "disposition", d.Disposition(),
		"reason", d.Reason, "sender", msg.From, "tag", msg.Tag)
	u := untrustedFor(msg)
	if !g.dispatchReply(ctx, msg, d) {
		g.dispatch(ctx, msg, d, u)
	}
	if d.Class == RouteHeld {
		g.svc.emitHeld(ctx, msg.ID, d)
		return nil
	}
	if d.Class == RouteDropped {
		return nil // drops never fire triggers or stream
	}
	g.inject(ctx, msg, room)
	g.svc.emitReceived(ctx, msg.ID)
	return nil
}

// inject gives a routed mail to the connector event stream.
func (g *Ingress) inject(ctx context.Context, msg IngressMessage, room string) {
	if _, err := g.conn.InjectNative(ctx, cursorName, msg, connectors.NativeMeta{
		SourceID: msg.ID,
		Identity: strings.ToLower(strings.TrimSpace(msg.From)),
		Room:     room,
		Thread:   ThreadRoot(msg),
	}); err != nil {
		g.log.Warn("email.inject", "id", msg.ID, "error", err)
	}
}

// dispatch performs the side effects of one verdict: wake, ask, or the
// low-severity inbox items. A side-effect failure logs; it never fails
// the poll, because the envelope row is already durable.
func (g *Ingress) dispatch(ctx context.Context, msg IngressMessage, d RouteDecision, u *runs.UntrustedMessage) {
	switch d.Class {
	case RouteAgent, RouteParticipant:
		convID, err := g.act.Ask(ctx, d, msg, d.Agent, inboundText(msg, u, ""))
		if err != nil {
			g.log.Warn("email.ask", "agent", d.Agent, "id", msg.ID, "error", err)
		}
		g.svc.LinkInbound(ctx, msg.ID, "", convID, "")
	case RouteAssistant, RouteMisrouted:
		var convID, inboxID string
		var err error
		if d.ToAssistant {
			if convID, err = g.act.Ask(ctx, d, msg, "", inboundText(msg, u, d.misroutedNote(msg.Tag))); err != nil {
				g.log.Warn("email.ask", "id", msg.ID, "error", err)
			}
		}
		if d.Class == RouteMisrouted {
			if inboxID, err = g.act.InboxItem(ctx, d, msg); err != nil {
				g.log.Warn("email.inbox", "id", msg.ID, "error", err)
			}
		}
		g.svc.LinkInbound(ctx, msg.ID, "", convID, inboxID)
	case RouteHeld:
		inboxID, err := g.act.InboxItem(ctx, d, msg)
		if err != nil {
			g.log.Warn("email.inbox", "id", msg.ID, "error", err)
			return
		}
		g.svc.LinkInbound(ctx, msg.ID, "", "", inboxID)
	case RouteDropped:
		if !d.Spoof() {
			return
		}
		inboxID, err := g.act.InboxItem(ctx, d, msg)
		if err != nil {
			g.log.Warn("email.inbox", "id", msg.ID, "error", err)
			return
		}
		g.svc.LinkInbound(ctx, msg.ID, "", "", inboxID)
	}
}

// noteFailure counts consecutive poll failures and fires
// connector.unhealthy after the third (plan H).
func (g *Ingress) noteFailure(err error) {
	g.mu.Lock()
	g.failures++
	g.lastError = err.Error()
	fire := g.failures >= unhealthyAfter && !g.unhealthy
	if fire {
		g.unhealthy = true
	}
	pos, failures, lastErr := g.pos, g.failures, g.lastError
	g.mu.Unlock()
	if fire {
		g.svc.emitConnectorState("connector.unhealthy", err.Error())
	}
	g.svc.SetPollState(PollState{Position: pos, Failures: failures, LastError: lastErr,
		LastPoll: time.Now().UTC().Format(time.RFC3339)})
}

// noteSuccess clears the failure count and fires connector.recovered on
// the first success after an unhealthy stretch.
func (g *Ingress) noteSuccess() {
	g.mu.Lock()
	recovered := g.unhealthy
	g.failures, g.unhealthy, g.lastError = 0, false, ""
	pos := g.pos
	g.mu.Unlock()
	if recovered {
		g.svc.emitConnectorState("connector.recovered", "ok")
	}
	g.svc.SetPollState(PollState{Position: pos, LastPoll: time.Now().UTC().Format(time.RFC3339)})
}

// addrListed matches one address against a list, case-insensitive.
func addrListed(list []string, addr string) bool {
	return slices.ContainsFunc(list, func(a string) bool {
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(addr))
	})
}

// untrustedFor maps one inbound message to the wake fence block (A.11).
func untrustedFor(msg IngressMessage) *runs.UntrustedMessage {
	u := &runs.UntrustedMessage{RowID: msg.ID, MessageID: msg.MessageID, From: msg.From,
		Subject: msg.Subject, Body: msg.Text}
	for _, a := range msg.Attachments {
		u.Attachments = append(u.Attachments, runs.UntrustedAttachment{
			ID: a.ID, Name: a.Name, Size: a.Size, Type: a.Type,
		})
	}
	return u
}

// misroutedNote is the mark the assistant sees on a misrouted message, or "".
func (d RouteDecision) misroutedNote(tag string) string {
	switch {
	case d.Class != RouteMisrouted:
		return ""
	case d.Reason == ReasonNotAccepted:
		return "Misrouted email for +" + tag + ": the agent with that ID does not accept this sender. " +
			"Its email.accept list is empty or does not name the sender. The original tag is kept.\n\n"
	default:
		return "Misrouted email for +" + tag + ": no agent in this workspace has that tag. The original tag is kept.\n\n"
	}
}

// inboundText composes the conversation message: the note when set, a
// header line, and the untrusted fence (A.8, A.13).
func inboundText(msg IngressMessage, u *runs.UntrustedMessage, note string) string {
	var b strings.Builder
	b.WriteString(note)
	b.WriteString("Email from " + msg.From + ": " + msg.Subject + "\n\n")
	b.WriteString(u.Fence())
	return b.String()
}
