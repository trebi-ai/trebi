package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeTranscriptFixtures(t *testing.T) {
	t.Parallel()
	full := readTranscript(t, "claude", "full.jsonl")
	turns, err := claudeTool{}.ParseTranscript(bytes.NewReader(full))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns %d want 2", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Text != "Hello there" {
		t.Fatalf("user %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "Hi! How can I help?" {
		t.Fatalf("assistant %+v", turns[1])
	}
	if turns[1].At.IsZero() {
		t.Fatal("missing timestamp")
	}

	empty, err := claudeTool{}.ParseTranscript(bytes.NewReader(nil))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty %v %v", empty, err)
	}

	tools := readTranscript(t, "claude", "tools.jsonl")
	tt, err := claudeTool{}.ParseTranscript(bytes.NewReader(tools))
	if err != nil {
		t.Fatal(err)
	}
	if len(tt) != 3 {
		t.Fatalf("tools turns %d: %+v", len(tt), tt)
	}
	if tt[1].Role != "assistant" || len(tt[1].Tools) != 1 || tt[1].Tools[0].Name != "Bash" {
		t.Fatalf("tool_use %+v", tt[1])
	}
	if tt[2].Role != "tools" || len(tt[2].Tools) != 1 {
		t.Fatalf("tool_result %+v", tt[2])
	}
}

func TestParseTranscriptBytesTruncated(t *testing.T) {
	t.Parallel()
	data := readTranscript(t, "claude", "truncated.jsonl")
	turns, n, err := ParseTranscriptBytes(claudeTool{}, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("complete turns %d want 2", len(turns))
	}
	if int(n) >= len(data) {
		t.Fatalf("consumed incomplete line n=%d len=%d", n, len(data))
	}
	if data[n-1] != '\n' {
		t.Fatal("consumed offset not on newline")
	}
}

