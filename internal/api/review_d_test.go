package api_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
