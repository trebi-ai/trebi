// Package daemon owns long-running trebid workers.
package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/ext"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/channels"
	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/license"
	"github.com/trebi-ai/trebi/internal/proposals"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tmux"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/webhooks"
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
	// EmailInterval overrides the inbound mail poll tick (default: the
	// configured poll_interval, else 30s; tests 50ms).
	EmailInterval time.Duration
	// ChannelInterval overrides the channel outbox and typing tick (default 1s).
	ChannelInterval time.Duration
	// LoginMaxAge is the interactive login session lifetime (default 10m).
	LoginMaxAge time.Duration
	Ext         ext.Set
	// License is the process-wide service built in service daemon. Nil
	// builds a new one from Ext.License so tests can omit it.
	License *license.Service
}

// ControlPlane wires workers to services.
type ControlPlane struct {
	Store         *store.Store
	Runs          *runs.Service
	Workspaces    *workspaces.Service
	Files         *files.Service
	Skills        *skills.Service
	Schedule      *schedule.Service
	Connectors    *connectors.Service
	Policy        *dispatch.Policy
	Reaper        *runs.Reaper
	Alerts        *alerts.Service
	Conversations *conversations.Service
	Channels      *channels.Service
	Harness       *harness.Service
	Email         *email.Service
	Proposals     *proposals.Service
	Cloud         *cloudlink.Service
	Webhooks      *webhooks.Service
	License       *license.Service
	Opts          Options

	// The assistant session stack (plan D). The API server binds these
	// through BindAssistant, so /v1 and the workers drive one set of
	// sessions.
	asst        *assistant.Manager
	asstStore   *assistant.Store
	asstResolve *assistant.Resolver
	asstDoctor  *assistant.Doctor
	asstRT      *agentwire.Runtime
	asstDisp    *toolDispatcher

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

// Assistant is the shared session manager.
func (c *ControlPlane) Assistant() *assistant.Manager { return c.asst }

// AssistantStore is the session and event store.
func (c *ControlPlane) AssistantStore() *assistant.Store { return c.asstStore }

// AssistantResolver merges settings defaults and workspace overrides.
func (c *ControlPlane) AssistantResolver() *assistant.Resolver { return c.asstResolve }

// AssistantDoctor reports assistant findings.
func (c *ControlPlane) AssistantDoctor() *assistant.Doctor { return c.asstDoctor }

// AssistantRuntime exposes the agent-wire runtime; tests inject the fake
// driver through SetDriver.
func (c *ControlPlane) AssistantRuntime() *agentwire.Runtime { return c.asstRT }

// SetToolDispatcher points the native session driver at the API server, so
// its tool calls reach the same /v1 surface.
func (c *ControlPlane) SetToolDispatcher(d tools.Dispatcher) {
	c.asstDisp.set(d)
}

// toolDispatcher is the late-bound dispatcher for the native loop. The
// daemon builds the sessions before the HTTP server exists.
type toolDispatcher struct {
	mu sync.Mutex
	d  tools.Dispatcher
}

func (t *toolDispatcher) set(d tools.Dispatcher) {
	t.mu.Lock()
	t.d = d
	t.mu.Unlock()
}

func (t *toolDispatcher) Dispatch(ctx context.Context, req request.HTTPRequest) (int, []byte, error) {
	t.mu.Lock()
	d := t.d
	t.mu.Unlock()
	if d == nil {
		return 0, nil, errors.New("no dispatcher bound")
	}
	return d.Dispatch(ctx, req)
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
	r.SetBudgetGate(ws)
	q := dispatch.NewQueue(st)
	conv := conversations.New(st).WithRuns(r).WithQueue(q).WithWorkspaces(ws)
	pol := dispatch.NewPolicy()
	sk := skills.New(st)
	connector := connectors.New(st, ws, sk).WithUserHome(harness.UserHome())
	r.WithActions(connector)
	lic := opts.License
	if lic == nil {
		lic = license.New(opts.Ext.License)
	}
	em := email.New(st).WithConversations(conv)
	ch := channels.New(st, connector, conv)
	cl := cloudlink.New(st).WithMail(em)
	hk := webhooks.New(st, connector, cl)
	cl.WithWebhooks(hk)
	proposalSvc := proposals.NewService(st, fs, ws, st, r)
	c := &ControlPlane{
		Store:         st,
		Runs:          r,
		Workspaces:    ws,
		Files:         fs,
		Proposals:     proposalSvc,
		Skills:        sk,
		Schedule:      schedule.New(st),
		Connectors:    connector,
		Policy:        pol,
		Reaper:        runs.NewReaper(r),
		Conversations: conv,
		Channels:      ch,
		Harness:       harness.New(st, tmux.DefaultDriver{}, harness.Options{LoginMaxAge: opts.LoginMaxAge}),
		Email:         em,
		Cloud:         cl,
		Webhooks:      hk,
		License:       lic,
		Opts:          opts,
		log:           slog.Default().With("source", "daemon"),
		queue:         q,
	}
	cl.WithOnChange(c.reselectEmail)
	approvalQuestion := func(ctx context.Context, runID string) string {
		a, err := r.PendingApproval(ctx, runID)
		if err != nil || a == nil {
			return ""
		}
		return a.Prompt
	}
	c.Alerts = newAlerts(st, opts, cfg, em, c.jobNotify, approvalQuestion)
	c.Alerts.SetDoctor(c.Alerts.CollectDoctorFindings)
	c.Alerts.SetProposalContent(func(ctx context.Context, id string) (string, string, bool) {
		p, err := c.Proposals.Get(ctx, id)
		if err != nil {
			return "", "", false
		}
		return p.BaseContent, p.Proposed, true
	})
	c.Alerts.SetFailureRouter(c)
	ws.SetBudgetAlerter(c)
	c.buildAssistant(cfg, ws, conv, maxBytes)
	c.configureCloud(cfg)
	c.configureEmail(cfg)
	return c
}

// buildAssistant wires the assistant session stack (plan D): store,
// resolver, agent-wire runtime with the native driver, and the manager. The
// conversations service prompts through the manager.
func (c *ControlPlane) buildAssistant(cfg *config.Config, ws *workspaces.Service, conv *conversations.Service, maxFileBytes int64) {
	timing := assistant.ResolverTiming{}
	if cfg != nil {
		timing = assistant.ResolverTiming{
			IdleClose:         cfg.Assistant.IdleCloseDuration(),
			PermissionTimeout: cfg.Assistant.PermissionTimeoutDuration(),
			MaxLive:           cfg.Assistant.MaxLiveOrDefault(),
		}
	}
	// The provider config is read live, so a save lands without a restart
	// (plan 2026-09-26 G). LiveNativeConfig merges the native model with the
	// assistant.model fallback.
	nativeFn := assistant.LiveNativeConfig
	c.asstDisp = &toolDispatcher{}
	c.asstStore = assistant.NewStore(c.Store)
	c.asstRT = agentwire.New(agentwire.Options{
		ClientName: "trebi",
		LedgerPath: filepath.Join(home.RunsDir(), "agent-procs.json"),
		Logger:     c.log,
	})
	assistant.RegisterNativeDriver(c.asstRT, nativeFn, c.asstDisp, c.asstStore, maxFileBytes, c.log)
	c.asstResolve = assistant.NewResolver(freshConfigSource{}, ws, c.asstRT, nativeFn, timing)
	c.asstDoctor = &assistant.Doctor{Res: c.asstResolve, Workspaces: func(ctx context.Context) ([]assistant.WorkspaceInfo, error) {
		list, err := ws.List(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]assistant.WorkspaceInfo, 0, len(list))
		for _, w := range list {
			out = append(out, assistant.WorkspaceInfo{ID: w.ID, Name: w.Name, Path: w.Path})
		}
		return out, nil
	}}
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	c.asst = assistant.NewManager(c.asstRT, c.asstStore, conv, c.asstResolve, ws, wsMemory{ws}, c,
		assistant.Options{
			MCPURL:       "http://127.0.0.1:" + c.mcpPort(cfg) + "/mcp",
			TrebiExe:     exe,
			SkillsDir:    home.SkillsDir(),
			MaxFileBytes: maxFileBytes,
			Log:          c.log,
		})
	conv.WithAssistant(c.asst)
	c.asst.OnTurnEnd(c.onAssistantTurnEnd)
}

