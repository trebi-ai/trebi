package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/agents"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/chains"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/folderenv"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/machines"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
	"gopkg.in/yaml.v3"
)

func (s *Server) routeAgents(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/agents", s.handleAgentsListAll)
	mux.HandleFunc("GET /v1/agent-templates", s.handleAgentTemplates)
	mux.HandleFunc("GET /v1/workspaces/{id}/agents", s.handleAgentsList)
	mux.HandleFunc("GET /v1/workspaces/{id}/agents/{name}", s.handleAgentGet)
	mux.HandleFunc("PUT /v1/workspaces/{id}/agents/{name}", s.handleAgentPut)
	mux.HandleFunc("PATCH /v1/workspaces/{id}/agents/{name}", s.handleAgentPatch)
	mux.HandleFunc("DELETE /v1/workspaces/{id}/agents/{name}", s.handleAgentDelete)
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/rename", s.handleAgentRename)
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/folders/rename", s.handleAgentFolderRename)
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/ask", s.handleAgentAsk)
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/handoff", s.handleAgentHandoff)
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/escalate", s.handleAgentEscalate)
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/wake", s.handleAgentWake)
}

type agentTextRequest struct {
	Text string `json:"text"`
}

func (s *Server) handleAgentTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"templates": s.templates.List()})
}

type agentAskRequest struct {
	Text           string `json:"text"`
	Context        string `json:"context"`
	IdempotencyKey string `json:"idempotency_key"`
	WaitMS         int    `json:"wait_ms"`
}

func (s *Server) handleAgentAsk(w http.ResponseWriter, r *http.Request) {
	s.agentHop(w, r, chains.KindAsk)
}

func (s *Server) handleAgentHandoff(w http.ResponseWriter, r *http.Request) {
	s.agentHop(w, r, chains.KindHandoff)
}

// agentHop sends an ask or a hand-off through the chains service.
func (s *Server) agentHop(w http.ResponseWriter, r *http.Request, kind chains.Kind) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	var body agentAskRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	opts := chains.AskOpts{
		Workspace: ws.ID, To: r.PathValue("name"), Kind: kind,
		Text: body.Text, Context: body.Context, IdempotencyKey: body.IdempotencyKey,
	}
	if a, ok := tools.ActorFrom(r.Context()); ok {
		opts.Actor = &a
	}
	res, err := s.chains.Ask(r.Context(), opts)
	if err != nil {
		writeChainErr(w, err)
		return
	}
	code := http.StatusCreated
	switch {
	case res.Repeat:
		code = http.StatusOK
	case res.Held:
		code = http.StatusAccepted
	}
	if kind == chains.KindAsk && body.WaitMS > 0 && res.Hop.From.Kind != chains.EndAgent && !res.Held {
		res.Replies, err = s.chains.WaitReplies(r.Context(), res.Hop.ID, time.Duration(body.WaitMS)*time.Millisecond)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	writeJSON(w, code, res)
}

type agentEscalateRequest struct {
	To     string `json:"to"`
	Reason string `json:"reason"`
	RunID  string `json:"run_id"`
}

