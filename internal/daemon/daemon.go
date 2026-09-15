// Package daemon owns long-running trebid workers.
package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tmux"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Options for the control plane.
type Options struct {
	Shadow bool // no connectors / no schedule fire / no queue drain of auto producers
	Drain  bool
	// ScheduleInterval overrides the schedule ticker (default 15s). Tests may set 50ms.
	ScheduleInterval time.Duration
	// QueueInterval overrides the queue worker tick (default 2s).
	QueueInterval time.Duration
	// AlertsInterval overrides the AlertService poll tick (default 2s).
	AlertsInterval time.Duration
	// TurnWatchInterval overrides the transcript watcher tick (default 2s).
	TurnWatchInterval time.Duration
	// AuthProbeInterval is how often blocked harnesses are re-probed (default 5m; tests 50ms).
	AuthProbeInterval time.Duration
	// LoginMaxAge is the interactive login session lifetime (default 10m).
	LoginMaxAge time.Duration
}

// ControlPlane wires workers to services.
type ControlPlane struct {
	Store         *store.Store
	Runs          *runs.Service
	Workspaces    *workspaces.Service
	Files         *files.Service
	Schedule      *schedule.Service
	Connectors    *connectors.Service
	Policy        *dispatch.Policy
	Reaper        *runs.Reaper
	Alerts        *alerts.Service
	Conversations *conversations.Service
	Harness       *harness.Service
	Opts          Options

	log   *slog.Logger
	queue *dispatch.Queue

	cancel context.CancelFunc
	wg     sync.WaitGroup

	turnMu     sync.Mutex
	turnCursor map[string]transcriptCursor

	jobCacheMu sync.Mutex
	jobCache   map[string]parsedJob

	tickCfg *config.Config
}

type parsedJob struct {
	hash string
	job  *job.Job
	path string
}

// New builds a control plane.
func New(st *store.Store, opts Options) *ControlPlane {
	ws := workspaces.New(st)
	cfg, _ := config.Load()
	maxBytes := files.DefaultMaxBytes
	if cfg != nil {
		maxBytes = cfg.Files.MaxBytesOrDefault()
		ws.WithRoot(cfg.Workspaces.RootOrDefault())
	}
	fs := files.New(maxBytes)
	r := runs.New(st, ws, fs)
	r.SetMemoryResolver(ws)
	q := dispatch.NewQueue(st)
	conv := conversations.New(st).WithRuns(r).WithQueue(q).WithWorkspaces(ws)
	pol := dispatch.NewPolicy()
	connector := connectors.New(st, ws, r, pol).WithUserHome(harness.UserHome())
	return &ControlPlane{
		Store:         st,
		Runs:          r,
		Workspaces:    ws,
		Files:         fs,
		Schedule:      schedule.New(st),
		Connectors:    connector,
		Policy:        pol,
		Reaper:        runs.NewReaper(r),
		Alerts:        newAlerts(st, opts, cfg),
		Conversations: conv,
		Harness:       harness.New(st, tmux.DefaultDriver{}, harness.Options{LoginMaxAge: opts.LoginMaxAge}),
		Opts:          opts,
		log:           slog.Default().With("source", "daemon"),
		queue:         q,
	}
}

func newAlerts(st *store.Store, opts Options, cfg *config.Config) *alerts.Service {
	aopts := alerts.Options{
		TickInterval: opts.AlertsInterval,
		Shadow:       opts.Shadow,
		Doctor:       alerts.CollectDoctorFindings,
	}
	if cfg != nil {
		if d, ok := cfg.Notify.WaitingAfterDuration(); ok {
			aopts.WaitingAfter = d
		} else {
			aopts.WaitingDisabled = true
		}
		aopts.UsageWarnAt = cfg.Notify.UsageWarnPercent()
		for _, ch := range cfg.Notify.Channels() {
			aopts.Notify = append(aopts.Notify, alerts.NotifyChannel{Name: ch.Name, Command: ch.Command})
		}
	}
	return alerts.New(st, aopts)
}

