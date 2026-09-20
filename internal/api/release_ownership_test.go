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

// The gate must run on main. It is the only proof a commit is releasable,
// because trebi-pro builds from a pinned tag of this repository.
func TestCheckRunsOnMain(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "check.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "branches-ignore: [main]") {
		t.Fatal("check.yml must run on main; no release workflow runs it there")
	}
}
