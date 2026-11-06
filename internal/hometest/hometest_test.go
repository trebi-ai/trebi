package hometest

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	for _, name := range vendorBins() {
		out, err := exec.Command(name, "--version").CombinedOutput()
		if err == nil || !strings.Contains(string(out), "hometest") {
			t.Errorf("%s ran: %v %s", name, err, out)
		}
	}
	if !slices.Contains(vendorBins(), "opencode2") || !slices.Contains(vendorBins(), "cursor-agent") {
		t.Fatalf("vendorBins misses a harness: %v", vendorBins())
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
