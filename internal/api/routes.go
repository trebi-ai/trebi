package api

import (
	"net/http"
	"strconv"

	"github.com/oklog/ulid/v2"
)

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
	s.routeConversations(mux)
	s.routeIntegrations(mux)
	s.routeHarness(mux)
	s.routeSettings(mux)
	return s.authMiddleware(s.auditMiddleware(mux))
}
