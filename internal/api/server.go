// Package api serves the /v1 HTTP API + SSE bus.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/ext"
	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/agents"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/artifacts"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/chains"
	"github.com/trebi-ai/trebi/internal/channels"
	"github.com/trebi-ai/trebi/internal/cloudlink"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/decisions/policies"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/launch"
	"github.com/trebi-ai/trebi/internal/machines"
	"github.com/trebi-ai/trebi/internal/mobile"
	"github.com/trebi-ai/trebi/internal/moves"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/onboarding"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/pauses"
	"github.com/trebi-ai/trebi/internal/proposals"
	"github.com/trebi-ai/trebi/internal/registry"
	"github.com/trebi-ai/trebi/internal/requests"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/state"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/ui"
	"github.com/trebi-ai/trebi/internal/uploads"
	"github.com/trebi-ai/trebi/internal/webhooks"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Version is reported on health. Stamped at build time; "dev" in a plain build.
var Version = "dev" //nolint:gochecknoglobals // -ldflags target, set once before main serves

// UIVersion is the embedded SPA version, empty when no UI is embedded.
var UIVersion = "" //nolint:gochecknoglobals // -ldflags target, set once before main serves

// Server is the trebid HTTP API.
type Server struct {
	st   *store.Store
	runs *runs.Service
	// ownRuns is the runs service that New built. The daemon closes the
	// one that it binds.
	ownRuns    *runs.Service
	ws         *workspaces.Service
	files      *files.Service
	uploads    *uploads.Store
	alerts     *alerts.Service
	skills     *skills.Service
	connectors *connectors.Service
	conv       *conversations.Service
	outbox     *outbox.Service
	router     *notify.Resolver
	harness    *harness.Service
	usage      *harness.UsagePoller
	sched      *schedule.Service
	pauses     *pauses.Service
	email      *email.Service
	cloud      *cloudlink.Service
	links      *machines.Links
	machines   *machines.Service
	moves      *moves.Service
	mobile     *mobile.Service
	rootOnce   sync.Once
	root       http.Handler
	hooks      *webhooks.Service
	templates  *agents.Catalog
	proposals  *proposals.Service
	requests   *requests.Service
	chains     *chains.Service
	artifacts  *artifacts.Service
	decisions  *decisions.Service
	launch     *launch.Launcher
	state      *state.Store
	onboarding *onboarding.Service
	// catalog is the registry client; it follows the configured list.
	catalog *registry.Live

	startedAt time.Time
	drain     bool
	shadow    bool
	// notifications is the status that the tray last reported; "" before a report.
	notifications string
	mu            sync.RWMutex
	done          chan struct{}
	doneOnce      sync.Once
	log           *slog.Logger
	queue         *dispatch.Queue

	// The doctor summary cache. A background refresher computes it every
	// minute and on a config change; the request path only reads it
	// (plan 2026-09-26 H).
	doctorMu     sync.Mutex
	doctorCached map[string]int
	doctorKick   chan struct{}

	// The assistant session stack. New builds a standalone default;
	// BindAssistant swaps in the daemon's shared instances.
	asst        *assistant.Manager
	asstStore   *assistant.Store
	asstResolve *assistant.Resolver
	asstDoctor  *assistant.Doctor
	nativeProbe *assistant.NativeProbe

	upgrader   websocket.Upgrader
	httpServer *http.Server
	listener   net.Listener
	scheme     string // http or https, set at listen

	apiMux     *http.ServeMux
	apiWrapped http.Handler
	toolsReg   *tools.Registry
	mcpHTTP    http.Handler

	ext          ext.Set
	lookupLatest func() (tag, url, dmgURL string, err error)
	latest       latestCache
}

// Options for New.
type Options struct {
	Shadow         bool
	Drain          bool
	FilesMaxBytes  int64  // 0 = files.DefaultMaxBytes
	WorkspacesRoot string // pathless workspace create; empty refuses it
	Ext            ext.Set
	// LookupLatest overrides the GitHub release lookup used by
	// /v1/system/update. Tests inject a stub so the suite never hits the network.
	LookupLatest func() (tag, url, dmgURL string, err error)
	// Registry overrides the MCP registry client used by the connector
	// catalog. Nil builds one from config.
	Registry *registry.Service
}

