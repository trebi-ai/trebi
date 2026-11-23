package runs

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/agentcli"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/folderenv"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/state"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/uploads"
	"github.com/trebi-ai/trebi/internal/visor"
)

const (
	readyTimeout       = 90 * time.Second
	resumeReadyTimeout = 90 * time.Second
	CompletionPrefix   = job.CompletionPrefix
)

// CompletionSentinel is the exact token a run must output to signal done.
func CompletionSentinel(runID string) string {
	if runID == "" {
		return CompletionPrefix
	}
	return CompletionPrefix + "_" + runID
}

// PromptSuffix appends the completion instruction for the harness.
func PromptSuffix(runID string) string {
	return fmt.Sprintf("\n\nWhen you have completed this task successfully, output exactly this on its own line (nothing else): %s",
		CompletionSentinel(runID))
}

// FailurePrefix starts the line a job run prints when it cannot do its task.
const FailurePrefix = "TREBI_RUN_FAILED"

// ClassReported is the class of the error event of a reported failure.
const ClassReported = "reported"

// FailureSentinel is the failure token of one turn.
func FailureSentinel(key string) string {
	return FailurePrefix + "_" + key
}

var failureLine = regexp.MustCompile(`(?m)^[^\pL\pN]*` + FailurePrefix + `(?:_([A-Za-z0-9_]+))?:[ \t]*(.*)$`)

// ReportedFailure finds the last failure line in text and returns its reason.
// An empty key accepts any key. The prompt echo has the reason "<reason>" and
// does not count.
func ReportedFailure(text, key string) (string, bool) {
	ms := failureLine.FindAllStringSubmatch(text, -1)
	for i := len(ms) - 1; i >= 0; i-- {
		if key != "" && ms[i][1] != key {
			continue
		}
		reason := strings.TrimSpace(strings.Trim(ms[i][2], " \t`*|│"))
		if reason == "" || strings.HasPrefix(reason, "<") {
			continue
		}
		return reason, true
	}
	return "", false
}

// memoryPromptSentence is the one extra completion instruction for memory runs.
const memoryPromptSentence = "Update MEMORY.md before you print the completion line. The daemon validates the file."

// maxMemoryFixes caps the fix-injection cycle per run (B22 §3.3). The gate
// checks the file again after each fix.
const maxMemoryFixes = 5

// MemoryLaunch is the MEMORY.md seam of one launch: the workspace root
// (the files jail), the file path relative to it, and the resolved options.
type MemoryLaunch struct {
	Root string
	Rel  string
	Opt  memory.Options
}

// NewMemoryLaunch resolves the MEMORY.md launch seam for a parsed job: the
// sibling path under root plus options after config defaults. Nil = off.
func NewMemoryLaunch(j *job.Job, root string) *MemoryLaunch {
	if j == nil || !j.Memory || root == "" {
		return nil
	}
	dir, err := filepath.Rel(root, j.Dir)
	if err != nil || dir == ".." || strings.HasPrefix(dir, ".."+string(filepath.Separator)) {
		return nil
	}
	def := config.DefaultMemory()
	if c, cerr := config.Load(); cerr == nil {
		def = c.Defaults.Memory
	}
	return &MemoryLaunch{Root: root, Rel: filepath.Join(dir, "MEMORY.md"), Opt: j.MemoryOpts.Resolve(def)}
}

// LaunchOpts controls how CreateAndLaunch runs a harness.
type LaunchOpts struct {
	CreateOpts
	// Prompt full body (not just preview).
	Prompt string
	// Attachments are the images of the first prompt. A pane run gets their
	// paths in the prompt text.
	Attachments uploads.Attachments
	// CoalesceText is the text a coalesce delivers to the live run. An
	// operator launch has no Prompt yet at that point. A job launch that
	// continues a session (session.continue) sends it in place of Prompt.
	CoalesceText string
	// Wake asks the composer to build the prompt for an operator launch
	// (Kind assistant|agent). Ignored when Prompt is set.
	Wake *WakeOpts
	// Memory is the MEMORY.md seam; nil = memory off.
	Memory *MemoryLaunch
	// StateDecl is the state collections that the job declares. JobID is
	// the state owner.
	StateDecl map[string]state.Collection
	// Kit is the owning agent's reusable run kit.
	Kit *job.KitYAML
	// JobSkills is the job's own preferred skill list.
	JobSkills []string
	// Skills is the effective skill set of the run. The launch fills it.
	Skills []skills.Resolved
	// JobConnectors is the job's own connector list. It adds to the kit
	// list.
	JobConnectors []string
	// Actions are the connector actions resolved for this launch (Part F.3).
	Actions []ConnectorAction
	// Command override (bare harness name or full argv string). Empty → harness name.
	Command string
	// ResumeSessionID warm-resumes that session.
	ResumeSessionID string
	// ReopenRunID makes the launch the next turn of that run, not a new run.
	ReopenRunID string
	// Fake: run a local shell command instead of a real harness (tests/CI).
	FakeCommand []string
	// SkipReady skips WaitReady (tests).
	SkipReady bool
	// Env extra env for the pane.
	Env map[string]string
	// Effort overrides the harness default for this run (launch flag).
	// It already includes the harness default; empty = vendor default.
	// CreateOpts.Model is the model flag.
	Effort string
	// IfRunning policy for overlap: queue|skip|replace. Empty resolves to
	// the config default, then queue.
	IfRunning string
	// Conversation sends the prompt into a live run of the same
	// conversation, whatever IfRunning says. The queue lane sets it for an
	// item with a conversation key.
	Conversation bool
	// ContinueMS is session.continue: with Conversation and no live run, the
	// launch continues the session of the newest run of the conversation
	// that ended inside this window. 0 is off.
	ContinueMS int64
	// BypassOverlap launches next to a live run. Only resume paths set it.
	BypassOverlap bool
	// FollowUp continues the newest run of the conversation with no time
	// limit when ContinueMS is 0 (plan A.11). It replaces Prompt.
	FollowUp *FollowUp
	// Files are written to home.RunDir(ID) just before the launch, by file
	// name. A skipped or coalesced launch writes none. They need ID.
	Files map[string][]byte

	// cfg is the config of this launch. CreateAndLaunch loads it once.
	cfg *config.Config
}

// FollowUp is a message that continues a conversation (plan A.11).
type FollowUp struct {
	Text        string // the prompt when the session resumes
	FreshPrompt string // the prompt when the launch starts a new session
}

// Values of session.if_running.
const (
	IfRunningQueue   = "queue"
	IfRunningSkip    = "skip"
	IfRunningReplace = "replace"
)

// Overlap outcomes that are not if_running values.
const (
	OverlapCoalesce = "coalesce"
)

// ErrRunActive is a queue launch that must wait for a live run of the same
// job and conversation. No run row is created.
var ErrRunActive = errors.New("run active")

// QueueItem returns the queue row of a launch that waits for a live run of
// its job. The queue lane rebuilds the options from the job file and this
// request.
func (o *LaunchOpts) QueueItem(connector string) *dispatch.QueueItem {
	lr := &dispatch.LaunchRequest{
		Reason:         string(o.Reason),
		Harness:        o.Harness,
		Runtime:        o.Runtime,
		Prompt:         o.Prompt,
		Inputs:         o.Inputs,
		ConversationID: o.ConversationID,
	}
	if o.Wake != nil {
		lr.Wake = &dispatch.LaunchWake{Body: o.Wake.Body, ExtraPrompt: o.Wake.ExtraPrompt, Message: o.Wake.Message}
	}
	if t := o.Trigger; t != nil && t.EventID != "" {
		lr.Event = &dispatch.LaunchEvent{ID: t.EventID, Connector: t.Connector, RouteID: t.RouteID, Vars: t.Vars}
	}
	lr.ChainPath = o.ChainPath
	return &dispatch.QueueItem{
		Connector:       connector,
		TraceID:         o.TraceID,
		WorkspaceID:     o.WorkspaceID,
		JobID:           o.JobID,
		JobName:         o.JobName,
		Kind:            o.Kind,
		ConversationKey: o.ConversationKey,
		Status:          dispatch.QueuePending,
		Launch:          lr,
	}
}

// config returns the config of the launch, and loads it when unset (tests).
func (o LaunchOpts) config() *config.Config {
	if o.cfg != nil {
		return o.cfg
	}
	cfg, _ := config.Load()
	return cfg
}

// QueueResult maps the overlap outcome of a launch of r to a queue result.
func (r *Run) QueueResult(outcome string) dispatch.QueueResult {
	switch outcome {
	case IfRunningSkip:
		return dispatch.ResultSkipped
	case OverlapCoalesce:
		return dispatch.ResultCoalesced
	}
	// Fail-fast launch (nil error, failed run) is a dead event, not launched.
	if r.Status == StatusFailed {
		return dispatch.ResultDead
	}
	return dispatch.ResultLaunched
}

// writeFiles writes the run input files of a launch.
func (o *LaunchOpts) writeFiles() error {
	if len(o.Files) == 0 {
		return nil
	}
	if o.ID == "" {
		return errors.New("run files need a run id")
	}
	dir := home.RunDir(o.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, data := range o.Files {
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(name)), data, 0o600); err != nil {
			return fmt.Errorf("run file %s: %w", name, err)
		}
	}
	return nil
}

// wantsFake reports the fake-harness path (contract e2e without real LLM binary).
func (o *LaunchOpts) wantsFake() bool {
	return len(o.FakeCommand) > 0 || o.Harness == "fake" || os.Getenv("TREBI_FAKE_HARNESS") != ""
}

func (o *LaunchOpts) fakeTranscriptPath(dir string) string {
	name := o.Harness
	if name == "" || name == "fake" {
		name = agentcli.Claude
	}
	tool, ok := agentcli.ByName(name)
	if !ok {
		return ""
	}
	path, err := tool.TranscriptPath(home.HarnessDir(name), "", o.SessionID)
	if err != nil {
		return ""
	}
	return path
}

