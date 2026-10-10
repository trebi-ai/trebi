package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/requests"
	"github.com/trebi-ai/trebi/internal/tools"
)

func (s *Server) routeConnectorOperations(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/connectors/{ref}/operations", s.handleOperationsList)
	mux.HandleFunc("POST /v1/connectors/{ref}/operations/{op}/call", s.handleOperationCall)
	mux.HandleFunc("PUT /v1/connectors/{ref}/operations/{op}", s.handleOperationState)
}

// operations builds the operations runner over the current services.
func (s *Server) operations() *connectors.Operations {
	return connectors.NewOperations(s.connectors, s.runs, opAsker{s})
}

func writeOperationErr(w http.ResponseWriter, s *Server, err error) {
	var appr *connectors.ApprovalError
	var call *connectors.CallError
	var in *manifest.InputError
	switch {
	case errors.As(err, &appr):
		writeErrDetails(w, http.StatusConflict, "approval_required", err.Error(), map[string]any{"request_id": appr.RequestID})
	case errors.As(err, &call):
		writeErrDetails(w, http.StatusBadGateway, "operation_failed", err.Error(),
			map[string]any{"exit_code": call.ExitCode, "stderr": call.Stderr})
	case errors.As(err, &in):
		writeErrDetails(w, http.StatusUnprocessableEntity, "invalid", err.Error(), map[string]any{"fields": in.Fields})
	case errors.Is(err, connectors.ErrNoOperation):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, connectors.ErrOperationOff):
		writeErr(w, http.StatusConflict, "operation_off", err.Error())
	case errors.Is(err, connectors.ErrNotReadOnly):
		writeErr(w, http.StatusConflict, "not_readonly", err.Error())
	case errors.Is(err, connectors.ErrConnectorNotKit):
		writeForbidden(w, err.Error())
	case errors.Is(err, connectors.ErrOperationTooLong):
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	default:
		if !s.writeConnectorErr(w, err) {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		}
	}
}

func (s *Server) handleOperationsList(w http.ResponseWriter, r *http.Request) {
	ws := r.URL.Query().Get("workspace")
	if a, ok := tools.ActorFrom(r.Context()); ok && a.WorkspaceID != "" {
		ws = a.WorkspaceID
	}
	list, err := s.operations().List(r.Context(), ws, r.PathValue("ref"))
	if err != nil {
		writeOperationErr(w, s, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleOperationCall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Workspace string         `json:"workspace"`
		Input     map[string]any `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	c := connectors.CallOpts{Workspace: body.Workspace, Connection: r.PathValue("ref"), Operation: r.PathValue("op"), Input: body.Input}
	if a, ok := tools.ActorFrom(r.Context()); ok {
		if a.WorkspaceID != "" {
			c.Workspace = a.WorkspaceID
		}
		c.RunID = a.RunID
		c.Agent = a.Kind != tools.ActorMobile
	}
	res, err := s.operations().Call(r.Context(), c)
	if err != nil {
		writeOperationErr(w, s, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleOperationState(w http.ResponseWriter, r *http.Request) {
	if a, ok := tools.ActorFrom(r.Context()); ok && a.Kind != tools.ActorMobile {
		writeForbidden(w, "only the user can change the state of an operation")
		return
	}
	var body struct {
		State manifest.OpState `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	ops := s.operations()
	if err := ops.SetState(r.Context(), r.PathValue("ref"), r.PathValue("op"), body.State); err != nil {
		writeOperationErr(w, s, err)
		return
	}
	list, err := ops.List(r.Context(), "", r.PathValue("ref"))
	if err != nil {
		writeOperationErr(w, s, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

// opAsker turns a call in state ask into a confirm request of the run.
type opAsker struct{ s *Server }

func (a opAsker) Approved(ctx context.Context, runID, target string) (bool, error) {
	list, _, err := a.s.requests.List(ctx, requests.ListOpts{RunID: runID, Status: requests.StatusAnswered, Limit: 100})
	if err != nil {
		return false, err
	}
	for _, r := range list {
		for _, f := range r.Forms {
			if f.Target == target && f.Answer != nil && f.Answer.Decision == requests.DecisionApprove {
				return true, nil
			}
		}
	}
	return false, nil
}

func (a opAsker) Ask(ctx context.Context, runID, target, title, summary string) (string, error) {
	r, err := a.s.requests.CreateSystem(ctx, runID, requests.CreateOpts{
		Title: title, Summary: summary,
		Forms: []requests.FormOpts{{Type: requests.TypeConfirm, Title: title, Summary: summary, Target: target,
			Allow: []requests.Decision{requests.DecisionApprove, requests.DecisionReject}}},
	})
	return r.ID, err
}