// New builds an API server.
func New(st *store.Store, opts Options) *Server {
	ws := workspaces.New(st).WithRoot(opts.WorkspacesRoot)
	fs := files.New(opts.FilesMaxBytes)
	r := runs.New(st, ws, fs)
	r.SetMemoryResolver(ws)
	r.SetBudgetGate(ws)
	pz := pauses.New(st)
	r.SetPauseGate(pz)
	r.SetKitSource(ws)
	q := dispatch.NewQueue(st)
	sk := skills.New(st).WithOwners(ws)
	r.WithSkills(sk)
	conn := connectors.New(st, ws, sk).WithConversations(r)
	cl := cloudlink.New(st)
	l := launch.NewLauncher(ws, r)
	stateStore := state.NewStore(st.DB(), stateLimits(), r, nil)
	r.SetStateBriefer(stateStore)
	ws.SetStateBriefer(stateStore)
	s := &Server{
		st:           st,
		runs:         r,
		ownRuns:      r,
		ws:           ws,
		files:        fs,
		uploads:      uploads.NewStore(st, home.UploadsDir(), uploadsMaxBytes()),
		alerts:       alerts.New(st, alerts.Options{Actions: r, Skills: sk}),
		skills:       sk,
		connectors:   conn,
		conv:         conversations.New(st).WithRuns(r).WithQueue(q).WithWorkspaces(ws),
		harness:      harness.New(st, agentcli.DefaultDriver{}, harness.Options{Visor: r.HarnessVisor}),
		sched:        schedule.New(st),
		pauses:       pz,
		email:        email.New(st),
		templates:    agents.NewCatalog(),
		proposals:    proposals.NewService(st, fs, ws, st, r),
		launch:       l,
		state:        stateStore,
		onboarding:   onboarding.New(st),
		requests:     requests.NewService(st, r, cl, requests.NewReportStore(home.RequestsDir()), requests.Options{}),
		catalog:      registry.NewLive(opts.Registry),
		cloud:        cl,
		hooks:        webhooks.New(st, conn, cl),
		startedAt:    time.Now().UTC(),
		shadow:       opts.Shadow,
		drain:        opts.Drain || opts.Shadow,
		done:         make(chan struct{}),
		log:          slog.Default().With("source", "http_server"),
		queue:        q,
		ext:          opts.Ext,
		lookupLatest: opts.LookupLatest,
		upgrader: websocket.Upgrader{
			CheckOrigin: checkWSOrigin,
		},
	}
	s.usage = harness.NewUsagePoller(harness.NewUsageStore(st), r, s.harness, harness.UsagePollEvery)
	s.links = machines.NewLinks(st, cl, Version)
	s.bindMachines()
	s.chains = chains.NewService(st, ws, s.conv, r, s.requests, slog.Default().With("source", "chains"))
	s.chains.WithAlerts(s.alerts)
	s.requests.WithDecider(s.chains)
	artConf, _ := config.Load()
	artCfg := artifacts.ConfigFrom(artConf)
	artBlobs := artifacts.NewBlobs(home.ArtifactBlobsDir(), artCfg.MaxBytes)
	s.artifacts = artifacts.NewService(st, artBlobs, r, ws, artCfg).
		WithContinuer(artContinuer{s}).WithSources(connectors.NewOperations(s.connectors, r, opAsker{s})).
		WithSchedule(s.sched).WithAlerts(s.alerts)
	s.email.WithArtifacts(s.artifacts)
	s.decisions = decisions.NewService(st, artBlobs, func() config.DecisionsConfig {
		if c, err := config.Load(); err == nil && c != nil {
			return c.Decisions
		}
		return config.DecisionsConfig{}
	})
	s.decisions.Register(policies.EmailReply())
	s.decisions.Register(policies.Route())
	s.connectors.WithRouteDecider(s.decisions)
	r.SetParker(runs.NewParker(r, l, s.requests, s.chains, artifacts.NewRunGate(s.artifacts, r, s.alerts), runs.NewRestartGate(r)))
	s.connectors.WithInstall(connectors.InstallConfig{Catalog: s.catalog, GitHubAPI: os.Getenv(connectors.EnvGitHubAPI), GitHubLogin: connectors.NewGHLogin()})
	_ = s.connectors.Load() // the service keeps the error; the list route and the daemon report it
	s.outbox = outbox.New(st)
	s.email.WithOutbox(s.outbox)
	s.outbox.Register(outbox.KindEmail, s.email.Outbound(), outbox.Policy{Base: 30 * time.Second, Max: 10 * time.Minute, Attempts: 5})
	s.outbox.Register(outbox.KindChannel, channels.New(st, s.outbox, s.connectors, s.conv),
		outbox.Policy{Base: 5 * time.Second, Max: 5 * time.Minute, Attempts: 5})
	s.toolsReg = tools.NewRegistry(s)
	s.buildAssistantStack(opts.FilesMaxBytes)
	s.mcpHTTP = s.newMCPHandler()
	return s
}