// applyResumePlan sets session identity for a resume launch.
func (o *LaunchOpts) applyResumePlan(plan *agentcli.LaunchPlan) {
	if plan.SessionID == "" {
		plan.SessionID = o.ResumeSessionID
	}
	o.SessionID = plan.SessionID
	// A request answer, a message, a chain message, and a conversation event keep their
	// own reason.
	switch o.Reason {
	case ReasonRequest, ReasonMessage, ReasonTrigger, ReasonAsk, ReasonHandoff, ReasonReply:
	default:
		o.Reason = ReasonResume
	}
}

// CreateAndLaunch inserts a run and starts the harness (in a pane) or fake process.
// The second return is the overlap outcome: "" (no live run or bypass) | skip | coalesce | replace.
// On skip/coalesce the returned run is the skipped row / the live run — no launch happens.
// On queue with a live run it returns ErrRunActive and creates no row.
func (s *Service) CreateAndLaunch(ctx context.Context, opts LaunchOpts) (*Run, string, error) {
	// Decide human status before minting a token: a token identifies the run
	// process, not the human who launched it.
	humanManual := opts.CreateOpts.ActorToken == "" && opts.Reason == ReasonManual
	if err := s.refuseArchived(ctx, opts.WorkspaceID); err != nil {
		return nil, "", err
	}
	if err := s.refuseArchivedJob(ctx, opts); err != nil {
		return nil, "", err
	}
	if err := s.checkPause(ctx, &opts); err != nil {
		return nil, "", err
	}
	// Mint before Create so every runtime branch can pass the plaintext to
	// the process environment; the persisted row gets only the hash.
	if opts.CreateOpts.ActorToken == "" {
		token, _, err := mintActorToken()
		if err != nil {
			return nil, "", err
		}
		opts.CreateOpts.ActorToken = token
	}
	// A human manual wake has no actor token and is deliberately allowed over
	// budget; the gate still gives automated launches and agent calls a vote.
	if s.budgetGate != nil && !humanManual {
		ok, reason, err := s.budgetGate.Allow(ctx, opts.WorkspaceID, opts.JobName, opts.Kind)
		if err != nil {
			return nil, "", err
		}
		if !ok {
			return nil, "", fmt.Errorf("%w: %s", ErrBudgetExceeded, reason)
		}
	}
	// Actions resolve before the overlap policy: a launch that then fails
	// must not replace a live run.
	if !opts.wantsFake() {
		if err := s.resolveActions(ctx, &opts); err != nil {
			var le *ConnectorLoginError
			if errors.As(err, &le) {
				run, ferr := s.FailBeforeLaunch(ctx, opts, EndConnectorLogin, map[string]string{
					"connector": le.Connector, "message": "Log in again to " + le.Title,
				})
				return run, "", ferr
			}
			return nil, "", err
		}
	}
	opts.cfg = opts.config()
	var outcome string
	// Overlap policy against live runs of the same job (assistant: same conversation).
	if opts.JobID != "" && opts.WorkspaceID != "" && !opts.BypassOverlap {
		oc, done, err := s.applyIfRunning(ctx, &opts)
		if err != nil {
			return nil, "", err
		}
		if done != nil {
			return done, oc, nil
		}
		outcome = oc
	}
	if opts.MaxDurationMS == 0 && (opts.Kind == KindJob || opts.Kind == KindAgent || opts.Kind == "") {
		opts.MaxDurationMS = defaultMaxDurationMS(opts.cfg)
	}
	var fallback map[string]string
	if opts.Conversation && opts.ResumeSessionID == "" && (opts.ContinueMS > 0 || opts.FollowUp != nil) {
		fallback = s.continueConversation(ctx, &opts)
	}
	if f := opts.FollowUp; f != nil {
		opts.Prompt = f.FreshPrompt
		if opts.ResumeSessionID != "" {
			opts.Prompt = f.Text
		}
	} else if opts.ResumeSessionID != "" && opts.CoalesceText != "" {
		// A continued session holds the job prompt or the wake; it gets only the message.
		opts.Prompt = opts.CoalesceText
	}

	if opts.SessionID == "" && opts.ResumeSessionID == "" {
		opts.SessionID = uuid.NewString()
	}
	s.prepMemory(ctx, &opts)
	s.resolveSkills(ctx, &opts)
	s.applyWakePrompt(ctx, &opts)
	var hints []string
	if opts.ResumeSessionID == "" {
		hints = append(hints, connectorHint(opts.Actions), scopedSkillHint(opts.Skills))
	}
	for _, hint := range append(hints, s.promptHints(ctx, opts)...) {
		if hint == "" {
			continue
		}
		if strings.TrimSpace(opts.Prompt) == "" {
			opts.Prompt = hint
		} else {
			opts.Prompt = strings.TrimSpace(opts.Prompt) + "\n\n" + hint
		}
	}
	if opts.PromptPreview == "" {
		opts.PromptPreview = opts.Prompt
	}
	opts.CreateOpts.Prompt = opts.Prompt
	if err := opts.writeFiles(); err != nil {
		return nil, "", err
	}

	run, err := s.launch(ctx, opts)
	if err == nil && run != nil && fallback != nil {
		s.AddEvent(ctx, run.ID, "continue_fallback", "runner", fallback)
	}
	return run, outcome, err
}

// continueConversation points opts at the session of the newest finished run
// of its conversation when that run ended inside the session.continue window.
// A ContinueMS of 0 has no window (a follow-up).
// It returns the run event detail when a session was due but cannot continue.
func (s *Service) continueConversation(ctx context.Context, opts *LaunchOpts) map[string]string {
	rows, err := s.List(ctx, ListOpts{
		Workspace:            opts.WorkspaceID,
		Job:                  opts.JobID,
		Kind:                 opts.Kind,
		ConversationKey:      opts.ConversationKey,
		ConversationKeyExact: true,
		Limit:                20,
	})
	if err != nil {
		s.log.Warn("continue lookup", "job_id", opts.JobID, "error", err)
		return nil
	}
	// A skip row has no session; the newest row with one is the conversation.
	i := slices.IndexFunc(rows, func(r *Run) bool { return r.SessionID != "" })
	if i < 0 {
		return nil
	}
	prev := rows[i]
	switch prev.Status {
	case StatusComplete, StatusExited, StatusTimedOut:
	default:
		return nil
	}
	if prev.EndedAt == nil || (opts.ContinueMS > 0 && time.Since(*prev.EndedAt) > time.Duration(opts.ContinueMS)*time.Millisecond) {
		return nil
	}
	if opts.Harness != "" && prev.Harness != opts.Harness {
		return nil
	}
	if tool, ok := agentcli.ByName(prev.Harness); ok {
		dir := agentcli.TranscriptWorkingDir(prev.Harness, prev.WorkingDir, prev.StartedAt)
		path, err := tool.TranscriptPath(home.HarnessDir(prev.Harness), dir, prev.SessionID)
		if err == nil {
			if _, statErr := os.Stat(path); statErr != nil {
				opts.PreviousRunID = prev.ID
				return map[string]string{"from": prev.ID, "because": "session file not found"}
			}
		}
	}
	opts.ResumeSessionID = prev.harnessSessionID()
	opts.ReopenRunID = prev.ID
	opts.WorkingDir = cmp.Or(prev.WorkingDir, opts.WorkingDir)
	return nil
}

// harnessSessionID returns the id that the harness resumes. A codex pane run
// stores the trebi id, so the codex id comes from its rollout file name.
func (r *Run) harnessSessionID() string {
	if r.Harness != agentcli.Codex || r.Runtime != "tui" || r.SessionID == "" {
		return r.SessionID
	}
	tool, _ := agentcli.ByName(agentcli.Codex)
	dir := agentcli.TranscriptWorkingDir(r.Harness, r.WorkingDir, r.StartedAt)
	path, err := tool.TranscriptPath(home.HarnessDir(r.Harness), dir, r.SessionID)
	if err != nil {
		return r.SessionID
	}
	return cmp.Or(agentcli.CodexRolloutID(path), r.SessionID)
}

// open creates the run, or reopens opts.ReopenRunID as its next turn.
func (s *Service) open(ctx context.Context, opts LaunchOpts) (*Run, error) {
	var run *Run
	var err error
	if opts.ReopenRunID == "" {
		run, err = s.Create(ctx, opts.CreateOpts)
	} else {
		run, err = s.Reopen(ctx, opts.ReopenRunID, opts.CreateOpts)
	}
	if err == nil && opts.FolderEnv != nil && !opts.DryRun {
		s.noteFolderEnv(ctx, run.ID, opts.FolderEnv)
	}
	if err == nil && !opts.DryRun {
		for _, a := range opts.Actions {
			if a.Note != "" {
				s.appendRunLog(run.ID, a.Note)
			}
		}
	}
	return run, err
}

// noteFolderEnv logs the .env load of one launch, with key names only.
func (s *Service) noteFolderEnv(ctx context.Context, runID string, f *folderenv.Summary) {
	s.log.InfoContext(ctx, "folderenv.loaded", "run_id", runID, "path", f.Path,
		"keys", len(f.Keys), "skipped", f.Skipped, "overridden", f.Overridden, "err", f.Error)
	line := fmt.Sprintf("Loaded %d values from .env: %s", len(f.Keys), strings.Join(f.Keys, ", "))
	if f.Error != "" {
		line = "Did not load .env: " + f.Error
	}
	for _, sk := range f.Skipped {
		line += fmt.Sprintf("\nSkipped .env line %d: %s", sk.Line, sk.Reason)
	}
	s.appendRunLog(runID, line)
}

