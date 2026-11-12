package runs

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/harness"
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
	lastPane    map[string]string
	// turns is the run turn the per-run maps describe.
	turns map[string]int
	stop  chan struct{}
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
		lastPane:    make(map[string]string),
		turns:       make(map[string]int),
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
	sessions, listOK := r.livePanes(ctx)
	now := r.now()
	for _, run := range list {
		r.checkOne(ctx, run, now, sessions, listOK)
	}
}

func (r *Reaper) livePanes(ctx context.Context) (map[string]struct{}, bool) {
	ss, err := r.svc.panes.List(ctx, nil)
	if err != nil {
		return nil, false
	}
	set := map[string]struct{}{}
	for _, s := range ss {
		set[s.ID] = struct{}{}
	}
	return set, true
}

func (r *Reaper) checkOne(ctx context.Context, run *Run, now time.Time, sessions map[string]struct{}, listOK bool) {
	r.mu.Lock()
	if r.turns[run.ID] != run.TurnCount {
		// A new turn starts with clean windows and counts.
		r.forgetLocked(run.ID)
		r.turns[run.ID] = run.TurnCount
	}
	_, keeping := r.idleSince["keep:"+run.ID]
	r.mu.Unlock()
	turnStart := run.turnStart()
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
			if now.Sub(turnStart) >= time.Duration(run.Session.MaxDurationMS)*time.Millisecond {
				r.finish(ctx, run, StatusTimedOut, EndMaxDuration)
			}
		}
		return
	}

	if !keeping && run.Session.MaxDurationMS > 0 {
		if now.Sub(turnStart) >= time.Duration(run.Session.MaxDurationMS)*time.Millisecond {
			r.finish(ctx, run, StatusTimedOut, EndMaxDuration)
			return
		}
	}

	// No pane (fake already terminal or deferred without session).
	// Native runs have no pane; max-duration already ran above.
	if run.PaneSession == "" {
		return
	}
	if listOK {
		if _, ok := sessions[run.PaneSession]; !ok {
			r.finish(ctx, run, StatusExited, EndSessionGone)
			return
		}
	} else if !r.svc.panes.Alive(ctx, run.PaneSession) {
		r.finish(ctx, run, StatusExited, EndSessionGone)
		return
	}

	screen, err := r.svc.panes.Screen(ctx, run.PaneSession)
	if err != nil {
		return
	}

	r.capturePaneLog(run, screen)

	if tool, ok := agentcli.ByName(run.Harness); ok && !r.svc.IsLaunching(run.ID) {
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
	if hist, herr := r.svc.panes.History(ctx, run.PaneSession); herr == nil && hist != "" {
		completeText = hist
	}
	if reason, ok := ReportedFailure(completeText, run.sentinelKey()); ok && kindOrDefault(run.Kind) == KindJob {
		r.svc.reportFailure(ctx, run.ID, "reaper", reason)
		r.finish(ctx, run, StatusFailed, EndFailed)
		return
	}
	if DetectCompletion(completeText, run.sentinelKey()) {
		r.gateCompletion(ctx, run, completeText, now)
		return
	}

	if r.checkIdle(ctx, run, screen, now) {
		return
	}

	// auth / limit chrome (coarse markers)
	if agentcli.IsAuthBlocker(screen) || authFailed(screen) {
		r.svc.emitAuthProbeFailed(ctx, run.Harness, run.ID, harness.SourceReaper)
		r.finish(ctx, run, StatusFailed, EndAuth)
		return
	}
	if agentcli.IsLimitBlocker(screen) {
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

func (r *Reaper) handleDialogs(ctx context.Context, run *Run, tool agentcli.Tool, screen string) {
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
		key := agentcli.DialogKey(screen, act)
		r.mu.Lock()
		n := hits[key]
		r.mu.Unlock()
		if n >= agentcli.MaxDialogTries {
			continue
		}
		if err := agentcli.SendAction(ctx, r.svc.panes.Pane(run.PaneSession), act); err != nil {
			r.log.Warn("dialog send", "run", run.ID, "error", err)
			continue
		}
		r.mu.Lock()
		hits[key]++
		r.mu.Unlock()
		fired++
	}

	if len(acts) > 0 && fired == 0 {
		q, _ := agentcli.ChoiceDialog(screen)
		r.markDialogWaiting(ctx, run, q)
	} else if len(acts) == 0 {
		if q, ok := agentcli.ChoiceDialog(screen); ok {
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
	if err := r.svc.sendPrompt(ctx, p.session, p.prompt, p.harness); err != nil {
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
	n := countSentinels(screen, run.sentinelKey())
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
		r.svc.recordMemoryValid(ctx, run)
		r.finishComplete(ctx, run, now)
		return
	}
	if attempts >= maxMemoryFixes {
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
	if err := r.svc.panes.Send(ctx, run.PaneSession, fixMessage(issues, run.sentinelKey())); err != nil {
		r.log.Warn("memory fix send", "run", run.ID, "error", err)
		return
	}
	time.Sleep(300 * time.Millisecond)
	_ = r.svc.panes.Key(ctx, run.PaneSession, "enter")
}

// finishComplete ends a completed run; keep_alive lets the session linger first.
func (r *Reaper) finishComplete(ctx context.Context, run *Run, now time.Time) {
	if run.Session.KeepAliveMS <= 0 || r.svc.mustPark(ctx, run.ID) {
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
	if r.svc.SessionState(run.ID) == SessionWaiting {
		return false
	}
	// An agent that stopped without the sentinel but has an open request
	// parks at once; it does not wait for the idle timeout.
	if run.screenIdle(screen) && r.svc.mustPark(ctx, run.ID) {
		r.finish(ctx, run, StatusParked, r.svc.parkEnd(ctx, run.ID))
		return true
	}
	if run.Session.IdleTimeoutMS <= 0 {
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
	usage := r.svc.teardownRuntime(ctx, run)
	if err := r.svc.TerminateTurn(ctx, run.ID, run.TurnCount, status, end, usage); err != nil {
		r.log.Warn("reaper terminate", "run", run.ID, "error", err)
	}
	r.svc.takePendingPrompt(run.ID)
	r.mu.Lock()
	r.forgetLocked(run.ID)
	delete(r.turns, run.ID)
	r.mu.Unlock()
	// Terminate records the finish ("run finished"); nothing to add here.
}

// forgetLocked drops the per-turn state of a run. The caller holds r.mu.
func (r *Reaper) forgetLocked(id string) {
	delete(r.idleSince, id)
	delete(r.idleSince, "keep:"+id)
	delete(r.idleSince, "cli-idle:"+id)
	delete(r.limitHits, id)
	delete(r.dialogHits, id)
	delete(r.sentinels, id)
	delete(r.fixAttempts, id)
	delete(r.lastPane, id)
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
		if now.Sub(run.turnStart()) < cliLaunchGrace {
			return false, false
		}
		// The pump unregisters after the terminal transition; a live row with
		// no session means the child is gone.
		r.finish(ctx, run, StatusExited, EndSessionGone)
		return true, false
	}
	snap := cr.tracker.Snapshot()
	switch snap.State {
	case agentwire.StatusWaiting:
		r.svc.SetSessionState(ctx, run.ID, SessionWaiting, "wire")
	case agentwire.StatusRunning:
		r.svc.SetSessionState(ctx, run.ID, SessionRunning, "wire")
	case agentwire.StatusIdle:
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
	if run.Session.KeepAliveMS > 0 && snap.LastResult != nil && snap.State == agentwire.StatusIdle {
		if cr.keepAliveExpired(run.Session.KeepAliveMS, now) || r.svc.mustPark(ctx, run.ID) {
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
	if run.Session.IdleTimeoutMS > 0 && snap.State == agentwire.StatusIdle && !keepAlive {
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

// turnStart is the start of the current turn.
func (r *Run) turnStart() time.Time {
	if r.TurnStartedAt != nil {
		return *r.TurnStartedAt
	}
	return r.StartedAt
}

func (r *Run) screenIdle(screen string) bool {
	if r != nil {
		if tool, ok := agentcli.ByName(r.Harness); ok {
			if c, ok := tool.(agentcli.ScreenClassifier); ok {
				switch c.ClassifyScreen(screen) {
				case agentcli.ScreenBusy:
					return false
				case agentcli.ScreenIdle:
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
