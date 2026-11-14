package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetupCommands(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"step":"ai","hidden":["pro"],"revision":2}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "setup")
	if err != nil || gotMethod != "GET" || gotPath != "/v1/onboarding" || !strings.Contains(out, "step: ai") || !strings.Contains(out, "hidden: pro") {
		t.Fatalf("status: %v %s %s %q", err, gotMethod, gotPath, out)
	}
	if _, err := runCLI(t, client, "setup", "update", "--step", "done", "--completed", "--hide", "pro", "--revision", "2"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"step":"done"`, `"completed":true`, `"hide":["pro"]`, `"revision":2`} {
		if gotMethod != "PATCH" || !strings.Contains(gotBody, want) {
			t.Fatalf("update body %s %s", gotMethod, gotBody)
		}
	}
	if _, err := runCLI(t, client, "setup", "update"); err == nil {
		t.Fatal("update with no field must fail")
	}
	if _, err := runCLI(t, client, "setup", "restart"); err != nil || gotMethod != "POST" || gotPath != "/v1/onboarding/restart" {
		t.Fatalf("restart: %v %s %s", err, gotMethod, gotPath)
	}
}

func TestWorkspaceHandleCheckAndSetHandle(t *testing.T) {
	var gotMethod, gotURL, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotURL = r.Method, r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"reason":"taken"}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "workspace", "handle-check", "acme", "--workspace", "ws1")
	if err != nil || gotMethod != "GET" || gotURL != "/v1/workspaces/handle-check?handle=acme&workspace=ws1" || !strings.Contains(out, "taken") {
		t.Fatalf("check: %v %s %s %q", err, gotMethod, gotURL, out)
	}
	if _, err := runCLI(t, client, "workspace", "set", "ws1", "--handle", "acme"); err != nil || gotMethod != "PATCH" || !strings.Contains(gotBody, `"handle":"acme"`) {
		t.Fatalf("set: %v %s %s", err, gotMethod, gotBody)
	}
}
