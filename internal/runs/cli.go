package runs

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/agentcli"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/uploads"
	"github.com/trebi-ai/trebi/internal/visor"
)

// apiVersion mirrors internal/api.Version (the -ldflags target); runs cannot
// import api.
const apiVersion = "dev"

// cliLaunchGrace keeps the reaper from reading the launch window (row
// inserted, session not yet registered) as a gone session.
const cliLaunchGrace = 5 * time.Second

// cliRun is the per-run subprocess runtime state owned by the event pump.
type cliRun struct {
	sess      *visor.Session
	harness   string
	tracker   *agentwire.Tracker
	result    *agentwire.Result
	memoryFix int
	// turn is the run turn this session serves.
	turn int
	// held counts prompts that agentwire sends when the vendor turn ends.
	held int

	mu             sync.Mutex
	usage          pricing.TokenUsage
	keepAliveSince time.Time
	terminal       bool
	stalled        bool
	// lastText is the assistant text since the last tool call.
	lastText string
	// seq is the last visor event that the pump handled. replay is the last
	// event that a daemon before this one handled: the pump only reads the
	// events up to it.
	seq    uint64
	replay uint64
}

// maxLastText caps lastText; the failure line is at the end.
const maxLastText = 8 << 10

// noteText records assistant text. A full message replaces the text, a delta
// adds to it, and a tool call clears it.
func (cr *cliRun) noteText(ev agentwire.Event) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	switch {
	case ev.Type != agentwire.EventAssistant:
		cr.lastText = ""
	case ev.Delta:
		cr.lastText += ev.Text
	default:
		cr.lastText = ev.Text
	}
	if n := len(cr.lastText); n > maxLastText {
		cr.lastText = cr.lastText[n-maxLastText:]
	}
}

func (cr *cliRun) text() string {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.lastText
}

// addUsage adds the usage of one result and returns the turn total.
func (cr *cliRun) addUsage(u *pricing.TokenUsage) *pricing.TokenUsage {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if u != nil {
		cr.usage.Input += u.Input
		cr.usage.Output += u.Output
		cr.usage.CacheRead += u.CacheRead
		cr.usage.CacheCreation += u.CacheCreation
		if u.Model != "" {
			cr.usage.Model = u.Model
		}
	}
	total := cr.usage
	return &total
}

// setModel keeps the model that the harness reports at session start.
func (cr *cliRun) setModel(model string) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if model != "" {
		cr.usage.Model = model
	}
}

// usageSoFar is the turn total, or nil before the first result.
func (cr *cliRun) usageSoFar() *pricing.TokenUsage {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.usage.Input+cr.usage.Output+cr.usage.CacheRead+cr.usage.CacheCreation == 0 {
		return nil
	}
	total := cr.usage
	return &total
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

// noteHeld counts one prompt that waits for the vendor turn to end.
func (cr *cliRun) noteHeld() {
	cr.mu.Lock()
	cr.held++
	cr.mu.Unlock()
}

// takeHeld reports whether held prompts started a new vendor turn at this
// result, and clears the count.
func (cr *cliRun) takeHeld() bool {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	n := cr.held
	cr.held = 0
	return n > 0
}

// setTerminal marks the pump's terminal transition.
func (cr *cliRun) setTerminal() {
	cr.mu.Lock()
	cr.terminal = true
	cr.mu.Unlock()
}

// isTerminal reports whether the pump or a teardown caller owns the terminal row.
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
	// The visor of this process takes its drivers when it opens.
	s.closeOwnHost(context.Background())
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

// registerCLIRun adds cr. It returns false when the daemon stops: then
// PauseCLIRuns already wrote the row of the run.
func (s *Service) registerCLIRun(id string, cr *cliRun) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return false
	}
	if s.cliRuns == nil {
		s.cliRuns = map[string]*cliRun{}
	}
	s.cliRuns[id] = cr
	return true
}

func (s *Service) cliRunFor(id string) *cliRun {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cliRuns[id]
}

// unregisterCLIRun drops cr. A newer turn of the run keeps its own session.
func (s *Service) unregisterCLIRun(id string, cr *cliRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cliRuns[id] == cr {
		delete(s.cliRuns, id)
	}
}

