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
func (p *probeStub) LoginCommand(string, string, string) (string, []string, error) {
	return "", nil, nil
}
func (p *probeStub) ExtractLogin(string, string) harness.LoginExtract {
	return harness.LoginExtract{NoExtract: true}
}
func (p *probeStub) ExecLogout(context.Context, string) error { return nil }
func (p *probeStub) LoginAPIKey(context.Context, string, harness.TokenLogin) error {
	return nil
}
func (p *probeStub) NewNamedSession(string, string) error { return nil }
func (p *probeStub) ReadScreen(string) (string, error)    { return "", nil }
func (p *probeStub) Send(string, string) error            { return nil }
func (p *probeStub) SendKey(string, string) error         { return nil }
func (p *probeStub) KillSession(string) error             { return nil }
func (p *probeStub) HasSession(string) bool               { return false }

// enableHarnesses writes a config.yaml that turns names on.
func enableHarnesses(t *testing.T, dir string, names ...string) {
	t.Helper()
	cfg := "tools:\n"
	for _, n := range names {
		cfg += "  " + n + ": {enabled: true}\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

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
	if _, err := harness.New(st, nil, harness.Options{}).Stamp(ctx, harness.AuthStatus{
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
	if _, err := harness.New(st, nil, harness.Options{}).Stamp(ctx, harness.AuthStatus{
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

	enableHarnesses(t, dir, "claude")
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

	// Write until a probe lands: a busy runner can arm the watch late.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := os.WriteFile(cred, []byte(`{"ok":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		if stub.probes() > 0 {
			return
		}
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

	enableHarnesses(t, dir, "copilot")
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

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := os.WriteFile(filepath.Join(credDir, "token.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		if stub.probes() > 0 {
			return
		}
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

// A probe that cannot refresh the credential leaves auth_expires_at in the
// past. The expiry timer must not re-arm on that same date: doing so spins the
// daemon at full CPU until the credential is renewed.
func TestAuthProbeExpiredDoesNotSpin(t *testing.T) {
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
	past := time.Now().UTC().Add(-42 * 24 * time.Hour)
	if _, err := harness.New(st, nil, harness.Options{}).Stamp(ctx, harness.AuthStatus{
		Tool: "gemini", Status: harness.AuthOK, ExpiresAt: &past,
	}, harness.SourceProbe); err != nil {
		t.Fatal(err)
	}

	// The probe keeps reporting expired and re-stamps the same past expiry.
	stub := &probeStub{probeFn: func() harness.AuthStatus {
		return harness.AuthStatus{Tool: "gemini", Status: harness.AuthExpired, ExpiresAt: &past}
	}}
	cp := daemon.New(st, daemon.Options{Shadow: true, AuthProbeInterval: time.Hour})
	cp.Harness = harness.New(st, stub, harness.Options{})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	time.Sleep(time.Second)
	if n := stub.probes(); n > 3 {
		t.Fatalf("expired credential probed %d times in 1s; expiry timer is spinning", n)
	}
}

func TestAuthProbeSkipsHarnessOff(t *testing.T) {
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
	enableHarnesses(t, dir, "claude")
	for _, n := range []string{"claude", "pi"} {
		if _, err := harness.New(st, nil, harness.Options{}).Stamp(ctx, harness.AuthStatus{
			Tool: n, Status: harness.AuthMissing,
		}, harness.SourceProbe); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	probed := map[string]int{}
	stub := &probeStub{}
	cp := daemon.New(st, daemon.Options{Shadow: true, AuthProbeInterval: time.Hour})
	cp.Harness = harness.New(st, &namedProbe{probeStub: stub, seen: func(n string) {
		mu.Lock()
		probed[n]++
		mu.Unlock()
	}}, harness.Options{})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := probed["claude"]
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if probed["claude"] == 0 {
		t.Fatal("blocked harness that is on was not probed")
	}
	if probed["pi"] != 0 {
		t.Fatalf("harness that is off was probed %d times", probed["pi"])
	}
}

// namedProbe records the harness name of each probe.
type namedProbe struct {
	*probeStub
	seen func(string)
}

func (p *namedProbe) ProbeAuth(ctx context.Context, name string) harness.AuthStatus {
	p.seen(name)
	return harness.AuthStatus{Tool: name, Status: harness.AuthMissing}
}
