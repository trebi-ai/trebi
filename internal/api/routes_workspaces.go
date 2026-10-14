package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeWorkspaces(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces", s.handleWorkspacesList)
	mux.HandleFunc("POST /v1/workspaces", s.handleWorkspacesAdd)
	mux.HandleFunc("GET /v1/workspaces/{id}", s.handleWorkspaceGet)
	mux.HandleFunc("PATCH /v1/workspaces/{id}", s.handleWorkspacePatch)
	mux.HandleFunc("POST /v1/workspaces/{id}/move", s.handleWorkspaceMove)
	mux.HandleFunc("DELETE /v1/workspaces/{id}", s.handleWorkspaceDelete)
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
		Handle string `json:"handle"`
		Create bool   `json:"create"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	ws, err := s.ws.AddWith(r.Context(), workspaces.AddOpts{
		Path: body.Path, Name: body.Name, Handle: body.Handle, Create: body.Create,
	})
	if err != nil {
		writeWorkspaceAddErr(w, err)
		return
	}
	s.ensureWorkspaceEmail(r.Context(), ws.ID, ws.Handle)
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

// handleAssistantAsk sends one message to the workspace assistant. The daemon
// resolves or creates the channel conversation, so a client needs no pointer
// calls of its own.
func (s *Server) handleAssistantAsk(w http.ResponseWriter, r *http.Request) {
	if scope, ok := scopeFrom(r.Context()); ok && scope.workspaceID != r.PathValue("id") {
		writeForbidden(w, "agent may message only its own workspace assistant")
		return
	}
	if !s.assistantScoped(w, r, r.PathValue("id")) {
		return
	}
	var body struct {
		Text string `json:"text"`
		conversations.Address
		IdempotencyKey string   `json:"idempotency_key"`
		Attachments    []string `json:"attachments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	atts, ok := s.attachUploads(r.Context(), w, s.workspaceID(r.Context(), r.PathValue("id")), body.Attachments)
	if !ok {
		return
	}
	out, err := s.conv.Ask(r.Context(), conversations.AskOpts{
		Workspace:      r.PathValue("id"),
		Text:           body.Text,
		Attachments:    atts,
		Address:        body.Address,
		IdempotencyKey: body.IdempotencyKey,
		Wait:           parseWait(r),
	})
	if s.convErr(w, err, "workspace not found") {
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleWorkspacePatch(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	var body struct {
		Name       *string                `json:"name"`
		GitEnabled *bool                  `json:"git_enabled"`
		Contact    json.RawMessage        `json:"contact"`
		Notify     json.RawMessage        `json:"notify"`
		Chains     *workspaces.ChainPatch `json:"chains"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body is a no-op get
	id := r.PathValue("id")
	if body.Name != nil && strings.TrimSpace(*body.Name) == "" {
		body.Name = nil
	}
	p := workspaces.Patch{Name: body.Name, GitEnabled: body.GitEnabled, Chains: body.Chains}
	if err := routePatch(&p, body.Contact, body.Notify); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.Name == nil && body.GitEnabled == nil && body.Contact == nil && body.Notify == nil && body.Chains == nil {
		ws, err := s.ws.Get(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
			return
		}
		writeJSON(w, http.StatusOK, ws)
		return
	}
	ws, err := s.ws.Patch(r.Context(), id, p)
	if errors.Is(err, workspaces.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	if errors.Is(err, workspaces.ErrChainLimits) {
		writeErrDetails(w, http.StatusBadRequest, "validation", err.Error(), map[string]any{"field": "chains"})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// routePatch decodes contact and notify into p. A JSON null clears the key.
func routePatch(p *workspaces.Patch, contact, route json.RawMessage) error {
	decode := func(raw json.RawMessage) (any, bool, error) {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, false, err
		}
		return v, v == nil, nil
	}
	if contact != nil {
		v, clear, err := decode(contact)
		if err != nil {
			return err
		}
		list, err := notify.ParseContact(v)
		if err != nil {
			return err
		}
		p.Contact, p.ClearContact = &list, clear
	}
	if route != nil {
		v, clear, err := decode(route)
		if err != nil {
			return err
		}
		steps, err := notify.ParseSteps(v)
		if err != nil {
			return err
		}
		if steps == nil && !clear {
			steps = []notify.Step{}
		}
		p.Notify, p.ClearNotify = &steps, clear
	}
	return nil
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
