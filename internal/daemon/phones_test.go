package daemon_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/mobile"
	"github.com/trebi-ai/trebi/internal/relay"
	"github.com/trebi-ai/trebi/internal/store"
)

// fakePhones is a PhoneCloud with a gate the test turns.
type fakePhones struct{ allowed atomic.Bool }

func (p *fakePhones) SendRelay(string, []byte) error  { return nil }
func (p *fakePhones) CloseRelay(string, string) error { return nil }
func (p *fakePhones) DeviceID() string                { return "01JDIRECTTEST000000000DEV0" }
func (p *fakePhones) DeviceName() string              { return "Test Mac" }
func (p *fakePhones) MobileAllowed() bool             { return p.allowed.Load() }
func (p *fakePhones) BindHub(*relay.Hub)              {}
func (p *fakePhones) PairTarget() mobile.Target       { return mobile.Target{} }
func (p *fakePhones) MintClaim(context.Context, int, time.Duration) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (p *fakePhones) RevokeClient(context.Context, string) error { return nil }

func (p *fakePhones) DirectOptions(int) relay.DirectOptions {
	return relay.DirectOptions{Listen: "127.0.0.1:0"}
}

// TestDirectListenerFollowsGate proves that the direct port is open only
// while the gate is open and mobile.direct.enabled is on.
func TestDirectListenerFollowsGate(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{Shadow: true, DirectInterval: 10 * time.Millisecond, DirectRetry: 10 * time.Millisecond})
	pc := &fakePhones{}
	if err := cp.StartPhones(http.NotFoundHandler(), pc); err != nil {
		t.Fatal(err)
	}
	cp.Start(context.Background())
	defer cp.Stop()

	open := func() bool { return cp.Relay.DirectAddr() != nil }
	waitFor := func(what string, want bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if open() == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timeout: %s", what)
	}
	time.Sleep(50 * time.Millisecond)
	if open() {
		t.Fatal("listener open with the gate closed")
	}
	pc.allowed.Store(true)
	waitFor("listener opens", true)
	pc.allowed.Store(false)
	waitFor("listener closes", false)

	pc.allowed.Store(true)
	waitFor("listener opens again", true)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("mobile:\n  direct:\n    enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor("mobile.direct.enabled off closes it", false)
}