// buildAssistantStack wires the default assistant session manager over this
// server's own services (plan F). BindAssistant replaces every part with the
// daemon's shared instances.
func (s *Server) buildAssistantStack(filesMaxBytes int64) {
	cfg, _ := config.Load()
	// The provider config is read live, so a save lands without a restart
	// (plan 2026-09-26 G). LiveNativeConfig merges the native model with the
	// assistant.model fallback.
	nativeFn := assistant.LiveNativeConfig
	s.asstStore = assistant.NewStore(s.st)
	s.nativeProbe = assistant.NewNativeProbe(assistant.NewNativeModel)
	// The runtime runs the native harness and probes the vendor harnesses.
	// The visor of the runs service runs their sessions.
	rt := agentwire.New(agentwire.Options{ClientName: "trebi", ClientVersion: Version, Logger: s.log})
	assistant.RegisterNativeDriver(rt, nativeFn, s, s.asstStore, filesMaxBytes)
	timing := assistant.ResolverTiming{}
	if cfg != nil {
		timing = assistant.ResolverTiming{
			IdleClose:         cfg.Assistant.IdleCloseDuration(),
			PermissionTimeout: cfg.Assistant.PermissionTimeoutDuration(),
			StartTimeout:      cfg.Assistant.StartTimeoutDuration(),
			MaxLive:           cfg.Assistant.MaxLiveOrDefault(),
		}
	}
	res := assistant.NewResolver(freshConfigSource{}, s.ws, rt, nativeFn, timing)
	s.asstResolve = res
	s.asstDoctor = &assistant.Doctor{Res: res, Workspaces: s.assistantWorkspaces}
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	s.asst = assistant.NewManager(rt, s.asstStore, s.conv, res, s.ws, wsMemory{s.ws}, nil,
		assistant.Options{
			MCPURL:       s.mcpURLGuess(),
			TrebiExe:     exe,
			SkillsDir:    home.SkillsDir(),
			Skills:       s.skills,
			MaxFileBytes: filesMaxBytes,
			Host:         s.runs.SessionHost,
			Log:          s.log,
		})
	s.conv.WithAssistant(s.asst)
}

// freshConfigSource re-reads config.yaml per call, so settings writes land
// in the resolver defaults without a restart.
type freshConfigSource struct{}

func (freshConfigSource) AssistantDefaults() assistant.Settings {
	c, err := config.Load()
	if err != nil || c == nil {
		return assistant.Settings{Accept: slices.Clone(wire.DefaultAccept)}
	}
	return assistant.SettingsFromConfig(c)
}

// wsMemory adapts the workspace memory render to the manager seam.
type wsMemory struct{ ws *workspaces.Service }

func (a wsMemory) Render(ctx context.Context, workspaceID string) (string, error) {
	return a.ws.AssistantMemory(ctx, workspaceID)
}

// assistantWorkspaces adapts the workspace list for the doctor.
func (s *Server) assistantWorkspaces(ctx context.Context) ([]assistant.WorkspaceInfo, error) {
	list, err := s.ws.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]assistant.WorkspaceInfo, 0, len(list))
	for _, ws := range list {
		out = append(out, assistant.WorkspaceInfo{ID: ws.ID, Name: ws.Name, Path: ws.Path})
	}
	return out, nil
}

