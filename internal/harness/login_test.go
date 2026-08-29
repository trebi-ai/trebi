package harness

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeDriver struct {
	mu      sync.Mutex
	caps    map[string]AuthCaps
	probe   AuthStatus
	screen  string
	has     bool
	sent    []string
	started string
}

func (f *fakeDriver) ProbeAuth(context.Context, string) AuthStatus { return f.probe }
func (f *fakeDriver) AuthCaps(name string) AuthCaps {
	if c, ok := f.caps[name]; ok {
		return c
	}
	return AuthCaps{LoginModes: []string{LoginRelay, LoginToken}}
}
func (f *fakeDriver) ApplyAuthMode(string, string) error { return nil }
func (f *fakeDriver) LoginCommand(string, string) (string, []string, error) {
	return "fake-login", nil, nil
}
func (f *fakeDriver) ExtractLogin(_, screen string) LoginExtract {
	return extractFrom(screen)
}
func extractFrom(screen string) LoginExtract {
	ex := LoginExtract{}
	if len(screen) > 0 {
		ex.URL = "https://example.test/device"
		ex.UserCode = "ABCD-EFGH"
		ex.AwaitCode = true
	}
	return ex
}
func (f *fakeDriver) ExecLogout(context.Context, string) error { return nil }
func (f *fakeDriver) LoginAPIKey(context.Context, string, string) error {
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
	ctx := context.Background()

	sess, err := svc.StartLogin(ctx, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.Active || sess.Phase != PhaseStarting {
		t.Fatalf("%+v", sess)
	}
	// second start is idempotent
	sess2, err := svc.StartLogin(ctx, "claude", "")
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

func TestLoginLocalOnly(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	drv := &fakeDriver{caps: map[string]AuthCaps{
		"cursor": {LoginModes: []string{LoginToken}, LocalCmd: "cursor-agent login"},
	}}
	svc := New(st, drv, Options{})
	_, err := svc.StartLogin(context.Background(), "cursor", "")
	if _, ok := IsLocalOnly(err); !ok {
		t.Fatalf("want local_only, got %v", err)
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
