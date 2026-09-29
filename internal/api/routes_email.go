package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/oklog/ulid/v2"

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

// handleEmailSend is the explicit send (plan I.3): one message from the
// workspace address, or the agent subaddress when agent is set. Recipients
// outside the operator list and the workspace allowlist are a 400.
func (s *Server) handleEmailSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Workspace string   `json:"workspace"`
		Agent     string   `json:"agent"`
		To        []string `json:"to"`
		Subject   string   `json:"subject"`
		Text      string   `json:"text"`
		InReplyTo string   `json:"in_reply_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if strings.TrimSpace(body.Workspace) == "" || len(body.To) == 0 ||
		strings.TrimSpace(body.Subject) == "" || strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, "validation",
			"workspace, to, subject, and text are required")
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
	opts := email.SendOpts{
		WorkspaceID: ws.ID, Agent: body.Agent, To: body.To,
		Subject: body.Subject, Text: body.Text, InReplyTo: body.InReplyTo, Recipients: recipientRule,
	}
	if actor, ok := tools.ActorFrom(r.Context()); ok {
		opts.RunID = actor.RunID
	}
	if body.Agent != "" {
		if row, err := s.findAgentRow(r.Context(), ws.ID, body.Agent); err == nil {
			if parsed, err := job.ParseFile(row.Path); err == nil && parsed != nil && parsed.Title != "" {
				opts.FromName = parsed.Title
			}
		}
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
	case errors.Is(err, email.ErrNotConfigured), errors.Is(err, email.ErrRecipient):
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
