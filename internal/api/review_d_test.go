package api_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/runs"
)

// TestReplayUsesCapturedTrigger covers D9/A6: replay rebuilds the prompt from
// the job file plus captured trigger vars, never from the previous preview.
func TestReplayUsesCapturedTrigger(t *testing.T) {
	s, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFile(t, wsDir, "ops")
	jobDir := filepath.Join(wsDir, "agents", "ops", "greeter")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\n---\n\nSay {{x}} now.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "replay-ws")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	defer st.Close()

	prev, err := s.Runs().Create(context.Background(), runs.CreateOpts{
		WorkspaceID: wsID, JobID: indexedJobID(t, st, wsID, "ops/greeter"), JobName: "ops/greeter", Harness: "fake",
		Reason:        runs.ReasonTrigger,
		PromptPreview: "STALE-PREVIEW-MUST-NOT-BE-REPLAYED",
		Trigger:       &runs.TriggerCapture{Connector: "src", Vars: map[string]string{"x": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Runs().Terminate(context.Background(), prev.ID, runs.StatusComplete, runs.EndSentinel, nil); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/runs/"+prev.ID+"/replay", nil)
	if code != 201 {
		t.Fatalf("replay %d %v", code, body)
	}
	if got := fmt.Sprint(body["prompt_preview"]); got != "Say hello now." {
		t.Fatalf("prompt_preview=%q want substituted job prompt", got)
	}
	trig, _ := body["trigger"].(map[string]any)
	if trig == nil {
		t.Fatalf("replay dropped trigger capture: %v", body)
	}
	vars, _ := trig["vars"].(map[string]any)
	if fmt.Sprint(vars["x"]) != "hello" {
		t.Fatalf("trigger vars not carried: %v", trig)
	}
	if fmt.Sprint(body["reason"]) != "replay" {
		t.Fatalf("reason=%v want replay", body["reason"])
	}
}

// TestRunExpandsJobDir: Run Now and dry-run fill {{job_dir}}/{{workspace_dir}}
// the same way a schedule fire does, without requiring them as inputs.
func TestRunExpandsJobDir(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFile(t, wsDir, "ops")
	jobDir := filepath.Join(wsDir, "agents", "ops", "digest")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\n---\n\nRun {{job_dir}}/scripts/x.sh in {{workspace_dir}} for {{who}}.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "jobdir-ws")

	code, body, _ := doJSON(t, "POST", base+"/v1/jobs/ops%2Fdigest/run", map[string]any{
		"workspace": wsID,
		"dry_run":   true,
		"inputs":    map[string]string{"who": "fritz"},
	})
	if code != 200 {
		t.Fatalf("dry_run %d %v", code, body)
	}
	got := fmt.Sprint(body["prompt_preview"])
	if !strings.Contains(got, jobDir+"/scripts/x.sh") {
		t.Fatalf("job_dir not expanded: %q", got)
	}
	if !strings.Contains(got, wsDir) {
		t.Fatalf("workspace_dir not expanded: %q", got)
	}
	if !strings.Contains(got, "fritz") {
		t.Fatalf("input who not expanded: %q", got)
	}
	if strings.Contains(got, "{{job_dir}}") || strings.Contains(got, "{{workspace_dir}}") {
		t.Fatalf("builtin placeholders left literal: %q", got)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/ops%2Fdigest/run", map[string]any{
		"workspace": wsID,
		"inputs":    map[string]string{"who": "fritz"},
	})
	if code != 201 {
		t.Fatalf("run %d %v", code, body)
	}
	got = fmt.Sprint(body["prompt_preview"])
	if strings.Contains(got, "{{job_dir}}") {
		t.Fatalf("launch left {{job_dir}} literal: %q", got)
	}
}

// TestAgentLaunchCanonical covers G.2: POST /v1/runs {kind:agent, job:cmo} is
// the canonical launch. The run carries the index name; the wake composer
// scopes the prompt to the agent folder. The resolver is kind-strict, so a
// kind=job launch cannot pick the agent row up.
func TestAgentLaunchCanonical(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-launch")

	code, run, _ := doJSON(t, "POST", base+"/v1/runs", map[string]any{
		"workspace": wsID, "kind": "agent", "job": "cmo",
	})
	if code != 201 {
		t.Fatalf("agent launch %d %v", code, run)
	}
	if run["kind"] != "agent" {
		t.Fatalf("kind=%v want agent", run["kind"])
	}
	if run["job_name"] != "cmo" || !strings.HasPrefix(fmt.Sprint(run["job_id"]), "agt_") {
		t.Fatalf("job_name=%v job_id=%v want index name cmo and an agent ID", run["job_name"], run["job_id"])
	}
	preview, _ := run["prompt_preview"].(string)
	if !strings.Contains(preview, "# Your jobs") || !strings.Contains(preview, "duty") {
		t.Fatalf("preview not agent-scoped: %q", preview)
	}
	if strings.Contains(preview, "alpha") || strings.Contains(preview, "# Workspace jobs") {
		t.Fatalf("preview leaks unowned workspace jobs: %q", preview)
	}
	if strings.Contains(preview, "# Conversation recap") {
		t.Fatalf("agent wake has no chat recap: %q", preview)
	}

	// Kind-strict resolver: without kind=agent the name cmo is not a job.
	code, body, _ := doJSON(t, "POST", base+"/v1/runs", map[string]any{
		"workspace": wsID, "job": "cmo",
	})
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("bare launch of agent name: %d %v want 404 not_found", code, body)
	}
}

