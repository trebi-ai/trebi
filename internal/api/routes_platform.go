package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tmux"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

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
	rows, err := s.ws.ListJobs(ctx, wsID)
	if err != nil {
		return workspaces.JobRow{}, err
	}
	var matches []workspaces.JobRow
	for _, row := range rows {
		if row.Name == name && row.Kind == "job" {
			matches = append(matches, row)
		}
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

// findAssistantRow resolves the workspace assistant row (TREBI.md).
func (s *Server) findAssistantRow(ctx context.Context, wsID string) (workspaces.JobRow, error) {
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

// resolveJob is findJobRow plus HTTP error mapping.
func (s *Server) resolveJob(w http.ResponseWriter, r *http.Request, name string) (workspaces.JobRow, bool) {
	row, err := s.findJobRow(r.Context(), r.URL.Query().Get("workspace"), name)
	if err != nil {
		writeResolveErr(w, err)
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

// memoryRootRel maps a memory request to workspace root + relative path + abs path.
func (s *Server) memoryRootRel(r *http.Request, assistant bool) (root, rel, abs string, err error) {
	abs, err = s.memoryPath(r, assistant)
	if err != nil {
		return "", "", "", err
	}
	var wsID string
	if assistant {
		wsID = r.PathValue("id")
	} else {
		row, err := s.findJobRow(r.Context(), r.URL.Query().Get("workspace"), r.PathValue("name"))
		if err != nil {
			return "", "", "", err
		}
		wsID = row.WorkspaceID
	}
	ws, err := s.ws.Get(r.Context(), wsID)
	if err != nil {
		return "", "", "", err
	}
	rel, err = workspaceRel(ws.Path, abs)
	if err != nil {
		return "", "", "", err
	}
	return ws.Path, rel, abs, nil
}

// writeResolveErr maps job resolution errors to HTTP codes.
func writeResolveErr(w http.ResponseWriter, err error) {
	var amb *ambiguousJobError
	switch {
	case errors.As(err, &amb):
		writeErr(w, http.StatusBadRequest, "ambiguous", amb.Error())
	case errors.Is(err, errJobNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "job not found")
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// appendBus emits one bus event after a non-DB write (files, secrets, config).
// Do not use this for DB-backed entities — wrap those with store.AppendEntity
// so the row and the bus frame commit together. A crash must not advance an
// entity past what the bus recorded.
func (s *Server) appendBus(ctx context.Context, ev store.BusEvent) {
	if _, err := s.st.Append(ctx, ev); err != nil {
		s.log.Warn("bus append failed", "topic", ev.Topic, "error", err)
	}
}

func (s *Server) doctorFindings() []map[string]any {
	src := alerts.CollectDoctorFindings()
	out := make([]map[string]any, 0, len(src))
	for _, f := range src {
		out = append(out, map[string]any{
			"id": f.ID, "severity": f.Severity, "code": f.Code, "message": f.Message,
		})
	}
	return out
}

func (s *Server) handleRunTranscript(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	data, tool, ok := s.readTranscript(run)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no transcript")
		return
	}
	if r.URL.Query().Get("raw") == "1" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write(data)
		return
	}
	turns, err := tool.ParseTranscript(bytes.NewReader(data))
	if err != nil {
		if errors.Is(err, tmux.ErrTranscriptUnsupported) {
			writeErr(w, http.StatusNotFound, "not_found", "transcript unsupported")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID, "turns": turns})
}

func (s *Server) readTranscript(run *runs.Run) ([]byte, tmux.Tool, bool) {
	if run.SessionID == "" {
		return nil, nil, false
	}
	tool, ok := tmux.ByName(run.Harness)
	if !ok {
		return nil, nil, false
	}
	path, err := tool.TranscriptPath(home.HarnessDir(run.Harness), "", run.SessionID)
	if err != nil {
		return nil, nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, false
	}
	return data, tool, true
}

func (s *Server) handleRunLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := filepath.Join(home.RunsLogDir(), id+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		// fallback: capture pane if live
		run, gerr := s.runs.Get(r.Context(), id)
		if gerr == nil && run.TmuxSession != "" {
			screen, rerr := tmux.ReadScreen(run.TmuxSession)
			if rerr == nil {
				writeJSON(w, http.StatusOK, map[string]any{"log": screen, "source": "pane"})
				return
			}
		}
		writeErr(w, http.StatusNotFound, "not_found", "log not found")
		return
	}
	if r.URL.Query().Get("follow") == "1" {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(string(data), "\n", "\ndata: "))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"log": string(data), "source": "file"})
}

func (s *Server) handleRunResume(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.Resume(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// launchOptsFromJob rebuilds launch options for prev from its job file:
// prompt with trigger vars/inputs applied, cwd, if_running, and session knobs.
// Trigger capture and conversation key carry over.
func (s *Server) launchOptsFromJob(ctx context.Context, prev *runs.Run, reason runs.RunReason) (runs.LaunchOpts, error) {
	row, err := s.findJobRow(ctx, prev.Workspace, prev.Job)
	if err != nil {
		return runs.LaunchOpts{}, err
	}
	j, err := job.ParseFile(row.Path)
	if err != nil || j == nil {
		if err == nil {
			err = errJobNotFound
		}
		return runs.LaunchOpts{}, err
	}
	prompt := j.Prompt
	if prev.Trigger != nil {
		for k, v := range prev.Trigger.Vars {
			prompt = strings.ReplaceAll(prompt, "{{"+k+"}}", v)
		}
	}
	for k, v := range prev.Inputs {
		prompt = strings.ReplaceAll(prompt, "{{"+k+"}}", v)
	}
	lopts := runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: prev.Workspace, Job: prev.Job, Kind: prev.Kind,
			Reason: reason, Harness: prev.Harness,
			PromptPreview:   prompt,
			Inputs:          prev.Inputs,
			Trigger:         prev.Trigger,
			ConversationKey: prev.ConversationKey,
		},
		Prompt: prompt,
	}
	lopts.WorkingDir = j.WorkingDir
	if lopts.WorkingDir == "" {
		lopts.WorkingDir = j.Dir
	}
	lopts.IfRunning = j.Session.IfRunning
	lopts.MaxDurationMS, lopts.IdleTimeoutMS, lopts.KeepAliveMS, lopts.NudgeMS = j.Session.KnobsMS()
	return lopts, nil
}

func (s *Server) handleRunReplay(w http.ResponseWriter, r *http.Request) {
	prev, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	// A replay rebuilds the prompt from the job file plus the captured
	// trigger vars/inputs — prompt_preview is truncated and not a prompt.
	lopts, err := s.launchOptsFromJob(r.Context(), prev, runs.ReasonReplay)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "job file for run no longer resolvable")
		return
	}
	run, _, err := s.runs.CreateAndLaunch(r.Context(), lopts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) handleRunRetry(w http.ResponseWriter, r *http.Request) {
	// D7: policy dispatch on end_reason.
	// - agent/logic failures → resume with failure note
	// - auth/limit → park (blocked resume intent)
	// - session_gone / infra → fresh rerun (replay-like)
	prev, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	switch prev.EndReason {
	case runs.EndAuth, runs.EndLimit:
		// Park: enqueue blocked resume intent
		br := dispatch.BlockAuthMissing
		if prev.EndReason == runs.EndLimit {
			br = dispatch.BlockLimitExceeded
		}
		item := &dispatch.QueueItem{
			Integration:     "retry",
			WorkspaceID:     prev.Workspace,
			Job:             prev.Job,
			Kind:            prev.Kind,
			ConversationKey: prev.ConversationKey,
			Status:          dispatch.QueueBlocked,
			BlockReason:     br,
			ResumeOf:        prev.ID,
			Prompt:          "resume after " + string(prev.EndReason),
		}
		if err := s.queue.Enqueue(r.Context(), item); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		s.runs.AddEvent(r.Context(), prev.ID, "parked", "retry", map[string]string{
			"queue_id": item.ID, "block_reason": string(br),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"action": "parked", "queue_id": item.ID, "block_reason": br, "run_id": prev.ID,
		})
		return
	case runs.EndSessionGone, runs.EndLaunch:
		// Fresh rerun. Kill is a deliberate user act — it takes the
		// default resume-with-note path (backend-0808 §D7).
		s.handleRunReplay(w, r)
		return
	default:
		// Agent/logic: resume with failure note
		note := "Retry after failure (end_reason=" + string(prev.EndReason) + "). Continue or fix the issue."
		if prev.SessionID != "" {
			run, err := s.runs.Resume(r.Context(), prev.ID)
			if err == nil && run != nil {
				// Inject failure note if session live
				if run.TmuxSession != "" {
					_ = s.runs.InjectMessage(r.Context(), run.ID, note) // resume already succeeded; note is best-effort
				}
				writeJSON(w, http.StatusCreated, map[string]any{
					"action": "resume", "run": run, "note": note,
				})
				return
			}
		}
		// Fall back to a fresh launch rebuilt from the job file, with the
		// failure note as prompt prefix.
		if prev.ResumeAttempts >= runs.MaxResumeAttempts {
			writeErr(w, http.StatusConflict, "conflict", "resume cap reached")
			return
		}
		lopts, err := s.launchOptsFromJob(r.Context(), prev, runs.ReasonResume)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "job file for run no longer resolvable")
			return
		}
		lopts.ResumedFrom = prev.ID
		lopts.ResumeAttempts = prev.ResumeAttempts + 1
		lopts.Prompt = note + "\n\n" + lopts.Prompt
		lopts.PromptPreview = lopts.Prompt
		run, _, err := s.runs.CreateAndLaunch(r.Context(), lopts)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"action": "rerun_with_note", "run": run})
	}
}

