package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
)

func isolateHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	cfg := &config.Config{}
	cfg.Tools.SetPin(Claude, &config.ToolPin{Auth: harness.AuthModeSeparate})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthCapsMatrix(t *testing.T) {
	t.Parallel()
	want := map[string]struct {
		relay, token, fragile, attach, expiry bool
		localCmd                              string
	}{
		Claude:   {relay: true, token: true, expiry: true, localCmd: "claude auth login"},
		Codex:    {relay: true, token: true, expiry: true, localCmd: "codex login --device-auth"},
		Grok:     {relay: true, token: true, expiry: true, localCmd: "grok login --device-auth"},
		Gemini:   {relay: true, token: true, fragile: true, expiry: true, localCmd: "gemini  (NO_BROWSER=1)"},
		OpenCode: {relay: true, token: true, localCmd: "opencode auth login"},
		Copilot:  {relay: true, token: true, attach: true},
		Cursor:   {token: true, localCmd: "cursor-agent login"},
		Pi:       {relay: true, token: true, attach: true},
	}
	for _, tool := range AllTools() {
		caps := tool.AuthCapabilities()
		w, ok := want[tool.Name()]
		if !ok {
			t.Errorf("%s: missing from matrix", tool.Name())
			continue
		}
		if caps.HasLoginMode(harness.LoginRelay) != w.relay {
			t.Errorf("%s relay=%v want %v", tool.Name(), caps.HasLoginMode(harness.LoginRelay), w.relay)
		}
		if caps.HasLoginMode(harness.LoginToken) != w.token {
			t.Errorf("%s token=%v want %v", tool.Name(), caps.HasLoginMode(harness.LoginToken), w.token)
		}
		args, _ := vendorLoginArgs(tool.Name(), harness.AuthModeShared)
		if w.relay && len(args) == 0 && !w.attach {
			// gemini/copilot/pi may have empty args (env-only or TUI)
			if tool.Name() != Gemini && tool.Name() != Copilot && tool.Name() != Pi {
				t.Errorf("%s relay without login args", tool.Name())
			}
		}
		if w.token && len(caps.TokenEnvKeys) == 0 && tool.Name() != OpenCode {
			t.Errorf("%s token without env keys", tool.Name())
		}
		if w.localCmd != "" && caps.LocalCmd != w.localCmd {
			t.Errorf("%s LocalCmd=%q want %q", tool.Name(), caps.LocalCmd, w.localCmd)
		}
		if caps.RelayFragile != w.fragile {
			t.Errorf("%s fragile=%v", tool.Name(), caps.RelayFragile)
		}
		if caps.AttachOnly != w.attach {
			t.Errorf("%s attach=%v", tool.Name(), caps.AttachOnly)
		}
		if caps.ExpiryReader != w.expiry {
			t.Errorf("%s expiry=%v", tool.Name(), caps.ExpiryReader)
		}
	}
	unk := unknownTool{}.AuthCapabilities()
	if len(unk.LoginModes) != 0 {
		t.Errorf("unknown LoginModes=%v", unk.LoginModes)
	}
}

