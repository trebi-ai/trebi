package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/trebi-ai/trebi/internal/conversations"
)

func (s *Server) routeConversations(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/conversations", s.handleConversations)
	mux.HandleFunc("POST /v1/conversations", s.handleConversationCreate)
	mux.HandleFunc("GET /v1/conversations/{id}", s.handleConversationGet)
	mux.HandleFunc("PATCH /v1/conversations/{id}", s.handleConversationPatch)
	mux.HandleFunc("GET /v1/conversations/{id}/messages", s.handleConversationMessages)
	mux.HandleFunc("POST /v1/conversations/{id}/messages", s.handleConversationSend)
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
		Limit:     parseLimit(r),
		Cursor:    cursor,
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

func (s *Server) handleConversationGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.conv.Get(r.Context(), r.PathValue("id"))
	if s.convErr(w, err, "conversation not found") {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleConversationCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Workspace     string                  `json:"workspace"`
		Title         string                  `json:"title"`
		OriginChannel string                  `json:"origin_channel"`
		Activate      *conversations.Activate `json:"activate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	c, err := s.conv.Create(r.Context(), conversations.CreateOpts{
		Workspace: body.Workspace, Title: body.Title,
		OriginChannel: body.OriginChannel, Activate: body.Activate,
	})
	if s.convErr(w, err, "workspace not found") {
		return
	}
	writeJSON(w, http.StatusCreated, c)
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
	resp := map[string]any{"messages": list}
	if len(list) == limit {
		resp["next_cursor"] = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleConversationSend(w http.ResponseWriter, r *http.Request) {
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
	p, err := s.conv.GetPointer(r.Context(), r.PathValue("type"), r.PathValue("ref"))
	if s.convErr(w, err, "pointer not found") {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handlePointerPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
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
	p, err := s.conv.PutPointer(r.Context(), r.PathValue("type"), r.PathValue("ref"), body.ConversationID)
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