// mcpPort renders the configured API port for the MCP URL.
func (c *ControlPlane) mcpPort(cfg *config.Config) string {
	if cfg != nil {
		if _, p, err := net.SplitHostPort(cfg.API.ListenAddr()); err == nil {
			return p
		}
	}
	return "7433"
}

// freshConfigSource re-reads config.yaml per call, so settings writes land
// in the resolver defaults without a restart.
type freshConfigSource struct{}

func (freshConfigSource) AssistantDefaults() assistant.Settings {
	c, err := config.Load()
	if err != nil || c == nil {
		return assistant.Settings{Accept: []string{"operators", "allowlist"}}
	}
	return assistant.SettingsFromConfig(c)
}

// wsMemory adapts the workspace memory render to the manager seam.
type wsMemory struct{ ws *workspaces.Service }

func (a wsMemory) Render(ctx context.Context, workspaceID string) (string, error) {
	return a.ws.AssistantMemory(ctx, workspaceID)
}

func newAlerts(st *store.Store, opts Options, cfg *config.Config, em *email.Service,
	jobNotify func(context.Context, string, string) []string,
	approvalQuestion func(context.Context, string) string) *alerts.Service {
	aopts := alerts.Options{
		TickInterval:     opts.AlertsInterval,
		Shadow:           opts.Shadow,
		Email:            em,
		JobNotify:        jobNotify,
		ApprovalQuestion: approvalQuestion,
	}
	if cfg != nil {
		if d, ok := cfg.Notify.WaitingAfterDuration(); ok {
			aopts.WaitingAfter = d
		} else {
			aopts.WaitingDisabled = true
		}
		if d, on := cfg.Notify.ApprovalAfterDuration(); on {
			aopts.ApprovalAfter = d
		} else {
			aopts.ApprovalDisabled = true
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
	c.Runs.RecoverCLI(ctx)
	c.syncEmailAddresses(ctx)

	// No assistant process survives a restart; rows left running or
	// waiting are closed (plan C.2).
	if n, err := c.asstStore.CloseStale(ctx); err != nil {
		c.log.Warn("assistant close stale", "error", err)
	} else if n > 0 {
		c.log.Info("assistant close stale", "count", n)
	}

	// The cloud socket pushes hosted mail and webhooks. It dials out; the
	// machine opens no port (plan 2026-09-22 Part N).
	c.Cloud.Start(ctx)

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

	// The inbound mail loop runs under shadow too: Run skips automatic
	// polls there, and the manual poll route still works.
	c.startEmail(ctx)

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

	// The channel worker sends pending replies and pulses typing. It skips
	// its tick under shadow and drain (plan G.3).
	if c.Channels != nil {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.channelWorker(ctx)
		}()
	}

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
	c.Cloud.Stop()
	c.Reaper.Stop()
	if c.Runs != nil {
		c.Runs.CloseCLIRuns(context.Background())
	}
	if c.Harness != nil {
		c.Harness.StopLoginWatchers()
	}
	c.wg.Wait()
}

// configureEmail maps the email config block onto the service and picks
// exactly one transport (plan 2026-09-22 Part D). A block with an
// unresolvable token or a bad poll interval leaves the channel off;
// doctor reports the cause, the daemon still boots. A Worker URL and a
// hosted link at once is a config error that doctor reports and that
// leaves the Worker path running.
func (c *ControlPlane) configureEmail(cfg *config.Config) {
	mode, _ := emailTransportMode(cfg, c.Cloud.Entitlement())
	if mode == emailOff {
		c.Email.Configure(nil, nil)
		return
	}
	interval, err := cfg.Email.PollIntervalDuration()
	if err != nil {
		c.log.Warn("email poll interval", "error", err)
		c.Email.Configure(nil, nil)
		return
	}
	settings := &email.Settings{
		Domain:       cfg.Email.Domain,
		Operators:    cfg.Email.Operators,
		PollInterval: interval,
		RequireDMARC: cfg.Email.DMARCRequired(),
		FromName:     cfg.Email.FromSenderName(),
	}
	if mode == emailHosted {
		c.Email.Configure(settings, cloudTransport{cl: c.Cloud})
		return
	}
	token, err := cfg.Email.ResolveToken()
	if err != nil {
		c.log.Warn("email token", "error", err)
		c.Email.Configure(nil, nil)
		return
	}
	settings.Worker = cfg.Email.Worker
	settings.Token = token
	c.Email.Configure(settings, email.NewClient(cfg.Email.Worker, token))
}

// syncEmailAddresses mints addresses for workspaces that lack one. With
// the channel off it is a no-op.
func (c *ControlPlane) syncEmailAddresses(ctx context.Context) {
	if !c.Email.Configured() {
		return
	}
	list, err := c.Workspaces.List(ctx)
	if err != nil {
		c.log.Warn("email sync list", "error", err)
		return
	}
	refs := make([]email.WorkspaceRef, len(list))
	for i, ws := range list {
		refs[i] = email.WorkspaceRef{ID: ws.ID, Name: ws.Name}
	}
	if err := c.Email.SyncAddresses(ctx, refs); err != nil {
		c.log.Warn("email sync", "error", err)
	}
}

// draining reports the persisted drain flag. Shadow is a process-lifetime
// mode (workers never start); drain is a runtime flag that undrain clears.
func (c *ControlPlane) draining(_ context.Context) bool {
	return c.Store.IsDraining()
}

// jobNotify resolves a job's notify: frontmatter for alert delivery. Nil
// means unset (every channel); an empty list suppresses. An absent or
// unparsable job never silences alerts.
func (c *ControlPlane) jobNotify(ctx context.Context, workspaceID, job string) []string {
	j, _ := c.findJob(ctx, workspaceID, job)
	if j == nil {
		return nil
	}
	return j.Notify
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
			// The ingress id ledger keeps one cloud_cursors row per acked
			// frame; old entries go with the same seven-day retention.
			if _, err := c.Store.DB().ExecContext(ctx,
				`DELETE FROM cloud_cursors WHERE stream LIKE 'ingress:%' AND updated_at < ?`,
				time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339)); err != nil {
				c.log.Warn("ingress ledger prune", "error", err)
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
