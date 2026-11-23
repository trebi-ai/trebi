package api

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/chains"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/launch"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/pauses"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/uploads"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// runCreateRequest is the POST /v1/runs body, shared with createRunFromJob.
type runCreateRequest struct {
	Workspace      string         `json:"workspace"`
	Job            string         `json:"job"`
	Kind           string         `json:"kind"`
	Reason         string         `json:"reason"`
	PromptOverride string         `json:"prompt_override"`
	ExtraPrompt    string         `json:"extra_prompt"`
	ConversationID string         `json:"conversation_id"`
	Inputs         map[string]any `json:"inputs"`
	Event          *runEventRef   `json:"event"`
	// Force skips the match stage of the trigger of Event.
	Force           bool   `json:"force"`
	DryRun          bool   `json:"dry_run"`
	Harness         string `json:"harness"`
	Runtime         string `json:"runtime,omitempty"` // cli|tui
	IdempotencyKey  string `json:"idempotency_key"`
	conversationKey string
	jobID           string
	// inputs is the prompt text of the filled inputs; trigger is the
	// stored event of a run from an event.
	inputs  map[string]string
	filled  *job.Filled
	trigger *runs.TriggerCapture
	// archived marks a dry run of an archived row.
	archived bool
}

// runEventRef names the stored event of a run that a person starts.
// RouteID picks the route; empty picks the first by-hand one.
type runEventRef struct {
	ID      string `json:"id"`
	RouteID string `json:"route_id"`
}

