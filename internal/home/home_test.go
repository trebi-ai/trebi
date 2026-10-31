package home

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBasePrecedence(t *testing.T) {
	t.Cleanup(func() { SetBase("") })

	// No SetBase, no env → ~/.trebi.
	SetBase("")
	t.Setenv("TREBI_HOME", "")
	h, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no user home dir")
	}
	if got, want := Base(), filepath.Join(h, ".trebi"); got != want {
		t.Fatalf("Base() = %q, want %q", got, want)
	}

	// TREBI_HOME wins over ~/.trebi.
	t.Setenv("TREBI_HOME", "/env/trebi")
	if got := Base(); got != "/env/trebi" {
		t.Fatalf("Base() = %q, want /env/trebi", got)
	}

	// SetBase wins over TREBI_HOME.
	SetBase("/set/trebi")
	if got := Base(); got != "/set/trebi" {
		t.Fatalf("Base() = %q, want /set/trebi", got)
	}
}

func TestEnsureDirsSecretsMode(t *testing.T) {
	t.Parallel()
	SetBase(t.TempDir())
	t.Cleanup(func() { SetBase("") })

	if err := EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(SecretsDir())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("secrets dir mode = %o, want 0700", fi.Mode().Perm())
	}

	// EnsureDirs fixes a wrong existing mode.
	if err := os.Chmod(SecretsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(SecretsDir())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("secrets dir mode after fix = %o, want 0700", fi.Mode().Perm())
	}
}

func TestMoveHarnessDirs(t *testing.T) {
	base := t.TempDir()
	SetBase(base)
	t.Cleanup(func() { SetBase("") })
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(base, "harness", "claude", ".credentials.json"))
	write(filepath.Join(base, "harness", "codex", "auth.json"))
	write(filepath.Join(HarnessDir("codex"), "auth.json"))
	moved, err := MoveHarnessDirs()
	if err != nil || !slices.Equal(moved, []string{"claude"}) {
		t.Fatalf("moved %v, %v", moved, err)
	}
	if _, err := os.Stat(filepath.Join(HarnessDir("claude"), ".credentials.json")); err != nil {
		t.Fatalf("claude not moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "harness", "codex", "auth.json")); err != nil {
		t.Fatalf("a home that is there already must keep the old one: %v", err)
	}
	if moved, err := MoveHarnessDirs(); err != nil || len(moved) != 0 {
		t.Fatalf("second move: %v, %v", moved, err)
	}
}
