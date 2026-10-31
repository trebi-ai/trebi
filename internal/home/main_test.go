package home

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain uses a temporary HOME and TREBI_HOME. It cannot use hometest,
// because hometest imports this package.
func TestMain(m *testing.M) {
	// A test binary that a test starts again keeps the env of its parent.
	if os.Getenv("TREBI_TEST_ISOLATED") != "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "trebi-home-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	user := filepath.Join(dir, "user")
	trebi := filepath.Join(user, ".trebi")
	if err := os.MkdirAll(trebi, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("HOME", user)
	_ = os.Setenv("TREBI_HOME", trebi)
	_ = os.Setenv("TREBI_TEST_ISOLATED", "1")
	code := m.Run()
	_ = os.RemoveAll(dir) // a leftover temp dir is harmless
	os.Exit(code)
}