// launch starts the run on the runtime that opts select.
func (s *Service) launch(ctx context.Context, opts LaunchOpts) (*Run, error) {
	if opts.ReopenRunID != "" {
		// The MCP plan and the pane env name the run before it reopens.
		opts.ID = opts.ReopenRunID
	}
	if opts.wantsFake() {
		opts.Prompt, opts.Attachments = opts.Attachments.Inline(opts.Prompt), nil
		return s.launchFake(ctx, opts)
	}
	opts.cfg = opts.config()
	command := opts.harnessCommand()
	name := agentcli.Detect(cmp.Or(command, "claude")).Name()
	if opts.Command == "" && opts.Harness != "" && harness.Known(opts.Harness) {
		name = opts.Harness
	}
	if !harnessOn(opts.cfg, name) {
		return s.FailBeforeLaunch(ctx, opts, EndHarnessOff, map[string]string{"message": harness.OffMessage(name)})
	}
	opts.Env = harnessEnvUnder(opts.cfg, name, opts.Env)
	// If binary not on PATH and no pin, fall back to fake when
	// TREBI_ALLOW_FAKE_FALLBACK=1, else fail fast.
	if command != "" && commandUnavailable(command) {
		if os.Getenv("TREBI_ALLOW_FAKE_FALLBACK") == "1" {
			return s.launchFake(ctx, opts)
		}
		return s.failFast(ctx, opts)
	}

	// Subprocess is the default vendor runtime (Part F); runtime: tui, the
	// kill switch, or a harness without a proven wire stays on the pane.
	if s.wantsCLI(agentcli.Detect(command), opts) {
		return s.launchCLI(ctx, opts)
	}
	opts.Prompt, opts.Attachments = opts.Attachments.Inline(opts.Prompt), nil
	return s.launchPane(ctx, opts)
}

// resolveActions resolves the connector names of a launch and checks the
// support matrix (decision 8) before a run row exists. The job list adds to
// the kit list. The email name is built in; the resolver gets the rest.
func (s *Service) resolveActions(ctx context.Context, opts *LaunchOpts) error {
	names := connectorNames(opts.JobConnectors, opts.Kit)
	trigger := s.triggerConnector(ctx, *opts)
	if len(names) == 0 && trigger == "" {
		return nil
	}
	var actions []ConnectorAction
	managed := make([]string, 0, len(names))
	for _, n := range names {
		if n == job.EmailConnector {
			if a, ok := s.emailAction(ctx, *opts); ok {
				actions = append(actions, a)
			} else {
				s.log.Warn("email action skipped: the run has no owner agent", "job", opts.JobName)
			}
			continue
		}
		managed = append(managed, n)
	}
	if len(managed) > 0 {
		if s.actions == nil {
			return fmt.Errorf("%w: no connector resolver is wired", ErrActionsInvalid)
		}
		resolved, err := s.actions.ResolveActions(ctx, opts.WorkspaceID, managed)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrActionsInvalid, err)
		}
		for i, a := range resolved {
			if a.RunToken && a.MCP != nil {
				srv := *a.MCP
				srv.Headers = maps.Clone(srv.Headers)
				if srv.Headers == nil {
					srv.Headers = map[string]string{}
				}
				srv.Headers[tools.HeaderRunToken] = opts.ActorToken
				resolved[i].MCP = &srv
			}
		}
		// A resume names the run's connection ids next to the kit slugs, so
		// one connection can resolve twice.
		for _, a := range resolved {
			if !slices.ContainsFunc(actions, func(b ConnectorAction) bool { return b.ID == a.ID }) {
				actions = append(actions, a)
			}
		}
	}
	opts.Connectors = make([]string, 0, len(actions))
	for _, a := range actions {
		opts.Connectors = append(opts.Connectors, a.ID)
	}
	if g := s.grantAction(ctx, *opts, trigger, actions); g != nil {
		actions = append(actions, *g)
	}
	opts.Actions = actions
	tool := agentcli.Detect(cmp.Or(opts.harnessCommand(), "claude"))
	return s.ActionsSupported(ctx, tool.Name(), runRuntimeName(s, tool, *opts), hasMCP(actions))
}

// grantAction is the read-only action of the trigger connection when the
// kit does not name it. It never fails a launch: a problem skips the grant.
func (s *Service) grantAction(ctx context.Context, opts LaunchOpts, trigger string, kit []ConnectorAction) *ConnectorAction {
	covered := func(id, slug string) bool {
		return slices.ContainsFunc(kit, func(a ConnectorAction) bool { return a.ID == id || a.Slug == slug })
	}
	if trigger == "" || covered(trigger, trigger) {
		return nil
	}
	skip := func(reason string) *ConnectorAction {
		s.log.Info("connector.grant_skipped", "connector", trigger, "job", opts.JobName, "reason", reason)
		return nil
	}
	if s.actions == nil {
		return skip("no connector resolver is wired")
	}
	g, err := s.actions.GrantActions(ctx, opts.WorkspaceID, trigger)
	if err != nil {
		return skip(err.Error())
	}
	if g == nil {
		return skip("the connection has no read-only operation")
	}
	if covered(g.ID, g.Slug) {
		return nil
	}
	g.ReadOnly = true
	return g
}

// applyWakePrompt composes the prompt for an operator launch (Part F.3).
// A set Prompt or nil Wake wins. Without a composer it falls back to the
// var-expanded body plus extra prompt.
func (s *Service) applyWakePrompt(ctx context.Context, opts *LaunchOpts) {
	if opts.Prompt != "" || opts.Wake == nil {
		return
	}
	hint := combinedSkillHint(opts.Kit, opts.JobSkills, usedSkillNames(opts.Skills))
	if opts.Kind != KindJob && opts.Kind != "" && s.composer != nil {
		wake := *opts.Wake
		wake.Memory = opts.Memory
		composed, err := s.composer.ComposeWakePrompt(ctx, WakeScope{
			WorkspaceID: opts.WorkspaceID, Kind: opts.Kind, Name: opts.JobName, OwnerID: opts.JobID,
		}, wake)
		if err != nil {
			s.log.Warn("wake compose failed; using raw body", "job_id", opts.JobID, "err", err)
		} else if strings.TrimSpace(composed) != "" {
			opts.Prompt = strings.TrimSpace(composed + "\n\n" + hint)
			return
		}
	}
	opts.Prompt = joinExtra(opts.Wake.Body, opts.Wake.ExtraPrompt)
	if hint != "" {
		opts.Prompt = strings.TrimSpace(opts.Prompt + "\n\n" + hint)
	}
}

// prepMemory seeds, hashes, and injects MEMORY.md and the state
// block for one launch (B22 §3.3, plan 2026-10-06 B5). It runs before wake
// composition so the composer reads a settled file. A files error skips
// memory for the launch (logged). Agents get both through the wake composer.
// A resumed session already holds both; it only hears of an edit made while
// it was not running.
func (s *Service) prepMemory(ctx context.Context, opts *LaunchOpts) {
	data, mtime := s.memoryData(ctx, opts.Memory)
	if data != nil {
		opts.MemoryHash = memory.Hash(data)
	}
	if opts.ResumeSessionID != "" {
		if data != nil && s.memoryEditedSince(ctx, opts.ReopenRunID, mtime) {
			opts.Prompt = strings.TrimSpace(memoryEditedNote + "\n\n" + opts.Prompt)
		}
		return
	}
	if opts.Kind == KindAgent {
		return
	}
	var parts []string
	var doc *memory.Doc
	if data != nil {
		doc, _ = memory.Parse(data)
		parts = append(parts, memory.Render(doc, opts.Memory.Opt))
	}
	if brief := s.stateBrief(ctx, *opts); brief != "" {
		parts = append(parts, strings.TrimSpace(brief))
	}
	if data != nil {
		parts = append(parts, memory.Protocol(opts.Memory.Opt, memory.NewProtocolInfo(doc, opts.Memory.Opt)))
	}
	if len(parts) > 0 {
		opts.Prompt = strings.Join(append(parts, opts.Prompt), "\n\n")
	}
}

// memoryEditedNote tells a resumed session that MEMORY.md changed while the
// session was not running.
const memoryEditedNote = "[trebi] MEMORY.md changed since your last turn. Read it again before you continue."

// memoryData seeds MEMORY.md and returns its bytes and mtime.
// Nil = memory off or not readable.
func (s *Service) memoryData(ctx context.Context, mem *MemoryLaunch) ([]byte, time.Time) {
	if mem == nil || s.fs == nil {
		return nil, time.Time{}
	}
	data, info, err := s.fs.Read(ctx, mem.Root, mem.Rel)
	switch {
	case errors.Is(err, files.ErrNotFound):
		if _, _, werr := s.fs.Write(ctx, mem.Root, mem.Rel, memory.Skeleton(), ""); werr != nil {
			s.log.Warn("memory seed", "rel", mem.Rel, "err", werr)
			return nil, time.Time{}
		}
		return memory.Skeleton(), time.Now()
	case err != nil:
		s.log.Warn("memory read", "rel", mem.Rel, "err", err)
		return nil, time.Time{}
	}
	return data, info.MTime
}

// memoryEditedSince reports a MEMORY.md mtime after the end of the last turn
// of runID. The session wrote its own edits before that turn ended.
func (s *Service) memoryEditedSince(ctx context.Context, runID string, mtime time.Time) bool {
	if runID == "" || mtime.IsZero() {
		return false
	}
	prev, err := s.Get(ctx, runID)
	if err != nil || prev.EndedAt == nil {
		return false
	}
	return mtime.After(*prev.EndedAt)
}

// stateBrief renders the state block of a job launch. "" when no briefer is
// wired, the run has no owner, or the owner has no state.
func (s *Service) stateBrief(ctx context.Context, opts LaunchOpts) string {
	if s.briefer == nil || opts.JobID == "" || (opts.Kind != KindJob && opts.Kind != "") {
		return ""
	}
	brief, err := s.briefer.Brief(ctx, state.Owner{ID: opts.JobID, WorkspaceID: opts.WorkspaceID, Collections: opts.StateDecl})
	if err != nil {
		s.log.Warn("state brief", "job_id", opts.JobID, "err", err)
		return ""
	}
	return brief
}

// memoryFor resolves the gate-time memory seam for a stored run. Nil when the
// run launched without memory or no resolver is wired.
func (s *Service) memoryFor(ctx context.Context, run *Run) *MemoryLaunch {
	if run.MemoryHash == "" || s.memRes == nil {
		return nil
	}
	return s.memRes.MemoryLaunchFor(ctx, run)
}

