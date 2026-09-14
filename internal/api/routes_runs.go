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
	"time"

	"github.com/gorilla/websocket"
	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/runtime/native"
	"github.com/trebi-ai/trebi/internal/tmux"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// runCreateRequest is the POST /v1/runs body, shared with createRunFromJob.
type runCreateRequest struct {
	Workspace       string            `json:"workspace"`
	Job             string            `json:"job"`
	Kind            string            `json:"kind"`
	Reason          string            `json:"reason"`
	PromptOverride  string            `json:"prompt_override"`
	ExtraPrompt     string            `json:"extra_prompt"`
	ConversationID  string            `json:"conversation_id"`
	Inputs          map[string]string `json:"inputs"`
	DryRun          bool              `json:"dry_run"`
	Harness         string            `json:"harness"`
	IdempotencyKey  string            `json:"idempotency_key"`
	conversationKey string
}

func (s *Server) routeRuns(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/runs", s.handleRunsList)
	mux.HandleFunc("POST /v1/runs", s.handleRunsCreate)
	mux.HandleFunc("GET /v1/runs/{id}", s.handleRunGet)
	mux.HandleFunc("GET /v1/runs/{id}/timeline", s.handleRunTimeline)
	mux.HandleFunc("GET /v1/runs/{id}/transcript", s.handleRunTranscript)
	mux.HandleFunc("GET /v1/runs/{id}/log", s.handleRunLog)
	mux.HandleFunc("POST /v1/runs/{id}/kill", s.handleRunKill)
	mux.HandleFunc("POST /v1/runs/kill", s.handleRunsKill)
	mux.HandleFunc("POST /v1/runs/{id}/resume", s.handleRunResume)
	mux.HandleFunc("POST /v1/runs/{id}/replay", s.handleRunReplay)
	mux.HandleFunc("POST /v1/runs/{id}/retry", s.handleRunRetry)
	mux.HandleFunc("POST /v1/runs/{id}/message", s.handleRunMessage)
	mux.HandleFunc("POST /v1/runs/{id}/approve", s.handleRunApprove)
	mux.HandleFunc("GET /v1/runs/{id}/attach", s.handleRunAttach)
	mux.HandleFunc("GET /v1/approvals", s.handleApprovals)
	mux.HandleFunc("GET /v1/runs/session-state", s.handleSessionState)
	mux.HandleFunc("GET /v1/stats/runs", s.handleStats)
	mux.HandleFunc("POST /v1/agent-hook/{event}", s.handleAgentHook)
}

