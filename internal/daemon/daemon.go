// Package daemon owns long-running trebid workers.
package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/integrations"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Options for the control plane.
type Options struct {
	Shadow bool // no integrations / no schedule fire / no queue drain of auto producers
	Drain  bool
	// ScheduleInterval overrides the schedule ticker (default 15s). Tests may set 50ms.
	ScheduleInterval time.Duration
	// QueueInterval overrides the queue worker tick (default 2s).
	QueueInterval time.Duration
}

// ControlPlane wires workers to services.
type ControlPlane struct {
	Store        *store.Store
	Runs         *runs.Service
	Workspaces   *workspaces.Service
	Schedule     *schedule.Service
	Integrations *integrations.Service
	Policy       *dispatch.Policy
	Reaper       *runs.Reaper
	Opts         Options

	log   *slog.Logger
	queue *dispatch.Queue

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New builds a control plane.
func New(st *store.Store, opts Options) *ControlPlane {
	ws := workspaces.New(st)
	r := runs.New(st)
	pol := dispatch.NewPolicy()
	integ := integrations.New(st, ws, r, pol)
	return &ControlPlane{
		Store:        st,
		Runs:         r,
		Workspaces:   ws,
		Schedule:     schedule.New(st),
		Integrations: integ,
		Policy:       pol,
		Reaper:       runs.NewReaper(r),
		Opts:         opts,
		log:          slog.Default().With("logger", "daemon"),
		queue:        dispatch.NewQueue(st),
	}
}

// Start launches background workers.
func (c *ControlPlane) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel

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
			_ = c.Integrations.Load()
			c.Integrations.StartAll(ctx)
			<-ctx.Done()
		}()
	}

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
}

// Stop cancels workers and waits.
func (c *ControlPlane) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.Reaper.Stop()
	c.wg.Wait()
}

// draining reports the shared drain state: the startup option or the
// store flag the API drain endpoints write.
func (c *ControlPlane) draining(_ context.Context) bool {
	if c.Opts.Drain {
		return true
	}
	v, ok, err := c.Store.GetFlag("drain")
	if err != nil {
		c.log.Warn("drain flag read", "error", err)
		return false
	}
	return ok && v == "1"
}

// findJob resolves a job by name in a workspace. It returns the parsed job
// (nil when absent or unparsable) and its file path.
func (c *ControlPlane) findJob(ctx context.Context, ws, name string) (*job.Job, string) {
	jobs, _ := c.Workspaces.ListJobs(ctx, ws)
	for _, row := range jobs {
		if row.Name == name {
			j, _ := job.ParseFile(row.Path)
			return j, row.Path
		}
	}
	return nil, ""
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
