package runs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/state"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestMemoryLaunchSeedsSkeleton(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	run, prompt := launchMemoryRun(t, svc, memoryLaunch(root))

	data, err := os.ReadFile(filepath.Join(root, "demo", "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(memory.Skeleton()) {
		t.Fatalf("seeded file = %q", data)
	}
	if run.MemoryHash != memory.Hash(data) {
		t.Fatalf("memory_hash = %s, want %s", run.MemoryHash, memory.Hash(data))
	}
	for _, want := range []string{
		"## Context", "## Lessons", "## Log",
		"MEMORY.md is next to your job file",
		"Keep records in `trebi state`",
		"Keep Log under 100 entries",
		"Update the file before you print the completion line.",
		"run the digest",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "## State") || strings.Contains(prompt, "deprecated") {
		t.Errorf("skeleton prompt has a State notice:\n%s", prompt)
	}
	// Memory block, then protocol, then the task itself.
	i := strings.Index(prompt, "## Log")
	j := strings.Index(prompt, "MEMORY.md is next to your job file")
	k := strings.Index(prompt, "run the digest")
	if i < 0 || j < i || k < j {
		t.Fatalf("prompt order broken: %d %d %d\n%s", i, j, k, prompt)
	}
}

func TestMemoryLaunchMigratesLegacyOnce(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	p := filepath.Join(root, "demo", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("Old notes follow.\n\n[2026-09-01] First thing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run1, _ := launchMemoryRun(t, svc, memoryLaunch(root))
	first, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	doc, issues := memory.Parse(first)
	if doc.Legacy {
		t.Fatalf("still legacy after launch: %s", first)
	}
	if len(issues) > 0 {
		t.Fatalf("migrated file invalid: %v", issues)
	}
	if !strings.Contains(doc.Context, "Old notes follow.") {
		t.Fatalf("context = %q", doc.Context)
	}
	if len(doc.Log) != 1 || doc.Log[0].Text != "First thing." || doc.Log[0].At != "2026-09-01" {
		t.Fatalf("log = %+v", doc.Log)
	}
	if run1.MemoryHash != memory.Hash(first) {
		t.Fatalf("memory_hash = %s, want %s", run1.MemoryHash, memory.Hash(first))
	}

	// A second launch must not rewrite the migrated file.
	run2, _ := launchMemoryRun(t, svc, memoryLaunch(root))
	second, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Fatalf("second launch rewrote the file:\n%s\n---\n%s", first, second)
	}
	if run2.MemoryHash != run1.MemoryHash {
		t.Fatalf("hash moved: %s vs %s", run2.MemoryHash, run1.MemoryHash)
	}
}

func TestMemoryOffTouchesNoFile(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	run, prompt := launchMemoryRun(t, svc, nil)

	if _, err := os.Stat(filepath.Join(root, "demo", "MEMORY.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("memory file exists: %v", err)
	}
	if run.MemoryHash != "" {
		t.Fatalf("memory_hash = %q, want empty", run.MemoryHash)
	}
	if strings.Contains(prompt, "## Context") {
		t.Fatalf("prompt carries memory block:\n%s", prompt)
	}
}

func TestMemoryGatePassesValidUpdate(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	script := fakeWriteMemory(gateValidMemory) + `echo "$TREBI_RUN_COMPLETE"`
	run := launchMemoryScript(t, svc, memoryLaunch(root), script)

	assertRunComplete(t, run, runs.EndSentinel)
	kinds := runEventKinds(t, svc, run.ID)
	if kinds["memory.fix_requested"] != 0 || kinds["memory.invalid"] != 0 {
		t.Fatalf("memory events = %v", kinds)
	}
}

func TestMemoryGateFixCycleRecovers(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	script := fakeWaitFix() + `echo "$TREBI_RUN_COMPLETE"
waitfix
` + fakeWriteMemory(gateValidMemory) + `echo "$TREBI_RUN_COMPLETE"`
	run := launchMemoryScript(t, svc, memoryLaunch(root), script)

	assertRunComplete(t, run, runs.EndSentinel)
	kinds := runEventKinds(t, svc, run.ID)
	if kinds["memory.fix_requested"] != 1 || kinds["memory.invalid"] != 0 {
		t.Fatalf("memory events = %v", kinds)
	}
	if issues := runEventIssues(t, svc, run.ID, "memory.fix_requested"); !strings.Contains(issues, "unchanged this run") {
		t.Fatalf("fix issues = %q", issues)
	}
}

// TestMemoryPromptRecordedInTranscript pins the opening transcript turn: the
// exact prompt the harness received, memory render included.
func TestMemoryPromptRecordedInTranscript(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	run, prompt := launchMemoryRun(t, svc, memoryLaunch(root))

	turns, err := svc.CLITurns(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || turns[0].Role != "user" {
		t.Fatalf("first turn: %+v", turns)
	}
	if turns[0].Text != prompt {
		t.Fatalf("recorded prompt differs from the submitted one:\n got %q\nwant %q", turns[0].Text, prompt)
	}
	if !strings.Contains(turns[0].Text, "## Lessons") || !strings.Contains(turns[0].Text, "run the digest") {
		t.Fatalf("prompt missing memory render or body: %q", turns[0].Text)
	}
}

// TestMemoryGateLogsOpsRecords pins the Log-tab side of the gate: each retry
// and the final failure write an ops record naming the run.
func TestMemoryGateLogsOpsRecords(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	svc, _, root := memoryFixture(t)
	script := fakeWaitFix() + `echo "$TREBI_RUN_COMPLETE"
waitfix
echo "$TREBI_RUN_COMPLETE"
waitfix
echo "$TREBI_RUN_COMPLETE"`
	run := launchMemoryScript(t, svc, memoryLaunch(root), script)
	assertRunComplete(t, run, runs.EndSentinel)

	logs := buf.String()
	if got := strings.Count(logs, "memory.fix_requested"); got != 2 {
		t.Fatalf("fix_requested records: %d\n%s", got, logs)
	}
	if got := strings.Count(logs, "memory.invalid"); got != 1 {
		t.Fatalf("memory.invalid records: %d\n%s", got, logs)
	}
	if !strings.Contains(logs, "id="+run.ID) {
		t.Fatalf("records must name the run:\n%s", logs)
	}
}

func TestMemoryGateBudgetExhaustion(t *testing.T) {
	t.Parallel()
	svc, st, root := memoryFixture(t)
	script := fakeWaitFix() + `echo "$TREBI_RUN_COMPLETE"
waitfix
echo "$TREBI_RUN_COMPLETE"
waitfix
echo "$TREBI_RUN_COMPLETE"`
	run := launchMemoryScript(t, svc, memoryLaunch(root), script)

	assertRunComplete(t, run, runs.EndSentinel)
	kinds := runEventKinds(t, svc, run.ID)
	if kinds["memory.fix_requested"] != 2 {
		t.Fatalf("fix_requested = %d, want 2", kinds["memory.fix_requested"])
	}
	if issues := runEventIssues(t, svc, run.ID, "memory.invalid"); !strings.Contains(issues, "unchanged this run") {
		t.Fatalf("invalid issues = %q", issues)
	}
	var busN int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic='memory.invalid' AND entity_id=?`, run.ID).Scan(&busN); err != nil {
		t.Fatal(err)
	}
	if busN != 1 {
		t.Fatalf("bus memory.invalid rows = %d, want 1", busN)
	}
}

func TestMemoryGateUnreachableFake(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	run := launchMemoryScript(t, svc, memoryLaunch(root), `echo "$TREBI_RUN_COMPLETE"`)

	assertRunComplete(t, run, runs.EndSentinel)
	kinds := runEventKinds(t, svc, run.ID)
	if kinds["memory.fix_requested"] != 1 || kinds["memory.invalid"] != 1 {
		t.Fatalf("memory events = %v", kinds)
	}
}

func TestMemoryGateRequireUpdateOff(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	mem := memoryLaunch(root)
	mem.Opt.RequireUpdate = false
	run := launchMemoryScript(t, svc, mem, `echo "$TREBI_RUN_COMPLETE"`)

	assertRunComplete(t, run, runs.EndSentinel)
	kinds := runEventKinds(t, svc, run.ID)
	if kinds["memory.fix_requested"] != 0 || kinds["memory.invalid"] != 0 {
		t.Fatalf("memory events = %v", kinds)
	}
}

func TestMemoryGateLogOverCap(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	over := &memory.Doc{Context: "Client sends CSV.", Log: []memory.Entry{
		{At: "2026-09-14 09:00", Text: "first"},
		{At: "2026-09-14 09:01", Text: "second"},
		{At: "2026-09-14 09:02", Text: "third"},
	}}
	p := filepath.Join(root, "demo", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, over.Marshal(), 0o644); err != nil {
		t.Fatal(err)
	}
	pruned := &memory.Doc{Context: "Client sends CSV.", Log: over.Log[1:]}
	mem := memoryLaunch(root)
	mem.Opt.MaxLogs = 2
	script := fakeWaitFix() + `echo "$TREBI_RUN_COMPLETE"
waitfix
` + fakeWriteMemory(string(pruned.Marshal())) + `echo "$TREBI_RUN_COMPLETE"`
	run := launchMemoryScript(t, svc, mem, script)

	assertRunComplete(t, run, runs.EndSentinel)
	if issues := runEventIssues(t, svc, run.ID, "memory.fix_requested"); !strings.Contains(issues, "Log has 3 entries, cap is 2") {
		t.Fatalf("fix issues = %q", issues)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	doc, issues := memory.Parse(data)
	if len(issues) > 0 {
		t.Fatalf("final file invalid: %v", issues)
	}
	if len(doc.Log) != 2 {
		t.Fatalf("final log entries = %d, want 2", len(doc.Log))
	}
}

func TestMemoryGateMissingFile(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	script := fakeWaitFix() + `rm -f "$TREBI_MEMORY_PATH"
echo "$TREBI_RUN_COMPLETE"
waitfix
` + fakeWriteMemory(gateValidMemory) + `echo "$TREBI_RUN_COMPLETE"`
	run := launchMemoryScript(t, svc, memoryLaunch(root), script)

	assertRunComplete(t, run, runs.EndSentinel)
	kinds := runEventKinds(t, svc, run.ID)
	if kinds["memory.fix_requested"] != 1 || kinds["memory.invalid"] != 0 {
		t.Fatalf("memory events = %v", kinds)
	}
	if issues := runEventIssues(t, svc, run.ID, "memory.fix_requested"); !strings.Contains(issues, "file is missing") {
		t.Fatalf("fix issues = %q", issues)
	}
}

func TestMemoryGateInvalidShape(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	broken := `<!-- trebi memory v2. Keep these three sections in this order. Do not add other "## " headings. -->
# Memory

## Context

Client sends CSV.

## Log
`
	script := fakeWaitFix() + fakeWriteMemory(broken) + `echo "$TREBI_RUN_COMPLETE"
waitfix
` + fakeWriteMemory(gateValidMemory) + `echo "$TREBI_RUN_COMPLETE"`
	run := launchMemoryScript(t, svc, memoryLaunch(root), script)

	assertRunComplete(t, run, runs.EndSentinel)
	if issues := runEventIssues(t, svc, run.ID, "memory.fix_requested"); !strings.Contains(issues, `missing section "## Lessons"`) {
		t.Fatalf("fix issues = %q", issues)
	}
}

// TestMemoryGateDeprecatedState pins Part C: a v1 State section gets a
// notice at launch, fails the gate, and the fix message names the move.
func TestMemoryGateDeprecatedState(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	old := &memory.Doc{Context: "Client sends CSV.", DeprecatedState: "Sent: a, b, c."}
	p := filepath.Join(root, "demo", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, old.Marshal(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, prompt := launchMemoryRun(t, svc, memoryLaunch(root))
	for _, want := range []string{"## State (deprecated, move it)\nSent: a, b, c.", "This file has a deprecated State section. Move it this run."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}

	svc, _, root = memoryFixture(t)
	p = filepath.Join(root, "demo", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, old.Marshal(), 0o644); err != nil {
		t.Fatal(err)
	}
	mem := memoryLaunch(root)
	mem.Opt.RequireUpdate = false
	script := fakeWaitFix() + `echo "$TREBI_RUN_COMPLETE"
waitfix
` + fakeWriteMemory(gateValidMemory) + `echo "$TREBI_RUN_COMPLETE"`
	run := launchMemoryScript(t, svc, mem, script)

	assertRunComplete(t, run, runs.EndSentinel)
	issues := runEventIssues(t, svc, run.ID, "memory.fix_requested")
	if !strings.Contains(issues, "the State section is deprecated") || !strings.Contains(issues, "`trebi state`") {
		t.Fatalf("fix issues = %q", issues)
	}
}

// TestMemoryLaunchOverCapNotice pins Part D: memory over max_bytes gets the
// trim notice at launch and fails the gate with the section sizes.
func TestMemoryLaunchOverCapNotice(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	big := &memory.Doc{Context: strings.Repeat("c", 3000)}
	p := filepath.Join(root, "demo", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, big.Marshal(), 0o644); err != nil {
		t.Fatal(err)
	}
	mem := memoryLaunch(root)
	mem.Opt.MaxBytes = 1024
	mem.Opt.RequireUpdate = false
	script := `printf %s "$TREBI_FAKE_PROMPT" > "$TREBI_MEMORY_PATH.prompt"
` + fakeWaitFix() + `echo "$TREBI_RUN_COMPLETE"
waitfix
` + fakeWriteMemory(gateValidMemory) + `echo "$TREBI_RUN_COMPLETE"`
	run := launchMemoryScript(t, svc, mem, script)

	assertRunComplete(t, run, runs.EndSentinel)
	prompt, err := os.ReadFile(p + ".prompt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "Memory is 3 KB in the prompt; the cap is 1 KB. Trim it this run.") {
		t.Fatalf("prompt lacks the trim notice:\n%s", prompt)
	}
	issues := runEventIssues(t, svc, run.ID, "memory.fix_requested")
	if !strings.Contains(issues, "(Context 3 KB, Lessons 0 KB, Log 0 KB); the cap is 1 KB. Trim it in this order") {
		t.Fatalf("fix issues = %q", issues)
	}
}

// TestMemoryResumeSkipsPrefix pins the resume prompt: the session already
// holds memory and hints, so a resume sends only its own text. An edit to
// MEMORY.md while the run is parked adds one note.
func TestMemoryResumeSkipsPrefix(t *testing.T) {
	t.Parallel()
	svc, _, root := memoryFixture(t)
	svc.SetParker(runs.NewParker(svc, nil, &gateStub{open: true, hint: "gate hint"}))
	mem := memoryLaunch(root)
	mem.Opt.RequireUpdate = false
	dir := t.TempDir()
	fake := []string{"sh", "-c", `printf %s "$TREBI_FAKE_PROMPT" > prompt.txt; echo "$TREBI_RUN_COMPLETE"`}
	ctx := context.Background()
	prev, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Kind: runs.KindJob, Harness: "fake",
			Reason: runs.ReasonManual, WorkingDir: dir,
		},
		Prompt: "run the digest", Memory: mem, FakeCommand: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prev.Status != runs.StatusParked {
		t.Fatalf("first run: %s", prev.Status)
	}
	readPrompt := func() string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, "prompt.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if p := readPrompt(); !strings.Contains(p, "## Lessons") || !strings.Contains(p, "gate hint") {
		t.Fatalf("fresh prompt lacks memory or hint:\n%s", p)
	}

	base := runs.LaunchOpts{Memory: mem, FakeCommand: fake}
	next, err := svc.Resume(ctx, prev.ID, base, runs.ResumeOpts{Reason: runs.ReasonRequest, Prompt: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	if p := readPrompt(); p != "answer" {
		t.Fatalf("resume prompt = %q, want %q", p, "answer")
	}
	if next.MemoryHash == "" {
		t.Fatal("resume dropped memory_hash")
	}

	p := filepath.Join(root, "demo", "MEMORY.md")
	later := next.EndedAt.Add(time.Minute)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resume(ctx, next.ID, base, runs.ResumeOpts{Reason: runs.ReasonRequest, Prompt: "answer 2"}); err != nil {
		t.Fatal(err)
	}
	want := "[trebi] MEMORY.md changed since your last turn. Read it again before you continue.\n\nanswer 2"
	if got := readPrompt(); got != want {
		t.Fatalf("edited resume prompt = %q, want %q", got, want)
	}
}

// memoryFixture builds a service with a files jail over a temp workspace root.
// It returns the service, its store, and the root.
func memoryFixture(t *testing.T) (*runs.Service, *store.Store, string) {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	root := t.TempDir()
	return runs.New(st, nil, files.New(files.DefaultMaxBytes)), st, root
}

func memoryLaunch(root string) *runs.MemoryLaunch {
	return &runs.MemoryLaunch{
		Root: root, Rel: filepath.Join("demo", "MEMORY.md"),
		Opt: memory.Options{MaxLogs: 100, RequireUpdate: true, InjectLogs: 20},
	}
}

// gateValidMemory is a valid, changed MEMORY.md the gate fakes write back.
const gateValidMemory = `<!-- trebi memory v2. Keep these three sections in this order. Do not add other "## " headings. -->
# Memory

## Context

Client sends CSV.

## Lessons

## Log

- [2026-09-14 09:00] ran the digest
`

// fakeWaitFix defines the shell helper the gate fakes call between sentinels:
// it drains stdin until the injected fix message arrives, whatever its length.
func fakeWaitFix() string {
	return `waitfix() { while IFS= read -r l; do case "$l" in *"Fix MEMORY.md"*) return 0;; esac; done; }
`
}

// fakeWriteMemory builds the shell snippet that replaces MEMORY.md.
func fakeWriteMemory(body string) string {
	return "cat > \"$TREBI_MEMORY_PATH\" <<'MEOF'\n" + body + "MEOF\n"
}

// launchMemoryScript launches one fake job run that runs script.
func launchMemoryScript(t *testing.T, svc *runs.Service, mem *runs.MemoryLaunch, script string) *runs.Run {
	t.Helper()
	run, _, err := svc.CreateAndLaunch(context.Background(), runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Kind: runs.KindJob, Harness: "fake",
			Reason: runs.ReasonManual, WorkingDir: t.TempDir(),
		},
		Prompt:      "run the digest",
		Memory:      mem,
		FakeCommand: []string{"sh", "-c", script},
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// assertRunComplete asserts the run ended complete with the given reason.
func assertRunComplete(t *testing.T, run *runs.Run, end runs.EndReason) {
	t.Helper()
	if run.Status != runs.StatusComplete || run.EndReason != end {
		t.Fatalf("status=%s end=%s, want complete/%s", run.Status, run.EndReason, end)
	}
}

// runEventKinds counts run events by kind.
func runEventKinds(t *testing.T, svc *runs.Service, runID string) map[string]int {
	t.Helper()
	tl, err := svc.Timeline(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, ev := range tl.Events {
		kinds[ev.Kind]++
	}
	return kinds
}

// runEventIssues returns the joined issues field of the first event of kind.
func runEventIssues(t *testing.T, svc *runs.Service, runID, kind string) string {
	t.Helper()
	tl, err := svc.Timeline(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range tl.Events {
		if ev.Kind != kind {
			continue
		}
		var detail struct {
			Issues string `json:"issues"`
		}
		if err := json.Unmarshal(ev.Detail, &detail); err != nil {
			t.Fatal(err)
		}
		return detail.Issues
	}
	return ""
}

// launchMemoryRun launches one fake job run and returns it with the prompt the
// fake harness received.
func launchMemoryRun(t *testing.T, svc *runs.Service, mem *runs.MemoryLaunch) (*runs.Run, string) {
	t.Helper()
	dir := t.TempDir()
	run, _, err := svc.CreateAndLaunch(context.Background(), runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Kind: runs.KindJob, Harness: "fake",
			Reason: runs.ReasonManual, WorkingDir: dir,
		},
		Prompt:      "run the digest",
		Memory:      mem,
		FakeCommand: []string{"sh", "-c", `printf %s "$TREBI_FAKE_PROMPT" > prompt.txt; echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return run, string(prompt)
}

// fakeBriefer records the owner and returns a fixed brief.
type fakeBriefer struct{ owner state.Owner }

func (f *fakeBriefer) Brief(_ context.Context, o state.Owner) (string, error) {
	f.owner = o
	return "## State\n\n- seen: 3 rows", nil
}

func TestStateBriefWithoutMemory(t *testing.T) {
	t.Parallel()
	svc, _, _ := memoryFixture(t)
	b := &fakeBriefer{}
	svc.SetStateBriefer(b)
	dir := t.TempDir()
	_, _, err := svc.CreateAndLaunch(context.Background(), runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "jb_demo", JobName: "demo", Kind: runs.KindJob, Harness: "fake",
			Reason: runs.ReasonManual, WorkingDir: dir,
		},
		Prompt:      "run the digest",
		StateDecl:   map[string]state.Collection{"seen": {Description: "Sent items."}},
		FakeCommand: []string{"sh", "-c", `printf %s "$TREBI_FAKE_PROMPT" > prompt.txt; echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "## State\n\n- seen: 3 rows\n\nrun the digest"; !strings.Contains(string(prompt), want) {
		t.Fatalf("prompt:\n%s", prompt)
	}
	if b.owner.ID != "jb_demo" || b.owner.WorkspaceID != "ws" || b.owner.Collections["seen"].Description != "Sent items." {
		t.Fatalf("owner %+v", b.owner)
	}
}
