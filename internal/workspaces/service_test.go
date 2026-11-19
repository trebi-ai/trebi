package workspaces_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

const jobMD = "---\nharness: claude\n---\n\nDo the task.\n"

func TestRebuildIndexIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "alpha", "JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "beta", "JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "TREBI.md"), "---\nharness: claude\n---\n\nAssist.\n")

	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	seq := mustSeq(t, st)

	// Second rebuild with no changes: no events, no row churn.
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if got := mustSeq(t, st); got != seq {
		t.Fatalf("bus grew on no-op rebuild: %v", busTopics(t, st, seq))
	}
}

func TestRebuildIndexEmitsDiffEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "alpha", "JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "beta", "JOB.md"), jobMD)

	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	alpha := indexedID(ctx, t, svc, ws.ID, "alpha", filepath.Join(root, "alpha", "JOB.md"))
	beta := indexedID(ctx, t, svc, ws.ID, "beta", filepath.Join(root, "beta", "JOB.md"))

	// Edit one job file and keep its ID: exactly one job.updated.
	seq := mustSeq(t, st)
	withID := "---\nid: " + alpha + "\nharness: claude\n---\n\nDo the task.\n"
	writeFile(t, filepath.Join(root, "alpha", "JOB.md"), withID+"\nMore work.\n")
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	topics := busTopics(t, st, seq)
	if len(topics) != 1 || topics[0] != "job.updated:"+alpha {
		t.Fatalf("topics=%v want [job.updated:%s]", topics, alpha)
	}

	// Write the file again with no ID: the job keeps its ID, and the
	// daemon writes it back into the file.
	seq = mustSeq(t, st)
	writeFile(t, filepath.Join(root, "alpha", "JOB.md"), jobMD+"\nLess work.\n")
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	topics = busTopics(t, st, seq)
	if len(topics) != 2 || topics[0] != "job.id_assigned:"+alpha || topics[1] != "job.updated:"+alpha {
		t.Fatalf("topics=%v want [job.id_assigned:%s job.updated:%s]", topics, alpha, alpha)
	}
	if got := indexedID(ctx, t, svc, ws.ID, "alpha", filepath.Join(root, "alpha", "JOB.md")); got != alpha {
		t.Fatalf("alpha id=%s want %s", got, alpha)
	}

	// Remove one job file: exactly one job.deleted.
	seq = mustSeq(t, st)
	if err := os.RemoveAll(filepath.Join(root, "beta")); err != nil {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	topics = busTopics(t, st, seq)
	if len(topics) != 1 || topics[0] != "job.deleted:"+beta {
		t.Fatalf("topics=%v want [job.deleted:%s]", topics, beta)
	}

	// New job file: the ID is assigned, then exactly one job.created.
	seq = mustSeq(t, st)
	writeFile(t, filepath.Join(root, "gamma", "JOB.md"), jobMD)
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	gamma := indexedID(ctx, t, svc, ws.ID, "gamma", filepath.Join(root, "gamma", "JOB.md"))
	topics = busTopics(t, st, seq)
	if len(topics) != 2 || topics[0] != "job.id_assigned:"+gamma || topics[1] != "job.created:"+gamma {
		t.Fatalf("topics=%v want [job.id_assigned:%s job.created:%s]", topics, gamma, gamma)
	}

	// TREBI.md is no longer indexed: no new events, no assistant rows.
	seq = mustSeq(t, st)
	writeFile(t, filepath.Join(root, "TREBI.md"), "---\nharness: claude\n---\n\nAssist.\n")
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	topics = busTopics(t, st, seq)
	if len(topics) != 0 {
		t.Fatalf("topics=%v want none", topics)
	}
}

