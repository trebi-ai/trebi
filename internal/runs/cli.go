package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// apiVersion mirrors internal/api.Version (the -ldflags target); runs cannot
// import api.
const apiVersion = "dev"

// cliLaunchGrace keeps the reaper from reading the launch window (row
// inserted, session not yet registered) as a gone session.
const cliLaunchGrace = 5 * time.Second

// cliRun is the per-run subprocess runtime state owned by the event pump.
type cliRun struct {
	sess      agentwire.Session
	tracker   *agentwire.Tracker
	result    *agentwire.Result
	memoryFix int
	oneShot   bool

	mu             sync.Mutex
	keepAliveSince time.Time
	terminal       bool
	stalled        bool
}

// markKeepAlive starts the keep-alive window once.
func (cr *cliRun) markKeepAlive(now time.Time) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.keepAliveSince.IsZero() {
		cr.keepAliveSince = now
	}
}

// keepAliveExpired reports whether the keep-alive window has elapsed.
func (cr *cliRun) keepAliveExpired(keepAliveMS int64, now time.Time) bool {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.keepAliveSince.IsZero() || keepAliveMS <= 0 {
		return false
	}
	return now.Sub(cr.keepAliveSince) >= time.Duration(keepAliveMS)*time.Millisecond
}

// keepAliveWithin reports the linger window is still open (max-duration guard).
func (cr *cliRun) keepAliveWithin(keepAliveMS int64, now time.Time) bool {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.keepAliveSince.IsZero() || keepAliveMS <= 0 {
		return false
	}
	return now.Sub(cr.keepAliveSince) < time.Duration(keepAliveMS)*time.Millisecond
}

// setTerminal marks the pump's terminal transition.
func (cr *cliRun) setTerminal() {
	cr.mu.Lock()
	cr.terminal = true
	cr.mu.Unlock()
}

// isTerminal reports whether the pump already moved the run to terminal.
func (cr *cliRun) isTerminal() bool {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.terminal
}

// setStalled records the stall observation latch.
func (cr *cliRun) setStalled(v bool) {
	cr.mu.Lock()
	cr.stalled = v
	cr.mu.Unlock()
}

// stalledNow reports the stall observation latch.
func (cr *cliRun) stalledNow() bool {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.stalled
}

// SetCLIDriver installs a subprocess driver override (tests). Nil restores
// the built-in per-harness drivers.
func (s *Service) SetCLIDriver(d agentwire.Driver) {
	s.mu.Lock()
	s.cliDriver = d
	rt := s.rt
	s.mu.Unlock()
	// The runtime, when it already exists, must see the override too: a nil
	// restores the built-in driver for every harness.
	if rt != nil {
		for _, h := range agentwire.Harnesses() {
			rt.SetDriver(h, d)
		}
	}
}

// agentwireRuntime lazily builds the shared subprocess runtime so TREBI_HOME
// set after runs.New (tests) still lands the ledger under the right home.
func (s *Service) agentwireRuntime() *agentwire.Runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rt == nil {
		rt := agentwire.New(agentwire.Options{
			ClientName:           "trebi",
			ClientVersion:        apiVersion,
			LedgerPath:           filepath.Join(home.RunsDir(), "agent-procs.json"),
			Logger:               s.log,
			FingerprintIgnoreEnv: []string{"TREBI_RUN_ID", "TREBI_HOOK_TOOL", "TREBI_MEMORY_PATH"},
		})
		if s.cliDriver != nil {
			for _, h := range agentwire.Harnesses() {
				rt.SetDriver(h, s.cliDriver)
			}
		}
		s.rt = rt
	}
	return s.rt
}

// cliDriverFor resolves the driver for a harness: the test override wins.
func (s *Service) cliDriverFor(tool string) agentwire.Driver {
	s.mu.RLock()
	override := s.cliDriver
	s.mu.RUnlock()
	if override != nil {
		return override
	}
	d, ok := s.agentwireRuntime().Driver(agentwire.Harness(tool))
	if !ok {
		return nil
	}
	return d
}

func (s *Service) registerCLIRun(id string, cr *cliRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cliRuns == nil {
		s.cliRuns = map[string]*cliRun{}
	}
	s.cliRuns[id] = cr
}

func (s *Service) cliRunFor(id string) *cliRun {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cliRuns[id]
}

func (s *Service) unregisterCLIRun(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cliRuns, id)
}

