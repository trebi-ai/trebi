package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/store"
)

func (s *Server) routeInbox(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/inbox", s.handleInbox)
	mux.HandleFunc("GET /v1/inbox/{id}", s.handleInboxGet)
	mux.HandleFunc("POST /v1/inbox/apply", s.handleInboxApply)
	mux.HandleFunc("GET /v1/alerts", s.handleAlerts)
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cursor, ok := parseULIDCursor(w, r, "inbox")
	if !ok {
		return
	}
	f := alerts.InboxFilter{
		View: alerts.InboxView(q.Get("view")), Kind: alerts.InboxKind(q.Get("kind")),
		Workspace: s.workspaceID(r.Context(), q.Get("workspace")),
		Unread:    q.Get("unread") == "1" || q.Get("unread") == "true", Query: q.Get("q"),
		AlertKind: q.Get("alert_kind"), Connector: q.Get("connector"),
	}
	if err := f.Check(); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	page, err := s.alerts.List(r.Context(), alerts.ListOpts{InboxFilter: f, Limit: parseLimit(r), Cursor: cursor})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
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

// inboxApplyBody is the body of POST /v1/inbox/apply.
type inboxApplyBody struct {
	Op      alerts.InboxOp      `json:"op"`
	IDs     []string            `json:"ids"`
	Filter  *alerts.InboxFilter `json:"filter"`
	Confirm bool                `json:"confirm"`
}

// handleInboxApply changes the read or Done state of ids or of a filter. An
// archive that would hide an open approval or request needs confirm.
func (s *Server) handleInboxApply(w http.ResponseWriter, r *http.Request) {
	var body inboxApplyBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "malformed body: "+err.Error())
		return
	}
	if body.Filter != nil {
		body.Filter.Workspace = s.workspaceID(r.Context(), body.Filter.Workspace)
	}
	res, err := s.alerts.Apply(r.Context(), body.Op, alerts.InboxSelector{
		IDs: body.IDs, Filter: body.Filter, Confirm: body.Confirm || confirmed(r),
	})
	switch {
	case errors.Is(err, alerts.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	case errors.Is(err, alerts.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if len(res.Pending) > 0 {
		n := len(res.Pending)
		msg := fmt.Sprintf("%d items still wait for your answer. Archive them anyway?", n)
		if n == 1 {
			msg = "1 item still waits for your answer. Archive it anyway?"
		}
		writeErrDetails(w, http.StatusBadRequest, "validation", msg, map[string]any{
			"confirm_required": true, "count": n, "pending": res.Pending,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": res.Updated})
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