// memoryIssues are the findings of one memory gate.
type memoryIssues []memory.Issue

func (m memoryIssues) lines() []string { return memory.FormatIssues("MEMORY.md", m) }

// detail is the damage for a person: no file name and no freshness issue,
// one issue per line, each a full sentence.
func (m memoryIssues) detail() string {
	out := make([]string, 0, len(m))
	for _, iss := range m {
		if iss.Code == memory.Unchanged().Code {
			continue
		}
		// The alert body drops double quotes, so keep the names readable.
		msg := strings.ReplaceAll(iss.Msg, `"`, "'")
		if !strings.HasSuffix(msg, ".") {
			msg += "."
		}
		if iss.Line > 0 {
			msg = fmt.Sprintf("Line %d: %s", iss.Line, msg)
		}
		out = append(out, msg)
	}
	return strings.Join(out, " ")
}

// broken tells if the file itself is damaged. A file that only missed its
// Log entry this run is sound: the next run writes one.
func (m memoryIssues) broken() bool {
	for _, iss := range m {
		if iss.Code != memory.Unchanged().Code {
			return true
		}
	}
	return false
}

// gateMemory validates MEMORY.md at a completion sentinel. Empty result = pass.
func (s *Service) gateMemory(ctx context.Context, run *Run, mem *MemoryLaunch) memoryIssues {
	if mem == nil || run.MemoryHash == "" || s.fs == nil {
		return nil
	}
	data, info, err := s.fs.Read(ctx, mem.Root, mem.Rel)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			return memoryIssues{{Code: "missing", Msg: "file is missing. Restore it with the three sections."}}
		}
		return memoryIssues{{Code: "read_failed", Msg: fmt.Sprintf("read failed: %v", err)}}
	}
	unchanged := mem.Opt.RequireUpdate && memory.Hash(data) == run.MemoryHash
	data = s.trimMemory(ctx, run, mem, data, info.Hash)
	issues := memoryIssues(memory.Validate(data, mem.Opt))
	if unchanged {
		issues = append(issues, memory.Unchanged())
	}
	return issues
}

// trimMemory removes the oldest Log entries that break the caps, before the
// agent gets a fix prompt. It returns the bytes to validate.
func (s *Service) trimMemory(ctx context.Context, run *Run, mem *MemoryLaunch, data []byte, hash string) []byte {
	trimmed, n := memory.Trim(data, mem.Opt)
	if n == 0 {
		return data
	}
	if _, _, err := s.fs.Write(ctx, mem.Root, mem.Rel, trimmed, hash); err != nil {
		s.log.Warn("memory trim", "run", run.ID, "error", err)
		return data
	}
	s.AddEvent(ctx, run.ID, "memory.trimmed", "runs", map[string]string{"removed": strconv.Itoa(n)})
	s.log.Info("memory.trimmed", "run", run.ID, "removed", n)
	return trimmed
}