// wantsCLI decides the run runtime (Part F/J): explicit `runtime: tui`, the
// TREBI_HARNESS_RUNTIME kill switch, the global harness.runtime, or a per-tool
// tools.<name>.runtime force the pane; a harness without a proven wire also
// falls back to tmux.
func (s *Service) wantsCLI(tool tmux.Tool, opts LaunchOpts) bool {
	if opts.Runtime == "tui" || strings.EqualFold(os.Getenv("TREBI_HARNESS_RUNTIME"), "tui") {
		return false
	}
	if cfg, err := config.Load(); err == nil && cfg != nil {
		if strings.EqualFold(cfg.HarnessRuntimeFor(tool.Name()), "tui") {
			return false
		}
	}
	return s.cliDriverFor(tool.Name()) != nil
}

// launchCLI starts a vendor harness as a subprocess and drives its wire.
func (s *Service) launchCLI(ctx context.Context, opts LaunchOpts) (*Run, error) {
	command := opts.Command
	if command == "" {
		command = opts.Harness
	}
	if command == "" {
		command = "claude"
	}
	command = resolveCommand(command)
	if opts.WorkingDir == "" {
		opts.WorkingDir, _ = os.Getwd()
	}
	tool := detectTool(command, &opts)
	drv := s.cliDriverFor(tool.Name())
	if drv == nil {
		return s.launchTmux(ctx, opts)
	}
	resuming := opts.ResumeSessionID != ""
	plan, err := preparePlan(tool, command, &opts)
	if err != nil {
		return nil, err
	}
	// Session identity belongs to the launch plan. A harness that assigns its
	// own session id (the OpenCode family) leaves plan.SessionID empty, and
	// the wire reads a set id as an attach to an existing session — a fresh
	// run must not carry the UUID trebi minted for the pane path. The vendor
	// id then arrives on the wire and fills the run row (setRunSessionID).
	opts.SessionID = plan.SessionID

	// Auth first: no child when the operator login is missing (Part F.2).
	if _, isFake := drv.(agentwire.FakeDriver); !isFake {
		// The probe spawns a vendor process; a canceled HTTP request must not
		// be recorded as an auth failure.
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		st := tool.ProbeAuth(probeCtx)
		cancel()
		if !st.OK() {
			hint := st.Hint
			if hint == "" {
				hint = "trebi harness auth login " + tool.Name()
			}
			return s.FailBeforeLaunch(ctx, opts, EndAuth, map[string]string{
				"message": "harness auth is not usable",
				"hint":    hint,
			})
		}
	}

	opts.Runtime = "cli"
	run, err := s.Create(ctx, opts.CreateOpts)
	if err != nil {
		return nil, err
	}
	// Spawn can take seconds (OpenCode health wait, Claude version probes);
	// the reaper must not read that window as a missing session.
	s.markLaunching(run.ID, true)
	defer s.markLaunching(run.ID, false)
	binName := ""
	if fields := strings.Fields(plan.Command); len(fields) > 0 {
		binName = filepath.Base(fields[0])
	}
	s.AddEvent(ctx, run.ID, "launch", "runner", map[string]string{
		"harness": tool.Name(), "session_id": opts.SessionID, "mode": "cli", "command": binName,
	})

	oneShot := cliOneShot(opts) && tool.Name() == tmux.Claude
	body := opts.Prompt
	if body == "" {
		body = opts.PromptPreview
	}
	bin, extra := cliExtraArgs(tool.Name(), plan.Command)
	req := agentwire.StartRequest{
		Harness:       agentwire.Harness(tool.Name()),
		Binary:        bin,
		WorkingDir:    opts.WorkingDir,
		Env:           mergeCLIEnv(tool.Name(), actionEnv(opts.Actions), opts.Env, plan.Env, run.ID, opts.CreateOpts.ActorToken, tool.Name()),
		MCPServers:    mcpServers(opts.Actions),
		Skills:        actionSkills(opts.Actions),
		SessionID:     opts.SessionID,
		Model:         opts.Model,
		Effort:        opts.Effort,
		Permissions:   cliPermissionPolicy(tool.Name(), command),
		OneShot:       oneShot,
		OneShotPrompt: body,
		ExtraArgs:     extra,
		LogPath:       filepath.Join(home.RunsLogDir(), run.ID+".log"),
	}
	// Home is deliberately unset: the tmux policy already renders the harness
	// home into plan.Env (CLAUDE_CONFIG_DIR, OPENCODE_CONFIG, PI_*, the cursor
	// vars) including the shared-auth mode, and a second source of truth would
	// let the two disagree.
	if opts.Memory != nil {
		req.Env["TREBI_MEMORY_PATH"] = filepath.Join(opts.Memory.Root, opts.Memory.Rel)
	}
	// Spawn is not tied to the HTTP request: a canceled request must not kill
	// a child that is still handshaking.
	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	rt := s.agentwireRuntime()
	var sess agentwire.Session
	if resuming {
		sess, err = rt.Resume(startCtx, req)
	} else {
		sess, err = rt.Start(startCtx, req)
	}
	if err != nil {
		// A driver that cannot resume must fail loudly (Part F): callers
		// surface ErrResumeUnsupported instead of a fake pane or a silent
		// failed row.
		if resuming && errors.Is(err, agentwire.ErrResumeUnsupported) {
			return nil, err
		}
		s.appendRunLog(run.ID, "cli launch failed: "+err.Error())
		s.AddEvent(ctx, run.ID, "error", "launch", map[string]string{"message": err.Error()})
		_ = s.Terminate(ctx, run.ID, StatusFailed, EndLaunch, nil)
		got, _ := s.Get(ctx, run.ID)
		return got, nil
	}
	// The library tracks its own sessions in the ledger; no registry Add here.
	cr := &cliRun{sess: sess, tracker: agentwire.NewTracker(), oneShot: oneShot}
	if oneShot {
		// The prompt is on argv, so the turn starts at launch.
		cr.tracker.PromptWritten()
	}
	s.registerCLIRun(run.ID, cr)
	s.setRunPid(ctx, run.ID, sess.PID())
	s.AddEvent(ctx, run.ID, "proc", "proc", map[string]string{"pid": strconv.Itoa(sess.PID())})

	go s.pumpCLI(run.ID, opts, cr)

	if !oneShot {
		if err := sess.Prompt(ctx, agentwire.Prompt{Text: body, Kind: kindOrDefault(opts.Kind)}); err != nil {
			s.appendRunLog(run.ID, "prompt write failed: "+err.Error())
			s.Terminate(ctx, run.ID, StatusFailed, EndLaunch, nil)
			s.unregisterCLIRun(run.ID)
			go sess.Close(context.Background())
			got, _ := s.Get(ctx, run.ID)
			return got, nil
		}
		cr.tracker.PromptWritten()
		s.SetSessionState(ctx, run.ID, SessionRunning, "wire")
	}
	s.log.Info("run launched", "id", run.ID, "harness", tool.Name(), "runtime", "cli", "pid", sess.PID(), "one_shot", oneShot)
	return s.Get(ctx, run.ID)
}

