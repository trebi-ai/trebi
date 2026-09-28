package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/outbox"
)

func (s *Server) routeConversations(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/conversations", s.handleConversations)
	mux.HandleFunc("GET /v1/workspaces/{id}/agents/{name}/conversations", s.handleAgentConversations)
	mux.HandleFunc("POST /v1/conversations", s.handleConversationCreate)
	mux.HandleFunc("GET /v1/conversations/{id}", s.handleConversationGet)
	mux.HandleFunc("PATCH /v1/conversations/{id}", s.handleConversationPatch)
	mux.HandleFunc("DELETE /v1/conversations/{id}", s.handleConversationDelete)
	mux.HandleFunc("GET /v1/conversations/{id}/messages", s.handleConversationMessages)
	mux.HandleFunc("POST /v1/conversations/{id}/messages", s.handleConversationSend)
	mux.HandleFunc("GET /v1/conversations/{id}/events", s.handleConversationEvents)
	mux.HandleFunc("POST /v1/conversations/{id}/interrupt", s.handleConversationInterrupt)
	mux.HandleFunc("POST /v1/conversations/{id}/permissions/{pid}", s.handleConversationPermission)
	mux.HandleFunc("POST /v1/conversations/{id}/mode", s.handleConversationMode)
	mux.HandleFunc("POST /v1/conversations/{id}/close", s.handleConversationClose)
	mux.HandleFunc("POST /v1/conversations/{id}/revert", s.handleConversationRevert)
	mux.HandleFunc("GET /v1/conversations/{id}/outbox", s.handleConversationOutbox)
	mux.HandleFunc("POST /v1/conversations/{id}/outbox/{row}/retry", s.handleConversationOutboxRetry)
	mux.HandleFunc("GET /v1/channel-pointers/{type}/{ref}", s.handlePointerGet)
	mux.HandleFunc("PUT /v1/channel-pointers/{type}/{ref}", s.handlePointerPut)
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	cursor, ok := parseULIDCursor(w, r, "conversation")
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := s.conv.List(r.Context(), conversations.ListOpts{
		Workspace: q.Get("workspace"),
		Channel:   q.Get("channel"),
		Operator:  q.Get("operator"),
		Limit:     parseLimit(r),
		Cursor:    cursor,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.writeConversations(r.Context(), w, list, len(list) == parseLimit(r))
}

// writeConversations renders the list payload with sessions attached.
func (s *Server) writeConversations(ctx context.Context, w http.ResponseWriter, list []conversations.Conversation, truncated bool) {
	rows := make([]conversationRow, len(list))
	for i := range list {
		rows[i] = s.assistantRow(ctx, &list[i])
	}
	resp := map[string]any{"conversations": rows}
	if truncated {
		resp["next_cursor"] = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleConversationGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.conv.Get(r.Context(), r.PathValue("id"))
	if s.convErr(w, err, "conversation not found") {
		return
	}
	writeJSON(w, http.StatusOK, s.assistantRow(r.Context(), c))
}

func (s *Server) handleConversationCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Workspace     string                  `json:"workspace"`
		Operator      string                  `json:"operator"`
		Title         string                  `json:"title"`
		OriginChannel string                  `json:"origin_channel"`
		Harness       string                  `json:"harness"`
		Mode          string                  `json:"mode"`
		Activate      *conversations.Activate `json:"activate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	operator := body.Operator
	if operator != "" {
		// Harness and mode pin the first assistant prompt only (plan F.2).
		if body.Harness != "" || body.Mode != "" {
			writeErr(w, http.StatusBadRequest, "validation", "harness and mode apply only to assistant conversations")
			return
		}
		short := strings.TrimPrefix(operator, "agents/")
		row, err := s.findAgentRow(r.Context(), body.Workspace, short)
		if err != nil {
			writeResolveErr(w, err, "agent")
			return
		}
		operator = row.Name
	} else {
		if body.Harness != "" && !assistant.KnownHarness(body.Harness) {
			writeErr(w, http.StatusBadRequest, "validation", fmt.Sprintf("unknown harness %q", body.Harness))
			return
		}
		switch body.Mode {
		case "", assistant.ModeAsk, assistant.ModeAutoEdit, assistant.ModeAuto:
		default:
			writeErr(w, http.StatusBadRequest, "validation", fmt.Sprintf("mode %q not in [ask, auto_edit, auto]", body.Mode))
			return
		}
	}
	c, err := s.conv.Create(r.Context(), conversations.CreateOpts{
		Workspace: body.Workspace, Title: body.Title,
		Operator: operator, OriginChannel: body.OriginChannel, Activate: body.Activate,
	})
	if s.convErr(w, err, "workspace not found") {
		return
	}
	// The picked harness and mode sit on the session row, so the manager
	// reads them at the first prompt (plan F.2: the process starts then).
	if operator == "" && (body.Harness != "" || body.Mode != "") {
		mode := body.Mode
		if mode == "" {
			mode = assistant.ModeAutoEdit
		}
		rec := assistant.Record{
			ConversationID: c.ID, WorkspaceID: c.Workspace,
			Harness: body.Harness, Mode: mode, Status: assistant.StatusClosed,
		}
		if err := s.asstStore.Create(r.Context(), rec); err != nil && !errors.Is(err, assistant.ErrNoSession) {
			s.log.Warn("assistant session pin", "conversation", c.ID, "error", err)
		}
	}
	writeJSON(w, http.StatusCreated, s.assistantRow(r.Context(), c))
}

func (s *Server) handleAgentConversations(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	row, err := s.findAgentRow(r.Context(), ws.ID, r.PathValue("name"))
	if err != nil {
		writeResolveErr(w, err, "agent")
		return
	}
	cursor, ok := parseULIDCursor(w, r, "conversation")
	if !ok {
		return
	}
	list, err := s.conv.List(r.Context(), conversations.ListOpts{
		Workspace: ws.ID, Operator: row.Name,
		Limit: parseLimit(r), Cursor: cursor,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := map[string]any{"conversations": list}
	if len(list) == parseLimit(r) {
		resp["next_cursor"] = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleConversationPatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title    *string `json:"title"`
		Summary  *string `json:"summary"`
		Revision int64   `json:"revision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	c, err := s.conv.Patch(r.Context(), r.PathValue("id"), conversations.PatchOpts{
		Title: body.Title, Summary: body.Summary, Revision: body.Revision,
	})
	if s.convErr(w, err, "conversation not found") {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleConversationMessages(w http.ResponseWriter, r *http.Request) {
	cursor, ok := parseULIDCursor(w, r, "message")
	if !ok {
		return
	}
	limit := parseLimit(r)
	list, err := s.conv.ListMessages(r.Context(), r.PathValue("id"), conversations.MessageListOpts{
		Limit: limit, Cursor: cursor,
	})
	if s.convErr(w, err, "conversation not found") {
		return
	}
	s.email.Decorate(r.Context(), list)
	resp := map[string]any{"messages": list}
	if len(list) == limit {
		resp["next_cursor"] = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// conversationOwned rejects an agent caller on a conversation it does not
// operate, and an assistant caller on another workspace. It reports true
// when the handler may go on.
func (s *Server) conversationOwned(w http.ResponseWriter, r *http.Request) bool {
	if scope, ok := scopeFrom(r.Context()); ok {
		c, err := s.conv.Get(r.Context(), r.PathValue("id"))
		if err != nil || c.Workspace != scope.workspaceID || c.Operator != scope.agent {
			writeForbidden(w, "agent may act only on its own conversation")
			return false
		}
		return true
	}
	if c, err := s.conv.Get(r.Context(), r.PathValue("id")); err == nil {
		return s.assistantScoped(w, r, c.Workspace)
	}
	return true
}

func (s *Server) handleConversationSend(w http.ResponseWriter, r *http.Request) {
	if !s.conversationOwned(w, r) {
		return
	}
	var body struct {
		Text           string `json:"text"`
		ChannelType    string `json:"channel_type"`
		ChannelRef     string `json:"channel_ref"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	out, err := s.conv.Send(r.Context(), r.PathValue("id"), conversations.SendOpts{
		Text: body.Text, ChannelType: body.ChannelType,
		ChannelRef: body.ChannelRef, IdempotencyKey: body.IdempotencyKey,
	})
	if s.convErr(w, err, "conversation not found") {
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handlePointerGet(w http.ResponseWriter, r *http.Request) {
	workspace := r.URL.Query().Get("workspace")
	if workspace == "" {
		writeErr(w, http.StatusBadRequest, "validation", "workspace is required")
		return
	}
	p, err := s.conv.GetPointer(r.Context(), workspace, r.PathValue("type"), r.PathValue("ref"))
	if s.convErr(w, err, "pointer not found") {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handlePointerPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Workspace      string `json:"workspace"`
		ConversationID string `json:"conversation_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.ConversationID == "" {
		writeErr(w, http.StatusBadRequest, "validation", "conversation_id is required")
		return
	}
	p, err := s.conv.PutPointer(r.Context(), body.Workspace, r.PathValue("type"), r.PathValue("ref"), body.ConversationID)
	if s.convErr(w, err, "conversation not found") {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) convErr(w http.ResponseWriter, err error, notFoundMsg string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, conversations.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", notFoundMsg)
	case errors.Is(err, conversations.ErrOperatorConflict):
		writeErr(w, http.StatusConflict, "conflict", "channel belongs to a different operator")
	case errors.Is(err, conversations.ErrConflict):
		// A stale revision reads like every other document: 412.
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", "revision conflict")
	case errors.Is(err, conversations.ErrValidation):
		writeErr(w, http.StatusBadRequest, "validation", strings.TrimPrefix(err.Error(), "validation: "))
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
	return true
}

// handleConversationOutbox lists the outbox rows of one conversation, newest
// first. This is a support read.
func (s *Server) handleConversationOutbox(w http.ResponseWriter, r *http.Request) {
	if !s.conversationOwned(w, r) {
		return
	}
	if _, err := s.conv.Get(r.Context(), r.PathValue("id")); s.convErr(w, err, "conversation not found") {
		return
	}
	rows, _, err := s.outbox.List(r.Context(), outbox.Filter{ConversationID: r.PathValue("id"), Limit: 500})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outbox": rows})
}

// handleConversationOutboxRetry sets one failed row of a conversation back
// to pending.
func (s *Server) handleConversationOutboxRetry(w http.ResponseWriter, r *http.Request) {
	if !s.conversationOwned(w, r) {
		return
	}
	row, err := s.outbox.Get(r.Context(), r.PathValue("row"))
	if err == nil && row.ConversationID != r.PathValue("id") {
		err = outbox.ErrNotFound
	}
	if err == nil {
		row, err = s.outbox.Retry(r.Context(), row.ID)
	}
	writeOutboxResult(w, row, err, "outbox row is not failed")
}