func (s *Server) handleAgentEscalate(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	var body agentEscalateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	row, err := s.findAgentRow(r.Context(), ws.ID, r.PathValue("name"))
	if err != nil {
		writeResolveErr(w, err, "agent")
		return
	}
	if scope, agent := scopeFrom(r.Context()); !agent || scope.agentID != row.ID {
		writeForbidden(w, "only the named agent caller may escalate")
		return
	}
	if body.Reason == "" {
		writeErr(w, http.StatusBadRequest, "validation", "reason required")
		return
	}
	switch body.To {
	case "operator":
		if _, err := s.alerts.Raise(r.Context(), alerts.RaiseOpts{
			Kind: alerts.KindAgentEscalation, WorkspaceID: ws.ID, JobName: row.Name,
			RunID: body.RunID, Body: body.Reason,
			Hint: "Ask the agent for more detail, or do the work yourself.",
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	case "assistant":
		assistantConvs, err := s.conv.List(r.Context(), conversations.ListOpts{Workspace: ws.ID, Limit: 1})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		assistantID := ""
		if len(assistantConvs) > 0 {
			assistantID = assistantConvs[0].ID
		}
		if _, err := s.conv.Ask(r.Context(), conversations.AskOpts{
			Workspace: ws.ID, Text: "Agent " + row.Name + " escalated:\n\n" + body.Reason,
			Address: conversations.Address{Connector: conversations.ChannelAgent, Room: "assistant:" + assistantID},
		}); err != nil {
			s.log.Error("assistant escalation ask failed", "error", err)
			s.convErr(w, err, "conversation error")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "validation", "to must be operator or assistant")
		return
	}
	_, _ = s.st.Append(r.Context(), store.BusEvent{
		Topic: "agent.escalated", Entity: "agent", EntityID: row.ID, Action: "escalated",
		Attrs: map[string]string{"workspace": ws.ID, "agent_id": row.ID, "agent_name": strings.TrimPrefix(row.Name, "agents/"), "to": body.To},
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"escalated": true, "to": body.To})
}

func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	rows, err := s.ws.ListJobs(r.Context(), ws.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	owned := ownedJobNames(rows)
	list := make([]agentPayload, 0, 4)
	for _, row := range rows {
		if row.Kind != "agent" {
			continue
		}
		list = append(list, buildAgentPayload(row, owned[row.Name], false))
	}
	list = filterAgentsFor(list, r.URL.Query().Get("for"))
	s.setAgentListMetadata(r.Context(), ws.ID, rows, list, true)
	writeJSON(w, http.StatusOK, map[string]any{"agents": list})
}

func (s *Server) handleAgentsListAll(w http.ResponseWriter, r *http.Request) {
	want := strings.TrimSpace(r.URL.Query().Get("workspace"))
	var wsFilter *workspaces.Workspace
	if want != "" {
		ws, err := s.ws.Resolve(r.Context(), want)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
			return
		}
		wsFilter = ws
	}
	all, err := s.ws.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	list := []agentPayload{}
	for i := range all {
		if wsFilter != nil && all[i].ID != wsFilter.ID {
			continue
		}
		rows, err := s.ws.ListJobs(r.Context(), all[i].ID)
		if err != nil {
			continue
		}
		owned := ownedJobNames(rows)
		base := len(list)
		for _, row := range rows {
			if row.Kind != "agent" {
				continue
			}
			list = append(list, buildAgentPayload(row, owned[row.Name], false))
		}
		list = append(list[:base], filterAgentsFor(list[base:], r.URL.Query().Get("for"))...)
		s.setAgentListMetadata(r.Context(), all[i].ID, rows, list[base:], true)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": list})
}

func (s *Server) handleAgentGet(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	row, err := s.findAgentRow(r.Context(), ws.ID, r.PathValue("name"))
	if err != nil {
		writeResolveErr(w, err, "agent")
		return
	}
	rows, err := s.ws.ListJobs(r.Context(), ws.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	p := buildAgentPayload(row, ownedJobNames(rows)[row.Name], true)
	if j, err := job.ParseFile(row.Path); err == nil && j != nil {
		sum := j.FolderEnvSummary(j.OwnerKit())
		p.FolderEnv = &sum
		p.Readiness = s.readiness(r.Context(), ws.ID, j, j.OwnerKit())
	}
	s.setAgentListMetadata(r.Context(), ws.ID, rows, []agentPayload{p}, false)
	setETagHash(w, p.Hash)
	writeJSON(w, http.StatusOK, p)
}

// setAgentEmail fills the masked subaddress when the workspace has an
// address. A channel error never fails the payload.
func (s *Server) setAgentEmail(ctx context.Context, p *agentPayload, wsID string) {
	addr, err := s.email.AgentAddress(ctx, wsID, p.Name)
	if err != nil {
		return
	}
	p.EmailAddress = addr
}

// handleAgentPut is the agent upsert. A new folder creates the agent and
// seeds MEMORY.md when memory is on; an existing AGENT.md takes a full
// replace behind If-Match.
func (s *Server) handleAgentPut(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !agentNameRE.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "validation", "agent name must match "+agentNameRE.String())
		return
	}
	var body agentPutRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	rel := "agents/" + name + "/AGENT.md"
	exists := true
	if _, err := s.files.Stat(r.Context(), ws.Path, rel); err != nil {
		if !errors.Is(err, files.ErrNotFound) {
			writeFilesErr(w, err)
			return
		}
		exists = false
	}
	if scope, ok := scopeFrom(r.Context()); ok && request.AgentIndexName(name) != scope.agent {
		writeForbidden(w, "agent may change only itself")
		return
	}
	if body.Template != "" {
		if exists {
			writeErr(w, http.StatusConflict, "conflict", "agent exists")
			return
		}
		rendered, err := s.templates.Render(agents.RenderOpts{Template: body.Template, AgentID: name, Title: body.Title, Description: body.Description, Accept: body.EmailAccept, Inputs: body.Inputs})
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		var hash string
		for _, file := range rendered {
			info, _, err := s.files.Write(r.Context(), ws.Path, "agents/"+name+"/"+file.Rel, file.Content, "")
			if err != nil {
				writeFilesErr(w, err)
				return
			}
			if file.Rel == "AGENT.md" {
				hash = info.Hash
			}
		}
		_ = s.ws.RebuildIndex(r.Context(), ws)
		writeJSON(w, http.StatusCreated, map[string]any{
			"name": name, "index_name": request.AgentIndexName(name), "template": body.Template, "hash": hash,
		})
		return
	}
	md, err := body.markdown()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if md == "" && exists {
		writeErr(w, http.StatusBadRequest, "validation", "content required; refusing empty full-replace")
		return
	}
	if md == "" {
		// Structured identity fields seed a new agent only. Replacing an
		// existing agent requires complete content.
		if !exists {
			md = seededAgentMarkdown(name, body.Title, body.Description, body.EmailAccept)
		}
	}
	if !exists {
		parsed, _ := job.ParseBytes(rel, []byte(md))
		desc := ""
		if parsed != nil {
			desc = parsed.Description
		}
		if err := job.CheckAgentDescription(desc); err != nil {
			writeErrDetails(w, http.StatusBadRequest, "validation", err.Error(), map[string]any{"field": "description"})
			return
		}
	}
	// An agent never rewrites its own behavior document. The full-replace
	// request is a proposal; other callers continue through direct file IO.
	if _, agent := scopeFrom(r.Context()); agent {
		s.proposeAgentMarkdown(ws, rel, []byte(md), w, r)
		return
	}
	info, _, err := s.files.Write(r.Context(), ws.Path, rel, []byte(md), ifMatchHeader(r))
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	_ = s.ws.RebuildIndex(r.Context(), ws)
	setETagHash(w, info.Hash)
	if !exists && agentMemoryOn(md) {
		s.seedAgentMemory(r.Context(), ws, name)
	}
	code := http.StatusOK
	if !exists {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{
		"name": name, "index_name": request.AgentIndexName(name),
		"path": filepath.Join(ws.Path, filepath.FromSlash(rel)), "hash": info.Hash,
	})
}

// renderPatchedMarkdown applies a JSON merge patch to an agent document
// in memory and returns the next bytes plus the base hash. It supports the
// complete AGENT.md identity/trust/spend surface, unlike job scalar patching.
func renderPatchedMarkdown(path string, body map[string]any) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	text := string(data)
	var fm map[string]any
	rest := text
	if strings.HasPrefix(text, "---\n") {
		end := strings.Index(text[4:], "\n---")
		if end < 0 {
			return nil, "", fmt.Errorf("agent frontmatter is not terminated")
		}
		if err := yaml.Unmarshal([]byte(text[4:4+end]), &fm); err != nil {
			return nil, "", fmt.Errorf("parsing frontmatter: %w", err)
		}
		rest = text[4+end+4:]
	} else {
		fm = map[string]any{}
	}
	for key, value := range body {
		if value == nil {
			delete(fm, key)
		} else {
			fm[key] = value
		}
	}
	var out strings.Builder
	out.WriteString("---\n")
	yb, err := yaml.Marshal(fm)
	if err != nil {
		return nil, "", err
	}
	out.Write(yb)
	out.WriteString("---\n")
	out.WriteString(rest)
	return []byte(out.String()), job.ContentHash(data), nil
}

