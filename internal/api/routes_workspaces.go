package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/machines"
	"github.com/trebi-ai/trebi/internal/moves"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeWorkspaces(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces", s.handleWorkspacesList)
	mux.HandleFunc("POST /v1/workspaces", s.handleWorkspacesAdd)
	mux.HandleFunc("GET /v1/workspaces/handle-check", s.handleWorkspaceHandleCheck)
	mux.HandleFunc("GET /v1/workspaces/{id}", s.handleWorkspaceGet)
	mux.HandleFunc("PATCH /v1/workspaces/{id}", s.handleWorkspacePatch)
	mux.HandleFunc("POST /v1/workspaces/{id}/move", s.handleWorkspaceMove)
	mux.HandleFunc("GET /v1/workspaces/{id}/moves/{move}", s.handleWorkspaceMoveGet)
	mux.HandleFunc("POST /v1/workspaces/import", s.handleWorkspaceImport)
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

// handleWorkspaceHandleCheck tells whether a handle can name the email address
// of a workspace. The reason is format, taken (another local workspace), or
// locked (the workspace has an address already).
func (s *Server) handleWorkspaceHandleCheck(w http.ResponseWriter, r *http.Request) {
	handle := strings.TrimSpace(r.URL.Query().Get("handle"))
	if handle == "" {
		writeErr(w, http.StatusBadRequest, "validation", "handle required")
		return
	}
	id := ""
	if ref := strings.TrimSpace(r.URL.Query().Get("workspace")); ref != "" {
		ws, err := s.ws.Resolve(r.Context(), ref)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
			return
		}
		id = ws.ID
		if handle != ws.Handle {
			has, err := s.ws.HasAddress(r.Context(), id)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			if has {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "reason": "locked"})
				return
			}
		}
	}
	if workspaces.ValidateHandle(handle) != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "reason": "format"})
		return
	}
	taken, err := s.ws.HandleTaken(r.Context(), handle, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if taken {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "reason": "taken"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reason": ""})
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
		Name           *string                `json:"name"`
		Handle         *string                `json:"handle"`
		GitEnabled     *bool                  `json:"git_enabled"`
		Contact        json.RawMessage        `json:"contact"`
		Notify         json.RawMessage        `json:"notify"`
		EmailAllowlist json.RawMessage        `json:"email_allowlist"`
		Chains         *workspaces.ChainPatch `json:"chains"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body is a no-op get
	id := r.PathValue("id")
	if body.Name != nil && strings.TrimSpace(*body.Name) == "" {
		body.Name = nil
	}
	p := workspaces.Patch{Name: body.Name, Handle: body.Handle, GitEnabled: body.GitEnabled, Chains: body.Chains}
	if err := routePatch(&p, body.Contact, body.Notify); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err := emailAllowlistPatch(&p, body.EmailAllowlist); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.Name == nil && body.Handle == nil && body.GitEnabled == nil && body.Contact == nil && body.Notify == nil &&
		body.EmailAllowlist == nil && body.Chains == nil {
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
	if errors.Is(err, workspaces.ErrHandleLocked) {
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if errors.Is(err, workspaces.ErrHandle) {
		writeErrDetails(w, http.StatusBadRequest, "validation", err.Error(), map[string]any{"field": "handle"})
		return
	}
	if errors.Is(err, workspaces.ErrEmailAllowlistInvalid) {
		writeErrDetails(w, http.StatusBadRequest, "validation", err.Error(), map[string]any{"field": "email_allowlist"})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// emailAllowlistPatch decodes the email_allowlist body value into p. A JSON
// list sets it; null clears the block.
func emailAllowlistPatch(p *workspaces.Patch, raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return errors.New("email_allowlist must be a list of addresses or null")
	}
	if list == nil {
		p.ClearEmailAllowlist = true
		return nil
	}
	p.EmailAllowlist = &list
	return nil
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
		Path    string `json:"path"`
		Machine string `json:"machine"`
		Mkdir   bool   `json:"mkdir"`
		DryRun  bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if strings.TrimSpace(body.Machine) != "" {
		if body.Path != "" || body.Mkdir || body.DryRun {
			writeErr(w, http.StatusBadRequest, "validation", "give machine or path, not both")
			return
		}
		s.moveToMachine(w, r, strings.TrimSpace(body.Machine))
		return
	}
	if strings.TrimSpace(body.Path) == "" {
		writeErr(w, http.StatusBadRequest, "validation", "path or machine required")
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

// moveToMachine starts a move of the workspace to another machine. Only a
// person on this machine, or the assistant for that person, starts it.
func (s *Server) moveToMachine(w http.ResponseWriter, r *http.Request, machine string) {
	if a, ok := tools.ActorFrom(r.Context()); ok && a.Kind != "assistant" {
		writeForbidden(w, "Only a person on this machine can move a workspace to another machine.")
		return
	}
	ws, err := s.ws.Resolve(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if s.workspaceHasLiveRuns(r.Context(), ws.ID) {
		writeErr(w, http.StatusConflict, "conflict", moves.ErrBusy.Error())
		return
	}
	mv, err := s.moves.Start(r.Context(), ws.ID, machine)
	if err != nil {
		writeMachineMoveErr(w, machine, err)
		return
	}
	writeJSON(w, http.StatusAccepted, mv)
}

func writeMachineMoveErr(w http.ResponseWriter, machine string, err error) {
	switch {
	case errors.Is(err, machines.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "machine "+machine+" not found")
	case errors.Is(err, machines.ErrNoLink), errors.Is(err, moves.ErrNoDevice):
		writeErr(w, http.StatusConflict, "not_connected", "Connect to "+machine+" first.")
	case errors.Is(err, moves.ErrArchived):
		writeErr(w, http.StatusConflict, "archived", "This workspace moved to "+strings.TrimPrefix(err.Error(), moves.ErrArchived.Error()+": ")+".")
	case errors.Is(err, moves.ErrSelf), errors.Is(err, moves.ErrBusy), errors.Is(err, moves.ErrInFlight):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) handleWorkspaceMoveGet(w http.ResponseWriter, r *http.Request) {
	mv, err := s.moves.Get(r.Context(), s.workspaceID(r.Context(), r.PathValue("id")), r.PathValue("move"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "move not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mv)
}

// handleWorkspaceImport restores a workspace that another machine of the
// account sends (POST /v1/workspaces/{id}/move on that machine).
func (s *Server) handleWorkspaceImport(w http.ResponseWriter, r *http.Request) {
	if a, ok := tools.ActorFrom(r.Context()); !ok || a.Kind != tools.ActorMobile || !strings.HasPrefix(a.Name, "machine:") {
		writeForbidden(w, "Only another machine of the account can send a workspace.")
		return
	}
	res, err := s.moves.Import(r.Context(), r.Body)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, moves.ErrVersion):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.Is(err, moves.ErrExists), errors.Is(err, moves.ErrNewer), errors.Is(err, moves.ErrNoFolder):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// moveTarget reads the machine field of a workspace move body and puts the
// body back. It fails closed: a body it cannot read counts as a machine move.
func moveTarget(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return "?"
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var in struct {
		Machine string `json:"machine"`
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	if json.Unmarshal(body, &in) != nil {
		return "?"
	}
	return strings.TrimSpace(in.Machine)
}

// archivedGate refuses a write to a workspace that moved to another
// machine. Reads, the delete, and a retry of the move still pass.
func (s *Server) archivedGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		ref := r.URL.Query().Get("workspace")
		if rest, ok := strings.CutPrefix(r.URL.Path, "/v1/workspaces/"); ok {
			id, sub, _ := strings.Cut(rest, "/")
			if (sub == "" && r.Method == http.MethodDelete) || sub == "move" || id == "import" {
				next.ServeHTTP(w, r)
				return
			}
			ref = id
		}
		if ref == "" || s.ws == nil {
			next.ServeHTTP(w, r)
			return
		}
		if to, ok := s.ws.MovedTo(r.Context(), ref); ok {
			writeErr(w, http.StatusConflict, "archived", "This workspace moved to "+to+".")
			return
		}
		next.ServeHTTP(w, r)
	})
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
