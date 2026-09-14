package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeAgents(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{id}/agents", s.handleAgentsList)
	mux.HandleFunc("GET /v1/workspaces/{id}/agents/{name}", s.handleAgentGet)
	mux.HandleFunc("PUT /v1/workspaces/{id}/agents/{name}", s.handleAgentPut)
	mux.HandleFunc("PATCH /v1/workspaces/{id}/agents/{name}", s.handleAgentPatch)
	mux.HandleFunc("DELETE /v1/workspaces/{id}/agents/{name}", s.handleAgentDelete)
	mux.HandleFunc("POST /v1/workspaces/{id}/agents/{name}/wake", s.handleAgentWake)
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
	setETagHash(w, p.Hash)
	writeJSON(w, http.StatusOK, p)
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
		md = seededAgentMarkdown(name)
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

// agentPayload is the agent wire shape: the job show fields plus memory,
// disabled, and the owned job list (Part H).
type agentPayload struct {
	Name        string          `json:"name"`
	IndexName   string          `json:"index_name"`
	WorkspaceID string          `json:"workspace_id"`
	Path        string          `json:"path"`
	Harness     string          `json:"harness"`
	Active      bool            `json:"active"`
	Disabled    bool            `json:"disabled"`
	Hash        string          `json:"hash"`
	ParseError  string          `json:"parse_error,omitempty"`
	Prompt      string          `json:"prompt"`
	Description string          `json:"description,omitempty"`
	Memory      bool            `json:"memory"`
	Triggers    []job.Trigger   `json:"triggers"`
	Session     job.SessionYAML `json:"session"`
	EnvSet      string          `json:"env_set"`
	JobCount    int             `json:"job_count"`
	Jobs        []string        `json:"jobs,omitempty"` // names, list/show only
	Markdown    string          `json:"markdown,omitempty"`
	MTime       string          `json:"mtime,omitempty"`
}

// buildAgentPayload reads the AGENT.md once. withDocs adds markdown, mtime,
// and the full-content hash; the list shape uses the index hash.
func buildAgentPayload(row workspaces.JobRow, jobs []string, withDocs bool) agentPayload {
	p := agentPayload{
		Name: strings.TrimPrefix(row.Name, "agents/"), IndexName: row.Name,
		WorkspaceID: row.WorkspaceID, Path: row.Path,
		Harness: row.Harness, Active: row.Active, Hash: row.Hash,
		Triggers: []job.Trigger{}, JobCount: len(jobs), Jobs: jobs,
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
		p.Active = !j.Disabled
		p.Disabled = j.Disabled
		// Layout errors live only on the index row; keep them beside the
		// live parse result so a broken layout stays visible.
		p.ParseError = job.JoinParseError(row.ParseError, j.ParseError)
		p.Prompt = j.Prompt
		p.Description = j.Description
		p.Memory = j.Memory
		p.Triggers = j.Triggers
		p.Session = j.Session
		p.EnvSet = j.Envset
	}
	if !withDocs && p.Triggers == nil {
		p.Triggers = []job.Trigger{}
	}
	return p
}

// agentPutRequest is the PUT agent body: full text (content) or the
// structured fields the server composes into front matter.
type agentPutRequest struct {
	Content     string           `json:"content"`
	Prompt      string           `json:"prompt"`
	Harness     string           `json:"harness"`
	Description string           `json:"description"`
	Cron        string           `json:"cron"`
	Every       string           `json:"every"`
	On          *jobEventTrigger `json:"on"`
}

// markdown composes the AGENT.md text. Empty means no field was set.
func (b agentPutRequest) markdown() (string, error) {
	spec := job.Spec{
		Content: b.Content, Prompt: b.Prompt, Harness: b.Harness,
		Description: b.Description, Cron: b.Cron, Every: b.Every,
	}
	if b.On != nil {
		spec.On = &job.EventSpec{Event: b.On.Event, Match: b.On.Match, Extract: b.On.Extract}
	}
	if spec.Content == "" && spec.Prompt == "" && spec.Harness == "" &&
		spec.Description == "" && spec.Cron == "" && spec.Every == "" && spec.On == nil {
		return "", nil
	}
	return spec.Render()
}

// ownedJobNames groups job names by owning agent index name.
func ownedJobNames(rows []workspaces.JobRow) map[string][]string {
	owned := map[string][]string{}
	for _, row := range rows {
		if row.Kind == "job" && row.Owner != "" {
			owned[row.Owner] = append(owned[row.Owner], row.Name)
		}
	}
	return owned
}

// agentNameRE bounds an agent short name: a lowercase slug, never a path.
var agentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// seededAgentMarkdown is the default AGENT.md of a bare create. It names
// {{agent_dir}} so the agent finds its ledger from the workspace root (C.3).
func seededAgentMarkdown(name string) string {
	return "---\nmemory: true\n---\n\n" +
		"You are " + name + ", an agent of this workspace.\n" +
		"Your folder is {{agent_dir}}.\n" +
		"Your memory ledger is {{agent_dir}}/MEMORY.md. Read it first. Write notes to it after each run.\n"
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