func (s *Server) handleAgentPatch(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	row, err := s.findAgentRow(r.Context(), ws.ID, r.PathValue("name"))
	if err != nil {
		writeResolveErr(w, err, "agent")
		return
	}
	rel, err := workspaceRel(ws.Path, row.Path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if scope, agent := scopeFrom(r.Context()); agent {
		if row.ID != scope.agentID {
			writeForbidden(w, "agent may change only itself")
			return
		}
		next, _, err := renderPatchedMarkdown(row.Path, body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		s.proposeAgentMarkdown(ws, rel, next, w, r)
		return
	}
	hash, ok := s.patchIndexedMarkdown(w, r, ws, row.Path, rel, body)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": strings.TrimPrefix(row.Name, "agents/"), "index_name": row.Name,
		"patched": true, "hash": hash,
	})
}

func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	row, err := s.findAgentRow(r.Context(), ws.ID, r.PathValue("name"))
	if err != nil {
		writeResolveErr(w, err, "agent")
		return
	}
	rel, err := workspaceRel(ws.Path, row.Path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	d, ok := s.agentDeletePlan(w, r, ws, row)
	if !ok {
		return
	}
	if !confirmed(r) {
		writeConfirmRequired(w, d.describe())
		return
	}
	if !s.requireIfMatch(w, r, ws.Path, rel) {
		return
	}
	moved, ok := s.moveAgentJobs(w, r, ws, d)
	if !ok {
		return
	}
	n, ok := s.purgeFolder(w, r, ws, filepath.Dir(row.Path))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": d.short, "index_name": row.Name, "moved": moved,
		"purged": len(d.jobs) > 0 && d.to.ID == "", "files_removed": n,
	})
}

// agentDelete is a checked agent delete: the jobs of the agent and the
// agent that gets them (empty when the jobs go with the agent).
type agentDelete struct {
	short string
	jobs  []workspaces.JobRow
	to    workspaces.JobRow
}

