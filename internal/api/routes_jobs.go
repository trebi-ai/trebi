package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/pricing"
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
	mux.HandleFunc("POST /v1/jobs/{name}/rename", s.handleJobRename)
	mux.HandleFunc("POST /v1/jobs/{name}/move", s.handleJobMove)
	mux.HandleFunc("POST /v1/jobs/validate", s.handleJobValidateText)
	mux.HandleFunc("POST /v1/jobs/{name}/validate", s.handleJobValidate)
	mux.HandleFunc("GET /v1/schedules", s.handleSchedules)

	// Owned-job paths have an agents/<id>/<job> wire name. Go's single-segment
	// job wildcard cannot match embedded slashes and a more specific wildcard
	// would conflict with the job-files wildcard, so the nested subtree gets
	// one dispatcher that presents the same resolved name to existing handlers.
	mux.HandleFunc("/v1/jobs/", s.handleJobsSubtree)
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
		owner:   ownerFilter(strings.TrimSpace(q.Get("owner"))),
	}
	if !filter.validActive() {
		writeErr(w, http.StatusBadRequest, "validation", "active must be true or false")
		return
	}
	kits, _ := s.agentKits(r.Context(), wsID)
	enrich := s.jobEnrichment(r.Context(), wsID)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// B4: the assistant is a separate singleton; jobs list is jobs only.
		if row.Kind != "job" || !filter.keep(row) {
			continue
		}
		key := row.ID
		waiting := false
		for _, id := range live[key] {
			if s.runs.SessionState(id) == runs.SessionWaiting {
				waiting = true
				break
			}
		}
		status := runs.DeriveJobDisplayStatus(row.Active, len(live[key]), waiting, last[key])
		item := map[string]any{
			"id": row.ID, "workspace_id": row.WorkspaceID, "name": row.Name, "path": row.Path,
			"kind": row.Kind, "active": row.Active, "harness": row.Harness,
			"owner_id": row.OwnerID, "owner_name": row.OwnerName, "folder": row.Folder,
			"hash": row.Hash, "parse_error": row.ParseError, "status": status,
			"last_run_at": "", "last_status": "", "next_due_at": "", "mtd_tokens": pricing.TokenTotals{},
			"triggers": []string{},
		}
		enrich(key, row.Path, item)
		item["kit_source"] = s.kitSourceFrom(kits, row)
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// jobFilter holds the A6.5 list filters of GET /v1/jobs.
type jobFilter struct {
	q       string
	harness string
	active  string
	owner   string // agent ID or index name (agents/<short>)
}

// ownerFilter keeps an agent ID and makes a name an index name.
func ownerFilter(ref string) string {
	if job.LooksLikeID(ref) {
		return ref
	}
	return request.AgentIndexName(ref)
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
	if f.owner != "" && row.OwnerID != f.owner && row.OwnerIndex() != f.owner {
		return false
	}
	return true
}