func (s *Service) launchFake(ctx context.Context, opts LaunchOpts) (*Run, error) {
	if opts.PaneSession == "" {
		opts.PaneSession = "trebi-fake-" + strings.ToLower(ulid.Make().String()[:8])
	}
	if opts.ResumeSessionID != "" {
		opts.SessionID = opts.ResumeSessionID
	}
	// Mark before the row turns running, so a sweep never sees a gap.
	if opts.ID == "" {
		opts.ID = ulid.Make().String()
	}
	s.markInProcess(opts.ID, true)
	defer s.markInProcess(opts.ID, false)
	run, err := s.open(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts.WorkingDir == "" {
		opts.WorkingDir = os.TempDir()
	}
	dir := opts.WorkingDir
	cmdArgs := opts.FakeCommand
	if len(cmdArgs) == 0 {
		cmd := os.Getenv("TREBI_FAKE_HARNESS_CMD")
		if cmd == "" {
			cmd = fmt.Sprintf("echo working; echo %s", CompletionSentinel(run.sentinelKey()))
		}
		cmdArgs = []string{"sh", "-c", cmd}
	}
	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = dir
	cmd.Env = fakeEnv(run, opts, dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	out, done := s.driveFake(ctx, run, opts, stdout, stdin)
	_ = stdin.Close()
	_ = cmd.Wait()
	output := out + errBuf.String()
	s.appendRunLog(run.ID, output)
	end, status := EndLaunch, StatusFailed
	switch {
	case done:
		end, status = EndSentinel, StatusComplete
	case ctx.Err() != nil:
		end, status = EndMaxDuration, StatusTimedOut
	}
	tokens := &pricing.TokenUsage{Input: 1, Output: 1, Model: opts.Harness}
	if termErr := s.TerminateTurn(ctx, run.ID, run.TurnCount, status, end, tokens); termErr != nil {
		return run, fmt.Errorf("terminate: %w", termErr)
	}
	return s.Get(ctx, run.ID)
}

// driveFake streams fake-harness stdout and gates memory at each completion
// sentinel. It returns the captured stdout and whether the run completed.
// A fix message goes to the fake's stdin; a fake that exits without answering
// it counts as budget spent (recordMemoryInvalid, complete).
func (s *Service) driveFake(ctx context.Context, run *Run, opts LaunchOpts, stdout io.Reader, stdin io.Writer) (string, bool) {
	sentinel := CompletionSentinel(run.sentinelKey())
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var b strings.Builder
	attempts := 0
	awaitFix := false
	var lastIssues memoryIssues
	for sc.Scan() {
		line := sc.Text()
		b.WriteString(line)
		b.WriteByte('\n')
		if strings.TrimSpace(line) != sentinel {
			continue
		}
		issues := s.gateMemory(ctx, run, opts.Memory)
		if len(issues) == 0 {
			s.recordMemoryValid(ctx, run)
			return b.String(), true
		}
		if attempts >= maxMemoryFixes {
			s.recordMemoryInvalid(ctx, run, issues, "fake")
			return b.String(), true
		}
		attempts++
		lastIssues = issues
		s.requestMemoryFix(ctx, run.ID, "fake", attempts, issues)
		if _, werr := io.WriteString(stdin, fixMessage(issues, run.sentinelKey())+"\n"); werr != nil {
			s.recordMemoryInvalid(ctx, run, issues, "fake")
			return b.String(), true
		}
		awaitFix = true
	}
	if awaitFix {
		s.recordMemoryInvalid(ctx, run, lastIssues, "fake")
		return b.String(), true
	}
	return b.String(), false
}

// launchPane drives a harness inside a pane (explicit runtime: tui).
func (s *Service) launchPane(ctx context.Context, opts LaunchOpts) (*Run, error) {
	opts.Runtime = "tui"
	command := cmp.Or(opts.harnessCommand(), "claude")
	// Prefer pinned binary under TREBI_HOME/bin/<tool>/current.
	command = resolveCommand(command)

	if opts.WorkingDir == "" && opts.ReopenRunID == "" {
		opts.WorkingDir, _ = os.Getwd()
	}
	cwd := opts.WorkingDir

	tool := detectTool(command, &opts)

	resuming := opts.ResumeSessionID != ""
	plan, err := preparePlan(tool, command, &opts)
	if err != nil {
		return nil, err
	}
	if err := injectMCPPlan(tool, &plan, &opts); err != nil {
		return nil, err
	}
	command = plan.Command

	s.log.Info("open a pane", "job_id", opts.JobID, "cwd", cwd, "harness", tool.Name())
	wsRef, err := s.panes.Open(ctx, visor.PaneSpec{
		Title: sessionTitle(opts.JobName), Dir: cwd,
		Labels: map[string]string{LabelJob: opts.JobID, LabelWorkspace: opts.WorkspaceID},
	})
	if err != nil {
		return nil, err
	}
	opts.PaneSession = wsRef

	// Persist run before readiness so reaper can attribute the session.
	run, err := s.open(ctx, opts)
	if err != nil {
		_ = s.panes.Kill(context.WithoutCancel(ctx), wsRef) // the open error is the one to report
		return nil, err
	}
	s.AddEvent(ctx, run.ID, "launch", "runner", map[string]string{
		"harness": tool.Name(), "session_id": opts.SessionID, "pane": wsRef, "mode": "pane",
	})
	s.recordSkills(ctx, run.ID, opts.Skills)
	s.recordAccess(ctx, run.ID, opts.Actions)

	sourceLine, envPath, err := paneEnvFile(run.WorkingDir, run.ID, opts.ActorToken, opts.Harness, tool.Name(), actionEnv(opts.Actions), opts.Env, plan.Env)
	if err != nil {
		return s.failLaunch(ctx, run, wsRef, err)
	}
	if err := s.panes.Send(ctx, wsRef, sourceLine+"\n"); err != nil {
		_ = os.Remove(envPath)
		return s.failLaunch(ctx, run, wsRef, err)
	}
	if err := s.panes.Send(ctx, wsRef, command+"\n"); err != nil {
		return s.failLaunch(ctx, run, wsRef, err)
	}

	prompt := run.panePrompt(opts)
	if opts.SkipReady {
		if err := s.sendPrompt(ctx, wsRef, prompt, tool.Name()); err != nil {
			return s.failLaunch(ctx, run, wsRef, err)
		}
		s.SetSessionState(ctx, run.ID, SessionRunning, "launch")
		s.log.Info("run launched", "id", run.ID, "pane", wsRef, "harness", tool.Name())
		return s.Get(ctx, run.ID)
	}

	// WaitReady must not hold the HTTP request: the CLI times out at 30s and
	// cancelling the request used to kill the pane (session_gone / launch).
	go s.finishLaunch(run, wsRef, tool, prompt, resuming)
	return s.Get(ctx, run.ID)
}

// panePrompt is the prompt that a pane run types: the job prompt plus the
// sentinel, memory, and failure sentences.
func (r *Run) panePrompt(opts LaunchOpts) string {
	prompt := opts.Prompt
	if prompt == "" {
		prompt = opts.PromptPreview
	}
	prompt += PromptSuffix(r.sentinelKey())
	if opts.Memory != nil {
		prompt += " " + memoryPromptSentence
	}
	if kindOrDefault(opts.Kind) == KindJob {
		prompt += failurePromptSentence(r.sentinelKey())
	}
	return prompt
}

// finishLaunch waits for the harness composer, then pastes the prompt.
func (s *Service) finishLaunch(run *Run, wsRef string, tool agentcli.Tool, prompt string, resuming bool) {
	ctx := context.Background()
	s.markLaunching(run.ID, true)
	defer s.markLaunching(run.ID, false)
	ready, waitQ := s.waitHarnessReady(ctx, run, wsRef, tool, resuming)
	if !ready {
		return
	}
	if waitQ != "" {
		s.SetSessionState(ctx, run.ID, SessionWaiting, "dialog")
		if _, err := s.OpenApproval(ctx, run.ID, waitQ); err != nil {
			s.log.Warn("open approval", "run", run.ID, "error", err)
		}
		s.stashPendingPrompt(run.ID, pendingPrompt{session: wsRef, prompt: prompt, harness: tool.Name()})
		s.AddEvent(ctx, run.ID, "waiting", "dialog", map[string]string{"question": waitQ})
		s.log.Info("run waiting on dialog", "id", run.ID, "question", waitQ)
		return
	}
	s.deliverPrompt(ctx, run, wsRef, tool.Name(), prompt)
}

func (s *Service) deliverPrompt(ctx context.Context, run *Run, session, harness, prompt string) {
	if err := s.sendPrompt(ctx, session, prompt, harness); err != nil {
		_, _ = s.failLaunch(ctx, run, session, err)
		return
	}
	s.SetSessionState(ctx, run.ID, SessionRunning, "launch")
	s.log.Info("run launched", "id", run.ID, "pane", session, "harness", harness)
}

// FailBeforeLaunch records a terminal failed run without starting the harness.
// Use it when a producer (schedule, queue) has claimed work but cannot launch.
func (s *Service) FailBeforeLaunch(ctx context.Context, opts LaunchOpts, end EndReason, detail map[string]string) (*Run, error) {
	if opts.PromptPreview == "" {
		opts.PromptPreview = opts.Prompt
	}
	// Record the runtime the launch would have used, so a blocked auth/limit
	// row shows the subprocess posture rather than a pane it never opened.
	if opts.Runtime == "" {
		if s.cliDriverFor(opts.Harness) != nil {
			opts.Runtime = "cli"
		}
	}
	if opts.ReopenRunID != "" {
		opts.ID = opts.ReopenRunID
	}
	run, err := s.open(ctx, opts)
	if err != nil {
		return nil, err
	}
	if detail == nil {
		detail = map[string]string{}
	}
	if detail["message"] == "" {
		detail["message"] = "run failed: " + string(end)
	}
	if opts.Harness != "" && detail["harness"] == "" {
		detail["harness"] = opts.Harness
	}
	s.AddEvent(ctx, run.ID, "error", "launch", detail)
	s.appendRunLog(run.ID, detail["message"])
	switch end {
	case EndAuth:
		s.emitAuthProbeFailed(ctx, opts.Harness, run.ID, harness.SourceLaunch)
	case EndLimit:
		s.emitLimitExceeded(ctx, opts.Harness, run.ID)
	}
	_ = s.TerminateTurn(ctx, run.ID, run.TurnCount, StatusFailed, end, nil)
	return s.Get(ctx, run.ID)
}

// failFast records a failed run so the API does not hang on WaitReady.
func (s *Service) failFast(ctx context.Context, opts LaunchOpts) (*Run, error) {
	return s.FailBeforeLaunch(ctx, opts, EndLaunch, map[string]string{
		"message": "harness binary unavailable",
	})
}

// appendRunLog adds chunk to the run log without replacing earlier captures.
func (s *Service) appendRunLog(runID, chunk string) {
	chunk = strings.TrimRight(chunk, "\n")
	if runID == "" || chunk == "" {
		return
	}
	path := filepath.Join(home.RunsLogDir(), runID+".log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.log.Warn("run log mkdir", "run_id", runID, "err", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.log.Warn("run log append", "run_id", runID, "err", err)
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		chunk = "\n" + chunk
	}
	if _, err := f.WriteString(chunk + "\n"); err != nil {
		s.log.Warn("run log append", "run_id", runID, "err", err)
	}
}

func (s *Service) runLogExists(runID string) bool {
	_, err := os.Stat(filepath.Join(home.RunsLogDir(), runID+".log"))
	return err == nil
}

// closeSession captures the pane before it stops it, so the run log keeps
// what a tui run printed. Every teardown path comes through here.
func (s *Service) closeSession(ctx context.Context, runID, session string) {
	if session == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if out, err := s.panes.History(ctx, session); err == nil {
		// Keep the in-run capture; a final alt-screen is often just Ctrl-C chrome.
		if !s.runLogExists(runID) {
			s.appendRunLog(runID, paneCaptureHeader(out)+"\n"+out)
		}
	} else {
		s.log.Warn("run log capture", "run_id", runID, "session", session, "err", err)
	}
	if err := s.panes.Kill(ctx, session); err != nil {
		s.log.Warn("stop a pane", "run_id", runID, "session", session, "err", err)
	}
}

func (s *Service) failLaunch(ctx context.Context, run *Run, wsRef string, err error) (*Run, error) {
	_ = s.TerminateTurn(ctx, run.ID, run.TurnCount, StatusFailed, EndLaunch, nil)
	s.closeSession(ctx, run.ID, wsRef)
	got, _ := s.Get(ctx, run.ID)
	if got != nil {
		return got, fmt.Errorf("launch: %w", err)
	}
	return run, fmt.Errorf("launch: %w", err)
}

// waitHarnessReady waits for the harness prompt.
// ready=false means the run is already terminal.
// ready=true with a non-empty waitQ means a TUI dialog is stuck: keep the
// session, mark waiting, and let the reaper paste once the composer appears.
func (s *Service) waitHarnessReady(ctx context.Context, run *Run, wsRef string, tool agentcli.Tool, resuming bool) (ready bool, waitQ string) {
	runID := run.ID
	rt := readyTimeout
	if resuming {
		rt = resumeReadyTimeout
	}
	err := agentcli.WaitReadyTool(ctx, s.panes.Pane(wsRef), tool, rt)
	if err == nil {
		time.Sleep(tool.PostReadyDelay())
		s.stampHarnessAuthOK(ctx, tool.Name())
		return true, ""
	}
	var dw *agentcli.DialogWaitingError
	if errors.As(err, &dw) {
		q := dw.Question
		if q == "" {
			q = "harness is waiting on a choice"
		}
		return true, q
	}
	end := EndLaunch
	switch {
	case errors.Is(err, agentcli.ErrAuthMissing):
		end = EndAuth
		s.emitAuthProbeFailed(ctx, tool.Name(), runID, harness.SourceProbe)
	case errors.Is(err, agentcli.ErrLimitExceeded):
		end = EndLimit
		s.emitLimitExceeded(ctx, tool.Name(), runID)
	}
	_ = s.TerminateTurn(ctx, runID, run.TurnCount, StatusFailed, end, nil)
	s.closeSession(ctx, runID, wsRef)
	return false, ""
}

// DetectCompletion reports whether the pane shows the sentinel of key (see
// Run.sentinelKey).
func DetectCompletion(screen, runID string) bool {
	if runID == "" {
		return false
	}
	sentinel := CompletionSentinel(runID)
	flat := flattenForDetect(screen)
	const echoMarker = "output exactly"
	const echoWindow = 64
	from := 0
	for {
		idx := strings.Index(flat[from:], sentinel)
		if idx < 0 {
			return false
		}
		pos := from + idx
		start := pos - echoWindow
		if start < 0 {
			start = 0
		}
		if !strings.Contains(flat[start:pos], echoMarker) {
			return true
		}
		from = pos + len(sentinel)
	}
}

// countSentinels counts exact-line sentinel occurrences on a whole screen. A
// fix message quotes the sentinel mid-line, so exact-line matching keeps the
// injected echo from counting as a fresh completion.
func countSentinels(screen, runID string) int {
	sentinel := CompletionSentinel(runID)
	n := 0
	for _, line := range strings.Split(screen, "\n") {
		if strings.TrimSpace(line) == sentinel {
			n++
		}
	}
	return n
}

// memoryFixAsk starts the last line of every memory fix prompt.
const memoryFixAsk = "Fix MEMORY.md. Trebi checks the file again when you finish. Then "

// IsMemoryFix tells if a session prompt is a memory fix that the daemon wrote.
func IsMemoryFix(text string) bool {
	return strings.Contains(text, "\n"+memoryFixAsk)
}

// completionMark matches the sentinel with its key, the markdown marks around
// it, and one closing "." or "!".
var completionMark = regexp.MustCompile("[`*_~]*" + CompletionPrefix + "[A-Za-z0-9_-]*[`*_~]*[.!]?")

// StripCompletion removes the completion sentinel from assistant text. A line
// that holds nothing else is dropped; in any other line, only the sentinel is cut.
func StripCompletion(text string) string {
	if !strings.Contains(text, CompletionPrefix) {
		return text
	}
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !strings.Contains(l, CompletionPrefix) {
			kept = append(kept, l)
			continue
		}
		rest := completionMark.ReplaceAllString(l, "")
		if strings.Trim(rest, " \t-*+>\u2022`_~") != "" {
			kept = append(kept, rest)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// NoReplyToken is the text an agent writes when a chat message needs no reply.
const NoReplyToken = job.NoReplyToken

// IsNoReply reports whether text holds NoReplyToken anywhere.
func IsNoReply(text string) bool { return strings.Contains(text, NoReplyToken) }

// ChannelText is the text that goes to a chat for one assistant block: ""
// when IsNoReply(text); otherwise StripCompletion(text), minus each line that
// contains FailurePrefix, trimmed.
func ChannelText(text string) string {
	if IsNoReply(text) {
		return ""
	}
	text = StripCompletion(text)
	if !strings.Contains(text, FailurePrefix) {
		return strings.TrimSpace(text)
	}
	var kept []string
	for _, l := range strings.Split(text, "\n") {
		if !strings.Contains(l, FailurePrefix) {
			kept = append(kept, l)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// harnessOn reports whether the user turned on the harness in config.
func harnessOn(cfg *config.Config, name string) bool {
	if cfg == nil {
		return true // a broken config fails the launch elsewhere with its own text
	}
	return cfg.HarnessEnabled(name)
}

// runRuntimeName names the runtime a launch will use, for error messages.
func runRuntimeName(s *Service, tool agentcli.Tool, opts LaunchOpts) string {
	if s.wantsCLI(tool, opts) {
		return "cli"
	}
	return "tui"
}

// fixMessage is the text injected into the pane when the memory gate fails.
func fixMessage(issues memoryIssues, runID string) string {
	return strings.Join(issues.lines(), "\n") + "\n" + memoryFixAsk + "print the completion line again: `" + CompletionSentinel(runID) + "`"
}

// failurePromptSentence asks a job run to report a task it cannot do.
func failurePromptSentence(key string) string {
	return fmt.Sprintf("\n\nIf you cannot do this task, stop and do not work around the block. End your last message with this line and the reason in one line: %s: <reason>",
		FailureSentinel(key))
}

// MessageResult tells how SendMessage delivered a message. Delivery is
// steered, prompted, held, resumed, or pane.
type MessageResult = DeliverResult

// SendMessage delivers a person's message and its images to the session of
// runID in its current state: the live turn, the next turn, a resume, or the
// pane. The pane gets the image paths in the text.
func (s *Service) SendMessage(ctx context.Context, runID, text string, atts ...uploads.Attachment) (MessageResult, error) {
	// The sentinel is how the agent reports it finished. A caller that sends it
	// ends the run as complete without the work being done. Text is validated
	// before run state, so the message is rejected for the real reason.
	if strings.Contains(text, CompletionPrefix) {
		return MessageResult{}, fmt.Errorf("%w: %s", ErrSentinelInMessage, runID)
	}
	run, err := s.Get(ctx, runID)
	if err != nil {
		return MessageResult{}, err
	}
	images := uploads.Attachments(atts)
	if run.Status == StatusRunning && s.cliRunFor(runID) == nil {
		if err := s.sendToPane(ctx, run, images.Inline(text)); err != nil {
			return MessageResult{}, err
		}
		detail := map[string]string{"text": trunc(text, maxMessageEventBytes), "origin": OriginMessage, "delivery": DeliveryPane}
		if len(images) > 0 {
			detail["attachments"] = strings.Join(images.IDs(), ",")
		}
		s.AddEvent(ctx, runID, "user", "trebi", detail)
		return MessageResult{Delivery: DeliveryPane, RunID: runID, Turn: run.TurnCount}, nil
	}
	p := s.parkerOf()
	if p == nil {
		if run.Status != StatusRunning {
			return MessageResult{}, fmt.Errorf("%w: %s", ErrNotRunning, runID)
		}
		d, err := s.writeLive(ctx, runID, text, images, OriginMessage)
		return MessageResult{Delivery: string(d), RunID: runID, Turn: run.TurnCount}, err
	}
	return p.Deliver(ctx, runID, ParkDelivery{Gate: GateMessage, Prompt: text, Attachments: images, At: time.Now().UTC()})
}

// SendLive sends a message only to a running run. It never resumes one.
func (s *Service) SendLive(ctx context.Context, runID, text string) error {
	run, err := s.Get(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != StatusRunning {
		return fmt.Errorf("%w: %s", ErrNotRunning, runID)
	}
	_, err = s.SendMessage(ctx, runID, text)
	return err
}

func (s *Service) sendToPane(ctx context.Context, r *Run, text string) error {
	if r.Harness == "fake" {
		return nil
	}
	if r.PaneSession == "" {
		return fmt.Errorf("run %s has no pane", r.ID)
	}
	if err := s.panes.Send(ctx, r.PaneSession, text); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	return s.panes.Key(ctx, r.PaneSession, "enter")
}

func fakeEnv(run *Run, opts LaunchOpts, dir string) []string {
	env := append(os.Environ(),
		"TREBI_RUN_ID="+run.ID,
		"TREBI_RUN_COMPLETE="+CompletionSentinel(run.sentinelKey()),
		"TREBI_SESSION_ID="+opts.SessionID,
		"TREBI_FAKE_PROMPT="+opts.Prompt,
		"TREBI_RESUME_SESSION_ID="+opts.ResumeSessionID,
	)
	if opts.ActorToken != "" {
		env = append(env, "TREBI_RUN_TOKEN="+opts.ActorToken)
	}
	for k, v := range opts.Env {
		env = append(env, k+"="+v)
	}
	if opts.Memory != nil {
		env = append(env, "TREBI_MEMORY_PATH="+filepath.Join(opts.Memory.Root, opts.Memory.Rel))
	}
	if path := opts.fakeTranscriptPath(dir); path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		env = append(env, "TREBI_TRANSCRIPT_PATH="+path)
	}
	return env
}

// Kill terminates a running run (interrupt + graceful close, or Ctrl+C then
// kill session for TUI runs). Idempotent for terminal runs.
func (s *Service) Kill(ctx context.Context, runID string) error {
	run, err := s.Get(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != StatusRunning {
		// already terminal — success (idempotent)
		return nil
	}
	usage := s.teardownRuntime(ctx, run)
	return s.Terminate(ctx, runID, StatusExited, EndKill, usage)
}

// MaxResumeAttempts caps the reopens with reason resume of one run. A
// follow-up or a request answer does not count toward it.
const MaxResumeAttempts = 5

// ResumeOpts selects why a session continues and with which prompt.
type ResumeOpts struct {
	Reason      RunReason
	Prompt      string
	Attachments uploads.Attachments
	// QueueID is the queue row that the new turn serves.
	QueueID string
	// The chain frame of the new turn: a reply continues the caller frame.
	TraceID string
	HopID   string
	Path    []string
}

// Resume reopens fromID as its next turn: the harness continues the same
// session. base holds the launch options rebuilt from the job file (model,
// env, memory, kit); Resume sets the session and the working directory from
// the run row.
func (s *Service) Resume(ctx context.Context, fromID string, base LaunchOpts, o ResumeOpts) (*Run, error) {
	prev, err := s.Get(ctx, fromID)
	if err != nil {
		return nil, err
	}
	if prev.SessionID == "" {
		return nil, fmt.Errorf("run %s: %w", fromID, ErrNoSession)
	}
	// A plain resume of a run that finished its job has nothing to continue.
	// A message or a request answer continues the session with new input.
	if prev.Status == StatusComplete && (prev.EndReason == EndResult || prev.EndReason == EndSentinel) &&
		o.Reason != ReasonMessage && o.Reason != ReasonRequest {
		return nil, fmt.Errorf("run %s finished successfully; replay it for a fresh run", fromID)
	}
	if o.Reason == "" {
		o.Reason = ReasonResume
	}
	attempts := prev.ResumeAttempts
	if o.Reason == ReasonResume {
		if prev.ResumeAttempts >= MaxResumeAttempts {
			s.emitResumeExhausted(ctx, prev)
			return nil, fmt.Errorf("run %s reached the resume cap (%d attempts)", fromID, MaxResumeAttempts)
		}
		attempts++
	}
	if o.Prompt == "" {
		o.Prompt = "Continue from where you left off."
	}
	opts := base
	opts.WorkspaceID, opts.JobID, opts.JobName, opts.Kind = prev.Workspace, prev.JobID, prev.JobName, prev.Kind
	opts.Reason = o.Reason
	opts.Harness = prev.Harness
	opts.Runtime = prev.Runtime
	opts.ReopenRunID = fromID
	opts.QueueID = o.QueueID
	opts.TraceID, opts.HopID, opts.ChainPath = cmp.Or(o.TraceID, opts.TraceID), o.HopID, o.Path
	opts.ResumeAttempts = attempts
	opts.ResumeSessionID = prev.harnessSessionID()
	opts.SessionID = ""
	opts.WorkingDir = prev.WorkingDir
	opts.ConversationKey, opts.ConversationID = prev.ConversationKey, prev.ConversationID
	opts.Prompt = o.Prompt
	opts.PromptPreview = o.Prompt
	opts.Attachments = o.Attachments
	opts.BypassOverlap = true
	opts.Wake = nil
	if opts.MaxDurationMS == 0 && opts.IdleTimeoutMS == 0 && opts.KeepAliveMS == 0 {
		opts.MaxDurationMS, opts.IdleTimeoutMS, opts.KeepAliveMS = prev.Session.MaxDurationMS, prev.Session.IdleTimeoutMS, prev.Session.KeepAliveMS
	}
	if len(opts.JobConnectors) == 0 {
		// Resolve the names again: the run gets the current definitions
		// and fresh secrets.
		opts.JobConnectors = s.RunConnectors(ctx, fromID)
	}
	run, _, err := s.CreateAndLaunch(ctx, opts)
	if err != nil {
		if errors.Is(err, agentwire.ErrResumeUnsupported) || errors.Is(err, agentcli.ErrResumeUnsupported) {
			return run, fmt.Errorf("%w: %w", ErrResumeUnsupported, err)
		}
		return run, err
	}
	return run, nil
}

// RunConnectors reads the connection ids a run launched with. A resume
// resolves the ids, so a slug change does not break it.
func (s *Service) RunConnectors(ctx context.Context, runID string) []string {
	var raw sql.NullString
	if err := s.st.DB().QueryRowContext(ctx, `SELECT connectors_json FROM runs WHERE id = ?`, runID).Scan(&raw); err != nil || !raw.Valid {
		return nil
	}
	var names []string
	_ = json.Unmarshal([]byte(raw.String), &names) // a bad row resumes without actions
	return names
}

// commandUnavailable reports a bare command whose binary is not on PATH.
// joinExtra appends extra text to a base body (AppendExtraPrompt semantics;
// request is a leaf this package must not import).
func joinExtra(base, extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return base
	}
	base = strings.TrimRight(base, "\n")
	if base == "" {
		return extra
	}
	return base + "\n" + extra
}

func commandUnavailable(command string) bool {
	resolved := resolveCommand(command)
	bin := strings.Fields(resolved)[0]
	if strings.Contains(bin, "/") {
		return false
	}
	_, err := exec.LookPath(bin)
	return err != nil
}

// detectTool detects the harness tool, stamps opts.Harness, and installs
// hooks. A variant keeps its name and gets the tool with its home.
func detectTool(command string, opts *LaunchOpts) agentcli.Tool {
	tool := agentcli.Detect(command)
	if v, ok := agentcli.ByName(opts.Harness); ok && opts.Command == "" && opts.Harness != "" &&
		opts.Harness != tool.Name() && v.Name() == tool.Name() {
		tool = v
	} else {
		opts.Harness = tool.Name()
	}
	if opts.Harness == "" || opts.Harness == "unknown" {
		opts.Harness = command
	}
	// Install lifecycle hooks into harness home (best-effort).
	if life := tool.Lifecycle(); life != nil {
		_ = life.Install(config.BaseDir(), config.BinDir())
	}
	return tool
}

// preparePlan builds the launch or resume plan and stamps session identity.
func preparePlan(tool agentcli.Tool, command string, opts *LaunchOpts) (agentcli.LaunchPlan, error) {
	if opts.ResumeSessionID != "" {
		plan, err := tool.PrepareResume(command, opts.ResumeSessionID)
		if err != nil {
			return plan, fmt.Errorf("resume: %w", err)
		}
		opts.applyResumePlan(&plan)
		return plan, nil
	}
	plan := tool.PrepareLaunch(command)
	plan.Command = agentcli.ApplyModelEffort(plan.Command, tool.Name(), opts.Model, opts.Effort)
	if plan.SessionID != "" {
		opts.SessionID = plan.SessionID
	}
	return plan, nil
}

// injectMCPPlan hands the MCP actions to a pane tool, for a launch and for a
// resume. The CLI runtime never calls it: agent-wire takes the servers from
// StartRequest.MCPServers. A tool without the injector cannot reach this
// point: resolveActions already refused the launch.
func injectMCPPlan(tool agentcli.Tool, plan *agentcli.LaunchPlan, opts *LaunchOpts) error {
	servers := mcpServers(opts.Actions)
	if len(servers) == 0 {
		return nil
	}
	inj, ok := tool.(agentcli.MCPInjector)
	if !ok {
		return fmt.Errorf("%w: %s cannot take an injected MCP server", ErrActionsUnsupported, tool.Name())
	}
	// The injector sees the job env, so it can merge into a value there
	// (OPENCODE_CONFIG_CONTENT) or refuse a conflict (codex).
	for k, v := range opts.Env {
		if _, ok := plan.Env[k]; ok {
			continue
		}
		if plan.Env == nil {
			plan.Env = map[string]string{}
		}
		plan.Env[k] = v
	}
	// The private files carry the run id, so the id exists before Create.
	if opts.ID == "" {
		opts.ID = ulid.Make().String()
	}
	if err := inj.InjectMCP(plan, servers, runFiles(opts.ID)); err != nil {
		return fmt.Errorf("%w: %s", ErrActionsInvalid, err)
	}
	return nil
}

// runFiles names the private files of one run. They sit next to the run
// log, never in the job directory.
func runFiles(runID string) agentcli.RunFiles {
	return agentcli.RunFiles{Dir: home.RunsLogDir(), RunID: runID}
}

// RemoveStaleRunFiles removes the private files of runs that are terminal
// or unknown. The daemon calls it at start, after a crash could leave them.
func (s *Service) RemoveStaleRunFiles(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(home.RunsLogDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		id, _, ok := strings.Cut(e.Name(), "-")
		if !ok || e.IsDir() || len(id) != ulid.EncodedSize {
			continue
		}
		var status string
		err := s.st.DB().QueryRowContext(ctx, `SELECT status FROM runs WHERE id=?`, id).Scan(&status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return removed, err
		}
		if err == nil && !Status(status).IsTerminal() {
			continue
		}
		if err := os.Remove(filepath.Join(home.RunsLogDir(), e.Name())); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// sessionTitle labels the pane with the job name and time.
func sessionTitle(jobName string) string {
	title := jobName
	if title == "" {
		title = "trebi"
	}
	return title + " - " + time.Now().Format("01-02 15:04")
}

// paneEnvFile writes launch secrets to a 0600 file and returns one source
// line. The line removes the file after sourcing, so values do not remain in
// shell history or pane scrollback.
func paneEnvFile(runDir, runID, token, harnessName, toolName string, actEnv, optEnv, planEnv map[string]string) (string, string, error) {
	if runDir == "" {
		runDir = "."
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return "", "", err
	}
	path := filepath.Join(runDir, ".trebi-env-"+runID+".sh")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	// Order: harness secret env, action env, opts.Env, plan.Env, then the run
	// identity, which always wins.
	env := map[string]string{}
	if secretEnv, err := harness.LoadSecretEnv(cmp.Or(harnessName, toolName)); err == nil {
		for k, v := range secretEnv {
			env[k] = v
		}
	}
	for k, v := range actEnv {
		env[k] = v
	}
	for k, v := range optEnv {
		env[k] = v
	}
	for k, v := range planEnv {
		env[k] = v
	}
	env["TREBI_RUN_ID"] = runID
	env["TREBI_RUN_TOKEN"] = token
	env["TREBI_HOOK_TOOL"] = toolName
	binCurrent := filepath.Join(config.BinDir(), toolName, "current")
	if st, err := os.Stat(binCurrent); err == nil && st.IsDir() {
		env["PATH"] = binCurrent + ":" + basePath(actEnv)
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(env[k]))
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = os.Remove(path)
		return "", "", err
	}
	return " . " + shellQuote(path) + " && rm -f " + shellQuote(path), path, nil
}

func (s *Service) sendPrompt(ctx context.Context, session, prompt, harness string) error {
	if err := s.panes.Send(ctx, session, prompt); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	if err := s.panes.Key(ctx, session, "enter"); err != nil {
		return err
	}
	// cursor/grok often need a second Enter
	switch harness {
	case "cursor", "grok":
		time.Sleep(time.Second)
		_ = s.panes.Key(ctx, session, "enter")
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func removeRunEnvFile(run *Run) {
	if run == nil || run.WorkingDir == "" || run.ID == "" {
		return
	}
	_ = os.Remove(filepath.Join(run.WorkingDir, ".trebi-env-"+run.ID+".sh"))
}

// harnessCommand is the command of a launch: the command, else the harness.
// A variant runs the program of its base.
func (o LaunchOpts) harnessCommand() string {
	if o.Command != "" || o.Harness == "" {
		return o.Command
	}
	if cfg := o.config(); cfg != nil {
		return cfg.HarnessBase(o.Harness)
	}
	return o.Harness
}

// harnessEnvUnder puts the env of the harness in config.yaml under env, with
// the secrets read. A job env already has it; a run with no job gets it here.
func harnessEnvUnder(cfg *config.Config, name string, env map[string]string) map[string]string {
	if cfg == nil {
		return env
	}
	base := cfg.HarnessEnv(name)
	if len(base) == 0 {
		return env
	}
	out := secrets.ResolveRefs(base)
	maps.Copy(out, env)
	return out
}

func resolveCommand(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return command
	}
	bin := fields[0]
	if strings.Contains(bin, "/") {
		return command
	}
	// Prefer TREBI_HOME/bin/<tool>/current/<tool>
	current := filepath.Join(config.BinDir(), bin, "current", bin)
	if st, err := os.Stat(current); err == nil && !st.IsDir() {
		fields[0] = current
		return strings.Join(fields, " ")
	}
	// Also try current as file symlink to binary
	link := filepath.Join(config.BinDir(), bin, "current")
	if st, err := os.Stat(link); err == nil && !st.IsDir() {
		fields[0] = link
		return strings.Join(fields, " ")
	}
	return command
}

func flattenForDetect(screen string) string {
	var b strings.Builder
	b.Grow(len(screen))
	prevSpace := false
	for _, r := range screen {
		if isDetectRune(r) {
			b.WriteRune(r)
			prevSpace = false
			continue
		}
		if !prevSpace {
			b.WriteByte(' ')
			prevSpace = true
		}
	}
	return b.String()
}

// isDetectRune keeps characters relevant to sentinel detection.
func isDetectRune(r rune) bool {
	if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
		return true
	}
	return strings.ContainsRune("_:,.!?-/", r)
}

func (s *Service) emitAuthProbeFailed(ctx context.Context, name, runID, source string) {
	_, _ = harness.New(s.st, nil, harness.Options{}).Stamp(ctx, harness.AuthStatus{
		Tool: name, Status: harness.AuthMissing, Hint: "trebi harness auth login " + name,
	}, source)
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "auth.probe.failed", Entity: "harness", EntityID: name, Action: "updated",
		Attrs: map[string]string{"harness": name, "run_id": runID, "source": source},
	}); err != nil {
		s.log.Warn("auth.probe.failed", "error", err)
	}
}

func (s *Service) stampHarnessAuthOK(ctx context.Context, name string) {
	if name == "" || name == "unknown" || name == "fake" {
		return
	}
	_, _ = harness.New(s.st, nil, harness.Options{}).Stamp(ctx, harness.AuthStatus{Tool: name, Status: harness.AuthOK}, harness.SourceLaunch)
}

func (s *Service) emitLimitExceeded(ctx context.Context, harness, runID string) {
	s.stampHarnessLimit(ctx, harness, true)
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "harness.limit_exceeded", Entity: "harness", EntityID: harness, Action: "updated",
		Attrs: map[string]string{"harness": harness, "run_id": runID},
	}); err != nil {
		s.log.Warn("harness.limit_exceeded", "error", err)
	}
}

// ClearLimit clears the limit flag of a harness when it is set. The queue
// then releases the rows that the limit blocked.
func (s *Service) ClearLimit(ctx context.Context, harness string) error {
	var hit sql.NullInt64
	err := s.st.DB().QueryRowContext(ctx, `SELECT limit_exceeded FROM harness_state WHERE name=?`, harness).Scan(&hit)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && hit.Int64 != 1) {
		return nil
	}
	if err != nil {
		return err
	}
	s.emitLimitCleared(ctx, harness)
	return nil
}

func (s *Service) emitLimitCleared(ctx context.Context, harness string) {
	s.stampHarnessLimit(ctx, harness, false)
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "harness.limit_cleared", Entity: "harness", EntityID: harness, Action: "updated",
		Attrs: map[string]string{"harness": harness},
	}); err != nil {
		s.log.Warn("harness.limit_cleared", "error", err)
	}
}

func (s *Service) emitResumeExhausted(ctx context.Context, run *Run) {
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "run.resume_exhausted", Entity: "run", EntityID: run.ID, Action: "updated",
		Attrs: map[string]string{
			"run_id": run.ID, "session_id": run.SessionID,
			"workspace": run.Workspace, "job_id": run.JobID, "job_name": run.JobName,
		},
	}); err != nil {
		s.log.Warn("run.resume_exhausted", "error", err)
	}
}

