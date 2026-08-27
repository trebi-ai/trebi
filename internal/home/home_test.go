package home

import (
	"os"
	"path/filepath"
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
