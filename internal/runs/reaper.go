package runs

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/runtime/cli"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// Reaper watches live runs for sentinel, idle timeout, max duration, and gone sessions.
type Reaper struct {
	svc      *Service
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger

	mu          sync.Mutex
	idleSince   map[string]time.Time
	limitHits   map[string]int
	dialogHits  map[string]map[string]int
	sentinels   map[string]int
	fixAttempts map[string]int
	memDone     map[string]bool
	lastPane    map[string]string
	stop        chan struct{}
}

// NewReaper builds a reaper bound to the runs service.
func NewReaper(svc *Service) *Reaper {
	return &Reaper{
		svc:         svc,
		interval:    5 * time.Second,
		now:         time.Now,
		log:         slog.Default().With("source", "reaper"),
		idleSince:   make(map[string]time.Time),
		limitHits:   make(map[string]int),
		dialogHits:  make(map[string]map[string]int),
		sentinels:   make(map[string]int),
		fixAttempts: make(map[string]int),
		memDone:     make(map[string]bool),
		lastPane:    make(map[string]string),
		stop:        make(chan struct{}),
	}
}

// Start runs the reaper loop until ctx is done or Stop.
func (r *Reaper) Start(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case <-t.C:
			r.Sweep(ctx)
		}
	}
}

// Stop ends the loop.
func (r *Reaper) Stop() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
}

// Sweep checks all running runs once.
func (r *Reaper) Sweep(ctx context.Context) {
	list, err := r.svc.List(ctx, ListOpts{Status: string(StatusRunning), Limit: 500})
	if err != nil {
		r.log.Warn("reaper list", "error", err)
		return
	}
	sessions, listOK := liveTmuxSessions()
	now := r.now()
	for _, run := range list {
		r.checkOne(ctx, run, now, sessions, listOK)
	}
}

func liveTmuxSessions() (map[string]struct{}, bool) {
	out, err := tmux.ListSessions()
	if err != nil {
		return nil, false
	}
	set := map[string]struct{}{}
	for _, line := range strings.Split(out, "\n") {
		name, _, _ := strings.Cut(line, "\t")
		name = strings.TrimSpace(name)
		if name != "" {
			set[name] = struct{}{}
		}
	}
	return set, true
}

func (r *Reaper) checkOne(ctx context.Context, run *Run, now time.Time, sessions map[string]struct{}, listOK bool) {
	r.mu.Lock()
	_, keeping := r.idleSince["keep:"+run.ID]
	r.mu.Unlock()
	// CLI keep-alive suppresses max duration exactly like the pane path: the
	// session is done, it lingers for follow-ups until the window closes.
	if !keeping && run.Runtime == "cli" && run.Session.KeepAliveMS > 0 {
		if cr := r.svc.cliRunFor(run.ID); cr != nil {
			keeping = cr.keepAliveWithin(run.Session.KeepAliveMS, now)
		}
	}

	// CLI runs are driven by the event pump and read the tracker, not a pane.
	// Keep-alive expiry and the idle timeout are decided before max duration
	// so an expired window reports keep_alive, not a timeout.
	if run.Runtime == "cli" {
		finished, linger := r.checkCLI(ctx, run, now)
		if finished {
			return
		}
		if linger {
			keeping = true
		}
		if !keeping && run.Session.MaxDurationMS > 0 {
			if now.Sub(run.StartedAt) >= time.Duration(run.Session.MaxDurationMS)*time.Millisecond {
				r.finish(ctx, run, StatusTimedOut, EndMaxDuration)
			}
		}
		return
	}

	if !keeping && run.Session.MaxDurationMS > 0 {
		if now.Sub(run.StartedAt) >= time.Duration(run.Session.MaxDurationMS)*time.Millisecond {
			r.finish(ctx, run, StatusTimedOut, EndMaxDuration)
			return
		}
	}

	// no tmux (fake already terminal or deferred without session).
	// Native runs have no pane; max-duration already ran above.
	if run.TmuxSession == "" {
		return
	}
	if listOK {
		if _, ok := sessions[run.TmuxSession]; !ok {
			r.finish(ctx, run, StatusExited, EndSessionGone)
			return
		}
	} else if !tmux.HasSession(run.TmuxSession) {
		r.finish(ctx, run, StatusExited, EndSessionGone)
		return
	}

	screen, err := tmux.ReadScreen(run.TmuxSession)
	if err != nil {
		return
	}

	r.capturePaneLog(run, screen)

	if tool, ok := tmux.ByName(run.Harness); ok && !r.svc.IsLaunching(run.ID) {
		r.handleDialogs(ctx, run, tool, screen)
	}

	idle := run.screenIdle(screen)
	if r.svc.SessionState(run.ID) != SessionWaiting {
		if idle {
			r.svc.SetSessionState(ctx, run.ID, SessionIdle, "screen")
		} else {
			r.svc.SetSessionState(ctx, run.ID, SessionRunning, "screen")
		}
	}

	// Visible pane is a few dozen rows; the sentinel often scrolls off.
	completeText := screen
	if hist, herr := tmux.ReadHistory(run.TmuxSession); herr == nil && hist != "" {
		completeText = hist
	}
	if DetectCompletion(completeText, run.ID) {
		r.gateCompletion(ctx, run, completeText, now)
		return
	}

	if r.checkIdle(ctx, run, screen, now) {
		return
	}

	// auth / limit chrome (coarse markers)
	if tmux.IsAuthBlocker(screen) || authFailed(screen) {
		r.svc.emitAuthProbeFailed(ctx, run.Harness, run.ID, harness.SourceReaper)
		r.finish(ctx, run, StatusFailed, EndAuth)
		return
	}
	if tmux.IsLimitBlocker(screen) {
		r.mu.Lock()
		r.limitHits[run.ID]++
		n := r.limitHits[run.ID]
		r.mu.Unlock()
		if n >= 2 {
			r.svc.emitLimitExceeded(ctx, run.Harness, run.ID)
			r.finish(ctx, run, StatusFailed, EndLimit)
		}
		return
	}
	r.mu.Lock()
	cleared := r.limitHits[run.ID] >= 2
	delete(r.limitHits, run.ID)
	r.mu.Unlock()
	if cleared {
		r.svc.emitLimitCleared(ctx, run.Harness)
	}
}