// agentDeletePlan reads move_jobs_to and purge. An agent with jobs needs
// one of the two choices.
func (s *Server) agentDeletePlan(w http.ResponseWriter, r *http.Request, ws *workspaces.Workspace, row workspaces.JobRow) (agentDelete, bool) {
	d := agentDelete{short: strings.TrimPrefix(row.Name, "agents/")}
	moveTo := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("move_jobs_to")), "agents/")
	purge := r.URL.Query().Get("purge") == "1"
	if moveTo != "" && purge {
		writeErr(w, http.StatusBadRequest, "validation", "use move_jobs_to or purge, not both")
		return d, false
	}
	rows, err := s.ws.ListJobs(r.Context(), ws.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return d, false
	}
	for _, j := range rows {
		if j.Kind == "job" && job.OwnerOf(j.Name) == row.Name {
			d.jobs = append(d.jobs, j)
		}
	}
	if moveTo != "" {
		if moveTo == d.short {
			writeErr(w, http.StatusBadRequest, "validation", "move_jobs_to names the agent that you delete")
			return d, false
		}
		if d.to, err = s.findAgentRow(r.Context(), ws.ID, moveTo); err != nil {
			writeResolveErr(w, err, "agent")
			return d, false
		}
	}
	if len(d.jobs) > 0 && moveTo == "" && !purge {
		writeErrDetails(w, http.StatusConflict, "conflict",
			fmt.Sprintf("agent %s has %d jobs: move them to another agent or delete them with the agent", d.short, len(d.jobs)),
			map[string]any{"jobs": len(d.jobs)})
		return d, false
	}
	return d, true
}

func (d agentDelete) describe() string {
	switch {
	case len(d.jobs) == 0:
		return "deletes agent " + d.short + " and its folder"
	case d.to.ID != "":
		return fmt.Sprintf("moves the %d jobs of agent %s to %s, then deletes agent %s and its folder",
			len(d.jobs), d.short, strings.TrimPrefix(d.to.Name, "agents/"), d.short)
	default:
		return fmt.Sprintf("deletes agent %s and its %d jobs; run rows stay", d.short, len(d.jobs))
	}
}

// movedJob is one entry of the moved list of an agent delete.
type movedJob struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// moveAgentJobs moves each job of the agent to d.to with its folder. The
// first failure stops the delete; the jobs moved before it stay moved.
func (s *Server) moveAgentJobs(w http.ResponseWriter, r *http.Request, ws *workspaces.Workspace, d agentDelete) ([]movedJob, bool) {
	out := []movedJob{}
	if d.to.ID == "" {
		return out, true
	}
	to := strings.TrimPrefix(d.to.Name, "agents/")
	for _, j := range d.jobs {
		folder := j.Folder
		res, moved, err := s.ws.MoveJob(r.Context(), ws.ID, j.Name, workspaces.JobMove{Agent: &to, Folder: &folder})
		if err != nil {
			code, status := http.StatusInternalServerError, "internal"
			if errors.Is(err, workspaces.ErrLiveRun) || errors.Is(err, workspaces.ErrExists) {
				code, status = http.StatusConflict, "conflict"
			}
			writeErrDetails(w, code, status, fmt.Sprintf("move job %s: %v; agent %s is not deleted", j.Name, err, d.short),
				map[string]any{"job": j.Name, "moved": out})
			return nil, false
		}
		s.recomputeMoved(r.Context(), ws.ID, moved)
		out = append(out, movedJob{From: res.From, To: res.To})
	}
	return out, true
}

func (s *Server) handleAgentWake(w http.ResponseWriter, r *http.Request) {
	if scope, ok := scopeFrom(r.Context()); ok && request.AgentIndexName(r.PathValue("name")) != scope.agent {
		writeForbidden(w, "agent may wake only itself")
		return
	}
	wsID := r.PathValue("id")
	var body runCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	row, err := s.findAgentRow(r.Context(), wsID, r.PathValue("name"))
	if err != nil {
		writeResolveErr(w, err, "agent")
		return
	}
	// Agent lane: keep client prompt_override/inputs, pin identity fields.
	body.Job = row.Name
	body.Workspace = wsID
	body.Kind = "agent"
	if body.Reason == "" {
		body.Reason = string(runs.ReasonManual)
	}
	s.createRun(w, r, body)
}

type agentRenameRequest struct {
	To string `json:"to"`
}

