package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// fakeNPM installs a fake claude into $NPM_CONFIG_PREFIX. The version 9.9.9
// fails.
const fakeNPM = `#!/bin/sh
spec="$3"; ver="${spec##*@}"
[ "$ver" = 9.9.9 ] && { echo "npm ERR! no such version" >&2; exit 1; }
/bin/mkdir -p "$NPM_CONFIG_PREFIX/bin"
/bin/cat > "$NPM_CONFIG_PREFIX/bin/claude" <<EOF
#!/bin/sh
case "\$1" in
--version) echo "$ver (Claude Code)";;
logout) /bin/rm -f "\$FAKE_HOME/cred";;
esac
EOF
/bin/chmod +x "$NPM_CONFIG_PREFIX/bin/claude"
`

type visorRig struct {
	svc  *Service
	st   *store.Store
	v    *agentvisor.Visor
	user string
}

func TestVisorInstallAndKeep(t *testing.T) {
	r := newVisorRig(t)
	ctx := t.Context()
	h, err := r.svc.Install(ctx, "claude", "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if h.Source != agentvisor.SourceVisor || h.Version != "2.0.0" {
		t.Fatalf("install: source %q version %q", h.Source, h.Version)
	}
	if countTopic(t, r.st, "harness.updated") != 1 {
		t.Fatal("install must publish harness.updated")
	}
	if got := filepath.Join(config.ToolCurrentLink("claude"), "bin", "claude"); !fileExists(got) {
		t.Fatalf("visor copy not at %s", got)
	}
	if err := r.svc.SetUpdates(ctx, "claude", UpdatesKeep, "2.1.0"); err != nil {
		t.Fatal(err)
	}
	if h, _ = r.svc.Version(ctx, "claude"); h.Version != "2.1.0" || h.Updates != agentvisor.UpdatesKeep {
		t.Fatalf("keep: version %q updates %q", h.Version, h.Updates)
	}
	if _, err := r.svc.Install(ctx, "claude", "9.9.9"); err == nil {
		t.Fatal("a failed install must give an error")
	}
	if r.svc.Updating(ctx, "claude") {
		t.Fatal("no update runs")
	}
}

