package api

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/trebi-ai/trebi/internal/chains"
)

func (s *Server) routeChains(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/chains", s.handleChainList)
	mux.HandleFunc("GET /v1/chains/{trace_id}", s.handleChainGet)
	mux.HandleFunc("GET /v1/hops/{id}", s.handleHopGet)
}

func (s *Server) handleChainList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := parseTimeFilter(q.Get("since"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "since: "+err.Error())
		return
	}
	cursor, ok := parseULIDCursor(w, r, "trace")
	if !ok {
		return
	}
	f := chains.ListFilter{
		Workspace: s.workspaceID(r.Context(), q.Get("workspace")), Status: q.Get("status"),
		Agent: q.Get("agent"), Live: q.Get("live") == "true" || q.Get("live") == "1",
		Limit: parseLimit(r), Cursor: cursor,
	}
	if since != "" {
		f.Since, _ = time.Parse(time.RFC3339, since) // parseTimeFilter wrote RFC3339
	}
	list, next, err := s.chains.List(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := map[string]any{"chains": list}
	if next != "" {
		resp["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleChainGet takes a trace ID, or a run ID that resolves to the trace of
// its latest chain turn.
func (s *Server) handleChainGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("trace_id")
	c, hops, err := s.chains.Get(ctx, id)
	if errors.Is(err, chains.ErrNotFound) {
		if rc, rerr := s.chains.ForRun(ctx, id); rerr == nil && rc != nil {
			c, hops, err = s.chains.Get(ctx, rc.TraceID)
		}
	}
	if errors.Is(err, chains.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "chain not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	list, err := s.chains.Runs(ctx, c.TraceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chain": c, "hops": hops, "runs": list})
}

func (s *Server) handleHopGet(w http.ResponseWriter, r *http.Request) {
	h, err := s.chains.Hop(r.Context(), r.PathValue("id"))
	if errors.Is(err, chains.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "hop not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hop": h})
}

// writeChainErr maps a chains error to its HTTP status.
func writeChainErr(w http.ResponseWriter, err error) {
	var ce *chains.Error
	if !errors.As(err, &ce) {
		if errors.Is(err, chains.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	code := http.StatusConflict
	switch ce.Code {
	case chains.CodeValidation:
		code = http.StatusBadRequest
	case chains.CodeForbidden:
		code = http.StatusForbidden
	case chains.CodeNotFound:
		code = http.StatusNotFound
	case chains.CodeRate:
		code = http.StatusTooManyRequests
	}
	if ce.HopID == "" && ce.Reason == "" {
		writeErr(w, code, ce.Code, ce.Message)
		return
	}
	writeErrDetails(w, code, ce.Code, ce.Message, map[string]string{
		"refuse_reason": string(ce.Reason), "hop_id": ce.HopID, "trace_id": ce.TraceID,
	})
}
