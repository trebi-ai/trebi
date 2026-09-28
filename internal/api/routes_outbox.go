package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/tools"
)

func (s *Server) routeOutbox(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/outbox", s.handleOutboxList)
	mux.HandleFunc("GET /v1/outbox/{id}", s.handleOutboxGet)
	mux.HandleFunc("POST /v1/outbox/{id}/retry", s.handleOutboxRetry)
	mux.HandleFunc("POST /v1/outbox/{id}/cancel", s.handleOutboxCancel)
}

// BindOutbox replaces the default outbox with the daemon's shared
// instance, so the API and the worker use one set of senders.
func (s *Server) BindOutbox(ob *outbox.Service) {
	if ob != nil {
		s.outbox = ob
		s.email.WithOutbox(ob)
	}
}

// handleOutboxList lists outbox rows newest first (plan 2026-09-28 B.4).
// An assistant sees only its workspace. An agent sees only the rows of its
// own conversations and runs.
func (s *Server) handleOutboxList(w http.ResponseWriter, r *http.Request) {
	cursor, ok := parseULIDCursor(w, r, "outbox row")
	if !ok {
		return
	}
	q := r.URL.Query()
	f := outbox.Filter{
		Kind: outbox.Kind(q.Get("kind")), Status: outbox.Status(q.Get("status")),
		Purpose: outbox.Purpose(q.Get("purpose")), WorkspaceID: s.workspaceID(r.Context(), q.Get("workspace")),
		ConversationID: q.Get("conversation"), InboxID: q.Get("inbox"),
		Cursor: cursor, Limit: parseLimit(r),
	}
	if a, ok := tools.ActorFrom(r.Context()); ok && a.Kind == "assistant" && a.WorkspaceID != "" {
		f.WorkspaceID = a.WorkspaceID
	}
	scope, scoped := scopeFrom(r.Context())
	if scoped {
		f.WorkspaceID = scope.workspaceID
	}
	rows, next, err := s.outbox.List(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if scoped {
		kept := rows[:0]
		for _, row := range rows {
			if s.agentOwnsRow(r.Context(), scope, row) {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	resp := map[string]any{"outbox": rows}
	if next != "" {
		resp["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleOutboxGet(w http.ResponseWriter, r *http.Request) {
	row, ok := s.outboxRowFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleOutboxRetry(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.outboxRowFor(w, r); !ok {
		return
	}
	row, err := s.outbox.Retry(r.Context(), r.PathValue("id"))
	writeOutboxResult(w, row, err, "outbox row is not failed or canceled")
}

func (s *Server) handleOutboxCancel(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.outboxRowFor(w, r); !ok {
		return
	}
	row, err := s.outbox.Cancel(r.Context(), r.PathValue("id"))
	writeOutboxResult(w, row, err, "outbox row is not pending or failed")
}

// outboxRowFor loads the row of the path and applies the caller scope.
func (s *Server) outboxRowFor(w http.ResponseWriter, r *http.Request) (outbox.Row, bool) {
	row, err := s.outbox.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, outbox.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "outbox row not found")
		return row, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return row, false
	}
	if scope, ok := scopeFrom(r.Context()); ok && !s.agentOwnsRow(r.Context(), scope, row) {
		writeForbidden(w, "agent may act only on the rows of its own conversations and runs")
		return row, false
	}
	if !s.assistantScoped(w, r, row.WorkspaceID) {
		return row, false
	}
	return row, true
}

// agentOwnsRow is true when the row belongs to a conversation the agent
// operates, or to a run of the agent.
func (s *Server) agentOwnsRow(ctx context.Context, scope agentScope, row outbox.Row) bool {
	if row.WorkspaceID != scope.workspaceID {
		return false
	}
	if row.ConversationID != "" {
		if c, err := s.conv.Get(ctx, row.ConversationID); err == nil && c.Operator == scope.agent {
			return true
		}
	}
	if row.RunID != "" {
		if row.RunID == scope.runID {
			return true
		}
		if run, err := s.runs.Get(ctx, row.RunID); err == nil && run.Job == scope.agent {
			return true
		}
	}
	return false
}

func writeOutboxResult(w http.ResponseWriter, row outbox.Row, err error, conflict string) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, row)
	case errors.Is(err, outbox.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "outbox row not found")
	case errors.Is(err, outbox.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", conflict)
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
