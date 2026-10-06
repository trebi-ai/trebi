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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	agentwire "github.com/trebi-ai/agent-wire"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tmux"
)

const (
	readyTimeout       = 90 * time.Second
	resumeReadyTimeout = 90 * time.Second
	CompletionPrefix   = "TREBI_RUN_COMPLETE"
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

// memoryPromptSentence is the one extra completion instruction for memory runs.
const memoryPromptSentence = "Update MEMORY.md before you print the completion line. The daemon validates the file."

// maxMemoryFixes caps the fix-injection cycle per run (B22 §3.3).
const maxMemoryFixes = 2

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
	// CoalesceText is the text a coalesce delivers to the live run. An
	// operator launch has no Prompt yet at that point.
	CoalesceText string
	// Wake asks the composer to build the prompt for an operator launch
	// (Kind assistant|agent). Ignored when Prompt is set.
	Wake *WakeOpts
	// Memory is the MEMORY.md seam; nil = memory off.
	Memory *MemoryLaunch
	// Kit is the owning agent's reusable run kit.
	Kit *job.KitYAML
	// JobSkills is the job's own preferred skill list.
	JobSkills []string
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
	// Model / Effort override the harness default for this run (launch flags).
	// Both already include harness defaults; empty = vendor default.
	Model  string
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
	return &dispatch.QueueItem{
		Connector:       connector,
		WorkspaceID:     o.WorkspaceID,
		Job:             o.Job,
		Kind:            o.Kind,
		ConversationKey: o.ConversationKey,
		Status:          dispatch.QueuePending,
		Launch:          lr,
	}
}

// wantsFake reports the fake-harness path (contract e2e without real LLM binary).
func (o *LaunchOpts) wantsFake() bool {
	return len(o.FakeCommand) > 0 || o.Harness == "fake" || os.Getenv("TREBI_FAKE_HARNESS") != ""
}