func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseLimit(r)
	cursor := q.Get("cursor")
	if cursor != "" {
		if _, err := ulid.Parse(cursor); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "malformed cursor: not a run id")
			return
		}
	}
	since, err := parseTimeFilter(q.Get("since"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "since: "+err.Error())
		return
	}
	until, err := parseTimeFilter(q.Get("until"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "until: "+err.Error())
		return
	}
	list, err := s.runs.List(r.Context(), runs.ListOpts{
		Workspace:    s.workspaceID(r.Context(), q.Get("workspace")),
		Job:          q.Get("job"),
		Status:       q.Get("status"),
		Kind:         q.Get("kind"),
		Harness:      q.Get("harness"),
		Model:        q.Get("model"),
		Conversation: q.Get("conversation"),
		EndReason:    q.Get("end_reason"),
		Q:            q.Get("q"),
		Since:        since,
		Until:        until,
		Limit:        limit,
		Cursor:       cursor,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := map[string]any{"runs": list}
	// A full page suggests more rows: the last id is the next cursor.
	effLimit := limit
	if effLimit <= 0 {
		effLimit = 50
	}
	if len(list) == effLimit {
		resp["next_cursor"] = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// createRun validates the request and launches the run.
func (s *Server) createRun(w http.ResponseWriter, r *http.Request, body runCreateRequest) {
	if body.Job == "" {
		writeErr(w, http.StatusBadRequest, "validation", "job is required")
		return
	}
	reason := runs.RunReason(body.Reason)
	if reason == "" {
		reason = runs.ReasonManual
	}
	// Drain blocks auto producers; shadow/drain still allows explicit manual launches (§7.5 / A10).
	if s.isDraining() && reason != runs.ReasonManual {
		writeErr(w, http.StatusServiceUnavailable, "draining", "daemon is draining")
		return
	}
	if key := idempotencyKey(r, body.IdempotencyKey); key != "" && !body.DryRun {
		if run, ok := s.runForKey(w, r, key); ok {
			if run != nil {
				writeJSON(w, http.StatusCreated, run)
			}
			return
		}
		body.IdempotencyKey = key
	}
	body.Workspace = s.workspaceID(r.Context(), body.Workspace)
	row, rerr := s.resolveRunRow(r.Context(), body)
	if rerr != nil {
		writeResolveErr(w, rerr, "job")
		return
	}
	if body.Kind == runs.KindAgent {
		// Canonical launch takes the short name; runs and wakes carry the index name.
		body.Job = row.Name
	}
	if body.Workspace == "" {
		body.Workspace = row.WorkspaceID
	}
	if err := s.bindConversation(r.Context(), &body); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "conversation not found")
		return
	}
	harness, prompt, spec := runSpec(row.Path, body.Harness, body.PromptOverride)
	prompt, wake := s.launchPrompt(r.Context(), body, prompt)
	if body.DryRun {
		// B25: dry_run evaluates schedule when: gates without launching.
		// An operator dry run previews the composed wake prompt, not the raw body.
		if wake != nil {
			if composed, err := s.ws.ComposeWakePrompt(r.Context(), runs.WakeScope{
				WorkspaceID: body.Workspace, Kind: body.Kind, Name: body.Job,
			}, *wake); err == nil {
				prompt = composed
			}
		}
		s.handleRunsDryRun(w, r, body.Workspace, body.Job, body.Kind, reason, harness, prompt, body.Inputs)
		return
	}

	// One launch path: CreateAndLaunch (real tmux when binary present; harness=fake for CI).
	lopts := launchOpts(body, reason, harness, prompt, spec)
	lopts.Wake = wake
	run, _, err := s.runs.CreateAndLaunch(r.Context(), lopts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if body.Kind == runs.KindAssistant {
		s.log.Info("assistant.woken", "reason", string(reason), "channel", "")
	}
	if body.Kind == runs.KindAgent {
		s.log.Info("agent.woken", "reason", string(reason), "agent", body.Job)
	}
	if body.IdempotencyKey != "" {
		if _, err := s.st.RememberIdempotent(r.Context(), idempotencyRuns, body.IdempotencyKey, run.ID); err != nil {
			s.log.Warn("remember idempotency key", "run", run.ID, "error", err)
		}
	}
	if wait := parseWait(r); wait > 0 {
		if waited, werr := s.runs.WaitFor(r.Context(), run.ID, wait); werr == nil {
			run = waited
		}
	}
	writeJSON(w, http.StatusCreated, run)
}

// runForKey answers a repeated launch. ok reports that the caller must stop: it
// returns the first run, or nil when the lookup already wrote an error.
func (s *Server) runForKey(w http.ResponseWriter, r *http.Request, key string) (*runs.Run, bool) {
	id, found, err := s.st.Idempotent(r.Context(), idempotencyRuns, key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, true
	}
	if !found {
		return nil, false
	}
	run, err := s.runs.Get(r.Context(), id)
	if err != nil {
		return nil, false
	}
	return run, true
}

// isDraining reports drain state. The persisted flag keeps drain effective
// across an API restart.
func (s *Server) isDraining() bool {
	s.mu.RLock()
	draining := s.drain
	s.mu.RUnlock()
	if draining {
		return true
	}
	v, ok, _ := s.st.GetFlag("drain")
	return ok && v == "1"
}

// resolveRunRow is the A4 resolver: unknown job gives 404, a duplicate name
// without a workspace gives 400 (mapped by writeResolveErr).
func (s *Server) resolveRunRow(ctx context.Context, body runCreateRequest) (workspaces.JobRow, error) {
	if body.Kind == runs.KindAssistant {
		return s.findAssistantRow(ctx, body.Workspace)
	}
	if body.Kind == runs.KindAgent {
		return s.findAgentRow(ctx, body.Workspace, body.Job)
	}
	return s.findJobRow(ctx, body.Workspace, body.Job)
}

// runSpec parses the job file once and fills harness/prompt defaults.
func runSpec(path, harness, prompt string) (string, string, *job.Job) {
	var spec *job.Job
	if j, err := job.ParseFile(path); err == nil && j != nil {
		spec = j
	}
	if spec != nil {
		if harness == "" {
			harness = spec.Harness
		}
		if prompt == "" {
			prompt = spec.Prompt
		}
	}
	return harness, prompt, spec
}

// launchOpts builds the launch options: cwd and session knobs from the spec,
// plus the fake harness override for tests/e2e.
func launchOpts(body runCreateRequest, reason runs.RunReason, harness, prompt string, spec *job.Job) runs.LaunchOpts {
	lopts := runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID:     body.Workspace,
			Job:             body.Job,
			Kind:            body.Kind,
			Reason:          reason,
			Harness:         harness,
			PromptPreview:   prompt,
			Inputs:          body.Inputs,
			ConversationID:  body.ConversationID,
			ConversationKey: body.conversationKey,
		},
		Prompt: prompt,
	}
	if spec != nil {
		// Agents resolve to the workspace root unless working_dir: is set (G.5).
		d := spec.LaunchDefaults()
		lopts.WorkingDir = d.WorkingDir
		lopts.IfRunning = d.IfRunning
		lopts.Env = d.Env
		lopts.MaxDurationMS, lopts.IdleTimeoutMS, lopts.KeepAliveMS, lopts.NudgeMS = d.MaxDurationMS, d.IdleTimeoutMS, d.KeepAliveMS, d.NudgeMS
	}
	if harness == "fake" || os.Getenv("TREBI_FAKE_HARNESS") != "" {
		// Test/e2e harness: shell that emits the run-scoped completion sentinel.
		cmd := os.Getenv("TREBI_FAKE_HARNESS_CMD")
		if cmd == "" {
			cmd = `echo working; echo "$TREBI_RUN_COMPLETE"`
		}
		lopts.FakeCommand = []string{"sh", "-c", cmd}
		lopts.WorkingDir = os.TempDir()
	}
	return lopts
}

