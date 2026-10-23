package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandler(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-API", "1")
		_, _ = io.WriteString(w, "api")
	})
	h := Handler(testFS(), api)

	tests := []struct {
		name      string
		method    string
		path      string
		wantCode  int
		wantBody  string
		wantCache string
		wantAPI   bool
	}{
		{name: "root serves index", method: "GET", path: "/", wantCode: 200, wantBody: "app shell", wantCache: "no-store"},
		{name: "spa route falls back", method: "GET", path: "/jobs/demo", wantCode: 200, wantBody: "app shell", wantCache: "no-store"},
		{name: "missing file falls back", method: "GET", path: "/favicon.ico", wantCode: 200, wantBody: "app shell", wantCache: "no-store"},
		{name: "manifest is no-store", method: "GET", path: "/manifest.json", wantCode: 200, wantBody: `"ui_version"`, wantCache: "no-store"},
		{name: "asset is immutable", method: "GET", path: "/assets/app-abc.js", wantCode: 200, wantBody: "console.log", wantCache: "immutable"},
		{name: "api is delegated", method: "GET", path: "/v1/system/health", wantCode: 200, wantBody: "api", wantAPI: true},
		{name: "mcp is delegated", method: "POST", path: "/mcp", wantCode: 200, wantBody: "api", wantAPI: true},
		{name: "healthz is delegated", method: "GET", path: "/healthz", wantCode: 200, wantBody: "api", wantAPI: true},
		{name: "login return is delegated", method: "GET", path: "/login/done", wantCode: 200, wantBody: "api", wantAPI: true},
		{name: "asset rejects POST", method: "POST", path: "/jobs", wantCode: 405},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("code=%d want %d body=%q", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantAPI && rec.Header().Get("X-API") == "" {
				t.Fatalf("api handler was not reached: %v", rec.Header())
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body=%q want substring %q", rec.Body.String(), tc.wantBody)
			}
			if tc.wantCache != "" && !strings.Contains(rec.Header().Get("Cache-Control"), tc.wantCache) {
				t.Fatalf("cache-control=%q want %q", rec.Header().Get("Cache-Control"), tc.wantCache)
			}
		})
	}
}

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":        {Data: []byte("<html>app shell</html>")},
		"manifest.json":     {Data: []byte(`{"ui_version":"0.1.0"}`)},
		"assets/app-abc.js": {Data: []byte("console.log(1)")},
	}
}
