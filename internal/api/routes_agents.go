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
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/notify"
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
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/ask", s.handleAgentAsk)
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/escalate", s.handleAgentEscalate)
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/wake", s.handleAgentWake)
}

type agentTextRequest struct {
	Text string `json:"text"`
}

func (s *Server) handleAgentTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"templates": s.templates.List()})
}

func (s *Server) handleAgentAsk(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByID(w, r)
	if !ok {
		return
	}
	var body agentTextRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, "validation", "text required")
		return
	}
	row, err := s.findAgentRow(r.Context(), ws.ID, r.PathValue("name"))
	if err != nil {
		writeResolveErr(w, err, "agent")
		return
	}
	channelType, channelRef, conversationID, err := s.agentAskChannel(r.Context(), ws.ID, row.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if conversationID != "" {
		sent, err := s.conv.Send(r.Context(), conversationID, conversations.SendOpts{
			Text: body.Text, Address: conversations.Address{Connector: channelType, Room: channelRef},
		})
		if s.convErr(w, err, "conversation error") {
			return
		}
		writeJSON(w, http.StatusCreated, conversations.AskResult{SendResult: *sent, ConversationID: conversationID})
		return
	}
	result, err := s.conv.Ask(r.Context(), conversations.AskOpts{
		Workspace: ws.ID, OperatorID: row.ID, Text: body.Text,
		Address: conversations.Address{Connector: channelType, Room: channelRef},
	})
	if s.convErr(w, err, "conversation error") {
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// agentAskChannel selects the stable thread for an agent ask. Assistant runs
// answer in the run's originating conversation; humans and other agents get
// deterministic channel threads rather than whichever conversation is newest.
func (s *Server) agentAskChannel(ctx context.Context, wsID, target string) (channelType, channelRef, conversationID string, err error) {
	targetName := "agents/" + target
	actor, hasActor := tools.ActorFrom(ctx)
	if hasActor && actor.Kind == "assistant" && actor.RunID != "" {
		run, err := s.runs.Get(ctx, actor.RunID)
		if err != nil {
			return "", "", "", err
		}
		if run.ConversationID != "" {
			return conversations.ChannelAssistant, run.ConversationID, run.ConversationID, nil
		}
	}
	if hasActor && actor.Kind == "agent" {
		caller := actor.Job
		return conversations.ChannelAgent, caller + "\u2192" + targetName, "", nil
	}
	return conversations.ChannelAssistant, wsID + "#" + targetName, "", nil
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
			RunID: body.RunID, Title: "Agent escalation", Body: body.Reason,
			Hint: "Ask the agent for detail or take over the work.",
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
		rendered, err := s.templates.Render(agents.RenderOpts{Template: body.Template, AgentID: name, Title: body.Title, Accept: body.EmailAccept, Inputs: body.Inputs})
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
			md = seededAgentMarkdown(name, body.Title, body.EmailAccept)
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
	short := strings.TrimPrefix(row.Name, "agents/")
	if r.URL.Query().Get("purge") == "1" {
		s.purgeAgent(w, r, ws, row, short, rel)
		return
	}
	if err := s.files.Delete(r.Context(), ws.Path, rel, ifMatchHeader(r)); err != nil {
		writeFilesErr(w, err)
		return
	}
	_ = s.ws.RebuildIndex(r.Context(), ws)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": short, "index_name": row.Name})
}

// purgeAgent removes the whole agents/<name>/ tree, owned jobs included.
// The index diff emits agent.deleted and one job.deleted per child.
func (s *Server) purgeAgent(w http.ResponseWriter, r *http.Request, ws *workspaces.Workspace, row workspaces.JobRow, short, rel string) {
	if !s.requireIfMatch(w, r, ws.Path, rel) {
		return
	}
	n, ok := s.purgeFolder(w, r, ws, filepath.Dir(row.Path))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": short, "purged": true, "files_removed": n,
	})
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
	if err := s.ws.RenameAgent(r.Context(), ws.ID, fromShort, body.To); err != nil {
		switch {
		case errors.Is(err, workspaces.ErrAgentExists):
			writeErr(w, http.StatusConflict, "conflict", "agent exists")
		case errors.Is(err, workspaces.ErrAgentLiveRun):
			writeErr(w, http.StatusConflict, "conflict", "agent has a live run")
		default:
			writeResolveErr(w, err, "agent")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"renamed": true, "from": fromShort, "to": body.To,
		"from_index": row.Name, "to_index": request.AgentIndexName(body.To),
	})
}

