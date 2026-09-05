package runs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
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

// LaunchOpts controls how CreateAndLaunch runs a harness.
type LaunchOpts struct {
	CreateOpts
	// Prompt full body (not just preview).
	Prompt string
	// Command override (bare harness name or full argv string). Empty → harness name.
	Command string
	// ResumeSessionID warm-resumes that session.
	ResumeSessionID string
	// Fake: run a local shell command instead of a real harness (tests/CI).
	FakeCommand []string
	// SkipReady skips WaitReady (tests).
	SkipReady bool
	// Env extra env for the pane.
	Env map[string]string
	// IfRunning policy for overlap: skip|replace|coalesce|allow.
	IfRunning string
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
// It keeps a caller-set ResumedFrom (predecessor run ULID) so the
// B17 UPDATE in Create matches the predecessor row (not a session UUID).
func (o *LaunchOpts) applyResumePlan(plan *tmux.LaunchPlan) {
	if plan.SessionID == "" {
		plan.SessionID = o.ResumeSessionID
	}
	o.SessionID = plan.SessionID
	if o.ResumedFrom == "" {
		o.ResumedFrom = o.ResumeSessionID
	}
	o.Reason = ReasonResume
}

// CreateAndLaunch inserts a run and starts the harness (tmux) or fake process.
// The second return is the if_running outcome: "" (no live run) | skip | coalesce | replace | allow.
// On skip/coalesce the returned run is the skipped row / the live run — no launch happens.
func (s *Service) CreateAndLaunch(ctx context.Context, opts LaunchOpts) (*Run, string, error) {
	if opts.IfRunning == "" {
		opts.IfRunning = "skip"
	}
	var outcome string
	// Overlap policy against live runs of same job.
	if opts.Job != "" && opts.WorkspaceID != "" {
		oc, done, err := s.applyIfRunning(ctx, &opts)
		if err != nil {
			return nil, "", err
		}
		if done != nil {
			return done, oc, nil
		}
		outcome = oc
	}

	if opts.SessionID == "" && opts.ResumeSessionID == "" {
		opts.SessionID = uuid.NewString()
	}
	if opts.PromptPreview == "" {
		opts.PromptPreview = opts.Prompt
	}

	if opts.wantsFake() {
		run, err := s.launchFake(ctx, opts)
		return run, outcome, err
	}

	command := opts.Command
	if command == "" {
		command = opts.Harness
	}
	// If binary not on PATH and no pin, fall back to fake when
	// TREBI_ALLOW_FAKE_FALLBACK=1, else fail fast.
	if command != "" && commandUnavailable(command) {
		if os.Getenv("TREBI_ALLOW_FAKE_FALLBACK") == "1" {
			run, err := s.launchFake(ctx, opts)
			return run, outcome, err
		}
		run, err := s.failFast(ctx, opts)
		return run, outcome, err
	}

	run, err := s.launchTmux(ctx, opts)
	return run, outcome, err
}

func (s *Service) launchFake(ctx context.Context, opts LaunchOpts) (*Run, error) {
	if opts.TmuxSession == "" {
		opts.TmuxSession = "trebi-fake-" + strings.ToLower(ulid.Make().String()[:8])
	}
	run, err := s.Create(ctx, opts.CreateOpts)
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
			cmd = fmt.Sprintf("echo working; echo %s", CompletionSentinel(run.ID))
		}
		cmdArgs = []string{"sh", "-c", cmd}
	}
	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = dir
	cmd.Env = fakeEnv(run, opts, dir)
	out, err := cmd.CombinedOutput()
	s.writeRunLog(run.ID, string(out))
	output := string(out)
	end := EndLaunch
	status := StatusFailed
	if strings.Contains(output, CompletionSentinel(run.ID)) {
		end = EndSentinel
		status = StatusComplete
	} else if err != nil && ctx.Err() != nil {
		end = EndMaxDuration
		status = StatusTimedOut
	}
	tokens := &pricing.TokenUsage{Input: 1, Output: 1, Model: opts.Harness}
	if termErr := s.Terminate(ctx, run.ID, status, end, tokens); termErr != nil {
		return run, fmt.Errorf("terminate: %w", termErr)
	}
	return s.Get(ctx, run.ID)
}

