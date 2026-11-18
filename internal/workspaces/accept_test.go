package workspaces_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func TestEmailStyle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := workspaces.New(openTest(t))
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "cmo", "AGENT.md"), "---\ndescription: Test agent.\nemail:\n  style:\n    accent: \"#0f766e\"\n---\n\nGrow.\n")
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

// TestJobEmailSenders: only event: email triggers of the job add send
// addresses. An empty allowlist, a non-email trigger, and an entry without
// "@" add none, and the result is sorted with no duplicates.
func TestJobEmailSenders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := workspaces.New(openTest(t))
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "ops", "AGENT.md"), "---\ndescription: Test agent.\n---\n\nOps.\n")
	writeFile(t, filepath.Join(root, "agents", "ops", "digest", "JOB.md"),
		"---\nharness: fake\ntriggers:\n"+
			"  - event: email\n    allowlist:\n      - B@X.io\n      - a@x.io\n      - a@x.io\n      - not-an-address\n"+
			"  - event: whatsapp\n    allowlist:\n      - wa@x.io\n"+
			"  - cron: \"0 3 * * *\"\n"+
			"---\n\nDo work.\n")
	writeFile(t, filepath.Join(root, "agents", "ops", "quiet", "JOB.md"),
		"---\nharness: fake\ntriggers:\n  - event: email\n    allowlist: []\n---\n\nDo work.\n")
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.JobEmailSenders(ctx, ws.ID, "agents/ops/digest")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a@x.io", "b@x.io"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("senders = %v, want %v", got, want)
	}

	got, err = svc.JobEmailSenders(ctx, ws.ID, "agents/ops/quiet")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty allowlist = %v, want none", got)
	}
}

// TestEmailAllowlistPatch: the workspace row and the reader see the patched
// list, case is folded, and a bad entry leaves the file alone.
func TestEmailAllowlistPatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := workspaces.New(openTest(t))
	root := t.TempDir()
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "trebi.yaml"),
		"id: "+ws.ID+"\nname: demo\nassistant:\n  name: Ellis\n")

	list := []string{" Emma@Example.com ", "ops@example.com", ""}
	patched, err := svc.Patch(ctx, ws.ID, workspaces.Patch{EmailAllowlist: &list})
	if err != nil {
		t.Fatal(err)
	}
	if got := patched.EmailAllowlist; len(got) != 2 || got[0] != "emma@example.com" || got[1] != "ops@example.com" {
		t.Fatalf("row allowlist = %v", got)
	}
	if got := svc.EmailAllowlist(patched); len(got) != 2 || got[0] != "emma@example.com" {
		t.Fatalf("reader allowlist = %v", got)
	}

	bad := []string{"@example.com"}
	if _, err := svc.Patch(ctx, ws.ID, workspaces.Patch{EmailAllowlist: &bad}); !errors.Is(err, workspaces.ErrEmailAllowlistInvalid) {
		t.Fatalf("domain entry = %v", err)
	}
	if got := svc.EmailAllowlist(patched); len(got) != 2 {
		t.Fatalf("a failed patch changed the file: %v", got)
	}

	cleared, err := svc.Patch(ctx, ws.ID, workspaces.Patch{ClearEmailAllowlist: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.EmailAllowlist) != 0 {
		t.Fatalf("cleared allowlist = %v", cleared.EmailAllowlist)
	}
	raw, err := os.ReadFile(filepath.Join(root, "trebi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "allowlist") {
		t.Fatalf("trebi.yaml keeps the block: %s", raw)
	}
	if !strings.Contains(string(raw), "name: Ellis") {
		t.Fatalf("trebi.yaml lost the assistant block: %s", raw)
	}
}

func TestAcceptRulesSkipsArchivedAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, ws := archiveFixture(t)
	agents, _, err := svc.AcceptRules(ctx, ws.ID)
	if err != nil || len(agents) != 2 {
		t.Fatalf("before archive: %v, %v", agents, err)
	}
	if _, err := svc.SetArchived(ctx, ws, archiveRow(t, svc, ws, "agents/peer"), true, time.Now()); err != nil {
		t.Fatal(err)
	}
	agents, _, err = svc.AcceptRules(ctx, ws.ID)
	if _, ok := agents["peer"]; ok || err != nil {
		t.Fatalf("after archive: %v, %v", agents, err)
	}
}
