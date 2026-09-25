package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/store"
)

func (s *Server) routeInbox(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/inbox", s.handleInbox)
	mux.HandleFunc("GET /v1/inbox/{id}", s.handleInboxGet)
	mux.HandleFunc("POST /v1/inbox/{id}/read", s.handleInboxRead)
	mux.HandleFunc("POST /v1/inbox/read-all", s.handleInboxReadAll)
	mux.HandleFunc("POST /v1/inbox/{id}/resolve", s.handleInboxResolve)
	mux.HandleFunc("POST /v1/inbox/resolve-all", s.handleInboxResolveAll)
	mux.HandleFunc("GET /v1/alerts", s.handleAlerts)
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cursor, ok := parseULIDCursor(w, r, "inbox")
	if !ok {
		return
	}
	limit := parseLimit(r)
	items, err := s.alerts.List(r.Context(), alerts.ListOpts{
		Status: q.Get("status"), Kind: q.Get("kind"), Workspace: s.workspaceID(r.Context(), q.Get("workspace")),
		Limit: limit, Cursor: cursor,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := map[string]any{"items": items}
	eff := limit
	if eff <= 0 {
		eff = 50
	}
	if len(items) == eff {
		resp["next_cursor"] = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleInboxGet(w http.ResponseWriter, r *http.Request) {
	item, err := s.alerts.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, alerts.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "inbox item not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleInboxRead(w http.ResponseWriter, r *http.Request) {
	s.setInboxStatus(w, r, r.PathValue("id"), alerts.InboxRead, "manual")
}

func (s *Server) handleInboxReadAll(w http.ResponseWriter, r *http.Request) {
	n, err := s.alerts.ReadAll(r.Context(), s.workspaceID(r.Context(), r.URL.Query().Get("workspace")))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"read": n})
}

func (s *Server) handleInboxResolve(w http.ResponseWriter, r *http.Request) {
	s.setInboxStatus(w, r, r.PathValue("id"), alerts.InboxResolved, "manual")
}

// handleInboxResolveAll resolves every open item. ?kind= narrows it to one
// kind; ?workspace= narrows it to one workspace plus global rows.
func (s *Server) handleInboxResolveAll(w http.ResponseWriter, r *http.Request) {
	n, err := s.alerts.ResolveAll(r.Context(), r.URL.Query().Get("kind"), s.workspaceID(r.Context(), r.URL.Query().Get("workspace")))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolved": n})
}

func (s *Server) setInboxStatus(w http.ResponseWriter, r *http.Request, id string, status alerts.InboxStatus, how string) {
	err := s.alerts.SetStatus(r.Context(), id, status, how)
	if errors.Is(err, alerts.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "inbox item not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t, err = time.Parse(time.RFC3339Nano, raw)
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "malformed since: want RFC3339")
			return
		}
		since = t
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.alerts.ListAlerts(r.Context(), since, limit, s.workspaceID(r.Context(), r.URL.Query().Get("workspace")))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if events == nil {
		events = []store.StreamEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": events})
}