func (s *Service) launchTmux(ctx context.Context, opts LaunchOpts) (*Run, error) {
	command := opts.Command
	if command == "" {
		command = opts.Harness
	}
	if command == "" {
		command = "claude"
	}
	// Prefer pinned binary under TREBI_HOME/bin/<tool>/current.
	command = resolveCommand(command)

	if opts.WorkingDir == "" {
		opts.WorkingDir, _ = os.Getwd()
	}
	cwd := opts.WorkingDir

	tool := detectTool(command, &opts)

	resuming := opts.ResumeSessionID != ""
	plan, err := preparePlan(tool, command, &opts)
	if err != nil {
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
	run, err := s.Create(ctx, opts.CreateOpts)
	if err != nil {
		_ = tmux.KillSession(wsRef)
		return nil, err
	}
	s.AddEvent(ctx, run.ID, "launch", "runner", map[string]string{
		"harness": tool.Name(), "session_id": opts.SessionID, "tmux": wsRef, "mode": "tmux",
	})

	envExport := paneEnvExport(run.ID, tool.Name(), opts.Env, plan.Env)
	if err := tmux.Send(wsRef, envExport+"\n"); err != nil {
		return s.failLaunch(ctx, run, wsRef, err)
	}
	if err := tmux.Send(wsRef, command+"\n"); err != nil {
		return s.failLaunch(ctx, run, wsRef, err)
	}

	prompt := opts.Prompt
	if prompt == "" {
		prompt = opts.PromptPreview
	}
	prompt += PromptSuffix(run.ID)

	if opts.SkipReady {
		if err := sendPrompt(wsRef, prompt, tool.Name()); err != nil {
			return s.failLaunch(ctx, run, wsRef, err)
		}
		s.SetAgentState(ctx, run.ID, AgentRunning, "launch")
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
	if !s.waitHarnessReady(ctx, run.ID, wsRef, tool, resuming) {
		return
	}
	if err := sendPrompt(wsRef, prompt, tool.Name()); err != nil {
		_, _ = s.failLaunch(ctx, run, wsRef, err)
		return
	}
	s.SetAgentState(ctx, run.ID, AgentRunning, "launch")
	s.log.Info("run launched", "id", run.ID, "tmux", wsRef, "harness", tool.Name())
}

// failFast records a failed run so the API does not hang on WaitReady.
func (s *Service) failFast(ctx context.Context, opts LaunchOpts) (*Run, error) {
	run, err := s.Create(ctx, opts.CreateOpts)
	if err != nil {
		return nil, err
	}
	_ = s.Terminate(ctx, run.ID, StatusFailed, EndLaunch, nil)
	return s.Get(ctx, run.ID)
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

// closeSession captures the pane before killing it. The pane is the only
// record of what a tmux run printed: once the session dies the scrollback is
// gone, so every teardown path must come through here rather than calling
// tmux.KillSession directly.
func (s *Service) closeSession(runID, session string) {
	if session == "" {
		return
	}
	if out, err := tmux.ReadHistory(session); err == nil {
		s.writeRunLog(runID, out)
	} else {
		s.log.Warn("run log capture", "run_id", runID, "session", session, "err", err)
	}
	_ = tmux.KillSession(session)
}

func (s *Service) failLaunch(ctx context.Context, run *Run, wsRef string, err error) (*Run, error) {
	_ = s.Terminate(ctx, run.ID, StatusFailed, EndLaunch, nil)
	s.closeSession(run.ID, wsRef)
	got, _ := s.Get(ctx, run.ID)
	if got != nil {
		return got, fmt.Errorf("launch: %w", err)
	}
	return run, fmt.Errorf("launch: %w", err)
}

// waitHarnessReady waits for the harness prompt. False means the launch
// failed and the run is already terminal.
func (s *Service) waitHarnessReady(ctx context.Context, runID, wsRef string, tool tmux.Tool, resuming bool) bool {
	rt := readyTimeout
	if resuming {
		rt = resumeReadyTimeout
	}
	if err := tmux.WaitReadyTool(wsRef, tool, rt); err != nil {
		end := EndLaunch
		switch {
		case errors.Is(err, tmux.ErrAuthMissing):
			end = EndAuth
			s.emitAuthProbeFailed(ctx, tool.Name(), runID, harness.SourceProbe)
		case errors.Is(err, tmux.ErrLimitExceeded):
			end = EndLimit
			s.emitLimitExceeded(ctx, tool.Name(), runID)
		}
		_ = s.Terminate(ctx, runID, StatusFailed, end, nil)
		s.closeSession(runID, wsRef)
		return false
	}
	time.Sleep(tool.PostReadyDelay())
	s.stampHarnessAuthOK(ctx, tool.Name())
	return true
}

// DetectCompletion reports whether the pane shows the run's sentinel.
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
		"TREBI_RUN_COMPLETE="+CompletionSentinel(run.ID),
		"TREBI_SESSION_ID="+opts.SessionID,
	)
	if path := opts.fakeTranscriptPath(dir); path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		env = append(env, "TREBI_TRANSCRIPT_PATH="+path)
	}
	return env
}

