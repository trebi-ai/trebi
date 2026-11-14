package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

func TestLineHasLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line string
		want string
		ok   bool
	}{
		{`{"time":"t","level":"INFO","msg":"hi"}`, "INFO", true},
		{`{"time":"t","level":"info","msg":"hi"}`, "INFO", true},
		{`{"time":"t","level":"ERROR","msg":"x"}`, "INFO", false},
		{`plain INFO line`, "INFO", true},
		{`plain warn line`, "WARN", true},
		{`nothing`, "ERROR", false},
	}
	for _, tc := range cases {
		if got := lineHasLevel(tc.line, tc.want); got != tc.ok {
			t.Fatalf("lineHasLevel(%q, %q)=%v want %v", tc.line, tc.want, got, tc.ok)
		}
	}
}

func TestStreamServiceLog_Filters(t *testing.T) {
	t.Parallel()
	in := strings.Join([]string{
		`{"level":"DEBUG","msg":"skip"}`,
		`{"level":"INFO","msg":"keep-me"}`,
		`{"level":"ERROR","msg":"other"}`,
		``,
		`noise without level`,
	}, "\n")
	var buf bytes.Buffer
	streamServiceLog(&buf, strings.NewReader(in), "info", "keep", false)
	got := buf.String()
	if !strings.Contains(got, "keep-me") {
		t.Fatalf("expected keep-me, got %q", got)
	}
	if strings.Contains(got, "skip") || strings.Contains(got, "other") || strings.Contains(got, "noise") {
		t.Fatalf("unexpected lines: %q", got)
	}
}

func TestStreamServiceLog_GrepOnly(t *testing.T) {
	t.Parallel()
	in := "alpha\nbeta line\ngamma\n"
	var buf bytes.Buffer
	streamServiceLog(&buf, strings.NewReader(in), "", "BETA", false)
	if got := strings.TrimSpace(buf.String()); got != "beta line" {
		t.Fatalf("got %q", got)
	}
}

func TestServiceCommandRegistration(t *testing.T) {
	t.Parallel()
	app := NewApp(Options{})
	svc := findCmd(app.Commands, "service")
	if svc == nil {
		t.Fatal("service missing")
	}
	for _, want := range []string{
		"install", "uninstall", "start", "stop", "restart",
		"status", "logs", "migrate-db", "daemon", "drain", "undrain",
	} {
		if findCmd(svc.Commands, want) == nil {
			t.Fatalf("service missing %s", want)
		}
	}
	inst := findCmd(svc.Commands, "install")
	hasBin, hasNoTray := false, false
	for _, f := range inst.Flags {
		switch f.Names()[0] {
		case "bin":
			hasBin = true
		case "no-tray":
			hasNoTray = true
		}
	}
	if !hasBin || !hasNoTray {
		t.Fatalf("install flags: bin=%v no-tray=%v", hasBin, hasNoTray)
	}
	daemon := findCmd(svc.Commands, "daemon")
	hasParent := false
	for _, f := range daemon.Flags {
		if f.Names()[0] == "parent-pid" {
			hasParent = true
		}
	}
	if !hasParent {
		t.Fatal("daemon missing --parent-pid")
	}
	logs := findCmd(svc.Commands, "logs")
	for _, name := range []string{"follow", "lines", "level", "grep", "json"} {
		found := false
		for _, f := range logs.Flags {
			if f.Names()[0] == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("logs missing flag %s", name)
		}
	}
}

func TestSetLogLevel(t *testing.T) {
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	for _, name := range []string{"", "info", "debug", "DEBUG", " warn ", "error"} {
		if err := setLogLevel(name); err != nil {
			t.Errorf("setLogLevel(%q): %v", name, err)
		}
	}
	if err := setLogLevel("bogus"); err == nil {
		t.Fatal("want error for bogus level")
	}
	// debug must actually enable debug records
	if err := setLogLevel("debug"); err != nil {
		t.Fatal(err)
	}
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug not enabled")
	}
	if err := setLogLevel("info"); err != nil {
		t.Fatal(err)
	}
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug still enabled at info")
	}
}

func TestResolveDaemonBind_FlagAndGuard(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })

	bind, err := resolveDaemonBind("127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	if bind.Addr != "127.0.0.1:9" || bind.Scheme != "http" {
		t.Fatalf("%+v", bind)
	}
	if _, err := resolveDaemonBind("0.0.0.0:9"); err == nil {
		t.Fatal("want bind guard on non-loopback without tls/insecure")
	}
}

func TestCloudLinkByEmail(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"linked":false,"status":"not_linked","pending":{"request_id":"req1","email":"a@b.co","status":"pending"}}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "cloud", "link", "--email", "a@b.co", "--name", "mini", "--no-wait", "--json")
	if err != nil || gotMethod != "POST" || gotPath != "/v1/cloud/link-request" || !strings.Contains(gotBody, `"email":"a@b.co"`) || !strings.Contains(gotBody, `"name":"mini"`) {
		t.Fatalf("request: %v %s %s %s", err, gotMethod, gotPath, gotBody)
	}
	if !strings.Contains(out, `"request_id": "req1"`) && !strings.Contains(out, `"request_id":"req1"`) {
		t.Fatalf("json output has no request_id: %s", out)
	}
	if _, err := runCLI(t, client, "cloud", "link"); err == nil {
		t.Fatal("link with no --email and no --code must fail")
	}
	if _, err := runCLI(t, client, "cloud", "link", "--email", "a@b.co", "--code", "abcd"); err == nil {
		t.Fatal("link with --email and --code must fail")
	}
}