// mcpURLGuess renders the /mcp endpoint from config before the listener is
// bound. serve() replaces it with the bound address.
func (s *Server) mcpURLGuess() string {
	port := 7433
	if cfg, err := config.Load(); err == nil && cfg != nil {
		if _, p, err := net.SplitHostPort(cfg.API.ListenAddr()); err == nil {
			if v, err := strconv.Atoi(p); err == nil {
				port = v
			}
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
}

// AssistantSource is the daemon-built assistant stack the server binds.
type AssistantSource interface {
	Assistant() *assistant.Manager
	AssistantStore() *assistant.Store
	AssistantResolver() *assistant.Resolver
	AssistantDoctor() *assistant.Doctor
}

// BindAssistant replaces the default assistant stack with the daemon's
// shared instances, so /v1, /mcp, and the workers drive one set of sessions.
func (s *Server) BindAssistant(src AssistantSource) {
	if src == nil {
		return
	}
	if m := src.Assistant(); m != nil {
		s.asst = m
	}
	if st := src.AssistantStore(); st != nil {
		s.asstStore = st
	}
	if res := src.AssistantResolver(); res != nil {
		s.asstResolve = res
	}
	if doc := src.AssistantDoctor(); doc != nil {
		s.asstDoctor = doc
	}
	if s.conv != nil && s.asst != nil {
		s.conv.WithAssistant(s.asst)
	}
	if d, ok := src.(interface{ SetToolDispatcher(tools.Dispatcher) }); ok {
		d.SetToolDispatcher(s)
	}
	if l := s.listener; l != nil {
		s.asst.SetMCPURL(s.boundMCPURL())
	}
}

// BindControlPlane replaces core services with the daemon's shared instances.
// BindCatalog shares the registry client of the control plane.
func (s *Server) BindCatalog(c *registry.Live) {
	if c != nil {
		s.catalog = c
	}
}

func (s *Server) BindControlPlane(runsSvc *runs.Service, ws *workspaces.Service, connectors *connectors.Service, sched *schedule.Service, alertsSvc *alerts.Service, filesSvc *files.Service, skillsSvc *skills.Service, conv *conversations.Service, harnessSvc *harness.Service, emailSvc *email.Service, cloudSvc *cloudlink.Service, hooksSvc *webhooks.Service, launcher *launch.Launcher, reqSvc *requests.Service) {
	bind(&s.runs, runsSvc)
	bind(&s.ws, ws)
	bind(&s.connectors, connectors)
	if s.listener != nil && s.connectors != nil {
		s.connectors.SetLocalURL(s.localURL())
	}
	bind(&s.sched, sched)
	bind(&s.alerts, alertsSvc)
	bind(&s.files, filesSvc)
	bind(&s.skills, skillsSvc)
	bind(&s.conv, conv)
	bind(&s.harness, harnessSvc)
	bind(&s.email, emailSvc)
	if runsSvc != nil && ws != nil && filesSvc != nil {
		s.proposals = proposals.NewService(s.st, s.files, s.ws, s.st, s.runs)
	}
	if cloudSvc != nil {
		s.cloud = cloudSvc.WithMCP(s)
	}
	bind(&s.hooks, hooksSvc)
	bind(&s.launch, launcher)
	bind(&s.requests, reqSvc)
	s.bindMachines()
}

// BindUsage swaps in the daemon's plan limits poller, so a refresh and the
// poll loop share one rate limit.
func (s *Server) BindUsage(p *harness.UsagePoller) { bind(&s.usage, p) }

// BindPauses swaps in the daemon's pause service.
func (s *Server) BindPauses(p *pauses.Service) { bind(&s.pauses, p) }

// BindMobile swaps in the daemon's phone service.
func (s *Server) BindMobile(m *mobile.Service) { bind(&s.mobile, m) }

// BindState swaps in the daemon's state store.
func (s *Server) BindState(st *state.Store) { bind(&s.state, st) }

// BindChains swaps in the daemon's chains service.
func (s *Server) BindChains(c *chains.Service) { bind(&s.chains, c) }

// BindArtifacts swaps in the daemon's artifacts service and wires Ask to
// this server.
func (s *Server) BindArtifacts(a *artifacts.Service) {
	if a != nil {
		s.artifacts = a.WithContinuer(artContinuer{s})
	}
}

// uploadsMaxBytes reads uploads.max_bytes from config. 0 is the default.
func uploadsMaxBytes() int64 {
	if c, err := config.Load(); err == nil && c != nil {
		return c.Uploads.MaxBytes
	}
	return 0
}

// stateLimits reads the owner limits from config.
func stateLimits() state.Limits {
	limits := state.DefaultLimits()
	if c, err := config.Load(); err == nil && c != nil {
		d := c.Defaults.State.Resolved()
		limits.OwnerRows, limits.OwnerBytes = d.MaxRows, d.MaxBytes
	}
	return limits
}

// bind keeps the current service when the shared one is nil.
func bind[T any](dst **T, v *T) {
	if v != nil {
		*dst = v
	}
}

func checkWSOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return true
	}
	// Same-origin: the SPA served by this daemon passes even on a
	// non-loopback bind (0.0.0.0 or a LAN/Tailscale address), so remote
	// browsers can attach. Cross-site origins still fail.
	reqHost := r.Host
	if host, _, err := net.SplitHostPort(reqHost); err == nil {
		reqHost = host
	}
	return strings.EqualFold(h, reqHost)
}

