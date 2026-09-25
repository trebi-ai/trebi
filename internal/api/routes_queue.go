package api

import (
	"errors"
	"net/http"

	"github.com/trebi-ai/trebi/internal/dispatch"
)

func (s *Server) routeQueue(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/queue", s.handleQueueList)
	mux.HandleFunc("POST /v1/queue/{id}/requeue", s.handleQueueRequeue)
	mux.HandleFunc("POST /v1/queue/{id}/cancel", s.handleQueueCancel)
	mux.HandleFunc("POST /v1/queue/cancel", s.handleQueueCancelBulk)
}

func (s *Server) handleQueueList(w http.ResponseWriter, r *http.Request) {
	cursor, ok := parseULIDCursor(w, r, "queue")
	if !ok {
		return
	}
	limit := parseLimit(r)
	q := r.URL.Query()
	items, err := s.queue.List(r.Context(), dispatch.ListOpts{
		Status:    q.Get("status"),
		Workspace: s.workspaceID(r.Context(), q.Get("workspace")),
		Cursor:    cursor,
		Limit:     limit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := map[string]any{"items": items}
	if len(items) == limit {
		resp["next_cursor"] = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleQueueRequeue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentQueueScope(w, r, r.PathValue("id")) {
		return
	}
	id := r.PathValue("id")
	if err := s.queue.Requeue(r.Context(), id); err != nil {
		writeQueueErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "pending"})
}

func (s *Server) handleQueueCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentQueueScope(w, r, r.PathValue("id")) {
		return
	}
	id := r.PathValue("id")
	if err := s.queue.Cancel(r.Context(), id); err != nil {
		writeQueueErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "status": string(dispatch.QueueDone), "result": string(dispatch.ResultSkipped),
	})
}

// handleQueueCancelBulk cancels every queue item named in ids.
func (s *Server) handleQueueCancelBulk(w http.ResponseWriter, r *http.Request) {
	ids, ok := bulkIDs(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, applyBulk(r.Context(), ids, s.queue.Cancel))
}

// writeQueueErr maps dispatch queue errors to HTTP codes.
func writeQueueErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, dispatch.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, dispatch.ErrDone):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