// requestMemoryFix records one completion-gate retry: a run event for the
// transcript/timeline plus an ops record so the run's Log tab shows the gate.
func (s *Service) requestMemoryFix(ctx context.Context, runID, source string, attempt int, issues memoryIssues) {
	joined := strings.Join(issues.lines(), "\n")
	s.AddEvent(ctx, runID, "memory.fix_requested", source, map[string]string{
		"issues": joined, "attempt": strconv.Itoa(attempt),
	})
	s.log.Warn("memory.fix_requested", "id", runID, "attempt", attempt, "issues", joined)
}

// recordMemoryInvalid logs the failed gate: one run event and one ops record.
// Only a damaged file gets the bus event for the alerts consumer. A sound
// file that missed its Log entry counts as valid for the alert.
func (s *Service) recordMemoryInvalid(ctx context.Context, run *Run, issues memoryIssues, source string) {
	joined := strings.Join(issues.lines(), "\n")
	s.AddEvent(ctx, run.ID, "memory.invalid", source, map[string]string{"issues": joined})
	s.log.Warn("memory.invalid", "id", run.ID, "source", source, "issues", joined)
	if !issues.broken() {
		s.recordMemoryValid(ctx, run)
		return
	}
	attrs := run.memoryAttrs()
	attrs["detail"] = issues.detail()
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "memory.invalid", Entity: "run", EntityID: run.ID, Action: "updated",
		Attrs: attrs,
	}); err != nil {
		s.log.Warn("memory.invalid append", "run", run.ID, "error", err)
	}
}

