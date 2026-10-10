package api

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/artifacts"
	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/decisions/policies"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeEmail(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/email/status", s.handleEmailStatus)
	mux.HandleFunc("POST /v1/email/test", s.handleEmailTest)
	mux.HandleFunc("POST /v1/email/send", s.handleEmailSend)
	mux.HandleFunc("GET /v1/email/messages", s.handleEmailMessages)
	mux.HandleFunc("GET /v1/email/messages/{id}", s.handleEmailMessage)
	mux.HandleFunc("GET /v1/email/messages/{id}/attachments/{aid}", s.handleEmailAttachment)
	mux.HandleFunc("GET /v1/email/held", s.handleEmailHeld)
	mux.HandleFunc("POST /v1/email/messages/{id}/release", s.handleEmailRelease)
	mux.HandleFunc("POST /v1/email/messages/{id}/ignore", s.handleEmailIgnore)
	mux.HandleFunc("POST /v1/email/poll", s.handleEmailPoll)
	mux.HandleFunc("GET /v1/workspaces/{id}/email", s.handleWorkspaceEmail)
	mux.HandleFunc("POST /v1/workspaces/{id}/email/rotate", s.handleWorkspaceEmailRotate)
	mux.HandleFunc("GET /v1/workspaces/{id}/agents/{name}/email", s.handleAgentEmail)
}

