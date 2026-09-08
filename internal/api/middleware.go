package api

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/runs"
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

// noTraversalMiddleware rejects a request path containing a "." or ".."
// segment. http.ServeMux would otherwise clean the path and answer 307 before
// any handler runs, which silently drops a PUT body and reports success to the
// caller. The jail check in the file handlers never sees such a request.
func noTraversalMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seg, bad := dotSegment(r.URL.EscapedPath()); bad {
			writeErr(w, http.StatusBadRequest, "validation",
				"path segment "+strconv.Quote(seg)+" is not allowed; use a path relative to the resource root")
			return
		}
		// A resource path that itself starts with "/" produces a doubled
		// slash. ServeMux would clean it and answer 307, so the caller sees a
		// redirect instead of the real mistake: an absolute path.
		if strings.Contains(r.URL.EscapedPath(), "//") {
			writeErr(w, http.StatusBadRequest, "validation",
				"path must be relative to the resource root; absolute paths are not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func dotSegment(p string) (string, bool) {
	for _, seg := range strings.Split(p, "/") {
		// Catch percent-encoded forms (%2e%2e) as well as literal dots.
		if unescaped, err := url.PathUnescape(seg); err == nil {
			seg = unescaped
		}
		if seg == "." || seg == ".." {
			return seg, true
		}
	}
	return "", false
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
		// A successful /mcp call logs at DEBUG: the inner /v1 request it
		// dispatched already logged at INFO.
		return r.Method == http.MethodPost && r.URL.Path == "/mcp"
	}
	switch r.URL.Path {
	case "/v1/system/health", "/v1/agents/state":
		return true
	}
	return false
}

// allowQueryToken permits ?token= only where clients cannot set headers:
// the SSE stream and WebSocket attach routes (run and harness login).
func allowQueryToken(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.URL.Path == "/v1/stream" {
		return true
	}
	if !strings.HasSuffix(r.URL.Path, "/attach") {
		return false
	}
	return strings.HasPrefix(r.URL.Path, "/v1/runs/") ||
		strings.HasPrefix(r.URL.Path, "/v1/harnesses/")
}

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// parseLimit reads ?limit= with default 50 and a hard cap of 200.
func parseLimit(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 {
		return defaultListLimit
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}

// parseTimeFilter reads a since/until value. It takes RFC3339, or a duration
// such as 24h meaning "that long ago". An empty value returns "".
func parseTimeFilter(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return "", fmt.Errorf("want RFC3339 or a duration such as 24h, got %q", v)
	}
	return time.Now().UTC().Add(-d).Format(time.RFC3339), nil
}

// parseWait reads ?wait_ms= as a long-poll duration, capped at runs.MaxWait.
func parseWait(r *http.Request) time.Duration {
	n, _ := strconv.Atoi(r.URL.Query().Get("wait_ms"))
	if n <= 0 {
		return 0
	}
	d := time.Duration(n) * time.Millisecond
	if d > runs.MaxWait {
		return runs.MaxWait
	}
	return d
}

// parseULIDCursor reads ?cursor=. Empty is ok; a malformed ULID is a 400.
func parseULIDCursor(w http.ResponseWriter, r *http.Request, kind string) (string, bool) {
	cursor := r.URL.Query().Get("cursor")
	if cursor == "" {
		return "", true
	}
	if _, err := ulid.Parse(cursor); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "malformed cursor: not a "+kind+" id")
		return "", false
	}
	return cursor, true
}

// Idempotency scopes keep run and job keys apart in one table.
const (
	idempotencyRuns = "runs"
	idempotencyJobs = "jobs"
)

// idempotencyKey reads the Idempotency-Key header, falling back to the body field.
func idempotencyKey(r *http.Request, bodyKey string) string {
	if h := strings.TrimSpace(r.Header.Get("Idempotency-Key")); h != "" {
		return h
	}
	return strings.TrimSpace(bodyKey)
}

// bulkIDs reads the {"ids": [...]} body of a bulk verb. An empty list is a 400,
// because a silent no-op would read as success.
func bulkIDs(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return nil, false
	}
	if len(body.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "validation", "ids must hold at least one id")
		return nil, false
	}
	return body.IDs, true
}

// applyBulk runs verb over ids and returns the per-id outcome. One failure does
// not stop the rest: the caller sees which id failed and why.
func applyBulk(ctx context.Context, ids []string, verb func(context.Context, string) error) map[string]any {
	done := make([]string, 0, len(ids))
	failed := map[string]string{}
	for _, id := range ids {
		if err := verb(ctx, id); err != nil {
			failed[id] = err.Error()
			continue
		}
		done = append(done, id)
	}
	out := map[string]any{"ok": done, "count": len(done)}
	if len(failed) > 0 {
		out["failed"] = failed
	}
	return out
}
