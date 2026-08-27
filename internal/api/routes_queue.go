package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/dispatch"
)

func (s *Server) routeQueue(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/queue", s.handleQueueList)
	mux.HandleFunc("POST /v1/queue/{id}/requeue", s.handleQueueRequeue)
	mux.HandleFunc("POST /v1/queue/{id}/cancel", s.handleQueueCancel)
}

func (s *Server) handleQueueList(w http.ResponseWriter, r *http.Request) {
	items, err := s.queue.List(r.Context(), r.URL.Query().Get("status"), 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		writeErr(w, http.StatusNotFound, "not_found", msg)
	case strings.Contains(msg, "done"):
		writeErr(w, http.StatusConflict, "conflict", msg)
	default:
		writeErr(w, http.StatusInternalServerError, "internal", msg)
	}
}

func (s *Server) handleRulesTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Match   string `json:"match"`
		Extract string `json:"extract"`
		Event   any    `json:"event"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	res := dispatch.EvalMatch(r.Context(), body.Match, body.Extract, body.Event)
	writeJSON(w, http.StatusOK, map[string]any{
		"matched": res.Matched, "vars": res.Vars, "error": res.Error,
	})
}

func (s *Server) handleRulesFire(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Integration string `json:"integration"`
		Workspace   string `json:"workspace"`
		Job         string `json:"job"`
		Match       string `json:"match"`
		Extract     string `json:"extract"`
		Event       any    `json:"event"`
		OnIndex     int    `json:"on_index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.Integration == "" {
		body.Integration = "rules-fire"
	}
	res := dispatch.EvalMatch(r.Context(), body.Match, body.Extract, body.Event)
	if res.Error != "" {
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "error", "error": res.Error})
		return
	}
	if !res.Matched {
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "no_match"})
		return
	}
	raw, err := json.Marshal(body.Event)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	// Durable event row (same pipeline as integration handleLine)
	eid := ulid.Make().String()
	trace := ulid.Make().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	summary := string(raw)
	if len(summary) > 200 {
		summary = summary[:200]
	}
	if _, err := s.st.DB().ExecContext(r.Context(), `INSERT INTO events (id, integration, received_at, summary, raw_json, trace_id)
		VALUES (?,?,?,?,?,?)`, eid, body.Integration, now, summary, string(raw), trace); err != nil {
		s.log.Warn("rules.fire event insert", "event_id", eid, "error", err)
	}

	item := &dispatch.QueueItem{
		Integration: body.Integration,
		WorkspaceID: body.Workspace,
		Job:         body.Job,
		OnIndex:     body.OnIndex,
		EventID:     eid,
		EventRaw:    string(raw),
		Vars:        res.Vars,
		Status:      dispatch.QueuePending,
		TraceID:     trace,
	}
	if err := s.queue.Enqueue(r.Context(), item); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if _, err := s.st.DB().ExecContext(r.Context(), `INSERT OR IGNORE INTO event_outcomes
		(event_id, workspace_id, job, on_index, outcome, queue_id, created_at)
		VALUES (?,?,?,?,?,?,?)`, eid, body.Workspace, body.Job, body.OnIndex,
		string(dispatch.OutcomeQueued), item.ID, now); err != nil {
		s.log.Warn("rules.fire outcome insert", "event_id", eid, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"outcome": "queued", "queue_id": item.ID, "event_id": eid,
	})
}