// jobEnrichment returns a function that fills the per-job A6.5 columns:
// last run, next due time, month-to-date tokens, and trigger kinds. It reads the
// three tables once, not once per row.
func (s *Server) jobEnrichment(ctx context.Context, wsID string) func(key, path string, item map[string]any) {
	last, _, _ := s.runs.StatusByJob(ctx)
	due := map[string]string{}
	if rows, err := s.sched.List(ctx, schedule.ListOpts{Workspace: wsID, Limit: maxListLimit}); err == nil {
		for _, row := range rows {
			k := row.JobID
			if due[k] == "" || (row.NextDueAt != "" && row.NextDueAt < due[k]) {
				due[k] = row.NextDueAt
			}
		}
	}
	mtd := map[string]pricing.TokenTotals{}
	recent, _ := s.runs.List(ctx, runs.ListOpts{Workspace: wsID, Limit: maxListLimit})
	lastAt := map[string]string{}
	monthStart := time.Now().UTC().Format("2006-01")
	for _, run := range recent {
		k := run.JobID
		if lastAt[k] == "" {
			lastAt[k] = run.StartedAt.UTC().Format(time.RFC3339)
		}
		if run.StartedAt.UTC().Format("2006-01") == monthStart {
			t := mtd[k]
			t.Add(run.Tokens)
			mtd[k] = t
		}
	}
	return func(key, path string, item map[string]any) {
		item["last_run_at"] = lastAt[key]
		if st, ok := last[key]; ok {
			item["last_status"] = string(st)
		}
		item["next_due_at"] = due[key]
		item["mtd_tokens"] = mtd[key]
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
	parseErr := j.ParseError
	if err := j.CheckFolderOwner(row.OwnerID); err != nil && parseErr == "" {
		parseErr = err.Error()
	}
	kit := s.ws.OwnerKit(r.Context(), row.WorkspaceID, row.Name)
	workingDir, workingDirSource := j.ResolvedWorkingDir(kit)
	// Index name, not ParseFile's folder basename. Runs, list, and pause
	// all key off row.Name (agents/watchtower/job, not job).
	writeJSON(w, http.StatusOK, map[string]any{
		"id": row.ID, "name": row.Name, "workspace": row.WorkspaceID, "path": row.Path,
		"owner_id": row.OwnerID, "owner_name": row.OwnerName, "folder": j.Folder,
		"harness": j.Harness, "model": j.Model, "effort": j.Effort,
		"active": !j.Disabled, "hash": doc.Hash,
		"parse_error": parseErr, "prompt": j.Prompt,
		"input_keys": job.DiscoverInputKeys(j.Prompt),
		// Transitional (plan H.3): env_sets is the list form; env_set carries
		// the first name for one release so the pinned UI keeps working.
		"env_sets": j.Envset.List(), "env_set": j.Envset.First(),
		"triggers": j.Triggers, "session": j.Session,
		"kit_source":  s.kitSource(r.Context(), row),
		"working_dir": workingDir, "working_dir_source": workingDirSource,
		"env_file": j.EnvFile, "folder_env": j.FolderEnvSummary(kit),
		"markdown": string(doc.Data), "mtime": doc.MTime,
	})
}

// agentKits parses all agent kit definitions once for a workspace request.
func (s *Server) agentKits(ctx context.Context, wsID string) (map[string]*job.KitYAML, error) {
	rows, err := s.ws.ListJobs(ctx, wsID)
	if err != nil {
		return map[string]*job.KitYAML{}, err
	}
	out := make(map[string]*job.KitYAML, len(rows))
	for _, row := range rows {
		if row.Kind != "agent" {
			continue
		}
		if parsed, err := job.ParseFile(row.Path); err == nil && parsed != nil {
			out[row.Name] = parsed.Kit
		}
	}
	return out, nil
}

// kitSource marks kit values inherited from an owner agent. A job-level
// envset, env, skills, or connectors entry wins and is labeled "job".
func (s *Server) kitSource(ctx context.Context, row workspaces.JobRow) map[string]string {
	kits, _ := s.agentKits(ctx, row.WorkspaceID)
	return s.kitSourceFrom(kits, row)
}

func (s *Server) kitSourceFrom(kits map[string]*job.KitYAML, row workspaces.JobRow) map[string]string {
	out := map[string]string{}
	if row.OwnerIndex() == "" {
		return out
	}
	kit := kits[row.OwnerIndex()]
	if kit == nil {
		return out
	}
	if len(kit.Envset) > 0 {
		out["envset"] = "agent"
	}
	if len(kit.Env) > 0 {
		out["env"] = "agent"
	}
	if len(kit.Skills) > 0 {
		out["skills"] = "agent"
	}
	if len(kit.Connectors) > 0 {
		out["connectors"] = "agent"
	}
	if row.Name != "" {
		if parsed, err := job.ParseFile(row.Path); err == nil && parsed != nil {
			if len(parsed.Envset) > 0 {
				out["envset"] = "job"
			}
			if len(parsed.Env) > 0 {
				out["env"] = "job"
			}
			if len(parsed.Skills) > 0 {
				out["skills"] = "job"
			}
			if len(parsed.Connectors) > 0 {
				out["connectors"] = "job"
			}
		}
	}
	return out
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
	if !s.requireAgentJobScope(w, r, row) {
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

type jobRenameRequest struct {
	To string `json:"to"`
}

// handleJobRename changes the last segment of a job name. The folder moves;
// the ID, owner, schedules, and earlier runs stay.
func (s *Server) handleJobRename(w http.ResponseWriter, r *http.Request) {
	row, ok := s.resolveJob(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	if !s.requireAgentJobScope(w, r, row) {
		return
	}
	var body jobRenameRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	live, err := s.runs.HasLiveJobs(r.Context(), row.WorkspaceID, row.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if live {
		writeErr(w, http.StatusConflict, "conflict", "Stop the running run first")
		return
	}
	res, moved, err := s.ws.RenameJob(r.Context(), row.WorkspaceID, row.Name, body.To)
	if err != nil {
		writeRenameErr(w, err, "job")
		return
	}
	s.recomputeMoved(r.Context(), row.WorkspaceID, moved)
	writeJSON(w, http.StatusOK, res)
}

// handleJobMove gives a job another owner agent, another folder, or both.
// The job keeps its ID, schedules, state, memory, and runs. A dry run
// returns the notes about an owner change and writes nothing.
func (s *Server) handleJobMove(w http.ResponseWriter, r *http.Request) {
	row, ok := s.resolveJob(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	if !s.requireAgentJobScope(w, r, row) {
		return
	}
	var body workspaces.JobMove
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	// An agent can change the folder of its own job but cannot give it away.
	if body.Agent != nil && strings.TrimPrefix(*body.Agent, "agents/") != row.OwnerName && rejectAnyAgentScope(w, r) {
		return
	}
	if !body.DryRun {
		live, err := s.runs.HasLiveJobs(r.Context(), row.WorkspaceID, row.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if live {
			writeErr(w, http.StatusConflict, "conflict", "Stop the running run first")
			return
		}
	}
	if body.Folder != nil {
		if _, err := job.NormalizeFolder(*body.Folder); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
	}
	res, moved, err := s.ws.MoveJob(r.Context(), row.WorkspaceID, row.Name, body)
	if err != nil {
		writeMoveErr(w, err)
		return
	}
	if res.Moved {
		if ws, err := s.ws.Get(r.Context(), row.WorkspaceID); err == nil {
			_ = s.ws.RebuildIndex(r.Context(), ws) // the move already wrote the index; this picks up a folder-only change
		}
		s.recomputeMoved(r.Context(), row.WorkspaceID, moved)
	}
	writeJSON(w, http.StatusOK, res)
}

// writeMoveErr maps the move errors of a job to HTTP.
func writeMoveErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workspaces.ErrNoAgent):
		writeErr(w, http.StatusNotFound, "not_found", "agent not found")
	case errors.Is(err, job.ErrFolderNoOwner):
		writeErr(w, http.StatusBadRequest, "validation", "Only a job of an agent can be in a folder")
	case errors.Is(err, workspaces.ErrRootJob):
		writeErr(w, http.StatusBadRequest, "validation", "A job at the workspace root cannot move")
	default:
		writeRenameErr(w, err, "job")
	}
}

// writeRenameErr maps the rename errors of a job or agent to HTTP.
func writeRenameErr(w http.ResponseWriter, err error, noun string) {
	switch {
	case errors.Is(err, workspaces.ErrInvalidName):
		writeErr(w, http.StatusBadRequest, "validation", "A job name uses lowercase letters, digits, and hyphens")
	case errors.Is(err, workspaces.ErrRootJob):
		writeErr(w, http.StatusBadRequest, "validation", "A job at the workspace root has the workspace name. Rename the workspace.")
	case errors.Is(err, workspaces.ErrNotJob):
		writeErr(w, http.StatusBadRequest, "validation", "This is an agent. Use the agent rename.")
	case errors.Is(err, workspaces.ErrExists):
		writeErr(w, http.StatusConflict, "conflict", "A "+noun+" with this name exists")
	case errors.Is(err, workspaces.ErrLiveRun):
		writeErr(w, http.StatusConflict, "conflict", "Stop the running run first")
	case errors.Is(err, workspaces.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", noun+" not found")
	default:
		writeResolveErr(w, err, noun)
	}
}

// recomputeMoved refreshes the schedules of moved jobs. The rows are keyed by
// job ID, so an unchanged trigger keeps its next due time.
func (s *Server) recomputeMoved(ctx context.Context, wsID string, moved []*job.Job) {
	for _, j := range moved {
		if j == nil || j.ParseError != "" {
			continue
		}
		if err := s.sched.RecomputeFromJob(ctx, wsID, j, time.Time{}); err != nil {
			s.log.Warn("schedule recompute after rename failed", "workspace", wsID, "job", j.Name, "error", err)
		}
	}
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
	if valid {
		if off := harnessOffError(j.Harness); off != "" {
			valid, msg = false, off
		}
	}
	out := map[string]any{"ok": valid, "error": msg}
	if valid {
		if warn := j.Warnings(); len(warn) > 0 {
			out["warnings"] = warn
		}
	}
	writeJSON(w, http.StatusOK, out)
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
	file := "JOB.md"
	switch body.Kind {
	case "", "job":
	case "agent":
		file = "AGENT.md"
	default:
		writeErr(w, http.StatusBadRequest, "validation", `kind must be "job" or "agent"`)
		return
	}
	name := cmp.Or(body.Name, "job")
	if body.Agent != "" {
		name = request.AgentIndexName(body.Agent) + "/" + name
	}
	j, err := job.ParseBytes(filepath.Join(name, file), []byte(md))
	if err == nil && j != nil && j.ParseError == "" && body.Kind != "agent" {
		err = j.CheckFolderOwner(job.OwnerOf(name))
	}
	switch {
	case err != nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "markdown": md})
	case j != nil && j.ParseError != "":
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": j.ParseError, "markdown": md})
	case j != nil && harnessOffError(j.Harness) != "":
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": harnessOffError(j.Harness), "markdown": md})
	default:
		out := map[string]any{"ok": true, "error": "", "markdown": md}
		if warn := j.Warnings(); len(warn) > 0 {
			out["warnings"] = warn
		}
		writeJSON(w, http.StatusOK, out)
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
			"workspace": row.WorkspaceID, "job_id": row.JobID, "job_name": row.JobName, "idx": row.Idx, "spec": row.Spec,
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

// jobNotFoundError is a failed lookup with the index names that end in the
// asked name. An owned job has the name agents/<owner>/<job>, and a run often
// asks for <job> or <owner>/<job> only.
type jobNotFoundError struct{ similar []string }

func (e *jobNotFoundError) Error() string {
	return "not found; use the full name: " + strings.Join(e.similar, ", ")
}

func (e *jobNotFoundError) Is(target error) bool { return target == errJobNotFound }

// ambiguousJobError lists candidate workspaces for a duplicate job name.
type ambiguousJobError struct{ workspaceIDs []string }

func (e *ambiguousJobError) Error() string {
	return "job name exists in workspaces [" + strings.Join(e.workspaceIDs, ", ") + "]; pass ?workspace="
}

// findJobRow resolves a job by (workspace, name). B4: zero matches means
// not found; duplicates without a workspace disambiguator are ambiguous.
func (s *Server) findJobRow(ctx context.Context, wsID, name string) (workspaces.JobRow, error) {
	return s.findIndexRow(ctx, wsID, name, func(row workspaces.JobRow) bool {
		return row.Name == name
	}, "job")
}

// findFileRow resolves the folder a file request jails to: a job by index
// name, or an agent by index name (agents/cmo). Both kinds own a folder, so
// the agent Files tab browses agents/<name>/ through the job file routes.
// Names stay exact -- a folder cannot hold both AGENT.md and JOB.md, so an
// index name resolves at most one row -- and this resolver is used only for
// file jails, never for launches.
func (s *Server) findFileRow(ctx context.Context, wsID, name string) (workspaces.JobRow, error) {
	return s.findIndexRow(ctx, wsID, name, func(row workspaces.JobRow) bool {
		return row.Name == name
	}, "job", "agent")
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
	return s.findIndexRow(ctx, wsID, name, func(row workspaces.JobRow) bool {
		return row.Name == name || strings.TrimPrefix(row.Name, "agents/") == name
	}, "agent")
}

// findIndexRow is the kind-scoped job_index lookup: only rows whose kind is
// in kinds match. A ref that is the ID of a row always matches. Zero matches is not found; duplicates without a workspace
// disambiguator are ambiguous.
func (s *Server) findIndexRow(ctx context.Context, wsID, ref string, match func(workspaces.JobRow) bool, kinds ...string) (workspaces.JobRow, error) {
	wsID = s.workspaceID(ctx, wsID)
	rows, err := s.ws.ListJobs(ctx, wsID)
	if err != nil {
		return workspaces.JobRow{}, err
	}
	var matches []workspaces.JobRow
	var similar []string
	for _, row := range rows {
		if !slices.Contains(kinds, row.Kind) {
			continue
		}
		if row.ID != ref && !match(row) {
			if ref != "" && strings.HasSuffix(row.Name, "/"+ref) {
				similar = append(similar, row.Name)
			}
			continue
		}
		matches = append(matches, row)
	}
	if len(matches) == 0 && len(similar) > 0 {
		return workspaces.JobRow{}, &jobNotFoundError{similar: similar}
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

// resolveFileRow is findFileRow plus HTTP error mapping. The file routes are
// job-flavored but jail to a folder, so an agent's index name resolves to
// the agent folder; the not-found noun stays "job".
func (s *Server) resolveFileRow(w http.ResponseWriter, r *http.Request, name string) (workspaces.JobRow, bool) {
	row, err := s.findFileRow(r.Context(), r.URL.Query().Get("workspace"), name)
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
	var nf *jobNotFoundError
	switch {
	case errors.As(err, &amb):
		writeErr(w, http.StatusBadRequest, "ambiguous", amb.Error())
	case errors.As(err, &nf):
		writeErrDetails(w, http.StatusNotFound, "not_found", noun+" "+nf.Error(), map[string]any{"similar": nf.similar})
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

// purgeFolder removes dir, rebuilds the index, and emits file.deleted. The
// index diff emits the job and agent events.
func (s *Server) purgeFolder(w http.ResponseWriter, r *http.Request, ws *workspaces.Workspace, dir string) (int, bool) {
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
	Model       string            `json:"model"`
	Effort      string            `json:"effort"`
	Description string            `json:"description"`
	Folder      string            `json:"folder"`
	Cron        string            `json:"cron"`
	Every       string            `json:"every"`
	On          *jobEventTrigger  `json:"on"`
	Session     job.SessionYAML   `json:"session"`
	Env         map[string]string `json:"env"`
	Agent       string            `json:"agent"` // short name; file lands under agents/<agent>/
	Kind        string            `json:"kind"`  // validate only: job (default) or agent
}

// jobEventTrigger is the on: field of a structured job body.
type jobEventTrigger struct {
	Event     string   `json:"event"`
	Match     string   `json:"match"`
	Extract   string   `json:"extract"`
	Rooms     []string `json:"rooms"`
	Allowlist []string `json:"allowlist"`
	Contains  []string `json:"contains"`
	From      string   `json:"from"`
	Replies   string   `json:"replies"`
	Batch     string   `json:"batch"`
}

// spec maps the body trigger to the writer form; nil stays nil.
func (t *jobEventTrigger) spec() *job.EventSpec {
	if t == nil {
		return nil
	}
	return &job.EventSpec{
		Event: t.Event, Match: t.Match, Extract: t.Extract, Rooms: t.Rooms, Allowlist: t.Allowlist,
		Contains: t.Contains, From: t.From, Replies: t.Replies, Batch: t.Batch,
	}
}

// markdown renders the JOB.md text of the request. A body with neither text nor
// a prompt gets the placeholder job, which keeps the old bare-create working.
func (b jobCreateRequest) markdown() (string, error) {
	spec := job.Spec{
		Content: cmp.Or(b.Markdown, b.Content), Prompt: b.Prompt, Harness: b.Harness,
		Model: b.Model, Effort: b.Effort,
		Description: b.Description, Folder: b.Folder, Cron: b.Cron, Every: b.Every,
		Session: b.Session, Env: b.Env,
	}
	spec.On = b.On.spec()
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
	if scope, agent := scopeFrom(r.Context()); agent {
		if scope.workspaceID != ws.ID {
			writeForbidden(w, "agent may create only in its own workspace")
			return
		}
		short := strings.TrimPrefix(scope.agent, "agents/")
		if body.Agent != "" && request.AgentIndexName(body.Agent) != scope.agent {
			writeForbidden(w, "agent may create only jobs owned by "+scope.agent)
			return
		}
		body.Agent = short
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
	if parsed, perr := job.ParseBytes(rel, []byte(md)); perr == nil && parsed != nil {
		if err := parsed.CheckFolderOwner(job.OwnerOf(wireName)); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
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
	if !s.requireAgentJobScope(w, r, row) {
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
	if !s.requireAgentJobScope(w, r, row) {
		return
	}
	ws, rel, ok := s.jobFileLoc(w, r, row)
	if !ok {
		return
	}
	if f, _ := body["folder"].(string); strings.TrimSpace(f) != "" && row.OwnerID == "" {
		writeErr(w, http.StatusBadRequest, "validation", job.ErrFolderNoOwner.Error())
		return
	}
	hash, ok := s.patchIndexedMarkdown(w, r, ws, row.Path, rel, body)
	if !ok {
		return
	}
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
	if !s.requireAgentJobScope(w, r, row) {
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
	n, ok := s.purgeFolder(w, r, ws, jobDir)
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

// handleOwnedJobs dispatches the nested agents/<id>/<job> wire shape. The
// helper restores the generic handler's `name` and, for files, `path` values.
func (s *Server) handleJobsSubtree(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 1 && parts[0] != "" && parts[0] != "agents" {
		r.SetPathValue("name", parts[0])
		switch r.Method {
		case http.MethodGet:
			s.handleJobGet(w, r)
		case http.MethodPut:
			s.handleJobPut(w, r)
		case http.MethodPatch:
			s.handleJobPatch(w, r)
		case http.MethodDelete:
			s.handleJobDelete(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}
	if len(parts) < 3 || parts[0] != "agents" || parts[1] == "" || parts[2] == "" {
		writeErr(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	owner, child := parts[1], parts[2]
	if strings.Contains(owner, "..") || strings.Contains(child, "..") {
		writeErr(w, http.StatusBadRequest, "validation", "invalid owned-job path")
		return
	}
	r.SetPathValue("name", "agents/"+owner+"/"+child)
	tail := "/" + strings.Join(parts[3:], "/")
	if len(parts) == 3 {
		tail = "/"
	}
	if len(parts) == 5 && parts[3] == "files" && parts[4] == "move" && r.Method == http.MethodPost {
		s.inJobFiles(s.filesMove)(w, r)
		return
	}
	if len(parts) > 4 && parts[3] == "folders" {
		r.SetPathValue("path", strings.Join(parts[4:], "/"))
		switch r.Method {
		case http.MethodPost:
			s.inJobFiles(s.foldersCreate)(w, r)
		case http.MethodDelete:
			s.inJobFiles(s.foldersDelete)(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}
	if len(parts) >= 4 && parts[3] == "files" {
		if len(parts) == 4 && r.Method == http.MethodGet {
			r.SetPathValue("path", "")
			s.handleJobFilesList(w, r)
			return
		}
		if len(parts) > 4 {
			r.SetPathValue("path", strings.Join(parts[4:], "/"))
			switch r.Method {
			case http.MethodGet:
				s.handleJobFilesGet(w, r)
			case http.MethodPut:
				s.handleJobFilesPut(w, r)
			case http.MethodDelete:
				s.handleJobFilesDelete(w, r)
			default:
				writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
			}
			return
		}
	}
	switch r.Method + " " + tail {
	case "GET /":
		s.handleJobGet(w, r)
	case "PUT /":
		s.handleJobPut(w, r)
	case "PATCH /":
		s.handleJobPatch(w, r)
	case "DELETE /":
		s.handleJobDelete(w, r)
	case "POST /run", "POST /pause", "POST /unpause", "POST /validate":
		s.handleJobRunForTail(w, r, tail)
	case "POST /rename":
		s.handleJobRename(w, r)
	case "POST /move":
		s.handleJobMove(w, r)
	case "GET /memory":
		s.handleMemoryGet(w, r, memoryJob)
	case "PUT /memory":
		s.handleMemoryPut(w, r, memoryJob)
	case "DELETE /memory":
		s.handleMemoryReset(w, r, memoryJob)
	case "POST /memory/validate":
		s.handleMemoryValidate(w, r, memoryJob)
	case "POST /memory/migrate":
		s.handleMemoryMigrate(w, r, memoryJob)
	case "PUT /memory/context":
		s.handleMemorySectionPut(w, r, memoryJob, "context")
	case "POST /memory/lessons":
		s.handleMemoryEntryPost(w, r, memoryJob, "lessons")
	case "POST /memory/log":
		s.handleMemoryEntryPost(w, r, memoryJob, "log")
	default:
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(tail, "/memory/lessons/"):
			r.SetPathValue("eid", parts[len(parts)-1])
			s.handleMemoryEntryDelete(w, r, memoryJob, "lessons")
		case r.Method == http.MethodDelete && strings.HasPrefix(tail, "/memory/log/"):
			r.SetPathValue("eid", parts[len(parts)-1])
			s.handleMemoryEntryDelete(w, r, memoryJob, "log")
		default:
			writeErr(w, http.StatusNotFound, "not_found", "job route not found")
		}
	}
}

func (s *Server) handleJobRunForTail(w http.ResponseWriter, r *http.Request, tail string) {
	switch tail {
	case "/run":
		s.handleJobRun(w, r)
	case "/pause":
		s.handleJobPause(w, r)
	case "/unpause":
		s.handleJobUnpause(w, r)
	case "/validate":
		s.handleJobValidate(w, r)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "job route not found")
	}
}