// TestAgentRetrySkipReruns: a skipped agent wake has no session. Retry must
// rebuild from AGENT.md (not a JOB.md) and launch a fresh run.
func TestAgentRetrySkipReruns(t *testing.T) {
	s, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-retry")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	defer st.Close()

	prev, err := s.Runs().Create(context.Background(), runs.CreateOpts{
		WorkspaceID: wsID, JobID: indexedJobID(t, st, wsID, "cmo"), JobName: "cmo", Kind: runs.KindAgent,
		Harness: "fake", Reason: runs.ReasonTrigger,
		PromptPreview: "skipped wake",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Runs().Terminate(context.Background(), prev.ID, runs.StatusSkipped, runs.EndSkip, nil); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/runs/"+prev.ID+"/retry", nil)
	if code != 201 {
		t.Fatalf("retry skip agent %d %v", code, body)
	}
	if fmt.Sprint(body["kind"]) != "agent" {
		t.Fatalf("kind=%v want agent", body["kind"])
	}
	if fmt.Sprint(body["job_name"]) != "cmo" {
		t.Fatalf("job_name=%v want cmo", body["job_name"])
	}
	if got := fmt.Sprint(body["prompt_preview"]); !strings.Contains(got, "Run growth.") {
		t.Fatalf("retry did not rebuild from AGENT.md: %q", got)
	}
	if fmt.Sprint(body["id"]) == prev.ID {
		t.Fatal("retry skip reused the skipped row")
	}
}

// TestErrorEnvelopeShapeAndCodes covers D11: every error is
// {"error":{"code","message"}} with a stable code vocabulary.
func TestErrorEnvelopeShapeAndCodes(t *testing.T) {
	_, base, _ := startAPI(t)
	cases := []struct {
		method, path string
		wantStatus   int
		wantCode     string
	}{
		{"GET", "/v1/runs/01NOPE", 404, "not_found"},
		{"POST", "/v1/runs/01NOPE/kill", 404, "not_found"},
		{"POST", "/v1/runs/01NOPE/resume", 404, "not_found"},
		{"GET", "/v1/runs?cursor=not-a-ulid", 400, "validation"},
		{"GET", "/v1/jobs/nope", 404, "not_found"},
	}
	for _, c := range cases {
		code, body, _ := doJSON(t, c.method, base+c.path, nil)
		if code != c.wantStatus {
			t.Errorf("%s %s: status %d want %d (%v)", c.method, c.path, code, c.wantStatus, body)
			continue
		}
		e, _ := body["error"].(map[string]any)
		if e == nil {
			t.Errorf("%s %s: no error envelope: %v", c.method, c.path, body)
			continue
		}
		if fmt.Sprint(e["code"]) != c.wantCode {
			t.Errorf("%s %s: code=%v want %s", c.method, c.path, e["code"], c.wantCode)
		}
		if msg, _ := e["message"].(string); msg == "" {
			t.Errorf("%s %s: empty error message", c.method, c.path)
		}
	}
}

// TestRunsListCursorPaginationIsStable covers D14: pages do not overlap, the
// cursor walks the full set, and a malformed cursor gives 400.
func TestRunsListCursorPaginationIsStable(t *testing.T) {
	s, base, _ := startAPI(t)
	ctx := context.Background()
	const total = 7
	for i := 0; i < total; i++ {
		if _, err := s.Runs().Create(ctx, runs.CreateOpts{
			WorkspaceID: "ws", JobID: fmt.Sprintf("j%d", i), JobName: fmt.Sprintf("j%d", i), Harness: "fake",
		}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 5; page++ {
		url := base + "/v1/runs?limit=3"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		code, body, _ := doJSON(t, "GET", url, nil)
		if code != 200 {
			t.Fatalf("list %d %v", code, body)
		}
		items, _ := body["runs"].([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			id := fmt.Sprint(m["id"])
			if seen[id] {
				t.Fatalf("duplicate run %s across pages", id)
			}
			seen[id] = true
		}
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != total {
		t.Fatalf("pagination covered %d runs, want %d", len(seen), total)
	}

	code, body, _ := doJSON(t, "GET", base+"/v1/runs?cursor=@@bad@@", nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("malformed cursor: %d %v", code, body)
	}
}

// writeAgentFixture writes one workspace with the agent cmo and its duty
// job, and the agent ops and its alpha job.
func writeAgentFixture(t *testing.T, wsDir string) {
	t.Helper()
	files := map[string]string{
		filepath.Join(wsDir, "agents", "cmo", "AGENT.md"):        "---\ndescription: Test agent.\nharness: fake\n---\n\nRun growth.\n",
		filepath.Join(wsDir, "agents", "cmo", "duty", "JOB.md"):  "---\nharness: fake\n---\n\nDo the duty.\n",
		filepath.Join(wsDir, "agents", "ops", "AGENT.md"):        "---\ndescription: Ops agent.\nharness: fake\n---\n\nRun ops.\n",
		filepath.Join(wsDir, "agents", "ops", "alpha", "JOB.md"): "---\nharness: fake\n---\n\nDo alpha.\n",
	}
	for path, md := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRunTurnsRoute covers GET /v1/runs/{id}/turns and the turn fields of a
// reopened run. The old group parameter has no effect.
func TestRunTurnsRoute(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	svc := runs.New(st, nil, nil)
	ctx := context.Background()
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "fake", PromptPreview: "first"})
	if err != nil {
		t.Fatal(err)
	}
	tok := &pricing.TokenUsage{Input: 10, Output: 5, Model: "claude-sonnet-4-5"}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndSentinel, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reopen(ctx, run.ID, runs.CreateOpts{Reason: runs.ReasonResume, Harness: "fake", PromptPreview: "second"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusFailed, runs.EndLaunch, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "fake", PreviousRunID: run.ID}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	_, out, _ := doJSON(t, "GET", base+"/v1/runs/"+run.ID+"/turns", nil)
	list, _ := out["turns"].([]any)
	if len(list) != 2 {
		t.Fatalf("turns = %v", out)
	}
	first, _ := list[0].(map[string]any)
	second, _ := list[1].(map[string]any)
	tokens, _ := first["tokens"].(map[string]any)
	if first["n"] != float64(1) || first["status"] != "complete" || first["reason"] != "manual" || first["prompt_preview"] != "first" || tokens["input"] != float64(10) {
		t.Fatalf("turn 1 = %v", first)
	}
	if second["n"] != float64(2) || second["status"] != "failed" || second["end_reason"] != "launch" || second["reason"] != "resume" {
		t.Fatalf("turn 2 = %v", second)
	}
	for _, k := range []string{"started_at", "ended_at", "duration_ms"} {
		if _, ok := second[k]; !ok {
			t.Fatalf("turn 2 has no %s: %v", k, second)
		}
	}
	_, out, _ = doJSON(t, "GET", base+"/v1/runs/"+run.ID, nil)
	if out["turn_count"] != float64(2) || out["status"] != "failed" || out["turn_started_at"] == nil {
		t.Fatalf("run = %v", out)
	}
	for _, k := range []string{"first_run_id", "turns", "resumed_from"} {
		if _, ok := out[k]; ok {
			t.Fatalf("run still has %s: %v", k, out)
		}
	}
	_, out, _ = doJSON(t, "GET", base+"/v1/runs?group=true", nil)
	if rows, _ := out["runs"].([]any); len(rows) != 2 {
		t.Fatalf("group must be ignored: %v", out)
	}
	_, out, _ = doJSON(t, "GET", base+"/v1/runs/NOPE/turns", nil)
	if e, _ := out["error"].(map[string]any); e["code"] != "not_found" {
		t.Fatalf("missing run = %v", out)
	}
}

// TestTranscriptRunTurn: the transcript of a reopened run marks each entry
// with the run turn it belongs to.
func TestTranscriptRunTurn(t *testing.T) {
	_, base, _ := startAPI(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	st := mustOpenStore(t, home.DBPath())
	defer st.Close()
	wd := t.TempDir()
	tool, _ := agentcli.ByName("claude")
	path, err := tool.TranscriptPath(home.HarnessDir("claude"), wd, "sess-r")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "agentcli", "testdata", "transcripts", "claude", "resumed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO runs (id,workspace_id,job_id,status,trace_id,harness,runtime,session_id,working_dir,turn_count,started_at,updated_at)
			VALUES ('R','ws','j','complete','t','claude','tui','sess-r','` + wd + `',2,'2026-09-30T12:00:00Z','x')`,
		`INSERT INTO run_turns (run_id,n,reason,status,started_at) VALUES
			('R',1,'manual','complete','2026-09-30T12:00:00Z'), ('R',2,'trigger','complete','2026-09-30T14:00:00Z')`,
	} {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	code, body, _ := doJSON(t, "GET", base+"/v1/runs/R/transcript", nil)
	entries, _ := body["turns"].([]any)
	if code != 200 || len(entries) != 4 {
		t.Fatalf("transcript %d %v", code, body)
	}
	var got []string
	for _, e := range entries {
		m, _ := e.(map[string]any)
		got = append(got, fmt.Sprint(m["run_turn"]))
	}
	if strings.Join(got, ",") != "1,1,2,2" {
		t.Fatalf("run_turn %v", got)
	}
}

// TestAgentHookWaitingOpensApprovalOnlyOffWire: on the cli runtime the wire
// owns permissions, so a waiting hook (grok sends one for each allowed MCP
// call) opens no approval. A pane run still gets one.
func TestAgentHookWaitingOpensApprovalOnlyOffWire(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	svc := runs.New(st, nil, nil)
	ctx := context.Background()
	wire, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "grok", Runtime: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	pane, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j2", JobName: "j2", Harness: "grok", Runtime: "tui"})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	for _, id := range []string{wire.ID, pane.ID} {
		if code, out, _ := doJSON(t, "POST", base+"/v1/agent-hook/waiting?run_id="+id, map[string]any{"message": "Tool permission requested"}); code != 200 {
			t.Fatalf("hook %s = %d %v", id, code, out)
		}
	}
	_, out, _ := doJSON(t, "GET", base+"/v1/approvals?state=pending", nil)
	list, _ := out["approvals"].([]any)
	if len(list) != 1 {
		t.Fatalf("pending approvals = %v", out)
	}
	if a, _ := list[0].(map[string]any); a["run_id"] != pane.ID {
		t.Fatalf("approval on the wrong run: %v", a)
	}
}

// TestRunChaptersRoute: the chapters of a run group its tool steps, and the
// route answers 404 for an unknown run and 410 for a pruned run.
func TestRunChaptersRoute(t *testing.T) {
	_, base, _ := startAPI(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	st := mustOpenStore(t, home.DBPath())
	defer st.Close()
	wd := t.TempDir()
	tool, _ := agentcli.ByName("claude")
	path, err := tool.TranscriptPath(home.HarnessDir("claude"), wd, "sess-c")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "agentcli", "testdata", "transcripts", "claude", "labels.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO runs (id,workspace_id,job_id,status,trace_id,harness,runtime,session_id,working_dir,turn_count,started_at,ended_at,updated_at)
			VALUES ('C','ws','j','complete','t','claude','tui','sess-c','` + wd + `',1,'2026-08-28T12:00:00Z','2026-08-28T12:01:00Z','x')`,
		`INSERT INTO runs (id,workspace_id,job_id,status,trace_id,harness,runtime,started_at,ended_at,pruned_at,updated_at)
			VALUES ('P','ws','j','complete','t','claude','tui','2026-08-28T12:00:00Z','2026-08-28T12:01:00Z','2026-09-01T00:00:00Z','x')`,
	} {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	code, body, _ := doJSON(t, "GET", base+"/v1/runs/C/chapters", nil)
	if code != 200 || body["run_id"] != "C" || body["step_count"] != float64(4) {
		t.Fatalf("chapters %d %v", code, body)
	}
	list, _ := body["chapters"].([]any)
	first, _ := list[0].(map[string]any)
	if len(list) < 3 || first["event"] != "started" {
		t.Fatalf("chapters %v", list)
	}
	if code, _, _ = doJSON(t, "GET", base+"/v1/runs/nope/chapters", nil); code != 404 {
		t.Fatalf("unknown run: %d", code)
	}
	if code, _, _ = doJSON(t, "GET", base+"/v1/runs/P/chapters", nil); code != 410 {
		t.Fatalf("pruned run: %d", code)
	}
}
