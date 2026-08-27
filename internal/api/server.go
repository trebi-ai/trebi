// Package api serves the /v1 HTTP API + SSE bus.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/integrations"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Version is reported on health.
const Version = "0.1.0"

// Server is the trebid HTTP API.
type Server struct {
	st     *store.Store
	runs   *runs.Service
	ws     *workspaces.Service
	alerts *alerts.Service
	skills *skills.Service
	integ  *integrations.Service

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
	Token  string
	Shadow bool
	Drain  bool
}

// New builds an API server.
func New(st *store.Store, opts Options) *Server {
	ws := workspaces.New(st)
	r := runs.New(st)
	pol := dispatch.NewPolicy()
	s := &Server{
		st:        st,
		runs:      r,
		ws:        ws,
		alerts:    alerts.New(st),
		skills:    skills.New(st),
		integ:     integrations.New(st, ws, r, pol),
		token:     opts.Token,
		startedAt: time.Now().UTC(),
		shadow:    opts.Shadow,
		drain:     opts.Drain || opts.Shadow,
		done:      make(chan struct{}),
		log:       slog.Default().With("source", "http_server"),
		queue:     dispatch.NewQueue(st),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	if s.token == "" {
		s.token = ulid.Make().String()
	}
	_ = s.integ.Load()
	return s
}

// BindControlPlane replaces core services with the daemon's shared instances.
// The policy and schedule services stay with the daemon; the API does not
// read them.
func (s *Server) BindControlPlane(runsSvc *runs.Service, ws *workspaces.Service, _ *dispatch.Policy, integ *integrations.Service, _ *schedule.Service) {
	if runsSvc != nil {
		s.runs = runsSvc
	}
	if ws != nil {
		s.ws = ws
	}
	if integ != nil {
		s.integ = integ
	}
}

// Runs returns the runs service.
func (s *Server) Runs() *runs.Service { return s.runs }

// Workspaces returns the workspace service.
func (s *Server) Workspaces() *workspaces.Service { return s.ws }

// Token returns the bearer token.
func (s *Server) Token() string { return s.token }

// Alerts returns the alert service (tests).
func (s *Server) Alerts() *alerts.Service { return s.alerts }

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

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