// handleAgentRename is the one identity-changing operation. The folder and
// every stored job reference move together (plan C.2).
func (s *Server) handleAgentRename(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	var body agentRenameRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	body.To = strings.TrimSpace(body.To)
	if !agentNameRE.MatchString(body.To) {
		writeErr(w, http.StatusBadRequest, "validation", "agent name must match "+agentNameRE.String())
		return
	}
	fromShort := r.PathValue("name")
	if !agentNameRE.MatchString(fromShort) {
		writeErr(w, http.StatusBadRequest, "validation", "agent name must match "+agentNameRE.String())
		return
	}
	row, err := s.findAgentRow(r.Context(), ws.ID, fromShort)
	if err != nil {
		writeResolveErr(w, err, "agent")
		return
	}
	live, err := s.runs.HasLiveJobs(r.Context(), ws.ID, row.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if live {
		writeErr(w, http.StatusConflict, "conflict", "agent has a live run")
		return
	}
	askers, err := s.agentAskers(r.Context(), ws.ID, fromShort)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	email := ""
	if s.email != nil {
		email, _ = s.email.AgentAddress(r.Context(), ws.ID, body.To) // a channel error never fails the rename
	}
	reply := agentRenamed{
		Renamed: true, From: fromShort, To: body.To,
		FromIndex: row.Name, ToIndex: request.AgentIndexName(body.To),
		Askers: askers, EmailAddress: email,
	}
	if r.URL.Query().Get("dry_run") == "1" {
		if fromShort == body.To {
			writeErr(w, http.StatusConflict, "conflict", "An agent with this name exists")
			return
		}
		if _, err := s.findAgentRow(r.Context(), ws.ID, body.To); err == nil {
			writeErr(w, http.StatusConflict, "conflict", "An agent with this name exists")
			return
		}
		reply.Renamed = false
		reply.DryRun = true
		writeJSON(w, http.StatusOK, reply)
		return
	}
	moved, err := s.ws.RenameAgent(r.Context(), ws.ID, fromShort, body.To)
	if err != nil {
		if errors.Is(err, workspaces.ErrExists) {
			writeErr(w, http.StatusConflict, "conflict", "An agent with this name exists")
			return
		}
		writeRenameErr(w, err, "agent")
		return
	}
	s.recomputeMoved(r.Context(), ws.ID, moved)
	writeJSON(w, http.StatusOK, reply)
}

type folderRenameRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// handleAgentFolderRename changes the folder of each job of the agent in
// one folder. To "" takes those jobs out of the folder.
func (s *Server) handleAgentFolderRename(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if scope, scoped := scopeFrom(r.Context()); scoped && (scope.workspaceID != ws.ID || scope.agent != "agents/"+name) {
		writeForbidden(w, "agent may change only jobs owned by "+scope.agent)
		return
	}
	var body folderRenameRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if _, err := job.NormalizeFolder(body.To); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if strings.TrimSpace(body.From) == "" {
		writeErr(w, http.StatusBadRequest, "validation", "from is required")
		return
	}
	res, err := s.ws.RenameFolder(r.Context(), ws.ID, name, body.From, body.To)
	switch {
	case errors.Is(err, workspaces.ErrNoAgent):
		writeErr(w, http.StatusNotFound, "not_found", "agent not found")
	case errors.Is(err, workspaces.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "no job of "+name+" is in folder "+body.From)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// agentRenamed is the reply of an agent rename. Askers are the agents whose
// team.can_ask lists the old name; the rename does not rewrite them.
type agentRenamed struct {
	Renamed      bool     `json:"renamed"`
	DryRun       bool     `json:"dry_run,omitempty"`
	From         string   `json:"from"`
	To           string   `json:"to"`
	FromIndex    string   `json:"from_index"`
	ToIndex      string   `json:"to_index"`
	Askers       []string `json:"askers"`
	EmailAddress string   `json:"email_address"`
}

// agentAskers returns the short names of the agents whose team.can_ask
// lists name.
func (s *Server) agentAskers(ctx context.Context, wsID, name string) ([]string, error) {
	rows, err := s.ws.ListJobs(ctx, wsID)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, row := range rows {
		if row.Kind != "agent" {
			continue
		}
		j, _ := job.ParseFile(row.Path)
		if j != nil && j.Team != nil && slices.Contains(j.Team.CanAsk, name) {
			out = append(out, strings.TrimPrefix(row.Name, "agents/"))
		}
	}
	return out, nil
}

// agentPayload is the agent wire shape: the job show fields plus memory,
// disabled, and the owned job list (Part H).
type agentPayload struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	IndexName         string              `json:"index_name"`
	WorkspaceID       string              `json:"workspace_id"`
	Path              string              `json:"path"`
	Harness           string              `json:"harness"`
	Model             string              `json:"model,omitempty"`
	Effort            string              `json:"effort,omitempty"`
	Active            bool                `json:"active"`
	Disabled          bool                `json:"disabled"`
	Hash              string              `json:"hash"`
	ParseError        string              `json:"parse_error,omitempty"`
	Prompt            string              `json:"prompt"`
	Description       string              `json:"description"`
	Title             string              `json:"title,omitempty"`
	Team              agentTeam           `json:"team"`
	EmailAccept       []string            `json:"email_accept,omitempty"`
	Status            string              `json:"status"`
	LastRun           *runs.RunSummary    `json:"last_run,omitempty"`
	NextRunAt         *time.Time          `json:"next_run_at,omitempty"`
	TokensMTD         pricing.TokenTotals `json:"tokens_mtd"`
	OpenItems         int                 `json:"open_items"`
	OpenConversations int                 `json:"open_conversations"`
	Budget            *agentBudget        `json:"budget,omitempty"`
	Kit               *job.KitYAML        `json:"kit,omitempty"`
	// FolderEnv is the working dir .env; the single GET fills it.
	FolderEnv *folderenv.Summary `json:"folder_env,omitempty"`
	// Readiness is what the agent needs on its machine; the single GET fills it.
	Readiness []machines.Item `json:"readiness,omitempty"`
	Report    *job.ReportYAML `json:"report,omitempty"`
	Memory    bool            `json:"memory"`
	Triggers  []job.Trigger   `json:"triggers"`
	Session   job.SessionYAML `json:"session"`
	JobCount  int             `json:"job_count"`
	Jobs      []string        `json:"jobs,omitempty"` // names, list/show only
	// RecentRuns backs the card sparkline: the last 20 runs of this
	// agent, oldest first. The list path fills it; the single GET
	// leaves it empty.
	RecentRuns []recentRun `json:"recent_runs"`
	// Runs7d and Ok7d count this agent's runs started in the last 7
	// days among the runs the list walk loaded. The walk is one
	// maxListLimit query for the whole workspace, so a workspace busier
	// than the cap undercounts the window: the numbers are a floor, not
	// an exact 7-day total.
	Runs7d   int    `json:"runs_7d"`
	Ok7d     int    `json:"ok_7d"`
	Markdown string `json:"markdown,omitempty"`
	MTime    string `json:"mtime,omitempty"`
	// EmailAddress is the agent's masked subaddress, set when the channel
	// is live for the workspace.
	EmailAddress string `json:"email_address,omitempty"`
}

// agentTeam is the team key of AGENT.md. A null can_ask is every agent of
// the workspace.
type agentTeam struct {
	CanAsk []string `json:"can_ask"`
	Chains bool     `json:"chains"`
}

type agentBudget struct {
	MonthlyUSD float64 `json:"monthly_usd"`
	SpentUSD   float64 `json:"spent_usd"`
	State      string  `json:"state"`
}

// recentRun is one sparkline bar (plan 2026-09-24 G.1): the run identity,
// its machine status, and when it started.
type recentRun struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at"`
}

// recentRunCap bounds the sparkline: the last 20 runs per agent.
const recentRunCap = 20

// buildAgentPayload reads the AGENT.md once. withDocs adds markdown, mtime,
// and the full-content hash; the list shape uses the index hash.
func buildAgentPayload(row workspaces.JobRow, jobs []string, withDocs bool) agentPayload {
	p := agentPayload{
		ID: row.ID, Name: strings.TrimPrefix(row.Name, "agents/"), IndexName: row.Name,
		WorkspaceID: row.WorkspaceID, Path: row.Path,
		Harness: row.Harness, Active: row.Active, Hash: row.Hash,
		Triggers: []job.Trigger{}, JobCount: len(jobs), Jobs: jobs,
		Team: agentTeam{Chains: true},
	}
	var j *job.Job
	if withDocs {
		doc, err := readIndexedDoc(row.Path)
		if err != nil {
			p.ParseError = job.JoinParseError(row.ParseError, err.Error())
			return p
		}
		j = doc.Job
		p.Hash = doc.Hash
		p.MTime = doc.MTime
		p.Markdown = string(doc.Data)
	} else {
		parsed, err := job.ParseFile(row.Path)
		if err != nil && parsed == nil {
			p.ParseError = job.JoinParseError(row.ParseError, err.Error())
			return p
		}
		j = parsed
	}
	if j != nil {
		p.Harness = j.Harness
		p.Model = j.Model
		p.Effort = j.Effort
		p.Active = !j.Disabled
		p.Disabled = j.Disabled
		// Layout errors live only on the index row; keep them beside the
		// live parse result so a broken layout stays visible.
		p.ParseError = job.JoinParseError(row.ParseError, j.ParseError)
		p.Prompt = j.Prompt
		p.Description = j.Description
		p.Team = agentTeam{Chains: j.Team.InChains()}
		if j.Team != nil {
			p.Team.CanAsk = j.Team.CanAsk
		}
		p.Title = j.Title
		if p.Title == "" {
			p.Title = p.Name
		}
		p.EmailAccept = j.EmailAccept
		p.Kit = j.Kit
		p.Report = j.Report
		if j.Budget != nil {
			var spent float64 // setSpend fills the month to date
			state := "ok"
			if spent >= j.Budget.MonthlyUSD {
				state = "exceeded"
			} else if j.Budget.MonthlyUSD > 0 && spent >= j.Budget.MonthlyUSD*0.8 {
				state = "warn"
			}
			p.Budget = &agentBudget{MonthlyUSD: j.Budget.MonthlyUSD, SpentUSD: spent, State: state}
		}
		p.Memory = j.Memory
		p.Triggers = j.Triggers
		p.Session = j.Session
	}
	if !withDocs && p.Triggers == nil {
		p.Triggers = []job.Trigger{}
	}
	return p
}

// agentPutRequest is the PUT agent body: full text (content) or the
// structured fields the server composes into front matter.
type agentPutRequest struct {
	Content     string            `json:"content"`
	Title       string            `json:"title"`
	Prompt      string            `json:"prompt"`
	Harness     string            `json:"harness"`
	Description string            `json:"description"`
	EmailAccept []string          `json:"email_accept"`
	Cron        string            `json:"cron"`
	Every       string            `json:"every"`
	On          *jobEventTrigger  `json:"on"`
	Template    string            `json:"template"`
	Inputs      map[string]string `json:"inputs"`
	Contact     []string          `json:"contact"`
	Notify      []notify.Step     `json:"notify"`
}

// markdown composes the AGENT.md text. Empty means no field was set.
func (b agentPutRequest) markdown() (string, error) {
	spec := job.Spec{
		Content: b.Content, Title: b.Title, Prompt: b.Prompt, Harness: b.Harness,
		Description: b.Description, Cron: b.Cron, Every: b.Every,
		EmailAccept: b.EmailAccept, Contact: b.Contact, Notify: b.Notify,
	}
	spec.On = b.On.spec()
	// Identity fields alone (title, description, accept) seed a new agent.
	if spec.Content == "" && spec.Prompt == "" && spec.Harness == "" &&
		spec.Cron == "" && spec.Every == "" && spec.On == nil &&
		spec.Contact == nil && spec.Notify == nil {
		return "", nil
	}
	return spec.Render()
}

// filterAgentsFor keeps the agents whose name, title, or description holds
// any word of query, case-insensitive. An empty query keeps all.
func filterAgentsFor(list []agentPayload, query string) []agentPayload {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return list
	}
	out := list[:0:0]
	for _, p := range list {
		hay := strings.ToLower(p.Name + " " + p.Title + " " + p.Description)
		if slices.ContainsFunc(words, func(w string) bool { return strings.Contains(hay, w) }) {
			out = append(out, p)
		}
	}
	return out
}