// TestAgentEditUpdatesItsJobs proves that an AGENT.md change emits
// job.updated for each job of that agent, because a job takes the harness of
// its agent. The jobs of another agent get no event.
func TestAgentEditUpdatesItsJobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	agentA := filepath.Join(root, "agents", "a", "AGENT.md")
	writeFile(t, agentA, "---\ndescription: A.\nharness: claude\n---\n\nA.\n")
	writeFile(t, filepath.Join(root, "agents", "b", "AGENT.md"), "---\ndescription: B.\n---\n\nB.\n")
	for _, name := range []string{"agents/a/x", "agents/a/y", "agents/b/z"} {
		writeFile(t, filepath.Join(root, filepath.FromSlash(name), "JOB.md"), "---\n---\n\nWork.\n")
	}
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, name := range []string{"agents/a/x", "agents/a/y", "agents/b/z"} {
		ids[name] = indexedID(ctx, t, svc, ws.ID, name, filepath.Join(root, filepath.FromSlash(name), "JOB.md"))
	}

	seq := mustSeq(t, st)
	data, err := os.ReadFile(agentA)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, agentA, strings.Replace(string(data), "harness: claude", "harness: codex", 1))
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	topics := strings.Join(busTopics(t, st, seq), " ")
	for _, name := range []string{"agents/a/x", "agents/a/y"} {
		if !strings.Contains(topics, "job.updated:"+ids[name]) {
			t.Fatalf("%s: no job.updated in %s", name, topics)
		}
	}
	if strings.Contains(topics, ids["agents/b/z"]) {
		t.Fatalf("a job of agent b got an event: %s", topics)
	}
}

func TestRebuildIndexSkipsNestedWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "alpha", "JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "sub", "nested", "JOB.md"), jobMD)

	wsA, err := svc.Add(ctx, root, "A")
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id, name, path, git_enabled, created_at, updated_at)
		VALUES (?,?,?,?,?,?)`, "ws_nested", "B", sub, 0, now, now); err != nil {
		t.Fatal(err)
	}
	wsB, err := svc.Get(ctx, "ws_nested")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(ctx, wsB); err != nil {
		t.Fatal(err)
	}

	// Remove B's trebi.yaml marker if present: the registry boundary must still hold.
	if err := os.Remove(filepath.Join(root, "sub", "trebi.yaml")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(ctx, wsA); err != nil {
		t.Fatal(err)
	}

	rowsA, err := svc.ListJobs(ctx, wsA.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rowsA {
		if strings.HasPrefix(r.Name, "sub/") {
			t.Fatalf("A's index contains B's job: %s", r.Name)
		}
	}
	rowsB, err := svc.ListJobs(ctx, wsB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsB) != 1 || rowsB[0].Name != "nested" {
		t.Fatalf("B rows=%v want [nested]", rowsB)
	}
}

func TestRebuildIndexAgents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "cmo", "AGENT.md"), "---\ndescription: Test agent.\nharness: codex\n---\n\nLead growth.\n")
	writeFile(t, filepath.Join(root, "agents", "cmo", "duty", "JOB.md"), jobMD)

	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}

	rows, err := svc.ListJobs(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	var agentRow, dutyRow *workspaces.JobRow
	for i := range rows {
		switch rows[i].Name {
		case "agents/cmo":
			agentRow = &rows[i]
		case "agents/cmo/duty":
			dutyRow = &rows[i]
		}
	}
	if agentRow == nil || dutyRow == nil {
		t.Fatalf("rows=%v want agents/cmo and agents/cmo/duty", rows)
	}
	if agentRow.Kind != "agent" || agentRow.OwnerID != "" {
		t.Errorf("agent row %+v want kind=agent, no owner", agentRow)
	}
	if dutyRow.OwnerID != agentRow.ID || dutyRow.OwnerIndex() != "agents/cmo" {
		t.Errorf("duty owner=%q (%s) want agents/cmo", dutyRow.OwnerIndex(), dutyRow.OwnerID)
	}
	agentID := indexedID(ctx, t, svc, ws.ID, "agents/cmo", filepath.Join(root, "agents", "cmo", "AGENT.md"))
	dutyID := indexedID(ctx, t, svc, ws.ID, "agents/cmo/duty", filepath.Join(root, "agents", "cmo", "duty", "JOB.md"))
	if !job.ValidID("agent", agentID) || !job.ValidID("job", dutyID) {
		t.Fatalf("agent id=%s duty id=%s", agentID, dutyID)
	}

	// Edit the agent file and keep its ID: agent.updated, then job.updated
	// for its job, because the job takes the agent harness.
	seq := mustSeq(t, st)
	writeFile(t, filepath.Join(root, "agents", "cmo", "AGENT.md"), "---\ndescription: Test agent.\nid: "+agentID+"\nharness: codex\n---\n\nLead all growth.\n")
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	topics := busTopics(t, st, seq)
	if len(topics) != 2 || topics[0] != "agent.updated:"+agentID || topics[1] != "job.updated:"+dutyID {
		t.Fatalf("topics=%v want [agent.updated:%s job.updated:%s]", topics, agentID, dutyID)
	}

	// Remove the agents folder: one agent.deleted plus one job.deleted.
	// Map order is random, so compare as a set.
	seq = mustSeq(t, st)
	if err := os.RemoveAll(filepath.Join(root, "agents")); err != nil {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	topics = busTopics(t, st, seq)
	want := map[string]bool{"agent.deleted:" + agentID: true, "job.deleted:" + dutyID: true}
	if len(topics) != len(want) {
		t.Fatalf("topics=%v want %v", topics, want)
	}
	for _, got := range topics {
		if !want[got] {
			t.Fatalf("topics=%v want %v", topics, want)
		}
	}
}

func TestRebuildIndexDisabledAgentInactive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "cmo", "AGENT.md"), "---\ndescription: Test agent.\ndisabled: true\n---\n\nLead growth.\n")
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListJobs(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%v want the agent row only", rows)
	}
	for _, r := range rows {
		if r.Kind != "agent" {
			continue
		}
		if r.Active {
			t.Fatalf("agent row active: %v", r)
		}
		return
	}
	t.Fatalf("rows=%v want one inactive agent row", rows)
}

func TestResolveIDOrName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)
	root := t.TempDir()
	ws, err := svc.Add(ctx, root, "Staging")
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Resolve(ctx, ws.ID)
	if err != nil || got.ID != ws.ID {
		t.Fatalf("id: %v %v", got, err)
	}
	got, err = svc.Resolve(ctx, "staging")
	if err != nil || got.ID != ws.ID {
		t.Fatalf("name: %v %v", got, err)
	}
	if _, err := svc.Resolve(ctx, "missing"); err == nil {
		t.Fatal("want not found")
	}
}

func TestAddIDCollision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	dir1 := t.TempDir()
	dir2 := t.TempDir()
	if _, err := svc.Add(ctx, dir1, "one"); err != nil {
		t.Fatal(err)
	}

	// Re-add same path: idempotent no-op, no new events.
	seq := mustSeq(t, st)
	if _, err := svc.Add(ctx, dir1, "one"); err != nil {
		t.Fatal(err)
	}
	if got := mustSeq(t, st); got != seq {
		t.Fatalf("bus grew on idempotent re-add: %v", busTopics(t, st, seq))
	}

	// Copied trebi.yaml (same id, different path): error names existing path.
	data, err := os.ReadFile(filepath.Join(dir1, "trebi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir2, "trebi.yaml"), string(data))
	_, err = svc.Add(ctx, dir2, "two")
	if err == nil {
		t.Fatal("want id-collision error")
	}
	if !strings.Contains(err.Error(), dir1) {
		t.Fatalf("error %q does not name existing path %s", err, dir1)
	}
}

func TestRemoveEmitsWorkspaceDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "alpha", "JOB.md"), jobMD)
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}

	seq := mustSeq(t, st)
	if err := svc.Remove(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	topics := busTopics(t, st, seq)
	if len(topics) != 1 || topics[0] != "workspace.deleted:"+ws.ID {
		t.Fatalf("topics=%v want [workspace.deleted:%s]", topics, ws.ID)
	}
	if _, err := svc.Get(ctx, ws.ID); err == nil {
		t.Fatal("workspace still present after Remove")
	}
	rows, err := svc.ListJobs(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("live job_index rows remain: %v", rows)
	}
	var retired int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM job_index WHERE workspace_id=? AND retired_at IS NOT NULL`, ws.ID).Scan(&retired); err != nil || retired != 1 {
		t.Fatalf("retired job_index rows = %d, %v; want 1", retired, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("workspace folder still present: %v", err)
	}
}

func TestRemoveDeletesCreatedFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	root := t.TempDir()
	svc := workspaces.New(st).WithRoot(root)

	ws, err := svc.AddWith(ctx, workspaces.AddOpts{Name: "test", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "trebi.yaml")); err != nil {
		t.Fatalf("trebi.yaml: %v", err)
	}
	if err := svc.Remove(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Fatalf("created folder still present: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("workspaces.root should remain: %v", err)
	}
}

func TestRemoveMissingFolderStillUnregisters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)
	root := t.TempDir()
	ws, err := svc.Add(ctx, root, "gone")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := svc.Remove(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, ws.ID); err == nil {
		t.Fatal("workspace still present after Remove")
	}
}

