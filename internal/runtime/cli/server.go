package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// openCodeReadyTimeout bounds the serve health wait (E.3).
const openCodeReadyTimeout = 30 * time.Second

// openCodeServerIdle is how long a refcount-zero shared server lingers before
// shutdown.
const openCodeServerIdle = 60 * time.Second

// openCodeServer is one daemon-owned `opencode serve` process.
type openCodeServer struct {
	key      string
	bin      string
	port     int
	password string
	username string
	proc     *Proc
	argv     []string
	// ledger is the daemon-owned helper ledger (server pid + startup token),
	// so a crashed daemon reaps leftovers on boot (E.3).
	ledger *Registry

	mu   sync.Mutex
	refs int
	idle *time.Timer
}

// baseURL is the loopback origin.
func (s *openCodeServer) baseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.port)
}

func (s *openCodeServer) acquire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs++
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
}

func (s *openCodeServer) release(idle time.Duration, onStop func()) {
	s.mu.Lock()
	s.refs--
	refs := s.refs
	if refs <= 0 && s.idle == nil {
		if idle <= 0 {
			idle = openCodeServerIdle
		}
		s.idle = time.AfterFunc(idle, func() {
			s.mu.Lock()
			stop := s.refs <= 0
			s.idle = nil
			s.mu.Unlock()
			if stop {
				onStop()
			}
		})
	}
	s.mu.Unlock()
}

func (s *openCodeServer) stop() {
	openCodeForgetServer(s)
	if s.ledger != nil {
		s.ledger.Remove("opencode:" + s.key)
	}
	if s.proc != nil {
		_ = s.proc.gracefulClose(gracefulCloseTimeout)
	}
}

// openCodeManager owns shared servers keyed by bin + env + extra args.
type openCodeManager struct {
	mu      sync.Mutex
	servers map[string]*openCodeServer
	idle    time.Duration
	// ledger records live servers under TREBI_HOME for boot reconcile.
	ledger *Registry
}

var openCodeServers = &openCodeManager{servers: map[string]*openCodeServer{}, idle: openCodeServerIdle}

// SetOpenCodeLedger installs the helper pid ledger (daemon wiring). Nil
// disables persistence (tests).
func SetOpenCodeLedger(r *Registry) {
	openCodeServers.mu.Lock()
	openCodeServers.ledger = r
	openCodeServers.mu.Unlock()
}

// ReconcileOpenCodeServers kills leftover daemon-owned serve processes from a
// previous daemon and drops the ledger. Only positively matched pids die.
func ReconcileOpenCodeServers() (killed []string) {
	openCodeServers.mu.Lock()
	ledger := openCodeServers.ledger
	openCodeServers.mu.Unlock()
	if ledger == nil {
		return nil
	}
	killed, _ = ledger.Reconcile()
	return killed
}

// shutdown stops every managed server (daemon shutdown/tests).
func (m *openCodeManager) shutdown() {
	m.mu.Lock()
	servers := m.servers
	m.servers = map[string]*openCodeServer{}
	m.mu.Unlock()
	for _, s := range servers {
		s.stop()
	}
}

// openCodeFingerprint keys a shared server by the env that changes what the
// server can serve: bin, non-run-scoped env, extra args. Per-run values
// (TREBI_RUN_ID, hook tool, memory path) and the server's own auth are
// excluded, so ordinary runs share one refcounted server (E.3).
func openCodeFingerprint(bin string, env map[string]string, extra []string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		if openCodeEnvExcluded(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(bin)
	b.WriteByte('\x00')
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(env[k])
		b.WriteByte('\x00')
	}
	b.WriteString(strings.Join(extra, "\x00"))
	return b.String()
}

// openCodeEnvExcluded reports env keys that must not split the shared server.
func openCodeEnvExcluded(key string) bool {
	switch key {
	case "OPENCODE_SERVER_PASSWORD", "OPENCODE_SERVER_USERNAME",
		"TREBI_RUN_ID", "TREBI_HOOK_TOOL", "TREBI_MEMORY_PATH":
		return true
	}
	return false
}

// ledgerRef returns the server pid ledger under lock.
func (m *openCodeManager) ledgerRef() *Registry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ledger
}