func (s *Server) bindConversation(ctx context.Context, body *runCreateRequest) error {
	if body.ConversationID == "" || s.conv == nil {
		return nil
	}
	conv, err := s.conv.Get(ctx, body.ConversationID)
	if err != nil {
		return err
	}
	body.conversationKey = conv.Key
	return nil
}

// launchPrompt resolves the prompt of one run (Part F.3): a full override
// wins; an operator run (assistant, agent) defers to the wake composer via
// CreateAndLaunch; extra_prompt appends to the job body for every other kind.
func (s *Server) launchPrompt(ctx context.Context, body runCreateRequest, base string) (string, *runs.WakeOpts) {
	if body.PromptOverride != "" {
		return body.PromptOverride, nil
	}
	operator := body.Kind == runs.KindAssistant || body.Kind == runs.KindAgent
	if !operator || s.ws == nil {
		if body.ExtraPrompt != "" {
			return request.AppendExtraPrompt(base, body.ExtraPrompt), nil
		}
		return base, nil
	}
	// Agents have no conversation, so the recap stays empty for them.
	recap := ""
	if body.ConversationID != "" && s.conv != nil {
		recap = s.conv.Recap(ctx, body.ConversationID, 10)
	}
	return "", &runs.WakeOpts{
		Body:        base,
		ExtraPrompt: body.ExtraPrompt,
		Recap:       recap,
		Message:     body.ExtraPrompt,
	}
}