func TestRenameBumpsRevisionAndBusFrame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	ws, err := svc.Add(ctx, root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Revision != 1 {
		t.Fatalf("create revision=%d want 1", ws.Revision)
	}

	seq := mustSeq(t, st)
	got, err := svc.Rename(ctx, ws.ID, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "beta" || got.Revision != 2 {
		t.Fatalf("first rename %+v", got)
	}
	evs, err := st.ListBus(ctx, seq, []string{"workspace.updated"}, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("bus %d %v", len(evs), err)
	}
	if evs[0].Revision != 2 || evs[0].ID != ws.ID {
		t.Fatalf("frame %+v", evs[0])
	}

	got, err = svc.Rename(ctx, ws.ID, "gamma")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 3 {
		t.Fatalf("second rename revision=%d want 3", got.Revision)
	}

	seq = mustSeq(t, st)
	if _, err := svc.Rename(ctx, "ws_nope", "x"); !errors.Is(err, workspaces.ErrNotFound) {
		t.Fatalf("missing id: %v", err)
	}
	n, err := st.ListBus(ctx, seq, nil, 10)
	if err != nil || len(n) != 0 {
		t.Fatalf("not-found must not emit: %d %v", len(n), err)
	}
}

func TestHandleForAndValidate(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"Ellis Labs", "ellis-labs"},
		{"", "ws"},
		{"---", "ws"},
		{"x", "ws"},
		{"Acme!!!Corp", "acme-corp"},
		{"--Lead--Trim--", "lead-trim"},
		{strings.Repeat("x", 40), strings.Repeat("x", 20)},
		{"Über Café", "ber-caf"},
	}
	for _, c := range cases {
		got := workspaces.HandleFor(c.in)
		if got != c.want {
			t.Errorf("HandleFor(%q) = %q, want %q", c.in, got, c.want)
		}
		if err := workspaces.ValidateHandle(got); err != nil {
			t.Errorf("HandleFor(%q) = %q is not valid: %v", c.in, got, err)
		}
	}
	for _, bad := range []string{"", "a", "-ab", "ab-", "Ab", "a_b", "a+b", strings.Repeat("x", 21)} {
		if err := workspaces.ValidateHandle(bad); !errors.Is(err, workspaces.ErrHandle) {
			t.Errorf("ValidateHandle(%q) = %v, want ErrHandle", bad, err)
		}
	}
}

func TestAddHandleAndSetHandle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	ws, err := svc.AddWith(ctx, workspaces.AddOpts{Path: t.TempDir(), Name: "Ellis Labs"})
	if err != nil || ws.Handle != "ellis-labs" {
		t.Fatalf("derived handle = %+v, %v", ws, err)
	}
	own, err := svc.AddWith(ctx, workspaces.AddOpts{Path: t.TempDir(), Name: "Reports", Handle: "acme"})
	if err != nil || own.Handle != "acme" {
		t.Fatalf("given handle = %+v, %v", own, err)
	}
	if got, _ := svc.Get(ctx, own.ID); got.Handle != "acme" {
		t.Fatalf("stored handle = %q", got.Handle)
	}
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: t.TempDir(), Name: "x", Handle: "Bad!"}); !errors.Is(err, workspaces.ErrHandle) {
		t.Fatalf("bad handle: %v", err)
	}

	// A rename leaves the handle alone.
	if got, err := svc.Rename(ctx, own.ID, "Other"); err != nil || got.Handle != "acme" {
		t.Fatalf("rename changed the handle: %+v, %v", got, err)
	}
	seq := mustSeq(t, st)
	got, err := svc.SetHandle(ctx, own.ID, "acme-2")
	if err != nil || got.Handle != "acme-2" || got.Revision != 3 {
		t.Fatalf("SetHandle = %+v, %v", got, err)
	}
	if evs, _ := st.ListBus(ctx, seq, []string{"workspace.updated"}, 10); len(evs) != 1 {
		t.Fatalf("SetHandle must emit one workspace.updated, got %d", len(evs))
	}
	if _, err := svc.SetHandle(ctx, own.ID, "-"); !errors.Is(err, workspaces.ErrHandle) {
		t.Fatalf("SetHandle bad: %v", err)
	}
	if _, err := svc.SetHandle(ctx, "ws_nope", "abc"); !errors.Is(err, workspaces.ErrNotFound) {
		t.Fatalf("SetHandle missing: %v", err)
	}
}

