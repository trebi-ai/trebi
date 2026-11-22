package api

import (
	"net/http"
	"strings"

	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/tools"
)

// confirmTarget names the object a destructive call removes. Path values come
// from the matched pattern; the mux fills them only after this gate runs, so
// the gate reads the segments itself.
type confirmTarget struct {
	segment int // index of the object name in the request path, -1 for none
	// describe nil: the handler asks for confirm, because its text needs the
	// resolved object. The gate only lists the route.
	describe func(name string, r *http.Request) string
	runOK    bool // a run acts on its own scope with no confirm
}

// confirmGate holds the destructive routes that need `?confirm=1`. The gate
// lives in the API so the CLI, the UI, and MCP share one rule.
type confirmGate struct {
	routes map[string]confirmTarget
}

func newConfirmGate() *confirmGate {
	return &confirmGate{routes: map[string]confirmTarget{
		"DELETE /v1/workspaces/{id}": {2, func(n string, r *http.Request) string {
			if r.URL.Query().Get("delete_folder") == "1" {
				return "removes workspace " + n + " from Trebi and deletes its folder on disk; run rows stay"
			}
			return "removes workspace " + n + " from Trebi; its folder stays on disk and run rows stay"
		}, false},
		"POST /v1/workspaces/{id}/move": {2, func(n string, r *http.Request) string {
			if m := moveTarget(r); m != "" {
				return "moves workspace " + n + " with its history and secrets to machine " + m + "; this machine keeps a read-only copy"
			}
			return "moves workspace " + n + " and rewrites absolute paths in its files"
		}, false},
		"POST /v1/workspaces/{id}/email/rotate": {2, func(n string, _ *http.Request) string {
			return "retires the email address of workspace " + n +
				"; mail to the old address stops arriving and open threads break"
		}, false},
		// The job handler asks, because its text lists the routes it removes.
		"DELETE /v1/jobs/{name}": {2, nil, false},
		"DELETE /v1/jobs/":       {-1, nil, false},
		"DELETE /v1/artifacts/{id}": {2, func(n string, _ *http.Request) string {
			return "deletes artifact " + n + " and every version of it"
		}, false},
		"DELETE /v1/jobs/{name}/memory": {2, func(n string, _ *http.Request) string {
			return "erases the memory file of job " + n
		}, false},
		"DELETE /v1/workspaces/{id}/assistant/memory": {2, func(n string, _ *http.Request) string {
			return "erases the assistant memory of workspace " + n
		}, false},
		"DELETE /v1/workspaces/{id}/agents/{name}": {4, nil, false},
		"DELETE /v1/workspaces/{id}/agents/{name}/memory": {4, func(n string, _ *http.Request) string {
			return "erases the memory of agent " + n
		}, false},
		"DELETE /v1/harnesses/{name}/files/{key}/own": {4, func(k string, r *http.Request) string {
			return "replaces Trebi's copy of " + pathSegment(r.URL.Path, 2) + " file " + k + " with your file; the copy goes to a backup for 30 days"
		}, false},
		"DELETE /v1/secrets/{name}": {2, func(n string, _ *http.Request) string {
			return "deletes secret " + n + "; jobs that read it will fail"
		}, false},
		"DELETE /v1/connectors/{ref}": {2, func(n string, _ *http.Request) string {
			return "deletes connection " + n + " with its folder, login, and cursor, and stops its event stream"
		}, false},
		"DELETE /v1/skills/{name}": {2, func(n string, _ *http.Request) string {
			return "deletes skill " + n + " and its folder"
		}, false},
		"DELETE /v1/workspaces/{id}/files/{path...}": {-1, func(_ string, r *http.Request) string {
			return "deletes " + r.URL.Path[strings.Index(r.URL.Path, "/files/")+len("/files/"):] + " from the workspace"
		}, false},
		"DELETE /v1/workspaces/{id}/folders/{path...}": {-1, nil, false},
		"DELETE /v1/jobs/{name}/folders/{path...}":     {-1, nil, false},
		"DELETE /v1/skills/{name}/folders/{path...}":   {-1, nil, false},
		"POST /v1/system/shutdown": {-1, func(string, *http.Request) string {
			return "stops the trebi daemon; scheduled jobs stop firing until it starts again"
		}, false},
		"POST /v1/system/restart": {-1, func(string, *http.Request) string {
			return "restarts Trebi; live runs and assistant sessions stop and scheduled jobs wait until it is back"
		}, false},
		"DELETE /v1/cloud/link": {-1, func(string, *http.Request) string {
			return "unlinks this daemon from the cloud; remote clients lose access"
		}, false},
		"POST /v1/chains/{trace_id}/stop": {2, func(n string, _ *http.Request) string {
			return "stops chain " + n + "; its live runs end and each waiting agent gets one turn to finish"
		}, false},
		"DELETE /v1/mobile/clients/{id}": {3, func(n string, _ *http.Request) string {
			return "removes phone " + n + "; it loses access to this machine and must pair again"
		}, false},
		"DELETE /v1/machines/{id}/link": {2, func(n string, _ *http.Request) string {
			return "disconnects from machine " + n + "; it must make a new pairing link to connect again"
		}, false},
		"DELETE /v1/hooks/{id}": {2, func(n string, _ *http.Request) string {
			return "revokes hook " + n + "; deliveries to its URL stop"
		}, false},
		"POST /v1/state/{owner}/{collection}/delete": {3, func(n string, _ *http.Request) string {
			return "deletes the given rows of state collection " + n
		}, true},
		"POST /v1/state/{owner}/{collection}/prune": {3, func(n string, _ *http.Request) string {
			return "deletes the rows of state collection " + n + " in the given time range"
		}, true},
		"DELETE /v1/state/{owner}/{collection}": {3, func(n string, _ *http.Request) string {
			return "deletes all rows of state collection " + n
		}, true},
	}}
}

