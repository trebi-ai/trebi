package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
)

func TestLastLogLines(t *testing.T) {
	t.Parallel()
	got := lastLogLines("a\nb\nc\n", 2)
	if strings.Join(got, ",") != "b,c" {
		t.Fatalf("got %v", got)
	}
	if lastLogLines("", 10) != nil {
		t.Fatal("empty")
	}
	if got := lastLogLines("only", 100); len(got) != 1 || got[0] != "only" {
		t.Fatalf("short %v", got)
	}
}

func TestSliceLogLines(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "L%02d\n", i)
	}
	chunk, offset, total := sliceLogLines(b.String(), 5, 3)
	if total != 10 || offset != 2 || strings.Join(chunk, ",") != "L03,L04,L05" {
		t.Fatalf("chunk=%v offset=%d total=%d", chunk, offset, total)
	}
	chunk, offset, total = sliceLogLines(b.String(), 2, 10)
	if offset != 0 || total != 10 || strings.Join(chunk, ",") != "L01,L02" {
		t.Fatalf("clamp chunk=%v offset=%d", chunk, offset)
	}
	chunk, offset, total = sliceLogLines(b.String(), 0, 5)
	if offset != 0 || total != 10 || len(chunk) != 0 {
		t.Fatalf("before 0: chunk=%v offset=%d total=%d", chunk, offset, total)
	}
}

func TestLogSuffixLines(t *testing.T) {
	t.Parallel()
	if logSuffixLines("abc", "abc") != nil {
		t.Fatal("equal")
	}
	got := logSuffixLines("ab", "abcd")
	if strings.Join(got, ",") != "cd" {
		t.Fatalf("suffix %v", got)
	}
	got = logSuffixLines("", "l1\nl2")
	if strings.Join(got, ",") != "l1,l2" {
		t.Fatalf("empty prev %v", got)
	}
	if logSuffixLines("xy", "ab") != nil {
		t.Fatal("rewrite is not a suffix")
	}
}

// TestLaunchOptsWorkingDirCoversG5: an AGENT.md resolves its cwd to the
// workspace root; a JOB.md keeps its own folder; working_dir: wins for both.
func TestLaunchOptsWorkingDirCoversG5(t *testing.T) {
	t.Parallel()
	wsDir := t.TempDir()
	agentMD := filepath.Join(wsDir, "agents", "cmo", "AGENT.md")
	if err := os.MkdirAll(filepath.Dir(agentMD), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentMD, []byte("---\nharness: claude\n---\n\nRun growth.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := job.ParseFile(agentMD)
	if err != nil {
		t.Fatal(err)
	}
	lopts := launchOpts(runCreateRequest{Kind: runs.KindAgent, Job: "agents/cmo"}, runs.ReasonManual, "", "", spec, nil)
	if lopts.WorkingDir != wsDir {
		t.Fatalf("agent cwd=%q want workspace root %q", lopts.WorkingDir, wsDir)
	}
}

// `trebi agent-hook` wraps the harness body in a payload string, so an
// approval opened from a waiting hook must still carry the question.
func TestHookQuestionReadsWrappedPayload(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{"wrapped", `{"payload":"{\"question\":\"delete prod?\"}"}`, "delete prod?"},
		{"bare", `{"prompt":"delete prod?"}`, "delete prod?"},
		{"wrapped prose", `{"payload":"not json"}`, ""},
		{"empty", `{}`, ""},
		{"garbage", `nope`, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hookQuestion([]byte(tt.body)); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

// Codex mints its own rollout id, so a trebi session id never matches a file
// name. The transcript is found by the directory the run launched in, which
// only reaches this reader because the run row persists it.
func TestReadTranscriptFindsCodexRolloutByWorkingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	now := time.Now().UTC()
	dir := filepath.Join(home, ".codex", "sessions",
		now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	workingDir := t.TempDir()
	rollout := filepath.Join(dir, "rollout-a-01a00000-0000-7000-8000-000000000001.jsonl")
	line := `{"type":"session_meta","payload":{"cwd":"` + workingDir +
		`","timestamp":"` + now.Add(-time.Minute).Format(time.RFC3339) + `"}}` + "\n"
	if err := os.WriteFile(rollout, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	run := &runs.Run{
		ID: "r1", Harness: agentcli.Codex, SessionID: "trebi-minted-id",
		WorkingDir: workingDir, StartedAt: now.Add(-2 * time.Minute),
	}
	if _, _, ok := s.readTranscript(run); !ok {
		t.Fatal("no transcript for a run whose working dir names its rollout")
	}

	// Without the working dir there is nothing to match, which is the bug.
	run.WorkingDir = ""
	if _, _, ok := s.readTranscript(run); ok {
		t.Fatal("resolved a rollout with no working dir to match on")
	}
}
