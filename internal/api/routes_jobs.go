package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func (s *Server) routeJobs(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/jobs", s.handleJobsList)
	mux.HandleFunc("POST /v1/jobs", s.handleJobsCreate)
	mux.HandleFunc("GET /v1/jobs/{name}", s.handleJobGet)
	mux.HandleFunc("PUT /v1/jobs/{name}", s.handleJobPut)
	mux.HandleFunc("PATCH /v1/jobs/{name}", s.handleJobPatch)
	mux.HandleFunc("DELETE /v1/jobs/{name}", s.handleJobDelete)
	mux.HandleFunc("POST /v1/jobs/{name}/run", s.handleJobRun)
	mux.HandleFunc("POST /v1/jobs/{name}/pause", s.handleJobPause)
	mux.HandleFunc("POST /v1/jobs/{name}/unpause", s.handleJobUnpause)
	mux.HandleFunc("POST /v1/jobs/validate", s.handleJobValidateText)
	mux.HandleFunc("POST /v1/jobs/{name}/validate", s.handleJobValidate)
	mux.HandleFunc("GET /v1/schedules", s.handleSchedules)
}

func (s *Server) handleJobsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	wsID := s.workspaceID(r.Context(), q.Get("workspace"))
	rows, err := s.ws.ListJobs(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	last, live, err := s.runs.StatusByJob(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	filter := jobFilter{
		q:       strings.ToLower(strings.TrimSpace(q.Get("q"))),
		harness: strings.TrimSpace(q.Get("harness")),
		active:  q.Get("active"),
		owner:   request.AgentIndexName(strings.TrimSpace(q.Get("owner"))),
	}
	if !filter.validActive() {
		writeErr(w, http.StatusBadRequest, "validation", "active must be true or false")
		return
	}
	enrich := s.jobEnrichment(r.Context(), wsID)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// B4: the assistant is a separate singleton; jobs list is jobs only.
		if row.Kind != "job" || !filter.keep(row) {
			continue
		}
		key := row.WorkspaceID + "/" + row.Name
		waiting := false
		for _, id := range live[key] {
			if s.runs.SessionState(id) == runs.SessionWaiting {
				waiting = true
				break
			}
		}
		status := runs.DeriveJobDisplayStatus(row.Active, len(live[key]), waiting, last[key])
		item := map[string]any{
			"workspace_id": row.WorkspaceID, "name": row.Name, "path": row.Path,
			"kind": row.Kind, "active": row.Active, "harness": row.Harness,
			"owner": row.Owner,
			"hash":  row.Hash, "parse_error": row.ParseError, "status": status,
			"last_run_at": "", "last_status": "", "next_due_at": "", "mtd_usd": 0.0,
			"triggers": []string{},
		}
		enrich(key, row.Path, item)
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// jobFilter holds the A6.5 list filters of GET /v1/jobs.
type jobFilter struct {
	q       string
	harness string
	active  string
	owner   string // agent index name (agents/<short>)
}

func (f jobFilter) validActive() bool {
	return f.active == "" || f.active == "true" || f.active == "false"
}

func (f jobFilter) keep(row workspaces.JobRow) bool {
	if f.q != "" && !strings.Contains(strings.ToLower(row.Name), f.q) {
		return false
	}
	if f.harness != "" && !strings.EqualFold(row.Harness, f.harness) {
		return false
	}
	if f.active != "" && row.Active != (f.active == "true") {
		return false
	}
	if f.owner != "" && row.Owner != f.owner {
		return false
	}
	return true
}

// jobEnrichment returns a function that fills the per-job A6.5 columns:
// last run, next due time, month-to-date cost, and trigger kinds. It reads the
// three tables once, not once per row.
func (s *Server) jobEnrichment(ctx context.Context, wsID string) func(key, path string, item map[string]any) {
	last, _, _ := s.runs.StatusByJob(ctx)
	due := map[string]string{}
	if rows, err := s.sched.List(ctx, schedule.ListOpts{Workspace: wsID, Limit: maxListLimit}); err == nil {
		for _, row := range rows {
			k := row.WorkspaceID + "/" + row.Job
			if due[k] == "" || (row.NextDueAt != "" && row.NextDueAt < due[k]) {
				due[k] = row.NextDueAt
			}
		}
	}
	mtd := map[string]float64{}
	recent, _ := s.runs.List(ctx, runs.ListOpts{Workspace: wsID, Limit: maxListLimit})
	lastAt := map[string]string{}
	monthStart := time.Now().UTC().Format("2006-01")
	for _, run := range recent {
		k := run.Workspace + "/" + run.Job
		if lastAt[k] == "" {
			lastAt[k] = run.StartedAt.UTC().Format(time.RFC3339)
		}
		if run.StartedAt.UTC().Format("2006-01") == monthStart {
			mtd[k] += run.Tokens.CostUSD
		}
	}
	return func(key, path string, item map[string]any) {
		item["last_run_at"] = lastAt[key]
		if st, ok := last[key]; ok {
			item["last_status"] = string(st)
		}
		item["next_due_at"] = due[key]
		item["mtd_usd"] = mtd[key]
		if j, err := job.ParseFile(path); err == nil && j != nil {
			kinds := make([]string, 0, len(j.Triggers))
			for _, t := range j.Triggers {
				kinds = append(kinds, t.Family)
			}
			item["triggers"] = kinds
		}
	}
}

// runStatusByJob returns the newest run status and live run ids per
// "workspace/job" lane (kind=job).

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	row, ok := s.resolveJob(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	doc, err := readIndexedDoc(row.Path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	j := doc.Job
	// Index name, not ParseFile's folder basename. Runs, list, and pause
	// all key off row.Name (agents/watchtower/job, not job).
	writeJSON(w, http.StatusOK, map[string]any{
		"name": row.Name, "workspace": row.WorkspaceID, "path": row.Path,
		"harness": j.Harness, "active": !j.Disabled, "hash": doc.Hash,
		"parse_error": j.ParseError, "prompt": j.Prompt,
		"input_keys": job.DiscoverInputKeys(j.Prompt),
		"triggers":   j.Triggers, "session": j.Session, "env_set": j.Envset,
		"markdown": string(doc.Data), "mtime": doc.MTime,
	})
}

func (s *Server) handleJobRun(w http.ResponseWriter, r *http.Request) {
	s.createRunFromJob(w, r, r.PathValue("name"))
}

func (s *Server) handleJobPause(w http.ResponseWriter, r *http.Request) {
	s.setJobDisabled(w, r, true)
}

func (s *Server) handleJobUnpause(w http.ResponseWriter, r *http.Request) {
	s.setJobDisabled(w, r, false)
}

func (s *Server) setJobDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	row, ok := s.resolveJob(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	ws, rel, ok := s.jobFileLoc(w, r, row)
	if !ok {
		return
	}
	if !s.requireIfMatch(w, r, ws.Path, rel) {
		return
	}
	if err := job.SetDisabled(row.Path, disabled); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	_ = s.ws.RebuildIndex(r.Context(), ws)
	writeJSON(w, http.StatusOK, map[string]any{"name": row.Name, "disabled": disabled})
}

func (s *Server) handleJobValidate(w http.ResponseWriter, r *http.Request) {
	row, ok := s.resolveJob(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	j, err := job.ParseFile(row.Path)
	valid := err == nil && j != nil && j.ParseError == ""
	msg := ""
	if err != nil {
		msg = err.Error()
	} else if j != nil {
		msg = j.ParseError
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": valid, "error": msg})
}

// handleJobValidateText checks job text, or a structured spec, without writing
// a job. Use it before create so a bad harness or trigger key is caught early.
func (s *Server) handleJobValidateText(w http.ResponseWriter, r *http.Request) {
	var body jobCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	md, err := body.markdown()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	name := cmp.Or(body.Name, "job")
	j, err := job.ParseBytes(filepath.Join(name, "JOB.md"), []byte(md))
	switch {
	case err != nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "markdown": md})
	case j != nil && j.ParseError != "":
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": j.ParseError, "markdown": md})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "error": "", "markdown": md})
	}
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	limit := parseLimit(r)
	dueBefore, err := parseTimeFilter(r.URL.Query().Get("due_before"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "due_before: "+err.Error())
		return
	}
	rows, err := s.sched.List(r.Context(), schedule.ListOpts{
		Workspace: s.workspaceID(r.Context(), r.URL.Query().Get("workspace")),
		DueBefore: dueBefore,
		Limit:     limit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	list := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		list = append(list, map[string]any{
			"workspace": row.WorkspaceID, "job": row.Job, "idx": row.Idx, "spec": row.Spec,
			"timezone": row.Timezone, "catch_up": row.CatchUp,
			"next_due_at": row.NextDueAt, "last_fired_at": row.LastFiredAt,
		})
	}
	resp := map[string]any{"schedules": list}
	if len(list) == limit {
		resp["truncated"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// errJobNotFound marks a failed job lookup.
var errJobNotFound = errors.New("job not found")

// ambiguousJobError lists candidate workspaces for a duplicate job name.
type ambiguousJobError struct{ workspaceIDs []string }

func (e *ambiguousJobError) Error() string {
	return "job name exists in workspaces [" + strings.Join(e.workspaceIDs, ", ") + "]; pass ?workspace="
}

// findJobRow resolves a job by (workspace, name). B4: zero matches means
// not found; duplicates without a workspace disambiguator are ambiguous.
func (s *Server) findJobRow(ctx context.Context, wsID, name string) (workspaces.JobRow, error) {
	return s.findIndexRow(ctx, wsID, "job", func(row workspaces.JobRow) bool {
		return row.Name == name
	})
}

// findAssistantRow resolves the workspace assistant row (TREBI.md).
func (s *Server) findAssistantRow(ctx context.Context, wsID string) (workspaces.JobRow, error) {
	wsID = s.workspaceID(ctx, wsID)
	rows, err := s.ws.ListJobs(ctx, wsID)
	if err != nil {
		return workspaces.JobRow{}, err
	}
	for _, row := range rows {
		if row.Kind == "assistant" {
			return row, nil
		}
	}
	return workspaces.JobRow{}, errJobNotFound
}

// findAgentRow resolves an agent by (workspace, name). Both the short name
// (cmo) and the index name (agents/cmo) match. Kind-strict: a kind=job
// launch can never pick up an agent row.
func (s *Server) findAgentRow(ctx context.Context, wsID, name string) (workspaces.JobRow, error) {
	return s.findIndexRow(ctx, wsID, "agent", func(row workspaces.JobRow) bool {
		return row.Name == name || strings.TrimPrefix(row.Name, "agents/") == name
	})
}

// findIndexRow is the kind-strict job_index lookup. Zero matches is not
// found; duplicates without a workspace disambiguator are ambiguous.
func (s *Server) findIndexRow(ctx context.Context, wsID, kind string, match func(workspaces.JobRow) bool) (workspaces.JobRow, error) {
	wsID = s.workspaceID(ctx, wsID)
	rows, err := s.ws.ListJobs(ctx, wsID)
	if err != nil {
		return workspaces.JobRow{}, err
	}
	var matches []workspaces.JobRow
	for _, row := range rows {
		if row.Kind != kind || !match(row) {
			continue
		}
		matches = append(matches, row)
	}
	if len(matches) == 0 {
		return workspaces.JobRow{}, errJobNotFound
	}
	if len(matches) > 1 && wsID == "" {
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.WorkspaceID
		}
		return workspaces.JobRow{}, &ambiguousJobError{workspaceIDs: ids}
	}
	return matches[0], nil
}

// resolveJob is findJobRow plus HTTP error mapping.
func (s *Server) resolveJob(w http.ResponseWriter, r *http.Request, name string) (workspaces.JobRow, bool) {
	row, err := s.findJobRow(r.Context(), r.URL.Query().Get("workspace"), name)
	if err != nil {
		writeResolveErr(w, err, "job")
		return workspaces.JobRow{}, false
	}
	return row, true
}

// jobFileLoc resolves the workspace root and relative path for a job row.
func (s *Server) jobFileLoc(w http.ResponseWriter, r *http.Request, row workspaces.JobRow) (*workspaces.Workspace, string, bool) {
	ws, err := s.ws.Get(r.Context(), row.WorkspaceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, "", false
	}
	rel, err := workspaceRel(ws.Path, row.Path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, "", false
	}
	return ws, rel, true
}

// writeResolveErr maps index resolution errors to HTTP codes. noun is the
// not-found message subject ("job", "agent").
func writeResolveErr(w http.ResponseWriter, err error, noun string) {
	var amb *ambiguousJobError
	switch {
	case errors.As(err, &amb):
		writeErr(w, http.StatusBadRequest, "ambiguous", amb.Error())
	case errors.Is(err, errJobNotFound):
		writeErr(w, http.StatusNotFound, "not_found", noun+" not found")
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// indexedDoc is the file IO of a JOB.md / AGENT.md GET: parse, bytes, full
// content hash, mtime. Callers own the JSON envelope.
type indexedDoc struct {
	Job   *job.Job
	Data  []byte
	Hash  string
	MTime string
}

func readIndexedDoc(path string) (indexedDoc, error) {
	j, err := job.ParseFile(path)
	if err != nil && j == nil {
		return indexedDoc{}, err
	}
	data, _ := os.ReadFile(path)
	var mtime string
	if st, statErr := os.Stat(path); statErr == nil {
		mtime = st.ModTime().UTC().Format(time.RFC3339Nano)
	}
	return indexedDoc{Job: j, Data: data, Hash: job.ContentHash(data), MTime: mtime}, nil
}

// patchIndexedMarkdown merge-patches frontmatter, rebuilds the index, and
// sets ETag. The caller writes the JSON envelope and any extra bus events.
func (s *Server) patchIndexedMarkdown(w http.ResponseWriter, r *http.Request, ws *workspaces.Workspace, path, rel string, body map[string]any) (hash string, ok bool) {
	if !s.requireIfMatch(w, r, ws.Path, rel) {
		return "", false
	}
	if err := job.MergePatchFrontmatter(path, body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return "", false
	}
	info, err := s.files.Stat(r.Context(), ws.Path, rel)
	if err != nil {
		writeFilesErr(w, err)
		return "", false
	}
	_ = s.ws.RebuildIndex(r.Context(), ws)
	setETagHash(w, info.Hash)
	return info.Hash, true
}

// purgeFolder removes dir, rebuilds the index, emits extra then file.deleted.
func (s *Server) purgeFolder(w http.ResponseWriter, r *http.Request, ws *workspaces.Workspace, dir string, extra ...store.BusEvent) (int, bool) {
	n, err := s.countFiles(r.Context(), dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return 0, false
	}
	if err := os.RemoveAll(dir); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return 0, false
	}
	_ = s.ws.RebuildIndex(r.Context(), ws)
	for _, ev := range extra {
		s.appendBus(r.Context(), ev)
	}
	folderRel, _ := workspaceRel(ws.Path, dir)
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "file.deleted", Entity: "file", EntityID: ws.ID + ":" + folderRel, Action: "deleted",
		Attrs: map[string]string{"workspace": ws.ID, "path": folderRel, "purge": "true"},
	})
	return n, true
}

// jobCreateRequest is the POST /v1/jobs body. It takes either full text
// (content or markdown) or the structured fields the server composes into
// front matter.
type jobCreateRequest struct {
	Workspace   string            `json:"workspace"`
	Name        string            `json:"name"`
	Content     string            `json:"content"`
	Markdown    string            `json:"markdown"`
	Idem        string            `json:"idempotency_key"`
	Prompt      string            `json:"prompt"`
	Harness     string            `json:"harness"`
	Description string            `json:"description"`
	Cron        string            `json:"cron"`
	Every       string            `json:"every"`
	On          *jobEventTrigger  `json:"on"`
	Session     job.SessionYAML   `json:"session"`
	Env         map[string]string `json:"env"`
	Agent       string            `json:"agent"` // short name; file lands under agents/<agent>/
}

// jobEventTrigger is the on: field of a structured job body.
type jobEventTrigger struct {
	Event   string `json:"event"`
	Match   string `json:"match"`
	Extract string `json:"extract"`
}

// markdown renders the JOB.md text of the request. A body with neither text nor
// a prompt gets the placeholder job, which keeps the old bare-create working.
func (b jobCreateRequest) markdown() (string, error) {
	spec := job.Spec{
		Content: cmp.Or(b.Markdown, b.Content), Prompt: b.Prompt, Harness: b.Harness,
		Description: b.Description, Cron: b.Cron, Every: b.Every,
		Session: b.Session, Env: b.Env,
	}
	if b.On != nil {
		spec.On = &job.EventSpec{Event: b.On.Event, Match: b.On.Match, Extract: b.On.Extract}
	}
	if spec.Content == "" && spec.Prompt == "" {
		spec.Prompt = "Do the task."
	}
	return spec.Render()
}

func (s *Server) handleJobsCreate(w http.ResponseWriter, r *http.Request) {
	var body jobCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	key := idempotencyKey(r, body.Idem)
	if key != "" {
		name, found, err := s.st.Idempotent(r.Context(), idempotencyJobs, key)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if found {
			if row, err := s.findJobRow(r.Context(), body.Workspace, name); err == nil {
				writeJSON(w, http.StatusCreated, map[string]any{
					"name": row.Name, "path": row.Path, "hash": row.Hash,
				})
				return
			}
		}
	}
	ws, err := s.ws.Resolve(r.Context(), body.Workspace)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "validation", "name required")
		return
	}
	md, err := body.markdown()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	// With agent set, the file lands under the agent folder and the wire
	// name is the full relative path (agents/<agent>/<name>).
	wireName := body.Name
	rel := filepath.ToSlash(filepath.Join(body.Name, "JOB.md"))
	if body.Agent != "" {
		agentRow, err := s.findAgentRow(r.Context(), ws.ID, body.Agent)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "agent not found")
			return
		}
		wireName = agentRow.Name + "/" + body.Name
		rel = filepath.ToSlash(filepath.Join(agentRow.Name, body.Name, "JOB.md"))
	}
	if _, err := s.files.Stat(r.Context(), ws.Path, rel); err == nil {
		writeErr(w, http.StatusConflict, "conflict", "job exists")
		return
	} else if err != nil && !errors.Is(err, files.ErrNotFound) {
		writeFilesErr(w, err)
		return
	}
	info, _, err := s.files.Write(r.Context(), ws.Path, rel, []byte(md), "")
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	path := filepath.Join(ws.Path, filepath.FromSlash(rel))
	if spec, perr := job.ParseBytes(path, []byte(md)); perr == nil && spec != nil && spec.Memory {
		s.seedMemory(r.Context(), ws, strings.TrimSuffix(rel, "JOB.md")+"MEMORY.md")
	}
	_ = s.ws.RebuildIndex(r.Context(), ws)
	if key != "" {
		if _, err := s.st.RememberIdempotent(r.Context(), idempotencyJobs, key, wireName); err != nil {
			s.log.Warn("remember idempotency key", "job", wireName, "error", err)
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"name": wireName, "path": path, "hash": info.Hash,
	})
}

func (s *Server) handleJobPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content  string `json:"content"`
		Markdown string `json:"markdown"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	md := body.Markdown
	if md == "" {
		md = body.Content
	}
	if md == "" {
		writeErr(w, http.StatusBadRequest, "validation", "markdown (or content) required; refusing empty full-replace")
		return
	}
	name := r.PathValue("name")
	row, ok := s.resolveJob(w, r, name)
	if !ok {
		return
	}
	ws, rel, ok := s.jobFileLoc(w, r, row)
	if !ok {
		return
	}
	info, _, err := s.files.Write(r.Context(), ws.Path, rel, []byte(md), ifMatchHeader(r))
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	// Frontmatter drives job_index; a full replace can change harness or active.
	_ = s.ws.RebuildIndex(r.Context(), ws)
	setETagHash(w, info.Hash)
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "job.updated", Entity: "job", EntityID: name, Action: "updated",
		Attrs: map[string]string{"workspace": row.WorkspaceID, "path": row.Path},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "path": row.Path, "hash": info.Hash,
	})
}

func (s *Server) handleJobPatch(w http.ResponseWriter, r *http.Request) {
	// D1 JSON-merge-patch of frontmatter scalar fields.
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	name := r.PathValue("name")
	row, ok := s.resolveJob(w, r, name)
	if !ok {
		return
	}
	ws, rel, ok := s.jobFileLoc(w, r, row)
	if !ok {
		return
	}
	hash, ok := s.patchIndexedMarkdown(w, r, ws, row.Path, rel, body)
	if !ok {
		return
	}
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "job.updated", Entity: "job", EntityID: name, Action: "updated",
		Attrs: map[string]string{"workspace": row.WorkspaceID, "path": row.Path},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "patched": true, "hash": hash,
	})
}

func (s *Server) handleJobDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	row, ok := s.resolveJob(w, r, name)
	if !ok {
		return
	}
	ws, rel, ok := s.jobFileLoc(w, r, row)
	if !ok {
		return
	}
	if r.URL.Query().Get("purge") == "1" {
		s.purgeJob(w, r, name, row, ws, rel)
		return
	}
	jobDir := filepath.Dir(row.Path)
	remaining := 0
	if n, err := s.countFiles(r.Context(), jobDir); err == nil && n > 0 {
		remaining = n - 1
	}
	if err := s.files.Delete(r.Context(), ws.Path, rel, ifMatchHeader(r)); err != nil {
		writeFilesErr(w, err)
		return
	}
	_ = s.ws.RebuildIndex(r.Context(), ws)
	out := map[string]any{"deleted": name}
	if remaining > 0 {
		out["files_remaining"] = remaining
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) purgeJob(w http.ResponseWriter, r *http.Request, name string, row workspaces.JobRow, ws *workspaces.Workspace, rel string) {
	if !s.requireIfMatch(w, r, ws.Path, rel) {
		return
	}
	jobDir := filepath.Dir(row.Path)
	if filepath.Clean(jobDir) == filepath.Clean(ws.Path) {
		writeErr(w, http.StatusConflict, "conflict", "cannot purge a root-level job; it would delete the workspace")
		return
	}
	if msg := s.purgeBlockers(r.Context(), row, jobDir); msg != "" {
		writeErr(w, http.StatusConflict, "conflict", msg)
		return
	}
	n, ok := s.purgeFolder(w, r, ws, jobDir, store.BusEvent{
		Topic: "job.deleted", Entity: "job", EntityID: name, Action: "deleted",
		Attrs: map[string]string{"workspace": row.WorkspaceID, "path": row.Path},
	})
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": name, "purged": true, "files_removed": n,
	})
}

func (s *Server) purgeBlockers(ctx context.Context, row workspaces.JobRow, jobDir string) string {
	rows, err := s.ws.ListJobs(ctx, row.WorkspaceID)
	if err != nil {
		return ""
	}
	var names []string
	for _, other := range rows {
		if other.Name == row.Name {
			continue
		}
		if workspaces.PathInside(jobDir, other.Path) {
			names = append(names, other.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "cannot purge: nested jobs " + strings.Join(names, ", ")
}

func (s *Server) countFiles(ctx context.Context, root string) (int, error) {
	n := 0
	after := ""
	for {
		items, next, err := s.files.List(ctx, root, files.ListOpts{AfterPath: after, Limit: 500})
		if err != nil {
			return 0, err
		}
		n += len(items)
		if next == "" {
			return n, nil
		}
		after = next
	}
}