func (r *Reaper) handleDialogs(ctx context.Context, run *Run, tool tmux.Tool, screen string) {
	r.mu.Lock()
	hits := r.dialogHits[run.ID]
	if hits == nil {
		hits = map[string]int{}
		r.dialogHits[run.ID] = hits
	}
	r.mu.Unlock()

	acts := tool.HandleStartup(screen)
	fired := 0
	for _, act := range acts {
		key := tmux.DialogKey(screen, act)
		r.mu.Lock()
		n := hits[key]
		r.mu.Unlock()
		if n >= tmux.MaxDialogTries {
			continue
		}
		if err := tmux.SendAction(run.TmuxSession, act); err != nil {
			r.log.Warn("dialog send", "run", run.ID, "error", err)
			continue
		}
		r.mu.Lock()
		hits[key]++
		r.mu.Unlock()
		fired++
	}

	if len(acts) > 0 && fired == 0 {
		q, _ := tmux.ChoiceDialog(screen)
		r.markDialogWaiting(ctx, run, q)
	} else if len(acts) == 0 {
		if q, ok := tmux.ChoiceDialog(screen); ok {
			r.markDialogWaiting(ctx, run, q)
		}
	}

	if !tool.IsReady(screen) {
		return
	}
	p, ok := r.svc.takePendingPrompt(run.ID)
	if !ok {
		return
	}
	if err := sendPrompt(p.session, p.prompt, p.harness); err != nil {
		r.log.Warn("pending prompt", "run", run.ID, "error", err)
		r.svc.stashPendingPrompt(run.ID, p)
		return
	}
	r.svc.SetSessionState(ctx, run.ID, SessionRunning, "launch")
	r.log.Info("run launched after dialog", "id", run.ID)
}

func (r *Reaper) markDialogWaiting(ctx context.Context, run *Run, question string) {
	if r.svc.SessionState(run.ID) == SessionWaiting {
		return
	}
	if question == "" {
		question = "harness is waiting on a choice"
	}
	r.svc.SetSessionState(ctx, run.ID, SessionWaiting, "dialog")
	if _, err := r.svc.OpenApproval(ctx, run.ID, question); err != nil {
		r.log.Warn("open approval", "run", run.ID, "error", err)
	}
	r.svc.AddEvent(ctx, run.ID, "waiting", "dialog", map[string]string{"question": question})
}