// handleRunsCreate is the canonical launch (B9). The job and assistant run
// routes sugar over it.
func (s *Server) handleRunsCreate(w http.ResponseWriter, r *http.Request) {
	var body runCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	s.createRun(w, r, body)
}

// createRunFromJob is the one launch path. The body carries every launch
// field; the query keeps workspace and dry_run so a body-less POST still works.
func (s *Server) createRunFromJob(w http.ResponseWriter, r *http.Request, name string) {
	var body runCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	body.Job = name
	if body.Workspace == "" {
		body.Workspace = r.URL.Query().Get("workspace")
	}
	if r.URL.Query().Get("dry_run") == "true" {
		body.DryRun = true
	}
	s.createRun(w, r, body)
}

func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.WaitFor(r.Context(), r.PathValue("id"), parseWait(r))
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleRunTimeline(w http.ResponseWriter, r *http.Request) {
	tl, err := s.runs.Timeline(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tl)
}

// handleRunsKill kills every run named in ids.
func (s *Server) handleRunsKill(w http.ResponseWriter, r *http.Request) {
	ids, ok := bulkIDs(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, applyBulk(r.Context(), ids, s.runs.Kill))
}

func (s *Server) handleRunKill(w http.ResponseWriter, r *http.Request) {
	if err := s.runs.Kill(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "exited"})
}

// handleRunsDryRun evaluates when: gates on schedule triggers without launching (B25).
func (s *Server) handleRunsDryRun(w http.ResponseWriter, r *http.Request,
	workspace, jobName, kind string, reason runs.RunReason, harness, prompt string, inputs map[string]string,
) {
	ctx := r.Context()
	// Resolve job file through the A4 resolver.
	var j *job.Job
	var path string
	if row, err := s.findJobRow(ctx, workspace, jobName); err == nil {
		path = row.Path
		if parsed, perr := job.ParseFile(row.Path); perr == nil {
			j = parsed
		}
		if workspace == "" {
			workspace = row.WorkspaceID
		}
	}
	verdicts := []map[string]any{}
	fire := true
	var vars map[string]string
	if j != nil {
		if harness == "" {
			harness = j.Harness
		}
		if prompt == "" {
			prompt = j.Prompt
		}
		verdicts, fire, vars = scheduleVerdicts(ctx, j)
	}
	// Apply extracted vars to prompt for preview
	preview := prompt
	for k, v := range vars {
		preview = strings.ReplaceAll(preview, "{{"+k+"}}", v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dry_run":        true,
		"fire":           fire,
		"workspace":      workspace,
		"job":            jobName,
		"kind":           kind,
		"reason":         reason,
		"harness":        harness,
		"prompt_preview": truncStr(preview, 500),
		"inputs":         inputs,
		"path":           path,
		"when":           verdicts,
		"vars":           vars,
	})
}

// scheduleVerdicts evaluates the when: gate of each cron/every trigger.
func scheduleVerdicts(ctx context.Context, j *job.Job) (verdicts []map[string]any, fire bool, vars map[string]string) {
	verdicts = []map[string]any{}
	fire = true
	cwd := j.WorkingDir
	if cwd == "" {
		cwd = j.Dir
	}
	for _, tr := range j.Triggers {
		if tr.Family != "cron" && tr.Family != "every" {
			continue
		}
		if tr.When == nil {
			verdicts = append(verdicts, map[string]any{
				"family": tr.Family, "index": tr.Index, "fire": true, "skip": false,
				"note": "no when: gate",
			})
			continue
		}
		wr := job.EvalWhen(ctx, tr.When, cwd, j.Env)
		if wr.Vars != nil {
			vars = wr.Vars
		}
		verdicts = append(verdicts, whenVerdict(tr, wr))
		if wr.Skip || !wr.Fire || wr.Error != "" {
			fire = false
		}
	}
	return verdicts, fire, vars
}

