package runs

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/tmux"
)

// Reaper watches live runs for sentinel, idle timeout, max duration, and gone sessions.
type Reaper struct {
	Svc      *Service
	Interval time.Duration
	Now      func() time.Time
	log      *slog.Logger

	mu        sync.Mutex
	idleSince map[string]time.Time
	stop      chan struct{}
}

// NewReaper builds a reaper bound to the runs service.
func NewReaper(svc *Service) *Reaper {
	return &Reaper{
		Svc:       svc,
		Interval:  5 * time.Second,
		Now:       time.Now,
		log:       slog.Default().With("logger", "reaper"),
		idleSince: make(map[string]time.Time),
		stop:      make(chan struct{}),
	}
}

// Start runs the reaper loop until ctx is done or Stop.
func (r *Reaper) Start(ctx context.Context) {
	t := time.NewTicker(r.Interval)
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
	list, err := r.Svc.List(ctx, ListOpts{Status: string(StatusRunning), Limit: 500})
	if err != nil {
		r.log.Warn("reaper list", "error", err)
		return
	}
	now := r.Now()
	for _, run := range list {
		r.checkOne(ctx, run, now)
	}
}

func (r *Reaper) checkOne(ctx context.Context, run *Run, now time.Time) {
	// max duration
	if run.Session.MaxDurationMS > 0 {
		if now.Sub(run.StartedAt) >= time.Duration(run.Session.MaxDurationMS)*time.Millisecond {
			r.finish(ctx, run, StatusTimedOut, EndMaxDuration)
			return
		}
	}

	// no tmux (fake already terminal or deferred without session)
	if run.TmuxSession == "" {
		return
	}
	if !tmux.HasSession(run.TmuxSession) {
		r.finish(ctx, run, StatusExited, EndSessionGone)
		return
	}

	screen, err := tmux.ReadScreen(run.TmuxSession)
	if err != nil {
		return
	}

	// agent screen classification (busy/idle) when not waiting via hooks
	if r.Svc.AgentState(run.ID) != AgentWaiting {
		if isScreenIdle(screen) {
			r.Svc.SetAgentState(ctx, run.ID, AgentIdle, "screen")
		} else {
			r.Svc.SetAgentState(ctx, run.ID, AgentRunning, "screen")
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
	if authFailed(screen) {
		r.finish(ctx, run, StatusFailed, EndAuth)
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
		r.Svc.AddEvent(ctx, run.ID, "sentinel", "reaper", map[string]string{"keep_alive": "true"})
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
	if run.Session.IdleTimeoutMS <= 0 || r.Svc.AgentState(run.ID) == AgentWaiting {
		return false
	}
	if !isScreenIdle(screen) {
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
	if run.TmuxSession != "" {
		_ = tmux.SendKey(run.TmuxSession, "C-c")
		time.Sleep(100 * time.Millisecond)
		_ = tmux.KillSession(run.TmuxSession)
	}
	if err := r.Svc.Terminate(ctx, run.ID, status, end, nil); err != nil {
		r.log.Warn("reaper terminate", "run", run.ID, "error", err)
	}
	r.mu.Lock()
	delete(r.idleSince, run.ID)
	delete(r.idleSince, "keep:"+run.ID)
	r.mu.Unlock()
	r.log.Info("reaper finished run", "id", run.ID, "status", status, "end_reason", end)
}

// authFailed reports coarse login-required markers on the screen.
func authFailed(screen string) bool {
	low := strings.ToLower(screen)
	return strings.Contains(low, "please login") || strings.Contains(screen, "Not logged in") ||
		strings.Contains(low, "please run /login")
}

func isScreenIdle(screen string) bool {
	// Coarse: no "esc to interrupt" / spinner-like busy markers.
	low := strings.ToLower(screen)
	busy := []string{"esc to interrupt", "thinking", "running…", "running...", "crafting", "working"}
	for _, b := range busy {
		if strings.Contains(low, b) {
			return false
		}
	}
	return true
}