func (s *Server) routeRuns(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/runs", s.handleRunsList)
	mux.HandleFunc("POST /v1/runs", s.handleRunsCreate)
	mux.HandleFunc("GET /v1/runs/{id}", s.handleRunGet)
	mux.HandleFunc("GET /v1/runs/{id}/timeline", s.handleRunTimeline)
	mux.HandleFunc("GET /v1/runs/{id}/turns", s.handleRunTurns)
	mux.HandleFunc("GET /v1/runs/{id}/transcript", s.handleRunTranscript)
	mux.HandleFunc("GET /v1/runs/{id}/chapters", s.handleRunChapters)
	mux.HandleFunc("GET /v1/runs/{id}/log", s.handleRunLog)
	mux.HandleFunc("POST /v1/runs/{id}/kill", s.handleRunKill)
	mux.HandleFunc("POST /v1/runs/kill", s.handleRunsKill)
	mux.HandleFunc("POST /v1/runs/{id}/resume", s.handleRunResume)
	mux.HandleFunc("POST /v1/runs/{id}/replay", s.handleRunReplay)
	mux.HandleFunc("POST /v1/runs/{id}/retry", s.handleRunRetry)
	mux.HandleFunc("POST /v1/runs/{id}/mark", s.handleRunMark)
	mux.HandleFunc("POST /v1/runs/{id}/message", s.handleRunMessage)
	mux.HandleFunc("POST /v1/runs/{id}/approve", s.handleRunApprove)
	mux.HandleFunc("GET /v1/runs/{id}/attach", s.handleRunAttach)
	mux.HandleFunc("GET /v1/approvals", s.handleApprovals)
	mux.HandleFunc("GET /v1/runs/session-state", s.handleSessionState)
	mux.HandleFunc("GET /v1/stats/runs", s.handleStats)
	mux.HandleFunc("GET /v1/stats/activity", s.handleActivity)
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
	switch q.Get("kind") {
	case "", runs.KindJob, runs.KindAgent:
	default:
		// The assistant is a session, not a run (plan F.3).
		writeErr(w, http.StatusBadRequest, "validation",
			fmt.Sprintf("unknown kind %q: runs are job or agent; assistant sessions live on conversations", q.Get("kind")))
		return
	}
	list, err := s.runs.List(r.Context(), runs.ListOpts{
		Workspace:    s.workspaceID(r.Context(), q.Get("workspace")),
		Job:          q.Get("job"),
		Owner:        strings.TrimSpace(q.Get("owner")),
		Status:       q.Get("status"),
		Kind:         q.Get("kind"),
		Harness:      q.Get("harness"),
		Model:        q.Get("model"),
		Conversation: q.Get("conversation"),
		Trace:        q.Get("trace"),
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
	if row.Kind == "job" && job.OwnerOf(row.Name) == "" {
		writeErr(w, http.StatusBadRequest, "validation", job.ErrNoOwner.Error())
		return
	}
	if aerr := archivedLaunchErr(row); aerr != nil && !body.DryRun && !runs.RunAllowedWhenArchived(reason) {
		writeLaunchErr(w, aerr)
		return
	}
	body.archived = row.Archived()
	// The ref can be a short name or an ID; runs and wakes carry the index name.
	body.Job, body.jobID = row.Name, row.ID
	if body.Workspace == "" {
		body.Workspace = row.WorkspaceID
	}
	if err := s.bindConversation(r.Context(), &body); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "conversation not found")
		return
	}
	kit := s.ws.OwnerKit(r.Context(), body.Workspace, body.Job)
	harness, prompt, spec := runSpec(row.Path, body.Harness, body.PromptOverride, kit)
	if !s.fillRun(w, r, &body, spec) {
		return
	}
	mem := s.memoryLaunchFor(r.Context(), body.Workspace, spec)
	prompt, wake := s.launchPrompt(r.Context(), body, prompt)
	prompt = s.expandLaunchPrompt(r.Context(), body.Workspace, spec, prompt, body.promptVars())
	if wake != nil {
		wake.Body = s.expandLaunchPrompt(r.Context(), body.Workspace, spec, wake.Body, body.promptVars())
	}
	if body.DryRun {
		// B25: dry_run evaluates schedule when: gates without launching.
		// An operator dry run previews the composed wake prompt, not the raw body.
		if wake != nil {
			wakeDry := *wake
			wakeDry.Memory = mem
			if composed, err := s.ws.ComposeWakePrompt(r.Context(), runs.WakeScope{
				WorkspaceID: body.Workspace, Kind: body.Kind, Name: body.Job, OwnerID: body.jobID,
			}, wakeDry); err == nil {
				prompt = composed
			}
		}
		s.handleRunsDryRun(w, r, body, reason, harness, prompt)
		return
	}
	var budgetWarning string
	if reason == runs.ReasonManual && r.Header.Get(tools.HeaderRunToken) == "" && s.ws != nil {
		if ok, blockingReason, err := s.ws.Allow(r.Context(), body.Workspace, body.Job, body.Kind); err == nil && !ok {
			budgetWarning = blockingReason
		}
		if p := s.activePauses(r.Context()).Cover(pauses.Subject{WorkspaceID: body.Workspace, AgentID: row.AgentID(),
			Harness: harness, Kind: body.Kind}); p != nil {
			budgetWarning = strings.TrimSpace(budgetWarning + " " + p.Label + " is paused; this run starts because you started it.")
		}
	}

	// One launch path: CreateAndLaunch (a real pane when the binary is present; harness=fake for CI).
	lopts := launchOpts(body, reason, harness, prompt, spec, kit)
	lopts.JobSkills = spec.Skills
	lopts.JobConnectors = spec.Connectors
	lopts.Wake = wake
	lopts.Memory = mem
	lopts.Kit = kit
	caller, _ := tools.ActorFrom(r.Context())
	trace, path, terr := s.chains.LaunchTrace(r.Context(), &caller, body.Workspace, body.jobID)
	if terr != nil {
		writeChainErr(w, terr)
		return
	}
	lopts.TraceID, lopts.ChainPath = trace, path
	var hand *dispatch.QueueItem
	if body.trigger != nil {
		// The queue row of a hand run joins the event outcome.
		hand = lopts.QueueItem(dispatch.ConnectorManual)
		hand.ID = ulid.Make().String()
		hand.EventID, hand.RouteID, hand.Launch = body.trigger.EventID, body.trigger.RouteID, nil
		lopts.QueueID = hand.ID
	}
	run, outcome, err := s.runs.CreateAndLaunch(r.Context(), lopts)
	if errors.Is(err, runs.ErrRunActive) {
		s.queueLaunch(w, r, body, lopts)
		return
	}
	if err != nil {
		writeLaunchErr(w, err)
		return
	}
	if hand != nil {
		s.recordHandRun(r.Context(), body, hand, run.QueueResult(outcome), run.ID)
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
	if budgetWarning != "" {
		writeJSON(w, http.StatusCreated, map[string]any{
			"run": run, "warning": budgetWarning,
		})
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// queueLaunch puts a manual launch in the queue behind the live run of its
// job and answers 202. The queue lane launches it when the live run ends.
func (s *Server) queueLaunch(w http.ResponseWriter, r *http.Request, body runCreateRequest, lopts runs.LaunchOpts) {
	lopts.Runtime = body.Runtime
	item := lopts.QueueItem(dispatch.ConnectorManual)
	err := s.queue.Enqueue(r.Context(), item)
	if errors.Is(err, dispatch.ErrLaneFull) {
		writeQueueFull(w)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if body.IdempotencyKey != "" {
		if _, err := s.st.RememberIdempotent(r.Context(), idempotencyRuns, body.IdempotencyKey, queuedKeyPrefix+item.ID); err != nil {
			s.log.Warn("remember idempotency key", "queue_id", item.ID, "error", err)
		}
	}
	if t := body.trigger; t != nil {
		s.connectors.RecordHandRun(r.Context(), t.EventID, body.Workspace, body.jobID, t.RouteID, item.ID)
	}
	writeQueued(w, item.ID)
}

// recordHandRun writes the finished queue row of a run that a person started
// from an event, and points the event outcome at it.
func (s *Server) recordHandRun(ctx context.Context, body runCreateRequest, item *dispatch.QueueItem, result dispatch.QueueResult, runID string) {
	if err := s.queue.RecordDone(ctx, item, result, runID); err != nil {
		s.log.Warn("hand run queue row", "run", runID, "error", err)
		return
	}
	s.connectors.RecordHandRun(ctx, item.EventID, body.Workspace, body.jobID, item.RouteID, item.ID)
}

// fillRun fills the typed inputs of a job run: the given values, then the
// stored event when body names one, then the defaults. It writes the error
// and returns false when the event or the values do not fit. A dry run
// shows the empty inputs instead of an error.
func (s *Server) fillRun(w http.ResponseWriter, r *http.Request, body *runCreateRequest, spec *job.Job) bool {
	if spec == nil || body.Kind == runs.KindAgent {
		if body.Event != nil {
			writeErr(w, http.StatusBadRequest, "validation", "event applies only to a job")
			return false
		}
		body.inputs = textInputs(body.Inputs)
		return true
	}
	var tr *job.Trigger
	var vars *job.EventVars
	if ref := body.Event; ref != nil {
		ev, err := s.connectors.StoredGateEvent(r.Context(), ref.ID)
		if err != nil {
			s.writeEventErr(w, err)
			return false
		}
		t, res, err := s.connectors.HandRoute(r.Context(), ev, body.Workspace, body.Job, ref.RouteID, body.Force)
		if err != nil {
			writeErrDetails(w, http.StatusUnprocessableEntity, "event_not_accepted", err.Error(),
				map[string]any{"stage": res.Stage, "error": res.Error})
			return false
		}
		tr = &t
		evVars := ev.Vars(res.Vars)
		vars = &job.EventVars{Vars: evVars, Payload: ev.Payload}
		body.trigger = &runs.TriggerCapture{EventID: ev.ID, Connector: ev.Instance, RouteID: t.Route,
			Vars: evVars, Source: runs.TriggerManual}
	}
	f := spec.Fill(tr, vars, body.Inputs)
	body.filled, body.inputs = &f, f.Text()
	if body.DryRun || (len(f.Missing) == 0 && len(f.Errors) == 0) {
		return true
	}
	fields := map[string]string{}
	routes, _ := s.ws.RoutesOf(r.Context(), body.Workspace, body.Job)
	list := spec.InputList(tr != nil || len(routes) > 0)
	for _, name := range f.Missing {
		in, _ := list.Get(name)
		fields[name] = in.Title() + " is required"
	}
	maps.Copy(fields, f.Errors)
	writeErrDetails(w, http.StatusUnprocessableEntity, "invalid", f.Problem(list), map[string]any{"fields": fields})
	return false
}

// promptVars are the vars that fill the prompt: the event vars, then the
// inputs, which win.
func (b runCreateRequest) promptVars() map[string]string {
	if b.trigger == nil {
		return b.inputs
	}
	out := maps.Clone(b.trigger.Vars)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, b.inputs)
	return out
}

// textInputs is the prompt text of each given value.
func textInputs(in map[string]any) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = job.RenderValue(v)
	}
	return out
}

// queuedKeyPrefix marks an idempotency value that names a queue row, not a run.
const queuedKeyPrefix = "queue:"

func writeQueued(w http.ResponseWriter, queueID string) {
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "queue_id": queueID})
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
	if qid, ok := strings.CutPrefix(id, queuedKeyPrefix); ok {
		writeQueued(w, qid)
		return nil, true
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
	if body.Kind == runs.KindAgent {
		return s.findAgentRow(ctx, body.Workspace, body.Job)
	}
	return s.findJobRow(ctx, body.Workspace, body.Job)
}