// Runs returns the runs service.
func (s *Server) Runs() *runs.Service { return s.runs }

// Workspaces returns the workspace service.
func (s *Server) Workspaces() *workspaces.Service { return s.ws }

// SetCLIDriver installs a subprocess driver override on the runs service
// (tests; production uses the built-in per-harness drivers).
func (s *Server) SetCLIDriver(d agentwire.Driver) { s.runs.SetCLIDriver(d) }

// Alerts returns the alert service (tests).
func (s *Server) Alerts() *alerts.Service { return s.alerts }

// Skills returns the skills service.
func (s *Server) Skills() *skills.Service { return s.skills }

// Assistant returns the session manager.
func (s *Server) Assistant() *assistant.Manager { return s.asst }

// AssistantStore returns the assistant session store.
func (s *Server) AssistantStore() *assistant.Store { return s.asstStore }

// Email returns the email channel service.
func (s *Server) Email() *email.Service { return s.email }

// Cloud returns the hosted link service.
func (s *Server) Cloud() *cloudlink.Service { return s.cloud }

// Hooks returns the hosted webhook service.
func (s *Server) Hooks() *webhooks.Service { return s.hooks }

// Handler returns the root mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routeSystem(mux)
	s.routeWorkspaces(mux)
	s.routeAssistant(mux)
	s.routeAgents(mux)
	s.routeChains(mux)
	s.routeMemory(mux)
	s.routeFiles(mux)
	s.routeUploads(mux)
	s.routeJobs(mux)
	s.routeRuns(mux)
	s.routeProposals(mux)
	s.routeRequests(mux)
	s.routeQueue(mux)
	s.routeEvents(mux)
	s.routeInbox(mux)
	s.routeConversations(mux)
	s.routeConnectors(mux)
	s.routeRouting(mux)
	s.routeHarness(mux)
	s.routePauses(mux)
	s.routeMachines(mux)
	s.routeOnboarding(mux)
	s.routeSettings(mux)
	s.routeSkills(mux)
	s.routeEmail(mux)
	s.routeOutbox(mux)
	s.routeNotify(mux)
	s.routeHooks(mux)
	s.routeMobile(mux)
	s.routeState(mux)
	s.routeArtifacts(mux)
	s.routeDecisions(mux)
	s.routeConnectorOperations(mux)
	s.routeMCP(mux)
	s.routeLoginReturn(mux)
	// Dispatch from catalog tools re-enters here, audit-wrapped and
	// noTraversal-wrapped. MCP and native dispatch share that path.
	s.apiMux = mux
	s.apiWrapped = s.actorMiddleware(s.auditMiddleware(s.forwardMachine(noTraversalMiddleware(newRemoteGate(mux).wrap(newScopeGate(mux).wrap(s.archivedGate(newConfirmGate().wrap(mux))))))))
	if s.ext.UI != nil {
		// The SPA sits beside the API on one mux.
		return ui.Handler(s.ext.UI, s.apiWrapped)
	}
	if fsys, ok := ui.FS(); ok {
		return ui.Handler(fsys, s.apiWrapped)
	}
	return s.apiWrapped
}

// Root is the one handler of the loopback listener. The relay hub serves
// each phone stream with it too. It is built once.
func (s *Server) Root() http.Handler {
	s.rootOnce.Do(func() { s.root = s.Handler() })
	return s.root
}

// ListenAndServe binds addr (e.g. 127.0.0.1:0) and serves HTTP.
func (s *Server) ListenAndServe(addr string) (actual string, err error) {
	return s.serve(addr, "", "")
}

// ListenAndServeTLS binds addr and serves HTTPS with the PEM pair.
func (s *Server) ListenAndServeTLS(addr, certFile, keyFile string) (actual string, err error) {
	return s.serve(addr, certFile, keyFile)
}