func (s *Server) handleRunMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == "" {
		writeErr(w, http.StatusBadRequest, "validation", "text required")
		return
	}
	if err := s.runs.InjectMessage(r.Context(), r.PathValue("id"), body.Text); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRunApprove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string `json:"decision"` // allow|deny
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body defaults to allow
	a, err := s.runs.DecideApproval(r.Context(), r.PathValue("id"), body.Decision)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if errors.Is(err, runs.ErrInvalidDecision) {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cursor, ok := parseULIDCursor(w, r, "inbox")
	if !ok {
		return
	}
	limit := parseLimit(r)
	items, err := s.alerts.List(r.Context(), alerts.ListOpts{
		Status: q.Get("status"), Kind: q.Get("kind"), Limit: limit, Cursor: cursor,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := map[string]any{"items": items}
	eff := limit
	if eff <= 0 {
		eff = 50
	}
	if len(items) == eff {
		resp["next_cursor"] = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleInboxGet(w http.ResponseWriter, r *http.Request) {
	item, err := s.alerts.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, alerts.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "inbox item not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleInboxRead(w http.ResponseWriter, r *http.Request) {
	s.setInboxStatus(w, r, r.PathValue("id"), alerts.InboxRead, "manual")
}

func (s *Server) handleInboxReadAll(w http.ResponseWriter, r *http.Request) {
	n, err := s.alerts.ReadAll(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"read": n})
}

func (s *Server) handleInboxResolve(w http.ResponseWriter, r *http.Request) {
	s.setInboxStatus(w, r, r.PathValue("id"), alerts.InboxResolved, "manual")
}

func (s *Server) setInboxStatus(w http.ResponseWriter, r *http.Request, id string, status alerts.InboxStatus, how string) {
	err := s.alerts.SetStatus(r.Context(), id, status, how)
	if errors.Is(err, alerts.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "inbox item not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
}

// handleEventGet is implemented in events.go (B15 outcome join).

func (s *Server) handleEventRetrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var integ, raw string
	err := s.st.DB().QueryRowContext(r.Context(), `SELECT integration, COALESCE(raw_json,'') FROM events WHERE id=?`, id).
		Scan(&integ, &raw)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "event not found")
		return
	}
	var event any
	_ = json.Unmarshal([]byte(raw), &event) // retrigger still fires with a raw string if json is corrupt
	_, err = s.integ.InjectEvent(r.Context(), integ, event)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retrigged": true, "event_id": id})
}

func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	cursor, ok := parseULIDCursor(w, r, "approval")
	if !ok {
		return
	}
	limit := parseLimit(r)
	list, err := s.runs.ListApprovals(r.Context(), runs.ApprovalListOpts{
		Limit: limit, Cursor: cursor,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := map[string]any{"approvals": list}
	if len(list) == limit {
		resp["next_cursor"] = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSystemLogs(w http.ResponseWriter, r *http.Request) {
	path := home.DaemonLogPath()
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}})
		return
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (s *Server) handleSystemUpdate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"current": Version, "latest": Version, "update_available": false,
	})
}