// wrap answers `validation` with `confirm_required` when a destructive route
// is called without `?confirm=1`.
func (g *confirmGate) wrap(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		target, gated := g.routes[pattern]
		if !gated || target.describe == nil || confirmed(r) || (target.runOK && inRun(r)) {
			mux.ServeHTTP(w, r)
			return
		}
		writeConfirmRequired(w, target.describe(pathSegment(r.URL.Path, target.segment), r))
	})
}

// writeConfirmRequired refuses a destructive call that has no confirm.
func writeConfirmRequired(w http.ResponseWriter, what string) {
	writeErrDetails(w, http.StatusBadRequest, "validation",
		"this call "+what+"; repeat it with confirm=1 once the user agrees",
		map[string]any{"confirm_required": true})
}

// writeConfirmRoutes refuses a delete that has no confirm. It lists the
// routes and rules that the delete removes from routing.
func writeConfirmRoutes(w http.ResponseWriter, what, name string, refs routing.Refs) {
	if refs.Empty() {
		writeConfirmRequired(w, what)
		return
	}
	writeErrDetails(w, http.StatusBadRequest, "validation",
		"this call "+what+". "+refs.Describe(name)+" Repeat it with confirm=1 once the user agrees.",
		map[string]any{"confirm_required": true, "routes": refs.Routes, "rules": refs.Rules})
}

// pathSegment returns the nth "/"-separated segment of path, or "" when the
// index is out of range.
func pathSegment(path string, n int) string {
	if n < 0 {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if n >= len(parts) {
		return ""
	}
	return parts[n]
}

// inRun reports a caller with a run token.
func inRun(r *http.Request) bool {
	a, ok := tools.ActorFrom(r.Context())
	return ok && a.RunID != ""
}

// confirmed reads the confirm query parameter.
func confirmed(r *http.Request) bool {
	v := strings.TrimSpace(r.URL.Query().Get("confirm"))
	return v == "1" || v == "true"
}