// cliOneShot reports the narrow one-shot fast path (A.7): a job run with no
// memory gate, no idle timeout, no keep-alive, and no follow-up surface.
func cliOneShot(opts LaunchOpts) bool {
	if opts.Kind == KindAgent {
		return false
	}
	if opts.Memory != nil || opts.ResumeSessionID != "" {
		return false
	}
	if opts.IdleTimeoutMS > 0 || opts.KeepAliveMS > 0 {
		return false
	}
	return true
}

func kindOrDefault(kind string) string {
	if kind == "" {
		return KindJob
	}
	return kind
}

// cliExtraArgs splits the prepared pane command into the pinned binary and the
// flags the library does not build. Session identity, model, effort and
// permission flags are dropped: the typed request owns them. The trebi hooks
// stay, except the Codex notify helper, which is a screen hook the wire does
// not need (A.14).
func cliExtraArgs(tool, command string) (string, []string) {
	argv := agentwire.SplitArgs(command)
	if len(argv) == 0 {
		return "", nil
	}
	bin, rest := argv[0], argv[1:]
	var out []string
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		name, value, hasValue := strings.Cut(arg, "=")
		take := func() string {
			if hasValue {
				return value
			}
			if i+1 < len(rest) {
				i++
				return rest[i]
			}
			return ""
		}
		switch {
		case tool == tmux.Claude && (name == "--session-id" || name == "--resume" || name == "--model" || name == "--effort" || name == "--permission-mode"):
			take()
		case tool == tmux.Claude && name == "--dangerously-skip-permissions":
			// the permission policy carries it
		case tool == tmux.Claude:
			out = append(out, arg)
			if !hasValue && claudeValueFlag(name) {
				out = append(out, take())
			}
		case tool == tmux.Codex && name == "-c":
			v := take()
			if !strings.HasPrefix(v, "notify=") && !strings.HasPrefix(v, "model_reasoning_effort=") {
				out = append(out, arg, v)
			}
		case name == "--model" || name == "--effort" || name == "--variant":
			take()
		default:
			out = append(out, arg)
		}
	}
	return bin, out
}

// claudeValueFlag reports the Claude flags trebi injects that take a separate
// value, so the value is not mistaken for the next flag.
func claudeValueFlag(name string) bool {
	return name == "--settings"
}

