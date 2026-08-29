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
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
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
	mux.HandleFunc("POST /v1/runs/{id}/resume", s.handleRunResume)
	mux.HandleFunc("POST /v1/runs/{id}/replay", s.handleRunReplay)
	mux.HandleFunc("POST /v1/runs/{id}/retry", s.handleRunRetry)
	mux.HandleFunc("POST /v1/runs/{id}/message", s.handleRunMessage)
	mux.HandleFunc("POST /v1/runs/{id}/approve", s.handleRunApprove)
	mux.HandleFunc("GET /v1/runs/{id}/attach", s.handleRunAttach)
}

func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	cursor := q.Get("cursor")
	if cursor != "" {
		if _, err := ulid.Parse(cursor); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "malformed cursor: not a run id")
			return
		}
	}
	list, err := s.runs.List(r.Context(), runs.ListOpts{
		Workspace: q.Get("workspace"),
		Job:       q.Get("job"),
		Status:    q.Get("status"),
		Kind:      q.Get("kind"),
		Harness:   q.Get("harness"),
		Model:     q.Get("model"),
		Limit:     limit,
		Cursor:    cursor,
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

func (s *Server) handleRunsCreate(w http.ResponseWriter, r *http.Request) {
	var body runCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	s.createRun(w, r, body)
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
	row, rerr := s.resolveRunRow(r.Context(), body)
	if rerr != nil {
		writeResolveErr(w, rerr)
		return
	}
	if body.Workspace == "" {
		body.Workspace = row.WorkspaceID
	}
	if err := s.bindConversation(r.Context(), &body); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "conversation not found")
		return
	}
	harness, prompt, spec := runSpec(row.Path, body.Harness, body.PromptOverride)
	prompt = s.assistantPrompt(r.Context(), body, prompt)
	if body.DryRun {
		// B25: dry_run evaluates schedule when: gates without launching.
		s.handleRunsDryRun(w, r, body.Workspace, body.Job, body.Kind, reason, harness, prompt, body.Inputs)
		return
	}

	// One launch path: CreateAndLaunch (real tmux when binary present; harness=fake for CI).
	run, _, err := s.runs.CreateAndLaunch(r.Context(), launchOpts(body, reason, harness, prompt, spec))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if body.Kind == runs.KindAssistant {
		s.log.Info("assistant.woken", "reason", string(reason), "channel", "")
	}
	writeJSON(w, http.StatusCreated, run)
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
	if body.Kind == "assistant" {
		return s.findAssistantRow(ctx, body.Workspace)
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
		lopts.WorkingDir = spec.WorkingDir
		if lopts.WorkingDir == "" {
			lopts.WorkingDir = spec.Dir
		}
		lopts.IfRunning = spec.Session.IfRunning
		lopts.MaxDurationMS, lopts.IdleTimeoutMS, lopts.KeepAliveMS, lopts.NudgeMS = spec.Session.KnobsMS()
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

func (s *Server) assistantPrompt(ctx context.Context, body runCreateRequest, base string) string {
	if body.PromptOverride != "" {
		return body.PromptOverride
	}
	if body.Kind != runs.KindAssistant || s.ws == nil {
		return base
	}
	recap := ""
	if body.ConversationID != "" && s.conv != nil {
		recap = s.conv.Recap(ctx, body.ConversationID, 10)
	}
	composed, err := s.ws.ComposeWakePrompt(ctx, body.Workspace, workspaces.WakeOpts{
		ExtraPrompt: body.ExtraPrompt,
		Recap:       recap,
		Message:     body.ExtraPrompt,
	})
	if err != nil || composed == "" {
		if body.ExtraPrompt != "" {
			return strings.TrimRight(base, "\n") + "\n" + body.ExtraPrompt
		}
		return base
	}
	return composed
}

func (s *Server) createRunFromJob(w http.ResponseWriter, r *http.Request, name string) {
	s.createRun(w, r, runCreateRequest{
		Job:       name,
		Workspace: r.URL.Query().Get("workspace"),
	})
}

func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.Get(r.Context(), r.PathValue("id"))
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
			if err := conn.WriteJSON(map[string]any{"type": "screen", "data": screen}); err != nil {
				return
			}
			if !tmux.HasSession(session) {
				_ = conn.WriteJSON(map[string]any{"type": "ended"})
				return
			}
		}
	}
}
