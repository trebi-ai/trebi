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

// trebi-pro asks this repository to cut each release tag, because an App
// token is refused when the asking run came from a workflow-touching push.
func TestTagWorkflowAnswersTrebiPro(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "tag.yml"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if !strings.Contains(src, "types: [cut-tag]") {
		t.Fatal("tag.yml must answer the cut-tag dispatch")
	}
	// The Actions token is refused a tag that points at a commit which
	// changes .github/workflows. Only the App token carries that right.
	if !strings.Contains(src, "create-github-app-token") {
		t.Fatal("tag.yml must push with an App token")
	}
	if !strings.Contains(src, "token: ${{ steps.app.outputs.token }}") {
		t.Fatal("checkout must use the App token, or the push uses the Actions token")
	}
}