// Kill terminates a running run (Ctrl+C then kill session). Idempotent for terminal runs.
func (s *Service) Kill(ctx context.Context, runID string) error {
	run, err := s.Get(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != StatusRunning {
		// already terminal — success (idempotent)
		return nil
	}
	if run.TmuxSession != "" {
		_ = tmux.SendKey(run.TmuxSession, "C-c")
		time.Sleep(200 * time.Millisecond)
		s.closeSession(runID, run.TmuxSession)
	}
	return s.Terminate(ctx, runID, StatusExited, EndKill, nil)
}

// MaxResumeAttempts caps the resume chain length for one session.
const MaxResumeAttempts = 5

// Resume launches a new run that resumes the prior session.
func (s *Service) Resume(ctx context.Context, fromID string) (*Run, error) {
	prev, err := s.Get(ctx, fromID)
	if err != nil {
		return nil, err
	}
	if prev.SessionID == "" {
		return nil, fmt.Errorf("run %s has no session_id to resume", fromID)
	}
	if prev.ResumeAttempts >= MaxResumeAttempts {
		s.emitResumeExhausted(ctx, prev)
		return nil, fmt.Errorf("run %s reached the resume cap (%d attempts)", fromID, MaxResumeAttempts)
	}
	opts := LaunchOpts{
		CreateOpts: CreateOpts{
			WorkspaceID:    prev.Workspace,
			Job:            prev.Job,
			Kind:           prev.Kind,
			Reason:         ReasonResume,
			Harness:        prev.Harness,
			PromptPreview:  "(resume)",
			ResumedFrom:    fromID,
			ResumeAttempts: prev.ResumeAttempts + 1,
			MaxDurationMS:  prev.Session.MaxDurationMS,
			IdleTimeoutMS:  prev.Session.IdleTimeoutMS,
			KeepAliveMS:    prev.Session.KeepAliveMS,
		},
		ResumeSessionID: prev.SessionID,
		Prompt:          "Continue from where you left off.",
		IfRunning:       "allow",
	}
	run, _, err := s.CreateAndLaunch(ctx, opts)
	return run, err
}

// commandUnavailable reports a bare command whose binary is not on PATH.
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
	if plan.SessionID != "" {
		opts.SessionID = plan.SessionID
	}
	return plan, nil
}

// sessionTitle labels the tmux session with the job name and time.
func sessionTitle(jobName string) string {
	title := jobName
	if title == "" {
		title = "trebi"
	}
	return title + " - " + time.Now().Format("01-02 15:04")
}

// paneEnvExport builds the export line typed into the pane. It prepends the
// pinned bin dir to PATH when the harness has one.
func paneEnvExport(runID, toolName string, optEnv, planEnv map[string]string) string {
	envExport := fmt.Sprintf("export TREBI_RUN_ID=%s TREBI_HOOK_TOOL=%s", runID, toolName)
	binCurrent := filepath.Join(config.BinDir(), toolName, "current")
	if st, err := os.Stat(binCurrent); err == nil && st.IsDir() {
		envExport = fmt.Sprintf("export PATH=%s:$PATH TREBI_RUN_ID=%s TREBI_HOOK_TOOL=%s",
			shellQuote(binCurrent), runID, toolName)
	}
	merged := map[string]string{}
	if env, err := harness.LoadSecretEnv(toolName); err == nil {
		for k, v := range env {
			merged[k] = v
		}
	}
	for k, v := range optEnv {
		merged[k] = v
	}
	for k, v := range planEnv {
		merged[k] = v
	}
	if extra := buildEnvExport(merged); extra != "" {
		envExport += " " + extra
	}
	return envExport
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

// applyIfRunning enforces session.if_running against live runs of the same job.
// It returns the outcome ("" when no live run, else skip|coalesce|replace|allow)
// and, for skip/coalesce, the run row the caller must return instead of a launch.
func (s *Service) applyIfRunning(ctx context.Context, opts *LaunchOpts) (string, *Run, error) {
	policy := opts.IfRunning
	if policy == "" {
		policy = "skip"
	}

	live, err := s.List(ctx, ListOpts{
		Workspace: opts.WorkspaceID,
		Job:       opts.Job,
		Status:    string(StatusRunning),
		Limit:     20,
	})
	if err != nil {
		return "", nil, err
	}
	if len(live) == 0 {
		return "", nil, nil
	}
	switch policy {
	case "allow":
		return "allow", nil, nil
	case "skip":
		row, err := s.overlapSkip(ctx, opts, live[0])
		if err != nil {
			return "", nil, err
		}
		return "skip", row, nil
	case "replace":
		s.overlapReplace(ctx, live)
		return "replace", nil, nil
	case "coalesce":
		s.overlapCoalesce(ctx, opts, live[0])
		return "coalesce", live[0], nil
	default:
		return "", nil, fmt.Errorf("unknown if_running policy %q", policy)
	}
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
		"because": existing.ID, "policy": "skip",
	})
	return s.Get(ctx, skipped.ID)
}

// overlapReplace kills every live run so a new launch can start.
func (s *Service) overlapReplace(ctx context.Context, live []*Run) {
	for _, r := range live {
		s.closeSession(r.ID, r.TmuxSession)
		_ = s.Terminate(ctx, r.ID, StatusExited, EndReplace, nil)
		s.AddEvent(ctx, r.ID, "replace", "runner", map[string]string{"by": "new-launch"})
	}
}

// overlapCoalesce types the new prompt into the live run's pane.
func (s *Service) overlapCoalesce(ctx context.Context, opts *LaunchOpts, existing *Run) {
	text := opts.Prompt
	if text == "" {
		text = opts.PromptPreview
	}
	if text == "" {
		text = "(coalesced event)"
	}
	if existing.TmuxSession != "" {
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