func (o *LaunchOpts) fakeTranscriptPath(dir string) string {
	name := o.Harness
	if name == "" || name == "fake" {
		name = tmux.Claude
	}
	tool, ok := tmux.ByName(name)
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
func (o *LaunchOpts) applyResumePlan(plan *tmux.LaunchPlan) {
	if plan.SessionID == "" {
		plan.SessionID = o.ResumeSessionID
	}
	o.SessionID = plan.SessionID
	// A request answer and a conversation event keep their own reason.
	if o.Reason != ReasonRequest && o.Reason != ReasonTrigger {
		o.Reason = ReasonResume
	}
}

// CreateAndLaunch inserts a run and starts the harness (tmux) or fake process.
// The second return is the overlap outcome: "" (no live run or bypass) | skip | coalesce | replace.
// On skip/coalesce the returned run is the skipped row / the live run — no launch happens.
// On queue with a live run it returns ErrRunActive and creates no row.
func (s *Service) CreateAndLaunch(ctx context.Context, opts LaunchOpts) (*Run, string, error) {
	// Decide human status before minting a token: a token identifies the run
	// process, not the human who launched it.
	humanManual := opts.CreateOpts.ActorToken == "" && opts.Reason == ReasonManual
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
		ok, reason, err := s.budgetGate.Allow(ctx, opts.WorkspaceID, opts.Job, opts.Kind)
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
			return nil, "", err
		}
	}
	var outcome string
	// Overlap policy against live runs of the same job (assistant: same conversation).
	if opts.Job != "" && opts.WorkspaceID != "" && !opts.BypassOverlap {
		oc, done, err := s.applyIfRunning(ctx, &opts)
		if err != nil {
			return nil, "", err
		}
		if done != nil {
			return done, oc, nil
		}
		outcome = oc
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
	}

	if opts.SessionID == "" && opts.ResumeSessionID == "" {
		opts.SessionID = uuid.NewString()
	}
	s.prepMemory(ctx, &opts)
	s.applyWakePrompt(ctx, &opts)
	for _, hint := range []string{connectorHint(opts.Actions), s.requestHint(opts)} {
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
		Job:                  opts.Job,
		Kind:                 opts.Kind,
		ConversationKey:      opts.ConversationKey,
		ConversationKeyExact: true,
		Limit:                20,
	})
	if err != nil {
		s.log.Warn("continue lookup", "job", opts.Job, "error", err)
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
	if tool, ok := tmux.ByName(prev.Harness); ok {
		dir := tmux.TranscriptWorkingDir(prev.Harness, prev.WorkingDir, prev.StartedAt)
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
	if r.Harness != tmux.Codex || r.Runtime != "tui" || r.SessionID == "" {
		return r.SessionID
	}
	tool, _ := tmux.ByName(tmux.Codex)
	dir := tmux.TranscriptWorkingDir(r.Harness, r.WorkingDir, r.StartedAt)
	path, err := tool.TranscriptPath(home.HarnessDir(r.Harness), dir, r.SessionID)
	if err != nil {
		return r.SessionID
	}
	return cmp.Or(tmux.CodexRolloutID(path), r.SessionID)
}

// open creates the run, or reopens opts.ReopenRunID as its next turn.
func (s *Service) open(ctx context.Context, opts LaunchOpts) (*Run, error) {
	if opts.ReopenRunID == "" {
		return s.Create(ctx, opts.CreateOpts)
	}
	return s.Reopen(ctx, opts.ReopenRunID, opts.CreateOpts)
}

// launch starts the run on the runtime that opts select.
func (s *Service) launch(ctx context.Context, opts LaunchOpts) (*Run, error) {
	if opts.ReopenRunID != "" {
		// The MCP plan and the pane env name the run before it reopens.
		opts.ID = opts.ReopenRunID
	}
	if opts.wantsFake() {
		return s.launchFake(ctx, opts)
	}
	command := opts.Command
	if command == "" {
		command = opts.Harness
	}
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
	if s.wantsCLI(tmux.Detect(command), opts) {
		return s.launchCLI(ctx, opts)
	}
	return s.launchTmux(ctx, opts)
}

// resolveActions resolves the connector names of a launch and checks the
// support matrix (decision 8) before a run row exists. The job list adds to
// the kit list. The email name is built in; the resolver gets the rest.
func (s *Service) resolveActions(ctx context.Context, opts *LaunchOpts) error {
	names := connectorNames(opts.JobConnectors, opts.Kit)
	if len(names) == 0 {
		return nil
	}
	var actions []ConnectorAction
	managed := make([]string, 0, len(names))
	for _, n := range names {
		if n == job.EmailConnector {
			actions = append(actions, s.emailAction(ctx, *opts))
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
			return fmt.Errorf("%w: %s", ErrActionsInvalid, err)
		}
		// A resume names the run's connection ids next to the kit slugs, so
		// one connection can resolve twice.
		for _, a := range resolved {
			if !slices.ContainsFunc(actions, func(b ConnectorAction) bool { return b.ID == a.ID }) {
				actions = append(actions, a)
			}
		}
	}
	opts.Actions = actions
	opts.Connectors = make([]string, 0, len(actions))
	for _, a := range actions {
		opts.Connectors = append(opts.Connectors, a.ID)
	}
	command := opts.Command
	if command == "" {
		command = opts.Harness
	}
	if command == "" {
		command = "claude"
	}
	tool := tmux.Detect(command)
	return s.ActionsSupported(ctx, tool.Name(), runRuntimeName(s, tool, *opts), hasMCP(actions))
}

// runRuntimeName names the runtime a launch will use, for error messages.
func runRuntimeName(s *Service, tool tmux.Tool, opts LaunchOpts) string {
	if s.wantsCLI(tool, opts) {
		return "cli"
	}
	return "tui"
}

// applyWakePrompt composes the prompt for an operator launch (Part F.3).
// A set Prompt or nil Wake wins. Without a composer it falls back to the
// var-expanded body plus extra prompt.
func (s *Service) applyWakePrompt(ctx context.Context, opts *LaunchOpts) {
	if opts.Prompt != "" || opts.Wake == nil {
		return
	}
	hint := combinedSkillHint(opts.Kit, opts.JobSkills)
	if opts.Kind != KindJob && opts.Kind != "" && s.composer != nil {
		wake := *opts.Wake
		wake.Memory = opts.Memory
		composed, err := s.composer.ComposeWakePrompt(ctx, WakeScope{
			WorkspaceID: opts.WorkspaceID, Kind: opts.Kind, Name: opts.Job,
		}, wake)
		if err != nil {
			s.log.Warn("wake compose failed; using raw body", "job", opts.Job, "err", err)
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

// prepMemory seeds, migrates, hashes, and injects MEMORY.md for one launch
// (B22 §3.3). It runs before wake composition so the composer reads a settled
// file. A files error skips memory for the launch (logged).
func (s *Service) prepMemory(ctx context.Context, opts *LaunchOpts) {
	mem := opts.Memory
	if mem == nil || s.fs == nil {
		return
	}
	data, _, err := s.fs.Read(ctx, mem.Root, mem.Rel)
	switch {
	case errors.Is(err, files.ErrNotFound):
		if _, _, werr := s.fs.Write(ctx, mem.Root, mem.Rel, memory.Skeleton(), ""); werr != nil {
			s.log.Warn("memory seed", "rel", mem.Rel, "err", werr)
			return
		}
		data = memory.Skeleton()
	case err != nil:
		s.log.Warn("memory read", "rel", mem.Rel, "err", err)
		return
	default:
		if doc, _ := memory.Parse(data); doc.Legacy {
			fixed := memory.Migrate(data)
			if _, _, werr := s.fs.Write(ctx, mem.Root, mem.Rel, fixed, ""); werr != nil {
				s.log.Warn("memory migrate", "rel", mem.Rel, "err", werr)
			} else {
				data = fixed
				s.log.Info("memory.migrated", "rel", mem.Rel)
			}
		}
	}
	opts.MemoryHash = memory.Hash(data)
	if opts.Kind == KindAgent {
		return // agents get memory through the wake composer
	}
	doc, _ := memory.Parse(data)
	opts.Prompt = memory.Render(doc, mem.Opt) + "\n\n" + memory.Protocol(mem.Opt, memory.NewProtocolInfo(doc, mem.Opt)) + "\n\n" + opts.Prompt
}

// memoryFor resolves the gate-time memory seam for a stored run. Nil when the
// run launched without memory or no resolver is wired.
func (s *Service) memoryFor(ctx context.Context, run *Run) *MemoryLaunch {
	if run.MemoryHash == "" || s.memRes == nil {
		return nil
	}
	return s.memRes.MemoryLaunchFor(ctx, run)
}

// gateMemory validates MEMORY.md at a completion sentinel. Empty result = pass.
func (s *Service) gateMemory(ctx context.Context, run *Run, mem *MemoryLaunch) []string {
	if mem == nil || run.MemoryHash == "" || s.fs == nil {
		return nil
	}
	data, _, err := s.fs.Read(ctx, mem.Root, mem.Rel)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			return []string{"MEMORY.md: file is missing. Restore it with the three sections."}
		}
		return []string{fmt.Sprintf("MEMORY.md: read failed: %v", err)}
	}
	issues := memory.FormatIssues("MEMORY.md", memory.Validate(data, mem.Opt))
	if mem.Opt.RequireUpdate && memory.Hash(data) == run.MemoryHash {
		issues = append(issues, memory.FormatIssues("MEMORY.md", []memory.Issue{memory.Unchanged()})...)
	}
	return issues
}

func (s *Service) launchFake(ctx context.Context, opts LaunchOpts) (*Run, error) {
	if opts.TmuxSession == "" {
		opts.TmuxSession = "trebi-fake-" + strings.ToLower(ulid.Make().String()[:8])
	}
	if opts.ResumeSessionID != "" {
		opts.SessionID = opts.ResumeSessionID
	}
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
	if run.TurnCount > 1 {
		s.appendRunLog(run.ID, output)
	} else {
		s.writeRunLog(run.ID, output)
	}
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
	lastIssues := []string(nil)
	for sc.Scan() {
		line := sc.Text()
		b.WriteString(line)
		b.WriteByte('\n')
		if strings.TrimSpace(line) != sentinel {
			continue
		}
		issues := s.gateMemory(ctx, run, opts.Memory)
		if len(issues) == 0 {
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

// launchTmux drives a harness inside a tmux pane (explicit runtime: tui).
func (s *Service) launchTmux(ctx context.Context, opts LaunchOpts) (*Run, error) {
	opts.Runtime = "tui"
	command := opts.Command
	if command == "" {
		command = opts.Harness
	}
	if command == "" {
		command = "claude"
	}
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

	s.log.Info("creating tmux session", "job", opts.Job, "cwd", cwd, "harness", tool.Name())
	wsRef, err := tmux.NewSession(cwd)
	if err != nil {
		return nil, fmt.Errorf("tmux new-session: %w", err)
	}
	opts.TmuxSession = wsRef
	_ = tmux.RenameSession(wsRef, sessionTitle(opts.Job))

	// Persist run before readiness so reaper can attribute the session.
	run, err := s.open(ctx, opts)
	if err != nil {
		_ = tmux.KillSession(wsRef)
		return nil, err
	}
	s.AddEvent(ctx, run.ID, "launch", "runner", map[string]string{
		"harness": tool.Name(), "session_id": opts.SessionID, "tmux": wsRef, "mode": "tmux",
	})

	sourceLine, envPath, err := paneEnvFile(run.WorkingDir, run.ID, opts.CreateOpts.ActorToken, tool.Name(), actionEnv(opts.Actions), opts.Env, plan.Env)
	if err != nil {
		return s.failLaunch(ctx, run, wsRef, err)
	}
	if err := tmux.Send(wsRef, sourceLine+"\n"); err != nil {
		_ = os.Remove(envPath)
		return s.failLaunch(ctx, run, wsRef, err)
	}
	if err := tmux.Send(wsRef, command+"\n"); err != nil {
		return s.failLaunch(ctx, run, wsRef, err)
	}

	prompt := opts.Prompt
	if prompt == "" {
		prompt = opts.PromptPreview
	}
	prompt += PromptSuffix(run.sentinelKey())
	if opts.Memory != nil {
		prompt += " " + memoryPromptSentence
	}

	if opts.SkipReady {
		if err := sendPrompt(wsRef, prompt, tool.Name()); err != nil {
			return s.failLaunch(ctx, run, wsRef, err)
		}
		s.SetSessionState(ctx, run.ID, SessionRunning, "launch")
		s.log.Info("run launched", "id", run.ID, "tmux", wsRef, "harness", tool.Name())
		return s.Get(ctx, run.ID)
	}

	// WaitReady must not hold the HTTP request: the CLI times out at 30s and
	// cancelling the request used to kill the pane (session_gone / launch).
	go s.finishLaunch(run, wsRef, tool, prompt, resuming)
	return s.Get(ctx, run.ID)
}

// finishLaunch waits for the harness composer, then pastes the prompt.
func (s *Service) finishLaunch(run *Run, wsRef string, tool tmux.Tool, prompt string, resuming bool) {
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
	if err := sendPrompt(session, prompt, harness); err != nil {
		_, _ = s.failLaunch(ctx, run, session, err)
		return
	}
	s.SetSessionState(ctx, run.ID, SessionRunning, "launch")
	s.log.Info("run launched", "id", run.ID, "tmux", session, "harness", harness)
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

// writeRunLog persists a run's output where handleRunLog reads it. A run that
// leaves no log answers `run logs` with not_found forever, so the agent cannot
// see what its own run did.
func (s *Service) writeRunLog(runID, out string) {
	if out == "" {
		return
	}
	path := filepath.Join(home.RunsLogDir(), runID+".log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.log.Warn("run log mkdir", "run_id", runID, "err", err)
		return
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		s.log.Warn("run log write", "run_id", runID, "err", err)
	}
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

// closeSession captures the pane before killing it. The pane is the only
// record of what a tmux run printed: once the session dies the scrollback is
// gone, so every teardown path must come through here rather than calling
// tmux.KillSession directly.
func (s *Service) closeSession(runID, session string) {
	if session == "" {
		return
	}
	if out, err := tmux.ReadHistory(session); err == nil {
		// Keep the in-run capture; a final alt-screen is often just Ctrl-C chrome.
		if !s.runLogExists(runID) {
			s.appendRunLog(runID, paneCaptureHeader(out)+"\n"+out)
		}
	} else {
		s.log.Warn("run log capture", "run_id", runID, "session", session, "err", err)
	}
	_ = tmux.KillSession(session)
}

func (s *Service) failLaunch(ctx context.Context, run *Run, wsRef string, err error) (*Run, error) {
	_ = s.TerminateTurn(ctx, run.ID, run.TurnCount, StatusFailed, EndLaunch, nil)
	s.closeSession(run.ID, wsRef)
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
func (s *Service) waitHarnessReady(ctx context.Context, run *Run, wsRef string, tool tmux.Tool, resuming bool) (ready bool, waitQ string) {
	runID := run.ID
	rt := readyTimeout
	if resuming {
		rt = resumeReadyTimeout
	}
	err := tmux.WaitReadyTool(wsRef, tool, rt)
	if err == nil {
		time.Sleep(tool.PostReadyDelay())
		s.stampHarnessAuthOK(ctx, tool.Name())
		return true, ""
	}
	var dw *tmux.DialogWaitingError
	if errors.As(err, &dw) {
		q := dw.Question
		if q == "" {
			q = "harness is waiting on a choice"
		}
		return true, q
	}
	end := EndLaunch
	switch {
	case errors.Is(err, tmux.ErrAuthMissing):
		end = EndAuth
		s.emitAuthProbeFailed(ctx, tool.Name(), runID, harness.SourceProbe)
	case errors.Is(err, tmux.ErrLimitExceeded):
		end = EndLimit
		s.emitLimitExceeded(ctx, tool.Name(), runID)
	}
	_ = s.TerminateTurn(ctx, runID, run.TurnCount, StatusFailed, end, nil)
	s.closeSession(runID, wsRef)
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

// fixMessage is the text injected into the pane when the memory gate fails.
func fixMessage(issues []string, runID string) string {
	return strings.Join(issues, "\n") + "\nFix MEMORY.md, then print the completion line again: `" + CompletionSentinel(runID) + "`"
}

// InjectMessage types a follow-up into a running run's pane.
func (s *Service) InjectMessage(ctx context.Context, runID, text string) error {
	// The sentinel is how the agent reports it finished. A caller that sends it
	// ends the run as complete without the work being done. Text is validated
	// before run state, so the message is rejected for the real reason.
	if strings.Contains(text, CompletionPrefix) {
		return fmt.Errorf("%w: %s", ErrSentinelInMessage, runID)
	}
	run, err := s.Get(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != StatusRunning {
		return fmt.Errorf("%w: %s", ErrNotRunning, runID)
	}
	// Subprocess sessions take the follow-up on stdin, never a pane paste.
	if cr := s.cliRunFor(runID); cr != nil {
		if err := cr.sess.Prompt(ctx, agentwire.Prompt{Text: text, Kind: "followup"}); err != nil {
			return err
		}
		cr.tracker.PromptWritten()
		s.SetSessionState(ctx, runID, SessionRunning, "wire")
		s.AddEvent(ctx, runID, "message", "api", map[string]string{"bytes": fmt.Sprint(len(text)), "delivery": "prompt"})
		return nil
	}
	if err := run.sendToPane(text); err != nil {
		return err
	}
	s.AddEvent(ctx, runID, "message", "api", map[string]string{"bytes": fmt.Sprint(len(text))})
	return nil
}

func (r *Run) sendToPane(text string) error {
	if r.Harness == "fake" || strings.HasPrefix(r.TmuxSession, "trebi-fake-") {
		return nil
	}
	if r.TmuxSession == "" {
		return fmt.Errorf("run %s has no tmux session", r.ID)
	}
	if err := tmux.Send(r.TmuxSession, text); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	return tmux.SendKey(r.TmuxSession, "enter")
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
	s.teardownRuntime(ctx, run)
	return s.Terminate(ctx, runID, StatusExited, EndKill, nil)
}

// MaxResumeAttempts caps the reopens with reason resume of one run. A
// follow-up or a request answer does not count toward it.
const MaxResumeAttempts = 5

// ResumeOpts selects why a session continues and with which prompt.
type ResumeOpts struct {
	Reason RunReason
	Prompt string
	// QueueID is the queue row that the new turn serves.
	QueueID string
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
	// A run that finished its job has nothing to continue: the vendor session
	// is complete, and resuming it errors out. A fresh run is the right move.
	if prev.Status == StatusComplete && (prev.EndReason == EndResult || prev.EndReason == EndSentinel) {
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
	opts.WorkspaceID, opts.Job, opts.Kind = prev.Workspace, prev.Job, prev.Kind
	opts.Reason = o.Reason
	opts.Harness = prev.Harness
	opts.Runtime = prev.Runtime
	opts.ReopenRunID = fromID
	opts.QueueID = o.QueueID
	opts.ResumeAttempts = attempts
	opts.ResumeSessionID = prev.harnessSessionID()
	opts.SessionID = ""
	opts.WorkingDir = prev.WorkingDir
	opts.ConversationKey, opts.ConversationID = prev.ConversationKey, prev.ConversationID
	opts.Prompt = o.Prompt
	opts.PromptPreview = o.Prompt
	opts.BypassOverlap = true
	opts.Wake = nil
	if opts.MaxDurationMS == 0 && opts.IdleTimeoutMS == 0 && opts.KeepAliveMS == 0 {
		opts.MaxDurationMS, opts.IdleTimeoutMS, opts.KeepAliveMS = prev.Session.MaxDurationMS, prev.Session.IdleTimeoutMS, prev.Session.KeepAliveMS
	}
	if len(opts.JobConnectors) == 0 {
		// Resolve the names again: the run gets the current definitions
		// and fresh secrets.
		opts.JobConnectors = s.runConnectors(ctx, fromID)
	}
	run, _, err := s.CreateAndLaunch(ctx, opts)
	if err != nil {
		if errors.Is(err, agentwire.ErrResumeUnsupported) || errors.Is(err, tmux.ErrResumeUnsupported) {
			return run, fmt.Errorf("%w: %w", ErrResumeUnsupported, err)
		}
		return run, err
	}
	return run, nil
}

// runConnectors reads the connection ids a run launched with. A resume
// resolves the ids, so a slug change does not break it.
func (s *Service) runConnectors(ctx context.Context, runID string) []string {
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

// detectTool detects the harness tool, stamps opts.Harness, and installs hooks.
func detectTool(command string, opts *LaunchOpts) tmux.Tool {
	tool := tmux.Detect(command)
	opts.Harness = tool.Name()
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
func preparePlan(tool tmux.Tool, command string, opts *LaunchOpts) (tmux.LaunchPlan, error) {
	if opts.ResumeSessionID != "" {
		plan, err := tool.PrepareResume(command, opts.ResumeSessionID)
		if err != nil {
			return plan, fmt.Errorf("resume: %w", err)
		}
		opts.applyResumePlan(&plan)
		return plan, nil
	}
	plan := tool.PrepareLaunch(command)
	plan.Command = tmux.ApplyModelEffort(plan.Command, tool.Name(), opts.Model, opts.Effort)
	if plan.SessionID != "" {
		opts.SessionID = plan.SessionID
	}
	return plan, nil
}

// injectMCPPlan hands the MCP actions to a pane tool, for a launch and for a
// resume. The CLI runtime never calls it: agent-wire takes the servers from
// StartRequest.MCPServers. A tool without the injector cannot reach this
// point: resolveActions already refused the launch.
func injectMCPPlan(tool tmux.Tool, plan *tmux.LaunchPlan, opts *LaunchOpts) error {
	servers := mcpServers(opts.Actions)
	if len(servers) == 0 {
		return nil
	}
	inj, ok := tool.(tmux.MCPInjector)
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
func runFiles(runID string) tmux.RunFiles {
	return tmux.RunFiles{Dir: home.RunsLogDir(), RunID: runID}
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

// sessionTitle labels the tmux session with the job name and time.
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
func paneEnvFile(runDir, runID, token, toolName string, actEnv, optEnv, planEnv map[string]string) (string, string, error) {
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
	if secretEnv, err := harness.LoadSecretEnv(toolName); err == nil {
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

func sendPrompt(session, prompt, harness string) error {
	if err := tmux.Send(session, prompt); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	if err := tmux.SendKey(session, "enter"); err != nil {
		return err
	}
	// cursor/grok often need a second Enter
	switch harness {
	case "cursor", "grok":
		time.Sleep(time.Second)
		_ = tmux.SendKey(session, "enter")
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

func buildEnvExport(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	var b strings.Builder
	for k, v := range env {
		if k == "" {
			continue
		}
		// shell-safe single-quote
		esc := strings.ReplaceAll(v, "'", `'\''`)
		fmt.Fprintf(&b, "%s='%s' ", k, esc)
	}
	return strings.TrimSpace(b.String())
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
			"workspace": run.Workspace, "job": run.Job,
		},
	}); err != nil {
		s.log.Warn("run.resume_exhausted", "error", err)
	}
}

// requestMemoryFix records one completion-gate retry: a run event for the
// transcript/timeline plus an ops record so the run's Log tab shows the gate.
func (s *Service) requestMemoryFix(ctx context.Context, runID, source string, attempt int, issues []string) {
	joined := strings.Join(issues, "\n")
	s.AddEvent(ctx, runID, "memory.fix_requested", source, map[string]string{
		"issues": joined, "attempt": strconv.Itoa(attempt),
	})
	s.log.Warn("memory.fix_requested", "id", runID, "attempt", attempt, "issues", joined)
}

// recordMemoryInvalid logs the failed gate: one run event, one ops record, and
// one bus event for the alerts consumer (first issue line rides the frame).
func (s *Service) recordMemoryInvalid(ctx context.Context, run *Run, issues []string, source string) {
	joined := strings.Join(issues, "\n")
	s.AddEvent(ctx, run.ID, "memory.invalid", source, map[string]string{"issues": joined})
	s.log.Warn("memory.invalid", "id", run.ID, "source", source, "issues", joined)
	attrs := map[string]string{
		"run_id": run.ID, "kind": run.Kind,
		"workspace": run.Workspace, "job": run.Job,
	}
	if len(issues) > 0 {
		attrs["issue"] = issues[0]
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "memory.invalid", Entity: "run", EntityID: run.ID, Action: "updated",
		Attrs: attrs,
	}); err != nil {
		s.log.Warn("memory.invalid append", "run", run.ID, "error", err)
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
		Job:                  opts.Job,
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
	switch policy := resolveIfRunning(opts.IfRunning); policy {
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
func resolveIfRunning(v string) string {
	if v != "" {
		return v
	}
	if cfg, err := config.Load(); err == nil && cfg != nil && cfg.Defaults.Session.IfRunning != "" {
		return cfg.Defaults.Session.IfRunning
	}
	return IfRunningQueue
}

// overlapSkip records a terminal skipped row instead of a launch.
func (s *Service) overlapSkip(ctx context.Context, opts *LaunchOpts, existing *Run) (*Run, error) {
	skipped, err := s.Create(ctx, CreateOpts{
		WorkspaceID:   opts.WorkspaceID,
		Job:           opts.Job,
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
	if err := s.Terminate(ctx, skipped.ID, StatusSkipped, EndSkip, nil); err != nil {
		s.log.Warn("overlap skip terminate", "run", skipped.ID, "error", err)
		return nil, err
	}
	s.AddEvent(ctx, skipped.ID, "skip", "runner", map[string]string{
		"because": existing.ID, "policy": IfRunningSkip,
	})
	return s.Get(ctx, skipped.ID)
}

// overlapReplace kills every live run so a new launch can start.
func (s *Service) overlapReplace(ctx context.Context, live []*Run) {
	for _, r := range live {
		s.teardownRuntime(ctx, r)
		_ = s.Terminate(ctx, r.ID, StatusExited, EndReplace, nil)
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
		if err := cr.sess.Prompt(ctx, agentwire.Prompt{Text: text, Kind: "coalesce"}); err == nil {
			cr.tracker.PromptWritten()
			s.SetSessionState(ctx, existing.ID, SessionRunning, "wire")
		}
	} else if existing.TmuxSession != "" {
		_ = tmux.Send(existing.TmuxSession, text)
		_ = tmux.SendKey(existing.TmuxSession, "enter")
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

func combinedSkillHint(kit *job.KitYAML, jobSkills []string) string {
	if kit == nil {
		kit = &job.KitYAML{}
	}
	seen := map[string]bool{}
	skills := make([]string, 0, len(jobSkills)+len(kit.Skills))
	for _, source := range [][]string{jobSkills, kit.Skills} {
		for _, skill := range source {
			key := strings.ToLower(skill)
			if seen[key] {
				continue
			}
			seen[key] = true
			skills = append(skills, skill)
		}
	}
	return kitPromptHint(&job.KitYAML{Skills: skills})
}