func (s *Server) handleEmailStatus(w http.ResponseWriter, r *http.Request) {
	st := s.email.Status(r.Context())
	// "ok | missing" answers why the channel is off. A cloud link whose
	// plan lacks hosted_ingress is one reason (plan 2026-09-22 Part D).
	if !st.Configured && s.cloud != nil {
		if ent := s.cloud.Entitlement(); ent.Linked && !ent.Has(entitle.HostedIngress) {
			st.Entitlement = "missing"
		}
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleEmailTest(w http.ResponseWriter, r *http.Request) {
	msgID, err := s.email.Test(r.Context())
	if err != nil {
		writeEmailErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "message_id": msgID})
}

// emailSendBodyCap fits the base64 of a full-size message plus the JSON.
const emailSendBodyCap = 8 << 20

// handleEmailSend is the explicit send (plan I.3): one message from the
// workspace address, or the agent subaddress when agent is set. Recipients
// outside the operator list and the workspace allowlist are a 400.
// emailArtifact resolves the artifact of a send in the workspace of the
// caller.
func (s *Server) emailArtifact(r *http.Request, ws string, in request.EmailArtifact) (*email.ArtifactRef, error) {
	owner, err := s.artifacts.OriginFor(r.Context(), artifactActor(r), "", "")
	if err != nil {
		return nil, err
	}
	a, n, err := s.artifacts.Resolve(r.Context(), ws, owner, in.Ref)
	if err != nil {
		return nil, err
	}
	n = cmp.Or(in.Version, n, a.LatestVersion)
	if _, err := s.artifacts.Version(r.Context(), a.ID, n); err != nil {
		return nil, err
	}
	if !a.Live {
		return &email.ArtifactRef{ID: a.ID, Version: n, URL: artifacts.PageURL(a.ID, n, "")}, nil
	}
	// A live artifact sends its newest snapshot; with none, it refreshes once.
	sn, err := s.artifacts.LatestSnapshot(r.Context(), a.ID)
	if errors.Is(err, artifacts.ErrNotFound) {
		sn, err = s.artifacts.Refresh(r.Context(), artifacts.RefreshOpts{ID: a.ID, Reason: artifacts.ReasonSend,
			Actor: artifacts.OriginUser})
	}
	if err != nil {
		return nil, err
	}
	return &email.ArtifactRef{ID: a.ID, Version: sn.VersionN, SnapshotID: sn.ID, URL: artifacts.PageURL(a.ID, sn.VersionN, sn.ID)}, nil
}

func (s *Server) handleEmailSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Workspace   string                 `json:"workspace"`
		Agent       string                 `json:"agent"`
		To          []string               `json:"to"`
		Subject     string                 `json:"subject"`
		Text        string                 `json:"text"`
		HTML        string                 `json:"html"`
		InReplyTo   string                 `json:"in_reply_to"`
		Attachments []request.EmailFile    `json:"attachments"`
		Artifact    *request.EmailArtifact `json:"artifact"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, emailSendBodyCap)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if strings.TrimSpace(body.Workspace) == "" || len(body.To) == 0 ||
		(strings.TrimSpace(body.Subject) == "" && body.Artifact == nil) {
		writeErr(w, http.StatusBadRequest, "validation",
			"workspace, to, and subject are required")
		return
	}
	ws, err := s.ws.Resolve(r.Context(), body.Workspace)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var recipientRule *email.AcceptRule
	if actor, ok := tools.ActorFrom(r.Context()); ok && actor.Kind == "agent" {
		if actor.WorkspaceID != ws.ID {
			writeForbidden(w, "agent may send only from its own workspace")
			return
		}
		caller := request.AgentIndexName(strings.TrimPrefix(actor.Job, "agents/"))
		if body.Agent != "" && request.AgentIndexName(body.Agent) != caller {
			writeForbidden(w, "agent may send only as itself")
			return
		}
		body.Agent = strings.TrimPrefix(caller, "agents/")
		if env, err := s.email.RouteEnvFor(r.Context(), ws.ID); err == nil {
			rule := env.Agents[body.Agent]
			recipientRule = &rule
		}
	} else if actor, ok := tools.ActorFrom(r.Context()); ok && actor.Kind == "job" {
		if actor.WorkspaceID != ws.ID {
			writeForbidden(w, "job may send only from its own workspace")
			return
		}
		// An owned job sends as its owner agent.
		if owner := job.OwnerOf(actor.Job); owner != "" {
			if body.Agent != "" && request.AgentIndexName(body.Agent) != owner {
				writeForbidden(w, "job may send only as its owner agent")
				return
			}
			body.Agent = strings.TrimPrefix(owner, "agents/")
		}
	} else if actor, ok := tools.ActorFrom(r.Context()); ok && actor.Kind == "assistant" {
		if actor.WorkspaceID != ws.ID {
			writeForbidden(w, "assistant may send only from its own workspace")
			return
		}
		if env, err := s.email.RouteEnvFor(r.Context(), ws.ID); err == nil {
			rule := env.Assistant
			recipientRule = &rule
		}
	}
	if s.email.Configured() {
		if _, err := s.email.EnsureAddress(r.Context(), ws.ID, ws.Handle); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	files, err := s.emailFiles(r.Context(), ws, body.Attachments)
	if err != nil {
		writeEmailErr(w, err)
		return
	}
	opts := email.SendOpts{
		WorkspaceID: ws.ID, Agent: body.Agent, To: body.To,
		Subject: body.Subject, Text: body.Text, HTML: body.HTML, Attachments: files,
		InReplyTo: body.InReplyTo, Recipients: recipientRule,
	}
	if body.Artifact != nil {
		ref, err := s.emailArtifact(r, ws.ID, *body.Artifact)
		if err != nil {
			writeArtifactErr(w, err)
			return
		}
		opts.Artifact = ref
	}
	if actor, ok := tools.ActorFrom(r.Context()); ok {
		opts.RunID = actor.RunID
	}
	if body.Agent != "" {
		opts.FromName = body.Agent
		if row, err := s.findAgentRow(r.Context(), ws.ID, body.Agent); err == nil {
			if parsed, err := job.ParseFile(row.Path); err == nil && parsed != nil && parsed.Title != "" {
				opts.FromName = parsed.Title
			}
		}
	} else {
		opts.FromName = ws.SenderName()
	}
	queued, err := s.email.Send(r.Context(), opts)
	if err != nil {
		writeEmailErr(w, err)
		return
	}
	// One attempt now, so the caller sees the first error at once. A failed
	// attempt stays queued and the outbox worker retries it.
	row, err := s.outbox.SendNow(r.Context(), queued.OutboxID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := map[string]any{
		"sent": row.Status == outbox.StatusSent, "message_id": queued.MessageID,
		"outbox_id": row.ID, "status": row.Status,
	}
	if row.ErrorMessage != "" {
		resp["error"] = row.ErrorMessage
	}
	writeJSON(w, http.StatusOK, resp)
}

// emailFiles resolves the files of one send. A path reads a file inside
// the workspace; otherwise the content is base64.
func (s *Server) emailFiles(ctx context.Context, ws *workspaces.Workspace, in []request.EmailFile) ([]email.SendAttachment, error) {
	if len(in) > email.MaxAttachments {
		return nil, fmt.Errorf("%w: at most %d files", email.ErrContent, email.MaxAttachments)
	}
	out := make([]email.SendAttachment, 0, len(in))
	for _, f := range in {
		if f.Path != "" {
			data, info, err := s.files.Read(ctx, ws.Path, f.Path)
			if err != nil {
				return nil, fmt.Errorf("%w: workspace file %s: %w", email.ErrContent, f.Path, err)
			}
			out = append(out, email.SendAttachment{
				Name: cmp.Or(f.Name, path.Base(info.Path)), Type: f.Type, Content: data,
			})
			continue
		}
		data, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			return nil, fmt.Errorf("%w: file %q content is not valid base64", email.ErrContent, f.Name)
		}
		out = append(out, email.SendAttachment{Name: f.Name, Type: f.Type, Content: data})
	}
	return out, nil
}

// handleEmailMessages is the envelope log (plan K): newest first, filtered
// by direction, workspace, or thread, cursor-paginated by row id.
func (s *Server) handleEmailMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := email.MessageListOpts{
		Direction: q.Get("direction"),
		Workspace: s.workspaceID(r.Context(), q.Get("workspace")),
		Thread:    q.Get("thread"),
		Limit:     parseLimit(r),
		Cursor:    q.Get("cursor"),
	}
	switch opts.Direction {
	case "", "in", "out":
	default:
		writeErr(w, http.StatusBadRequest, "validation", "direction must be in or out")
		return
	}
	if opts.Cursor != "" {
		if _, err := ulid.Parse(opts.Cursor); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "malformed cursor: not a message id")
			return
		}
	}
	list, err := s.email.ListMessages(r.Context(), opts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if list == nil {
		list = []email.Message{}
	}
	resp := map[string]any{"messages": list}
	if len(list) == opts.Limit {
		resp["next_cursor"] = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleEmailMessage returns one envelope with its verdicts and links.
func (s *Server) handleEmailMessage(w http.ResponseWriter, r *http.Request) {
	m, err := s.email.GetMessage(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "message not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleEmailAttachment streams one attachment's bytes through the Worker
// (plan A.11: the fetch is explicit; the Worker logs it). Bytes never
// touch the store. Names and types come from untrusted mail, so both pass
// through media-type formatting.
func (s *Server) handleEmailAttachment(w http.ResponseWriter, r *http.Request) {
	m, err := s.email.GetMessage(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "message not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var meta email.Attachment
	found := false
	for _, a := range m.Attachments {
		if a.ID == r.PathValue("aid") {
			meta, found = a, true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "attachment not on this message")
		return
	}
	body, remoteType, err := s.email.Attachment(r.Context(), m.MessageID+"/"+meta.ID)
	if err != nil {
		writeEmailErr(w, err)
		return
	}
	ctype := meta.Type
	if _, _, perr := mime.ParseMediaType(ctype); perr != nil {
		ctype = remoteType
	}
	if _, _, perr := mime.ParseMediaType(ctype); perr != nil {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": meta.Name}))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleEmailPoll pulls the Worker spool now, outside the tick (plan K).
// It is a manual action, so it passes drain by design.
func (s *Server) handleEmailPoll(w http.ResponseWriter, r *http.Request) {
	n, err := s.email.PollNow(r.Context())
	switch {
	case errors.Is(err, email.ErrNoIngress):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case err != nil:
		writeEmailErr(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"pulled": n})
	}
}

// handleWorkspaceEmail returns the stored address, masked unless reveal=1.
// With the channel configured it lazily mints, so a workspace added before
// the block existed still answers.
func (s *Server) handleWorkspaceEmail(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.emailWorkspace(w, r)
	if !ok {
		return
	}
	if !s.email.Configured() {
		writeErr(w, http.StatusNotFound, "not_found", "email is not configured")
		return
	}
	reveal := r.URL.Query().Get("reveal") == "1"
	if _, err := s.email.EnsureAddress(r.Context(), ws.ID, ws.Handle); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var (
		a   email.Address
		err error
	)
	if reveal {
		a, err = s.email.Address(r.Context(), ws.ID)
	} else {
		a, err = s.email.Masked(r.Context(), ws.ID)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, emailAddressPayload(a, !reveal))
}

// handleWorkspaceEmailRotate mints a new address. An optional handle
// replaces the workspace handle first; this is the only way to change it.
// The response carries the full new address: the operator acted, so the
// mask has no use here.
func (s *Server) handleWorkspaceEmailRotate(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.emailWorkspace(w, r)
	if !ok {
		return
	}
	if !s.email.Configured() {
		writeErr(w, http.StatusNotFound, "not_found", "email is not configured")
		return
	}
	var body struct {
		Handle string `json:"handle"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Handle != "" && body.Handle != ws.Handle {
		updated, err := s.ws.SetHandle(r.Context(), ws.ID, body.Handle)
		if errors.Is(err, workspaces.ErrHandle) {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		ws = updated
	}
	a, err := s.email.Rotate(r.Context(), ws.ID, ws.Handle)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, emailAddressPayload(a, false))
}

