package eval

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFileChecks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !fileStatCheck("exists", p, true).Pass {
		t.Fatal("exists")
	}
	if fileStatCheck("absent", p, false).Pass {
		t.Fatal("absent")
	}
	if !fileTextCheck("has", p, "hello", true).Pass {
		t.Fatal("contains")
	}
	if !fileTextCheck("no", p, "sk-eval", false).Pass {
		t.Fatal("not contains")
	}
	if fileTextCheck("missing", p, "nope", true).Pass {
		t.Fatal("should fail")
	}
}

func TestJQTruthy(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	t.Parallel()
	ok, _, err := jqTruthy(".a == 1", []byte(`{"a":1}`))
	if err != nil || !ok {
		t.Fatalf("true: %v %v", ok, err)
	}
	ok, _, err = jqTruthy(".a == 2", []byte(`{"a":1}`))
	if err != nil || ok {
		t.Fatalf("false: %v %v", ok, err)
	}
}

func TestSplitCommand(t *testing.T) {
	t.Parallel()
	got := splitCommand(`trebi job validate hello`)
	if len(got) != 4 || got[3] != "hello" {
		t.Fatalf("%v", got)
	}
	got = splitCommand(`trebi config show -k 'defaults.command' --json`)
	if len(got) != 6 || got[4] != "defaults.command" {
		t.Fatalf("%v", got)
	}
}

func TestDaemonOwned(t *testing.T) {
	t.Parallel()
	if !daemonOwned("logs/trebid.log") || !daemonOwned("trebi.db") {
		t.Fatal("owned")
	}
	if daemonOwned("hack.sh") {
		t.Fatal("not owned")
	}
}

func TestHashTree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := hashTree(dir)
	if err != nil || h["a"] == "" {
		t.Fatalf("%v %v", h, err)
	}
}