func TestAddCreateAndGuards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	root := t.TempDir()
	svc := workspaces.New(st).WithRoot(root)

	missing := filepath.Join(root, "fresh")
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: missing, Name: "fresh", Create: true}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(missing); err != nil || !st.IsDir() {
		t.Fatalf("mkdir: %v %v", st, err)
	}
	seq := mustSeq(t, st)
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: missing, Name: "fresh", Create: true}); err != nil {
		t.Fatal(err)
	}
	if got := mustSeq(t, st); got != seq {
		t.Fatalf("bus grew on idempotent create: %v", busTopics(t, st, seq))
	}

	inside := filepath.Join(missing, "nested")
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: inside, Name: "nested", Create: true}); !errors.Is(err, workspaces.ErrNested) {
		t.Fatalf("nested: %v", err)
	}

	pathless, err := svc.AddWith(ctx, workspaces.AddOpts{Name: "reports", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if pathless.Path != filepath.Join(root, "reports") {
		t.Fatalf("pathless path %s", pathless.Path)
	}

	bare := workspaces.New(st)
	if _, err := bare.AddWith(ctx, workspaces.AddOpts{Name: "x", Create: true}); !errors.Is(err, workspaces.ErrNoRoot) {
		t.Fatalf("no root: %v", err)
	}

	filePath := filepath.Join(root, "not-a-dir")
	writeFile(t, filePath, "x")
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: filepath.Join(filePath, "sub"), Name: "bad", Create: true}); !errors.Is(err, workspaces.ErrAncestorFile) {
		t.Fatalf("ancestor file: %v", err)
	}

	child := filepath.Join(root, "child")
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: child, Name: "child", Create: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: root, Name: "parent"}); !errors.Is(err, workspaces.ErrContains) {
		t.Fatalf("contains: %v", err)
	}
}

