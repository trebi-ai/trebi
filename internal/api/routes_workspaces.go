package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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

// handleMemoryDelete removes the whole memory file (reset to empty).
func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request, assistant bool) {
	root, rel, path, err := s.memoryRootRel(r, assistant)
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	if !s.requireIfMatch(w, r, root, rel) {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": path})
}

// handleMemoryEntriesPost appends one B22 ledger entry ([YYYY-MM-DD] text).
func (s *Server) handleMemoryEntriesPost(w http.ResponseWriter, r *http.Request, assistant bool) {
	var body struct {
		Date  string `json:"date"`
		Text  string `json:"text"`
		Entry string `json:"entry"` // legacy alias for text
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	text := body.Text
	if text == "" {
		text = body.Entry
	}
	if text == "" {
		writeErr(w, http.StatusBadRequest, "validation", "text required")
		return
	}
	root, rel, path, err := s.memoryRootRel(r, assistant)
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	if !s.requireIfMatch(w, r, root, rel) {
		return
	}
	ent, err := job.AppendMemoryEntry(path, body.Date, text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	hash := s.fileHash(r.Context(), root, rel)
	setETagHash(w, hash)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": ent.ID, "date": ent.Date, "text": ent.Text,
		"path": path, "hash": hash,
	})
}

// handleMemoryEntriesDelete removes one ledger entry by id.
func (s *Server) handleMemoryEntriesDelete(w http.ResponseWriter, r *http.Request, assistant bool) {
	eid := r.PathValue("eid")
	if eid == "" {
		eid = r.PathValue("id")
	}
	root, rel, path, err := s.memoryRootRel(r, assistant)
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	if !s.requireIfMatch(w, r, root, rel) {
		return
	}
	if err := job.RemoveMemoryEntry(path, eid); err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "not found") {
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	hash := s.fileHash(r.Context(), root, rel)
	setETagHash(w, hash)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": eid, "hash": hash})
}

// memoryPath maps the request to the MEMORY.md sibling of the job file.
func (s *Server) memoryPath(r *http.Request, assistant bool) (string, error) {
	if assistant {
		wsID := r.PathValue("id")
		rows, err := s.ws.ListJobs(r.Context(), wsID)
		if err != nil {
			return "", err
		}
		for _, row := range rows {
			if row.Kind == "assistant" {
				return filepath.Join(filepath.Dir(row.Path), "MEMORY.md"), nil
			}
		}
		// assistant fallback: workspace-root MEMORY.md (TREBI.md sibling)
		ws, err := s.ws.Get(r.Context(), wsID)
		if err != nil {
			return "", errJobNotFound
		}
		return filepath.Join(ws.Path, "MEMORY.md"), nil
	}
	row, err := s.findJobRow(r.Context(), r.URL.Query().Get("workspace"), r.PathValue("name"))
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(row.Path), "MEMORY.md"), nil
}

func (s *Server) handleAssistantPut(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	ws, err := s.ws.Get(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	path := filepath.Join(ws.Path, "TREBI.md")
	if body.Content == "" {
		body.Content = "---\nharness: claude\n---\n\nYou are the workspace assistant.\n"
	}
	info, _, err := s.files.Write(r.Context(), ws.Path, "TREBI.md", []byte(body.Content), ifMatchHeader(r))
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	_ = s.ws.RebuildIndex(r.Context(), ws)
	setETagHash(w, info.Hash)
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "hash": info.Hash})
}

func (s *Server) handleAssistantPatch(w http.ResponseWriter, r *http.Request) {
	// patch disabled flag on TREBI.md
	wsID := r.PathValue("id")
	ws, err := s.ws.Get(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	var body struct {
		Disabled *bool `json:"disabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	path := filepath.Join(ws.Path, "TREBI.md")
	if !s.requireIfMatch(w, r, ws.Path, "TREBI.md") {
		return
	}
	if body.Disabled != nil {
		_ = job.SetDisabled(path, *body.Disabled)
	}
	setETagHash(w, s.fileHash(r.Context(), ws.Path, "TREBI.md"))
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "patched": true})
}

func (s *Server) handleAssistantDelete(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	ws, err := s.ws.Get(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	path := filepath.Join(ws.Path, "TREBI.md")
	if !s.requireIfMatch(w, r, ws.Path, "TREBI.md") {
		return
	}
	_ = os.Remove(path)
	_ = s.ws.RebuildIndex(r.Context(), ws)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": path})
}