// gateCompletion runs the memory gate on a fresh completion sentinel (B22
// §3.3). Only a rising exact-line sentinel count re-runs the gate, so the fix
// message's quoted sentinel does not retrigger it.
func (r *Reaper) gateCompletion(ctx context.Context, run *Run, screen string, now time.Time) {
	n := countSentinels(screen, run.ID)
	r.mu.Lock()
	if n <= r.sentinels[run.ID] {
		r.mu.Unlock()
		return
	}
	r.sentinels[run.ID] = n
	attempts := r.fixAttempts[run.ID]
	r.mu.Unlock()

	issues := r.svc.gateMemory(ctx, run, r.svc.memoryFor(ctx, run))
	if len(issues) == 0 {
		r.finishComplete(ctx, run, now)
		return
	}
	if attempts >= maxMemoryFixes {
		r.markMemDone(run.ID)
		r.svc.recordMemoryInvalid(ctx, run, issues, "reaper")
		r.finishComplete(ctx, run, now)
		return
	}
	attempts++
	r.mu.Lock()
	r.fixAttempts[run.ID] = attempts
	// The fix keeps the pane busy; reset the idle clock.
	delete(r.idleSince, run.ID)
	r.mu.Unlock()
	r.svc.requestMemoryFix(ctx, run.ID, "reaper", attempts, issues)
	if err := tmux.Send(run.TmuxSession, fixMessage(issues, run.ID)); err != nil {
		r.log.Warn("memory fix send", "run", run.ID, "error", err)
		return
	}
	time.Sleep(300 * time.Millisecond)
	_ = tmux.SendKey(run.TmuxSession, "enter")
}

// gateAtFinish validates memory when a run ends without a passing sentinel
// (idle, kill, session gone, max duration). No fix cycle: record and alert.
func (r *Reaper) gateAtFinish(ctx context.Context, run *Run) {
	r.mu.Lock()
	done := r.memDone[run.ID]
	r.mu.Unlock()
	if done {
		return
	}
	issues := r.svc.gateMemory(ctx, run, r.svc.memoryFor(ctx, run))
	if len(issues) == 0 {
		return
	}
	r.markMemDone(run.ID)
	r.svc.recordMemoryInvalid(ctx, run, issues, "reaper")
}

func (r *Reaper) markMemDone(runID string) {
	r.mu.Lock()
	r.memDone[runID] = true
	r.mu.Unlock()
}

// finishComplete ends a completed run; keep_alive lets the session linger first.
func (r *Reaper) finishComplete(ctx context.Context, run *Run, now time.Time) {
	if run.Session.KeepAliveMS <= 0 {
		r.finish(ctx, run, StatusComplete, EndSentinel)
		return
	}
	// Mark complete but leave the session; a later sweep kills after keep_alive.
	key := "keep:" + run.ID
	r.mu.Lock()
	since, ok := r.idleSince[key]
	if !ok {
		r.idleSince[key] = now
		r.mu.Unlock()
		r.svc.AddEvent(ctx, run.ID, "sentinel", "reaper", map[string]string{"keep_alive": "true"})
		return
	}
	r.mu.Unlock()
	if now.Sub(since) < time.Duration(run.Session.KeepAliveMS)*time.Millisecond {
		return
	}
	r.finish(ctx, run, StatusComplete, EndKeepAlive)
}

// checkIdle tracks the idle-timeout window. True means checkOne must stop.
func (r *Reaper) checkIdle(ctx context.Context, run *Run, screen string, now time.Time) bool {
	if run.Session.IdleTimeoutMS <= 0 || r.svc.SessionState(run.ID) == SessionWaiting {
		return false
	}
	if !run.screenIdle(screen) {
		r.mu.Lock()
		delete(r.idleSince, run.ID)
		r.mu.Unlock()
		return false
	}
	r.mu.Lock()
	since, ok := r.idleSince[run.ID]
	if !ok {
		r.idleSince[run.ID] = now
		r.mu.Unlock()
		return true
	}
	r.mu.Unlock()
	if now.Sub(since) >= time.Duration(run.Session.IdleTimeoutMS)*time.Millisecond {
		r.finish(ctx, run, StatusTimedOut, EndIdleTimeout)
		return true
	}
	return false
}

func (r *Reaper) capturePaneLog(run *Run, screen string) {
	r.mu.Lock()
	prev := r.lastPane[run.ID]
	r.lastPane[run.ID] = screen
	r.mu.Unlock()
	if extra := paneDelta(prev, screen); extra != "" {
		r.svc.appendRunLog(run.ID, paneCaptureHeader(extra)+"\n"+extra)
	}
}

func (r *Reaper) finish(ctx context.Context, run *Run, status Status, end EndReason) {
	if status != StatusComplete {
		r.gateAtFinish(ctx, run)
	}
	r.svc.teardownRuntime(ctx, run)
	if err := r.svc.Terminate(ctx, run.ID, status, end, nil); err != nil {
		r.log.Warn("reaper terminate", "run", run.ID, "error", err)
	}
	r.svc.takePendingPrompt(run.ID)
	r.mu.Lock()
	delete(r.idleSince, run.ID)
	delete(r.idleSince, "keep:"+run.ID)
	delete(r.idleSince, "cli-idle:"+run.ID)
	delete(r.limitHits, run.ID)
	delete(r.dialogHits, run.ID)
	delete(r.sentinels, run.ID)
	delete(r.fixAttempts, run.ID)
	delete(r.memDone, run.ID)
	delete(r.lastPane, run.ID)
	r.mu.Unlock()
	// Terminate records the finish ("run finished"); nothing to add here.
}