// whenVerdict maps one when: evaluation to its wire object.
func whenVerdict(tr job.Trigger, wr job.WhenResult) map[string]any {
	v := map[string]any{
		"family": tr.Family, "index": tr.Index,
		"fire": wr.Fire, "skip": wr.Skip || !wr.Fire,
	}
	if wr.Error != "" {
		v["error"] = wr.Error
	}
	if wr.Vars != nil {
		v["vars"] = wr.Vars
	}
	if wr.Stdout != "" {
		v["stdout"] = truncStr(wr.Stdout, 500)
	}
	return v
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// handleRunAttach streams terminal bytes over WebSocket (D2).
// Protocol: client may send {"type":"input","data":"..."} or {"type":"take_control"}.
// Server sends {"type":"screen","data":"..."}.
func (s *Server) handleRunAttach(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if run.Harness == "native" || run.Runtime == "native" {
		writeErr(w, http.StatusNotFound, "not_found", "native runs have no attach")
		return
	}
	if run.TmuxSession == "" {
		writeErr(w, http.StatusBadRequest, "validation", "no tmux session")
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	done := make(chan struct{})
	go s.attachInputPump(conn, run.TmuxSession, done)
	s.attachScreenLoop(r.Context(), conn, run.TmuxSession, done)
}

// attachInputPump forwards client input messages into tmux. It closes done
// when the client read fails (disconnect).
func (s *Server) attachInputPump(conn *websocket.Conn, session string, done chan<- struct{}) {
	defer close(done)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(msg, &m) != nil {
			continue
		}
		switch fmt.Sprint(m["type"]) {
		case "input", "keys":
			_ = tmux.Send(session, fmt.Sprint(m["data"]))
		case "key":
			_ = tmux.SendKey(session, fmt.Sprint(m["data"]))
		case "take_control":
			// acknowledged — full TUI already live in tmux
		}
	}
}

// attachScreenLoop sends screen snapshots until the client disconnects or
// the tmux session ends.
func (s *Server) attachScreenLoop(ctx context.Context, conn *websocket.Conn, session string, done <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var last string
	sent := false
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			screen, err := tmux.ReadScreen(session)
			if err != nil {
				_ = conn.WriteJSON(map[string]any{"type": "error", "data": err.Error()})
				return
			}
			if !sent || screen != last {
				last = screen
				sent = true
				if err := conn.WriteJSON(map[string]any{"type": "screen", "data": screen}); err != nil {
					return
				}
			}
			if !tmux.HasSession(session) {
				_ = conn.WriteJSON(map[string]any{"type": "ended"})
				return
			}
		}
	}
}

func (s *Server) handleRunTranscript(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if run.Harness == "native" || run.Runtime == "native" {
		s.writeNativeTranscript(w, r, run)
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
	workingDir := tmux.TranscriptWorkingDir(run.Harness, run.WorkingDir, run.StartedAt)
	path, err := tool.TranscriptPath(home.HarnessDir(run.Harness), workingDir, run.SessionID)
	if err != nil {
		return nil, nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, false
	}
	return data, tool, true
}

func (s *Server) writeNativeTranscript(w http.ResponseWriter, r *http.Request, run *runs.Run) {
	path := native.TranscriptPath(run.ID)
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "no transcript")
		return
	}
	if r.URL.Query().Get("raw") == "1" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write(data)
		return
	}
	turns, err := native.ReadTurns(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID, "turns": turns})
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
// jobFileForRun resolves the markdown that produced this run. Assistant
// runs are TREBI.md (kind=assistant); agent runs are AGENT.md; findJobRow
// only returns JOB.md rows.
func (s *Server) jobFileForRun(ctx context.Context, prev *runs.Run) (workspaces.JobRow, error) {
	if prev.Kind == runs.KindAssistant {
		return s.findAssistantRow(ctx, prev.Workspace)
	}
	if prev.Kind == runs.KindAgent {
		return s.findAgentRow(ctx, prev.Workspace, prev.Job)
	}
	return s.findJobRow(ctx, prev.Workspace, prev.Job)
}