// runSpec parses the job file once and fills harness/prompt defaults. The
// harness comes from the job, then the owner agent, then the config.
func runSpec(path, harness, prompt string, kit *job.KitYAML) (string, string, *job.Job) {
	var spec *job.Job
	if j, err := job.ParseFile(path); err == nil && j != nil {
		spec = j
	}
	if spec != nil {
		if harness == "" {
			harness = spec.ResolvedHarness(kit)
		}
		if prompt == "" {
			prompt = spec.Prompt
		}
	}
	return harness, prompt, spec
}

// expandLaunchPrompt fills {{job_dir}}/{{workspace_dir}} and caller inputs.
// Operator wakes still go through the composer for {{agent_dir}}.
func (s *Server) expandLaunchPrompt(ctx context.Context, wsID string, spec *job.Job, prompt string, extra map[string]string) string {
	jobDir, wsPath := "", ""
	if spec != nil {
		jobDir = spec.Dir
	}
	if s.ws != nil && wsID != "" {
		if ws, err := s.ws.Get(ctx, wsID); err == nil {
			wsPath = ws.Path
		}
	}
	return job.ExpandPrompt(prompt, jobDir, wsPath, extra)
}

// launchOpts builds the launch options: cwd and session knobs from the spec,
// plus the fake harness override for tests/e2e.
func launchOpts(body runCreateRequest, reason runs.RunReason, harness, prompt string, spec *job.Job, kit *job.KitYAML) runs.LaunchOpts {
	lopts := runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID:     body.Workspace,
			JobID:           body.jobID,
			JobName:         body.Job,
			Kind:            body.Kind,
			Reason:          reason,
			Harness:         harness,
			PromptPreview:   prompt,
			Inputs:          body.inputs,
			Trigger:         body.trigger,
			ConversationID:  body.ConversationID,
			ConversationKey: body.conversationKey,
		},
		Prompt: prompt,
	}
	if spec != nil {
		// Agents resolve to the workspace root unless working_dir: is set (G.5).
		d := spec.LaunchDefaultsWithKit(kit)
		lopts.WorkingDir = d.WorkingDir
		lopts.IfRunning = d.IfRunning
		lopts.Env, lopts.FolderEnv = d.Env, d.FolderEnv
		lopts.Model, lopts.Effort = d.Model, d.Effort
		lopts.MaxDurationMS, lopts.IdleTimeoutMS, lopts.KeepAliveMS, lopts.NudgeMS = d.MaxDurationMS, d.IdleTimeoutMS, d.KeepAliveMS, d.NudgeMS
		lopts.Runtime = spec.Runtime
		lopts.StateDecl = spec.State
	}
	if body.Runtime != "" {
		lopts.Runtime = body.Runtime
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
	operator := body.Kind == runs.KindAgent
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
	row, ok := s.resolveJob(w, r, name)
	if !ok {
		return
	}
	if !s.requireAgentJobScope(w, r, row) {
		return
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
	rc, err := s.chains.ForRun(r.Context(), run.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*runs.Run
		Chain *chains.RunChain `json:"chain"`
	}{run, rc})
}

func (s *Server) handleRunTimeline(w http.ResponseWriter, r *http.Request) {
	tl, err := s.runs.Timeline(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tl)
}