func (s *Server) serve(addr, certFile, keyFile string) (actual string, err error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	s.listener = ln
	s.httpServer = &http.Server{Handler: s.Root()}
	s.scheme = "http"
	if certFile != "" {
		s.scheme = "https"
	}
	actual = ln.Addr().String()
	// Sessions started from now on inject the bound /mcp URL.
	if s.asst != nil {
		s.asst.SetMCPURL(s.boundMCPURL())
	}
	if s.connectors != nil {
		s.connectors.SetLocalURL(s.localURL())
	}
	s.startDoctorRefresher()
	// Serve exits when Shutdown closes the listener; Server owns that lifetime.
	go func() {
		var err error
		if certFile != "" {
			err = s.httpServer.ServeTLS(ln, certFile, keyFile)
		} else {
			err = s.httpServer.Serve(ln)
		}
		if err != nil && err != http.ErrServerClosed {
			s.log.Error("http serve", "error", err)
		}
	}()
	return actual, nil
}

// WriteAPIJSON writes the port and URL to api.json. Clients read it to find
// a daemon that bound an ephemeral port.
func (s *Server) WriteAPIJSON(listenAddr string) error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	host, portStr, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return err
	}
	port, _ := strconv.Atoi(portStr)
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		// A wildcard bind is dual-stack; 127.0.0.1 is the canonical client
		// URL (Go reports [::] for an IPv4 wildcard on macOS).
		host = "127.0.0.1"
	}
	scheme := s.scheme
	if scheme == "" {
		scheme = "http"
	}
	doc := map[string]any{
		"port": port,
		"url":  scheme + "://" + net.JoinHostPort(host, portStr),
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return os.WriteFile(home.APIJSONPath(), b, 0o600)
}

// boundMCPURL renders the /mcp endpoint on the bound listener.
func (s *Server) boundMCPURL() string {
	if s.listener == nil {
		return s.mcpURLGuess()
	}
	host, port, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		return s.mcpURLGuess()
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	scheme := s.scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + net.JoinHostPort(host, port) + "/mcp"
}

// Shutdown stops the HTTP server and closes open streams.
func (s *Server) Shutdown(ctx context.Context) error {
	s.doneOnce.Do(func() { close(s.done) })
	if s.moves != nil {
		s.moves.Close()
	}
	if s.asst != nil {
		_ = s.asst.Shutdown(ctx)
	}
	if s.runs == s.ownRuns {
		s.runs.CloseCLIRuns(ctx)
	}
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

// localURL is the API address on the bound listener, for the login return
// page on this computer.
func (s *Server) localURL() string { return strings.TrimSuffix(s.boundMCPURL(), "/mcp") }

func (s *Server) uiVersion() string {
	if s.ext.UI != nil {
		return s.ext.UIVersion
	}
	return UIVersion
}

func (s *Server) edition() string {
	if s.ext.Edition != "" {
		return s.ext.Edition
	}
	return "free"
}

// maskedBody builds one masked list body (plan 2026-09-22 Part G): every
// row passes through payload with the mask flag, and ?reveal=1 unmasks
// all of them and records one audit entry.
func maskedBody[T any](s *Server, r *http.Request, key, audit string, rows []T, payload func(T, bool) map[string]any) map[string]any {
	masked := r.URL.Query().Get("reveal") != "1"
	if !masked {
		s.log.Info(audit, "count", len(rows))
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, payload(row, masked))
	}
	return map[string]any{key: items}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// APIError envelope.
type APIError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

type ErrorBody struct {
	Error APIError `json:"error"`
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, ErrorBody{Error: APIError{Code: errCode, Message: msg}})
}

func writeErrDetails(w http.ResponseWriter, code int, errCode, msg string, details any) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{"code": errCode, "message": msg, "details": details},
	})
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// appendBus emits one bus event after a non-DB write (files, secrets, config).
// Do not use this for DB-backed entities — wrap those with store.AppendEntity
// so the row and the bus frame commit together. A crash must not advance an
// entity past what the bus recorded.
func (s *Server) appendBus(ctx context.Context, ev store.BusEvent) {
	if _, err := s.st.Append(ctx, ev); err != nil {
		s.log.Warn("bus append failed", "topic", ev.Topic, "error", err)
	}
}
