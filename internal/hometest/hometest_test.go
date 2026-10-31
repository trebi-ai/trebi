package hometest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

func TestMain(m *testing.M) { Main(m) }

func TestIsolated(t *testing.T) {
	real := realBase()
	if err := guard(real); err != nil {
		t.Fatal(err)
	}
	if h, _ := os.UserHomeDir(); filepath.Join(h, ".trebi") == real {
		t.Fatalf("HOME is the real user home %s", h)
	}
	out, err := exec.Command("claude", "--version").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "hometest") {
		t.Fatalf("claude ran: %v %s", err, out)
	}
}

func TestGuardRejectsRealHome(t *testing.T) {
	real := realBase()
	t.Cleanup(func() { home.SetBase(os.Getenv("TREBI_HOME")) })
	for _, dir := range []string{real, filepath.Join(real, "visor")} {
		home.SetBase(dir)
		if guard(real) == nil {
			t.Fatalf("guard accepts %s", dir)
		}
	}
}
