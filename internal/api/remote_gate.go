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

// scopeMobileFull is mobile.ScopeFull. A phone without it is read only.
const scopeMobileFull = "full"

const readOnlyMessage = "This connection is read only. Ask the owner of the machine for full control."

// remoteGate enforces the granted scope on remote MCP callers and paired
// phones. Local callers pass unchanged.
type remoteGate struct {
	mux        *http.ServeMux
	deny       map[string]bool // route patterns that no remote caller may use
	denyMobile map[string]bool // route patterns that no phone may use
	reads      map[string]bool // POST patterns that only read
}

func newRemoteGate(mux *http.ServeMux) *remoteGate {
	return &remoteGate{mux: mux, deny: map[string]bool{
		"POST /v1/system/shutdown": true,
		"POST /v1/cloud/link":      true,
		"DELETE /v1/cloud/link":    true,
	}, denyMobile: map[string]bool{
		"POST /v1/system/shutdown": true,
		"POST /v1/cloud/link":      true,
		"DELETE /v1/cloud/link":    true,
		"PUT /v1/system/license":   true,
		"POST /v1/mobile/pairings": true,
	}, reads: map[string]bool{
		"POST /v1/state/{owner}/{collection}/query": true,
		"POST /v1/state/{owner}/{collection}/get":   true,
		"POST /v1/state/{owner}/{collection}/has":   true,
	}}
}

func (g *remoteGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := tools.ActorFrom(r.Context())
		if ok && a.Kind == tools.ActorMobile {
			g.mobile(w, r, a, next)
			return
		}
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
			writeErr(w, http.StatusForbidden, "remote_read_only", readOnlyMessage)
			return
		}
		if g.deny[pattern] || (pattern == "PATCH /v1/config" && touchesRemoteSwitch(r)) {
			writeErr(w, http.StatusForbidden, "remote_denied",
				"A remote connection cannot do this. Use the Trebi app on this machine.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mobile applies the phone rules (plan D8): the read scope allows only
// reads, and no phone may mint access or turn off a remote switch.
func (g *remoteGate) mobile(w http.ResponseWriter, r *http.Request, a tools.Actor, next http.Handler) {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		next.ServeHTTP(w, r)
		return
	}
	if !slices.Contains(a.Scopes, scopeMobileFull) {
		writeErr(w, http.StatusForbidden, "remote_read_only", readOnlyMessage)
		return
	}
	_, pattern := g.mux.Handler(r)
	if g.denyMobile[pattern] || (pattern == "PATCH /v1/config" && touchesRemoteSwitch(r)) {
		writeErr(w, http.StatusForbidden, "remote_denied",
			"A phone cannot do this. Use the Trebi app on this machine.")
		return
	}
	next.ServeHTTP(w, r)
}

// touchesRemoteSwitch reports whether a config patch names cloud.remote_mcp,
// cloud.mobile, or mobile.direct. It fails closed on a body it cannot read.
func touchesRemoteSwitch(r *http.Request) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return bytes.Contains(body, []byte("remote_mcp")) || bytes.Contains(body, []byte("cloud.mobile")) || bytes.Contains(body, []byte("mobile.direct")) || bytes.Contains(body, []byte(`"mobile"`))
}
