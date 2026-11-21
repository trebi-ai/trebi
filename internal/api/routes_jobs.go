package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/pauses"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/skills"
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
	mux.HandleFunc("POST /v1/jobs/{name}/archive", s.handleJobArchive)
	mux.HandleFunc("POST /v1/jobs/{name}/unarchive", s.handleJobUnarchive)
	mux.HandleFunc("POST /v1/jobs/{name}/rename", s.handleJobRename)
	mux.HandleFunc("POST /v1/jobs/{name}/move", s.handleJobMove)
	mux.HandleFunc("POST /v1/jobs/validate", s.handleJobValidateText)
	mux.HandleFunc("POST /v1/jobs/simulate", s.handleJobSimulate)
	mux.HandleFunc("POST /v1/jobs/{name}/validate", s.handleJobValidate)
	mux.HandleFunc("GET /v1/schedules", s.handleSchedules)

	// An owned job has the wire name x/y. Go's single-segment job wildcard
	// cannot match a raw slash and a more specific wildcard would conflict
	// with the job-files wildcard, so the subtree gets one dispatcher that
	// presents the same resolved name to existing handlers.
	mux.HandleFunc("/v1/jobs/", s.handleJobsSubtree)
}

// jobRecentRuns is the number of bars in the sparkline of a job.
const jobRecentRuns = 14

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
		owner:   strings.TrimSpace(q.Get("owner")),
	}
	if !filter.validActive() {
		writeErr(w, http.StatusBadRequest, "validation", "active must be true or false")
		return
	}
	view, ok := parseArchivedView(q.Get("archived"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "validation", errArchivedValue)
		return
	}
	kits := agentKitsFrom(rows)
	fallback := job.DefaultHarness()
	enrich := s.jobEnrichment(r.Context(), wsID)
	active := s.activePauses(r.Context())
	out := make([]map[string]any, 0, len(rows))
	var counts archiveCounts
	for _, row := range rows {
		// B4: the assistant is a separate singleton; jobs list is jobs only.
		if row.Kind != "job" || !filter.keep(row) {
			continue
		}
		harness, harnessSource := job.HarnessFrom(row.Harness, kits[row.OwnerName], fallback)
		if filter.harness != "" && !strings.EqualFold(harness, filter.harness) {
			continue
		}
		counts.add(row.Archived())
		if !view.keep(row.Archived()) {
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
		status := runs.DeriveJobDisplayStatus(row.Archived(), row.Active, len(live[key]), waiting, last[key])
		item := map[string]any{
			"id": row.ID, "workspace_id": row.WorkspaceID, "name": row.Name, "path": row.Path,
			"kind": row.Kind, "active": row.Active, "harness": harness, "harness_source": harnessSource,
			"owner_id": row.OwnerID, "owner_name": row.OwnerName, "folder": row.Folder,
			"hash": row.Hash, "parse_error": row.ParseError, "status": status,
			"last_run_at": "", "last_status": "", "next_due_at": "", "tokens_30d": pricing.TokenTotals{}, "recent_runs": []runs.RecentRun{},
			"triggers": []string{}, "run_count": 0,
			"archived_at": row.EffectiveArchivedAt(), "archived_with_agent": row.ArchivedAt == "" && row.OwnerArchivedAt != "",
		}
		if row.Active && !row.Archived() {
			if p := active.Cover(pauses.Subject{WorkspaceID: row.WorkspaceID, AgentID: row.OwnerID, Harness: harness, Kind: dispatch.KindJob}); p != nil {
				item["held_by"] = p.Hold()
				if len(live[key]) == 0 {
					item["status"] = runs.JobDisplayHeld
				}
			}
		}
		var parsed *job.Job
		if j, err := job.ParseFile(row.Path); err == nil {
			parsed = j
		}
		enrich(key, parsed, item)
		item["kit_source"] = kitSourceFrom(kits[row.OwnerName], parsed)
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out, "counts": counts})
}

// jobFilter holds the A6.5 list filters of GET /v1/jobs. The handler
// matches harness on the resolved value.
type jobFilter struct {
	q       string
	harness string
	active  string
	owner   string // agent ID or agent name
}

func (f jobFilter) validActive() bool {
	return f.active == "" || f.active == "true" || f.active == "false"
}