// handleAgentEmail returns the agent subaddress (plan 2026-09-24 B.3):
// masked unless reveal=1, full on reveal. The UI copies from here, so the
// daemon stays the one owner of the address format. The reveal rides the
// same audited request log as the workspace reveal, and a scoped agent run
// is rejected: a run token may not lift the mask.
func (s *Server) handleAgentEmail(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	ws, ok := s.emailWorkspace(w, r)
	if !ok {
		return
	}
	if !s.email.Configured() {
		writeErr(w, http.StatusNotFound, "not_found", "email is not configured")
		return
	}
	row, err := s.findAgentRow(r.Context(), ws.ID, r.PathValue("name"))
	if err != nil {
		writeResolveErr(w, err, "agent")
		return
	}
	name := strings.TrimPrefix(row.Name, "agents/")
	// Lazy mint, as the workspace reveal: a workspace added before the
	// block existed still answers.
	if _, err := s.email.EnsureAddress(r.Context(), ws.ID, ws.Handle); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var a email.Address
	reveal := r.URL.Query().Get("reveal") == "1"
	if reveal {
		a, err = s.email.AgentAddressFull(r.Context(), ws.ID, name)
	} else {
		a, err = s.email.AgentAddressMasked(r.Context(), ws.ID, name)
	}
	if err != nil {
		writeEmailErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, emailAddressPayload(a, !reveal))
}

