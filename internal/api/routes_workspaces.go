package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeWorkspaces(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces", s.handleWorkspacesList)
	mux.HandleFunc("POST /v1/workspaces", s.handleWorkspacesAdd)
	mux.HandleFunc("GET /v1/workspaces/{id}", s.handleWorkspaceGet)
	mux.HandleFunc("PATCH /v1/workspaces/{id}", s.handleWorkspacePatch)
	mux.HandleFunc("POST /v1/workspaces/{id}/move", s.handleWorkspaceMove)
	mux.HandleFunc("DELETE /v1/workspaces/{id}", s.handleWorkspaceDelete)
	mux.HandleFunc("GET /v1/workspaces/{id}/assistant", s.handleAssistantGet)
	mux.HandleFunc("PUT /v1/workspaces/{id}/assistant", s.handleAssistantPut)
	mux.HandleFunc("PATCH /v1/workspaces/{id}/assistant", s.handleAssistantPatch)
	mux.HandleFunc("DELETE /v1/workspaces/{id}/assistant", s.handleAssistantDelete)
	mux.HandleFunc("POST /v1/workspaces/{id}/assistant/wake", s.handleAssistantWake)
	mux.HandleFunc("POST /v1/workspaces/{id}/assistant/messages", s.handleAssistantAsk)
}

// workspaceID resolves a workspace id or display name to an id. Unknown
// values are returned unchanged so existing not-found paths still fire.
func (s *Server) workspaceID(ctx context.Context, idOrName string) string {
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		return ""
	}
	ws, err := s.ws.Resolve(ctx, idOrName)
	if err != nil {
		return idOrName
	}
	return ws.ID
}

func (s *Server) handleWorkspacesList(w http.ResponseWriter, r *http.Request) {
	list, err := s.ws.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	limit := parseLimit(r)
	truncated := len(list) > limit
	if truncated {
		list = list[:limit]
	}
	if _, live, err := s.runs.StatusByJob(r.Context()); err == nil {
		running := map[string]int{}
		for key, ids := range live {
			wsID, _, ok := strings.Cut(key, "/")
			if ok {
				running[wsID] += len(ids)
			}
		}
		for i := range list {
			list[i].RunningCount = running[list[i].ID]
		}
	}
	resp := map[string]any{"workspaces": list}
	if truncated {
		resp["truncated"] = true
	}
	writeJSON(w, http.StatusOK, resp)
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

// handleWorkspaceGet takes an id or a name, the same as `trebi workspace show`.
func (s *Server) handleWorkspaceGet(w http.ResponseWriter, r *http.Request) {
	ws, err := s.ws.Resolve(r.Context(), r.PathValue("id"))
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
	ws, ok := s.workspaceByID(w, r)
	if !ok {
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
	cfg, _ := config.Load()
	var asst config.AssistantConfig
	if cfg != nil {
		asst = cfg.Assistant
	}
	runtime, reason, auth := asst.Route(j.Runtime)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": j.Name, "harness": j.Harness, "description": j.Description,
		"memory": j.Memory, "path": path, "parse_error": j.ParseError, "hash": j.Hash,
		"disabled": j.Disabled,
		"runtime":  runtime, "model": asst.Model, "auth_status": auth, "runtime_reason": reason,
	})
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
		body.Content = "---\n---\n\nYou are the workspace assistant.\n# Escape hatch: runtime: tui\n"
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

// handleAssistantAsk sends one message to the workspace assistant. The daemon
// resolves or creates the channel conversation, so a client needs no pointer
// calls of its own.
func (s *Server) handleAssistantAsk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text           string `json:"text"`
		ChannelType    string `json:"channel_type"`
		ChannelRef     string `json:"channel_ref"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	out, err := s.conv.Ask(r.Context(), conversations.AskOpts{
		Workspace:      r.PathValue("id"),
		Text:           body.Text,
		ChannelType:    body.ChannelType,
		ChannelRef:     body.ChannelRef,
		IdempotencyKey: body.IdempotencyKey,
		Wait:           parseWait(r),
	})
	if s.convErr(w, err, "workspace not found") {
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleAssistantWake(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	var body runCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	row, err := s.findAssistantRow(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "no assistant in workspace")
		return
	}
	// Assistant lane: keep client prompt_override/inputs, pin identity fields.
	body.Job = row.Name
	body.Workspace = wsID
	body.Kind = "assistant"
	if body.Reason == "" {
		body.Reason = string(runs.ReasonManual)
	}
	s.createRun(w, r, body)
}

func (s *Server) handleWorkspacePatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       *string `json:"name"`
		GitEnabled *bool   `json:"git_enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body is a no-op get
	id := r.PathValue("id")
	if body.Name != nil && strings.TrimSpace(*body.Name) == "" {
		body.Name = nil
	}
	if body.Name == nil && body.GitEnabled == nil {
		ws, err := s.ws.Get(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
			return
		}
		writeJSON(w, http.StatusOK, ws)
		return
	}
	ws, err := s.ws.Patch(r.Context(), id, workspaces.Patch{Name: body.Name, GitEnabled: body.GitEnabled})
	if errors.Is(err, workspaces.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleWorkspaceMove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path   string `json:"path"`
		Mkdir  bool   `json:"mkdir"`
		DryRun bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if strings.TrimSpace(body.Path) == "" {
		writeErr(w, http.StatusBadRequest, "validation", "path required")
		return
	}
	ws, err := s.ws.Resolve(r.Context(), r.PathValue("id"))
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !body.DryRun && s.workspaceHasLiveRuns(r.Context(), ws.ID) {
		writeErr(w, http.StatusConflict, "conflict", "workspace has live runs; kill them first")
		return
	}
	out, err := s.ws.Move(r.Context(), ws.ID, workspaces.MoveOpts{
		Path: body.Path, Mkdir: body.Mkdir, DryRun: body.DryRun,
	})
	if err != nil {
		writeWorkspaceMoveErr(w, err)
		return
	}
	resp := struct {
		workspaces.MoveResult
		ConnectorsRewritten []string `json:"connectors_rewritten,omitempty"`
	}{MoveResult: *out}
	if s.connectors != nil {
		names, err := s.connectors.RewritePathPrefix(ws.Path, out.Path, body.DryRun)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		resp.ConnectorsRewritten = names
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) workspaceHasLiveRuns(ctx context.Context, wsID string) bool {
	if s.runs == nil {
		return false
	}
	_, live, err := s.runs.StatusByJob(ctx)
	if err != nil {
		return false
	}
	prefix := wsID + "/"
	for key, ids := range live {
		if strings.HasPrefix(key, prefix) && len(ids) > 0 {
			return true
		}
	}
	return false
}

func writeWorkspaceMoveErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workspaces.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, workspaces.ErrNested), errors.Is(err, workspaces.ErrContains),
		errors.Is(err, workspaces.ErrDestExists), errors.Is(err, workspaces.ErrDestInside),
		errors.Is(err, workspaces.ErrSrcInsideDest):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	}
}

func (s *Server) handleWorkspaceDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.ws.Remove(r.Context(), id); err != nil {
		if errors.Is(err, workspaces.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