// wantsCLI decides the run runtime (Part F/J): explicit `runtime: tui`, the
// TREBI_HARNESS_RUNTIME kill switch, the global harness.runtime, or a per-tool
// tools.<name>.runtime force the pane; a harness without a proven wire also
// falls back to a pane.
func (s *Service) wantsCLI(tool agentcli.Tool, opts LaunchOpts) bool {
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
	if opts.WorkingDir == "" && opts.ReopenRunID == "" {
		opts.WorkingDir, _ = os.Getwd()
	}
	tool := detectTool(command, &opts)
	drv := s.cliDriverFor(tool.Name())
	if drv == nil {
		return s.launchPane(ctx, opts)
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
	if !resuming && tool.Name() == agentcli.Grok {
		// The grok plan mints --session-id for the pane. On the wire grok
		// assigns its own id.
		opts.SessionID = ""
	}

	cfg, _ := config.Load()
	policy := cliPermissionPolicy(tool.Name(), command, cfg.HarnessApproval(tool.Name()))
	if policy.Mode != agentwire.PermissionAuto && !config.HarnessAsksApproval(tool.Name()) {
		label := tool.Name()
		if spec, ok := harness.SpecFor(tool.Name()); ok {
			label = spec.Label
		}
		return s.FailBeforeLaunch(ctx, opts, EndLaunch, map[string]string{
			"message": label + " cannot ask before it uses a tool, so Trebi cannot run it with approvals.",
			"hint":    "Turn on Approve tool use automatically in the " + label + " settings.",
		})
	}

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
	run, err := s.open(ctx, opts)
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
	s.recordSkills(ctx, run.ID, opts.Skills)

	body := opts.Prompt
	if body == "" {
		body = opts.PromptPreview
	}
	origin := opts.Reason.origin(resuming)
	if kindOrDefault(opts.Kind) == KindJob && (origin == OriginLaunch || origin == OriginResume) {
		body += failurePromptSentence(run.sentinelKey())
	}
	bin, extra := cliExtraArgs(tool.Name(), plan.Command)
	req := agentwire.StartRequest{
		Harness:     agentwire.Harness(tool.Name()),
		Binary:      bin,
		WorkingDir:  opts.WorkingDir,
		Env:         mergeCLIEnv(tool.Name(), actionEnv(opts.Actions), opts.Env, plan.Env, run.ID, opts.CreateOpts.ActorToken, tool.Name()),
		MCPServers:  mcpServers(opts.Actions),
		Skills:      append(actionSkills(opts.Actions), wireSkills(opts.Skills)...),
		SessionID:   opts.SessionID,
		Model:       opts.Model,
		Effort:      opts.Effort,
		Permissions: policy,
		ExtraArgs:   extra,
	}
	if tool.Name() == agentcli.Codex {
		// The wire default is workspace-write, which blocks the network and the
		// trebi API. A job needs both.
		req.Raw = map[string]any{"sandbox": "danger-full-access"}
	}
	// Home is deliberately unset: the agentcli policy already renders the harness
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
	sess, err := s.startVisorSession(startCtx, run, req, resuming)
	if err != nil {
		// A driver that cannot resume must fail loudly (Part F): callers
		// surface ErrResumeUnsupported instead of a fake pane or a silent
		// failed row.
		if resuming && errors.Is(err, agentwire.ErrResumeUnsupported) {
			_ = s.TerminateTurn(ctx, run.ID, run.TurnCount, StatusFailed, EndLaunch, nil)
			return nil, err
		}
		s.appendRunLog(run.ID, "cli launch failed: "+err.Error())
		detail := map[string]string{"message": err.Error()}
		end := EndLaunch
		// A login that expires after the probe fails here, in the handshake.
		if class := agentwire.ClassifyError(err.Error(), 0); endReasonFor(string(class.Class)) == EndAuth {
			end = EndAuth
			detail["class"] = string(class.Class)
			detail["hint"] = hintFor(class.Class, class.Code)
			s.emitAuthProbeFailed(ctx, opts.Harness, run.ID, harness.SourceLaunch)
		}
		s.AddEvent(ctx, run.ID, "error", "launch", detail)
		_ = s.TerminateTurn(ctx, run.ID, run.TurnCount, StatusFailed, end, nil)
		got, _ := s.Get(ctx, run.ID)
		return got, nil
	}
	cr := &cliRun{sess: sess, harness: tool.Name(), tracker: agentwire.NewTracker(), turn: run.TurnCount}
	if !s.registerCLIRun(run.ID, cr) {
		closeCLISession(sess)
		return s.Get(ctx, run.ID)
	}
	s.setRunPid(ctx, run.ID, sess.PID())
	s.AddEvent(ctx, run.ID, "proc", "proc", map[string]string{"pid": strconv.Itoa(sess.PID())})

	go s.pumpCLI(run.ID, opts, cr)

	if _, err := s.promptCLI(ctx, run.ID, cr, agentwire.Prompt{Text: body, Kind: kindOrDefault(opts.Kind)}, opts.Attachments, origin); err != nil {
		s.appendRunLog(run.ID, "prompt write failed: "+err.Error())
		_ = s.TerminateTurn(ctx, run.ID, cr.turn, StatusFailed, EndLaunch, nil)
		s.unregisterCLIRun(run.ID, cr)
		closeCLISession(sess)
		got, _ := s.Get(ctx, run.ID)
		return got, nil
	}
	s.log.Info("run launched", "id", run.ID, "harness", tool.Name(), "runtime", "cli", "pid", sess.PID())
	return s.Get(ctx, run.ID)
}

// EventRequest is the run event kind that records the state of a request of
// the run. Its detail is a agentcli.TurnRequest.
const EventRequest = "request"

// Prompt origins on a user event: what made trebi write the prompt.
const (
	OriginLaunch    = "launch"
	OriginResume    = "resume"
	OriginMessage   = "message"
	OriginRequest   = "request"
	OriginAsk       = "ask"
	OriginMemoryFix = "memory_fix"
	OriginCoalesce  = "coalesce"
)

// origin is the prompt origin of the first prompt of a turn with reason r.
func (r RunReason) origin(resuming bool) string {
	switch r {
	case ReasonRequest:
		return OriginRequest
	case ReasonMessage:
		return OriginMessage
	case ReasonAsk, ReasonHandoff, ReasonReply:
		return OriginAsk
	}
	if resuming {
		return OriginResume
	}
	return OriginLaunch
}

// promptCLI writes one prompt with agentwire.Send and records it as a user
// event. The vendors do not echo a prompt, so this event is the only record.
// A harness without images gets the image paths in the text.
func (s *Service) promptCLI(ctx context.Context, runID string, cr *cliRun, p agentwire.Prompt, atts uploads.Attachments, origin string) (agentwire.Delivery, error) {
	text := p.Text
	if len(atts) > 0 && takesImages(cr.harness) {
		p.Attachments = atts.Wire()
	} else {
		p.Text = atts.Inline(text)
	}
	d, err := cr.sess.Send(ctx, p)
	if errors.Is(err, agentwire.ErrUnsupported) && len(p.Attachments) > 0 {
		p.Attachments, p.Text = nil, atts.Inline(text)
		d, err = cr.sess.Send(ctx, p)
	}
	if err != nil {
		return "", err
	}
	switch d {
	case agentwire.DeliveryHeld:
		cr.noteHeld()
	case agentwire.DeliveryPrompted:
		cr.tracker.PromptWritten()
		s.SetSessionState(ctx, runID, SessionRunning, "wire")
	}
	detail := map[string]string{"text": trunc(text, maxMessageEventBytes), "origin": origin, "delivery": string(d)}
	if len(atts) > 0 {
		detail["attachments"] = strings.Join(atts.IDs(), ",")
	}
	s.AddEvent(ctx, runID, "user", "trebi", detail)
	return d, nil
}

// takesImages reports whether a harness takes image attachments on the
// wire. Pi holds a steer with images and drops it later, so it gets the
// text form from the start.
func takesImages(harness string) bool { return harness != agentcli.Pi }

// maxMessageEventBytes bounds the text of one user or assistant event. The
// transcript reads the text back from the event, so a run report must fit.
const maxMessageEventBytes = 64 << 10

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
		case tool == agentcli.Claude && (name == "--session-id" || name == "--resume" || name == "--model" || name == "--effort" || name == "--permission-mode"):
			take()
		case tool == agentcli.Claude && name == "--dangerously-skip-permissions":
			// the permission policy carries it
		case tool == agentcli.Claude:
			out = append(out, arg)
			if !hasValue && claudeValueFlag(name) {
				out = append(out, take())
			}
		case tool == agentcli.Codex && name == agentcli.CodexFullAccess:
			// the sandbox request carries it
		case tool == agentcli.Codex && name == "-c":
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

// cliPermissionPolicy maps the harness approval setting and the launch
// command flags onto the library policy. approval auto runs unattended on
// every harness. Otherwise: Claude with --dangerously-skip-permissions and
// grok with --always-approve run unattended, Codex runs with approvalPolicy
// never, and every other harness asks trebi. Claude without the flag inherits
// the operator's own mode: trebi must not force `default` and add approval
// gates the interactive CLI would not show.
func cliPermissionPolicy(tool, command, approval string) agentwire.PermissionPolicy {
	if approval == "auto" {
		return agentwire.PermissionPolicy{Mode: agentwire.PermissionAuto}
	}
	switch tool {
	case agentcli.Grok:
		if hasCommandFlag(command, "--always-approve") {
			return agentwire.PermissionPolicy{Mode: agentwire.PermissionAuto}
		}
	case agentcli.Claude:
		if hasCommandFlag(command, "--dangerously-skip-permissions") {
			return agentwire.PermissionPolicy{Mode: agentwire.PermissionAuto}
		}
		return agentwire.PermissionPolicy{Mode: agentwire.PermissionInherit}
	case agentcli.Codex:
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
		merged["PATH"] = binCurrent + string(os.PathListSeparator) + basePath(actEnv)
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

// setRunModel records the model that the harness reports on the run and
// on the turn.
func (s *Service) setRunModel(ctx context.Context, runID string, turn int, model string) {
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET tokens_model=?, updated_at=? WHERE id=? AND turn_count=?`,
			model, time.Now().UTC().Format(time.RFC3339Nano), runID, turn); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE run_turns SET tokens_model=? WHERE run_id=? AND n=?`, model, runID, turn)
		return err
	})
	if err != nil {
		s.log.Warn("set run model", "run", runID, "error", err)
	}
}

