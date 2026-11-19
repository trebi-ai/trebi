package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// routeRouting serves the routes files: one file for each connection in a
// workspace.
func (s *Server) routeRouting(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{id}/routing", s.handleRoutingList)
	mux.HandleFunc("GET /v1/workspaces/{id}/connectors/{ref}/routing", s.handleRoutingGet)
	mux.HandleFunc("PATCH /v1/workspaces/{id}/connectors/{ref}/routing", s.handleRoutingSet)
}

// routingConn is the connection that a routes file belongs to.
type routingConn struct{ ID, Slug, Type string }

// routingPatchBody is the body of PATCH .../routing.
type routingPatchBody struct {
	Mode         *string              `json:"mode"`
	Notes        *string              `json:"notes"`
	Unsure       *string              `json:"unsure"`
	NoFit        *string              `json:"no_fit"`
	SureAt       *float64             `json:"sure_at"`
	Routes       []map[string]any     `json:"routes"`
	RemoveRoutes []string             `json:"remove_routes"`
	Rules        *[]map[string]string `json:"rules"`
}

// patch maps the body to the patch of the routing package.
func (b routingPatchBody) patch() (routing.Patch, error) {
	p := routing.Patch{
		Mode: b.Mode, Notes: b.Notes, Unsure: b.Unsure, NoFit: b.NoFit, SureAt: b.SureAt,
		Routes: b.Routes, RemoveRoutes: b.RemoveRoutes,
	}
	if b.Rules == nil {
		return p, nil
	}
	rules := []routing.Rule{}
	for i, m := range *b.Rules {
		var r routing.Rule
		for k, v := range m {
			switch {
			case k == "to":
				r.To = v
			case r.Field != "":
				return p, fmt.Errorf("rule %d has more than one condition", i)
			default:
				r.Field, r.Value = k, v
			}
		}
		rules = append(rules, r)
	}
	p.Rules = &rules
	return p, nil
}

// routingWorkspace finds the workspace of a route by id or name.
func (s *Server) routingWorkspace(w http.ResponseWriter, r *http.Request) (*workspaces.Workspace, bool) {
	ws, err := s.ws.Resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
		} else {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return nil, false
	}
	return ws, true
}

// routingConnection finds the connection of {ref}. A native name is its own
// connection.
func (s *Server) routingConnection(ref string) (routingConn, error) {
	if manifest.Reserved(ref) {
		return routingConn{ID: ref, Slug: ref, Type: ref}, nil
	}
	c, err := s.connectors.Lookup(ref)
	if err != nil {
		return routingConn{}, err
	}
	return routingConn{ID: c.ID, Slug: c.Slug, Type: c.Type}, nil
}

func (s *Server) handleRoutingGet(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.routingWorkspace(w, r)
	if !ok {
		return
	}
	conn, err := s.routingConnection(r.PathValue("ref"))
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	s.writeRoutingView(w, r, ws, conn)
}

func (s *Server) handleRoutingSet(w http.ResponseWriter, r *http.Request) {
	if rejectAnyAgentScope(w, r) {
		return
	}
	ws, ok := s.routingWorkspace(w, r)
	if !ok {
		return
	}
	conn, err := s.routingConnection(r.PathValue("ref"))
	if err != nil {
		s.writeAnyConnectorErr(w, err)
		return
	}
	var body routingPatchBody
	if !decodeBody(w, r, &body) {
		return
	}
	patch, err := body.patch()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	_, err = s.ws.SetRouting(r.Context(), ws.ID, conn.Slug, patch, ifMatchHeader(r))
	var ve *routing.ValidationError
	switch {
	case errors.Is(err, workspaces.ErrPrecondition):
		writeErr(w, http.StatusPreconditionFailed, "precondition_failed", "the routes file changed; read it again and retry")
	case errors.As(err, &ve):
		s.addFixWorkspace(ws.ID, ve.Problems)
		writeErrDetails(w, http.StatusBadRequest, "validation", ve.Error(), map[string]any{"problems": ve.Problems})
	case errors.Is(err, workspaces.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "workspace not found")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	default:
		s.writeRoutingView(w, r, ws, conn)
	}
}