func TestPatchWritesTrebiYAML(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)
	root := t.TempDir()
	ws, err := svc.Add(ctx, root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	on := true
	got, err := svc.Patch(ctx, ws.ID, workspaces.Patch{GitEnabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if !got.GitEnabled || got.Revision != 2 {
		t.Fatalf("%+v", got)
	}
	data, err := os.ReadFile(filepath.Join(root, "trebi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "git_enabled: true") || !strings.Contains(string(data), "name: alpha") {
		t.Fatalf("yaml: %s", data)
	}

	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	name := "blocked"
	if _, err := svc.Patch(ctx, ws.ID, workspaces.Patch{Name: &name}); err == nil {
		t.Fatal("want file-write error")
	}
	cur, err := svc.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Name != "alpha" {
		t.Fatalf("registry changed after file failure: %s", cur.Name)
	}
}

func TestRemoveNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	seq := mustSeq(t, st)
	err := svc.Remove(ctx, "ws_nope")
	if !errors.Is(err, workspaces.ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
	if got := mustSeq(t, st); got != seq {
		t.Fatalf("bus grew on not-found remove: %v", busTopics(t, st, seq))
	}
}

func openTest(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func busTopics(t *testing.T, st *store.Store, after int64) []string {
	t.Helper()
	evs, err := st.ListBus(context.Background(), after, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	var topics []string
	for _, ev := range evs {
		topics = append(topics, ev.Topic+":"+ev.ID)
	}
	return topics
}

func mustSeq(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var v sql.NullInt64
	err := st.DB().QueryRowContext(context.Background(), `SELECT MAX(seq) FROM event_log`).Scan(&v)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Valid {
		return 0
	}
	return v.Int64
}

func TestWorkspaceSenderName(t *testing.T) {
	t.Parallel()
	nick, blank := "Ellis", "  "
	cases := []struct {
		ws   workspaces.Workspace
		want string
	}{
		{workspaces.Workspace{Name: "ocral"}, "ocral"},
		{workspaces.Workspace{Name: "ocral", Assistant: &blank}, "ocral"},
		{workspaces.Workspace{Name: "ocral", Assistant: &nick}, "Ellis"},
	}
	for _, tc := range cases {
		if got := tc.ws.SenderName(); got != tc.want {
			t.Errorf("SenderName(%+v) = %q, want %q", tc.ws, got, tc.want)
		}
	}
}

// indexedID returns the index ID of name. It fails when the file at path
// does not start its frontmatter with the same ID.
func indexedID(ctx context.Context, t *testing.T, svc *workspaces.Service, wsID, name, path string) string {
	t.Helper()
	row, err := svc.JobByName(ctx, wsID, name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "---\nid: "+row.ID+"\n") {
		t.Fatalf("%s: file does not start with id %s:\n%s", name, row.ID, data)
	}
	return row.ID
}

func TestRebuildIndexFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "cmo", "AGENT.md"), "---\nharness: claude\n---\n\nLead.\n")
	jobPath := filepath.Join(root, "agents", "cmo", "a", "JOB.md")
	writeFile(t, jobPath, "---\nharness: claude\nfolder: SEO\n---\n\nA.\n")
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	row, err := svc.JobByName(ctx, ws.ID, "agents/cmo/a")
	if err != nil {
		t.Fatal(err)
	}
	if row.Folder != "SEO" {
		t.Fatalf("folder=%q want SEO", row.Folder)
	}

	seq := mustSeq(t, st)
	writeFile(t, jobPath, "---\nid: "+row.ID+"\nharness: claude\nfolder: Growth\n---\n\nA.\n")
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if topics := busTopics(t, st, seq); len(topics) != 1 || topics[0] != "job.updated:"+row.ID {
		t.Fatalf("topics=%v want [job.updated:%s]", topics, row.ID)
	}
	row, err = svc.JobByName(ctx, ws.ID, "agents/cmo/a")
	if err != nil {
		t.Fatal(err)
	}
	if row.Folder != "Growth" {
		t.Fatalf("folder=%q want Growth", row.Folder)
	}

	seq = mustSeq(t, st)
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if got := mustSeq(t, st); got != seq {
		t.Fatalf("bus grew on no-op rebuild: %v", busTopics(t, st, seq))
	}
}

func TestPatchHandle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)
	ws, err := svc.AddWith(ctx, workspaces.AddOpts{Path: t.TempDir(), Name: "Reports"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.AddWith(ctx, workspaces.AddOpts{Path: t.TempDir(), Name: "Other", Handle: "taken"})
	if err != nil {
		t.Fatal(err)
	}
	h := func(v string) workspaces.Patch { return workspaces.Patch{Handle: &v} }

	got, err := svc.Patch(ctx, ws.ID, h("mine"))
	if err != nil || got.Handle != "mine" {
		t.Fatalf("patch handle = %+v, %v", got, err)
	}
	if _, err := svc.Patch(ctx, ws.ID, h("Bad!")); !errors.Is(err, workspaces.ErrHandle) {
		t.Fatalf("bad handle: %v", err)
	}
	if taken, _ := svc.HandleTaken(ctx, "taken", ws.ID); !taken {
		t.Fatal("taken handle not reported")
	}
	if taken, _ := svc.HandleTaken(ctx, "taken", other.ID); taken {
		t.Fatal("own handle reported as taken")
	}

	if _, err := st.DB().ExecContext(ctx, `INSERT INTO email_addresses (workspace_id, local_part, domain, created_at)
		VALUES (?, 'mine-01', 'in.example.com', 't')`, ws.ID); err != nil {
		t.Fatal(err)
	}
	if has, _ := svc.HasAddress(ctx, ws.ID); !has {
		t.Fatal("HasAddress false")
	}
	if _, err := svc.Patch(ctx, ws.ID, h("later")); !errors.Is(err, workspaces.ErrHandleLocked) {
		t.Fatalf("locked: %v", err)
	}
}

const agentMD = "---\ndescription: Test agent.\n---\n\nLead.\n"

func TestIndexArchivedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, ws := archiveFixture(t)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	if _, err := svc.SetArchived(ctx, ws, archiveRow(t, svc, ws, "agents/boss/a"), true, now); err != nil {
		t.Fatal(err)
	}
	a := archiveRow(t, svc, ws, "agents/boss/a")
	if a.ArchivedAt != "2026-11-18T10:00:00Z" || !a.Archived() || a.EffectiveArchivedAt() != a.ArchivedAt {
		t.Fatalf("own archive: %+v", a)
	}
	if b := archiveRow(t, svc, ws, "agents/boss/b"); b.Archived() {
		t.Fatalf("sibling archived: %+v", b)
	}
	res, err := svc.SetArchived(ctx, ws, archiveRow(t, svc, ws, "agents/boss"), true, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	b := archiveRow(t, svc, ws, "agents/boss/b")
	if b.ArchivedAt != "" || b.OwnerArchivedAt != "2026-11-18T11:00:00Z" || !b.Archived() || b.EffectiveArchivedAt() != b.OwnerArchivedAt {
		t.Fatalf("inherited archive: %+v", b)
	}
	if a = archiveRow(t, svc, ws, "agents/boss/a"); a.EffectiveArchivedAt() != "2026-11-18T10:00:00Z" {
		t.Fatalf("own time must win: %+v", a)
	}
	// The agent and the job with no own flag change state; job a does not.
	if len(res.JobIDs) != 2 || len(res.Jobs) != 3 {
		t.Fatalf("result: %+v", res)
	}
	if peer := archiveRow(t, svc, ws, "agents/peer"); peer.Archived() {
		t.Fatalf("peer archived: %+v", peer)
	}
}

func TestIndexArchiveBusAttrs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, st, ws := archiveFixture(t)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	// frames returns topic:name:archived for each frame after seq.
	frames := func(after int64) map[string]string {
		evs, err := st.ListBus(ctx, after, nil, 100)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, ev := range evs {
			var attrs map[string]string
			if err := json.Unmarshal(ev.Attrs, &attrs); err != nil {
				t.Fatal(err)
			}
			name := attrs["job_name"]
			if name == "" {
				name = "agents/" + attrs["agent_name"]
			}
			out[ev.Topic+":"+name] = attrs["archived"] + "|" + attrs["archived_at"]
		}
		return out
	}
	check := func(label string, got, want map[string]string) {
		t.Helper()
		if !maps.Equal(got, want) {
			t.Fatalf("%s: got %v want %v", label, got, want)
		}
	}

	seq := mustSeq(t, st)
	if _, err := svc.SetArchived(ctx, ws, archiveRow(t, svc, ws, "agents/boss/a"), true, now); err != nil {
		t.Fatal(err)
	}
	check("job archive", frames(seq), map[string]string{"job.updated:agents/boss/a": "true|2026-11-18T10:00:00Z"})

	seq = mustSeq(t, st)
	if _, err := svc.SetArchived(ctx, ws, archiveRow(t, svc, ws, "agents/boss"), true, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Job a has its own flag, so its state does not change.
	check("agent archive", frames(seq), map[string]string{
		"agent.updated:agents/boss": "true|2026-11-18T11:00:00Z",
		"job.updated:agents/boss/b": "true|2026-11-18T11:00:00Z",
		"job.updated:agents/boss/a": "|",
	})

	seq = mustSeq(t, st)
	if _, err := svc.SetArchived(ctx, ws, archiveRow(t, svc, ws, "agents/boss"), false, now); err != nil {
		t.Fatal(err)
	}
	check("agent unarchive", frames(seq), map[string]string{
		"agent.updated:agents/boss": "false|",
		"job.updated:agents/boss/b": "false|",
		"job.updated:agents/boss/a": "|",
	})

	// An edit with no change of archive state has no archived attr.
	seq = mustSeq(t, st)
	peer := archiveRow(t, svc, ws, "agents/peer")
	writeFile(t, peer.Path, strings.Replace(mustRead(t, peer.Path), "Lead.", "Lead more.", 1))
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	check("plain edit", frames(seq), map[string]string{"agent.updated:agents/peer": "|"})

	seq = mustSeq(t, st)
	if _, err := svc.SetArchived(ctx, ws, archiveRow(t, svc, ws, "agents/boss/a"), false, now); err != nil {
		t.Fatal(err)
	}
	check("job unarchive", frames(seq), map[string]string{"job.updated:agents/boss/a": "false|"})
}

func TestTriggerJobsSkipsArchived(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, ws := archiveFixture(t)
	now := time.Now()
	// peer gets its own job; a is archived; boss/b is archived through boss.
	if _, err := svc.SetArchived(ctx, ws, archiveRow(t, svc, ws, "agents/boss/a"), true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetArchived(ctx, ws, archiveRow(t, svc, ws, "agents/boss"), true, now); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.TriggerJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"agents/peer"}) {
		t.Fatalf("trigger jobs = %v", names)
	}
	if all, _ := svc.ListJobs(ctx, ws.ID); len(all) != 4 {
		t.Fatalf("ListJobs must keep archived rows: %d", len(all))
	}
}

// archiveFixture holds the agents boss and peer. boss owns the jobs a and b.
func archiveFixture(t *testing.T) (*workspaces.Service, *store.Store, *workspaces.Workspace) {
	t.Helper()
	st := openTest(t)
	svc := workspaces.New(st)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents/boss/AGENT.md"), agentMD)
	writeFile(t, filepath.Join(root, "agents/boss/a/JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "agents/boss/b/JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "agents/peer/AGENT.md"), agentMD)
	ws, err := svc.Add(context.Background(), root, "arch")
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, ws
}

func archiveRow(t *testing.T, svc *workspaces.Service, ws *workspaces.Workspace, name string) workspaces.JobRow {
	t.Helper()
	row, err := svc.JobByName(context.Background(), ws.ID, name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return *row
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestAssistantHasTriggers: an assistant block with a triggers key is
// reported for the doctor, and the override still reads.
func TestAssistantHasTriggers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := workspaces.New(openTest(t))
	root := t.TempDir()
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	with := "assistant:\n  harness: fake\n  triggers:\n    - event: tick\n"
	writeFile(t, filepath.Join(root, "trebi.yaml"), with)
	if !workspaces.AssistantHasTriggers([]byte(with)) {
		t.Fatal("triggers key not found")
	}
	if ov, _, err := svc.AssistantOverride(ctx, ws.ID); err != nil || ov.Harness == nil || *ov.Harness != "fake" {
		t.Fatalf("override = %+v, %v", ov, err)
	}
	if workspaces.AssistantHasTriggers([]byte("assistant:\n  harness: fake\n")) || workspaces.AssistantHasTriggers(nil) {
		t.Fatal("false positive")
	}
}

// TestRouteRows: two workspaces give two rows, an archived workspace gives
// none, and a broken file gives a row with one invalid_route problem.
func TestRouteRows(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)
	var ids []string
	for _, name := range []string{"one", "two", "old"} {
		root := t.TempDir()
		routingtest.Write(t, root, "gmail-2", routing.File{Routes: []routing.Route{{Agent: "a", Gets: routing.GetsEveryMessage}}})
		ws, err := svc.Add(ctx, root, name)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ws.ID)
		if name == "two" {
			routingtest.WriteRaw(t, root, "gmail-2", "routes:\n  - gets: maybe\n    agent: a\n")
		}
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE workspaces SET archived_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), ids[2]); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.RouteRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		bad := r.WorkspaceID == ids[1]
		if r.File.Connection != "gmail-2" || (len(r.File.Problems) == 1) != bad || (bad && r.File.Problems[0].Code != routing.CodeInvalidRoute) {
			t.Fatalf("row = %+v", r)
		}
	}
}

