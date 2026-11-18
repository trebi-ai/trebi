package visor_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/visor"
	"github.com/trebi-ai/trebi/internal/waittest"
)

// The visor lists the fake harness and runs its login and logout with the
// cmd/fake program.
func TestFakeHarnessLogin(t *testing.T) {
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "fake"), "github.com/trebi-ai/trebi/cmd/fake")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake: %v\n%s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := home.Base()
	t.Cleanup(func() { home.SetBase(prev) })
	base := t.TempDir()
	o := agentvisor.Options{Home: filepath.Join(base, "visor"), Client: "test"}
	visor.Configure(&o)
	v, err := agentvisor.New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close(context.Background())
	ctx := t.Context()

	list, err := v.Harnesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var fake *agentvisor.Harness
	names := map[string]bool{}
	for i := range list {
		names[list[i].Name] = true
		if list[i].Name == "fake" {
			fake = &list[i]
		}
	}
	if fake == nil || !names["opencode2"] {
		t.Fatalf("list: %v", names)
	}
	if !fake.Installed || fake.Source != agentvisor.SourceSystem || fake.Installable || fake.Auth != agentvisor.AuthMissing {
		t.Fatalf("fake: %+v", *fake)
	}

	l, err := v.LoginHarness(ctx, "fake", agentvisor.LoginSpec{})
	if err != nil {
		t.Fatal(err)
	}
	l = waitLogin(t, v, func(l agentvisor.Login) bool { return l.Phase == agentvisor.LoginCode })
	if l.URL != "https://example.invalid/fake/device" || l.Code != "FAKE-0000" {
		t.Fatalf("login: %+v", l)
	}
	if _, err := v.HarnessLoginCode(ctx, "fake", "1234"); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, v, func(l agentvisor.Login) bool { return l.Phase == agentvisor.LoginDone })
	marker := filepath.Join(home.HarnessDir("fake"), "auth.json")
	if !strings.HasPrefix(marker, base) {
		t.Fatalf("marker %s is not under %s", marker, base)
	}
	if b, err := os.ReadFile(marker); err != nil || !strings.Contains(string(b), "1234") {
		t.Fatalf("marker: %s %v", b, err)
	}
	if h, err := v.Harness(ctx, "fake"); err != nil || h.Auth != agentvisor.AuthOK {
		t.Fatalf("auth after login: %+v %v", h.Auth, err)
	}

	if err := v.LogoutHarness(ctx, "fake"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker after logout: %v", err)
	}
}

func waitLogin(t *testing.T, v *agentvisor.Visor, ok func(agentvisor.Login) bool) agentvisor.Login {
	t.Helper()
	deadline := time.Now().Add(waittest.D(20 * time.Second))
	for {
		l, err := v.HarnessLogin(t.Context(), "fake")
		if err == nil && ok(l) {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("login: %+v %v", l, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The visor looks up harness programs on the user path, not the short
// LaunchAgent path.
func TestConfigureHarnessesPath(t *testing.T) {
	t.Setenv("PATH", "/x/bin")
	prev := home.Base()
	t.Cleanup(func() { home.SetBase(prev) })
	o := agentvisor.Options{Home: filepath.Join(t.TempDir(), "visor"), Client: "test"}
	visor.Configure(&o)
	if !strings.HasPrefix(o.Harness.Path, "/x/bin:") || !strings.Contains(o.Harness.Path, "/opt/homebrew/bin") {
		t.Errorf("Harness.Path = %q", o.Harness.Path)
	}
}
