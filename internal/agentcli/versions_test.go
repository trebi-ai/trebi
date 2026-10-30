package agentcli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

func TestRunEnvTurnsAutoUpdateOff(t *testing.T) {
	isolateHome(t)
	cfg, _ := config.Load()
	for _, n := range []string{Grok, OpenCode} {
		cfg.Tools.SetPin(n, &config.ToolPin{Auth: harness.AuthModeSeparate})
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{Claude, claudeTool{}.launchEnv(), []string{"DISABLE_AUTOUPDATER", "DISABLE_UPDATES"}},
		{Grok, grokTool{}.launchEnv(), []string{"GROK_DISABLE_AUTOUPDATER"}},
		{OpenCode, opencodeTool{name: OpenCode}.launchEnv(), []string{"OPENCODE_DISABLE_AUTOUPDATE"}},
	}
	for _, c := range cases {
		for _, k := range c.want {
			if c.env[k] != "1" {
				t.Errorf("%s: %s=%q, want 1", c.name, k, c.env[k])
			}
		}
	}
}

func TestLookPathToolPrefersKeptCopy(t *testing.T) {
	isolateHome(t)
	dir := filepath.Join(config.BinDir(), Claude, "1.0.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, Claude), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("1.0.0", filepath.Join(config.BinDir(), Claude, "current")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(config.BinDir(), Claude, "current", Claude)
	if got := lookPathTool(Claude); got != want {
		t.Fatalf("lookPathTool = %q, want %q", got, want)
	}
}

// hostRewrite sends every request to body by URL.
type hostRewrite map[string]string

func (h hostRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := h[r.URL.String()]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func TestVersionSources(t *testing.T) {
	t.Parallel()
	var calls []string
	run := func(_ context.Context, bin string, args ...string) ([]byte, error) {
		calls = append(calls, filepath.Base(bin)+" "+strings.Join(args, " "))
		switch strings.Join(args, " ") {
		case "--version":
			return []byte("1.0.40 (Claude Code)\n"), nil
		case "update --check --json":
			return []byte(`{"currentVersion":"1.0.40","latestVersion":"1.0.50","updateAvailable":true}`), nil
		}
		return nil, fmt.Errorf("unexpected %v", args)
	}
	vs := VersionSources{
		Run: run,
		HTTP: &http.Client{Transport: hostRewrite{
			"https://registry.npmjs.org/-/package/@anthropic-ai/claude-code/dist-tags": `{"latest":"2.1.300","next":"2.2.0"}`,
			"https://api.github.com/repos/openai/codex/releases/latest":                `{"tag_name":"rust-v0.163.0"}`,
			"https://api.github.com/repos/sst/opencode/releases/latest":                `{"tag_name":"v1.18.30"}`,
		}},
	}
	ctx := context.Background()
	want := map[string]string{Claude: "2.1.300", Codex: "0.163.0", OpenCode: "1.18.30"}
	for name, w := range want {
		src, ok := vs.Source(name)
		if !ok {
			t.Fatalf("%s: no source", name)
		}
		if got, err := src.Latest(ctx); err != nil || got != w {
			t.Errorf("%s Latest = %q, %v; want %q", name, got, err, w)
		}
	}
	if v, err := (vendorVersions{name: Claude, run: run}).Installed(ctx, "/x/claude"); err != nil || v != "1.0.40" {
		t.Errorf("Installed = %q, %v", v, err)
	}
	if _, ok := vs.Source(Cursor); ok {
		t.Error("cursor has no version source")
	}
	if _, ok := mustSource(t, vs, Claude).(harness.VersionInstaller); ok {
		t.Error("claude cannot install a named version")
	}
	if _, ok := mustSource(t, vs, OpenCode).(harness.VersionInstaller); !ok {
		t.Error("opencode installs a named version")
	}
	if got := mustSource(t, vs, Codex).AutoUpdateOff(); len(got) != 0 {
		t.Errorf("codex turns updates off with a flag, not env: %v", got)
	}
}

func mustSource(t *testing.T, vs VersionSources, name string) harness.VersionSource {
	t.Helper()
	src, ok := vs.Source(name)
	if !ok {
		t.Fatalf("%s: no source", name)
	}
	return src
}
