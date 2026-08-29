package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

type probeStub struct {
	mu      sync.Mutex
	n       int
	status  harness.AuthStatus
	probeFn func() harness.AuthStatus
	caps    harness.AuthCaps
}

func (p *probeStub) ProbeAuth(context.Context, string) harness.AuthStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	if p.probeFn != nil {
		return p.probeFn()
	}
	return p.status
}
func (p *probeStub) AuthCaps(string) harness.AuthCaps { return p.caps }
func (p *probeStub) ApplyAuthMode(string, string) error {
	return nil
}
func (p *probeStub) LoginCommand(string, string) (string, []string, error) {
	return "", nil, nil
}
func (p *probeStub) ExtractLogin(string, string) harness.LoginExtract {
	return harness.LoginExtract{NoExtract: true}
}
func (p *probeStub) ExecLogout(context.Context, string) error { return nil }
func (p *probeStub) LoginAPIKey(context.Context, string, string) error {
	return nil
}
func (p *probeStub) NewNamedSession(string, string) error { return nil }
func (p *probeStub) ReadScreen(string) (string, error)    { return "", nil }
func (p *probeStub) Send(string, string) error            { return nil }
func (p *probeStub) SendKey(string, string) error         { return nil }
func (p *probeStub) KillSession(string) error             { return nil }
func (p *probeStub) HasSession(string) bool               { return false }

func (p *probeStub) probes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

func TestAuthProbeExpiryWakes(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Minute)
	if _, err := harness.Stamp(ctx, st, harness.AuthStatus{
		Tool: "claude", Status: harness.AuthOK, ExpiresAt: &past,
	}, harness.SourceProbe); err != nil {
		t.Fatal(err)
	}

	stub := &probeStub{status: harness.AuthStatus{Tool: "claude", Status: harness.AuthExpired}}
	cp := daemon.New(st, daemon.Options{Shadow: true, AuthProbeInterval: time.Hour})
	cp.Harness = harness.New(st, stub, harness.Options{})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		row, err := cp.Harness.GetState(ctx, "claude")
		if err == nil && row.Status == harness.AuthExpired {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expiry probe did not stamp expired")
}

func TestAuthProbeExpiryRefreshStaysOK(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Minute)
	future := time.Now().UTC().Add(time.Hour)
	if _, err := harness.Stamp(ctx, st, harness.AuthStatus{
		Tool: "claude", Status: harness.AuthOK, ExpiresAt: &past,
	}, harness.SourceProbe); err != nil {
		t.Fatal(err)
	}

	stub := &probeStub{status: harness.AuthStatus{
		Tool: "claude", Status: harness.AuthOK, ExpiresAt: &future,
	}}
	cp := daemon.New(st, daemon.Options{Shadow: true, AuthProbeInterval: time.Hour})
	cp.Harness = harness.New(st, stub, harness.Options{})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if stub.probes() > 0 {
			row, err := cp.Harness.GetState(ctx, "claude")
			if err != nil {
				t.Fatal(err)
			}
			if !row.AuthOK.Valid || row.AuthOK.Int64 != 1 {
				t.Fatalf("refresh should stay ok: %+v", row)
			}
			var n int
			if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic=?`, "auth.probe.failed").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatal("silent refresh must not alert")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expiry probe did not run")
}

func TestAuthProbeCredWatcher(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cred := filepath.Join(dir, "harness", "claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(cred), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := &probeStub{
		status: harness.AuthStatus{Tool: "claude", Status: harness.AuthOK},
		caps:   harness.AuthCaps{CredFile: cred, LoginModes: []string{harness.LoginRelay}},
	}
	cp := daemon.New(st, daemon.Options{Shadow: true, AuthProbeInterval: time.Hour})
	cp.Harness = harness.New(st, stub, harness.Options{})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(cred, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if stub.probes() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("cred watcher did not re-probe")
}

func TestAuthProbeCredWatcherDirCred(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// A directory-shaped CredFile (copilot) must be watched itself, not its parent.
	credDir := filepath.Join(dir, ".copilot")
	if err := os.MkdirAll(credDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := &probeStub{
		status: harness.AuthStatus{Tool: "copilot", Status: harness.AuthOK},
		caps:   harness.AuthCaps{CredFile: credDir, LoginModes: []string{harness.LoginToken}},
	}
	cp := daemon.New(st, daemon.Options{Shadow: true, AuthProbeInterval: time.Hour})
	cp.Harness = harness.New(st, stub, harness.Options{})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(credDir, "token.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if stub.probes() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("cred watcher did not re-probe on dir-shaped CredFile")
}

func TestAuthProbeCredWatcherSkipsHome(t *testing.T) {
	dir := t.TempDir()
	userHome := filepath.Join(dir, "userhome")
	if err := os.MkdirAll(userHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", userHome)
	base := filepath.Join(dir, "trebihome")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	home.SetBase(base)
	t.Setenv("TREBI_HOME", base)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(base, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Missing CredFile whose parent is the user home: the watcher must refuse
	// the home dir, so an event there must not probe.
	stub := &probeStub{
		status: harness.AuthStatus{Tool: "copilot", Status: harness.AuthOK},
		caps: harness.AuthCaps{
			CredFile:   filepath.Join(userHome, ".copilot"),
			LoginModes: []string{harness.LoginToken},
		},
	}
	cp := daemon.New(st, daemon.Options{Shadow: true, AuthProbeInterval: time.Hour})
	cp.Harness = harness.New(st, stub, harness.Options{})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(userHome, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := stub.probes(); n != 0 {
		t.Fatalf("watcher observed the user home: %d probes", n)
	}
}