// emailWorkspace resolves the {id} path value (id or name) to a workspace.
func (s *Server) emailWorkspace(w http.ResponseWriter, r *http.Request) (*workspaces.Workspace, bool) {
	ws, err := s.ws.Resolve(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return nil, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, false
	}
	return ws, true
}

func emailAddressPayload(a email.Address, masked bool) map[string]any {
	return map[string]any{
		"workspace_id": a.WorkspaceID,
		"local_part":   a.LocalPart,
		"domain":       a.Domain,
		"address":      a.Full(),
		"masked":       masked,
		"created_at":   a.CreatedAt,
		"rotated_at":   a.RotatedAt,
		"revision":     a.Revision,
	}
}

// writeEmailErr maps the service sentinels to stable codes.
func writeEmailErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, email.ErrNotHeld):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, email.ErrNotConfigured), errors.Is(err, email.ErrRecipient), errors.Is(err, email.ErrContent):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", "workspace has no email address")
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// ensureWorkspaceEmail is the workspace-add hook: mint on add when the
// channel is live. A failure logs; it never fails the add.
func (s *Server) ensureWorkspaceEmail(ctx context.Context, wsID, handle string) {
	if !s.email.Configured() {
		return
	}
	if _, err := s.email.EnsureAddress(ctx, wsID, handle); err != nil {
		s.log.Warn("email.mint_on_add", "workspace_id", wsID, "err", err.Error())
	}
}

