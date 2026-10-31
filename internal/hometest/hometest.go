// Package hometest keeps a test binary away from the real Trebi home, the
// real user home, and the real vendor programs.
package hometest

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

// vendorBins are the harness programs that a test must never start.
//
//nolint:gochecknoglobals // fixed list
var vendorBins = []string{"agent", "claude", "codex", "copilot", "cursor-agent", "gemini", "grok", "omp", "opencode", "pi"}

// vendorEnv points a vendor program at a real config folder.
//
//nolint:gochecknoglobals // fixed list
var vendorEnv = []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "OPENCODE_CONFIG_DIR", "PI_CODING_AGENT_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"}

// Main runs the tests of a package in a temporary home and exits.
func Main(m *testing.M) { os.Exit(Run(m)) }

// Run runs the tests of a package in a temporary home and returns the exit
// code. The live build tags run the tests unchanged.
func Run(m *testing.M) int {
	if live {
		return m.Run()
	}
	// A test binary that a test starts again keeps the env of its parent.
	if os.Getenv(isolatedEnv) == "" {
		dir, err := isolate()
		if err != nil {
			fmt.Fprintln(os.Stderr, "hometest:", err)
			return 1
		}
		defer os.RemoveAll(dir) // a leftover temp dir is harmless
	}
	real := realBase()
	if err := guard(real); err != nil {
		fmt.Fprintln(os.Stderr, "hometest:", err)
		return 1
	}
	code := m.Run()
	if err := guard(real); err != nil {
		fmt.Fprintln(os.Stderr, "hometest:", err)
		return 1
	}
	return code
}

// isolate sets a temporary HOME and TREBI_HOME and puts failing stubs of the
// vendor programs first on PATH. It keeps the Go caches of the real home.
func isolate() (string, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	gopath := cmp.Or(os.Getenv("GOPATH"), filepath.Join(userHome, "go"))
	dir, err := os.MkdirTemp("", "trebi-test-")
	if err != nil {
		return "", err
	}
	tmpUser := filepath.Join(dir, "user")
	trebi := filepath.Join(tmpUser, ".trebi")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(trebi, 0o755); err != nil {
		return dir, err
	}
	if err := writeStubs(bin); err != nil {
		return dir, err
	}
	env := [][2]string{
		{"GOCACHE", cmp.Or(os.Getenv("GOCACHE"), filepath.Join(cache, "go-build"))},
		{"GOPATH", gopath},
		{"GOMODCACHE", cmp.Or(os.Getenv("GOMODCACHE"), filepath.Join(filepath.SplitList(gopath)[0], "pkg", "mod"))},
		{"HOME", tmpUser},
		{"TREBI_HOME", trebi},
		{isolatedEnv, "1"},
		{"PATH", bin + string(os.PathListSeparator) + os.Getenv("PATH")},
	}
	for _, kv := range env {
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			return dir, err
		}
	}
	for _, k := range vendorEnv {
		if err := os.Unsetenv(k); err != nil {
			return dir, err
		}
	}
	home.SetBase(trebi)
	return dir, nil
}

// writeStubs writes a program for each vendor name that fails at once.
func writeStubs(bin string) error {
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	stub := []byte("#!/bin/sh\necho \"hometest: a test must not start a vendor program\" >&2\nexit 127\n")
	for _, name := range vendorBins {
		if err := os.WriteFile(filepath.Join(bin, name), stub, 0o755); err != nil { //nolint:gosec // the stub must run
			return err
		}
	}
	return nil
}

// isolatedEnv marks a process env that isolate already set.
const isolatedEnv = "TREBI_TEST_ISOLATED"

// realBase is the Trebi home of the real user. The account record does not
// change with HOME.
func realBase() string {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return filepath.Join("/", ".trebi-unknown-user")
	}
	return filepath.Join(u.HomeDir, ".trebi")
}

// guard fails when home.Base is the real Trebi home or a folder in it.
func guard(real string) error {
	base, err := filepath.Abs(home.Base())
	if err != nil {
		return err
	}
	if base == real || strings.HasPrefix(base, real+string(filepath.Separator)) {
		return errors.New("home.Base() is the real Trebi home " + real)
	}
	return nil
}