// ownedJobNames groups job names by owning agent index name.
func ownedJobNames(rows []workspaces.JobRow) map[string][]string {
	owned := map[string][]string{}
	for _, row := range rows {
		if row.Kind == "job" && row.OwnerIndex() != "" {
			owned[row.OwnerIndex()] = append(owned[row.OwnerIndex()], row.Name)
		}
	}
	return owned
}

// agentNameRE bounds an agent short name: a lowercase slug, never a path.
// The 24-character cap is the email tag budget (plan Part D): the name
// becomes a subaddress, so it must fail here, not at the first send.
var agentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)

// seededAgentMarkdown is the default AGENT.md of a bare create. It names
// {{agent_dir}} so the agent finds its ledger from the workspace root (C.3).
// The kit attaches email, so the agent and its jobs can send mail.
func seededAgentMarkdown(name, title, description string, accept []string) string {
	display := title
	if display == "" {
		display = name
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %q\n", display)
	fmt.Fprintf(&b, "description: %q\n", strings.TrimSpace(description))
	b.WriteString("memory: true\n")
	if len(accept) == 0 {
		b.WriteString("email:\n")
		b.WriteString("  accept: [] # admins always pass; add more senders here\n")
	} else {
		b.WriteString("email:\n  accept:\n")
		for _, entry := range accept {
			fmt.Fprintf(&b, "    - %q\n", entry)
		}
	}
	b.WriteString("kit:\n  connectors:\n    - email\n")
	b.WriteString("---\n\n")
	b.WriteString(
		"You are " + name + ", an agent of this workspace.\n" +
			"Your folder is {{agent_dir}}.\n" +
			"Your memory ledger is {{agent_dir}}/MEMORY.md. Read it first. Write notes to it after each run.\n",
	)
	return b.String()
}

// agentMemoryOn parses composed AGENT.md text and reports the memory flag.
func agentMemoryOn(md string) bool {
	j, err := job.ParseBytes("AGENT.md", []byte(md))
	return err == nil && j != nil && j.Memory
}

// seedAgentMemory writes the MEMORY.md skeleton at create time (C.4).
func (s *Server) seedAgentMemory(ctx context.Context, ws *workspaces.Workspace, name string) {
	s.seedMemory(ctx, ws, "agents/"+name+"/MEMORY.md")
}

// setAgentListMetadata fills one workspace's agent page rollup in batch:
// recent runs, schedules, conversations, inbox items, and priced MTD cost.
// withRuns adds the sparkline fields (G.1); the single-agent GET passes
// false and leaves them empty. rows are the workspace index rows.
func (s *Server) setAgentListMetadata(ctx context.Context, wsID string, rows []workspaces.JobRow, list []agentPayload, withRuns bool) {
	if len(list) == 0 {
		return
	}
	r := newAgentRollup(rows, list)
	for i := range list {
		s.setAgentEmail(ctx, &list[i], wsID)
	}
	if spend, err := s.runs.OwnerSpend(ctx, wsID, r.owners()); err == nil {
		r.setSpend(spend)
	}
	if page, err := s.alerts.List(ctx, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Workspace: wsID, Unread: true}, Limit: 200}); err == nil {
		for _, item := range page.Items {
			if p := r.agent(r.ownerOf[item.JobID]); p != nil {
				p.OpenItems++
			}
		}
	}
	if convs, err := s.conv.List(ctx, conversations.ListOpts{Workspace: wsID, Limit: 200}); err == nil {
		for _, conv := range convs {
			if p := r.agent(conv.OperatorID); p != nil {
				p.OpenConversations++
			}
		}
	}
	if schedules, err := s.sched.List(ctx, schedule.ListOpts{Workspace: wsID, Limit: maxListLimit}); err == nil {
		for _, row := range schedules {
			r.setNextRun(row.JobID, row.NextDueAt)
		}
	}
	recent, _ := s.runs.List(ctx, runs.ListOpts{Workspace: wsID, Limit: maxListLimit})
	r.setRuns(recent, withRuns, time.Now().Add(-7*24*time.Hour))
}

