package assistant

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/runs"
)

func TestCompose(t *testing.T) {
	t.Parallel()
	got := compose(Settings{Context: "Be brief."}, WorkspaceInfo{Name: "home", ID: "ws1", Path: "/w"}, "fact")
	for _, want := range []string{"## Role", runs.ChatStyleLine, "Name: home", "## Context\n\nBe brief.", "## Memory\n\nfact"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, runs.ChatStyleLine) > strings.Index(got, "## Workspace") {
		t.Fatal("the chat line belongs to the role section")
	}
}
