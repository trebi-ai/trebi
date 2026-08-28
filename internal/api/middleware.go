package api

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// statusWriter records the response code for the audit log.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush lets SSE handlers stream through the audit wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap supports http.ResponseController passthrough.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Hijack lets WebSocket upgrades pass through the audit wrapper.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying writer does not implement http.Hijacker")
	}
	return h.Hijack()
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		token := strings.TrimPrefix(h, "Bearer ")
		if token == "" && allowQueryToken(r) {
			token = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			s.log.Warn("api.auth_failed", "remote", r.RemoteAddr, "path", r.URL.Path)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or missing bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(rw, r)
		level := slog.LevelInfo
		if isPollRoute(r) && rw.code < 400 {
			// The tray polls these every 2s. At INFO they bury real activity.
			level = slog.LevelDebug
		}
		s.log.Log(r.Context(), level, "api.request",
			"method", r.Method,
			"route", r.URL.Path,
			"status", rw.code,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// isPollRoute reports whether r is a routine background read. Only successful
// ones are demoted: a failing poll still says so at INFO.
func isPollRoute(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/v1/system/health", "/v1/agents/state":
		return true
	}
	return false
}

// allowQueryToken permits ?token= only where clients cannot set headers:
// the SSE stream and the WebSocket attach route.
func allowQueryToken(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.URL.Path == "/v1/stream" {
		return true
	}
	return strings.HasPrefix(r.URL.Path, "/v1/runs/") && strings.HasSuffix(r.URL.Path, "/attach")
}