func (s *Server) handleRunTurns(w http.ResponseWriter, r *http.Request) {
	turns, err := s.runs.Turns(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	hops, err := s.chains.TurnHops(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	type chainTurn struct {
		runs.Turn
		Hop *chains.HopRef `json:"hop"`
	}
	out := make([]chainTurn, len(turns))
	for i, t := range turns {
		out[i].Turn = t
		if h, ok := hops[t.HopID]; ok {
			out[i].Hop = &h
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": out})
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
	if !s.requireAgentRunScope(w, r, r.PathValue("id")) {
		return
	}
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
func (s *Server) handleRunsDryRun(w http.ResponseWriter, r *http.Request, body runCreateRequest, reason runs.RunReason, harness, prompt string) {
	ctx := r.Context()
	workspace, jobName, kind := body.Workspace, body.Job, body.Kind
	// Resolve job file through the A4 resolver.
	var j *job.Job
	var path, jobID string
	if row, err := s.findJobRow(ctx, workspace, jobName); err == nil {
		path, jobID = row.Path, row.ID
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
		kit := s.ws.OwnerKit(ctx, workspace, jobName)
		if harness == "" {
			harness = j.ResolvedHarness(kit)
		}
		if prompt == "" {
			prompt = j.Prompt
		}
		verdicts, fire, vars = scheduleVerdicts(ctx, j, kit)
	}
	// Apply builtin path vars, when: extract, and caller inputs for preview.
	extra := map[string]string{}
	for k, v := range vars {
		extra[k] = v
	}
	maps.Copy(extra, body.promptVars())
	preview := s.expandLaunchPrompt(ctx, workspace, j, prompt, extra)
	out := map[string]any{
		"dry_run":        true,
		"fire":           fire,
		"workspace":      workspace,
		"job_id":         jobID,
		"job_name":       jobName,
		"kind":           kind,
		"reason":         reason,
		"harness":        harness,
		"prompt_preview": truncStr(preview, 500),
		"inputs":         body.filledWire(),
		"path":           path,
		"when":           verdicts,
		"vars":           vars,
	}
	if body.filled != nil {
		out["missing"] = body.filled.Missing
		if len(body.filled.Errors) > 0 {
			out["errors"] = body.filled.Errors
		}
	}
	if t := body.trigger; t != nil {
		out["trigger"] = t
	}
	if body.archived {
		out["warning"] = "archived"
	}
	writeJSON(w, http.StatusOK, out)
}

// filledWire is the dry run shape of the inputs: {name: {value, source}}.
func (b runCreateRequest) filledWire() map[string]job.FilledValue {
	if b.filled != nil {
		return b.filled.Wire()
	}
	out := map[string]job.FilledValue{}
	for k, v := range b.Inputs {
		out[k] = job.FilledValue{Value: v, Source: job.SourceGiven}
	}
	return out
}

// scheduleVerdicts evaluates the when: gate of each cron/every trigger.
func scheduleVerdicts(ctx context.Context, j *job.Job, kit *job.KitYAML) (verdicts []map[string]any, fire bool, vars map[string]string) {
	verdicts = []map[string]any{}
	fire = true
	cwd, _ := j.ResolvedWorkingDir(kit)
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
// CLI-runtime runs stream normalized wire events instead
// ({"type":"event","data":{...}} + periodic {"type":"status","data":{...}}).
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
	if run.Runtime == "cli" || (run.Attach != nil && run.Attach.Kind == "events") {
		s.attachEventStream(w, r, run)
		return
	}
	if run.PaneSession == "" {
		writeErr(w, http.StatusBadRequest, "validation", "the run has no pane")
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	done := make(chan struct{})
	go s.attachInputPump(r.Context(), conn, run.PaneSession, done)
	s.attachScreenLoop(r.Context(), conn, run.PaneSession, done)
}

// attachEventStream is the CLI-runtime attach: replay + tail of normalized
// run events with a periodic status frame (Part H). No pane is opened.
func (s *Server) attachEventStream(w http.ResponseWriter, r *http.Request, run *runs.Run) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ctx := r.Context()
	done := make(chan struct{})
	go s.attachEventInput(conn, run.ID, done)

	// Seed with the tail of the log so the client is not empty-handed.
	if text, _ := s.runLogText(ctx, run.ID); text != "" {
		_ = conn.WriteJSON(map[string]any{"type": "event", "data": map[string]string{
			"kind": "log", "text": strings.Join(lastLogLines(text, 40), "\n"),
		}})
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var lastID int64
	var lastState runs.SessionState
	var lastStatusSent time.Time
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			events, err := s.runs.EventsSince(ctx, run.ID, lastID, 200)
			if err != nil {
				_ = conn.WriteJSON(map[string]any{"type": "error", "data": err.Error()})
				return
			}
			for _, ev := range events {
				lastID = ev.ID
				if err := conn.WriteJSON(map[string]any{"type": "event", "data": ev}); err != nil {
					return
				}
			}
			cur, err := s.runs.Get(ctx, run.ID)
			if err != nil {
				_ = conn.WriteJSON(map[string]any{"type": "ended"})
				return
			}
			// Periodic status frame: state, last output, in-flight tool.
			if cur.SessionState != lastState || time.Since(lastStatusSent) >= 5*time.Second {
				lastState = cur.SessionState
				lastStatusSent = time.Now()
				status := map[string]any{"state": string(cur.SessionState)}
				if lastID > 0 {
					if latest, lerr := s.runs.EventsSince(ctx, run.ID, lastID-1, 1); lerr == nil && len(latest) > 0 {
						status["last_event_at"] = latest[0].At
					}
				}
				if tool := s.lastToolName(ctx, run.ID); tool != "" {
					status["tool"] = tool
				}
				_ = conn.WriteJSON(map[string]any{"type": "status", "data": status})
			}
			if cur.Status.IsTerminal() {
				// One more sweep so the final result frame is delivered.
				if events, err := s.runs.EventsSince(ctx, run.ID, lastID, 200); err == nil {
					for _, ev := range events {
						_ = conn.WriteJSON(map[string]any{"type": "event", "data": ev})
					}
				}
				_ = conn.WriteJSON(map[string]any{"type": "ended"})
				return
			}
		}
	}
}

// attachEventInput accepts follow-up messages on an event attach; key/input
// frames are ignored because there is no pane.
func (s *Server) attachEventInput(conn *websocket.Conn, runID string, done chan<- struct{}) {
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
		if fmt.Sprint(m["type"]) == "message" {
			if _, err := s.runs.SendMessage(context.Background(), runID, fmt.Sprint(m["data"])); err != nil {
				_ = conn.WriteJSON(map[string]any{"type": "error", "data": err.Error()})
			}
		}
	}
}

// lastToolName returns the most recent tool name from the run timeline.
func (s *Server) lastToolName(ctx context.Context, runID string) string {
	tl, err := s.runs.Timeline(ctx, runID)
	if err != nil {
		return ""
	}
	for i := len(tl.Events) - 1; i >= 0; i-- {
		if tl.Events[i].Kind != "tool" {
			continue
		}
		var d map[string]string
		if json.Unmarshal(tl.Events[i].Detail, &d) == nil {
			return d["name"]
		}
	}
	return ""
}

// attachInputPump forwards client input messages into the pane. It closes
// done when the client read fails (disconnect).
func (s *Server) attachInputPump(ctx context.Context, conn *websocket.Conn, session string, done chan<- struct{}) {
	panes := s.runs.Panes()
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
			_ = panes.Send(ctx, session, fmt.Sprint(m["data"]))
		case "key":
			_ = panes.Key(ctx, session, fmt.Sprint(m["data"]))
		case "take_control":
			// acknowledged — the full TUI is already live in the pane
		}
	}
}

// attachScreenLoop sends screen snapshots until the client disconnects or
// the pane ends.
func (s *Server) attachScreenLoop(ctx context.Context, conn *websocket.Conn, session string, done <-chan struct{}) {
	panes := s.runs.Panes()
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
			screen, err := panes.Screen(ctx, session)
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
			if !panes.Alive(ctx, session) {
				_ = conn.WriteJSON(map[string]any{"type": "ended"})
				return
			}
		}
	}
}

