package api

import (
	"context"
	"errors"
	"net/http"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/machines"
	"github.com/trebi-ai/trebi/internal/visor"
)

func (s *Server) routeMachines(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/machines", s.handleMachineList)
	mux.HandleFunc("GET /v1/machines/{id}", s.handleMachineGet)
	mux.HandleFunc("GET /v1/machines/{id}/sessions", s.handleObservedList)
	mux.HandleFunc("GET /v1/machines/{id}/sessions/{session}", s.handleObservedGet)
}

// bindMachines builds the machines service over the current harness,
// connector, and cloud services.
func (s *Server) bindMachines() {
	s.machines = machines.New(s.st, s.harness, agentcli.HarnessLocator{}, s.connectors, s.cloud).WithVersion(Version)
}

// readiness checks what a job or an agent needs on the machine of its
// workspace. A failed check gives an empty list; it never fails the read.
func (s *Server) readiness(ctx context.Context, wsID string, j *job.Job, kit *job.KitYAML) []machines.Item {
	items, err := s.machines.Readiness(ctx, wsID, machines.NeedsOf(j, kit))
	if err != nil {
		s.log.WarnContext(ctx, "machine.readiness", "err", err, "workspace_id", wsID)
		return []machines.Item{}
	}
	return items
}

// handleMachineList returns the machine roster. It is short, so it has no
// pages.
func (s *Server) handleMachineList(w http.ResponseWriter, r *http.Request) {
	list, err := s.machines.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": list})
}

// handleMachineGet returns one machine. The id "self" is this machine. The
// self row also has its capabilities.
func (s *Server) handleMachineGet(w http.ResponseWriter, r *http.Request) {
	m, err := s.machines.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, machines.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if m.IsSelf {
		caps, err := s.machines.Capabilities(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		caps.Versions = machines.Versions{Trebi: Version, Edition: s.ext.EditionVersion, UI: s.uiVersion()}
		m.Capabilities = &caps
	}
	writeJSON(w, http.StatusOK, m)
}

// observed gives the sessions on this machine that started outside Trebi.
func (s *Server) observed() *visor.Observed { return visor.NewObserved(s.runs.SessionHost) }

// selfMachine writes an error and gives false when the id is not this
// machine. Only this machine sees its own sessions.
func (s *Server) selfMachine(w http.ResponseWriter, r *http.Request) bool {
	m, err := s.machines.Get(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, machines.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	case !m.IsSelf:
		writeErr(w, http.StatusConflict, "remote_machine", "Open Trebi on "+m.Name+" to see its sessions.")
	default:
		return true
	}
	return false
}

// handleObservedList returns the agent sessions that the user started
// outside Trebi on this machine.
func (s *Server) handleObservedList(w http.ResponseWriter, r *http.Request) {
	if !s.selfMachine(w, r) {
		return
	}
	list, err := s.observed().List(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}

// handleObservedGet returns one observed session with its turns.
func (s *Server) handleObservedGet(w http.ResponseWriter, r *http.Request) {
	if !s.selfMachine(w, r) {
		return
	}
	sess, turns, err := s.observed().Get(r.Context(), r.PathValue("session"))
	switch {
	case errors.Is(err, agentvisor.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "no session "+r.PathValue("session"))
	case err != nil:
		writeErr(w, http.StatusServiceUnavailable, "unavailable", err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"session": sess, "turns": turns})
	}
}
