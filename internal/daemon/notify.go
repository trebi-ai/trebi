package daemon

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/runs"
)

// notifyLayers reads the notify layers for the route resolver: job and agent
// files, trebi.yaml, and the global config. The global layer changes when the
// daemon reloads the config.
type notifyLayers struct {
	c *ControlPlane

	mu     sync.RWMutex
	route  []notify.Step
	admins []string

	// cfgMod and cfgSize identify the config.yaml of the last load.
	cfgMod  time.Time
	cfgSize int64
}

var _ notify.Layers = (*notifyLayers)(nil)

func (l *notifyLayers) JobLayer(ctx context.Context, ws, name string) ([]notify.Step, bool, []string, string) {
	// An agent run names the agent itself; it has no owner.
	j, _ := l.c.findJob(ctx, ws, name)
	if j == nil {
		return nil, false, nil, ""
	}
	return j.Notify, j.NotifySet, j.Contact, j.Owner
}

func (l *notifyLayers) AgentLayer(ctx context.Context, ws, name string) ([]notify.Step, bool, []string) {
	j, _ := l.c.findJobKind(ctx, ws, name, runs.KindAgent)
	if j == nil {
		return nil, false, nil
	}
	return j.Notify, j.NotifySet, j.Contact
}

func (l *notifyLayers) WorkspaceLayer(ctx context.Context, id string) ([]notify.Step, bool, []string) {
	w, err := l.c.Workspaces.Get(ctx, id)
	if err != nil || w == nil {
		return nil, false, nil
	}
	route, set, contact, err := l.c.Workspaces.Route(w)
	if err != nil {
		l.c.log.Warn("notify workspace layer", "workspace", id, "error", err)
		return nil, false, nil
	}
	return route, set, contact
}

// Global re-reads config.yaml when its mod time or size changes, so a config
// PATCH changes the route without a restart. A read error keeps the last good
// values.
func (l *notifyLayers) Global() ([]notify.Step, bool, []string) {
	fi, statErr := os.Stat(home.ConfigPath())
	l.mu.RLock()
	same := statErr == nil && fi.ModTime().Equal(l.cfgMod) && fi.Size() == l.cfgSize
	l.mu.RUnlock()
	if !same {
		if cfg, err := config.Load(); err == nil {
			l.setGlobal(cfg)
			if statErr == nil {
				l.mu.Lock()
				l.cfgMod, l.cfgSize = fi.ModTime(), fi.Size()
				l.mu.Unlock()
			}
		}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.route, l.route != nil, l.admins
}

// setGlobal takes notify.route and email.admins from cfg.
func (l *notifyLayers) setGlobal(cfg *config.Config) {
	if cfg == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.route, l.admins = cfg.Notify.Route, cfg.Email.Admins
}

// connectorChannels tells the resolver which connectors can send.
type connectorChannels struct{ conn *connectors.Service }

func (c connectorChannels) ChannelProblem(name, workspaceID string) string {
	if _, err := c.conn.Lookup(name); err != nil {
		return notify.ProblemNoConn
	}
	if !c.conn.IsChannel(name) {
		return notify.ProblemNoChan
	}
	if errors.Is(c.conn.HiddenIn(name, workspaceID), connectors.ErrNotInScope) {
		return notify.ProblemScope
	}
	if id := c.conn.ConnectionFor(name, workspaceID); id == "" || c.conn.Down(id) {
		return notify.ProblemDown
	}
	return ""
}

// workspacePicker picks the system sender workspace for email.
type workspacePicker struct {
	c *ControlPlane

	mu  sync.RWMutex
	def string
}

var _ email.WorkspacePicker = (*workspacePicker)(nil)

// Default resolves defaults.workspace (an id or a name) to an id.
func (p *workspacePicker) Default() string {
	p.mu.RLock()
	def := p.def
	p.mu.RUnlock()
	if def == "" {
		return ""
	}
	w, err := p.c.Workspaces.Resolve(context.Background(), def)
	if err != nil || w == nil {
		return ""
	}
	return w.ID
}

// MostActive is the workspace with the most active jobs; the oldest wins a tie.
func (p *workspacePicker) MostActive(ctx context.Context) string {
	var id string
	err := p.c.Store.DB().QueryRowContext(ctx, `SELECT j.workspace_id FROM job_index j
		JOIN workspaces w ON w.id=j.workspace_id
		WHERE j.kind='job' AND j.active=1 AND j.retired_at IS NULL AND w.orphaned=0
		GROUP BY j.workspace_id, w.created_at ORDER BY COUNT(*) DESC, w.created_at ASC LIMIT 1`).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

// setDefault takes defaults.workspace from cfg.
func (p *workspacePicker) setDefault(cfg *config.Config) {
	if cfg == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.def = cfg.Defaults.Workspace
}