// agentPayload is the agent wire shape: the job show fields plus memory,
// disabled, and the owned job list (Part H).
type agentPayload struct {
	ID                string           `json:"id"`
	Name              string           `json:"name"`
	IndexName         string           `json:"index_name"`
	WorkspaceID       string           `json:"workspace_id"`
	Path              string           `json:"path"`
	Harness           string           `json:"harness"`
	Model             string           `json:"model,omitempty"`
	Effort            string           `json:"effort,omitempty"`
	Active            bool             `json:"active"`
	Disabled          bool             `json:"disabled"`
	Hash              string           `json:"hash"`
	ParseError        string           `json:"parse_error,omitempty"`
	Prompt            string           `json:"prompt"`
	Description       string           `json:"description,omitempty"`
	Title             string           `json:"title,omitempty"`
	EmailAccept       []string         `json:"email_accept,omitempty"`
	Status            string           `json:"status"`
	LastRun           *runs.RunSummary `json:"last_run,omitempty"`
	NextRunAt         *time.Time       `json:"next_run_at,omitempty"`
	CostMTD           float64          `json:"cost_mtd_usd"`
	OpenItems         int              `json:"open_items"`
	OpenConversations int              `json:"open_conversations"`
	Budget            *agentBudget     `json:"budget,omitempty"`
	Kit               *job.KitYAML     `json:"kit,omitempty"`
	Report            *job.ReportYAML  `json:"report,omitempty"`
	Memory            bool             `json:"memory"`
	Triggers          []job.Trigger    `json:"triggers"`
	Session           job.SessionYAML  `json:"session"`
	// EnvSets is the frontmatter envset list, in file order. EnvSet below
	// is its first value, kept for one release so the pinned UI keeps
	// working; drop it once the UI reads env_sets (plan 2026-09-24 H.3).
	EnvSets  []string `json:"env_sets"`
	EnvSet   string   `json:"env_set"`
	JobCount int      `json:"job_count"`
	Jobs     []string `json:"jobs,omitempty"` // names, list/show only
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
		EnvSets: []string{},
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
		p.Title = j.Title
		if p.Title == "" {
			p.Title = p.Name
		}
		p.EmailAccept = j.EmailAccept
		p.Kit = j.Kit
		p.Report = j.Report
		if j.Budget != nil {
			spent := p.CostMTD
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
		// env_set carries the first value for one release (H.3); the
		// list is the wire shape going forward.
		p.EnvSets = j.Envset.List()
		p.EnvSet = j.Envset.First()
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
	if spec.Content == "" && spec.Prompt == "" && spec.Harness == "" &&
		spec.Description == "" && spec.Cron == "" && spec.Every == "" && spec.On == nil &&
		spec.Title == "" && spec.EmailAccept == nil && spec.Contact == nil && spec.Notify == nil {
		return "", nil
	}
	return spec.Render()
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
func seededAgentMarkdown(name, title string, accept []string) string {
	display := title
	if display == "" {
		display = name
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %q\n", display)
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
	owners := make([]string, 0, len(list))
	byOwner := make(map[string]int, len(list))
	for i := range list {
		owners = append(owners, list[i].ID)
		byOwner[list[i].ID] = i
		s.setAgentEmail(ctx, &list[i], wsID)
	}
	// ownerOf maps a job or agent ID to the agent ID that owns its lane.
	ownerOf := make(map[string]string, len(rows))
	for _, row := range rows {
		switch {
		case row.Kind == "agent":
			ownerOf[row.ID] = row.ID
		case row.OwnerID != "":
			ownerOf[row.ID] = row.OwnerID
		}
	}
	if costs, err := s.runs.OwnerCosts(ctx, wsID, owners); err == nil {
		for owner, i := range byOwner {
			list[i].CostMTD = costs[owner]
			if list[i].Budget != nil {
				list[i].Budget.SpentUSD = costs[owner]
				switch {
				case costs[owner] >= list[i].Budget.MonthlyUSD:
					list[i].Budget.State = "exceeded"
				case list[i].Budget.MonthlyUSD > 0 && costs[owner] >= list[i].Budget.MonthlyUSD*0.8:
					list[i].Budget.State = "warn"
				default:
					list[i].Budget.State = "ok"
				}
			}
		}
	}
	if items, err := s.alerts.List(ctx, alerts.ListOpts{Workspace: wsID, Status: string(alerts.InboxUnread), Limit: 200}); err == nil {
		for _, item := range items {
			owner := ownerOf[item.JobID]
			if i, ok := byOwner[owner]; ok {
				list[i].OpenItems++
			}
		}
	}
	if convs, err := s.conv.List(ctx, conversations.ListOpts{Workspace: wsID, Limit: 200}); err == nil {
		for _, conv := range convs {
			if i, ok := byOwner[conv.OperatorID]; ok {
				list[i].OpenConversations++
			}
		}
	}
	if schedules, err := s.sched.List(ctx, schedule.ListOpts{Workspace: wsID, Limit: maxListLimit}); err == nil {
		for _, row := range schedules {
			i, ok := byOwner[ownerOf[row.JobID]]
			if !ok || row.NextDueAt == "" {
				continue
			}
			if due, err := time.Parse(time.RFC3339, row.NextDueAt); err == nil && (list[i].NextRunAt == nil || due.Before(*list[i].NextRunAt)) {
				due := due
				list[i].NextRunAt = &due
			}
		}
	}
	recent, _ := s.runs.List(ctx, runs.ListOpts{Workspace: wsID, Limit: maxListLimit})
	live := map[string]bool{}
	last := map[string]runs.RunSummary{}
	tail := map[string][]recentRun{} // per owner, newest first, capped
	runs7 := map[string]int{}
	ok7 := map[string]int{}
	window := time.Now().Add(-7 * 24 * time.Hour)
	for _, run := range recent {
		owner := ownerOf[run.JobID]
		if owner == "" {
			continue
		}
		if withRuns {
			if n := len(tail[owner]); n < recentRunCap {
				tail[owner] = append(tail[owner], recentRun{
					ID:        run.ID,
					Status:    string(run.Status),
					StartedAt: run.StartedAt.UTC().Format(time.RFC3339Nano),
				})
			}
			if !run.StartedAt.Before(window) {
				runs7[owner]++
				if run.Status == runs.StatusComplete {
					ok7[owner]++
				}
			}
		}
		if run.Status.IsLive() {
			live[owner] = true
		}
		if _, exists := last[owner]; !exists && !run.Status.IsLive() {
			last[owner] = runs.RunSummary{
				ID: run.ID, Status: run.Status, EndReason: run.EndReason,
				StartedAt: run.StartedAt, DurationMS: run.DurationMS,
				CostUSD: run.Tokens.CostUSD,
			}
		}
	}
	for owner, i := range byOwner {
		if summary, ok := last[owner]; ok {
			summary := summary
			list[i].LastRun = &summary
		}
		if withRuns {
			rr := tail[owner]
			slices.Reverse(rr)
			if rr == nil {
				rr = []recentRun{}
			}
			list[i].RecentRuns = rr
			list[i].Runs7d = runs7[owner]
			list[i].Ok7d = ok7[owner]
		}
		switch {
		case list[i].Disabled:
			list[i].Status = "disabled"
		case list[i].ParseError != "":
			list[i].Status = "error"
		case live[owner]:
			list[i].Status = "running"
		case list[i].OpenItems > 0:
			list[i].Status = "attention"
		default:
			list[i].Status = "idle"
		}
	}
}