func (s *Server) handleEmailHeld(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.artifactWorkspace(w, r, r.URL.Query().Get("workspace"))
	if !ok {
		return
	}
	list, err := s.email.Held(r.Context(), ws)
	if err != nil {
		writeEmailErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

// heldMessage loads a held mail for a release or an ignore.
func (s *Server) heldMessage(w http.ResponseWriter, r *http.Request) (email.Message, bool) {
	m, err := s.email.HeldMessage(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "message not found")
		return m, false
	}
	if err != nil {
		writeEmailErr(w, err)
		return m, false
	}
	return m, true
}

// heldDecision returns the decision that held m, or false when no
// decision held it.
func (s *Server) heldDecision(ctx context.Context, m email.Message) (decisions.Decision, bool) {
	if m.DecisionID == "" {
		return decisions.Decision{}, false
	}
	d, err := s.decisions.Get(ctx, m.DecisionID)
	if err != nil || d.Status != decisions.StatusOK || d.Outcome != policies.EmailHold {
		return decisions.Decision{}, false
	}
	return d, true
}

func (s *Server) handleEmailRelease(w http.ResponseWriter, r *http.Request) {
	if personOnly(w, r, "only a person can release held mail") {
		return
	}
	var body struct {
		Scope string `json:"scope"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	scope := cmp.Or(r.URL.Query().Get("scope"), body.Scope, email.ReleaseOnce)
	m, ok := s.heldMessage(w, r)
	if !ok {
		return
	}
	entry, err := email.AcceptEntry(scope, m.Sender)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	d, risky := s.heldDecision(r.Context(), m)
	if risky && !confirmed(r) {
		writeErrDetails(w, http.StatusBadRequest, "validation", fmt.Sprintf(
			"The safety check found a risk in this mail: %s. Let %s read it? Repeat it with confirm=1 once the user agrees",
			strings.Join(d.Reasons, ", "), m.AgentName), map[string]any{"confirm_required": true})
		return
	}
	if entry != "" {
		if err := s.addEmailAccept(r.Context(), m.WorkspaceID, m.AgentName, entry); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	out, err := s.email.Release(r.Context(), m.ID, scope)
	if err != nil {
		writeEmailErr(w, err)
		return
	}
	s.closeHeld(r.Context(), m, risky, decisions.FeedbackDisagree, "released")
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleEmailIgnore(w http.ResponseWriter, r *http.Request) {
	if personOnly(w, r, "only a person can ignore held mail") {
		return
	}
	m, ok := s.heldMessage(w, r)
	if !ok {
		return
	}
	out, err := s.email.Ignore(r.Context(), m.ID)
	if err != nil {
		writeEmailErr(w, err)
		return
	}
	_, risky := s.heldDecision(r.Context(), m)
	s.closeHeld(r.Context(), m, risky, decisions.FeedbackAgree, "ignored")
	writeJSON(w, http.StatusOK, out)
}

// closeHeld resolves the inbox item of a held mail. When a decision held
// the mail, the choice of the user is feedback on that decision.
func (s *Server) closeHeld(ctx context.Context, m email.Message, decided bool, feedback, how string) {
	if err := s.alerts.ResolveSubject(ctx, "email/held/"+m.ID, how); err != nil {
		s.log.Warn("email.held.resolve", "id", m.ID, "err", err.Error())
	}
	if decided {
		if _, err := s.decisions.Feedback(ctx, m.DecisionID, feedback, how); err != nil {
			s.log.Warn("email.held.feedback", "id", m.ID, "err", err.Error())
		}
	}
}

// addEmailAccept adds entry to email.accept of the agent.
func (s *Server) addEmailAccept(ctx context.Context, wsID, agent, entry string) error {
	ws, err := s.ws.Get(ctx, wsID)
	if err != nil {
		return err
	}
	row, err := s.findAgentRow(ctx, wsID, agent)
	if err != nil {
		return err
	}
	j, err := job.ParseFile(row.Path)
	if err != nil {
		return err
	}
	if slices.Contains(j.EmailAccept, entry) {
		return nil
	}
	accept := append(slices.Clone(j.EmailAccept), entry)
	if err := job.MergePatchFrontmatter(row.Path, map[string]any{"email_accept": accept}); err != nil {
		return fmt.Errorf("agent %s email.accept: %w", agent, err)
	}
	return s.ws.RebuildIndex(ctx, ws)
}
