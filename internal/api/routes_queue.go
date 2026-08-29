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
}

func (s *Server) handleQueueList(w http.ResponseWriter, r *http.Request) {
	cursor, ok := parseULIDCursor(w, r, "queue")
	if !ok {
		return
	}
	limit := parseLimit(r)
	items, err := s.queue.List(r.Context(), r.URL.Query().Get("status"), cursor, limit)
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
	id := r.PathValue("id")
	if err := s.queue.Requeue(r.Context(), id); err != nil {
		writeQueueErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "pending"})
}

func (s *Server) handleQueueCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.queue.Cancel(r.Context(), id); err != nil {
		writeQueueErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "status": string(dispatch.QueueDone), "result": string(dispatch.ResultSkipped),
	})
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
