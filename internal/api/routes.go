package api

import "net/http"

// Handler returns the root mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routeSystem(mux)
	s.routeWorkspaces(mux)
	s.routeJobs(mux)
	s.routeRuns(mux)
	s.routeQueue(mux)
	s.routeEvents(mux)
	s.routeIntegrations(mux)
	s.routeSettings(mux)
	return s.authMiddleware(s.auditMiddleware(mux))
}
