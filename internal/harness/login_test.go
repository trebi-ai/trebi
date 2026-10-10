package harness

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
)

type fakeDriver struct {
	mu      sync.Mutex
	caps    map[string]AuthCaps
	probe   AuthStatus
	probeFn func(string) AuthStatus
	screen  string
	has     bool
	sent    []string
	started string
}

func (f *fakeDriver) ProbeAuth(_ context.Context, name string) AuthStatus {
	if f.probeFn != nil {
		return f.probeFn(name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probe
}
func (f *fakeDriver) AuthCaps(name string) AuthCaps {
	if c, ok := f.caps[name]; ok {
		return c
	}
	return AuthCaps{LoginModes: []string{LoginRelay, LoginToken}}
}
func (f *fakeDriver) ApplyAuthMode(string, string) error { return nil }
func (f *fakeDriver) LoginCommand(string, string, string) (string, []string, error) {
	return "fake-login", nil, nil
}
func (f *fakeDriver) ExtractLogin(_, screen string) LoginExtract {
	return extractFrom(screen)
}
func extractFrom(screen string) LoginExtract {
	ex := LoginExtract{}
	if i := strings.Index(screen, "sk-ant-oat01-"); i >= 0 {
		tok := screen[i:]
		if j := strings.IndexAny(tok, " \n\t"); j >= 0 {
			tok = tok[:j]
		}
		ex.Token = tok
		return ex
	}
	if len(screen) > 0 {
		ex.URL = "https://example.test/device"
		ex.UserCode = "ABCD-EFGH"
		ex.AwaitCode = true
	}
	return ex
}
func (f *fakeDriver) ExecLogout(context.Context, string) error { return nil }
func (f *fakeDriver) LoginAPIKey(context.Context, string, TokenLogin) error {
	return nil
}
func (f *fakeDriver) NewNamedSession(session, cwd string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = session
	f.has = true
	return nil
}
func (f *fakeDriver) ReadScreen(string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screen, nil
}
func (f *fakeDriver) Send(_ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeDriver) SendKey(string, string) error { return nil }
func (f *fakeDriver) KillSession(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.has = false
	return nil
}
func (f *fakeDriver) HasSession(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.has
}

func TestLoginSessionMachine(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	drv := &fakeDriver{probe: AuthStatus{Tool: "claude", Status: AuthOK}}
	svc := New(st, drv, Options{LoginMaxAge: 5 * time.Second})
	t.Cleanup(svc.StopLoginWatchers)
	ctx := context.Background()

	sess, err := svc.StartLogin(ctx, "claude", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.Active || sess.Phase != PhaseStarting {
		t.Fatalf("%+v", sess)
	}
	// second start is idempotent
	sess2, err := svc.StartLogin(ctx, "claude", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if sess2.Phase != sess.Phase {
		t.Fatalf("idempotent start changed phase %s → %s", sess.Phase, sess2.Phase)
	}

	drv.mu.Lock()
	drv.screen = "Visit https://example.test/device\nEnter code: ABCD-EFGH\n"
	drv.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := svc.GetLogin("claude")
		if got.Phase == PhaseAwaitingCode && got.URL != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := svc.GetLogin("claude")
	if got.URL == "" || got.UserCode != "ABCD-EFGH" {
		t.Fatalf("extract %+v", got)
	}

	_, err = svc.SubmitCode(ctx, "claude", "ABCD-EFGH")
	if err != nil {
		t.Fatal(err)
	}
	drv.mu.Lock()
	drv.has = false
	drv.mu.Unlock()

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got = svc.GetLogin("claude")
		if got.Phase == PhaseDone {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Phase != PhaseDone {
		t.Fatalf("want done, got %+v", got)
	}
}

func TestLoginCancelAndTimeout(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	drv := &fakeDriver{probe: AuthStatus{Tool: "claude", Status: AuthMissing}}
	svc := New(st, drv, Options{LoginMaxAge: 200 * time.Millisecond})
	t.Cleanup(svc.StopLoginWatchers)
	ctx := context.Background()
	if _, err := svc.StartLogin(ctx, "claude", "", ""); err != nil {
		t.Fatal(err)
	}
	svc.CancelLogin("claude")
	got := svc.GetLogin("claude")
	if got.Active || got.Phase != "" {
		t.Fatalf("cancel must delete the session: %+v", got)
	}

	st2 := openStore(t)
	drv2 := &fakeDriver{probe: AuthStatus{Tool: "claude", Status: AuthMissing}, has: true}
	svc2 := New(st2, drv2, Options{LoginMaxAge: 150 * time.Millisecond})
	t.Cleanup(svc2.StopLoginWatchers)
	if _, err := svc2.StartLogin(ctx, "claude", "", ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got = svc2.GetLogin("claude")
		if got.Phase == PhaseFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Phase != PhaseFailed {
		t.Fatalf("timeout want failed, got %+v", got)
	}
}

func TestLoginLocalOnly(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	drv := &fakeDriver{caps: map[string]AuthCaps{
		"cursor": {LoginModes: []string{LoginToken}, LocalCmd: "cursor-agent login"},
	}}
	svc := New(st, drv, Options{})
	_, err := svc.StartLogin(context.Background(), "cursor", "", "")
	if _, ok := IsLocalOnly(err); !ok {
		t.Fatalf("want local_only, got %v", err)
	}
}

func TestLoginCapturesSetupToken(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	if err := os.MkdirAll(home.SecretsDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	st := openStore(t)
	ctx := context.Background()
	auth := New(st, nil, Options{})
	_, err := auth.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthMissing}, SourceProbe)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := auth.BlockedNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range blocked {
		if n == "claude" {
			found = true
		}
	}
	if !found {
		t.Fatal("queue gate should block claude before login")
	}

	drv := &fakeDriver{
		probe: AuthStatus{Tool: "claude", Status: AuthMissing},
		probeFn: func(name string) AuthStatus {
			if env, _ := LoadSecretEnv(name); env["CLAUDE_CODE_OAUTH_TOKEN"] != "" {
				return AuthStatus{Tool: name, Status: AuthOK, Detail: "key (CLAUDE_CODE_OAUTH_TOKEN)"}
			}
			return AuthStatus{Tool: name, Status: AuthMissing}
		},
	}
	svc := New(st, drv, Options{LoginMaxAge: 5 * time.Second})
	t.Cleanup(svc.StopLoginWatchers)
	if _, err := svc.StartLogin(ctx, "claude", "", ""); err != nil {
		t.Fatal(err)
	}
	drv.mu.Lock()
	drv.screen = "Your token: sk-ant-oat01-goldenTOKEN\n"
	drv.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		env, _ := LoadSecretEnv("claude")
		if env["CLAUDE_CODE_OAUTH_TOKEN"] == "sk-ant-oat01-goldenTOKEN" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	env, _ := LoadSecretEnv("claude")
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat01-goldenTOKEN" {
		t.Fatalf("secrets env %v", env)
	}

	drv.mu.Lock()
	drv.has = false
	drv.mu.Unlock()
	deadline = time.Now().Add(2 * time.Second)
	var got LoginSession
	for time.Now().Before(deadline) {
		got = svc.GetLogin("claude")
		if got.Phase == PhaseDone {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Phase != PhaseDone {
		t.Fatalf("want done after setup-token capture, got %+v", got)
	}
	row, err := svc.GetState(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !row.AuthOK.Valid || row.AuthOK.Int64 != 1 {
		t.Fatalf("stamp %+v", row)
	}
	blocked, err = auth.BlockedNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range blocked {
		if n == "claude" {
			t.Fatal("queue gate must open after setup-token")
		}
	}
}

func TestSubmitCodeNoSession(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	svc := New(st, &fakeDriver{}, Options{})
	_, err := svc.SubmitCode(context.Background(), "claude", "ABCD")
	if err != ErrNoLogin() && err.Error() != ErrNoLogin().Error() {
		t.Fatalf("%v", err)
	}
}

func waitPhase(t *testing.T, svc *Service, name, phase string) LoginSession {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got LoginSession
	for time.Now().Before(deadline) {
		got = svc.GetLogin(name)
		if got.Phase == phase {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want phase %s, got %+v", phase, got)
	return got
}

func TestLoginCredChangedFinishes(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	var mu sync.Mutex
	probes := 0
	drv := &fakeDriver{probeFn: func(name string) AuthStatus {
		mu.Lock()
		probes++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return AuthStatus{Tool: name, Status: AuthOK}
	}}
	svc := New(st, drv, Options{LoginMaxAge: 5 * time.Second})
	t.Cleanup(svc.StopLoginWatchers)
	if _, err := svc.StartLogin(context.Background(), "grok", "", ""); err != nil {
		t.Fatal(err)
	}
	// The vendor CLI writes the file; the shell stays open.
	svc.CredChanged("grok")
	svc.CredChanged("grok")
	waitPhase(t, svc, "grok", PhaseDone)
	svc.CredChanged("grok")
	svc.StopLoginWatchers()
	mu.Lock()
	defer mu.Unlock()
	if probes != 1 {
		t.Fatalf("want one probe, got %d", probes)
	}
	if n := countTopic(t, st, "harness.login.updated"); n < 2 {
		t.Fatalf("want login events on the bus, got %d", n)
	}
}

// A logout also changes the files. A failed probe keeps the session.
func TestLoginCredChangedKeepsOnFail(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	drv := &fakeDriver{probe: AuthStatus{Tool: "grok", Status: AuthMissing}}
	svc := New(st, drv, Options{LoginMaxAge: 5 * time.Second})
	t.Cleanup(svc.StopLoginWatchers)
	if _, err := svc.StartLogin(context.Background(), "grok", "", ""); err != nil {
		t.Fatal(err)
	}
	svc.CredChanged("grok")
	time.Sleep(100 * time.Millisecond)
	if got := svc.GetLogin("grok"); !got.Active || got.Phase == PhaseFailed {
		t.Fatalf("session ended: %+v", got)
	}
	// The watcher still reads the screen.
	drv.mu.Lock()
	drv.screen = "Enter code: ABCD-1234"
	drv.mu.Unlock()
	waitPhase(t, svc, "grok", PhaseAwaitingCode)
	drv.mu.Lock()
	drv.probe = AuthStatus{Tool: "grok", Status: AuthOK}
	drv.mu.Unlock()
	svc.CredChanged("grok")
	waitPhase(t, svc, "grok", PhaseDone)
}

func TestLoginCLIExitFinishes(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	drv := &fakeDriver{probe: AuthStatus{Tool: "codex", Status: AuthOK}}
	svc := New(st, drv, Options{LoginMaxAge: 5 * time.Second})
	t.Cleanup(svc.StopLoginWatchers)
	if _, err := svc.StartLogin(context.Background(), "codex", "", ""); err != nil {
		t.Fatal(err)
	}
	drv.mu.Lock()
	drv.has = false
	drv.mu.Unlock()
	waitPhase(t, svc, "codex", PhaseDone)
	// A done session still answers for doneRetain.
	time.Sleep(1500 * time.Millisecond)
	if got := svc.GetLogin("codex"); got.Phase != PhaseDone {
		t.Fatalf("done session must stay, got %+v", got)
	}
	// A new start replaces the ended session.
	sess, err := svc.StartLogin(context.Background(), "codex", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Phase != PhaseStarting {
		t.Fatalf("restart %+v", sess)
	}
}
