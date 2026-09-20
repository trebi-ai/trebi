package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This repository does not release. trebi-pro tags the version, creates the
// release here, and uploads the one binary as trebi_* with SHA256SUMS.
// A release.yml in this repository would tag against that owner.
func TestRepositoryDoesNotRelease(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(filepath.Join("..", "..", ".github", "workflows", "release.yml")); !os.IsNotExist(err) {
		t.Fatal("trebi must not have a release workflow; trebi-pro owns tags and releases")
	}
}

// A commit on main must pass the gate before it is dispatched, because
// trebi-pro tags it and releases it unseen. dispatch.yml calls check.yml and
// the dispatch job waits on it.
func TestMainIsGatedBeforeDispatch(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "dispatch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if !strings.Contains(src, "uses: ./.github/workflows/check.yml") {
		t.Fatal("dispatch.yml must run the gate")
	}
	if !strings.Contains(src, "needs: [gate]") {
		t.Fatal("dispatch.yml must dispatch only after the gate passes")
	}
	if !strings.Contains(src, "branches: [main]") {
		t.Fatal("dispatch.yml must trigger on main")
	}
}