func (s *Server) handleCloudLink(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"linked": false, "status": "not_configured", "note": "cloudlink broker not configured"})
}

func (s *Server) handleCloudUnlink(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"linked": false})
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

func (s *Server) handleAssistantMemoryGet(w http.ResponseWriter, r *http.Request) {
	s.handleMemoryFile(w, r, true)
}

func (s *Server) handleAssistantMemoryPut(w http.ResponseWriter, r *http.Request) {
	s.handleMemoryFilePut(w, r, true)
}

func (s *Server) handleJobMemoryGet(w http.ResponseWriter, r *http.Request) {
	s.handleMemoryFile(w, r, false)
}

func (s *Server) handleJobMemoryPut(w http.ResponseWriter, r *http.Request) {
	s.handleMemoryFilePut(w, r, false)
}

// handleMemoryFile serves the B22 memory ledger wire shape.
func (s *Server) handleMemoryFile(w http.ResponseWriter, r *http.Request, assistant bool) {
	path, err := s.memoryPath(r, assistant)
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	info, err := job.LoadMemoryInfo(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if info == nil {
		info = &job.MemoryInfo{Entries: []job.MemoryEntry{}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path, "size_bytes": info.SizeBytes, "updated_at": info.UpdatedAt,
		"hash": info.Hash, "preamble": info.Preamble, "entries": info.Entries,
		"content": info.Content,
	})
}

// handleMemoryFilePut replaces the whole memory file (reset).
func (s *Server) handleMemoryFilePut(w http.ResponseWriter, r *http.Request, assistant bool) {
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	root, rel, abs, err := s.memoryRootRel(r, assistant)
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	info, _, err := s.files.Write(r.Context(), root, rel, []byte(body.Content), ifMatchHeader(r))
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	setETagHash(w, info.Hash)
	writeJSON(w, http.StatusOK, map[string]any{"path": abs, "hash": info.Hash})
}

func (s *Server) handleJobsCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Workspace string `json:"workspace"`
		Name      string `json:"name"`
		Content   string `json:"content"`
		Markdown  string `json:"markdown"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	ws, err := s.ws.Get(r.Context(), body.Workspace)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "validation", "name required")
		return
	}
	// Prefer markdown (contract) over content alias
	md := body.Markdown
	if md == "" {
		md = body.Content
	}
	if md == "" {
		md = "---\nharness: claude\n---\n\nDo the task.\n"
	}
	rel := filepath.ToSlash(filepath.Join(body.Name, "JOB.md"))
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
	_ = s.ws.RebuildIndex(r.Context(), ws) // file is written; watcher will retry the index
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "job.created", Entity: "job", EntityID: body.Name, Action: "created",
		Attrs: map[string]string{"workspace": ws.ID, "path": path},
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"name": body.Name, "path": path, "hash": info.Hash,
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
	if !s.requireIfMatch(w, r, ws.Path, rel) {
		return
	}
	if err := job.MergePatchFrontmatter(row.Path, body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	info, err := s.files.Stat(r.Context(), ws.Path, rel)
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	setETagHash(w, info.Hash)
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "job.updated", Entity: "job", EntityID: name, Action: "updated",
		Attrs: map[string]string{"workspace": row.WorkspaceID, "path": row.Path},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "patched": true, "hash": info.Hash,
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
	_ = s.ws.RebuildIndex(r.Context(), ws) // file is gone; watcher will retry the index
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "job.deleted", Entity: "job", EntityID: name, Action: "deleted",
		Attrs: map[string]string{"workspace": row.WorkspaceID, "path": row.Path},
	})
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
	n, err := s.countFiles(r.Context(), jobDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := os.RemoveAll(jobDir); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	_ = s.ws.RebuildIndex(r.Context(), ws)
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "job.deleted", Entity: "job", EntityID: name, Action: "deleted",
		Attrs: map[string]string{"workspace": row.WorkspaceID, "path": row.Path},
	})
	folderRel, _ := workspaceRel(ws.Path, jobDir)
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "file.deleted", Entity: "file", EntityID: ws.ID + ":" + folderRel, Action: "deleted",
		Attrs: map[string]string{"workspace": ws.ID, "path": folderRel, "purge": "true"},
	})
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
		if pathInside(jobDir, other.Path) {
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

func (s *Server) handleAgentHook(w http.ResponseWriter, r *http.Request) {
	event := r.PathValue("event")
	runID := r.Header.Get("X-Trebi-Run-Id")
	if runID == "" {
		runID = r.URL.Query().Get("run_id")
	}
	body, _ := io.ReadAll(r.Body)
	_ = body // hook payload is logged via AddEvent; body is unused by state mapping
	if runID != "" {
		switch event {
		case "waiting", "permission-request":
			s.runs.SetAgentState(r.Context(), runID, runs.AgentWaiting, "hook")
		case "running", "tool-start", "session-start":
			s.runs.SetAgentState(r.Context(), runID, runs.AgentRunning, "hook")
		case "stop", "session-end", "idle":
			s.runs.SetAgentState(r.Context(), runID, runs.AgentIdle, "hook")
		}
		s.runs.AddEvent(r.Context(), runID, "hook:"+event, "agent-hook", map[string]string{"event": event})
	}
	// Claude requires JSON hook stdout
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}")) // Claude requires JSON hook stdout; disconnect is fine
}