// agentRollup maps index rows to the agents of one list page.
type agentRollup struct {
	list    []agentPayload
	byOwner map[string]int    // agent ID → index in list
	ownerOf map[string]string // job or agent ID → agent ID that owns its lane
}

// runTally collects the run facts of each owner, newest run first.
type runTally struct {
	live  map[string]bool
	last  map[string]runs.RunSummary
	tail  map[string][]recentRun
	runs7 map[string]int
	ok7   map[string]int
}

func newAgentRollup(rows []workspaces.JobRow, list []agentPayload) *agentRollup {
	r := &agentRollup{list: list, byOwner: make(map[string]int, len(list)), ownerOf: make(map[string]string, len(rows))}
	for i := range list {
		r.byOwner[list[i].ID] = i
	}
	for _, row := range rows {
		switch {
		case row.Kind == "agent":
			r.ownerOf[row.ID] = row.ID
		case row.OwnerID != "":
			r.ownerOf[row.ID] = row.OwnerID
		}
	}
	return r
}

func (r *agentRollup) owners() []string {
	out := make([]string, 0, len(r.list))
	for i := range r.list {
		out = append(out, r.list[i].ID)
	}
	return out
}

// agent returns the list entry of an agent ID, or nil.
func (r *agentRollup) agent(owner string) *agentPayload {
	if i, ok := r.byOwner[owner]; ok {
		return &r.list[i]
	}
	return nil
}

