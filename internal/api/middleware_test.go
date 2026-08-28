package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestIsPollRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v1/system/health", true},
		{"GET", "/v1/agents/state", true},
		{"GET", "/v1/config", false},
		{"GET", "/v1/runs", false},
		{"PUT", "/v1/workspaces/x/files/a.py", false},
		// Only reads are routine; a write to the same path is not.
		{"POST", "/v1/system/health", false},
		// Prefix matches must not count.
		{"GET", "/v1/system/health/extra", false},
		{"GET", "/v1/system", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := isPollRoute(r); got != c.want {
			t.Errorf("isPollRoute(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestAuditMiddlewareDemotesPolls(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var mu sync.Mutex
	h := slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})
	srv := &Server{log: slog.New(h)}

	call := func(method, path string, code int) {
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		})
		srv.auditMiddleware(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
	}

	call("GET", "/v1/system/health", 200)
	call("GET", "/v1/agents/state", 200)
	call("GET", "/v1/config", 200)
	// A failing poll must stay visible at INFO.
	call("GET", "/v1/system/health", 500)

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 log lines, got %d:\n%s", len(lines), out)
	}
	for i, want := range []string{"DEBUG", "DEBUG", "INFO", "INFO"} {
		if !strings.Contains(lines[i], "level="+want) {
			t.Errorf("line %d: want %s, got %q", i, want, lines[i])
		}
	}
}

// syncWriter guards the buffer so the handler is safe under -race.
type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
