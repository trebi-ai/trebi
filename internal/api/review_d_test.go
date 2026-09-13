package api_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/runs"
)

// TestReplayUsesCapturedTrigger covers D9/A6: replay rebuilds the prompt from
// the job file plus captured trigger vars, never from the previous preview.
func TestReplayUsesCapturedTrigger(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	jobDir := filepath.Join(wsDir, "greeter")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\n---\n\nSay {{x}} now.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, s.Token(), wsDir, "replay-ws")

	prev, err := s.Runs().Create(context.Background(), runs.CreateOpts{
		WorkspaceID: wsID, Job: "greeter", Harness: "fake",
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

	code, body, _ := doJSON(t, "POST", base+"/v1/runs/"+prev.ID+"/replay", s.Token(), nil)
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

// TestRetrySkipRerunsAssistant: a skipped assistant wake has no session.
// Retry must rebuild from TREBI.md (not JOB.md) and launch a fresh run.
func TestRetrySkipRerunsAssistant(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, "TREBI.md"), []byte("---\nharness: fake\n---\n\nYou are the assistant.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, s.Token(), wsDir, "assistant-retry")

	prev, err := s.Runs().Create(context.Background(), runs.CreateOpts{
		WorkspaceID: wsID, Job: "assistant-retry", Kind: runs.KindAssistant,
		Harness: "fake", Reason: runs.ReasonTrigger,
		PromptPreview: "skipped wake",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Runs().Terminate(context.Background(), prev.ID, runs.StatusSkipped, runs.EndSkip, nil); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/runs/"+prev.ID+"/retry", s.Token(), nil)
	if code != 201 {
		t.Fatalf("retry skip assistant %d %v", code, body)
	}
	if fmt.Sprint(body["kind"]) != "assistant" {
		t.Fatalf("kind=%v want assistant", body["kind"])
	}
	if fmt.Sprint(body["id"]) == prev.ID {
		t.Fatal("retry skip reused the skipped row")
	}
}

// TestAgentLaunchCanonical covers G.2: POST /v1/runs {kind:agent, job:cmo} is
// the canonical launch. The run carries the index name; the wake composer
// scopes the prompt to the agent folder. The resolver is kind-strict, so a
// kind=job launch cannot pick the agent row up.
func TestAgentLaunchCanonical(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, s.Token(), wsDir, "agent-launch")

	code, run, _ := doJSON(t, "POST", base+"/v1/runs", s.Token(), map[string]any{
		"workspace": wsID, "kind": "agent", "job": "cmo",
	})
	if code != 201 {
		t.Fatalf("agent launch %d %v", code, run)
	}
	if run["kind"] != "agent" {
		t.Fatalf("kind=%v want agent", run["kind"])
	}
	if run["job"] != "agents/cmo" {
		t.Fatalf("job=%v want index name agents/cmo", run["job"])
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
	code, body, _ := doJSON(t, "POST", base+"/v1/runs", s.Token(), map[string]any{
		"workspace": wsID, "job": "cmo",
	})
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("bare launch of agent name: %d %v want 404 not_found", code, body)
	}
}

// TestAgentRetrySkipReruns: a skipped agent wake has no session. Retry must
// rebuild from AGENT.md (not a JOB.md) and launch a fresh run.
func TestAgentRetrySkipReruns(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, s.Token(), wsDir, "agent-retry")

	prev, err := s.Runs().Create(context.Background(), runs.CreateOpts{
		WorkspaceID: wsID, Job: "agents/cmo", Kind: runs.KindAgent,
		Harness: "fake", Reason: runs.ReasonTrigger,
		PromptPreview: "skipped wake",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Runs().Terminate(context.Background(), prev.ID, runs.StatusSkipped, runs.EndSkip, nil); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/runs/"+prev.ID+"/retry", s.Token(), nil)
	if code != 201 {
		t.Fatalf("retry skip agent %d %v", code, body)
	}
	if fmt.Sprint(body["kind"]) != "agent" {
		t.Fatalf("kind=%v want agent", body["kind"])
	}
	if fmt.Sprint(body["job"]) != "agents/cmo" {
		t.Fatalf("job=%v want agents/cmo", body["job"])
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
	s, base, _ := startAPI(t)
	cases := []struct {
		method, path, token string
		wantStatus          int
		wantCode            string
	}{
		{"GET", "/v1/runs/01NOPE", s.Token(), 404, "not_found"},
		{"POST", "/v1/runs/01NOPE/kill", s.Token(), 404, "not_found"},
		{"POST", "/v1/runs/01NOPE/resume", s.Token(), 404, "not_found"},
		{"GET", "/v1/runs?cursor=not-a-ulid", s.Token(), 400, "validation"},
		{"GET", "/v1/jobs/nope", s.Token(), 404, "not_found"},
		{"GET", "/v1/system/health", "bad-token", 401, "unauthorized"},
	}
	for _, c := range cases {
		code, body, _ := doJSON(t, c.method, base+c.path, c.token, nil)
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
			WorkspaceID: "ws", Job: fmt.Sprintf("j%d", i), Harness: "fake",
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
		code, body, _ := doJSON(t, "GET", url, s.Token(), nil)
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

	code, body, _ := doJSON(t, "GET", base+"/v1/runs?cursor=@@bad@@", s.Token(), nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("malformed cursor: %d %v", code, body)
	}
}

// writeAgentFixture writes one workspace with the agent cmo, one owned duty
// job, and one unowned job.
func writeAgentFixture(t *testing.T, wsDir string) {
	t.Helper()
	files := map[string]string{
		filepath.Join(wsDir, "agents", "cmo", "AGENT.md"):       "---\nharness: fake\n---\n\nRun growth.\n",
		filepath.Join(wsDir, "agents", "cmo", "duty", "JOB.md"): "---\nharness: fake\n---\n\nDo the duty.\n",
		filepath.Join(wsDir, "alpha", "JOB.md"):                 "---\nharness: fake\n---\n\nDo alpha.\n",
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