// TestRouteTargets: an agent, a job of the agent, and an archived job are
// targets. A job outside an agent is not.
func TestRouteTargets(t *testing.T) {
	ctx := context.Background()
	svc := workspaces.New(openTest(t))
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "ops", "AGENT.md"), "---\ndescription: Test agent.\nharness: fake\n---\n\nOps.\n")
	writeFile(t, filepath.Join(root, "agents", "ops", "digest", "JOB.md"), "---\nharness: fake\n---\n\nDo work.\n")
	writeFile(t, filepath.Join(root, "agents", "ops", "old", "JOB.md"), "---\nharness: fake\narchived_at: 2026-01-01T00:00:00Z\n---\n\nDo work.\n")
	writeFile(t, filepath.Join(root, "loose", "JOB.md"), "---\nharness: fake\n---\n\nDo work.\n")
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	got, err := svc.RouteTargets(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got["ops"].Kind != "agent" || got["ops/digest"].Index != "agents/ops/digest" || !got["ops/old"].Archived || got[routing.Assistant].Kind != routing.Assistant {
		t.Fatalf("targets = %+v", got)
	}
	for name := range got {
		if strings.Contains(name, "loose") {
			t.Fatalf("job outside an agent is a target: %s", name)
		}
	}
}

// TestEnsureRouteIDs: an index rebuild writes ids into a routes file that has
// none and emits one routing.updated. A second rebuild writes nothing.
func TestEnsureRouteIDs(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)
	root := t.TempDir()
	path := routingtest.WriteRaw(t, root, "gmail", "routes:\n  - agent: a\n  - agent: b\n")
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := svc.RebuildIndex(ctx, ws); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "id: rt_"); n != 2 {
		t.Fatalf("ids = %d in %s", n, data)
	}
	n := 0
	for _, topic := range busTopics(t, st, 0) {
		if strings.HasPrefix(topic, "routing.updated:") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("routing.updated frames = %d, want 1", n)
	}
}