// recordMemoryValid tells the alerts consumer that the memory of the run
// owner is sound again, so it can close an open memory_invalid item.
func (s *Service) recordMemoryValid(ctx context.Context, run *Run) {
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "memory.valid", Entity: "run", EntityID: run.ID, Action: "updated",
		Attrs: run.memoryAttrs(),
	}); err != nil {
		s.log.Warn("memory.valid append", "run", run.ID, "error", err)
	}
}

// memoryAttrs names the memory owner on a memory bus event.
func (r *Run) memoryAttrs() map[string]string {
	return map[string]string{
		"run_id": r.ID, "kind": r.Kind,
		"workspace": r.Workspace, "job_id": r.JobID, "job_name": r.JobName,
	}
}

func (s *Service) stampHarnessLimit(ctx context.Context, name string, exceeded bool) {
	if name == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	flag := 0
	if exceeded {
		flag = 1
	}
	_, _ = s.st.DB().ExecContext(ctx, `INSERT INTO harness_state (name, limit_exceeded, updated_at)
		VALUES (?,?,?) ON CONFLICT(name) DO UPDATE SET limit_exceeded=excluded.limit_exceeded, updated_at=excluded.updated_at`,
		name, flag, now)
}

// applyIfRunning enforces session.if_running against live runs of the same job
// (or, for assistant, the same conversation).
// It returns the outcome ("" when no live run, else skip|coalesce|replace)
// and, for skip/coalesce, the run row the caller must return instead of a launch.
func (s *Service) applyIfRunning(ctx context.Context, opts *LaunchOpts) (string, *Run, error) {
	// Overlap is per conversation (plan E.3): an empty key matches only runs
	// with an empty key, so one busy chat does not block another.
	live, err := s.List(ctx, ListOpts{
		Workspace:            opts.WorkspaceID,
		Job:                  opts.JobID,
		Status:               string(StatusRunning),
		ConversationKey:      opts.ConversationKey,
		ConversationKeyExact: true,
		Limit:                20,
	})
	if err != nil {
		return "", nil, err
	}
	if len(live) == 0 {
		return "", nil, nil
	}
	if opts.Conversation {
		s.overlapCoalesce(ctx, opts, live[0])
		return OverlapCoalesce, live[0], nil
	}
	switch policy := resolveIfRunning(opts.cfg, opts.IfRunning); policy {
	case IfRunningQueue:
		return "", nil, ErrRunActive
	case IfRunningSkip:
		row, err := s.overlapSkip(ctx, opts, live[0])
		if err != nil {
			return "", nil, err
		}
		return IfRunningSkip, row, nil
	case IfRunningReplace:
		s.overlapReplace(ctx, live)
		return IfRunningReplace, nil, nil
	default:
		return "", nil, fmt.Errorf("unknown if_running policy %q", policy)
	}
}

