package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/pauses"
	"github.com/trebi-ai/trebi/internal/tools"
)

func (s *Server) routePauses(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/pauses", s.handlePausesList)
	mux.HandleFunc("POST /v1/pauses", s.handlePauseSet)
	mux.HandleFunc("GET /v1/pauses/{id}", s.handlePauseGet)
	mux.HandleFunc("PATCH /v1/pauses/{id}", s.handlePauseUpdate)
	mux.HandleFunc("POST /v1/pauses/{id}/resume", s.handlePauseResume)
}

// activePauses returns the open pauses, or none when the read fails.
func (s *Server) activePauses(ctx context.Context) pauses.Active {
	if s.pauses == nil {
		return nil
	}
	active, err := s.pauses.Active(ctx)
	if err != nil {
		s.log.Warn("read pauses", "error", err)
		return nil
	}
	return active
}

// requirePauseWriter refuses a job or agent run: only a person or the
// assistant pauses work.
func requirePauseWriter(w http.ResponseWriter, r *http.Request) bool {
	if a, ok := tools.ActorFrom(r.Context()); ok && (a.Kind == "job" || a.Kind == "agent") {
		writeForbidden(w, "a run cannot pause or resume work")
		return false
	}
	return true
}

func pauseCreator(r *http.Request) string {
	if a, ok := tools.ActorFrom(r.Context()); ok && a.Kind != "" {
		return a.Kind
	}
	return "user"
}

func writePauseErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pauses.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	case errors.Is(err, pauses.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, pauses.ErrEnded):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) handlePausesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := s.pauses.List(r.Context(), pauses.ListOpts{All: q.Get("all") == "true", Limit: limit})
	if err != nil {
		writePauseErr(w, err)
		return
	}
	if list == nil {
		list = []pauses.Pause{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pauses": list})
}

func (s *Server) handlePauseGet(w http.ResponseWriter, r *http.Request) {
	p, err := s.pauses.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writePauseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// pauseBody is the body of POST /v1/pauses and PATCH /v1/pauses/{id}.
// Until is RFC 3339, "reset", or "" for no end; For is a length such as
// "3h" or "2d". A new pause with no Until and no For has no end.
type pauseBody struct {
	Scope     pauses.Scope  `json:"scope"`
	Target    string        `json:"target"`
	Workspace string        `json:"workspace"`
	Until     *string       `json:"until"`
	For       string        `json:"for"`
	Missed    pauses.Missed `json:"missed"`
	Note      *string       `json:"note"`
}

// end reads Until and For. set is false when the body names no end.
func (s *Server) end(ctx context.Context, b pauseBody, scope pauses.Scope, target string) (until *time.Time, set bool, err error) {
	now := time.Now().UTC()
	if b.For != "" {
		if b.Until != nil && *b.Until != "" {
			return nil, false, errors.New("set until or for, not both")
		}
		d, err := pauses.ParseFor(b.For)
		if err != nil {
			return nil, false, err
		}
		t := now.Add(d).Truncate(time.Second)
		return &t, true, nil
	}
	if b.Until == nil {
		return nil, false, nil
	}
	v := strings.TrimSpace(*b.Until)
	switch v {
	case "":
		return nil, true, nil
	case "reset":
		if scope != pauses.ScopeHarness {
			return nil, false, errors.New("until reset needs a harness pause")
		}
		t := s.harnessLimit(ctx, target).Reset(now)
		if t.IsZero() {
			return nil, false, errors.New("the usage limit reset time of " + target + " is not known; set a time")
		}
		return &t, true, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, false, errors.New("until must be an RFC 3339 time or reset")
	}
	return &t, true, nil
}

// harnessLimit reads the usage windows and the hard limit of one harness.
func (s *Server) harnessLimit(ctx context.Context, name string) pauses.Limit {
	l := pauses.Limit{Harness: name}
	if usage, err := harness.NewUsageStore(s.st).List(ctx); err == nil {
		for _, u := range usage {
			if u.Harness == name {
				l.Windows = u.Windows
			}
		}
	}
	_ = s.st.DB().QueryRowContext(ctx, `SELECT limit_exceeded = 1 FROM harness_state WHERE name = ?`, name).Scan(&l.Hit) // no row: no hard limit
	return l
}

// target resolves the target of a pause to its stored form: a workspace
// ID, a harness name, or an agent ID.
func (s *Server) target(ctx context.Context, b pauseBody) (string, int, error) {
	ref := strings.TrimSpace(b.Target)
	switch b.Scope {
	case pauses.ScopeAll:
		return "", 0, nil
	case pauses.ScopeWorkspace:
		ws, err := s.ws.Resolve(ctx, ref)
		if err != nil {
			return "", http.StatusNotFound, errors.New("workspace not found")
		}
		return ws.ID, 0, nil
	case pauses.ScopeHarness:
		if !harness.Known(ref) {
			return "", http.StatusNotFound, errors.New("unknown harness")
		}
		return ref, 0, nil
	case pauses.ScopeAgent:
		wsID := ""
		if b.Workspace != "" {
			ws, err := s.ws.Resolve(ctx, b.Workspace)
			if err != nil {
				return "", http.StatusNotFound, errors.New("workspace not found")
			}
			wsID = ws.ID
		}
		if wsID == "" {
			if row, err := s.ws.JobByID(ctx, ref); err == nil && row.Kind == "agent" {
				return row.ID, 0, nil
			}
			return "", http.StatusBadRequest, errors.New("an agent name needs a workspace")
		}
		row, err := s.findAgentRow(ctx, wsID, ref)
		if err != nil {
			return "", http.StatusNotFound, errors.New("agent not found")
		}
		return row.ID, 0, nil
	}
	return "", http.StatusBadRequest, errors.New("scope must be all, workspace, harness, or agent")
}

func (s *Server) handlePauseSet(w http.ResponseWriter, r *http.Request) {
	if !requirePauseWriter(w, r) {
		return
	}
	var b pauseBody
	if !decodeBody(w, r, &b) {
		return
	}
	target, code, err := s.target(r.Context(), b)
	if err != nil {
		name := "validation"
		if code == http.StatusNotFound {
			name = "not_found"
		}
		writeErr(w, code, name, err.Error())
		return
	}
	until, _, err := s.end(r.Context(), b, b.Scope, target)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	note := ""
	if b.Note != nil {
		note = *b.Note
	}
	p, created, err := s.pauses.Set(r.Context(), pauses.SetOpts{Scope: b.Scope, Target: target, Until: until,
		Missed: b.Missed, Note: note, CreatedBy: pauseCreator(r)})
	if err != nil {
		writePauseErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, p)
}

func (s *Server) handlePauseUpdate(w http.ResponseWriter, r *http.Request) {
	if !requirePauseWriter(w, r) {
		return
	}
	var b pauseBody
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Scope != "" || b.Target != "" || b.Workspace != "" {
		writeErr(w, http.StatusBadRequest, "validation", "the scope of a pause cannot change; resume it and pause again")
		return
	}
	cur, err := s.pauses.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writePauseErr(w, err)
		return
	}
	until, set, err := s.end(r.Context(), b, cur.Scope, cur.Target)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	o := pauses.UpdateOpts{Note: b.Note}
	if set {
		o.Until, o.ClearUntil = until, until == nil
	}
	if b.Missed != "" {
		o.Missed = &b.Missed
	}
	p, err := s.pauses.Update(r.Context(), cur.ID, o)
	if err != nil {
		writePauseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handlePauseResume(w http.ResponseWriter, r *http.Request) {
	if !requirePauseWriter(w, r) {
		return
	}
	p, err := s.pauses.Resume(r.Context(), r.PathValue("id"))
	if err != nil {
		writePauseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