func TestProbeLadder_Claude(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()

	st := claudeTool{}.ProbeAuth(ctx)
	if st.Status != harness.AuthMissing {
		t.Fatalf("empty: %s", st.Status)
	}

	if err := os.MkdirAll(config.SecretsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := harness.WriteSecretEnv(Claude, map[string]string{"ANTHROPIC_API_KEY": "sk"}); err != nil {
		t.Fatal(err)
	}
	st = claudeTool{}.ProbeAuth(ctx)
	if st.Status != harness.AuthOK || !strings.Contains(st.Detail, "ANTHROPIC_API_KEY") {
		t.Fatalf("key: %+v", st)
	}

	// L1 beats expired file.
	credDir := filepath.Join(home.Base(), "harness", Claude)
	if err := os.MkdirAll(credDir, 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).UnixMilli()
	if err := os.WriteFile(filepath.Join(credDir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"expiresAt":`+itoa(past)+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st = claudeTool{}.ProbeAuth(ctx)
	if st.Status != harness.AuthOK {
		t.Fatalf("expired+key should be ok: %+v", st)
	}
}

func TestProbeLadder_FileUnknown(t *testing.T) {
	isolateHome(t)
	credDir := filepath.Join(home.Base(), "harness", Claude)
	if err := os.MkdirAll(credDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// No L2 binary on a PATH without claude.
	t.Setenv("PATH", t.TempDir()+"/no-such-bin")
	st := claudeTool{}.ProbeAuth(context.Background())
	if st.Status != harness.AuthUnknown {
		t.Fatalf("file without L2 must be unknown, got %s", st.Status)
	}
}

func TestProbeClaudeL2(t *testing.T) {
	isolateHome(t)
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = auth ] && [ \"$2\" = status ]; then echo '{\"loggedIn\":true,\"email\":\"a@b.c\"}'; exit 0; fi\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	st := claudeTool{}.ProbeAuth(context.Background())
	if st.Status != harness.AuthOK || st.Detail != "a@b.c" {
		t.Fatalf("%+v", st)
	}
}

func TestParseExpiryMS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ms := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	p := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(p, []byte(`{"claudeAiOauth":{"expiresAt":`+itoa(ms)+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := parseExpiryMS(p, "claudeAiOauth.expiresAt")
	if got == nil || got.Year() != 2026 {
		t.Fatalf("%v", got)
	}
	p2 := filepath.Join(dir, "oauth_creds.json")
	if err := os.WriteFile(p2, []byte(`{"expiry_date":`+itoa(ms)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got = parseExpiryMS(p2, "expiry_date")
	if got == nil || got.Year() != 2026 {
		t.Fatalf("%v", got)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if parseExpiryMS(bad, "expiry_date") != nil {
		t.Fatal("malformed must not panic or return a time")
	}
}

func TestExtractLoginGoldens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path, tool string
		wantURL    bool
		wantCode   string
		await      bool
	}{
		{"testdata/screens/claude/login/paste/01-url.txt", Claude, true, "", true},
		{"testdata/screens/codex/login/device/01-url.txt", Codex, true, "ABCD-EFGH", true},
	}
	for _, tc := range cases {
		b, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		ex := extractLogin(tc.tool, string(b))
		if tc.wantURL && ex.URL == "" {
			t.Errorf("%s: no url in %+v", tc.path, ex)
		}
		if tc.wantCode != "" && ex.UserCode != tc.wantCode {
			t.Errorf("%s: code=%q want %q", tc.path, ex.UserCode, tc.wantCode)
		}
		if ex.AwaitCode != tc.await {
			t.Errorf("%s: await=%v want %v", tc.path, ex.AwaitCode, tc.await)
		}
		if ex.NoExtract {
			t.Errorf("%s: unexpected NoExtract", tc.path)
		}
	}
}

func TestExtractLogin(t *testing.T) {
	t.Parallel()
	claude := extractLogin(Claude, "Visit https://claude.ai/oauth/authorize?x=1\nEnter the authorization code:\n")
	if claude.URL != "https://claude.ai/oauth/authorize?x=1" || !claude.AwaitCode {
		t.Fatalf("%+v", claude)
	}
	codex := extractLogin(Codex, "To authenticate, visit:\nhttps://auth.openai.com/codex/device\nEnter code: ABCD-EFGH\n")
	if codex.URL == "" || codex.UserCode != "ABCD-EFGH" {
		t.Fatalf("%+v", codex)
	}
	token := extractLogin(Claude, "Your token: sk-ant-oat01-abcDEF123\n")
	if token.Token != "sk-ant-oat01-abcDEF123" {
		t.Fatalf("%+v", token)
	}
	none := extractLogin(Pi, "anything")
	if !none.NoExtract {
		t.Fatal("pi must be attach-only")
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
