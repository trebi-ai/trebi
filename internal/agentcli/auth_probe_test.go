package agentcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

// probeEnv isolates the trebi home and the user home, sets the auth mode of
// name, and puts a stub vendor CLI on PATH when script is not empty.
func probeEnv(t *testing.T, name, mode, script string) (userHome string) {
	t.Helper()
	isolateHome(t)
	userHome = t.TempDir()
	t.Setenv("HOME", userHome)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tools.SetPin(name, &config.ToolPin{Auth: mode})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if script != "" {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return userHome
}

func copyFixture(t *testing.T, fixture, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "auth", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

const grokLoggedIn = `echo "You are logged in with grok.com"; echo "grok-4"`
const grokRejected = `echo "You are not authenticated."; rm -f "$GROK_HOME/auth.json"`

// grokRefreshRace rejects the first call and accepts the next, as grok does
// while it refreshes an expired token.
const grokRefreshRace = `if [ -e "$GROK_HOME/.called" ]; then ` + grokLoggedIn + `; else touch "$GROK_HOME/.called"; echo "You are not authenticated."; fi`

func TestProbeGrok(t *testing.T) {
	cases := []struct {
		name, fixture, script, want, detail string
	}{
		{"ok", "grok/ok.json", grokLoggedIn, harness.AuthOK, ""},
		{"refreshable", "grok/expired_refreshable.json", grokLoggedIn, harness.AuthOK, ""},
		{"refresh rejected", "grok/ok.json", grokRejected, harness.AuthExpired, "The login ended. Log in again."},
		{"refresh race", "grok/ok.json", grokRefreshRace, harness.AuthOK, ""},
		{"no refresh token", "grok/no_refresh.json", grokRefreshRace, harness.AuthExpired, "The login ended. Log in again."},
		{"expired no refresh", "grok/expired_no_refresh.json", grokLoggedIn, harness.AuthExpired, "credentials expired"},
		{"missing", "", grokLoggedIn, harness.AuthMissing, ""},
		{"no live check", "grok/ok.json", "", harness.AuthOK, "login file present; live check unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probeEnv(t, Grok, harness.AuthModeSeparate, tc.script)
			if tc.script == "" {
				t.Setenv("PATH", t.TempDir())
			}
			if tc.fixture != "" {
				copyFixture(t, tc.fixture, filepath.Join(config.HarnessDir(Grok), "auth.json"))
			}
			st := grokTool{}.ProbeAuth(context.Background())
			if st.Status != tc.want || st.Detail != tc.detail {
				t.Fatalf("got %s %q, want %s %q", st.Status, st.Detail, tc.want, tc.detail)
			}
			if tc.fixture != "" && st.Account != "dev@example.test" {
				t.Fatalf("account %q", st.Account)
			}
		})
	}
}

func TestProbeCodexAccount(t *testing.T) {
	probeEnv(t, Codex, harness.AuthModeSeparate, `echo "Logged in using ChatGPT"`)
	copyFixture(t, "codex/ok.json", filepath.Join(config.HarnessDir(Codex), "auth.json"))
	st := codexTool{}.ProbeAuth(context.Background())
	if st.Status != harness.AuthOK || st.Account != "dev@example.test (Plus)" {
		t.Fatalf("%+v", st)
	}
}

func TestProbeCodexRejected(t *testing.T) {
	probeEnv(t, Codex, harness.AuthModeSeparate, `echo "Not logged in"; exit 1`)
	copyFixture(t, "codex/ok.json", filepath.Join(config.HarnessDir(Codex), "auth.json"))
	st := codexTool{}.ProbeAuth(context.Background())
	if st.Status != harness.AuthExpired || st.Detail != "The login ended. Log in again." {
		t.Fatalf("%+v", st)
	}
}

func TestProbeOpenCode(t *testing.T) {
	cases := []struct {
		name, fixture, script, want string
	}{
		{"ok", "opencode/two.json", `echo "2 credentials"`, harness.AuthOK},
		{"ok table", "opencode/two.json", `printf 'Anthropic         default      stored\nOpenRouter        OpenRouter   stored\n'`, harness.AuthOK},
		{"old service", "opencode/two.json", `echo "Background service uses an incompatible health protocol."`, harness.AuthUnknown},
		{"empty store", "", `echo "0 credentials"`, harness.AuthMissing},
		{"no live check", "opencode/two.json", "", harness.AuthUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probeEnv(t, OpenCode, harness.AuthModeSeparate, tc.script)
			if tc.script == "" {
				t.Setenv("PATH", t.TempDir())
			}
			if tc.fixture != "" {
				copyFixture(t, tc.fixture, opencodeAuthPath(OpenCode))
			}
			st := opencodeTool{name: OpenCode}.ProbeAuth(context.Background())
			if st.Status != tc.want {
				t.Fatalf("got %+v", st)
			}
			if tc.fixture != "" && st.Account != "Anthropic, OpenRouter" {
				t.Fatalf("account %q", st.Account)
			}
		})
	}
}

func TestOpenCodeKeyAndArgs(t *testing.T) {
	probeEnv(t, OpenCode, harness.AuthModeSeparate, "")
	copyFixture(t, "opencode/two.json", opencodeAuthPath(OpenCode))
	if err := writeOpenCodeKey(OpenCode, "opencode-go", "k1"); err != nil {
		t.Fatal(err)
	}
	ids := opencodeProviderIDs(opencodeAuthPath(OpenCode))
	if len(ids) != 3 {
		t.Fatalf("ids %v", ids)
	}
	fi, err := os.Stat(opencodeAuthPath(OpenCode))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi, err)
	}
	if err := writeOpenCodeKey(OpenCode, "../x", "k"); err == nil {
		t.Fatal("bad provider id must fail")
	}
	if _, err := opencodeLoginArgs(""); err == nil {
		t.Fatal("empty provider must fail")
	}
	if _, err := opencodeLoginArgs("anthropic"); err == nil {
		t.Fatal("anthropic has no browser login in 1.18.29")
	}
	args, err := opencodeLoginArgs("openai")
	if err != nil || len(args) != 4 {
		t.Fatalf("%v %v", args, err)
	}
	if n, ok := opencodeCredentialCount("\x1b[90m┌  Credentials\n└  2 credentials\x1b[0m"); !ok || n != 2 {
		t.Fatalf("count %d %v", n, ok)
	}
	var have []string
	for _, p := range OpenCodeProviders(OpenCode) {
		if p.LoggedIn {
			have = append(have, p.ID)
		}
	}
	if len(have) != 3 {
		t.Fatalf("logged in %v", have)
	}
}

// OpenCode 2.x reads its keys from opencode.db, so a shared login runs on the
// user data folder, not on a linked auth.json.
func TestOpenCodeDataHomeByMode(t *testing.T) {
	user := probeEnv(t, OpenCode, harness.AuthModeShared, "")
	if got, want := opencodeDataHome(OpenCode), filepath.Join(user, ".local", "share"); got != want {
		t.Fatalf("shared: %s, want %s", got, want)
	}
	if got := (opencodeTool{name: OpenCode}).launchEnv()["XDG_DATA_HOME"]; got != filepath.Join(user, ".local", "share") {
		t.Fatalf("shared launch env: %s", got)
	}
	probeEnv(t, OpenCode, harness.AuthModeSeparate, "")
	if got := opencodeDataHome(OpenCode); got != opencodeOwnData(OpenCode) {
		t.Fatalf("separate: %s", got)
	}
}
