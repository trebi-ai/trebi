package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tools"
)

// actorMiddleware resolves X-Trebi-Run-Token into tools.WithActor. A run
// token resolves first; an assistant session token resolves next. A bad or
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
			actor, err = s.assistantActor(r.Context(), token)
			if err != nil {
				writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid run token")
				return
			}
		}
		actor.Token = token
		next.ServeHTTP(w, r.WithContext(tools.WithActor(r.Context(), actor)))
	})
}

// assistantActor resolves one live assistant session token (plan D.8). A
// closed or errored session has no live token.
func (s *Server) assistantActor(ctx context.Context, token string) (tools.Actor, error) {
	if s.asstStore == nil {
		return tools.Actor{}, errors.New("invalid run token")
	}
	rec, err := s.asstStore.ByTokenHash(ctx, runs.HashActorToken(token))
	if err != nil {
		return tools.Actor{}, err
	}
	return tools.Actor{
		Kind:         "assistant",
		WorkspaceID:  rec.WorkspaceID,
		Conversation: rec.ConversationID,
		Name:         "assistant",
	}, nil
}
