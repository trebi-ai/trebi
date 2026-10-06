package api

import (
	"bytes"
	"io"
	"net/http"
	"slices"

	"github.com/trebi-ai/trebi/internal/tools"
)

// Remote MCP scopes that the cloud grants (plan 2026-10-04 D6).
const (
	scopeMCPRead  = "mcp:read"
	scopeMCPWrite = "mcp:write"
)

// remoteGate enforces the cloud-granted scope on remote MCP callers.
// Local callers pass unchanged.
type remoteGate struct {
	mux   *http.ServeMux
	deny  map[string]bool // route patterns that no remote caller may use
	reads map[string]bool // POST patterns that only read
}

func newRemoteGate(mux *http.ServeMux) *remoteGate {
	return &remoteGate{mux: mux, deny: map[string]bool{
		"POST /v1/system/shutdown": true,
		"POST /v1/cloud/link":      true,
		"DELETE /v1/cloud/link":    true,
	}, reads: map[string]bool{
		"POST /v1/state/{owner}/{collection}/query": true,
		"POST /v1/state/{owner}/{collection}/get":   true,
		"POST /v1/state/{owner}/{collection}/has":   true,
	}}
}

func (g *remoteGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := tools.ActorFrom(r.Context())
		if !ok || a.Kind != tools.ActorRemote {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		_, pattern := g.mux.Handler(r)
		if g.reads[pattern] {
			next.ServeHTTP(w, r)
			return
		}
		if !slices.Contains(a.Scopes, scopeMCPWrite) {
			writeErr(w, http.StatusForbidden, "remote_read_only",
				"This connection is read only. Reconnect with Full control to make changes.")
			return
		}
		if g.deny[pattern] || (pattern == "PATCH /v1/config" && touchesRemoteMCP(r)) {
			writeErr(w, http.StatusForbidden, "remote_denied",
				"A remote connection cannot do this. Use the Trebi app on this machine.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// touchesRemoteMCP reports whether a config patch names cloud.remote_mcp.
// It fails closed on a body it cannot read.
func touchesRemoteMCP(r *http.Request) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return bytes.Contains(body, []byte("remote_mcp"))
}
