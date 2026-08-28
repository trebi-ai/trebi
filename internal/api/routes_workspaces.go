package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeWorkspaces(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces", s.handleWorkspacesList)
	mux.HandleFunc("POST /v1/workspaces", s.handleWorkspacesAdd)
	mux.HandleFunc("GET /v1/workspaces/{id}", s.handleWorkspaceGet)
	mux.HandleFunc("PATCH /v1/workspaces/{id}", s.handleWorkspacePatch)
	mux.HandleFunc("DELETE /v1/workspaces/{id}", s.handleWorkspaceDelete)
	mux.HandleFunc("GET /v1/workspaces/{id}/assistant", s.handleAssistantGet)
	mux.HandleFunc("PUT /v1/workspaces/{id}/assistant", s.handleAssistantPut)
	mux.HandleFunc("PATCH /v1/workspaces/{id}/assistant", s.handleAssistantPatch)
	mux.HandleFunc("DELETE /v1/workspaces/{id}/assistant", s.handleAssistantDelete)
	mux.HandleFunc("POST /v1/workspaces/{id}/assistant/wake", s.handleAssistantWake)
	mux.HandleFunc("GET /v1/workspaces/{id}/assistant/memory", s.handleAssistantMemoryGet)
	mux.HandleFunc("PUT /v1/workspaces/{id}/assistant/memory", s.handleAssistantMemoryPut)
	mux.HandleFunc("DELETE /v1/workspaces/{id}/assistant/memory", func(w http.ResponseWriter, r *http.Request) {
		s.handleMemoryDelete(w, r, true)
	})
	mux.HandleFunc("POST /v1/workspaces/{id}/assistant/memory/entries", func(w http.ResponseWriter, r *http.Request) {
		s.handleMemoryEntriesPost(w, r, true)
	})
	mux.HandleFunc("DELETE /v1/workspaces/{id}/assistant/memory/entries/{eid}", func(w http.ResponseWriter, r *http.Request) {
		s.handleMemoryEntriesDelete(w, r, true)
	})
}

func (s *Server) handleWorkspacesList(w http.ResponseWriter, r *http.Request) {
	list, err := s.ws.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": list})
}

func (s *Server) handleWorkspacesAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path   string `json:"path"`
		Name   string `json:"name"`
		Create bool   `json:"create"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	ws, err := s.ws.AddWith(r.Context(), workspaces.AddOpts{
		Path: body.Path, Name: body.Name, Create: body.Create,
	})
	if err != nil {
		writeWorkspaceAddErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

func writeWorkspaceAddErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workspaces.ErrNested), errors.Is(err, workspaces.ErrContains):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	}
}

func (s *Server) handleWorkspaceGet(w http.ResponseWriter, r *http.Request) {
	ws, err := s.ws.Get(r.Context(), r.PathValue("id"))
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleAssistantGet(w http.ResponseWriter, r *http.Request) {
	ws, err := s.ws.Get(r.Context(), r.PathValue("id"))
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	path := filepath.Join(ws.Path, "TREBI.md")
	j, err := job.ParseFile(path)
	if err != nil && j == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no assistant")
		return
	}
	if j == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no assistant")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": j.Name, "harness": j.Harness, "description": j.Description,
		"memory": j.Memory, "path": path, "parse_error": j.ParseError, "hash": j.Hash,
	})
}
