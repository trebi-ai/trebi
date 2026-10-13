package connectors_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
)

func TestCommandLinks(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ctx := context.Background()
	_, svc := installStack(t, "demo", nil)
	dir := home.ConnectorCommandsDir()
	link := filepath.Join(dir, "demo-cli")
	target := func() string {
		t.Helper()
		got, err := os.Readlink(link)
		if err != nil {
			return ""
		}
		return got
	}

	if _, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo", Version: "1.0.0", Track: "exact"}); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home.PackageDir("demo", "1.0.0"), "bin", "demo-cli"); target() != want {
		t.Fatalf("link = %q, want %q", target(), want)
	}
	if err := os.WriteFile(filepath.Join(dir, "mine"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nowhere", filepath.Join(dir, "stale")); err != nil {
		t.Fatal(err)
	}

	if err := svc.Update(ctx, "demo", "1.0.1"); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home.PackageDir("demo", "1.0.1"), "bin", "demo-cli"); target() != want {
		t.Fatalf("link after update = %q, want %q", target(), want)
	}
	if out, err := exec.Command(link).Output(); err != nil || string(out) != "demo-cli 1.0.1\n" {
		t.Fatalf("run link = %q %v", out, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale link stays: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mine")); err != nil {
		t.Fatalf("a file that is not a link went: %v", err)
	}

	if err := svc.Delete(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("link stays after delete: %v", err)
	}
}