// Start launches background workers.
func (c *ControlPlane) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.Runs.SetNativeHooks(runs.NativeHooks{
		Lifetime: ctx,
		Ingest: func(ctx context.Context, runID, convID string, texts []string) error {
			if c.Conversations == nil {
				return nil
			}
			turns := make([]conversations.IncomingTurn, len(texts))
			now := time.Now()
			for i, t := range texts {
				turns[i] = conversations.IncomingTurn{Role: conversations.RoleAssistant, Text: t, At: now}
			}
			_, err := c.Conversations.IngestAssistantTurns(ctx, runID, convID, turns)
			return err
		},
		Recap: func(ctx context.Context, convID, text string) error {
			if c.Conversations == nil {
				return nil
			}
			return c.Conversations.AppendSystem(ctx, convID, text)
		},
	})
	c.Runs.RecoverNative(ctx)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.Reaper.Start(ctx)
	}()

	if !c.Opts.Shadow {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.queueWorker(ctx)
		}()
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.scheduleTicker(ctx)
		}()
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			_ = c.Connectors.Load()
			c.Connectors.StartAll(ctx)
			<-ctx.Done()
		}()
	}

	// Alerts keep running in shadow: consuming the bus and writing inbox is
	// observation. Outbound notify is suppressed inside the service when
	// Shadow or Drain is set.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.Alerts.Run(ctx)
	}()

	// Turn watcher is a consumer: persist assistant replies even under
	// shadow and drain so chat history is complete.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.turnWatch(ctx)
	}()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.watchWorkspaces(ctx)
	}()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.busPrune(ctx)
	}()

	if c.Harness != nil {
		c.Harness.ReapOrphanLogins()
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.authProbe(ctx)
		}()
	}
}

// Stop cancels workers and waits.
func (c *ControlPlane) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.Reaper.Stop()
	if c.Harness != nil {
		c.Harness.StopLoginWatchers()
	}
	c.wg.Wait()
}

// draining reports the persisted drain flag. Shadow is a process-lifetime
// mode (workers never start); drain is a runtime flag that undrain clears.
func (c *ControlPlane) draining(_ context.Context) bool {
	return c.Store.IsDraining()
}

// findJob resolves a job by name in a workspace. It returns the parsed job
// (nil when absent or unparsable) and its file path.
func (c *ControlPlane) findJob(ctx context.Context, ws, name string) (*job.Job, string) {
	return c.findJobKind(ctx, ws, name, "")
}

// findJobKind matches name and, when kind is set, job_index.kind.
func (c *ControlPlane) findJobKind(ctx context.Context, ws, name, kind string) (*job.Job, string) {
	jobs, _ := c.Workspaces.ListJobs(ctx, ws)
	for _, row := range jobs {
		if row.Name != name {
			continue
		}
		if kind != "" && row.Kind != kind {
			continue
		}
		key := row.WorkspaceID + "\x00" + row.Name + "\x00" + row.Kind
		c.jobCacheMu.Lock()
		if c.jobCache == nil {
			c.jobCache = map[string]parsedJob{}
		}
		if hit, ok := c.jobCache[key]; ok && hit.hash == row.Hash && hit.path == row.Path {
			j := hit.job
			c.jobCacheMu.Unlock()
			return j, row.Path
		}
		c.jobCacheMu.Unlock()
		j, _ := job.ParseFile(row.Path)
		if j != nil {
			j.WorkspacePath = c.workspacePath(ctx, ws)
			if j.Kind == "job" {
				j.Prompt = job.SubstituteJobVars(j.Prompt, j.Dir, j.WorkspacePath)
				j.FollowupPrompt = job.SubstituteJobVars(j.FollowupPrompt, j.Dir, j.WorkspacePath)
			}
		}
		c.jobCacheMu.Lock()
		c.jobCache[key] = parsedJob{hash: row.Hash, job: j, path: row.Path}
		c.jobCacheMu.Unlock()
		return j, row.Path
	}
	return nil, ""
}

func (c *ControlPlane) workspacePath(ctx context.Context, id string) string {
	if c.Workspaces == nil || id == "" {
		return ""
	}
	w, err := c.Workspaces.Get(ctx, id)
	if err != nil {
		return ""
	}
	return w.Path
}

// memoryLaunch resolves the MEMORY.md seam for a daemon launch. Nil = off.
func (c *ControlPlane) memoryLaunch(ctx context.Context, wsID string, j *job.Job) *runs.MemoryLaunch {
	if j == nil || !j.Memory {
		return nil
	}
	ws, err := c.Workspaces.Get(ctx, wsID)
	if err != nil {
		c.log.Warn("memory workspace lookup", "workspace", wsID, "err", err)
		return nil
	}
	return runs.NewMemoryLaunch(j, ws.Path)
}

func (c *ControlPlane) busPrune(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := c.Store.PruneBus(ctx, 7*24*time.Hour)
			if err != nil {
				c.log.Warn("bus prune", "error", err)
			} else if n > 0 {
				c.log.Info("bus.pruned", "rows", n)
			}
			if _, err := c.Store.PruneIdempotent(ctx, 24*time.Hour); err != nil {
				c.log.Warn("idempotency prune", "error", err)
			}
		}
	}
}

func replaceVar(s, k, v string) string {
	return strings.ReplaceAll(s, "{{"+k+"}}", v)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func dirOf(path string) string {
	if path == "" {
		return home.Base()
	}
	return filepath.Dir(path)
}