func (f jobFilter) keep(row workspaces.JobRow) bool {
	if f.q != "" && !strings.Contains(strings.ToLower(row.Name), f.q) {
		return false
	}
	if f.active != "" && row.Active != (f.active == "true") {
		return false
	}
	if f.owner != "" && row.OwnerID != f.owner && row.OwnerName != f.owner {
		return false
	}
	return true
}

// jobEnrichment returns a function that fills the per-job A6.5 columns:
// last run, the newest runs, next due time, tokens of the last 30 days, and
// trigger kinds. It reads each table once, not once per row.
func (s *Server) jobEnrichment(ctx context.Context, wsID string) func(key string, j *job.Job, item map[string]any) {
	due := map[string]string{}
	if rows, err := s.sched.List(ctx, schedule.ListOpts{Workspace: wsID, Limit: maxListLimit}); err == nil {
		for _, row := range rows {
			k := row.JobID
			if due[k] == "" || (row.NextDueAt != "" && row.NextDueAt < due[k]) {
				due[k] = row.NextDueAt
			}
		}
	}
	act, err := s.runs.JobActivity(ctx, wsID, jobRecentRuns, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		s.log.Warn("job activity", "error", err)
	}
	return func(key string, j *job.Job, item map[string]any) {
		if ja, ok := act[key]; ok {
			if ja.LastRunAt != nil {
				item["last_run_at"] = ja.LastRunAt.UTC().Format(time.RFC3339)
			}
			item["last_status"] = string(ja.LastStatus)
			item["recent_runs"] = ja.Recent
			item["tokens_30d"] = ja.Tokens30d
			item["run_count"] = ja.RunCount
		}
		if item["archived_at"] == "" {
			item["next_due_at"] = due[key]
		}
		if j != nil {
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
	if job.OwnerOf(row.Name) == "" {
		parseErr = job.JoinParseError(parseErr, job.ErrNoOwner.Error())
	}
	kit := s.ws.OwnerKit(r.Context(), row.WorkspaceID, row.Name)
	ownerKit := kit
	if row.OwnerName == "" {
		ownerKit = nil
	}
	workingDir, workingDirSource := j.ResolvedWorkingDir(kit)
	choice := j.Choice(kit)
	var heldBy *pauses.Hold
	if !j.Disabled {
		if p := s.activePauses(r.Context()).Cover(pauses.Subject{WorkspaceID: row.WorkspaceID, AgentID: row.AgentID(), Harness: choice.Harness, Kind: row.Kind}); p != nil {
			heldBy = p.Hold()
		}
	}
	// Index name, not ParseFile's folder basename. Runs, list, and pause
	// all key off row.Name (watchtower/job, not job).
	runCount, _ := s.runs.JobRunCount(r.Context(), row.ID)
	names := j.Skills
	if kit != nil {
		names = append(slices.Clone(names), kit.Skills...)
	}
	routes, _ := s.ws.RoutesOf(r.Context(), row.WorkspaceID, row.Name)
	routed := len(routes) > 0
	writeJSON(w, http.StatusOK, map[string]any{
		"archived_skills": s.archivedSkillNotes(r.Context(), skills.RunScope{WorkspaceID: row.WorkspaceID, JobID: row.ID}, names),
		"id":              row.ID, "name": row.Name, "workspace": row.WorkspaceID, "path": row.Path,
		"owner_id": row.OwnerID, "owner_name": row.OwnerName, "folder": j.Folder,
		"harness": j.Harness, "model": j.Model, "effort": j.Effort,
		"resolved_harness": choice.Harness, "harness_source": choice.HarnessSource,
		"resolved_model": choice.Model, "model_source": choice.ModelSource,
		"resolved_effort": choice.Effort, "effort_source": choice.EffortSource,
		"active": !j.Disabled, "hash": doc.Hash,
		"parse_error": parseErr, "prompt": j.Prompt,
		"inputs":   j.InputList(routed),
		"triggers": j.Triggers, "routes": s.targetRoutes(r.Context(), row.WorkspaceID, row.Name), "session": j.Session,
		"kit_source":  kitSourceFrom(ownerKit, j),
		"working_dir": workingDir, "working_dir_source": workingDirSource,
		"env_file": j.EnvFile, "folder_env": j.FolderEnvSummary(kit),
		"markdown": string(doc.Data), "mtime": doc.MTime,
		"readiness":   s.readiness(r.Context(), row.WorkspaceID, j, kit),
		"held_by":     heldBy,
		"archived_at": row.EffectiveArchivedAt(), "archived_with_agent": row.ArchivedAt == "" && row.OwnerArchivedAt != "",
		"run_count": runCount,
	})
}

// agentKitsFrom parses the agent rows once and returns each kit by agent
// name. A file that does not parse has no entry.
func agentKitsFrom(rows []workspaces.JobRow) map[string]*job.KitYAML {
	out := make(map[string]*job.KitYAML, len(rows))
	for _, row := range rows {
		if row.Kind != "agent" {
			continue
		}
		if parsed, err := job.ParseFile(row.Path); err == nil && parsed != nil {
			out[row.Name] = parsed.OwnerKit()
		}
	}
	return out
}

// kitSourceFrom marks kit values inherited from the owner agent kit. A
// job-level env, skills, or connectors entry wins and is labeled "job". A job
// with no owner kit has no labels.
func kitSourceFrom(kit *job.KitYAML, j *job.Job) map[string]string {
	out := map[string]string{}
	if kit == nil {
		return out
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
	if j != nil {
		if len(j.Env) > 0 {
			out["env"] = "job"
		}
		if len(j.Skills) > 0 {
			out["skills"] = "job"
		}
		if len(j.Connectors) > 0 {
			out["connectors"] = "job"
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
	if body.Agent != nil && *body.Agent != row.OwnerName && rejectAnyAgentScope(w, r) {
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
	case errors.Is(err, workspaces.ErrMoveNeedsAgent):
		writeErr(w, http.StatusBadRequest, "validation", "Choose the agent that gets the job")
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
		if job.OwnerOf(row.Name) == "" {
			valid, msg = false, job.JoinParseError(msg, job.ErrNoOwner.Error())
		}
	}
	if valid {
		if off := harnessOffError(j.ResolvedHarness(s.ws.OwnerKit(r.Context(), row.WorkspaceID, row.Name))); off != "" {
			valid, msg = false, off
		}
	}
	out := map[string]any{"ok": valid, "error": msg, "issues": job.IssueList{}}
	if valid {
		addIssues(out, s.jobIssues(r.Context(), row.WorkspaceID, row.Name, j))
	}
	writeJSON(w, http.StatusOK, out)
}

// jobIssues lists the issues of a parsed job at the index name: the file
// checks, then the problems of the routes that name the job.
func (s *Server) jobIssues(ctx context.Context, wsID, name string, j *job.Job) job.IssueList {
	var routed bool
	var route job.IssueList
	if s.connectors != nil {
		routed, route = s.connectors.JobRouteIssues(ctx, wsID, name)
	}
	return append(j.Issues(routed), route...)
}

// addIssues puts the issues on a validate result. An error issue makes the
// result not ok.
func addIssues(out map[string]any, issues job.IssueList) {
	if issues == nil {
		issues = job.IssueList{}
	}
	out["issues"] = issues
	if warn := issues.Messages(job.SeverityWarning); len(warn) > 0 {
		out["warnings"] = warn
	}
	if e := issues.Err(); e != nil {
		out["ok"], out["error"] = false, e.Message
	}
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
	if body.Agent != "" && job.OwnerOf(name) == "" {
		name = body.Agent + "/" + name
	}
	j, err := job.ParseBytes(filepath.Join(name, file), []byte(md))
	if err == nil && j != nil && j.ParseError == "" && body.Kind != "agent" && job.OwnerOf(name) == "" {
		err = job.ErrNoOwner
	}
	none := job.IssueList{}
	switch {
	case err != nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "markdown": md, "issues": none})
	case j != nil && j.ParseError != "":
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": j.ParseError, "markdown": md, "issues": none})
	case j != nil && harnessOffError(j.Harness) != "":
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": harnessOffError(j.Harness), "markdown": md, "issues": none})
	default:
		out := map[string]any{"ok": true, "error": "", "markdown": md}
		addIssues(out, s.jobIssues(r.Context(), s.workspaceID(r.Context(), body.Workspace), name, j))
		writeJSON(w, http.StatusOK, out)
	}
}

// maxSimulateEvents caps the events of one simulate call.
const maxSimulateEvents = 50

// jobSimulateRequest is the POST /v1/jobs/simulate body: a job or a draft,
// and the stored events to run it against.
type jobSimulateRequest struct {
	Workspace string         `json:"workspace"`
	Job       string         `json:"job"`
	Content   string         `json:"content"`
	RouteID   string         `json:"route_id"`
	EventIDs  []string       `json:"event_ids"`
	Since     string         `json:"since"`
	Limit     int            `json:"limit"`
	Inputs    map[string]any `json:"inputs"`
	Preview   bool           `json:"preview"`
}

// handleJobSimulate runs the event triggers of a job, or of a draft, against
// stored events: the gate, the input fill, and the prompt. It never launches.
func (s *Server) handleJobSimulate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body jobSimulateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	wsID := s.workspaceID(ctx, body.Workspace)
	j, ok := s.simulateJob(w, r, &body, wsID)
	if !ok {
		return
	}
	ids, err := s.simulateEvents(ctx, &body, wsID, j)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	results := make([]map[string]any, 0, len(ids))
	summary := map[string]any{"events": len(ids)}
	matched, ready, missing := 0, 0, map[string]int{}
	for _, id := range ids {
		res := s.simulateEvent(ctx, &body, wsID, j, id)
		if g, _ := res["gate"].(map[string]any); g["matched"] == true {
			matched++
		}
		if res["ready"] == true {
			ready++
		}
		for _, name := range res["missing"].([]string) {
			missing[name]++
		}
		results = append(results, res)
	}
	summary["matched"], summary["ready"], summary["missing"] = matched, ready, missing
	issues := s.jobIssues(ctx, wsID, j.Name, j)
	if issues == nil {
		issues = job.IssueList{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": summary, "results": results, "issues": issues})
}

// simulateJob parses the draft content, or loads the named job.
func (s *Server) simulateJob(w http.ResponseWriter, r *http.Request, body *jobSimulateRequest, wsID string) (*job.Job, bool) {
	switch {
	case body.Job == "" && body.Content == "":
		writeErr(w, http.StatusBadRequest, "validation", "set job or content")
		return nil, false
	case len(body.EventIDs) > maxSimulateEvents:
		writeErr(w, http.StatusBadRequest, "validation", fmt.Sprintf("event_ids: at most %d events", maxSimulateEvents))
		return nil, false
	case body.Content != "":
		j, err := job.ParseBytes("simulate/JOB.md", []byte(body.Content))
		if err == nil && j.ParseError != "" {
			err = errors.New(j.ParseError)
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return nil, false
		}
		// With job set, the draft uses the routes of that job.
		j.Name = body.Job
		return j, true
	}
	row, err := s.findJobRow(r.Context(), wsID, body.Job)
	if err != nil {
		writeResolveErr(w, err, "job")
		return nil, false
	}
	doc, err := readIndexedDoc(row.Path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, false
	}
	doc.Job.Name = row.Name
	if body.Workspace == "" {
		body.Workspace = row.WorkspaceID
	}
	return doc.Job, true
}

// simulateEvents is the event_ids of the body, else the newest events on
// the connections of the routes of j.
func (s *Server) simulateEvents(ctx context.Context, body *jobSimulateRequest, wsID string, j *job.Job) ([]string, error) {
	if len(body.EventIDs) > 0 {
		return body.EventIDs, nil
	}
	since, err := parseTimeFilter(body.Since)
	if err != nil {
		return nil, fmt.Errorf("since: %w", err)
	}
	limit := body.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, maxSimulateEvents)
	var instances []any
	located, err := s.ws.RoutesOf(ctx, wsID, j.Name)
	if err != nil {
		return nil, err
	}
	for _, lr := range located {
		if lr.Route.Gets == routing.GetsOff || (body.RouteID != "" && lr.Route.ID != body.RouteID) {
			continue
		}
		if id := s.connectors.IDOf(lr.Connection); id != "" {
			instances = append(instances, id)
		}
	}
	if len(instances) == 0 {
		return nil, nil
	}
	q := `SELECT id FROM events WHERE instance IN (?` + strings.Repeat(",?", len(instances)-1) + `)`
	args := instances
	if since != "" {
		q += ` AND received_at >= ?`
		args = append(args, since)
	}
	rows, err := s.st.DB().QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// simulateEvent is the result of one stored event.
func (s *Server) simulateEvent(ctx context.Context, body *jobSimulateRequest, wsID string, j *job.Job, id string) map[string]any {
	out := map[string]any{
		"event_id": id, "route_id": "", "manual": false,
		"inputs": map[string]job.FilledValue{}, "missing": []string{}, "asks": []string{}, "ready": false,
	}
	ev, err := s.connectors.StoredGateEvent(ctx, id)
	if err != nil {
		out["gate"] = map[string]any{"matched": false, "stage": "", "error": "event not found"}
		return out
	}
	tr, res, err := s.connectors.HandRoute(ctx, ev, wsID, j.Name, body.RouteID, false)
	out["gate"] = map[string]any{"matched": err == nil, "stage": res.Stage, "error": res.Error}
	if err != nil {
		return out
	}
	vars := &job.EventVars{Vars: ev.Vars(res.Vars), Payload: ev.Payload}
	f := j.Fill(&tr, vars, body.Inputs)
	out["route_id"], out["manual"] = tr.Route, tr.Manual
	out["inputs"] = f.Wire()
	out["missing"] = append([]string{}, f.Missing...)
	out["asks"] = append([]string{}, j.Fill(&tr, vars, nil).Missing...)
	out["ready"] = len(f.Missing) == 0 && len(f.Errors) == 0
	if len(f.Errors) > 0 {
		out["errors"] = f.Errors
	}
	if body.Preview {
		pv := maps.Clone(vars.Vars)
		if pv == nil {
			pv = map[string]string{}
		}
		maps.Copy(pv, f.Text())
		out["prompt_preview"] = truncStr(s.expandLaunchPrompt(ctx, wsID, j, j.Prompt, pv), 500)
	}
	return out
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
// asked name. An owned job has the name <owner>/<job>, and a run often
// asks for <job> only.
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
// name, or an agent by name (cmo). Both kinds own a folder, so
// the agent Files tab browses agents/<name>/ through the job file routes.
// Names stay exact -- a folder cannot hold both AGENT.md and JOB.md, so an
// index name resolves at most one row -- and this resolver is used only for
// file jails, never for launches.
func (s *Server) findFileRow(ctx context.Context, wsID, name string) (workspaces.JobRow, error) {
	return s.findIndexRow(ctx, wsID, name, func(row workspaces.JobRow) bool {
		return row.Name == name
	}, "job", "agent")
}

// findAgentRow resolves an agent by (workspace, name). Kind-strict: a
// kind=job launch can never pick up an agent row.
func (s *Server) findAgentRow(ctx context.Context, wsID, name string) (workspaces.JobRow, error) {
	return s.findIndexRow(ctx, wsID, name, func(row workspaces.JobRow) bool {
		return row.Name == name
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
	Inputs      job.Inputs        `json:"inputs"`
	Session     job.SessionYAML   `json:"session"`
	Env         map[string]string `json:"env"`
	Agent       string            `json:"agent"` // agent name; file lands under agents/<agent>/
	Kind        string            `json:"kind"`  // validate only: job (default) or agent
}

// splitOwner moves an <x>/ prefix of the name into Agent. It
// reports false when the prefix names another agent than Agent.
func (b *jobCreateRequest) splitOwner() bool {
	owner := job.OwnerOf(b.Name)
	if owner == "" {
		return true
	}
	if b.Agent != "" && b.Agent != owner {
		return false
	}
	b.Agent, b.Name = owner, strings.TrimPrefix(b.Name, owner+"/")
	return true
}

// markdown renders the JOB.md text of the request. A body with neither text nor
// a prompt gets the placeholder job, which keeps the old bare-create working.
func (b jobCreateRequest) markdown() (string, error) {
	spec := job.Spec{
		Content: cmp.Or(b.Markdown, b.Content), Prompt: b.Prompt, Harness: b.Harness,
		Model: b.Model, Effort: b.Effort,
		Description: b.Description, Folder: b.Folder, Cron: b.Cron, Every: b.Every,
		Session: b.Session, Env: b.Env, Inputs: b.Inputs,
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
	if !body.splitOwner() {
		writeErr(w, http.StatusBadRequest, "validation", "agent "+body.Agent+" does not match the name "+body.Name)
		return
	}
	if scope, agent := scopeFrom(r.Context()); agent {
		if scope.workspaceID != ws.ID {
			writeForbidden(w, "agent may create only in its own workspace")
			return
		}
		if body.Agent != "" && body.Agent != scope.agent {
			writeForbidden(w, "agent may create only jobs owned by "+scope.agent)
			return
		}
		body.Agent = scope.agent
	}
	if body.Agent == "" {
		writeErr(w, http.StatusBadRequest, "validation", job.ErrNoOwner.Error()+": set agent")
		return
	}
	md, err := body.markdown()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	// The file lands under the agent folder; the wire name is <agent>/<name>.
	agentRow, err := s.findAgentRow(r.Context(), ws.ID, body.Agent)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	wireName := agentRow.Name + "/" + body.Name
	rel := filepath.ToSlash(filepath.Join(job.Dir(agentRow.Name), body.Name, "JOB.md"))
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
	if err := job.KeepArchived(row.Path, row.ArchivedAt); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if info, err = s.files.Stat(r.Context(), ws.Path, rel); err != nil {
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
	purge := r.URL.Query().Get("purge") == "1"
	refs, err := s.ws.RouteRefs(r.Context(), row.WorkspaceID, []string{row.Name})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !confirmed(r) {
		writeConfirmRoutes(w, jobDeleteText(name, purge), name, refs)
		return
	}
	s.deleteWithRoutes(w, r, row.WorkspaceID, refs, func(w http.ResponseWriter) {
		if purge {
			s.purgeJob(w, r, name, row, ws, rel)
			return
		}
		s.deleteJobFile(w, r, name, row, ws, rel)
	})
}

// jobDeleteText is the confirm text of a job delete.
func jobDeleteText(name string, purge bool) string {
	if purge {
		return "deletes job " + name + " and its whole folder; run rows stay"
	}
	return "deletes the JOB.md of " + name + "; other files in the job folder and run rows stay"
}

// deleteJobFile deletes the JOB.md of a job and keeps the rest of its folder.
func (s *Server) deleteJobFile(w http.ResponseWriter, r *http.Request, name string, row workspaces.JobRow, ws *workspaces.Workspace, rel string) {
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

// handleJobsSubtree dispatches the raw-slash job names that the single-segment
// {name} routes cannot match: an owned job x/y, or a row outside the agent
// layout whose name starts with "/". It restores the `name` and, for files,
// `path` values of the generic handlers.
func (s *Server) handleJobsSubtree(w http.ResponseWriter, r *http.Request) {
	segs, ok := splitEscapedPath(strings.TrimPrefix(r.URL.EscapedPath(), "/v1/jobs/"))
	if !ok || len(segs) == 0 || segs[0] == "" {
		writeErr(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	name, parts := segs[0], segs[1:]
	if !strings.HasPrefix(name, "/") {
		if len(parts) == 0 || parts[0] == "" {
			writeErr(w, http.StatusNotFound, "not_found", "job not found")
			return
		}
		name, parts = name+"/"+parts[0], parts[1:]
	}
	for _, seg := range append([]string{name}, parts...) {
		if strings.Contains(seg, "..") {
			writeErr(w, http.StatusBadRequest, "validation", "invalid job path")
			return
		}
	}
	r.SetPathValue("name", name)
	tail := "/" + strings.Join(parts, "/")
	if len(parts) == 0 {
		tail = "/"
	}
	if len(parts) == 2 && parts[0] == "files" && parts[1] == "move" && r.Method == http.MethodPost {
		s.inJobFiles(s.filesMove)(w, r)
		return
	}
	if len(parts) > 1 && parts[0] == "folders" {
		r.SetPathValue("path", strings.Join(parts[1:], "/"))
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
	if len(parts) >= 1 && parts[0] == "files" {
		if len(parts) == 1 && r.Method == http.MethodGet {
			r.SetPathValue("path", "")
			s.handleJobFilesList(w, r)
			return
		}
		if len(parts) > 1 {
			r.SetPathValue("path", strings.Join(parts[1:], "/"))
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
	case "POST /run", "POST /pause", "POST /unpause", "POST /archive", "POST /unarchive", "POST /validate":
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

// splitEscapedPath splits an escaped path on "/" and unescapes each segment.
func splitEscapedPath(escaped string) ([]string, bool) {
	segs := strings.Split(strings.Trim(escaped, "/"), "/")
	for i, seg := range segs {
		un, err := url.PathUnescape(seg)
		if err != nil {
			return nil, false
		}
		segs[i] = un
	}
	return segs, true
}

func (s *Server) handleJobRunForTail(w http.ResponseWriter, r *http.Request, tail string) {
	switch tail {
	case "/run":
		s.handleJobRun(w, r)
	case "/pause":
		s.handleJobPause(w, r)
	case "/unpause":
		s.handleJobUnpause(w, r)
	case "/archive":
		s.handleJobArchive(w, r)
	case "/unarchive":
		s.handleJobUnarchive(w, r)
	case "/validate":
		s.handleJobValidate(w, r)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "job route not found")
	}
}
