// Package api serves the /v1 HTTP API + SSE bus.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/ext"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/license"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/runtime/cli"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tmux"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/ui"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Version is reported on health. Stamped at build time; "dev" in a plain build.
var Version = "dev" //nolint:gochecknoglobals // -ldflags target, set once before main serves

// UIVersion is the embedded SPA version, empty when no UI is embedded.
var UIVersion = "" //nolint:gochecknoglobals // -ldflags target, set once before main serves

// Server is the trebid HTTP API.
type Server struct {
	st         *store.Store
	runs       *runs.Service
	ws         *workspaces.Service
	files      *files.Service
	alerts     *alerts.Service
	skills     *skills.Service
	connectors *connectors.Service
	conv       *conversations.Service
	harness    *harness.Service
	sched      *schedule.Service
	email      *email.Service
	lic        *license.Service

	token     string
	startedAt time.Time
	drain     bool
	shadow    bool
	mu        sync.RWMutex
	done      chan struct{}
	doneOnce  sync.Once
	log       *slog.Logger
	queue     *dispatch.Queue

	upgrader   websocket.Upgrader
	httpServer *http.Server
	listener   net.Listener
	scheme     string // http or https, set at listen

	apiMux     *http.ServeMux
	apiWrapped http.Handler
	toolsReg   *tools.Registry
	mcpHTTP    http.Handler

	ext          ext.Set
	lookupLatest func() (tag, url string, err error)
	latest       latestCache
}

// Options for New.
type Options struct {
	Token          string
	Shadow         bool
	Drain          bool
	FilesMaxBytes  int64  // 0 = files.DefaultMaxBytes
	WorkspacesRoot string // pathless workspace create; empty refuses it
	Ext            ext.Set
	// License is the process-wide service built in service daemon. Nil
	// builds a new one from Ext.License so tests can omit it.
	License *license.Service
	// LookupLatest overrides the GitHub release lookup used by
	// /v1/system/update. Tests inject a stub so the suite never hits the network.
	LookupLatest func() (tag, url string, err error)
}

// New builds an API server.
func New(st *store.Store, opts Options) *Server {
	ws := workspaces.New(st).WithRoot(opts.WorkspacesRoot)
	fs := files.New(opts.FilesMaxBytes)
	r := runs.New(st, ws, fs)
	r.SetMemoryResolver(ws)
	q := dispatch.NewQueue(st)
	pol := dispatch.NewPolicy()
	lic := opts.License
	if lic == nil {
		lic = license.New(opts.Ext.License)
	}
	s := &Server{
		st:           st,
		runs:         r,
		ws:           ws,
		files:        fs,
		alerts:       alerts.New(st, alerts.Options{}),
		skills:       skills.New(st),
		connectors:   connectors.New(st, ws, r, pol),
		conv:         conversations.New(st).WithRuns(r).WithQueue(q).WithWorkspaces(ws),
		harness:      harness.New(st, tmux.DefaultDriver{}, harness.Options{}),
		sched:        schedule.New(st),
		lic:          lic,
		email:        email.New(st, lic),
		token:        opts.Token,
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
	if s.token == "" {
		s.token = ulid.Make().String()
	}
	r.SetNativeHooks(runs.NativeHooks{
		Lifetime: context.Background(),
		Ingest: func(ctx context.Context, runID, convID string, texts []string) error {
			turns := make([]conversations.IncomingTurn, len(texts))
			now := time.Now()
			for i, t := range texts {
				turns[i] = conversations.IncomingTurn{Role: conversations.RoleAssistant, Text: t, At: now}
			}
			_, err := s.conv.IngestAssistantTurns(ctx, runID, convID, turns)
			return err
		},
		Recap: func(ctx context.Context, convID, text string) error {
			return s.conv.AppendSystem(ctx, convID, text)
		},
		Dispatch: s,
	})
	_ = s.connectors.Load()
	s.toolsReg = tools.NewRegistry(s)
	s.mcpHTTP = s.newMCPHandler()
	return s
}

// BindControlPlane replaces core services with the daemon's shared instances.
func (s *Server) BindControlPlane(runsSvc *runs.Service, ws *workspaces.Service, connectors *connectors.Service, sched *schedule.Service, alertsSvc *alerts.Service, filesSvc *files.Service, conv *conversations.Service, harnessSvc *harness.Service, emailSvc *email.Service, licSvc *license.Service) {
	if runsSvc != nil {
		s.runs = runsSvc
		s.runs.SetNativeHooks(runs.NativeHooks{Dispatch: s})
	}
	if ws != nil {
		s.ws = ws
	}
	if connectors != nil {
		s.connectors = connectors
	}
	if sched != nil {
		s.sched = sched
	}
	if alertsSvc != nil {
		s.alerts = alertsSvc
	}
	if filesSvc != nil {
		s.files = filesSvc
	}
	if conv != nil {
		s.conv = conv
	}
	if harnessSvc != nil {
		s.harness = harnessSvc
	}
	if emailSvc != nil {
		s.email = emailSvc
	}
	if licSvc != nil {
		s.lic = licSvc
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

// Token returns the bearer token.
func (s *Server) Token() string { return s.token }

// SetCLIDriver installs a subprocess driver override on the runs service
// (tests; production uses the built-in per-harness drivers).
func (s *Server) SetCLIDriver(d cli.Driver) { s.runs.SetCLIDriver(d) }

// Alerts returns the alert service (tests).
func (s *Server) Alerts() *alerts.Service { return s.alerts }

// Skills returns the skills service.
func (s *Server) Skills() *skills.Service { return s.skills }

// Email returns the email channel service.
func (s *Server) Email() *email.Service { return s.email }

// License returns the license service.
func (s *Server) License() *license.Service { return s.lic }

// Handler returns the root mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routeSystem(mux)
	s.routeWorkspaces(mux)
	s.routeAgents(mux)
	s.routeMemory(mux)
	s.routeFiles(mux)
	s.routeJobs(mux)
	s.routeRuns(mux)
	s.routeQueue(mux)
	s.routeEvents(mux)
	s.routeInbox(mux)
	s.routeConversations(mux)
	s.routeConnectors(mux)
	s.routeHarness(mux)
	s.routeSettings(mux)
	s.routeEmail(mux)
	s.routeMCP(mux)
	// Dispatch from catalog tools re-enters here, audit-wrapped but not
	// auth-wrapped. MCP already passed authMiddleware; native is the daemon.
	s.apiMux = mux
	s.apiWrapped = s.auditMiddleware(noTraversalMiddleware(newConfirmGate().wrap(mux)))
	apiHandler := s.authMiddleware(s.apiWrapped)
	if s.ext.UI != nil {
		// The SPA sits outside auth; every /v1 call still needs the bearer.
		return ui.Handler(s.ext.UI, apiHandler)
	}
	if fsys, ok := ui.FS(); ok {
		return ui.Handler(fsys, apiHandler)
	}
	return apiHandler
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
	s.httpServer = &http.Server{Handler: s.Handler()}
	s.scheme = "http"
	if certFile != "" {
		s.scheme = "https"
	}
	actual = ln.Addr().String()
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

// WriteAPIJSON writes token+port to api.json.
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
		"token": s.token,
		"port":  port,
		"url":   scheme + "://" + net.JoinHostPort(host, portStr),
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return os.WriteFile(home.APIJSONPath(), b, 0o600)
}

// Shutdown stops the HTTP server and closes open streams.
func (s *Server) Shutdown(ctx context.Context) error {
	s.doneOnce.Do(func() { close(s.done) })
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

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