// checkCLI reads the CLI tracker instead of a pane (G.5): idle timeout is
// suspended while an approval is open and blocked by in-flight tools;
// keep-alive closes a completed session after its window. finished=true means
// the run is terminal; linger=true means a keep-alive window is open and max
// duration must not fire.
func (r *Reaper) checkCLI(ctx context.Context, run *Run, now time.Time) (finished, linger bool) {
	// A launch in flight (spawn, OpenCode health wait) is not a gone session.
	if r.svc.IsLaunching(run.ID) {
		return false, false
	}
	cr := r.svc.cliRunFor(run.ID)
	if cr == nil {
		// A sweep can race the launch window (row inserted, session not yet
		// registered); only a fresh row is protected by the grace period.
		if now.Sub(run.StartedAt) < cliLaunchGrace {
			return false, false
		}
		// The pump unregisters after the terminal transition; a live row with
		// no session means the child is gone.
		r.finish(ctx, run, StatusExited, EndSessionGone)
		return true, false
	}
	snap := cr.tracker.Snapshot()
	switch snap.State {
	case cli.StatusWaiting:
		r.svc.SetSessionState(ctx, run.ID, SessionWaiting, "wire")
	case cli.StatusRunning:
		r.svc.SetSessionState(ctx, run.ID, SessionRunning, "wire")
	case cli.StatusIdle:
		r.svc.SetSessionState(ctx, run.ID, SessionIdle, "wire")
	}
	if snap.Stalled && !cr.stalledNow() {
		cr.setStalled(true)
		r.svc.AddEvent(ctx, run.ID, "agent.stall", "wire", map[string]string{
			"last_event_at":  snap.LastEventAt.UTC().Format(time.RFC3339Nano),
			"inflight_tools": strconv.Itoa(snap.InflightTools),
		})
	} else if !snap.Stalled && cr.stalledNow() {
		cr.setStalled(false)
	}

	// Keep-alive: a result landed and the session lingers. The clock starts
	// at the first idle sweep after the result.
	if run.Session.KeepAliveMS > 0 && snap.LastResult != nil && snap.State == cli.StatusIdle {
		if cr.keepAliveExpired(run.Session.KeepAliveMS, now) {
			r.finish(ctx, run, StatusComplete, EndKeepAlive)
			return true, false
		}
		if cr.keepAliveSince.IsZero() {
			cr.markKeepAlive(now)
		}
		return false, true
	}
	// Idle timeout: only a genuinely idle session (no turn, no approval); in
	// flight tools block it. A keep-alive window supersedes it.
	keepAlive := run.Session.KeepAliveMS > 0 && snap.LastResult != nil
	if run.Session.IdleTimeoutMS > 0 && snap.State == cli.StatusIdle && !keepAlive {
		key := "cli-idle:" + run.ID
		r.mu.Lock()
		since, ok := r.idleSince[key]
		if !ok {
			r.idleSince[key] = now
			r.mu.Unlock()
			return false, false
		}
		r.mu.Unlock()
		if now.Sub(since) >= time.Duration(run.Session.IdleTimeoutMS)*time.Millisecond {
			r.finish(ctx, run, StatusTimedOut, EndIdleTimeout)
			return true, false
		}
		return false, false
	}
	r.mu.Lock()
	delete(r.idleSince, "cli-idle:"+run.ID)
	r.mu.Unlock()
	return false, keepAlive
}

// authFailed reports coarse login-required markers on the screen.
func authFailed(screen string) bool {
	low := strings.ToLower(screen)
	return strings.Contains(low, "please login") || strings.Contains(screen, "Not logged in") ||
		strings.Contains(low, "please run /login")
}

func (r *Run) screenIdle(screen string) bool {
	if r != nil {
		if tool, ok := tmux.ByName(r.Harness); ok {
			if c, ok := tool.(tmux.ScreenClassifier); ok {
				switch c.ClassifyScreen(screen) {
				case tmux.ScreenBusy:
					return false
				case tmux.ScreenIdle:
					return true
				}
			}
		}
	}
	return isScreenIdle(screen)
}

func isScreenIdle(screen string) bool {
	low := strings.ToLower(screen)
	busy := []string{"esc to interrupt", "thinking", "running…", "running...", "crafting"}
	for _, b := range busy {
		if strings.Contains(low, b) {
			return false
		}
	}
	return true
}
