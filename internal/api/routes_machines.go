package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/machines"
)

func (s *Server) routeMachines(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/machines", s.handleMachineList)
	mux.HandleFunc("GET /v1/machines/{id}", s.handleMachineGet)
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
