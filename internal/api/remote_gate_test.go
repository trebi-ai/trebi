package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/tools"
)

func TestRemoteGate(t *testing.T) {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	for _, p := range []string{
		"POST /v1/system/shutdown", "POST /v1/cloud/link", "DELETE /v1/cloud/link",
		"PUT /v1/system/license", "PATCH /v1/config", "POST /v1/jobs", "GET /v1/jobs",
	} {
		mux.HandleFunc(p, ok)
	}
	h := newRemoteGate(mux).wrap(mux)
	remote := func(scopes ...string) *tools.Actor {
		return &tools.Actor{Kind: tools.ActorRemote, Name: "remote:claude.ai", Scopes: scopes}
	}
	for _, tc := range []struct {
		name   string
		actor  *tools.Actor
		method string
		path   string
		body   string
		want   int
		code   string
	}{
		{"local write", nil, "POST", "/v1/system/shutdown", "", 204, ""},
		{"read only get", remote("mcp:read"), "GET", "/v1/jobs", "", 204, ""},
		{"read only post", remote("mcp:read"), "POST", "/v1/jobs", "{}", 403, "remote_read_only"},
		{"write post", remote("mcp:read", "mcp:write"), "POST", "/v1/jobs", "{}", 204, ""},
		{"shutdown", remote("mcp:read", "mcp:write"), "POST", "/v1/system/shutdown", "", 403, "remote_denied"},
		{"link", remote("mcp:read", "mcp:write"), "POST", "/v1/cloud/link", "", 403, "remote_denied"},
		{"unlink", remote("mcp:read", "mcp:write"), "DELETE", "/v1/cloud/link", "", 403, "remote_denied"},
		{"license", remote("mcp:read", "mcp:write"), "PUT", "/v1/system/license", "", 403, "remote_denied"},
		{"config remote_mcp", remote("mcp:read", "mcp:write"), "PATCH", "/v1/config", `{"set":{"cloud.remote_mcp":"false"}}`, 403, "remote_denied"},
		{"config other", remote("mcp:read", "mcp:write"), "PATCH", "/v1/config", `{"set":{"defaults.command":"claude"}}`, 204, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.actor != nil {
				req = req.WithContext(tools.WithActor(req.Context(), *tc.actor))
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body.String(), tc.want, tc.code)
			}
		})
	}
}
