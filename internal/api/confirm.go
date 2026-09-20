package api

import (
	"net/http"
	"strings"
)

// confirmTarget names the object a destructive call removes. Path values come
// from the matched pattern; the mux fills them only after this gate runs, so
// the gate reads the segments itself.
type confirmTarget struct {
	segment  int // index of the object name in the request path, -1 for none
	describe func(name string, r *http.Request) string
}

// confirmGate holds the destructive routes that need `?confirm=1`. The gate
// lives in the API so the CLI, the UI, and MCP share one rule.
type confirmGate struct {
	routes map[string]confirmTarget
}

func newConfirmGate() *confirmGate {
	return &confirmGate{routes: map[string]confirmTarget{
		"DELETE /v1/workspaces/{id}": {2, func(n string, _ *http.Request) string {
			return "deletes workspace " + n + " and its folder on disk; run rows stay"
		}},
		"POST /v1/workspaces/{id}/move": {2, func(n string, _ *http.Request) string {
			return "moves workspace " + n + " and rewrites absolute paths in its files"
		}},
		"POST /v1/workspaces/{id}/email/rotate": {2, func(n string, _ *http.Request) string {
			return "retires the email address of workspace " + n +
				"; mail to the old address stops arriving and open threads break"
		}},
		"DELETE /v1/jobs/{name}": {2, func(n string, r *http.Request) string {
			if r.URL.Query().Get("purge") == "1" {
				return "deletes job " + n + " and its whole folder; run rows stay"
			}
			return "deletes the JOB.md of " + n + "; other files in the job folder and run rows stay"
		}},
		"DELETE /v1/jobs/{name}/memory": {2, func(n string, _ *http.Request) string {
			return "erases the memory file of job " + n
		}},
		"DELETE /v1/workspaces/{id}/assistant/memory": {2, func(n string, _ *http.Request) string {
			return "erases the assistant memory of workspace " + n
		}},
		"DELETE /v1/workspaces/{id}/agents/{name}": {4, func(n string, r *http.Request) string {
			if r.URL.Query().Get("purge") == "1" {
				return "deletes agent " + n + " and its whole agents/" + n + "/ folder; run rows stay"
			}
			return "deletes the AGENT.md of agent " + n + "; its jobs stay and become unowned"
		}},
		"DELETE /v1/workspaces/{id}/agents/{name}/memory": {4, func(n string, _ *http.Request) string {
			return "erases the memory of agent " + n
		}},
		"DELETE /v1/secrets/{name}": {2, func(n string, _ *http.Request) string {
			return "deletes secret " + n + "; jobs that read it will fail"
		}},
		"DELETE /v1/connectors/{name}": {2, func(n string, _ *http.Request) string {
			return "deletes connector " + n + " and stops its event stream"
		}},
		"DELETE /v1/skills/{name}": {2, func(n string, _ *http.Request) string {
			return "deletes skill " + n + " and its folder"
		}},
		"DELETE /v1/workspaces/{id}/files/{path...}": {-1, func(_ string, r *http.Request) string {
			return "deletes " + r.URL.Path[strings.Index(r.URL.Path, "/files/")+len("/files/"):] + " from the workspace"
		}},
		"POST /v1/system/shutdown": {-1, func(string, *http.Request) string {
			return "stops the trebi daemon; scheduled jobs stop firing until it starts again"
		}},
		"DELETE /v1/cloud/link": {-1, func(string, *http.Request) string {
			return "unlinks this daemon from the cloud; remote clients lose access"
		}},
	}}
}

// wrap answers `validation` with `confirm_required` when a destructive route
// is called without `?confirm=1`.
func (g *confirmGate) wrap(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		target, gated := g.routes[pattern]
		if !gated || confirmed(r) {
			mux.ServeHTTP(w, r)
			return
		}
		writeErrDetails(w, http.StatusBadRequest, "validation",
			"this call "+target.describe(pathSegment(r.URL.Path, target.segment), r)+
				"; repeat it with confirm=1 once the user agrees",
			map[string]any{"confirm_required": true})
	})
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

// confirmed reads the confirm query parameter.
func confirmed(r *http.Request) bool {
	v := strings.TrimSpace(r.URL.Query().Get("confirm"))
	return v == "1" || v == "true"
}
