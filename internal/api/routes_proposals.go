package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/trebi-ai/trebi/internal/proposals"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeProposals(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/proposals", s.handleProposalsList)
	mux.HandleFunc("GET /v1/proposals/{id}", s.handleProposalGet)
	mux.HandleFunc("POST /v1/proposals/{id}/decide", s.handleProposalDecide)
}

func (s *Server) agentOwnAgentMarkdown(r *http.Request, workspaceID, rel string) bool {
	scope, ok := scopeFrom(r.Context())
	if !ok || workspaceID != scope.workspaceID {
		return false
	}
	clean := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	return strings.EqualFold(strings.TrimPrefix(clean, "/"), scope.agent+"/AGENT.md")
}

// proposeAgentMarkdown converts one of the three AGENT.md write paths into a
// pending proposal. It writes the response and always returns true.
func (s *Server) proposeAgentMarkdown(ws *workspaces.Workspace, rel string, next []byte, w http.ResponseWriter, r *http.Request) bool {
	scope, _ := scopeFrom(r.Context())
	runID := ""
	if actor, ok := tools.ActorFrom(r.Context()); ok {
		runID = actor.RunID
	}
	p, err := s.proposals.Create(r.Context(), proposals.CreateOpts{
		WorkspaceID: ws.ID, Agent: scope.agent, RunID: runID, Proposed: next,
	})
	switch {
	case errors.Is(err, proposals.ErrNoChange):
		writeJSON(w, http.StatusOK, map[string]any{"proposal_id": "", "status": "applied", "message": "AGENT.md is unchanged"})
	case errors.Is(err, proposals.ErrInvalid), errors.Is(err, proposals.ErrTooLarge):
		code := http.StatusBadRequest
		if errors.Is(err, proposals.ErrTooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		writeErr(w, code, "validation", err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{
			"proposal_id": p.ID, "status": p.Status, "protected_keys": p.ProtectedKeys,
			"message": "AGENT.md changes need human approval. The change is in the operator inbox.",
		})
	}
	return true
}

func (s *Server) handleProposalsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, err := s.proposals.List(r.Context(), proposals.ListOpts{
		Workspace: q.Get("workspace"), Agent: q.Get("agent"), State: q.Get("state"), Limit: parseLimit(r),
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": list})
}

func (s *Server) handleProposalGet(w http.ResponseWriter, r *http.Request) {
	p, err := s.proposals.Get(r.Context(), r.PathValue("id"))
	if err == nil {
		if scope, ok := scopeFrom(r.Context()); ok && (scope.workspaceID != p.WorkspaceID || scope.agentID != p.AgentID) {
			writeForbidden(w, "agent may read only its own proposal")
			return
		}
	}
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleProposalDecide(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	allow := false
	switch body.Decision {
	case "allow":
		allow = true
	case "deny":
	default:
		writeErr(w, http.StatusBadRequest, "validation", `decision must be "allow" or "deny"`)
		return
	}
	p, err := s.proposals.Decide(r.Context(), r.PathValue("id"), proposals.Decision{Allow: allow, By: "ui"})
	switch {
	case errors.Is(err, proposals.ErrNotPending):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, proposals.ErrStale):
		writeErr(w, http.StatusConflict, "stale", err.Error())
	case err != nil:
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeJSON(w, http.StatusOK, p)
	}
}
