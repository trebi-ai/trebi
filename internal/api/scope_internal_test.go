package api

import (
	"context"
	"testing"

	"github.com/trebi-ai/trebi/internal/tools"
)

func TestScopeFromAgentName(t *testing.T) {
	t.Parallel()
	ctx := tools.WithActor(context.Background(), tools.Actor{Kind: "agent", Job: "cmo", WorkspaceID: "ws_1"})
	s, ok := scopeFrom(ctx)
	if !ok || s.agent != "cmo" || s.dir != "agents/cmo" {
		t.Fatalf("scope = %+v ok=%t want agent cmo, dir agents/cmo", s, ok)
	}
	for _, job := range []string{"/foo", "cmo/weekly", "agents/cmo", "assistant", ""} {
		ctx := tools.WithActor(context.Background(), tools.Actor{Kind: "agent", Job: job})
		if _, ok := scopeFrom(ctx); ok {
			t.Errorf("job %q is scoped", job)
		}
	}
}
