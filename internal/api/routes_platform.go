package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/dispatch"
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

// ifMatchOK enforces D1 If-Match: an absent header proceeds; a present
// header that does not match the current content hash gives 412.
func ifMatchOK(w http.ResponseWriter, r *http.Request, path string) bool {
	ifMatch := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if ifMatch == "" || ifMatch == "*" {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, http.StatusPreconditionFailed, "precondition_failed", "If-Match set but file is missing")
			return false
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return false
	}
	if hash := job.ContentHash(data); hash != ifMatch {
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", "If-Match mismatch; current hash: "+hash)
		return false
	}
	return true
}

// setETag writes ETag response header from file content hash.
func setETag(w http.ResponseWriter, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	w.Header().Set("ETag", `"`+job.ContentHash(data)+`"`)
}

// appendBus emits one bus event and logs append failures.
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
	if run.SessionID == "" {
		writeErr(w, http.StatusNotFound, "not_found", "no transcript")
		return
	}
	tool, ok := tmux.ByName(run.Harness)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "transcript unsupported")
		return
	}
	path, err := tool.TranscriptPath(home.HarnessDir(run.Harness), "", run.SessionID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	_, _ = w.Write(data)
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
		id := ulid.Make().String()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := s.st.DB().ExecContext(r.Context(), `INSERT INTO event_queue (
			id, integration, workspace_id, job, kind, conversation_key, status, block_reason,
			resume_of, prompt, trace_id, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, "retry", prev.Workspace, prev.Job, prev.Kind, prev.ConversationKey,
			string(dispatch.QueueBlocked), string(br), prev.ID, "resume after "+string(prev.EndReason),
			ulid.Make().String(), now, now)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		s.runs.AddEvent(r.Context(), prev.ID, "parked", "retry", map[string]string{
			"queue_id": id, "block_reason": string(br),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"action": "parked", "queue_id": id, "block_reason": br, "run_id": prev.ID,
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
					_ = s.runs.InjectMessage(r.Context(), run.ID, note)
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
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Decision == "" {
		body.Decision = "allow"
	}
	id := ulid.Make().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	runID := r.PathValue("id")
	_, err := s.st.DB().ExecContext(r.Context(), `INSERT INTO approvals (id, run_id, question, status, decision, created_at, decided_at)
		VALUES (?,?,?,?,?,?,?)`, id, runID, "", "decided", body.Decision, now, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "approval.decided", Entity: "approval", EntityID: id, Action: "updated",
		Attrs: map[string]string{"run_id": runID, "decision": body.Decision},
	})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "decision": body.Decision})
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cursor := q.Get("cursor")
	if cursor != "" {
		if _, err := ulid.Parse(cursor); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "malformed cursor: not an inbox id")
			return
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
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
	_ = json.Unmarshal([]byte(raw), &event)
	_, err = s.integ.InjectEvent(r.Context(), integ, event)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retrigged": true, "event_id": id})
}

// listRows runs query, scans nCols strings per row, and maps each row to a
// wire object. A query error returns an empty list (table may be absent).
func (s *Server) listRows(ctx context.Context, query string, nCols int, mapRow func([]string) map[string]any) []map[string]any {
	list := []map[string]any{}
	rows, err := s.st.DB().QueryContext(ctx, query)
	if err != nil {
		return list
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]string, nCols)
		ptrs := make([]any, nCols)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if rows.Scan(ptrs...) == nil {
			list = append(list, mapRow(vals))
		}
	}
	return list
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	list := s.listRows(r.Context(), `
		SELECT id, workspace_id, COALESCE(key,''), COALESCE(title,''), updated_at
		FROM conversations ORDER BY updated_at DESC LIMIT 100`, 5,
		func(v []string) map[string]any {
			return map[string]any{
				"id": v[0], "workspace": v[1], "key": v[2], "title": v[3], "updated_at": v[4],
			}
		})
	writeJSON(w, http.StatusOK, map[string]any{"conversations": list})
}

func (s *Server) handleConversationGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var ws, key, title, updated string
	err := s.st.DB().QueryRowContext(r.Context(), `
		SELECT workspace_id, COALESCE(key,''), COALESCE(title,''), updated_at
		FROM conversations WHERE id=?`, id).Scan(&ws, &key, &title, &updated)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "conversation not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "workspace": ws, "key": key, "title": title, "updated_at": updated,
	})
}

func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	list := s.listRows(r.Context(), `
		SELECT id, run_id, status, COALESCE(question,''), created_at FROM approvals
		ORDER BY created_at DESC LIMIT 100`, 5,
		func(v []string) map[string]any {
			return map[string]any{
				"id": v[0], "run_id": v[1], "status": v[2], "prompt": v[3], "created_at": v[4],
			}
		})
	writeJSON(w, http.StatusOK, map[string]any{"approvals": list})
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
	path, err := s.memoryPath(r, assistant)
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	if !ifMatchOK(w, r, path) {
		return
	}
	if err := job.WriteAtomic(path, []byte(body.Content)); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	setETag(w, path)
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "hash": job.ContentHash([]byte(body.Content))})
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
	path := filepath.Join(ws.Path, body.Name, "JOB.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if _, err := os.Stat(path); err == nil {
		writeErr(w, http.StatusConflict, "conflict", "job exists")
		return
	}
	if err := job.WriteAtomic(path, []byte(md)); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	_ = s.ws.RebuildIndex(r.Context(), ws)
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "job.created", Entity: "job", EntityID: body.Name, Action: "created",
		Attrs: map[string]string{"workspace": ws.ID, "path": path},
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"name": body.Name, "path": path, "hash": job.ContentHash([]byte(md)),
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
	if !ifMatchOK(w, r, row.Path) {
		return
	}
	if err := job.WriteAtomic(row.Path, []byte(md)); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	setETag(w, row.Path)
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "job.updated", Entity: "job", EntityID: name, Action: "updated",
		Attrs: map[string]string{"workspace": row.WorkspaceID, "path": row.Path},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "path": row.Path, "hash": job.ContentHash([]byte(md)),
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
	if !ifMatchOK(w, r, row.Path) {
		return
	}
	if err := job.MergePatchFrontmatter(row.Path, body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	data, _ := os.ReadFile(row.Path)
	setETag(w, row.Path)
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "job.updated", Entity: "job", EntityID: name, Action: "updated",
		Attrs: map[string]string{"workspace": row.WorkspaceID, "path": row.Path},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "patched": true, "hash": job.ContentHash(data),
	})
}

func (s *Server) handleJobDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	row, ok := s.resolveJob(w, r, name)
	if !ok {
		return
	}
	if !ifMatchOK(w, r, row.Path) {
		return
	}
	if err := os.Remove(row.Path); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if ws, err := s.ws.Get(r.Context(), row.WorkspaceID); err == nil {
		_ = s.ws.RebuildIndex(r.Context(), ws)
	}
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "job.deleted", Entity: "job", EntityID: name, Action: "deleted",
		Attrs: map[string]string{"workspace": row.WorkspaceID, "path": row.Path},
	})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

func (s *Server) handleWorkspacePatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	id := r.PathValue("id")
	if body.Name != "" {
		_, err := s.st.DB().ExecContext(r.Context(), `UPDATE workspaces SET name=?, updated_at=? WHERE id=?`,
			body.Name, time.Now().UTC().Format(time.RFC3339), id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	ws, err := s.ws.Get(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	s.appendBus(r.Context(), store.BusEvent{
		Topic: "workspace.updated", Entity: "workspace", EntityID: id, Action: "updated",
		Attrs: map[string]string{"name": ws.Name},
	})
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
	_ = body
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
	_, _ = w.Write([]byte("{}"))
}