func TestVisorLogoutSeparate(t *testing.T) {
	r := newVisorRig(t)
	ctx := t.Context()
	if _, err := r.svc.Install(ctx, "claude", "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.persistMode(ctx, "claude", AuthModeSeparate); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.svc.Version(ctx, "claude"); h.AuthMode != agentvisor.AuthSeparate {
		t.Fatalf("visor auth mode %q", h.AuthMode)
	}
	cred := filepath.Join(home.HarnessDir("claude"), "cred")
	if err := os.WriteFile(cred, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretEnv("claude", map[string]string{"ANTHROPIC_API_KEY": "sk"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthOK}, SourceLaunch); err != nil {
		t.Fatal(err)
	}
	out, err := r.svc.Logout(ctx, "claude", AuthModeSeparate, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != AuthMissing || fileExists(cred) {
		t.Fatalf("logout: status %s, cred left %v", out.Status, fileExists(cred))
	}
	if env, _ := LoadSecretEnv("claude"); env["ANTHROPIC_API_KEY"] != "" {
		t.Fatal("secret env left")
	}
	if countTopic(t, r.st, "auth.probe.failed") != 0 {
		t.Fatal("logout must not emit auth.probe.failed")
	}
	if _, err := r.svc.Logout(ctx, "claude", AuthModeShared, false); err == nil {
		t.Fatal("a shared logout needs confirm")
	}
}

func TestVisorSyncGivesConfigModes(t *testing.T) {
	r := newVisorRig(t)
	ctx := t.Context()
	if _, err := r.svc.Install(ctx, "claude", "2.0.0"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Tools.SetPin("claude", &config.ToolPin{Auth: AuthModeSeparate, Updates: UpdatesKeep, Version: "2.0.0"})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	h, _ := r.svc.Version(ctx, "claude")
	if h.AuthMode != agentvisor.AuthSeparate || h.Updates != agentvisor.UpdatesKeep {
		t.Fatalf("sync: auth %q updates %q", h.AuthMode, h.Updates)
	}
	if _, err := os.Stat(home.HarnessDir("claude")); err != nil {
		t.Fatalf("sync must build the harness home: %v", err)
	}
}

func TestVisorFiles(t *testing.T) {
	r := newVisorRig(t)
	ctx := t.Context()
	if err := os.MkdirAll(filepath.Join(r.user, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(r.user, ".claude", "CLAUDE.md")
	if err := os.WriteFile(userFile, []byte("user rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.persistMode(ctx, "claude", AuthModeSeparate); err != nil {
		t.Fatal(err)
	}
	rows, err := r.svc.Files(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || r.svc.OwnCount(ctx, "claude") != 0 {
		t.Fatalf("files: %d rows, own %d", len(rows), r.svc.OwnCount(ctx, "claude"))
	}
	if _, err := r.svc.ReadFile(ctx, "claude", "nope", ""); !errors.Is(err, ErrUnknownFile) {
		t.Fatalf("unknown key: %v", err)
	}
	if err := r.svc.OwnFile(ctx, "claude", "user_memory", ""); err != nil {
		t.Fatal(err)
	}
	if r.svc.OwnCount(ctx, "claude") != 1 {
		t.Fatal("own count after own")
	}
	fc, err := r.svc.ReadFile(ctx, "claude", "user_memory", "")
	if err != nil || fc.Content != "user rules\n" {
		t.Fatalf("read own copy: %q %v", fc.Content, err)
	}
	if _, err := r.svc.WriteFile(ctx, "claude", "user_memory", "trebi rules\n", "stale"); !errors.Is(err, agentvisor.ErrStale) {
		t.Fatalf("stale write: %v", err)
	}
	if _, err := r.svc.WriteFile(ctx, "claude", "user_memory", "trebi rules\n", fc.ETag); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(userFile); string(b) != "user rules\n" {
		t.Fatal("a write must not change the user file")
	}
	if err := r.svc.ResetFile(ctx, "claude", "user_memory"); err != nil {
		t.Fatal(err)
	}
	if r.svc.OwnCount(ctx, "claude") != 0 {
		t.Fatal("own count after reset")
	}
}

func TestNoVisor(t *testing.T) {
	t.Parallel()
	svc := New(openStore(t), nil, Options{})
	if _, err := svc.Install(t.Context(), "claude", ""); !errors.Is(err, ErrNoVisor) {
		t.Fatalf("install: %v", err)
	}
	if svc.Updating(t.Context(), "claude") {
		t.Fatal("updating with no visor")
	}
}

// newVisorRig gives a harness service on an embedded visor that knows only
// claude, with fake vendor facts and a fake npm.
func newVisorRig(t *testing.T) *visorRig {
	t.Helper()
	base := t.TempDir()
	home.SetBase(base)
	t.Setenv("TREBI_HOME", base)
	t.Cleanup(func() { home.SetBase("") })
	user := t.TempDir()
	t.Setenv("HOME", user)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(fakeNPM), 0o755); err != nil { //nolint:gosec // a test program
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	v, err := agentvisor.New(t.Context(), agentvisor.Options{
		Home: home.VisorDir(), Client: "test",
		Harness: agentvisor.HarnessOptions{
			Names: []string{"claude"}, Path: bin, UserHome: user,
			Facts: agentvisor.HarnessFacts{
				Recipe: func(h agentwire.Harness) (agentwire.InstallRecipe, bool) {
					return agentwire.InstallRecipe{Harness: h, Method: agentwire.InstallNPM, Package: "fake-claude", Bin: "claude"}, true
				},
				LogoutCommand: func(agentwire.Harness) []string { return []string{"claude", "logout"} },
				LoginEnv: func(_ agentwire.Harness, home string) map[string]string {
					return map[string]string{"FAKE_HOME": home}
				},
				AuthStatus: func(_ context.Context, _ agentwire.Harness, home string) (agentwire.AuthState, error) {
					if _, err := os.Stat(filepath.Join(home, "cred")); err == nil {
						return agentwire.AuthOK, nil
					}
					return agentwire.AuthMissing, nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close(context.Background()) })
	st := openStore(t)
	svc := New(st, nil, Options{Visor: func(context.Context) (Visor, error) { return v, nil }})
	return &visorRig{svc: svc, st: st, v: v, user: user}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
