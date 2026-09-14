package runs

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// Reaper watches live runs for sentinel, idle timeout, max duration, and gone sessions.
type Reaper struct {
	svc      *Service
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger

	mu        sync.Mutex
	idleSince map[string]time.Time
	limitHits map[string]int
	stop      chan struct{}
}

// NewReaper builds a reaper bound to the runs service.
func NewReaper(svc *Service) *Reaper {
	return &Reaper{
		svc:       svc,
		interval:  5 * time.Second,
		now:       time.Now,
		log:       slog.Default().With("source", "reaper"),
		idleSince: make(map[string]time.Time),
		limitHits: make(map[string]int),
		stop:      make(chan struct{}),
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

	idle := run.screenIdle(screen)
	if r.svc.SessionState(run.ID) != SessionWaiting {
		if idle {
			r.svc.SetSessionState(ctx, run.ID, SessionIdle, "screen")
		} else {
			r.svc.SetSessionState(ctx, run.ID, SessionRunning, "screen")
		}
	}

	if DetectCompletion(screen, run.ID) {
		r.finishComplete(ctx, run, now)
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

func (r *Reaper) finish(ctx context.Context, run *Run, status Status, end EndReason) {
	r.svc.cancelNative(run.ID)
	if run.TmuxSession != "" {
		_ = tmux.SendKey(run.TmuxSession, "C-c")
		time.Sleep(100 * time.Millisecond)
		r.svc.closeSession(run.ID, run.TmuxSession)
	}
	if err := r.svc.Terminate(ctx, run.ID, status, end, nil); err != nil {
		r.log.Warn("reaper terminate", "run", run.ID, "error", err)
	}
	r.mu.Lock()
	delete(r.idleSince, run.ID)
	delete(r.idleSince, "keep:"+run.ID)
	delete(r.limitHits, run.ID)
	r.mu.Unlock()
	r.log.Info("reaper finished run", "id", run.ID, "status", status, "end_reason", end)
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