// resolveIfRunning returns v, else defaults.session.if_running, else queue.
func resolveIfRunning(cfg *config.Config, v string) string {
	if v != "" {
		return v
	}
	if cfg != nil && cfg.Defaults.Session.IfRunning != "" {
		return cfg.Defaults.Session.IfRunning
	}
	return IfRunningQueue
}

// defaultMaxDurationMS returns defaults.session.max_duration, else 24h.
func defaultMaxDurationMS(cfg *config.Config) int64 {
	if cfg == nil {
		return config.SessionDefaults{}.MaxDurationMS()
	}
	return cfg.Defaults.Session.MaxDurationMS()
}

// overlapSkip records a terminal skipped row instead of a launch.
func (s *Service) overlapSkip(ctx context.Context, opts *LaunchOpts, existing *Run) (*Run, error) {
	skipped, err := s.Create(ctx, CreateOpts{
		WorkspaceID:   opts.WorkspaceID,
		JobID:         opts.JobID,
		JobName:       opts.JobName,
		Kind:          opts.Kind,
		Reason:        opts.Reason,
		Harness:       opts.Harness,
		PromptPreview: opts.PromptPreview,
		TraceID:       opts.TraceID,
		QueueID:       opts.QueueID,
	})
	if err != nil {
		return nil, err
	}
	// The event comes first, so the run.updated frame finds the blocking run.
	s.AddEvent(ctx, skipped.ID, "skip", "runner", map[string]string{
		"because": existing.ID, "policy": IfRunningSkip,
	})
	if err := s.Terminate(ctx, skipped.ID, StatusSkipped, EndSkip, nil); err != nil {
		s.log.Warn("overlap skip terminate", "run", skipped.ID, "error", err)
		return nil, err
	}
	return s.Get(ctx, skipped.ID)
}

// overlapReplace kills every live run so a new launch can start.
func (s *Service) overlapReplace(ctx context.Context, live []*Run) {
	for _, r := range live {
		usage := s.teardownRuntime(ctx, r)
		_ = s.Terminate(ctx, r.ID, StatusExited, EndReplace, usage)
		s.AddEvent(ctx, r.ID, "replace", "runner", map[string]string{"by": "new-launch"})
	}
}

// overlapCoalesce types the new prompt into the live run's pane.
func (s *Service) overlapCoalesce(ctx context.Context, opts *LaunchOpts, existing *Run) {
	text := opts.CoalesceText
	if text == "" {
		text = opts.Prompt
	}
	if text == "" {
		text = opts.PromptPreview
	}
	if text == "" {
		text = "(coalesced event)"
	}
	if cr := s.cliRunFor(existing.ID); cr != nil {
		if _, err := s.promptCLI(ctx, existing.ID, cr, agentwire.Prompt{Text: text, Kind: "coalesce"}, nil, OriginCoalesce); err != nil {
			s.log.Warn("coalesce prompt", "run", existing.ID, "error", err)
		}
	} else if existing.PaneSession != "" {
		_ = s.panes.Send(ctx, existing.PaneSession, text)
		_ = s.panes.Key(ctx, existing.PaneSession, "enter")
	}
	s.AddEvent(ctx, existing.ID, "coalesce", "runner", map[string]string{
		"prompt_preview": trunc(text, 200),
	})
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "run.updated", Entity: "run", EntityID: existing.ID, Action: "updated",
		Attrs: map[string]any{"coalesced": true},
	}); err != nil {
		s.log.Warn("coalesce bus append", "run", existing.ID, "error", err)
	}
}

// kitPromptHint is v1's portable skills inheritance hint. Connector actions
// reach a run by injection, not by a prompt line (Part F.4).
func kitPromptHint(kit *job.KitYAML) string {
	if kit == nil || len(kit.Skills) == 0 {
		return ""
	}
	return "Preferred skills: " + strings.Join(kit.Skills, ", ")
}

// combinedSkillHint lists the preferred skills of the job and the kit. A
// non-nil used keeps only the names that the run gets.
func combinedSkillHint(kit *job.KitYAML, jobSkills []string, used map[string]bool) string {
	if kit == nil {
		kit = &job.KitYAML{}
	}
	seen := map[string]bool{}
	skills := make([]string, 0, len(jobSkills)+len(kit.Skills))
	for _, source := range [][]string{jobSkills, kit.Skills} {
		for _, skill := range source {
			key := strings.ToLower(skill)
			if seen[key] || (used != nil && !used[key]) {
				continue
			}
			seen[key] = true
			skills = append(skills, skill)
		}
	}
	return kitPromptHint(&job.KitYAML{Skills: skills})
}
