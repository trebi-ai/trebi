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
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/integrations"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tmux"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Version is reported on health.
const Version = "0.1.0"

// Server is the trebid HTTP API.
type Server struct {
	st      *store.Store
	runs    *runs.Service
	ws      *workspaces.Service
	files   *files.Service
	alerts  *alerts.Service
	skills  *skills.Service
	integ   *integrations.Service
	conv    *conversations.Service
	harness *harness.Service
	sched   *schedule.Service

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
}

// Options for New.
type Options struct {
	Token          string
	Shadow         bool
	Drain          bool
	FilesMaxBytes  int64  // 0 = files.DefaultMaxBytes
	WorkspacesRoot string // pathless workspace create; empty refuses it
}

// New builds an API server.
func New(st *store.Store, opts Options) *Server {
	ws := workspaces.New(st).WithRoot(opts.WorkspacesRoot)
	r := runs.New(st)
	q := dispatch.NewQueue(st)
	pol := dispatch.NewPolicy()
	s := &Server{
		st:        st,
		runs:      r,
		ws:        ws,
		files:     files.New(opts.FilesMaxBytes),
		alerts:    alerts.New(st, alerts.Options{}),
		skills:    skills.New(st),
		integ:     integrations.New(st, ws, r, pol),
		conv:      conversations.New(st).WithRuns(r).WithQueue(q).WithWorkspaces(ws),
		harness:   harness.New(st, tmux.DefaultDriver{}, harness.Options{}),
		sched:     schedule.New(st),
		token:     opts.Token,
		startedAt: time.Now().UTC(),
		shadow:    opts.Shadow,
		drain:     opts.Drain || opts.Shadow,
		done:      make(chan struct{}),
		log:       slog.Default().With("source", "http_server"),
		queue:     q,
		upgrader: websocket.Upgrader{
			CheckOrigin: checkWSOrigin,
		},
	}
	if s.token == "" {
		s.token = ulid.Make().String()
	}
	_ = s.integ.Load()
	return s
}

// BindControlPlane replaces core services with the daemon's shared instances.
func (s *Server) BindControlPlane(runsSvc *runs.Service, ws *workspaces.Service, integ *integrations.Service, sched *schedule.Service, alertsSvc *alerts.Service, filesSvc *files.Service, conv *conversations.Service, harnessSvc *harness.Service) {
	if runsSvc != nil {
		s.runs = runsSvc
	}
	if ws != nil {
		s.ws = ws
	}
	if integ != nil {
		s.integ = integ
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
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// Runs returns the runs service.
func (s *Server) Runs() *runs.Service { return s.runs }

// Workspaces returns the workspace service.
func (s *Server) Workspaces() *workspaces.Service { return s.ws }

// Token returns the bearer token.
func (s *Server) Token() string { return s.token }

// Alerts returns the alert service (tests).
func (s *Server) Alerts() *alerts.Service { return s.alerts }

// Handler returns the root mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routeSystem(mux)
	s.routeWorkspaces(mux)
	s.routeFiles(mux)
	s.routeJobs(mux)
	s.routeRuns(mux)
	s.routeQueue(mux)
	s.routeEvents(mux)
	s.routeInbox(mux)
	s.routeConversations(mux)
	s.routeIntegrations(mux)
	s.routeHarness(mux)
	s.routeSettings(mux)
	return s.authMiddleware(s.auditMiddleware(mux))
}

// ListenAndServe binds addr (e.g. 127.0.0.1:0) and serves.
func (s *Server) ListenAndServe(addr string) (actual string, err error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	s.listener = ln
	s.httpServer = &http.Server{Handler: s.Handler()}
	actual = ln.Addr().String()
	// Serve exits when Shutdown closes the listener; Server owns that lifetime.
	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
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
	// parse port
	_, portStr, _ := net.SplitHostPort(listenAddr)
	port, _ := strconv.Atoi(portStr)
	doc := map[string]any{
		"token": s.token,
		"port":  port,
		"url":   "http://" + listenAddr,
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