// cliPermissionPolicy maps the launch command flags onto the library policy.
// The mapping is today's behavior, no more: Claude with
// --dangerously-skip-permissions runs unattended, Codex runs with
// approvalPolicy never, and every other harness asks trebi. Claude without the
// flag inherits the operator's own mode: trebi must not force `default` and
// add approval gates the interactive CLI would not show.
func cliPermissionPolicy(tool, command string) agentwire.PermissionPolicy {
	switch tool {
	case tmux.Claude:
		if hasCommandFlag(command, "--dangerously-skip-permissions") {
			return agentwire.PermissionPolicy{Mode: agentwire.PermissionAuto}
		}
		return agentwire.PermissionPolicy{Mode: agentwire.PermissionInherit}
	case tmux.Codex:
		return agentwire.PermissionPolicy{Mode: agentwire.PermissionAuto}
	}
	return agentwire.PermissionPolicy{}
}

func hasCommandFlag(command, flag string) bool {
	for _, f := range strings.Fields(command) {
		if f == flag || strings.HasPrefix(f, flag+"=") {
			return true
		}
	}
	return false
}

// mergeCLIEnv mirrors paneEnvFile precedence: secret env < action env < job
// env < plan env, plus the run identity and the pinned bin dir on PATH.
func mergeCLIEnv(toolName string, actEnv, optEnv, planEnv map[string]string, runID, token, tool string) map[string]string {
	merged := map[string]string{}
	if env, err := harness.LoadSecretEnv(toolName); err == nil {
		for k, v := range env {
			merged[k] = v
		}
	}
	for k, v := range actEnv {
		merged[k] = v
	}
	for k, v := range optEnv {
		merged[k] = v
	}
	for k, v := range planEnv {
		merged[k] = v
	}
	merged["TREBI_RUN_ID"] = runID
	merged["TREBI_RUN_TOKEN"] = token
	merged["TREBI_HOOK_TOOL"] = tool
	binCurrent := filepath.Join(config.BinDir(), toolName, "current")
	if st, err := os.Stat(binCurrent); err == nil && st.IsDir() {
		merged["PATH"] = binCurrent + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	return merged
}

// setRunPid records the child pid (Part D).
func (s *Service) setRunPid(ctx context.Context, runID string, pid int) {
	if _, err := s.st.DB().ExecContext(ctx, `UPDATE runs SET runtime_pid=?, updated_at=? WHERE id=?`,
		pid, time.Now().UTC().Format(time.RFC3339Nano), runID); err != nil {
		s.log.Warn("set run pid", "run", runID, "error", err)
	}
}

// setRunSessionID fills the vendor session id learned on the wire.
func (s *Service) setRunSessionID(ctx context.Context, runID, sessionID string) {
	if sessionID == "" {
		return
	}
	if _, err := s.st.DB().ExecContext(ctx, `UPDATE runs SET session_id=?, updated_at=? WHERE id=? AND COALESCE(session_id,'') = ''`,
		sessionID, time.Now().UTC().Format(time.RFC3339Nano), runID); err != nil {
		s.log.Warn("set run session id", "run", runID, "error", err)
	}
}

// cliFixMessage is the memory fix on the subprocess path: no completion
// sentinel (the wire owns completion, A.5).
func cliFixMessage(issues []string) string {
	return strings.Join(issues, "\n") + "\nFix MEMORY.md, then finish the run."
}

// pumpCLI drains the session event stream: it owns session state, the run
// log, approvals, the memory gate, and the terminal transition.
func (s *Service) pumpCLI(runID string, opts LaunchOpts, cr *cliRun) {
	ctx := context.Background()
	defer s.unregisterCLIRun(runID)
	sawResult := false
	var lastErr *agentwire.Event
	for ev := range cr.sess.Events() {
		snap := cr.tracker.Observe(ev)
		s.logCLIEvent(runID, ev)
		switch ev.Type {
		case agentwire.EventInit:
			if ev.SessionID != "" {
				s.setRunSessionID(ctx, runID, ev.SessionID)
			}
			s.AddEvent(ctx, runID, "session.init", "wire", map[string]string{"session_id": ev.SessionID})
		case agentwire.EventAssistant:
			s.AddEvent(ctx, runID, "assistant", "wire", map[string]string{
				"text": trunc(ev.Text, 2000), "delta": strconv.FormatBool(ev.Delta),
			})
		case agentwire.EventUser:
			s.AddEvent(ctx, runID, "user", "wire", map[string]string{"text": trunc(ev.Text, 2000)})
		case agentwire.EventUsage:
			if ev.Usage != nil {
				s.AddEvent(ctx, runID, "usage", "wire", map[string]string{
					"input":          strconv.FormatInt(ev.Usage.Input, 10),
					"output":         strconv.FormatInt(ev.Usage.Output, 10),
					"cache_read":     strconv.FormatInt(ev.Usage.CacheRead, 10),
					"cache_creation": strconv.FormatInt(ev.Usage.CacheCreation, 10),
					"cost_usd":       strconv.FormatFloat(ev.Usage.CostUSD, 'f', 4, 64),
					"model":          ev.Usage.Model,
				})
			}
		case agentwire.EventTool:
			s.addCLIToolEvent(ctx, runID, ev)
		case agentwire.EventPermission:
			s.handleCLIPermission(ctx, runID, cr, ev)
		case agentwire.EventError:
			e := ev
			lastErr = &e
			detail := map[string]string{"message": trunc(ev.Error, 1000), "class": ev.EndReason}
			if h := hintFor(agentwire.FailureClass(ev.EndReason), ev.Code); h != "" {
				detail["hint"] = h
			}
			s.AddEvent(ctx, runID, "error", "wire", detail)
		case agentwire.EventResult:
			sawResult = true
			s.finishCLIResult(ctx, runID, opts, cr, ev)
		case agentwire.EventExit:
			if run, err := s.Get(ctx, runID); err == nil && !run.Status.IsTerminal() && !sawResult {
				e := ev
				if lastErr == nil && e.Error != "" {
					lastErr = &e
				}
			}
		}
		s.applyCLIState(ctx, runID, cr, snap)
	}
	s.afterCLIExit(ctx, runID, opts, cr, sawResult, lastErr)
}

func (s *Service) logCLIEvent(runID string, ev agentwire.Event) {
	switch ev.Type {
	case agentwire.EventAssistant:
		if ev.Text != "" && !ev.Delta {
			s.appendRunLog(runID, ev.Text)
		}
	case agentwire.EventTool:
		if ev.Tool != nil {
			line := "tool " + ev.Tool.Name + " " + ev.Tool.Status
			if ev.Tool.Output != "" {
				line += ": " + trunc(strings.TrimSpace(ev.Tool.Output), 500)
			}
			s.appendRunLog(runID, line)
		}
	case agentwire.EventResult:
		if ev.Result != nil {
			s.appendRunLog(runID, fmt.Sprintf("result subtype=%s is_error=%v", ev.Result.Subtype, ev.Result.IsError))
		}
	case agentwire.EventUsage:
		if ev.Usage != nil {
			s.appendRunLog(runID, fmt.Sprintf("usage input=%d output=%d cache_read=%d cache_creation=%d cost_usd=%.4f",
				ev.Usage.Input, ev.Usage.Output, ev.Usage.CacheRead, ev.Usage.CacheCreation, ev.Usage.CostUSD))
		}
	case agentwire.EventError:
		s.appendRunLog(runID, "error: "+ev.Error)
	}
}

func (s *Service) addCLIToolEvent(ctx context.Context, runID string, ev agentwire.Event) {
	if ev.Tool == nil {
		return
	}
	s.AddEvent(ctx, runID, "tool", "wire", map[string]string{
		"name": ev.Tool.Name, "status": ev.Tool.Status,
		"id": ev.Tool.ID, "input": trunc(ev.Tool.Input, 500), "output": trunc(ev.Tool.Output, 500),
	})
}

// CLITurns reconstructs the transcript from wire events (Part H: prefer live
// events over the vendor jsonl). Empty means the pump recorded nothing usable,
// and the caller falls back to the vendor session file.
func (s *Service) CLITurns(ctx context.Context, runID string) ([]tmux.Turn, error) {
	var evs []RunEvent
	var after int64
	for {
		page, err := s.EventsSince(ctx, runID, after, 2000)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		evs = append(evs, page...)
		after = page[len(page)-1].ID
	}
	var turns []tmux.Turn
	// Tool calls open on a "started" event and close on a "completed"/"failed"
	// one. The closing frame may carry only the id (claude tool_result frames
	// carry no name), so keep the opening call until its result lands.
	type pendingTool struct {
		call tmux.ToolCall
		at   time.Time
		done bool
	}
	var pending []*pendingTool
	byID := map[string]*pendingTool{}
	for _, ev := range evs {
		var d map[string]string
		if len(ev.Detail) > 0 {
			_ = json.Unmarshal(ev.Detail, &d)
		}
		switch ev.Kind {
		case "user":
			if d["text"] != "" {
				turns = append(turns, tmux.Turn{Role: "user", Text: d["text"], At: ev.At})
			}
		case "assistant":
			if d["text"] != "" && d["delta"] != "true" {
				turns = append(turns, tmux.Turn{Role: "assistant", Text: d["text"], At: ev.At})
			}
		case "tool":
			id := d["id"]
			if id == "" {
				continue
			}
			switch d["status"] {
			case "completed", "failed":
				var call tmux.ToolCall
				if open := byID[id]; open != nil {
					call = open.call
					open.done = true
				} else if d["name"] != "" {
					call = tmux.ToolCall{Name: d["name"], Input: d["input"]}
				} else {
					continue
				}
				if d["output"] != "" {
					call.Output = d["output"]
				}
				turns = append(turns, tmux.Turn{Role: "tools", Tools: []tmux.ToolCall{call}, At: ev.At})
			default:
				if d["name"] == "" {
					continue
				}
				if open := byID[id]; open != nil {
					open.call = tmux.ToolCall{Name: d["name"], Input: d["input"], Output: d["output"]}
					open.at = ev.At
					continue
				}
				open := &pendingTool{call: tmux.ToolCall{Name: d["name"], Input: d["input"], Output: d["output"]}, at: ev.At}
				pending = append(pending, open)
				byID[id] = open
			}
		}
	}
	for _, open := range pending {
		if open.done {
			continue
		}
		turns = append(turns, tmux.Turn{Role: "tools", Tools: []tmux.ToolCall{open.call}, At: open.at})
	}
	if turns == nil {
		turns = []tmux.Turn{}
	}
	return turns, nil
}

// OpenCodeMessages returns the raw vendor message list for an OpenCode session
// (transcript fallback). ok is false when no shared server serves that session.
func (s *Service) OpenCodeMessages(ctx context.Context, sessionID string) ([]byte, bool) {
	return s.agentwireRuntime().OpenCodeMessages(ctx, sessionID)
}

func (s *Service) handleCLIPermission(ctx context.Context, runID string, cr *cliRun, ev agentwire.Event) {
	q := "permission requested"
	permID, tool := "", ""
	if ev.Permission != nil {
		if ev.Permission.Question != "" {
			q = ev.Permission.Question
		}
		permID = ev.Permission.ID
		tool = ev.Permission.Tool
	}
	s.SetSessionState(ctx, runID, SessionWaiting, "wire")
	if _, err := s.OpenApprovalKeyed(ctx, runID, q, permID); err != nil {
		s.log.Warn("open approval", "run", runID, "error", err)
		// A wire permission must never sit unanswered (A.10): deny explicitly
		// when the approval cannot be recorded.
		if aerr := cr.sess.AnswerPermission(ctx, permID, agentwire.Decision{
			Allow: false, Message: "trebi could not record the approval",
		}); aerr != nil {
			s.log.Warn("deny unrecorded permission", "run", runID, "error", aerr)
		}
		snap := cr.tracker.PermissionDecided(permID)
		s.applyCLIState(ctx, runID, cr, snap)
	}
	s.AddEvent(ctx, runID, "waiting", "wire", map[string]string{
		"question": q, "permission_id": permID, "tool": tool,
	})
}

// applyCLIState maps the tracker snapshot to session state and emits the
// stall observation (never a killer; max duration is the backstop).
func (s *Service) applyCLIState(ctx context.Context, runID string, cr *cliRun, snap agentwire.Snapshot) {
	switch snap.State {
	case agentwire.StatusRunning:
		s.SetSessionState(ctx, runID, SessionRunning, "wire")
	case agentwire.StatusWaiting:
		s.SetSessionState(ctx, runID, SessionWaiting, "wire")
	case agentwire.StatusIdle:
		s.SetSessionState(ctx, runID, SessionIdle, "wire")
	}
	if snap.Stalled && !cr.stalledNow() {
		cr.setStalled(true)
		s.AddEvent(ctx, runID, "agent.stall", "wire", map[string]string{
			"last_event_at":  snap.LastEventAt.UTC().Format(time.RFC3339Nano),
			"inflight_tools": strconv.Itoa(snap.InflightTools),
		})
	} else if !snap.Stalled {
		cr.setStalled(false)
	}
}

// finishCLIResult handles one terminal turn frame: error classification,
// memory gate, keep-alive, or completion.
func (s *Service) finishCLIResult(ctx context.Context, runID string, opts LaunchOpts, cr *cliRun, ev agentwire.Event) {
	if cr.isTerminal() {
		return
	}
	res := ev.Result
	if res == nil {
		res = &agentwire.Result{Subtype: "success"}
	}
	cr.result = res
	if res.SessionID != "" {
		s.setRunSessionID(ctx, runID, res.SessionID)
	}
	tokens := tokenUsage(res, opts)
	if res.IsError {
		s.failCLIResult(ctx, runID, opts, cr, res, tokens)
		return
	}
	run, err := s.Get(ctx, runID)
	if err != nil {
		return
	}
	// Memory gate: the fix is a second prompt on the same session.
	mem := opts.Memory
	if mem == nil {
		mem = s.memoryFor(ctx, run)
	}
	if mem != nil {
		issues := s.gateMemory(ctx, run, mem)
		if len(issues) > 0 {
			if cr.memoryFix < maxMemoryFixes && !cr.oneShot {
				cr.memoryFix++
				s.requestMemoryFix(ctx, runID, "wire", cr.memoryFix, issues)
				if err := cr.sess.Prompt(ctx, agentwire.Prompt{Text: cliFixMessage(issues), Kind: "memory_fix"}); err != nil {
					s.recordMemoryInvalid(ctx, run, issues, "wire")
					s.terminateCLI(ctx, runID, cr, StatusFailed, EndLaunch, tokens)
					return
				}
				cr.tracker.PromptWritten()
				cr.result = nil
				s.SetSessionState(ctx, runID, SessionRunning, "wire")
				return
			}
			s.recordMemoryInvalid(ctx, run, issues, "wire")
		}
	}
	if run.Session.KeepAliveMS > 0 && !cr.oneShot {
		cr.markKeepAlive(time.Now())
		s.AddEvent(ctx, runID, "result", "wire", map[string]string{"keep_alive": "true"})
		s.SetSessionState(ctx, runID, SessionIdle, "wire")
		return
	}
	s.appendRunLog(runID, "run complete")
	s.terminateCLI(ctx, runID, cr, StatusComplete, EndResult, tokens)
}

// failCLIResult classifies an error result into an end reason and hint.
func (s *Service) failCLIResult(ctx context.Context, runID string, opts LaunchOpts, cr *cliRun, res *agentwire.Result, tokens *pricing.TokenUsage) {
	class := agentwire.Classification{Class: agentwire.FailureClass(res.EndReason)}
	if class.Class == "" || class.Class == agentwire.FailUnknown {
		class = agentwire.ClassifyResult(res)
	}
	msg := res.Text
	if msg == "" {
		msg = res.Subtype
	}
	detail := map[string]string{"message": trunc(msg, 1000), "class": string(class.Class)}
	if h := hintFor(class.Class, class.Code); h != "" {
		detail["hint"] = h
	}
	s.AddEvent(ctx, runID, "error", "wire", detail)
	s.appendRunLog(runID, "run failed: "+msg)
	end := endReasonFor(string(class.Class))
	if end == EndAuth {
		s.emitAuthProbeFailed(ctx, opts.Harness, runID, harness.SourceReaper)
	}
	if end == EndLimit {
		s.emitLimitExceeded(ctx, opts.Harness, runID)
	}
	s.terminateCLI(ctx, runID, cr, StatusFailed, end, tokens)
}

// endReasonFor maps a CLI failure class to the runs vocabulary.
func endReasonFor(class string) EndReason {
	switch agentwire.FailureClass(class) {
	case agentwire.FailAuth, agentwire.FailEntitlement:
		return EndAuth
	case agentwire.FailLimit:
		return EndLimit
	case agentwire.FailProtocol:
		return EndLaunch
	}
	return EndFailed
}

func tokenUsage(res *agentwire.Result, opts LaunchOpts) *pricing.TokenUsage {
	if res == nil {
		return nil
	}
	model := res.Usage.Model
	if model == "" {
		model = opts.Model
	}
	return &pricing.TokenUsage{
		Input: res.Usage.Input, Output: res.Usage.Output,
		CacheRead: res.Usage.CacheRead, CacheCreation: res.Usage.CacheCreation,
		Model: model,
	}
}

// terminateCLI marks the run terminal and starts the graceful close in the
// background: the pump must keep draining until the event channel closes.
func (s *Service) terminateCLI(ctx context.Context, runID string, cr *cliRun, status Status, end EndReason, tokens *pricing.TokenUsage) {
	if cr.isTerminal() {
		return
	}
	cr.setTerminal()
	if err := s.Terminate(ctx, runID, status, end, tokens); err != nil {
		s.log.Warn("cli terminate", "run", runID, "error", err)
	}
	go func() {
		_ = cr.sess.Close(context.Background())
	}()
}

// afterCLIExit reconciles a process that ended without a result.
func (s *Service) afterCLIExit(ctx context.Context, runID string, opts LaunchOpts, cr *cliRun, sawResult bool, lastErr *agentwire.Event) {
	run, err := s.Get(ctx, runID)
	if err != nil || run.Status.IsTerminal() {
		return
	}
	if sawResult {
		// A result already landed; the process exiting just ends the
		// keep-alive window early. Complete the run rather than reporting a
		// missing session.
		if opts.Memory != nil {
			if issues := s.gateMemory(ctx, run, opts.Memory); len(issues) > 0 {
				s.recordMemoryInvalid(ctx, run, issues, "wire")
			}
		}
		if run.Session.KeepAliveMS > 0 {
			_ = s.Terminate(ctx, runID, StatusComplete, EndKeepAlive, nil)
		} else {
			_ = s.Terminate(ctx, runID, StatusComplete, EndResult, nil)
		}
		return
	}
	class, msg, code := "", "", ""
	if lastErr != nil {
		class, msg, code = lastErr.EndReason, lastErr.Error, lastErr.Code
	}
	if msg == "" {
		msg = "session ended before a result"
		class = string(agentwire.FailProtocol)
	}
	if class == "" || class == string(agentwire.FailUnknown) {
		c := agentwire.ClassifyError(msg, 0)
		class, code = string(c.Class), c.Code
	}
	detail := map[string]string{
		"message": trunc(msg, 1000), "class": class,
	}
	if h := hintFor(agentwire.FailureClass(class), code); h != "" {
		detail["hint"] = h
	}
	s.AddEvent(ctx, runID, "error", "proc", detail)
	s.appendRunLog(runID, "session ended: "+msg)
	end := endReasonFor(class)
	if end == EndAuth {
		s.emitAuthProbeFailed(ctx, opts.Harness, runID, harness.SourceReaper)
	}
	if end == EndLimit {
		s.emitLimitExceeded(ctx, opts.Harness, runID)
	}
	_ = s.Terminate(ctx, runID, StatusFailed, end, nil)
}

// stopCLIRun interrupts and closes a live CLI session (kill/replace/reaper).
func (s *Service) stopCLIRun(runID string, cr *cliRun) {
	if cr == nil {
		return
	}
	_ = cr.sess.Interrupt(context.Background())
	go func() {
		_ = cr.sess.Close(context.Background())
	}()
}

// teardownRuntime stops whichever runtime owns the run, without terminating
// it. The caller writes the terminal row.
func (s *Service) teardownRuntime(ctx context.Context, run *Run) {
	removeRunEnvFile(run)
	if cr := s.cliRunFor(run.ID); cr != nil {
		s.stopCLIRun(run.ID, cr)
	}
	if run.TmuxSession != "" {
		_ = tmux.SendKey(run.TmuxSession, "C-c")
		time.Sleep(100 * time.Millisecond)
		s.closeSession(run.ID, run.TmuxSession)
	}
}

// CLIActive reports whether a subprocess session is registered for the run
// (diagnostics and test teardown).
func (s *Service) CLIActive(runID string) bool { return s.cliRunFor(runID) != nil }

// CLIActiveCount is the number of registered subprocess sessions.
func (s *Service) CLIActiveCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.cliRuns)
}