// setSpend shows month-to-date tokens; the budget stays in USD.
func (r *agentRollup) setSpend(spend map[string]runs.Spend) {
	for owner, i := range r.byOwner {
		p := &r.list[i]
		sp := spend[owner]
		p.TokensMTD = sp.Tokens
		if p.Budget == nil {
			continue
		}
		p.Budget.SpentUSD = sp.CostUSD
		switch {
		case sp.CostUSD >= p.Budget.MonthlyUSD:
			p.Budget.State = "exceeded"
		case p.Budget.MonthlyUSD > 0 && sp.CostUSD >= p.Budget.MonthlyUSD*0.8:
			p.Budget.State = "warn"
		default:
			p.Budget.State = "ok"
		}
	}
}

// setNextRun keeps the earliest due time of the jobs in an agent lane.
func (r *agentRollup) setNextRun(jobID, nextDueAt string) {
	p := r.agent(r.ownerOf[jobID])
	if p == nil || nextDueAt == "" {
		return
	}
	if due, err := time.Parse(time.RFC3339, nextDueAt); err == nil && (p.NextRunAt == nil || due.Before(*p.NextRunAt)) {
		p.NextRunAt = &due
	}
}

// setRuns sets the last run, the sparkline, and the status. recent is newest first.
func (r *agentRollup) setRuns(recent []*runs.Run, withRuns bool, window time.Time) {
	t := runTally{live: map[string]bool{}, last: map[string]runs.RunSummary{}, tail: map[string][]recentRun{}, runs7: map[string]int{}, ok7: map[string]int{}}
	for _, run := range recent {
		if owner := r.ownerOf[run.JobID]; owner != "" {
			t.add(owner, run, withRuns, window)
		}
	}
	for owner, i := range r.byOwner {
		p := &r.list[i]
		if summary, ok := t.last[owner]; ok {
			p.LastRun = &summary
		}
		if withRuns {
			rr := t.tail[owner]
			slices.Reverse(rr)
			if rr == nil {
				rr = []recentRun{}
			}
			p.RecentRuns, p.Runs7d, p.Ok7d = rr, t.runs7[owner], t.ok7[owner]
		}
		switch {
		case p.Disabled:
			p.Status = "disabled"
		case p.ParseError != "":
			p.Status = "error"
		case t.live[owner]:
			p.Status = "running"
		case p.OpenItems > 0:
			p.Status = "attention"
		default:
			p.Status = "idle"
		}
	}
}

func (t *runTally) add(owner string, run *runs.Run, withRuns bool, window time.Time) {
	if withRuns {
		if len(t.tail[owner]) < recentRunCap {
			t.tail[owner] = append(t.tail[owner], recentRun{
				ID:        run.ID,
				Status:    string(run.Status),
				StartedAt: run.StartedAt.UTC().Format(time.RFC3339Nano),
			})
		}
		if !run.StartedAt.Before(window) {
			t.runs7[owner]++
			if run.Status == runs.StatusComplete {
				t.ok7[owner]++
			}
		}
	}
	if run.Status.IsLive() {
		t.live[owner] = true
	}
	if _, exists := t.last[owner]; !exists && !run.Status.IsLive() {
		t.last[owner] = runs.RunSummary{
			ID: run.ID, Status: run.Status, EndReason: run.EndReason,
			StartedAt: run.StartedAt, DurationMS: run.DurationMS,
			CostUSD: run.Tokens.CostUSD, Tokens: run.Tokens,
		}
	}
}
