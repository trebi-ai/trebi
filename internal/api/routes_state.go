package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/state"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// routeState mounts the state store of jobs and agents (plan 2026-10-06 B4).
// {owner} is a job ID, an agent ID, "self", or a name with ?workspace=.
// Keys can hold "/" and ":", so they go in the body.
func (s *Server) routeState(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/state/{owner}", s.handleStateSummary)
	mux.HandleFunc("POST /v1/state/{owner}/{collection}/query", s.handleStateQuery)
	mux.HandleFunc("POST /v1/state/{owner}/{collection}/get", s.handleStateGet)
	mux.HandleFunc("POST /v1/state/{owner}/{collection}/has", s.handleStateHas)
	mux.HandleFunc("POST /v1/state/{owner}/{collection}/rows", s.handleStatePut)
	mux.HandleFunc("POST /v1/state/{owner}/{collection}/delete", s.handleStateDelete)
	mux.HandleFunc("POST /v1/state/{owner}/{collection}/prune", s.handleStatePrune)
	mux.HandleFunc("DELETE /v1/state/{owner}/{collection}", s.handleStateDrop)
}

// stateKeys is the body of get, has, and delete.
type stateKeys struct {
	Keys []string `json:"keys"`
}

// stateRows is the body of a batch write.
type stateRows struct {
	Rows []state.Row `json:"rows"`
	Mode state.Mode  `json:"mode"`
}

// stateRange is the body of prune.
type stateRange struct {
	Since string `json:"since"`
	Until string `json:"until"`
	By    string `json:"by"`
}

// stateTarget is one resolved state request.
type stateTarget struct {
	owner state.Owner
	row   *workspaces.JobRow
}

// errStateSelf is a "self" owner with no run actor.
var errStateSelf = errors.New(`owner "self" needs a run; pass a job or agent ID, or a name with workspace`)

// stateTarget resolves {owner} once at the edge. A run actor may only act on
// the state of its own job or agent, or of a job that its agent owns. It
// writes the error and returns false.
func (s *Server) stateTarget(w http.ResponseWriter, r *http.Request) (stateTarget, bool) {
	ref := r.PathValue("owner")
	actor, _ := tools.ActorFrom(r.Context())
	inRun := actor.RunID != "" && actor.JobID != ""
	agent, isAgent := scopeFrom(r.Context())
	if ref == "self" {
		if !inRun {
			writeErr(w, http.StatusBadRequest, "validation", errStateSelf.Error())
			return stateTarget{}, false
		}
		ref = actor.JobID
	}
	row, err := s.ws.ResolveJob(r.Context(), s.workspaceID(r.Context(), r.URL.Query().Get("workspace")), ref)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", "no job or agent "+ref)
		return stateTarget{}, false
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return stateTarget{}, false
	case row.Kind != "job" && row.Kind != "agent":
		writeErr(w, http.StatusBadRequest, "validation", "state belongs to a job or an agent, not to "+row.Kind+" "+row.Name)
		return stateTarget{}, false
	case inRun && row.ID != actor.JobID && (!isAgent || !agent.canWriteJob(*row)):
		writeErr(w, http.StatusForbidden, "state_scope", "a run reads and writes only the state of its own job or agent, or of a job that its agent owns; use owner self")
		return stateTarget{}, false
	}
	o := state.Owner{ID: row.ID, WorkspaceID: row.WorkspaceID}
	if inRun {
		o.RunID = actor.RunID
	}
	if j, err := job.ParseFile(row.Path); err == nil {
		o.Collections = j.State
	}
	return stateTarget{owner: o, row: row}, true
}

