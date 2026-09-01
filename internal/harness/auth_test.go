package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestStampFlipOnly(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()
	svc := New(st, nil, Options{})

	res, err := svc.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthOK}, SourceLaunch)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Flipped || !res.OK {
		t.Fatalf("%+v", res)
	}
	res, err = svc.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthOK}, SourceLaunch)
	if err != nil {
		t.Fatal(err)
	}
	if res.Flipped {
		t.Fatal("second ok must not flip")
	}

	n := countTopic(t, st, "harness.updated")
	if n != 1 {
		t.Fatalf("harness.updated count=%d want 1", n)
	}

	res, err = svc.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthMissing}, SourceProbe)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Flipped || res.OK {
		t.Fatalf("1→0 %+v", res)
	}
	if countTopic(t, st, "harness.updated") != 2 {
		t.Fatal("expected second harness.updated on 1→0")
	}
}

func TestLogoutDoesNotAlert(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()
	svc := New(st, nil, Options{})
	_, err := svc.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthOK}, SourceLaunch)
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.Logout(ctx, "claude", AuthModeSeparate, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != AuthMissing {
		t.Fatalf("%+v", out)
	}
	if countTopic(t, st, "auth.probe.failed") != 0 {
		t.Fatal("logout must not emit auth.probe.failed")
	}
}

func TestLogoutSeparateRemovesFilesKeepsSymlink(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	hdir := config.HarnessDir("claude")
	if err := os.MkdirAll(hdir, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(hdir, "auth.json")
	if err := os.WriteFile(real, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(hdir, ".credentials.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config.SecretsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretEnv("claude", map[string]string{"ANTHROPIC_API_KEY": "sk"}); err != nil {
		t.Fatal(err)
	}

	st := openStore(t)
	svc := New(st, nil, Options{})
	ctx := context.Background()
	if _, err := svc.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthOK}, SourceLaunch); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Logout(ctx, "claude", AuthModeSeparate, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(real); !os.IsNotExist(err) {
		t.Fatalf("real cred file should be gone: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink should remain: %v", err)
	}
	env, _ := LoadSecretEnv("claude")
	if env["ANTHROPIC_API_KEY"] != "" {
		t.Fatalf("secret env %v", env)
	}
	if countTopic(t, st, "auth.probe.failed") != 0 {
		t.Fatal("logout must not emit auth.probe.failed")
	}
	row, err := svc.GetState(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !row.AuthOK.Valid || row.AuthOK.Int64 != 0 {
		t.Fatalf("stamp %+v", row)
	}
}

func TestLocalOnlyDetails(t *testing.T) {
	t.Parallel()
	d := LocalOnlyDetails("cursor", AuthCaps{LoginModes: []string{LoginToken}, LocalCmd: "cursor-agent login"})
	if d["local_cmd"] != "cursor-agent login" {
		t.Fatalf("%v", d)
	}
	if d["hint"] == nil || d["hint"] == "" {
		t.Fatal("hint")
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func countTopic(t *testing.T, st *store.Store, topic string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic=?`, topic).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEarliestExpiry(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()
	svc := New(st, nil, Options{})
	soon := time.Now().UTC().Add(200 * time.Millisecond)
	_, err := svc.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthOK, ExpiresAt: &soon}, SourceProbe)
	if err != nil {
		t.Fatal(err)
	}
	name, at, err := svc.EarliestExpiry(ctx)
	if err != nil || name != "claude" || at.IsZero() {
		t.Fatalf("%s %v %v", name, at, err)
	}
}
