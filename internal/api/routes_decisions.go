package api

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/decisions"
)

func (s *Server) routeDecisions(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/decisions", s.handleDecisionList)
	mux.HandleFunc("GET /v1/decisions/kinds", s.handleDecisionKinds)
	mux.HandleFunc("GET /v1/decisions/stats", s.handleDecisionStats)
	mux.HandleFunc("POST /v1/decisions/test", s.handleDecisionTest)
	mux.HandleFunc("GET /v1/decisions/{id}", s.handleDecisionGet)
	mux.HandleFunc("GET /v1/decisions/{id}/raw", s.handleDecisionRaw)
	mux.HandleFunc("POST /v1/decisions/{id}/feedback", s.handleDecisionFeedback)
}

// BindDecisions swaps in the daemon's decisions service.
func (s *Server) BindDecisions(d *decisions.Service) { bind(&s.decisions, d) }

func writeDecisionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, decisions.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, decisions.ErrUnknownKind):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) handleDecisionList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ws, ok := s.artifactWorkspace(w, r, q.Get("workspace"))
	if !ok {
		return
	}
	cursor := q.Get("cursor")
	if cursor != "" && !strings.HasPrefix(cursor, "dec_") {
		writeErr(w, http.StatusBadRequest, "validation", "cursor is not a decision id")
		return
	}
	f := decisions.ListFilter{Workspace: ws, Kind: q.Get("kind"),
		Outcome: q.Get("outcome"), Status: q.Get("status"), SubjectKind: q.Get("subject_kind"),
		SubjectID: q.Get("subject_id"), Scope: cmp.Or(q.Get("scope"), q.Get("connection")),
		Feedback: q.Get("feedback"), After: cursor, Limit: parseLimit(r)}
	if !slices.Contains([]string{"", decisions.FeedbackAgree, decisions.FeedbackDisagree, "none"}, f.Feedback) {
		writeErr(w, http.StatusBadRequest, "validation", "feedback must be agree, disagree, or none")
		return
	}
	if v := q.Get("waiting"); v == "1" || v == "true" {
		ids, err := s.connectors.WaitingDecisionIDs(r.Context(), ws, f.Scope)
		if err != nil {
			writeDecisionErr(w, err)
			return
		}
		f.IDs = append([]string{}, ids...)
	}
	list, next, err := s.decisions.List(r.Context(), f)
	if err != nil {
		writeDecisionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.decisionViews(r.Context(), list), "next_cursor": next})
}

func (s *Server) handleDecisionKinds(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": s.decisions.Kinds()})
}

func (s *Server) handleDecisionStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ws, ok := s.artifactWorkspace(w, r, q.Get("workspace"))
	if !ok {
		return
	}
	since := time.Now().Add(-30 * 24 * time.Hour)
	v, err := parseTimeFilter(q.Get("since"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if v != "" {
		since, _ = time.Parse(time.RFC3339, v) // parseTimeFilter returns RFC3339
	}
	st, err := s.decisions.Stats(r.Context(), decisions.StatsFilter{Workspace: ws, Kind: q.Get("kind"), Scope: cmp.Or(q.Get("scope"), q.Get("connection")), Since: since})
	if err != nil {
		writeDecisionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// decisionFor returns the decision of {id}. A run sees only its workspace.
func (s *Server) decisionFor(w http.ResponseWriter, r *http.Request) (decisions.Decision, bool) {
	d, err := s.decisions.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDecisionErr(w, err)
		return d, false
	}
	if a := artifactActor(r); a != nil && a.WorkspaceID != "" && a.WorkspaceID != d.Workspace {
		writeErr(w, http.StatusNotFound, "not_found", decisions.ErrNotFound.Error())
		return d, false
	}
	return d, true
}

func (s *Server) handleDecisionGet(w http.ResponseWriter, r *http.Request) {
	if d, ok := s.decisionFor(w, r); ok {
		writeJSON(w, http.StatusOK, s.decisionView(r.Context(), d))
	}
}

func (s *Server) handleDecisionRaw(w http.ResponseWriter, r *http.Request) {
	d, ok := s.decisionFor(w, r)
	if !ok {
		return
	}
	raw, err := s.decisions.Raw(r.Context(), d.ID)
	if err != nil {
		writeDecisionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": d.ID, "raw": string(raw)})
}

func (s *Server) handleDecisionFeedback(w http.ResponseWriter, r *http.Request) {
	if personOnly(w, r, "only a person can give feedback on a decision") {
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Value != decisions.FeedbackAgree && body.Value != decisions.FeedbackDisagree {
		writeErr(w, http.StatusBadRequest, "validation", "value must be agree or disagree")
		return
	}
	d, err := s.decisions.Feedback(r.Context(), r.PathValue("id"), body.Value, "user")
	if err != nil {
		writeDecisionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleDecisionTest(w http.ResponseWriter, r *http.Request) {
	if personOnly(w, r, "only a person can test a decision") {
		return
	}
	var body struct {
		Kind  string            `json:"kind"`
		Text  string            `json:"text"`
		Facts map[string]string `json:"facts"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Kind) == "" || strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, "validation", "kind and text are required")
		return
	}
	d, err := s.decisions.Test(r.Context(), body.Kind, decisions.State{Text: body.Text, Untrusted: true, Facts: body.Facts})
	if err != nil {
		writeDecisionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}
