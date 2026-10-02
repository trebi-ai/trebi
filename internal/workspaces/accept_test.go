package workspaces_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func TestEmailStyle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := workspaces.New(openTest(t))
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "cmo", "AGENT.md"), "---\nemail:\n  style:\n    accent: \"#0f766e\"\n---\n\nGrow.\n")
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	ov := wire.Override{Email: &wire.EmailOverride{Style: &job.StyleYAML{Accent: "#2563eb", Footer: "Acme"}}}
	if _, err := svc.SetAssistantOverride(ctx, ws.ID, ov, ""); err != nil {
		t.Fatal(err)
	}

	got, err := svc.EmailStyle(ctx, ws.ID, "")
	if err != nil || got != (job.StyleYAML{Accent: "#2563eb", Footer: "Acme"}) {
		t.Fatalf("workspace style = %+v, %v", got, err)
	}
	got, err = svc.EmailStyle(ctx, ws.ID, "cmo")
	if err != nil || got != (job.StyleYAML{Accent: "#0f766e", Footer: "Acme"}) {
		t.Fatalf("agent style = %+v, %v", got, err)
	}

	ov.Email.Style.Accent = "teal"
	if _, err := svc.SetAssistantOverride(ctx, ws.ID, ov, ""); err == nil {
		t.Fatal("bad accent saved")
	}
}
