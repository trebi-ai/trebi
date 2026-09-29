package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This repository does not release. trebi-pro owns the version series and
// creates the release here, uploading the one binary as trebi_* with
// SHA256SUMS. A release.yml here would tag against that owner.
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

// trebi-pro cuts each release tag here after its builds pass. No workflow in
// this repository creates a tag.
func TestRepositoryDoesNotTag(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "git tag") || strings.Contains(string(data), "refs/tags/") {
			t.Fatalf("%s creates a tag; trebi-pro owns the tags", filepath.Base(p))
		}
	}
}