// errNoTranscript means that a run has no transcript source.
var errNoTranscript = errors.New("no transcript")

func (s *Server) handleRunTranscript(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if r.URL.Query().Get("raw") == "1" {
		if _, live := s.cliTranscript(r.Context(), run); !live {
			if data, _, found := s.readTranscript(run); found {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write(data)
				return
			}
		}
	}
	turns, err := s.transcriptTurns(r.Context(), run)
	switch {
	case errors.Is(err, errNoTranscript):
		writeErr(w, http.StatusNotFound, "not_found", "no transcript")
		return
	case errors.Is(err, agentcli.ErrTranscriptUnsupported):
		writeErr(w, http.StatusNotFound, "not_found", "transcript unsupported")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if turns, err = s.runs.WithRequests(r.Context(), run.ID, turns); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := s.runs.LabelTurns(r.Context(), run.ID, turns); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID, "turns": turns})
}

// transcriptTurns reads the turns of a run: the wire events of a CLI run
// first, then the vendor session file. It returns errNoTranscript when no
// source exists, and agentcli.ErrTranscriptUnsupported when the harness has
// no parser.
func (s *Server) transcriptTurns(ctx context.Context, run *runs.Run) ([]agentcli.Turn, error) {
	if turns, ok := s.cliTranscript(ctx, run); ok {
		return turns, nil
	}
	data, tool, found := s.readTranscript(run)
	if !found {
		return nil, errNoTranscript
	}
	return tool.ParseTranscript(bytes.NewReader(data))
}

