package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/tools"
)

func (s *Server) routeNotify(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/notify/route", s.handleNotifyRoute)
	mux.HandleFunc("POST /v1/notify/test", s.handleNotifyTest)
}

// BindRouter sets the daemon's route resolver. Without it the notify routes
// return 503.
func (s *Server) BindRouter(r *notify.Resolver) { s.router = r }

// notifyTestResult is the outcome of one tested step.
type notifyTestResult struct {
	Step     int    `json:"step"`
	OutboxID string `json:"outbox_id,omitempty"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

// notifyScope reads the scope of a notify request. An assistant sees only
// its own workspace.
func (s *Server) notifyScope(ctx context.Context, ws, agent, job string) notify.Scope {
	sc := notify.Scope{WorkspaceID: s.workspaceID(ctx, ws), Agent: agent, Job: job}
	if a, ok := tools.ActorFrom(ctx); ok && a.Kind == "assistant" && a.WorkspaceID != "" {
		sc.WorkspaceID = a.WorkspaceID
	}
	return sc
}

// handleNotifyRoute returns the resolved route of a scope (plan 2026-09-28 E.2).
func (s *Server) handleNotifyRoute(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeErr(w, http.StatusServiceUnavailable, "internal", "notify router is not running")
		return
	}
	q := r.URL.Query()
	sc := s.notifyScope(r.Context(), q.Get("workspace"), q.Get("agent"), q.Get("job"))
	writeJSON(w, http.StatusOK, s.router.Resolve(r.Context(), sc))
}

// handleNotifyTest sends one test message per usable step, or only the given
// step. A failed step does not stop the test; each step gets one attempt.
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeErr(w, http.StatusServiceUnavailable, "internal", "notify router is not running")
		return
	}
	var body struct {
		Workspace string `json:"workspace"`
		Agent     string `json:"agent"`
		Job       string `json:"job"`
		Step      *int   `json:"step"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "invalid JSON body")
			return
		}
	}
	ctx := r.Context()
	sc := s.notifyScope(ctx, body.Workspace, body.Agent, body.Job)
	route := s.router.Resolve(ctx, sc)
	if body.Step != nil && (*body.Step < 0 || *body.Step >= len(route.Steps)) {
		writeErr(w, http.StatusBadRequest, "validation", "step is out of range")
		return
	}
	results := []notifyTestResult{}
	for i, st := range route.Steps {
		if body.Step != nil && i != *body.Step {
			continue
		}
		res := notifyTestResult{Step: i}
		if st.Problem != "" {
			res.Status, res.Error = "skipped", st.Problem
			results = append(results, res)
			continue
		}
		row, err := s.enqueueNotifyTest(ctx, sc.WorkspaceID, st)
		if err == nil {
			row, err = s.outbox.SendNow(ctx, row.ID)
		}
		res.OutboxID, res.Status, res.Error = row.ID, string(row.Status), row.ErrorMessage
		if err != nil {
			res.Status, res.Error = string(outbox.StatusFailed), err.Error()
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"route": route, "results": results})
}

// enqueueNotifyTest writes the test row of one usable step.
func (s *Server) enqueueNotifyTest(ctx context.Context, workspaceID string, st notify.ResolvedStep) (outbox.Row, error) {
	if st.Via == notify.ViaEmail {
		return s.email.SendTest(ctx, workspaceID, st.Targets)
	}
	return s.outbox.Enqueue(ctx, outbox.Row{
		Kind: outbox.KindChannel, Purpose: outbox.PurposeTest, MaxAttempts: 1,
		WorkspaceID: workspaceID, Connector: st.Via, Target: st.To,
		Body: "This is a test message from trebi. This step of the notify route works.",
	})
}