// TestParseCodexTranscriptFixture pins the codex rollout reader. Without it an
// assistant reply never reaches its conversation: ingest reads the transcript
// from disk, and codex used to report that it had none.
func TestParseCodexTranscriptFixture(t *testing.T) {
	t.Parallel()
	data := readTranscript(t, "codex", "full.jsonl")
	turns, err := codexTool{}.ParseTranscript(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, tn := range turns {
		roles = append(roles, tn.Role)
	}
	want := []string{"user", "tools", "assistant"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
	if turns[2].Text != "It holds the ops job." {
		t.Errorf("assistant text = %q", turns[2].Text)
	}
	if turns[0].Text != "what is this workspace for?" {
		t.Errorf("user text = %q", turns[0].Text)
	}
	if got := turns[1].Tools; len(got) != 1 || got[0].Name != "exec_command" || got[0].Output != "JOB.md\n" {
		t.Errorf("tools = %+v", got)
	}
	if turns[2].At.IsZero() {
		t.Error("assistant turn has no timestamp")
	}
}

// TestParseCodexTranscriptEmpty pins that an empty rollout is not an error.
func TestParseCodexTranscriptEmpty(t *testing.T) {
	t.Parallel()
	turns, err := codexTool{}.ParseTranscript(bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 0 {
		t.Fatalf("turns %+v", turns)
	}
}

func TestParseGrokTranscriptFixture(t *testing.T) {
	t.Parallel()
	data := readTranscript(t, "grok", "full.jsonl")
	turns, err := grokTool{}.ParseTranscript(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 4 {
		t.Fatalf("turns %d want 4: %+v", len(turns), turns)
	}
	if turns[0].Role != "user" || !strings.HasPrefix(turns[0].Text, "hi") {
		t.Fatalf("user0 %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "hi" {
		t.Fatalf("assistant0 %+v", turns[1])
	}
	if turns[2].Role != "user" || turns[2].Text != "hello" {
		t.Fatalf("user1 %+v", turns[2])
	}
	if turns[3].Role != "assistant" || turns[3].Text != "hello" {
		t.Fatalf("assistant1 %+v", turns[3])
	}
}

func TestParseTranscriptUnsupported(t *testing.T) {
	t.Parallel()
	_, err := geminiTool{}.ParseTranscript(bytes.NewReader(nil))
	if !errors.Is(err, ErrTranscriptUnsupported) {
		t.Fatalf("gemini %v", err)
	}
}

func TestParseTranscriptFake(t *testing.T) {
	t.Parallel()
	turns, err := fakeTool{}.ParseTranscript(bytes.NewReader([]byte("{\"role\":\"assistant\",\"text\":\"fake: ok\"}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Role != "assistant" || turns[0].Text != "fake: ok" {
		t.Fatalf("%+v", turns)
	}
}

// TestParseResumedTranscripts: a resumed session keeps both prompts in one
// file, each with its time, so the API can label the run turn of each entry.
func TestParseResumedTranscripts(t *testing.T) {
	t.Parallel()
	for name, tool := range map[string]Tool{"claude": claudeTool{}, "codex": codexTool{}} {
		turns, err := tool.ParseTranscript(bytes.NewReader(readTranscript(t, name, "resumed.jsonl")))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var roles []string
		for _, tr := range turns {
			if tr.At.IsZero() {
				t.Fatalf("%s: entry without time %+v", name, tr)
			}
			roles = append(roles, tr.Role)
		}
		if strings.Join(roles, ",") != "user,assistant,user,assistant" || turns[3].Text != "apple" {
			t.Fatalf("%s: turns %+v", name, turns)
		}
	}
}

func readTranscript(t *testing.T, tool, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "transcripts", tool, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCodexTranscriptPathByCwd pins the fallback that makes ingest work at all:
// codex mints its own rollout id, so a trebi session id never matches a file
// name and the session must be found by its working directory.
func TestCodexTranscriptPathByCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	dir := codexSessionDay(t, home)
	data := readTranscript(t, "codex", "full.jsonl")
	hit := filepath.Join(dir, "rollout-2026-09-01T14-36-33-01a05d66-6eaa-7961-b470-6964936c59b2.jsonl")
	if err := os.WriteFile(hit, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// A rollout from another directory must not match.
	other := filepath.Join(dir, "rollout-2026-09-01T14-40-00-01a05d99-0000-7000-8000-000000000000.jsonl")
	if err := os.WriteFile(other, []byte(`{"type":"session_meta","payload":{"cwd":"/elsewhere"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := codexTool{}.TranscriptPath(home, "/w/ops", "trebi-generated-uuid")
	if err != nil {
		t.Fatalf("by cwd: %v", err)
	}
	if got != hit {
		t.Fatalf("path = %s, want %s", got, hit)
	}

	if _, err := (codexTool{}).TranscriptPath(home, "/no/such/dir", "trebi-generated-uuid"); err == nil {
		t.Error("unknown working dir resolved a rollout")
	}
	if _, err := (codexTool{}).TranscriptPath(home, "", ""); err == nil {
		t.Error("empty session id and dir resolved a rollout")
	}
}

// Several runs of one assistant share a working directory, so the cwd fallback
// alone would give every run the same rollout. The run start time picks the one
// that belongs to it.
func TestCodexTranscriptPathByStartTime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	dir := codexSessionDay(t, home)
	write := func(name, started string) string {
		p := filepath.Join(dir, name)
		line := `{"type":"session_meta","payload":{"cwd":"/w/ops","timestamp":"` + started + `"}}` + "\n"
		if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := time.Now().UTC().Add(-2 * time.Hour)
	at := func(d time.Duration) time.Time { return base.Add(d) }
	stamp := func(d time.Duration) string { return at(d).Format(time.RFC3339) }

	first := write("rollout-a-01a00000-0000-7000-8000-000000000001.jsonl", stamp(0))
	second := write("rollout-b-01a00000-0000-7000-8000-000000000002.jsonl", stamp(3*time.Minute))

	cases := []struct {
		run  time.Duration
		want string
	}{
		{-3 * time.Second, first},
		{3*time.Minute - 2*time.Second, second},
	}
	for _, c := range cases {
		got, err := codexTool{}.TranscriptPath(home, CodexWorkingDir("/w/ops", at(c.run)), "trebi-uuid")
		if err != nil {
			t.Fatalf("run %s: %v", c.run, err)
		}
		if got != c.want {
			t.Errorf("run %s: path = %s, want %s", c.run, got, c.want)
		}
	}

	// A run that started after every rollout has no session of its own.
	if _, err := (codexTool{}).TranscriptPath(home, CodexWorkingDir("/w/ops", at(time.Hour)), "trebi-uuid"); err == nil {
		t.Error("a run later than every rollout resolved one anyway")
	}
}

// The daemon passes the trebi harness directory, not a user home. Codex still
// writes to the user store because PrepareLaunch leaves CODEX_HOME unset, so
// that store must stay in the search list.
func TestCodexSessionRootsKeepsUserStore(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no user home")
	}
	user := filepath.Join(home, ".codex", "sessions")
	if st, err := os.Stat(user); err != nil || !st.IsDir() {
		t.Skip("no user codex store")
	}
	roots := codexSessionRoots(t.TempDir())
	for _, r := range roots {
		if r == user {
			return
		}
	}
	t.Fatalf("user store %s missing from roots %v", user, roots)
}

// A busy day holds far more rollouts than a run's own. The right one is matched
// by start time, which does not order the same way as mtime: a session keeps
// writing after it starts. Selection must not drop candidates by count.
func TestCodexRolloutFoundBehindNewerOnes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	now := time.Now().UTC()
	day := filepath.Join(home, ".codex", "sessions",
		now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	started := now.Add(-2 * time.Hour)

	// The one we want: correct cwd, correct start, but the oldest mtime.
	want := filepath.Join(day, "rollout-target.jsonl")
	line := fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":"/w/ops","timestamp":%q}}`, started.Format(time.RFC3339Nano))
	if err := os.WriteFile(want, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(want, started, started); err != nil {
		t.Fatal(err)
	}

	// 100 unrelated rollouts, all newer.
	for i := range 100 {
		p := filepath.Join(day, fmt.Sprintf("rollout-noise-%03d.jsonl", i))
		if err := os.WriteFile(p, []byte(`{"type":"session_meta","payload":{"cwd":"/elsewhere"}}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		m := now.Add(-time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, m, m); err != nil {
			t.Fatal(err)
		}
	}

	got := newestCodexRolloutFor(codexSessionRoots(""), "/w/ops", started.Add(-time.Minute))
	t.Logf("got  = %s", got)
	t.Logf("want = %s", want)
	if got != want {
		t.Errorf("scan limit dropped the correct rollout")
	}
}

// codexSessionDay makes the rollout directory for today. The lookup only scans
// the last two day directories, so a fixed date turns the test into a time bomb.
func codexSessionDay(t *testing.T, home string) string {
	t.Helper()
	now := time.Now().UTC()
	dir := filepath.Join(home, ".codex", "sessions",
		now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestParseOpenCodeMessages pins the serve transcript fallback shape:
// {info, parts} pairs map to user/assistant/tools turns.
func TestParseOpenCodeMessages(t *testing.T) {
	t.Parallel()
	raw := []byte(`[
  {"info":{"role":"user","time":{"created":1000}},"parts":[{"type":"text","text":"hi"}]},
  {"info":{"role":"assistant","time":{"created":2000}},"parts":[
    {"type":"text","text":"hello"},
    {"type":"tool","tool":"bash","state":{"status":"completed","input":{"cmd":"ls"},"output":"ok"}}
  ]}
]`)
	turns, err := ParseOpenCodeMessages(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns: %+v", turns)
	}
	if turns[0].Role != "user" || turns[0].Text != "hi" || turns[0].At.IsZero() {
		t.Fatalf("user: %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "hello" {
		t.Fatalf("assistant: %+v", turns[1])
	}
	if len(turns[1].Tools) != 1 || turns[1].Tools[0].Name != "bash" || turns[1].Tools[0].Output != "ok" {
		t.Fatalf("tools: %+v", turns[1].Tools)
	}
	if !strings.Contains(turns[1].Tools[0].Input, "ls") {
		t.Fatalf("tool input: %q", turns[1].Tools[0].Input)
	}
}