func (s *Server) handleRunChapters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	run, err := s.runs.Get(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if run.PrunedAt != nil {
		writeErr(w, http.StatusGone, "pruned", "the details of this run were removed: its job keeps only its newest runs (keep_runs)")
		return
	}
	turns, err := s.transcriptTurns(ctx, run)
	if err != nil && !errors.Is(err, errNoTranscript) && !errors.Is(err, agentcli.ErrTranscriptUnsupported) {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	notices, err := s.runNotices(ctx, run.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	ch, err := s.runs.Chapters(ctx, run.ID, turns, notices)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

// runNotices lists what Trebi sent to a person for the run: its inbox items,
// and its outbox rows that no inbox item owns.
func (s *Server) runNotices(ctx context.Context, runID string) ([]runs.Notice, error) {
	notices, err := s.alerts.RunNotices(ctx, runID)
	if err != nil {
		return nil, err
	}
	rows, _, err := s.outbox.List(ctx, outbox.Filter{RunIDs: []string{runID}, Limit: 200})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.InboxID != "" || row.Status == outbox.StatusCanceled {
			continue
		}
		at := row.CreatedAt
		if row.SentAt != nil {
			at = *row.SentAt
		}
		notices = append(notices, runs.Notice{
			Kind: runs.NoticeMail, At: at, Title: row.Subject, Status: string(row.Status),
			Deliveries: []runs.NoticeDelivery{{Via: cmp.Or(row.Instance, string(row.Kind)), Status: string(row.Status), SentAt: row.SentAt}},
		})
	}
	return notices, nil
}

// cliTranscript reads a CLI run from the live wire events (Part H), then
// from OpenCode serve. The caller falls back to the vendor session file.
func (s *Server) cliTranscript(ctx context.Context, run *runs.Run) ([]agentcli.Turn, bool) {
	if run.Runtime != "cli" {
		return nil, false
	}
	if turns, err := s.runs.CLITurns(ctx, run.ID); err == nil && len(turns) > 0 {
		return turns, true
	}
	if run.Harness == agentcli.OpenCode && run.SessionID != "" {
		if raw, ok := s.runs.OpenCodeMessages(ctx, run.SessionID); ok {
			if turns, err := agentcli.ParseOpenCodeMessages(raw); err == nil {
				return turns, true
			}
		}
	}
	return nil, false
}

func (s *Server) readTranscript(run *runs.Run) ([]byte, agentcli.Tool, bool) {
	if run.SessionID == "" {
		return nil, nil, false
	}
	tool, ok := agentcli.ByName(run.Harness)
	if !ok {
		return nil, nil, false
	}
	workingDir := agentcli.TranscriptWorkingDir(run.Harness, run.WorkingDir, run.StartedAt)
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

const (
	runLogFollowLines    = 100
	runLogFollowInterval = 500 * time.Millisecond
	runLogPageLines      = 200
	runLogPageMax        = 2000
)

func (s *Server) handleRunLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	follow := r.URL.Query().Get("follow") == "1"
	text, source := s.runLogText(r.Context(), id)
	if !follow {
		if text == "" {
			if run, err := s.runs.Get(r.Context(), id); err == nil && run.PrunedAt != nil {
				writeErr(w, http.StatusGone, "pruned", "the details of this run were removed: its job keeps only its newest runs (keep_runs)")
				return
			}
			writeErr(w, http.StatusNotFound, "not_found", "log not found")
			return
		}
		all := lastLogLines(text, 0)
		if beforeQ := r.URL.Query().Get("before"); beforeQ != "" {
			before, err := strconv.Atoi(beforeQ)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "validation", "before must be an integer")
				return
			}
			n, _ := strconv.Atoi(r.URL.Query().Get("n"))
			chunk, offset, total := sliceLogLines(text, before, n)
			writeJSON(w, http.StatusOK, map[string]any{
				"log":    strings.Join(chunk, "\n"),
				"source": source,
				"offset": offset,
				"total":  total,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"log": text, "source": source, "offset": 0, "total": len(all),
		})
		return
	}

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	// Headers must go out before the first log line, or an empty file + follow
	// holds the connection with no response and proxies time out as 502.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	prev := text
	snap := lastLogLines(text, runLogFollowLines)
	total := len(lastLogLines(text, 0))
	offset := total - len(snap)
	if offset < 0 {
		offset = 0
	}
	fmt.Fprintf(w, "event: ready\ndata: {\"offset\":%d,\"total\":%d}\n\n", offset, total)
	flush()
	writeSSELogLines(w, flush, snap)
	if !s.runLogLive(r.Context(), id) {
		return
	}

	t := time.NewTicker(runLogFollowInterval)
	defer t.Stop()
	idle := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			cur, _ := s.runLogText(r.Context(), id)
			if extra := logSuffixLines(prev, cur); len(extra) > 0 {
				writeSSELogLines(w, flush, extra)
			}
			prev = cur
			if s.runLogLive(r.Context(), id) {
				idle = 0
				continue
			}
			idle++
			if idle >= 2 {
				return
			}
		}
	}
}

func (s *Server) runLogText(ctx context.Context, id string) (text, source string) {
	path := filepath.Join(home.RunsLogDir(), id+".log")
	if data, err := os.ReadFile(path); err == nil {
		return string(data), "file"
	}
	run, err := s.runs.Get(ctx, id)
	if err != nil || run.PaneSession == "" {
		return "", ""
	}
	if hist, herr := s.runs.Panes().History(ctx, run.PaneSession); herr == nil {
		return hist, "pane"
	}
	if screen, rerr := s.runs.Panes().Screen(ctx, run.PaneSession); rerr == nil {
		return screen, "pane"
	}
	return "", ""
}

func (s *Server) runLogLive(ctx context.Context, id string) bool {
	run, err := s.runs.Get(ctx, id)
	return err == nil && run.Status.IsLive()
}

func sliceLogLines(s string, before, n int) (chunk []string, offset, total int) {
	lines := lastLogLines(s, 0)
	total = len(lines)
	if n <= 0 {
		n = runLogPageLines
	}
	if n > runLogPageMax {
		n = runLogPageMax
	}
	if before < 0 || before > total {
		before = total
	}
	offset = before - n
	if offset < 0 {
		offset = 0
	}
	return lines[offset:before], offset, total
}

func lastLogLines(s string, n int) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if n > 0 && len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

func logSuffixLines(prev, next string) []string {
	if next == "" || next == prev {
		return nil
	}
	if prev == "" {
		return lastLogLines(next, runLogFollowLines)
	}
	if strings.HasPrefix(next, prev) {
		return lastLogLines(next[len(prev):], 0)
	}
	return nil
}

func writeSSELogLines(w http.ResponseWriter, flush func(), lines []string) {
	for _, line := range lines {
		fmt.Fprintf(w, "data: %s\n\n", line)
		flush()
	}
}

func (s *Server) handleRunResume(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentRunScope(w, r, r.PathValue("id")) {
		return
	}
	run, err := s.launch.Resume(r.Context(), r.PathValue("id"), runs.ResumeOpts{Reason: runs.ReasonResume})
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeErr(w, http.StatusNotFound, "not_found", "run not found")
		case errors.Is(err, launch.ErrJobMissing):
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, runs.ErrRunActive):
			writeErr(w, http.StatusConflict, "conflict", "the run is still active")
		case errors.Is(err, runs.ErrJobArchived):
			writeErr(w, http.StatusConflict, "archived", runs.JobArchivedMessage(err))
		default:
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// memoryLaunchFor resolves the MEMORY.md seam for a manual launch. Nil = off.
func (s *Server) memoryLaunchFor(ctx context.Context, wsID string, spec *job.Job) *runs.MemoryLaunch {
	if spec == nil || !spec.Memory || s.ws == nil {
		return nil
	}
	ws, err := s.ws.Get(ctx, wsID)
	if err != nil {
		return nil
	}
	return runs.NewMemoryLaunch(spec, ws.Path)
}

