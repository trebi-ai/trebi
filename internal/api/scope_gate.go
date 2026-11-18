package api

import (
	"net/http"
	"strings"
)

type scopePolicy int

const (
	scopeDeny scopePolicy = iota
	scopeHandler
)

type scopeGate struct {
	mux      *http.ServeMux
	policies map[string]scopePolicy
}

func newScopeGate(mux *http.ServeMux) *scopeGate {
	allow := map[string]bool{
		// The handler owns path/resource ownership. The table is the explicit
		// allow-list; every unlisted mutating pattern fails closed.
		"POST /v1/jobs":                                   true,
		"PUT /v1/jobs/{name}":                             true,
		"PATCH /v1/jobs/{name}":                           true,
		"DELETE /v1/jobs/{name}":                          true,
		"POST /v1/jobs/{name}/run":                        true,
		"POST /v1/jobs/{name}/pause":                      true,
		"POST /v1/jobs/{name}/unpause":                    true,
		"POST /v1/jobs/{name}/archive":                    true,
		"POST /v1/jobs/{name}/unarchive":                  true,
		"POST /v1/jobs/{name}/validate":                   true,
		"POST /v1/jobs/validate":                          true,
		"PUT /v1/workspaces/{id}/files/{path...}":         true,
		"DELETE /v1/workspaces/{id}/files/{path...}":      true,
		"POST /v1/workspaces/{id}/uploads":                true,
		"PUT /v1/jobs/{name}/files/{path...}":             true,
		"DELETE /v1/jobs/{name}/files/{path...}":          true,
		"POST /v1/workspaces/{id}/files/move":             true,
		"POST /v1/workspaces/{id}/folders/{path...}":      true,
		"DELETE /v1/workspaces/{id}/folders/{path...}":    true,
		"POST /v1/jobs/{name}/files/move":                 true,
		"POST /v1/jobs/{name}/folders/{path...}":          true,
		"DELETE /v1/jobs/{name}/folders/{path...}":        true,
		"PUT /v1/workspaces/{id}/agents/{name}":           true,
		"PATCH /v1/workspaces/{id}/agents/{name}":         true,
		"POST /v1/workspaces/{id}/agents/{name}/ask":      true,
		"POST /v1/workspaces/{id}/agents/{name}/handoff":  true,
		"POST /v1/workspaces/{id}/agents/{name}/escalate": true,
		"POST /v1/workspaces/{id}/agents/{name}/wake":     true,
		"POST /v1/workspaces/{id}/assistant/messages":     true,
		"POST /v1/skills":                                 true,
		"PUT /v1/skills/{name}":                           true,
		"DELETE /v1/skills/{name}":                        true,
		"POST /v1/skills/{name}/update":                   true,
		"POST /v1/skills/{name}/refresh":                  true,
		"POST /v1/skills/{name}/move":                     true,
		"POST /v1/skills/{name}/archive":                  true,
		"POST /v1/skills/{name}/unarchive":                true,
		"PUT /v1/skills/{name}/files/{path...}":           true,
		"DELETE /v1/skills/{name}/files/{path...}":        true,
		"POST /v1/skills/{name}/files/move":               true,
		"POST /v1/skills/{name}/folders/{path...}":        true,
		"DELETE /v1/skills/{name}/folders/{path...}":      true,
		"POST /v1/runs":                                   true,
		"POST /v1/runs/{id}/kill":                         true,
		"POST /v1/runs/{id}/retry":                        true,
		"POST /v1/runs/{id}/resume":                       true,
		"POST /v1/runs/{id}/replay":                       true,
		"POST /v1/runs/{id}/mark":                         true,
		"POST /v1/runs/{id}/message":                      true,
		"POST /v1/queue/{id}/requeue":                     true,
		"POST /v1/queue/{id}/cancel":                      true,
		"POST /v1/conversations/{id}/messages":            true,
		"POST /v1/email/send":                             true,
		"POST /v1/agent-hook/{event}":                     true,
		// The artifact and operation handlers limit a run to its workspace.
		"POST /v1/artifacts":                             true,
		"PATCH /v1/artifacts/{id}":                       true,
		"POST /v1/artifacts/{id}/versions/{n}/send":      true,
		"POST /v1/artifacts/{id}/refresh":                true,
		"POST /v1/connectors/{ref}/operations/{op}/call": true,
		"POST /v1/connectors/{ref}/mcp":                  true,
		"DELETE /v1/connectors/{ref}/mcp":                true,
		// The state handler limits a run to its own owner and an agent to its owned jobs.
		"POST /v1/state/{owner}/{collection}/query":  true,
		"POST /v1/state/{owner}/{collection}/get":    true,
		"POST /v1/state/{owner}/{collection}/has":    true,
		"POST /v1/state/{owner}/{collection}/rows":   true,
		"POST /v1/state/{owner}/{collection}/delete": true,
		"POST /v1/state/{owner}/{collection}/prune":  true,
		"DELETE /v1/state/{owner}/{collection}":      true,
	}
	// The memory handlers limit an agent to its own ledger and the ledgers
	// of its owned jobs.
	for _, root := range []string{"/v1/jobs/{name}", "/v1/workspaces/{id}/agents/{name}"} {
		for _, route := range []string{
			"PUT /memory", "DELETE /memory", "POST /memory/validate", "POST /memory/migrate",
			"PUT /memory/context", "POST /memory/lessons", "DELETE /memory/lessons/{eid}",
			"POST /memory/log", "DELETE /memory/log/{eid}",
		} {
			method, tail, _ := strings.Cut(route, " ")
			allow[method+" "+root+tail] = true
		}
	}
	policies := make(map[string]scopePolicy, len(allow))
	for pattern := range allow {
		policies[pattern] = scopeHandler
	}
	return &scopeGate{mux: mux, policies: policies}
}

func (g *scopeGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		scope, ok := scopeFrom(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// Owned jobs (agents/<owner>/<job>) route through the /v1/jobs/
		// catch-all, so mux.Handler does not expose a method-specific pattern.
		_, pattern := g.mux.Handler(r)
		if r.Method == http.MethodDelete && agentOwnMarkdown(r.URL.Path, scope.agent) {
			writeForbidden(w, "AGENT.md delete requires a human")
			return
		}
		if pattern == "/v1/jobs/" && allowedJobSubtreeMutation(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if g.policies[pattern] != scopeHandler {
			writeForbidden(w, "agent callers may not change this resource")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func agentOwnMarkdown(path, agent string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 6 || parts[0] != "v1" {
		return false
	}
	if parts[1] == "workspaces" && parts[3] == "files" {
		rel := strings.Join(parts[4:], "/")
		return strings.EqualFold(rel, agent+"/AGENT.md")
	}
	if parts[1] == "jobs" && len(parts) >= 5 && parts[3] == "files" {
		rel := strings.Join(parts[4:], "/")
		return strings.EqualFold(rel, agent+"/AGENT.md")
	}
	return false
}

// allowedJobSubtreeMutation permits the owned-job routes whose handlers
// check ownership: the job itself, memory, files, validate, run, pause,
// unpause, archive, and unarchive.
func allowedJobSubtreeMutation(path string) bool {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(path, "/v1/jobs/"), "/"), "/")
	// agents/<owner>/<job>[/<action>/...]
	if len(parts) < 3 || parts[0] != "agents" {
		return false
	}
	if len(parts) == 3 {
		return true
	}
	switch parts[3] {
	case "memory", "files", "folders", "validate", "run", "pause", "unpause", "archive", "unarchive":
		return true
	}
	return false
}