// setRunSessionID records the vendor session id learned on the wire on the
// run and on the turn. A resume can report a new id; the latest one wins.
func (s *Service) setRunSessionID(ctx context.Context, runID string, turn int, sessionID string) {
	if sessionID == "" {
		return
	}
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET session_id=?, updated_at=? WHERE id=? AND turn_count=?`,
			sessionID, time.Now().UTC().Format(time.RFC3339Nano), runID, turn); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE run_turns SET session_id=? WHERE run_id=? AND n=?`, sessionID, runID, turn)
		return err
	})
	if err != nil {
		s.log.Warn("set run session id", "run", runID, "error", err)
	}
}

// cliFixMessage is the memory fix on the subprocess path: no completion
// sentinel (the wire owns completion, A.5).
func cliFixMessage(issues memoryIssues) string {
	return strings.Join(issues.lines(), "\n") + "\n" + memoryFixAsk + "finish the run."
}

// pumpCLI drains the session event stream: it owns session state, the run
// log, approvals, the memory gate, and the terminal transition.
func (s *Service) pumpCLI(runID string, opts LaunchOpts, cr *cliRun) {
	ctx := context.Background()
	defer s.unregisterCLIRun(runID, cr)
	sawResult := false
	var lastErr *agentwire.Event
	for ve := range cr.sess.Events() {
		ev := ve.Event
		if ve.Seq != 0 && ve.Seq <= cr.replay {
			cr.replayEvent(ev, opts.Model)
			continue
		}
		cr.seq = max(cr.seq, ve.Seq)
		// A delta writes no checkpoint: after a crash, a few deltas come twice.
		if ev.Type != agentwire.EventAssistant || !ev.Delta {
			s.setRunVisorSeq(ctx, runID, cr.seq)
		}
		snap := cr.tracker.Observe(ev)
		s.logCLIEvent(runID, ev)
		switch ev.Type {
		case agentwire.EventInit:
			if ev.SessionID != "" {
				s.setRunSessionID(ctx, runID, cr.turn, ev.SessionID)
			}
			// Claude puts the model of the session in the init text.
			if ev.Text != "" {
				cr.setModel(ev.Text)
				s.setRunModel(ctx, runID, cr.turn, ev.Text)
			}
			s.AddEvent(ctx, runID, "session.init", "wire", map[string]string{"session_id": ev.SessionID, "model": ev.Text})
		case agentwire.EventAssistant:
			cr.noteText(ev)
			s.AddEvent(ctx, runID, "assistant", "wire", map[string]string{
				"text": trunc(ev.Text, maxMessageEventBytes), "delta": strconv.FormatBool(ev.Delta),
			})
		case agentwire.EventUser:
			s.AddEvent(ctx, runID, "user", "wire", map[string]string{"text": trunc(ev.Text, maxMessageEventBytes)})
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
			cr.noteText(ev)
			s.addCLIToolEvent(ctx, runID, ev)
		case agentwire.EventLimits:
			if ev.Limits != nil {
				if err := s.usage.Record(ctx, opts.Harness, *ev.Limits, harness.UsageSourceRun, time.Time{}); err != nil {
					s.log.Warn("record harness usage", "run", runID, "error", err)
				}
			}
		case agentwire.EventPermission:
			s.handleCLIPermission(ctx, runID, cr, ev)
		case agentwire.EventError:
			if ev.Code == agentwire.CodeHeldPromptDropped {
				cr.takeHeld()
				s.AddEvent(ctx, runID, "message.dropped", "wire", map[string]string{"text": trunc(ev.Text, maxMessageEventBytes)})
				continue
			}
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
			if run, err := s.Get(ctx, runID); err == nil && run.TurnCount == cr.turn && !run.Status.IsTerminal() && !sawResult {
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

// replayEvent reads one event that a daemon before this one handled. It
// builds the tracker and the turn state again, and writes nothing.
func (cr *cliRun) replayEvent(ev agentwire.Event, launchModel string) {
	cr.tracker.Observe(ev)
	switch ev.Type {
	case agentwire.EventInit:
		if ev.Text != "" {
			cr.setModel(ev.Text)
		}
	case agentwire.EventAssistant, agentwire.EventTool:
		cr.noteText(ev)
	case agentwire.EventResult:
		if ev.Result != nil {
			cr.result = ev.Result
			cr.addUsage(cr.tokenUsage(ev.Result, launchModel))
		}
	}
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

// LabelTurns sets RunTurn on each transcript entry: the last run turn that
// started at or before the entry. An entry with no time takes the number of
// the entry before it.
func (s *Service) LabelTurns(ctx context.Context, runID string, entries []agentcli.Turn) error {
	turns, err := s.Turns(ctx, runID)
	if err != nil {
		return err
	}
	n := 1
	for i := range entries {
		if at := entries[i].At; !at.IsZero() {
			n = 1
			for _, t := range turns {
				if !t.StartedAt.After(at) {
					n = t.N
				}
			}
		}
		entries[i].RunTurn = n
	}
	return nil
}

// allEvents reads all events of a run, page by page.
func (s *Service) allEvents(ctx context.Context, runID string) ([]RunEvent, error) {
	var evs []RunEvent
	var after int64
	for {
		page, err := s.EventsSince(ctx, runID, after, 2000)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return evs, nil
		}
		evs = append(evs, page...)
		after = page[len(page)-1].ID
	}
}

// CLITurns reconstructs the transcript from wire events (Part H: prefer live
// events over the vendor jsonl). Empty means the pump recorded nothing usable,
// and the caller falls back to the vendor session file.
func (s *Service) CLITurns(ctx context.Context, runID string) ([]agentcli.Turn, error) { //nolint:gocyclo // one switch over the event kinds keeps the transcript order in one place
	evs, err := s.allEvents(ctx, runID)
	if err != nil {
		return nil, err
	}
	var turns []agentcli.Turn
	// Tool calls open on a "started" event and close on a "completed"/"failed"
	// one. The closing frame may carry only the id (claude tool_result frames
	// carry no name), so keep the opening call until its result lands.
	type pendingTool struct {
		call agentcli.ToolCall
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
			if d["text"] != "" || d["attachments"] != "" {
				turn := agentcli.Turn{Role: "user", Text: d["text"], At: ev.At, Origin: d["origin"], Delivery: d["delivery"]}
				if ids := d["attachments"]; ids != "" {
					turn.Attachments = strings.Split(ids, ",")
				}
				turns = append(turns, turn)
			}
		case "assistant":
			if d["text"] != "" && d["delta"] != "true" {
				turns = append(turns, agentcli.Turn{Role: "assistant", Text: d["text"], At: ev.At})
			}
		case "tool":
			id := d["id"]
			if id == "" {
				continue
			}
			switch d["status"] {
			case "completed", "failed":
				var call agentcli.ToolCall
				if open := byID[id]; open != nil {
					call = open.call
					open.done = true
				} else if d["name"] != "" {
					call = agentcli.ToolCall{Name: d["name"], Input: d["input"]}
				} else {
					continue
				}
				if d["output"] != "" {
					call.Output = d["output"]
				}
				turns = append(turns, agentcli.Turn{Role: "tools", Tools: []agentcli.ToolCall{call}, At: ev.At})
			default:
				if d["name"] == "" {
					continue
				}
				if open := byID[id]; open != nil {
					open.call = agentcli.ToolCall{Name: d["name"], Input: d["input"], Output: d["output"]}
					open.at = ev.At
					continue
				}
				open := &pendingTool{call: agentcli.ToolCall{Name: d["name"], Input: d["input"], Output: d["output"]}, at: ev.At}
				pending = append(pending, open)
				byID[id] = open
			}
		}
	}
	for _, open := range pending {
		if open.done {
			continue
		}
		turns = append(turns, agentcli.Turn{Role: "tools", Tools: []agentcli.ToolCall{open.call}, At: open.at})
	}
	if turns == nil {
		turns = []agentcli.Turn{}
	}
	return turns, nil
}

// WithRequests merges the request events of the run into turns by time. Each
// request is one turn at the time of its first event, with its latest state.
func (s *Service) WithRequests(ctx context.Context, runID string, turns []agentcli.Turn) ([]agentcli.Turn, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT at, detail_json FROM run_events
		WHERE run_id=? AND kind=? ORDER BY id`, runID, EventRequest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reqs []agentcli.Turn
	byID := map[string]int{}
	for rows.Next() {
		var at string
		var detail sql.NullString
		if err := rows.Scan(&at, &detail); err != nil {
			return nil, err
		}
		var tr agentcli.TurnRequest
		if !detail.Valid || json.Unmarshal([]byte(detail.String), &tr) != nil || tr.ID == "" {
			continue
		}
		if i, ok := byID[tr.ID]; ok {
			*reqs[i].Request = tr
			continue
		}
		t, _ := time.Parse(time.RFC3339Nano, at)
		byID[tr.ID] = len(reqs)
		reqs = append(reqs, agentcli.Turn{Role: EventRequest, At: t, Request: &tr})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		return turns, nil
	}
	out := make([]agentcli.Turn, 0, len(turns)+len(reqs))
	i := 0
	for _, t := range turns {
		for i < len(reqs) && !t.At.IsZero() && reqs[i].At.Before(t.At) {
			out = append(out, reqs[i])
			i++
		}
		out = append(out, t)
	}
	return append(out, reqs[i:]...), nil
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
	// A stopped run has no one to answer: the resumed turn asks again.
	if cr.isTerminal() {
		return
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
	// The row is final: a session that drains must not open a segment again.
	if cr.isTerminal() {
		return
	}
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
	s.setRunSessionID(ctx, runID, cr.turn, res.SessionID)
	tokens := cr.addUsage(cr.tokenUsage(res, opts.Model))
	if cr.takeHeld() {
		// agentwire sent the held messages at this result: the session runs
		// the next vendor turn.
		cr.tracker.PromptWritten()
		cr.result = nil
		return
	}
	if res.IsError {
		s.failCLIResult(ctx, runID, opts, cr, res, tokens)
		return
	}
	run, err := s.Get(ctx, runID)
	if err != nil {
		return
	}
	if kindOrDefault(run.Kind) == KindJob {
		text := cr.text()
		if text == "" {
			text = res.Text
		}
		if reason, ok := ReportedFailure(text, ""); ok {
			s.reportFailure(ctx, runID, "wire", reason)
			s.terminateCLI(ctx, runID, cr, StatusFailed, EndFailed, tokens)
			return
		}
	}
	// Memory gate: the fix is a second prompt on the same session.
	mem := opts.Memory
	if mem == nil {
		mem = s.memoryFor(ctx, run)
	}
	if mem != nil {
		issues := s.gateMemory(ctx, run, mem)
		if len(issues) > 0 {
			if cr.memoryFix < maxMemoryFixes {
				cr.memoryFix++
				s.requestMemoryFix(ctx, runID, "wire", cr.memoryFix, issues)
				if _, err := s.promptCLI(ctx, runID, cr, agentwire.Prompt{Text: cliFixMessage(issues), Kind: "memory_fix"}, nil, OriginMemoryFix); err != nil {
					s.recordMemoryInvalid(ctx, run, issues, "wire")
					s.terminateCLI(ctx, runID, cr, StatusFailed, EndLaunch, tokens)
					return
				}
				cr.result = nil
				return
			}
			s.recordMemoryInvalid(ctx, run, issues, "wire")
		} else {
			s.recordMemoryValid(ctx, run)
		}
	}
	// A run that waits for a request answer parks at once: keep-alive would
	// hold the session for nothing.
	if run.Session.KeepAliveMS > 0 && !s.mustPark(ctx, runID) {
		cr.markKeepAlive(time.Now())
		s.AddEvent(ctx, runID, "result", "wire", map[string]string{"keep_alive": "true"})
		s.SetSessionState(ctx, runID, SessionIdle, "wire")
		return
	}
	s.appendRunLog(runID, "run complete")
	s.terminateCLI(ctx, runID, cr, StatusComplete, EndResult, tokens)
}

// reportFailure records the reason of a run that reports it cannot do its
// task. The alert takes its body from this event.
func (s *Service) reportFailure(ctx context.Context, runID, source, reason string) {
	reason = trunc(reason, 1000)
	s.AddEvent(ctx, runID, "error", source, map[string]string{"message": reason, "class": ClassReported})
	s.appendRunLog(runID, "run reported failure: "+reason)
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

// tokenUsage reads the usage of one result. The model falls back to the
// session model, then to the launch model.
func (cr *cliRun) tokenUsage(res *agentwire.Result, launchModel string) *pricing.TokenUsage {
	if res == nil {
		return nil
	}
	cr.mu.Lock()
	model := cmp.Or(res.Usage.Model, cr.usage.Model, launchModel)
	cr.mu.Unlock()
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
	if err := s.TerminateTurn(ctx, runID, cr.turn, status, end, tokens); err != nil {
		s.log.Warn("cli terminate", "run", runID, "error", err)
	}
	go func() {
		_ = cr.sess.Close(context.Background())
	}()
}

// afterCLIExit reconciles a process that ended without a result.
func (s *Service) afterCLIExit(ctx context.Context, runID string, opts LaunchOpts, cr *cliRun, sawResult bool, lastErr *agentwire.Event) {
	if cr.isTerminal() {
		return
	}
	run, err := s.Get(ctx, runID)
	if err != nil || run.Status.IsTerminal() || run.TurnCount != cr.turn {
		return
	}
	if st := cr.sess.End(); !sawResult && st == agentvisor.StateLost {
		// The visor lost the process, not the turn: resume it.
		s.parkRestart(ctx, run, cr.usageSoFar(), "session "+string(st))
		return
	}
	if sawResult {
		// A result already landed; the process exiting just ends the
		// keep-alive window early. Complete the run rather than reporting a
		// missing session.
		if opts.Memory != nil {
			if issues := s.gateMemory(ctx, run, opts.Memory); len(issues) > 0 {
				s.recordMemoryInvalid(ctx, run, issues, "wire")
			} else {
				s.recordMemoryValid(ctx, run)
			}
		}
		end := EndResult
		if run.Session.KeepAliveMS > 0 {
			end = EndKeepAlive
		}
		_ = s.TerminateTurn(ctx, runID, cr.turn, StatusComplete, end, cr.usageSoFar())
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
	_ = s.TerminateTurn(ctx, runID, cr.turn, StatusFailed, end, cr.usageSoFar())
}

// stopCLIRun interrupts and closes a live CLI session (kill/replace/reaper).
func (s *Service) stopCLIRun(runID string, cr *cliRun) {
	if cr == nil {
		return
	}
	// The caller writes the terminal row, so the pump must not.
	cr.setTerminal()
	_ = cr.sess.Interrupt(context.Background())
	go func() {
		_ = cr.sess.Close(context.Background())
	}()
}

// teardownRuntime stops whichever runtime owns the run, without terminating
// it. The caller writes the terminal row.
func (s *Service) teardownRuntime(ctx context.Context, run *Run) {
	removeRunEnvFile(run)
	if cr := s.cliRunFor(run.ID); cr != nil && (run.TurnCount == 0 || cr.turn == run.TurnCount) {
		s.stopCLIRun(run.ID, cr)
	}
	if run.PaneSession != "" {
		_ = s.panes.Key(ctx, run.PaneSession, "c-c")
		time.Sleep(100 * time.Millisecond)
		s.closeSession(ctx, run.ID, run.PaneSession)
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

// RestartPrompt is the prompt of the turn that continues a run after a daemon
// restart stopped it.
const RestartPrompt = "Trebi restarted and stopped your last turn. The interrupt and any rejected or cancelled tool call " +
	"came from that restart, not from the user. Continue the task from where you stopped. " +
	"A tool call that was in progress may or may not have completed: check its result before you run it again."

// DetachCLIRuns lets go of each live CLI run at daemon shutdown. The session
// stays alive in the visor, and the next daemon attaches to it again. A run
// whose session did not start yet parks, so that it resumes after boot.
func (s *Service) DetachCLIRuns(ctx context.Context) {
	s.mu.Lock()
	s.stopping = true
	live := maps.Clone(s.cliRuns)
	var launching []string
	for id := range s.launching {
		if live[id] == nil {
			launching = append(launching, id)
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for id, cr := range live {
		wg.Go(func() {
			// The pump writes nothing after this.
			cr.setTerminal()
			cr.sess.Detach(ctx)
			s.setRunVisorSeq(ctx, id, cr.seq)
		})
	}
	for _, id := range launching {
		wg.Go(func() { s.pauseLaunch(ctx, id) })
	}
	wg.Wait()
}

// pauseLaunch writes the row of a run whose session did not start yet.
func (s *Service) pauseLaunch(ctx context.Context, runID string) {
	run, err := s.Get(ctx, runID)
	if err != nil || run.Status != StatusRunning {
		return
	}
	// A first turn has no vendor session before its prompt.
	if run.TurnCount < 2 {
		run.SessionID = ""
	}
	s.parkRestart(ctx, run, nil, "daemon restart (launch)")
}

// parkRestart parks run with end reason restart. A run with no session to
// resume ends session_gone.
func (s *Service) parkRestart(ctx context.Context, run *Run, tokens *pricing.TokenUsage, reason string) {
	status, end, kind := StatusParked, EndRestart, "paused"
	if run.SessionID == "" {
		status, end, kind = StatusExited, EndSessionGone, "recovered"
	}
	if err := s.TerminateTurn(ctx, run.ID, run.TurnCount, status, end, tokens); err != nil {
		s.log.Warn("cli pause", "run", run.ID, "error", err)
		return
	}
	if err := s.expireApprovals(ctx, run.ID); err != nil {
		s.log.Warn("expire approvals", "run", run.ID, "error", err)
	}
	s.AddEvent(ctx, run.ID, kind, "proc", map[string]string{"reason": reason})
	s.log.Info("cli run paused", "id", run.ID, "end_reason", end)
}

// closeCLISession stops a session in the background.
func closeCLISession(sess *visor.Session) {
	go func() { _ = sess.Close(context.Background()) }() // the session ends in the visor in any case
}

// RecoverCLI is the daemon-boot reconcile. A live cli run whose visor session
// lives gets its pump again. The pump reads the events that the last daemon
// handled without a write, then continues. Any other live cli run parks with
// end reason restart (session_gone when it has no session), so that it
// resumes after boot.
func (s *Service) RecoverCLI(ctx context.Context) {
	list, err := s.List(ctx, ListOpts{Status: string(StatusRunning), Limit: 500})
	if err != nil {
		s.log.Warn("list live runs", "error", err)
		return
	}
	for _, run := range list {
		if run.Runtime != "cli" {
			continue
		}
		if err := s.attachCLI(ctx, run); err != nil {
			s.log.Info("cli run not attached", "id", run.ID, "reason", err)
			s.parkRestart(ctx, run, nil, "daemon restart (recovered)")
		}
	}
}

// attachCLI follows the live visor session of run again.
func (s *Service) attachCLI(ctx context.Context, run *Run) error {
	id, seq, err := s.runVisor(ctx, run.ID)
	if err != nil || id == "" {
		return fmt.Errorf("no visor session: %w", err)
	}
	host, err := s.SessionHost(ctx)
	if err != nil {
		return err
	}
	sess, err := host.Session(ctx, id)
	if err != nil {
		return err
	}
	if !visor.Followable(sess.State) {
		return fmt.Errorf("visor session %s is %s", id, sess.State)
	}
	vs, err := visor.Follow(ctx, host, sess, 0)
	if err != nil {
		return err
	}
	opts := LaunchOpts{CreateOpts: CreateOpts{WorkspaceID: run.Workspace, JobID: run.JobID, Kind: run.Kind, Harness: run.Harness, Model: run.Model}}
	if s.memRes != nil {
		opts.Memory = s.memRes.MemoryLaunchFor(ctx, run)
	}
	cr := &cliRun{sess: vs, harness: run.Harness, tracker: agentwire.NewTracker(), turn: run.TurnCount, seq: seq, replay: seq}
	if !s.registerCLIRun(run.ID, cr) {
		vs.Detach(ctx)
		return errors.New("the daemon stops")
	}
	go s.pumpCLI(run.ID, opts, cr)
	s.AddEvent(ctx, run.ID, "attached", "proc", map[string]string{"session": id})
	s.log.Info("cli run attached", "id", run.ID, "visor_session", id, "after", seq)
	return nil
}

// CloseCLIRuns closes the visor that this process opened (tests). A visor
// service keeps its sessions.
func (s *Service) CloseCLIRuns(ctx context.Context) {
	s.closeOwnHost(ctx)
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

// Limits reads the plan limits of a harness account without a run.
func (s *Service) Limits(ctx context.Context, h agentwire.Harness, q agentwire.ModelQuery) (agentwire.Limits, error) {
	return s.agentwireRuntime().Limits(ctx, h, q)
}

// AgentwireRuntime exposes the shared subprocess runtime for callers that
// must reach library-level state, such as the detect cache.
func (s *Service) AgentwireRuntime() *agentwire.Runtime {
	return s.agentwireRuntime()
}