func (s *Server) handleRunReplay(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentRunScope(w, r, r.PathValue("id")) {
		return
	}
	prev, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	// A replay rebuilds the prompt from the job file plus the captured
	// trigger vars/inputs — prompt_preview is truncated and not a prompt.
	lopts, err := s.launch.FromRun(r.Context(), prev, runs.ReasonReplay)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "job file for run no longer resolvable")
		return
	}
	run, _, err := s.runs.CreateAndLaunch(r.Context(), lopts)
	if err != nil {
		writeLaunchErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// writeLaunchErr maps a CreateAndLaunch error to its status and code.
func writeLaunchErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runs.ErrRunActive):
		writeErr(w, http.StatusConflict, "conflict", "a run of this job is still active; wait for it to finish")
	case errors.Is(err, runs.ErrPaused):
		writeErr(w, http.StatusConflict, "paused", err.Error())
	case errors.Is(err, runs.ErrBudgetExceeded):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, runs.ErrWorkspaceArchived):
		writeErr(w, http.StatusConflict, "archived", runs.ArchivedMessage(err))
	case errors.Is(err, runs.ErrJobArchived):
		writeErr(w, http.StatusConflict, "archived", runs.JobArchivedMessage(err))
	case errors.Is(err, runs.ErrActionsInvalid) || errors.Is(err, runs.ErrActionsUnsupported):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.Is(err, runs.ErrResumeUnsupported) || errors.Is(err, runs.ErrNoSession):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// writeQueueFull answers a launch or an event that finds its queue full.
func writeQueueFull(w http.ResponseWriter) {
	writeErr(w, http.StatusTooManyRequests, "queue_full",
		fmt.Sprintf("%d runs of this job are already waiting", dispatch.LaneCap))
}

func (s *Server) handleRunRetry(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentRunScope(w, r, r.PathValue("id")) {
		return
	}
	// D7: policy dispatch on end_reason.
	// - agent/logic failures → resume with failure note
	// - auth/limit → park (blocked resume intent)
	// - session_gone / infra → fresh rerun (replay-like)
	// - restart → resume with the restart prompt
	prev, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	// Retry continues a failure; a run that finished its job has no failure to
	// continue. Replay is the fresh-run action for it.
	if prev.Status == runs.StatusComplete && (prev.EndReason == runs.EndResult || prev.EndReason == runs.EndSentinel) {
		writeErr(w, http.StatusConflict, "conflict", "run finished successfully; replay it for a fresh run")
		return
	}
	if row, err := s.ws.JobByID(r.Context(), prev.JobID); err == nil {
		if aerr := archivedLaunchErr(*row); aerr != nil {
			writeLaunchErr(w, aerr)
			return
		}
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
			JobID:           prev.JobID,
			JobName:         prev.JobName,
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
	case runs.EndRestart:
		// A restart stopped the turn: continue the session, never a fresh run.
		run, err := s.launch.Resume(r.Context(), prev.ID, runs.ResumeOpts{Reason: runs.ReasonResume, Prompt: runs.RestartPrompt})
		if err != nil {
			writeLaunchErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"action": "resume", "run": run})
		return
	case runs.EndSessionGone, runs.EndLaunch, runs.EndSkip, runs.EndConnectorLogin:
		// Fresh rerun. Skip and a missing login never launched, so resume has no session.
		// Kill is a deliberate user act — it takes the default
		// resume-with-note path (backend-0808 §D7).
		s.handleRunReplay(w, r)
		return
	default:
		// Agent/logic: resume with failure note
		note := "Retry after failure (end_reason=" + string(prev.EndReason) + "). Continue or fix the issue."
		if prev.SessionID != "" {
			// The note is the prompt of the resumed turn.
			run, err := s.launch.Resume(r.Context(), prev.ID, runs.ResumeOpts{Reason: runs.ReasonResume, Prompt: note})
			if err == nil && run != nil {
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
		lopts, err := s.launch.FromRun(r.Context(), prev, runs.ReasonResume)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "job file for run no longer resolvable")
			return
		}
		lopts.PreviousRunID = prev.ID
		lopts.ResumeAttempts = prev.ResumeAttempts + 1
		lopts.Prompt = note + "\n\n" + lopts.Prompt
		lopts.PromptPreview = lopts.Prompt
		run, _, err := s.runs.CreateAndLaunch(r.Context(), lopts)
		if err != nil {
			writeLaunchErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"action": "rerun_with_note", "run": run})
	}
}

