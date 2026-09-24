package api

import (
	"net/http"

	"github.com/trebi-ai/trebi/internal/tools"
)

// actorMiddleware resolves X-Trebi-Run-Token into tools.WithActor. A bad or
// dead token is 401; no header is a human call and remains unscoped.
func (s *Server) actorMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(tools.HeaderRunToken)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		actor, err := s.runs.ActorForToken(r.Context(), token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid run token")
			return
		}
		actor.Token = token
		next.ServeHTTP(w, r.WithContext(tools.WithActor(r.Context(), actor)))
	})
}