// RecoverCLI is the daemon-boot reconcile: mark every live cli row
// session_gone first (so a killed child's old pump cannot rewrite the reason),
// then kill positively matched leftover children from the pid ledger.
func (s *Service) RecoverCLI(ctx context.Context) {
	list, err := s.List(ctx, ListOpts{Status: string(StatusRunning), Limit: 500})
	if err == nil {
		for _, run := range list {
			if run.Runtime != "cli" {
				continue
			}
			_ = s.Terminate(ctx, run.ID, StatusExited, EndSessionGone, nil)
			s.AddEvent(ctx, run.ID, "recovered", "proc", map[string]string{"reason": "daemon restart"})
			s.log.Info("cli run recovered", "id", run.ID, "end_reason", EndSessionGone)
		}
	}
	// The library logs every killed process itself; one reconcile pass covers
	// both run children and the shared `opencode serve` helpers (E.3).
	if err := s.agentwireRuntime().Reconcile(); err != nil {
		s.log.Warn("reconcile agent processes", "error", err)
	}
}

// CloseCLIRuns gracefully closes every live CLI session on daemon shutdown.
func (s *Service) CloseCLIRuns(ctx context.Context) {
	_ = s.agentwireRuntime().Close(ctx)
}

// onApprovalDecided forwards an operator decision to a live CLI session.
// key is the vendor permission id. Hook-fallback approvals (no key) clear the
// oldest tracker approval instead.
func (s *Service) onApprovalDecided(ctx context.Context, runID, key, decision string) {
	cr := s.cliRunFor(runID)
	if cr == nil {
		return
	}
	id := key
	if id == "" {
		id = cr.tracker.OldestApprovalID()
	}
	d := agentwire.Decision{Allow: decision == "allow"}
	if decision != "allow" {
		d.Message = "denied by operator"
	}
	if err := cr.sess.AnswerPermission(ctx, id, d); err != nil {
		s.log.Warn("answer permission", "run", runID, "permission", id, "error", err)
	}
	snap := cr.tracker.PermissionDecided(id)
	s.applyCLIState(ctx, runID, cr, snap)
}

// AgentwireRuntime exposes the shared subprocess runtime for callers that
// must reach library-level state, such as the detect cache.
func (s *Service) AgentwireRuntime() *agentwire.Runtime {
	return s.agentwireRuntime()
}