func (s *Server) handleRunMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentRunScope(w, r, r.PathValue("id")) {
		return
	}
	var body struct {
		Text        string   `json:"text"`
		Attachments []string `json:"attachments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.Text == "" && len(body.Attachments) == 0) {
		writeErr(w, http.StatusBadRequest, "validation", "text or attachments required")
		return
	}
	var atts uploads.Attachments
	if len(body.Attachments) > 0 {
		run, err := s.runs.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		var ok bool
		if atts, ok = s.attachUploads(r.Context(), w, run.Workspace, body.Attachments); !ok {
			return
		}
	}
	res, err := s.runs.SendMessage(r.Context(), r.PathValue("id"), body.Text, atts...)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
	case errors.Is(err, runs.ErrRunActive):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, runs.ErrResumeUnsupported), errors.Is(err, runs.ErrNoSession):
		writeErr(w, http.StatusConflict, "resume_unsupported", err.Error())
	case errors.Is(err, runs.ErrSentinelInMessage):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

func (s *Server) handleRunMark(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentRunScope(w, r, r.PathValue("id")) {
		return
	}
	var body struct {
		Status string `json:"status"` // complete|failed|skipped (success accepted)
		Note   string `json:"note"`
		Clear  bool   `json:"clear"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	status := runs.OutcomeStatus(body.Status)
	if !body.Clear && status == "" {
		writeErr(w, http.StatusBadRequest, "validation",
			"status must be success|complete|failed|skipped, or clear=true")
		return
	}
	if body.Clear {
		status = ""
	}
	run, err := s.runs.Mark(r.Context(), runs.MarkOpts{
		ID: r.PathValue("id"), Status: status, Note: body.Note, Clear: body.Clear,
	})
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	case errors.Is(err, runs.ErrBadOutcome):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	case errors.Is(err, runs.ErrNotTerminal):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
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
		Workspace: s.workspaceID(r.Context(), r.URL.Query().Get("workspace")),
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
	if actor, ok := tools.ActorFrom(r.Context()); ok && actor.RunID != "" && actor.RunID != runID {
		writeForbidden(w, "run token may post only its own hook")
		return
	}
	body, _ := io.ReadAll(r.Body)
	if runID != "" {
		// Wire events are authoritative on the subprocess runtime (A.14,
		// G.2): hooks never flip session state and never open approvals
		// there. The wire opens a keyed approval for each real permission; a
		// vendor notification for a call that the policy allows is not one.
		advisory := false
		if run, err := s.runs.Get(r.Context(), runID); err == nil && run.Runtime == "cli" {
			advisory = true
		}
		switch event {
		case "waiting", "permission-request":
			if !advisory {
				s.runs.SetSessionState(r.Context(), runID, runs.SessionWaiting, "hook")
				if _, err := s.runs.OpenApproval(r.Context(), runID, hookQuestion(body)); err != nil {
					s.log.Warn("open approval", "run", runID, "error", err)
				}
			}
		case "running", "tool-start", "session-start":
			if !advisory {
				s.runs.SetSessionState(r.Context(), runID, runs.SessionRunning, "hook")
			}
		case "stop", "session-end", "idle":
			if !advisory {
				s.runs.SetSessionState(r.Context(), runID, runs.SessionIdle, "hook")
			}
		}
		s.runs.AddEvent(r.Context(), runID, "hook:"+event, "agent-hook", map[string]string{"event": event, "advisory": fmt.Sprint(advisory)})
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

// handleActivity serves the Home activity board: runs, tokens, and planned
// fires per bin, one row per agent or per job.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := runs.ActivityOpts{
		Workspace: s.workspaceID(r.Context(), q.Get("workspace")),
		Owner:     strings.TrimSpace(q.Get("agent")),
		Group:     runs.ActivityGroup(q.Get("group")),
	}
	if opts.Group == "" {
		opts.Group = runs.ActivityByAgent
	}
	var err error
	if opts.From, err = time.Parse(time.RFC3339, q.Get("from")); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "from must be an RFC3339 time")
		return
	}
	if opts.To, err = time.Parse(time.RFC3339, q.Get("to")); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "to must be an RFC3339 time")
		return
	}
	if opts.Bin, err = time.ParseDuration(q.Get("bin")); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "bin must be a duration such as 15m, 1h, or 4h")
		return
	}
	if err := opts.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	fires, err := s.sched.Fires(r.Context(), schedule.FiresOpts{Workspace: opts.Workspace, From: opts.From, To: opts.To})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	opts.Due = s.unheldFires(r.Context(), fires)
	act, err := s.runs.Activity(r.Context(), opts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, act)
}

// unheldFires drops the fires that an open pause holds at the time of the fire.
func (s *Server) unheldFires(ctx context.Context, fires []schedule.Fire) []runs.DueFire {
	out := make([]runs.DueFire, 0, len(fires))
	active := s.activePauses(ctx)
	subjects := map[string]pauses.Subject{}
	if len(active) > 0 {
		subjects = s.fireSubjects(ctx, fires)
	}
	for _, f := range fires {
		if p := active.Cover(subjects[f.JobID]); p != nil && (p.Until == nil || f.At.Before(store.ParseTS(*p.Until))) {
			continue
		}
		out = append(out, runs.DueFire{WorkspaceID: f.WorkspaceID, JobID: f.JobID, Spec: f.Spec, At: f.At})
	}
	return out
}

// fireSubjects maps the job ids of the fires to what a pause matches.
func (s *Server) fireSubjects(ctx context.Context, fires []schedule.Fire) map[string]pauses.Subject {
	out := map[string]pauses.Subject{}
	seen := map[string]bool{}
	fallback := job.DefaultHarness()
	for _, f := range fires {
		if seen[f.WorkspaceID] {
			continue
		}
		seen[f.WorkspaceID] = true
		rows, err := s.ws.ListJobs(ctx, f.WorkspaceID)
		if err != nil {
			continue
		}
		kits := agentKitsFrom(rows)
		for _, row := range rows {
			harness, _ := job.HarnessFrom(row.Harness, kits[row.OwnerName], fallback)
			out[row.ID] = pauses.Subject{WorkspaceID: row.WorkspaceID, AgentID: row.AgentID(), Harness: harness, Kind: row.Kind}
		}
	}
	return out
}