func (s *Server) launchOptsFromJob(ctx context.Context, prev *runs.Run, reason runs.RunReason) (runs.LaunchOpts, error) {
	row, err := s.jobFileForRun(ctx, prev)
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
	d := j.LaunchDefaults()
	lopts.WorkingDir = d.WorkingDir
	lopts.Env = d.Env
	lopts.IfRunning = d.IfRunning
	lopts.MaxDurationMS, lopts.IdleTimeoutMS, lopts.KeepAliveMS, lopts.NudgeMS = d.MaxDurationMS, d.IdleTimeoutMS, d.KeepAliveMS, d.NudgeMS
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
			Connector:       "retry",
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
	case runs.EndSessionGone, runs.EndLaunch, runs.EndSkip:
		// Fresh rerun. Skip never launched, so resume has no session.
		// Kill is a deliberate user act — it takes the default
		// resume-with-note path (backend-0808 §D7).
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
		Decision   string `json:"decision"` // allow|deny
		ApprovalID string `json:"approval_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.Decision == "" {
		writeErr(w, http.StatusBadRequest, "validation", "decision required (allow|deny)")
		return
	}
	a, err := s.runs.DecideApproval(r.Context(), r.PathValue("id"), body.ApprovalID, body.Decision)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "run or approval not found")
		return
	}
	if errors.Is(err, runs.ErrInvalidDecision) || errors.Is(err, runs.ErrApprovalRunMismatch) {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	cursor, ok := parseULIDCursor(w, r, "approval")
	if !ok {
		return
	}
	limit := parseLimit(r)
	state := r.URL.Query().Get("state")
	if state != "" && state != "pending" && state != "decided" {
		writeErr(w, http.StatusBadRequest, "validation", "state must be pending or decided")
		return
	}
	list, err := s.runs.ListApprovals(r.Context(), runs.ApprovalListOpts{
		Limit: limit, Cursor: cursor, State: state,
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

func (s *Server) handleAgentHook(w http.ResponseWriter, r *http.Request) {
	event := r.PathValue("event")
	runID := r.Header.Get("X-Trebi-Run-Id")
	if runID == "" {
		runID = r.URL.Query().Get("run_id")
	}
	body, _ := io.ReadAll(r.Body)
	if runID != "" {
		switch event {
		case "waiting", "permission-request":
			s.runs.SetSessionState(r.Context(), runID, runs.SessionWaiting, "hook")
			if _, err := s.runs.OpenApproval(r.Context(), runID, hookQuestion(body)); err != nil {
				s.log.Warn("open approval", "run", runID, "error", err)
			}
		case "running", "tool-start", "session-start":
			s.runs.SetSessionState(r.Context(), runID, runs.SessionRunning, "hook")
		case "stop", "session-end", "idle":
			s.runs.SetSessionState(r.Context(), runID, runs.SessionIdle, "hook")
		}
		s.runs.AddEvent(r.Context(), runID, "hook:"+event, "agent-hook", map[string]string{"event": event})
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// hookQuestion reads the prompt text a harness hook sends with a waiting event.
// The key differs per harness, so the first non-empty one wins. `trebi
// agent-hook` wraps the harness body in a payload string, so unwrap that first.
func hookQuestion(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if inner, ok := payload["payload"].(string); ok {
		if q := hookQuestion([]byte(inner)); q != "" {
			return q
		}
	}
	for _, key := range []string{"question", "prompt", "message", "text"} {
		if s, ok := payload[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func (s *Server) handleSessionState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.runs.SessionSnapshot()})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !runs.ValidRange(q.Get("range")) {
		writeErr(w, http.StatusBadRequest, "validation", "range must be 24h, 7d, 30d, or mtd")
		return
	}
	stats, err := s.runs.Stats(r.Context(), runs.StatsOpts{
		Workspace: s.workspaceID(r.Context(), q.Get("workspace")),
		Job:       q.Get("job"),
		Range:     q.Get("range"),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	total, running, err := s.runs.Counts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range": stats.Range, "totals": stats.Totals, "mtd": stats.MTD, "days": stats.Days,
		"total": total, "running": running,
	})
}