// acquireServer returns a live shared server, starting one when needed.
func (m *openCodeManager) acquireServer(ctx context.Context, bin string, env map[string]string, extraArgs []string) (*openCodeServer, error) {
	key := openCodeFingerprint(bin, env, extraArgs)
	m.mu.Lock()
	if s, ok := m.servers[key]; ok && s.proc != nil && s.proc.ExitCode() == -1 {
		m.mu.Unlock()
		s.acquire()
		return s, nil
	}
	m.mu.Unlock()

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	password := strings.ReplaceAll(nextRequestID(), "req_", "t")
	childEnv := map[string]string{}
	for k, v := range env {
		childEnv[k] = v
	}
	childEnv["OPENCODE_SERVER_PASSWORD"] = password
	if childEnv["OPENCODE_SERVER_USERNAME"] == "" {
		childEnv["OPENCODE_SERVER_USERNAME"] = "opencode"
	}
	args := append([]string{"serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"}, extraArgs...)
	proc, err := StartProc(ctx, ProcOpts{Path: bin, Args: args, Dir: "", Env: childEnvOf(childEnv)})
	if err != nil {
		return nil, fmt.Errorf("opencode serve: %w", err)
	}
	s := &openCodeServer{
		key: key, bin: bin, port: port, password: password,
		username: childEnv["OPENCODE_SERVER_USERNAME"], proc: proc, refs: 1,
		argv:   append([]string{bin}, args...),
		ledger: m.ledgerRef(),
	}
	if err := s.waitHealthy(ctx); err != nil {
		_ = proc.KillTree()
		return nil, err
	}
	if s.ledger != nil {
		s.ledger.Track("opencode:"+key, "opencode", proc.PID(), proc.Pgid(), s.argv)
	}
	m.mu.Lock()
	if existing, ok := m.servers[key]; ok && existing.proc != nil && existing.proc.ExitCode() == -1 {
		// A concurrent acquire won the race; keep the first.
		m.mu.Unlock()
		s.stop()
		existing.acquire()
		return existing, nil
	}
	m.servers[key] = s
	m.mu.Unlock()
	return s, nil
}

// childEnvOf merges process env + overrides, like childEnv but for a map.
func childEnvOf(env map[string]string) []string { return childEnv(env) }

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitHealthy polls /global/health until the server answers or the child dies.
func (s *openCodeServer) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(openCodeReadyTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		if s.proc.ExitCode() != -1 {
			return fmt.Errorf("opencode serve exited early: %s", strings.TrimSpace(s.proc.Stderr()))
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL()+"/global/health", nil)
		req.SetBasicAuth(s.username, s.password)
		if resp, err := client.Do(req); err == nil {
			var body struct {
				Healthy bool   `json:"healthy"`
				Version string `json:"version"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if body.Healthy {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("opencode serve: health timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// call runs one HTTP request against the server with directory + basic auth.
func (s *openCodeServer) call(ctx context.Context, method, path, directory string, body any, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL()+path, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if directory != "" {
		req.Header.Set("x-opencode-directory", directory)
	}
	req.SetBasicAuth(s.username, s.password)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
			Data  struct {
				Message string `json:"message"`
			} `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		msg := e.Error
		if msg == "" {
			msg = e.Data.Message
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("opencode %s %s: %s", method, path, msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// sseEvent is one server-sent event frame.
type sseEvent struct {
	Event string
	Data  []byte
}

// stream opens /event and forwards parsed frames until ctx ends.
func (s *openCodeServer) stream(ctx context.Context, directory string) (<-chan sseEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL()+"/event", nil)
	if err != nil {
		return nil, err
	}
	if directory != "" {
		req.Header.Set("x-opencode-directory", directory)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.SetBasicAuth(s.username, s.password)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("opencode /event: HTTP %d", resp.StatusCode)
	}
	out := make(chan sseEvent, 128)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)
		var event string
		var data strings.Builder
		flush := func() bool {
			if data.Len() == 0 {
				event = ""
				return true
			}
			frame := sseEvent{Event: event, Data: []byte(strings.TrimRight(data.String(), "\n"))}
			event = ""
			data.Reset()
			select {
			case out <- frame:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				data.WriteByte('\n')
			case line == "":
				if !flush() {
					return
				}
			}
		}
		flush()
	}()
	return out, nil
}