// writeStateErr maps state errors to the envelope.
func writeStateErr(w http.ResponseWriter, err error) {
	var be *state.BatchError
	switch {
	case errors.As(err, &be):
		writeErrDetails(w, http.StatusUnprocessableEntity, "state_invalid", be.Error(), map[string]any{"errors": be.Rows})
	case errors.Is(err, state.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// changed emits state.changed for one write that changed rows.
func (s *Server) stateChanged(ctx context.Context, t stateTarget, coll string, inserted, updated, deleted int) {
	if inserted+updated+deleted == 0 {
		return
	}
	s.appendBus(ctx, store.BusEvent{Topic: "state.changed", Attrs: map[string]any{
		"owner_id": t.owner.ID, "collection": coll, "run_id": t.owner.RunID,
		"inserted": inserted, "updated": updated, "deleted": deleted,
	}})
}

func (s *Server) handleStateSummary(w http.ResponseWriter, r *http.Request) {
	t, ok := s.stateTarget(w, r)
	if !ok {
		return
	}
	stats, err := s.state.Collections(r.Context(), t.owner)
	if err != nil {
		writeStateErr(w, err)
		return
	}
	rows, size := 0, int64(0)
	for _, c := range stats {
		rows += c.Rows
		size += c.Bytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"owner_id": t.row.ID, "owner_name": t.row.Name, "kind": t.row.Kind, "workspace_id": t.row.WorkspaceID,
		"collections": stats, "rows": rows, "bytes": size,
	})
}

func (s *Server) handleStateQuery(w http.ResponseWriter, r *http.Request) {
	t, ok := s.stateTarget(w, r)
	if !ok {
		return
	}
	var q state.Query
	if !decodeBody(w, r, &q) {
		return
	}
	q.Collection = r.PathValue("collection")
	res, err := s.state.Query(r.Context(), t.owner, q)
	if err != nil {
		writeStateErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleStateGet(w http.ResponseWriter, r *http.Request) {
	t, ok := s.stateTarget(w, r)
	if !ok {
		return
	}
	var body stateKeys
	if !decodeBody(w, r, &body) {
		return
	}
	rows, err := s.state.Get(r.Context(), t.owner, r.PathValue("collection"), body.Keys)
	if err != nil {
		writeStateErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (s *Server) handleStateHas(w http.ResponseWriter, r *http.Request) {
	t, ok := s.stateTarget(w, r)
	if !ok {
		return
	}
	var body stateKeys
	if !decodeBody(w, r, &body) {
		return
	}
	found, err := s.state.Has(r.Context(), t.owner, r.PathValue("collection"), body.Keys)
	if err != nil {
		writeStateErr(w, err)
		return
	}
	missing := []string{}
	for _, k := range body.Keys {
		if _, ok := found[k]; !ok {
			missing = append(missing, k)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": found, "missing": missing})
}

func (s *Server) handleStatePut(w http.ResponseWriter, r *http.Request) {
	t, ok := s.stateTarget(w, r)
	if !ok {
		return
	}
	var body stateRows
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		msg := err.Error()
		// The data of a row goes in value, so a data field at the top of a
		// row is the usual cause.
		if f, ok := strings.CutPrefix(msg, "json: unknown field "); ok {
			msg = "field " + f + " is not a row field. A row has key, at, value, and inc. " +
				`Put the data in value: {"key": "<key>", "value": {` + f + `: ...}}`
		}
		writeErr(w, http.StatusBadRequest, "validation", msg)
		return
	}
	coll := r.PathValue("collection")
	res, err := s.state.Put(r.Context(), t.owner, coll, body.Rows, body.Mode)
	if err != nil {
		writeStateErr(w, err)
		return
	}
	s.stateChanged(r.Context(), t, coll, res.Inserted, res.Updated, 0)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleStateDelete(w http.ResponseWriter, r *http.Request) {
	t, ok := s.stateTarget(w, r)
	if !ok {
		return
	}
	var body stateKeys
	if !decodeBody(w, r, &body) {
		return
	}
	coll := r.PathValue("collection")
	n, err := s.state.Delete(r.Context(), t.owner, coll, body.Keys)
	s.writeStateDeleted(w, r, t, coll, n, err)
}

func (s *Server) handleStatePrune(w http.ResponseWriter, r *http.Request) {
	t, ok := s.stateTarget(w, r)
	if !ok {
		return
	}
	var body stateRange
	if !decodeBody(w, r, &body) {
		return
	}
	rng, err := s.state.Range(r.Context(), t.owner, body.Since, body.Until, body.By)
	if err != nil {
		writeStateErr(w, err)
		return
	}
	coll := r.PathValue("collection")
	n, err := s.state.Prune(r.Context(), t.owner, coll, rng)
	s.writeStateDeleted(w, r, t, coll, n, err)
}

func (s *Server) handleStateDrop(w http.ResponseWriter, r *http.Request) {
	t, ok := s.stateTarget(w, r)
	if !ok {
		return
	}
	coll := r.PathValue("collection")
	n, err := s.state.Drop(r.Context(), t.owner, coll)
	s.writeStateDeleted(w, r, t, coll, n, err)
}

// writeStateDeleted answers delete, prune, and drop.
func (s *Server) writeStateDeleted(w http.ResponseWriter, r *http.Request, t stateTarget, coll string, n int, err error) {
	if err != nil {
		writeStateErr(w, err)
		return
	}
	s.stateChanged(r.Context(), t, coll, 0, 0, n)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}