func (s *Server) handleRoutingList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.routingWorkspace(w, r)
	if !ok {
		return
	}
	files, err := s.ws.RoutingFiles(r.Context(), ws.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	targets, err := s.ws.RouteTargets(r.Context(), ws.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	models := s.routerModels(r.Context(), ws.ID)
	out := []map[string]any{}
	for _, c := range s.routingConnections(ws.ID, files) {
		f, _ := routing.Load(ws.Path, c.Slug)
		out = append(out, s.routingView(ws, c, f, targets, models))
	}
	writeJSON(w, http.StatusOK, map[string]any{"routing": out})
}

// routingConnections lists the connections that the workspace can see, and
// the native connections that have a routes file.
func (s *Server) routingConnections(wsID string, files []routing.File) []routingConn {
	var out []routingConn
	for _, row := range s.connectors.List(wsID) {
		slug, _ := row["slug"].(string)
		id, _ := row["id"].(string)
		typ, _ := row["type"].(string)
		if _, off := row["disabled"].(bool); off || row["in_scope"] == false || slug == "" || row["load_error"] != nil || s.connectors.IDOf(slug) != id {
			continue
		}
		out = append(out, routingConn{ID: id, Slug: slug, Type: typ})
	}
	for _, f := range files {
		if manifest.Reserved(f.Connection) && !slices.ContainsFunc(out, func(c routingConn) bool { return c.Slug == f.Connection }) {
			out = append(out, routingConn{ID: f.Connection, Slug: f.Connection, Type: f.Connection})
		}
	}
	return out
}

// writeRoutingView answers with the view of one routes file.
func (s *Server) writeRoutingView(w http.ResponseWriter, r *http.Request, ws *workspaces.Workspace, conn routingConn) {
	f, err := s.ws.Routing(r.Context(), ws.ID, conn.Slug)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	targets, err := s.ws.RouteTargets(r.Context(), ws.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	setETagHash(w, f.Hash)
	writeJSON(w, http.StatusOK, s.routingView(ws, conn, f, targets, s.routerModels(r.Context(), ws.ID)))
}

// routingView builds the JSON of one routes file.
func (s *Server) routingView(ws *workspaces.Workspace, c routingConn, f routing.File, targets map[string]routing.Target, models []agentcli.ModelInfo) map[string]any {
	routes := make([]map[string]any, 0, len(f.Routes))
	for _, r := range f.Routes {
		routes = append(routes, routeView(r, targets))
	}
	rules := make([]map[string]any, 0, len(f.Rules))
	for _, r := range f.Rules {
		rules = append(rules, map[string]any{"field": r.Field, "value": r.Value, "to": r.To, "line": r.Line})
	}
	return map[string]any{
		"workspace":  ws.ID,
		"connection": map[string]any{"id": c.ID, "slug": c.Slug, "type": c.Type},
		"path":       routing.Dir + "/" + c.Slug + ".yaml",
		"exists":     f.Exists, "hash": f.Hash,
		"mode": f.Mode, "notes": f.Notes, "unsure": f.Unsure, "no_fit": f.NoFit, "sure_at": f.SureAt,
		"routes": routes, "rules": rules,
		"problems": s.routingProblems(ws.ID, f, targets),
		"models":   models,
	}
}

// routeView is the JSON of one route.
func routeView(r routing.Route, targets map[string]routing.Target) map[string]any {
	kind := "agent"
	switch {
	case r.Assistant:
		kind = routing.Assistant
	case r.Job != "":
		kind = "job"
	}
	target := map[string]any{"kind": kind, "name": r.TargetName(), "index": r.IndexName(), "exists": false, "active": false, "description": ""}
	if t, ok := targets[r.TargetName()]; ok {
		target["exists"], target["active"], target["description"] = true, t.Active && !t.Archived, t.Description
	}
	inputs := r.Inputs
	if inputs == nil {
		inputs = job.InputMap{}
	}
	return map[string]any{
		"id": r.ID, "target": target, "gets": r.Gets, "use_for": r.UseFor,
		"filters": map[string]any{
			"rooms": orEmpty(r.Filter.Rooms), "from": r.Filter.From, "contains": orEmpty(r.Filter.Contains),
			"allowlist": orEmpty(r.Filter.Allowlist), "match": r.Filter.Match, "extract": r.Filter.Extract,
			"types": orEmpty(r.Filter.Types), "replies": r.Filter.Replies, "batch": r.Filter.Batch,
			"chat": r.Filter.Chat, "limit": r.Filter.Limit,
		},
		"inputs": inputs, "line": r.Line,
	}
}

func orEmpty[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

// routingProblems lists the faults of a routes file, sorted by line. Each
// fix names the workspace.
func (s *Server) routingProblems(wsID string, f routing.File, targets map[string]routing.Target) []routing.Problem {
	out := slices.Concat(f.Problems, routing.Check(f, targets), s.connectors.RouteIssues(wsID, f, targets))
	slices.SortStableFunc(out, func(a, b routing.Problem) int { return a.Line - b.Line })
	s.addFixWorkspace(wsID, out)
	return orEmpty(out)
}

// addFixWorkspace puts the workspace in the input of each fix.
func (s *Server) addFixWorkspace(wsID string, problems []routing.Problem) {
	for _, p := range problems {
		if p.Fix != nil {
			p.Fix.Input["workspace"] = wsID
		}
	}
}

// routerModels lists the model choices of the router: the assistant model
// first, then the models of the assistant harness.
func (s *Server) routerModels(ctx context.Context, wsID string) []agentcli.ModelInfo {
	out := []agentcli.ModelInfo{{ID: "", Label: "Default (assistant model)", Default: true}}
	if s.asstResolve == nil {
		return out
	}
	eff, err := s.asstResolve.Effective(ctx, wsID)
	if err != nil || eff.Resolved == "" {
		return out
	}
	cfg, _ := config.Load()
	return append(out, agentcli.ListModels(ctx, cfg.HarnessBase(eff.Resolved)).Models...)
}

// targetRoutes lists the routes of one job or agent for its show answer.
func (s *Server) targetRoutes(ctx context.Context, wsID, name string) []map[string]any {
	out := []map[string]any{}
	files, err := s.ws.RoutingFiles(ctx, wsID)
	if err != nil {
		s.log.Warn("routes read failed", "workspace", wsID, "error", err)
		return out
	}
	targets, err := s.ws.RouteTargets(ctx, wsID)
	if err != nil {
		s.log.Warn("route targets failed", "workspace", wsID, "error", err)
		return out
	}
	name = strings.TrimPrefix(name, "agents/")
	for _, f := range files {
		problems := s.routingProblems(wsID, f, targets)
		for _, r := range f.Routes {
			if r.TargetName() != name {
				continue
			}
			v := routeView(r, targets)
			mine := []routing.Problem{}
			for _, p := range problems {
				if p.Route == r.ID && r.ID != "" {
					mine = append(mine, p)
				}
			}
			out = append(out, map[string]any{
				"connection": f.Connection, "id": r.ID, "gets": r.Gets, "use_for": r.UseFor,
				"filters": v["filters"], "problems": mine,
			})
		}
	}
	return out
}
