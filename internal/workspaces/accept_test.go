package workspaces_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
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

// TestJobEmailSenders: only routes of routes/email.yaml that name the job
// add send addresses. An empty allowlist, a route of another connection, and
// an entry without "@" add none, and the result is sorted with no duplicates.
func TestJobEmailSenders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := workspaces.New(openTest(t))
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "ops", "AGENT.md"), "---\ndescription: Test agent.\n---\n\nOps.\n")
	writeFile(t, filepath.Join(root, "agents", "ops", "digest", "JOB.md"), "---\nharness: fake\n---\n\nDo work.\n")
	writeFile(t, filepath.Join(root, "agents", "ops", "quiet", "JOB.md"), "---\nharness: fake\n---\n\nDo work.\n")
	every := routing.GetsEveryMessage
	routingtest.Write(t, root, "email", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Job: "ops/digest", Gets: every, Filter: job.TriggerYAML{Allowlist: []string{"B@X.io", "a@x.io", "a@x.io", "not-an-address"}}},
		{Job: "ops/quiet", Gets: every},
	}})
	routingtest.Write(t, root, "whatsapp", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Job: "ops/digest", Gets: every, Filter: job.TriggerYAML{Allowlist: []string{"wa@x.io"}}},
	}})
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.JobEmailSenders(ctx, ws.ID, "ops/digest")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a@x.io", "b@x.io"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("senders = %v, want %v", got, want)
	}

	got, err = svc.JobEmailSenders(ctx, ws.ID, "ops/quiet")
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
	if _, err := svc.SetArchived(ctx, ws, archiveRow(t, svc, ws, "peer"), true, time.Now()); err != nil {
		t.Fatal(err)
	}
	agents, _, err = svc.AcceptRules(ctx, ws.ID)
	if _, ok := agents["peer"]; ok || err != nil {
		t.Fatalf("after archive: %v, %v", agents, err)
	}
}

// TestJobHandoffSetting: team.handoff and a route set the gate of a job.
func TestJobHandoffSetting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := workspaces.New(openTest(t))
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "ops", "AGENT.md"), "---\ndescription: Test agent.\n---\n\nOps.\n")
	jobs := map[string]string{
		"plain":    "",
		"routed":   "",
		"off":      "team:\n  handoff: false\n",
		"on":       "team:\n  handoff: true\n",
		"on-route": "team:\n  handoff: true\n",
	}
	for name, team := range jobs {
		writeFile(t, filepath.Join(root, "agents", "ops", name, "JOB.md"), "---\nharness: fake\n"+team+"---\n\nDo work.\n")
	}
	every := routing.GetsEveryMessage
	routingtest.Write(t, root, "email", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Job: "ops/routed", Gets: every}, {Job: "ops/on-route", Gets: every},
	}})
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	owner := archiveRow(t, svc, ws, "ops")
	tests := []struct {
		name   string
		routed bool
		set    string
		want   bool
	}{
		{"plain", false, "", true},
		{"routed", true, "", false},
		{"off", false, "false", false},
		{"on", false, "true", true},
		{"on-route", true, "true", true},
	}
	for _, tc := range tests {
		row := archiveRow(t, svc, ws, "ops/"+tc.name)
		g, err := svc.JobHandoff(ctx, ws.ID, row.ID)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		set := ""
		if g.Setting != nil {
			set = strconv.FormatBool(*g.Setting)
		}
		if g.Routed != tc.routed || set != tc.set || g.Allowed(false) != tc.want || g.OwnerID != owner.ID || g.Job != "ops/"+tc.name {
			t.Errorf("%s: %+v allowed %v", tc.name, g, g.Allowed(false))
		}
	}
	row := archiveRow(t, svc, ws, "ops/plain")
	if _, err := svc.JobHandoff(ctx, "other", row.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("other workspace: %v", err)
	}
	if _, err := svc.JobHandoff(ctx, ws.ID, owner.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("agent id: %v", err)
	}
}
