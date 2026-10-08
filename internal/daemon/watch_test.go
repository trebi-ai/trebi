package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tmux"
)

func TestReadTranscriptDeltaResumeFromOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	first := []byte("{\"role\":\"assistant\",\"text\":\"one\"}\n")
	if err := os.WriteFile(path, first, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, base, from, skip, err := readTranscriptDelta(path, transcriptCursor{})
	if err != nil || skip || base != 0 || from != 0 {
		t.Fatalf("first read skip=%v base=%d from=%d err=%v", skip, base, from, err)
	}
	if string(data) != string(first) {
		t.Fatalf("first data %q", data)
	}
	cur := transcriptCursor{
		path: path, offset: int64(len(first)), size: st.Size(),
		mtime: st.ModTime().UnixNano(), turnCount: 1,
	}
	_, _, _, skip, err = readTranscriptDelta(path, cur)
	if err != nil || !skip {
		t.Fatalf("unchanged skip=%v err=%v", skip, err)
	}

	time.Sleep(10 * time.Millisecond) // some FS have 1s mtime granularity; size still changes
	second := []byte("{\"role\":\"assistant\",\"text\":\"two\"}\n")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(second); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	data, base, from, skip, err = readTranscriptDelta(path, cur)
	if err != nil || skip {
		t.Fatalf("append skip=%v err=%v", skip, err)
	}
	if base != int64(len(first)) || from != 1 {
		t.Fatalf("append base=%d from=%d", base, from)
	}
	if string(data) != string(second) {
		t.Fatalf("append data %q", data)
	}
}

// TestIngestAcrossReopen: a reopened run appends to the same session file.
// After a restart the cursor starts at 0, and the dedupe on the run id still
// stores each assistant message once.
// ingestFixture is a control plane with one claude run on a conversation.
type ingestFixture struct {
	c    *ControlPlane
	st   *store.Store
	run  *runs.Run
	conv string
	path string
}

func newIngestFixture(t *testing.T, sessionID string) ingestFixture {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	c := New(st, Options{})
	ws, err := c.Workspaces.Add(ctx, t.TempDir(), "ws")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := c.Conversations.Create(ctx, conversations.CreateOpts{Workspace: ws.ID, OriginChannel: "ui"})
	if err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()
	run, err := c.Runs.Create(ctx, runs.CreateOpts{
		WorkspaceID: ws.ID, JobID: "jb_j", JobName: "j", Harness: "claude", SessionID: sessionID, WorkingDir: wd, ConversationID: conv.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := tmux.ByName("claude")
	path, err := tool.TranscriptPath(home.HarnessDir("claude"), wd, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	return ingestFixture{c: c, st: st, run: run, conv: conv.ID, path: path}
}

// assistantTexts returns the stored assistant rows in turn order.
func (f ingestFixture) assistantTexts(t *testing.T) []string {
	t.Helper()
	rows, err := f.st.DB().Query(`SELECT text FROM conversation_messages WHERE conversation_id=? AND role='assistant' ORDER BY turn_index`, f.conv)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		got = append(got, text)
	}
	return got
}

func TestIngestAcrossReopen(t *testing.T) {
	f := newIngestFixture(t, "sess-i")
	ctx := context.Background()
	fixture, err := os.ReadFile(filepath.Join("..", "tmux", "testdata", "transcripts", "claude", "resumed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(fixture), "\n")
	if err := os.WriteFile(f.path, []byte(lines[0]+lines[1]), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.ingestRun(ctx, f.run)

	// Turn 2 appends to the file; a restart drops the in-memory cursor.
	if err := os.WriteFile(f.path, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.turnMu.Lock()
	f.c.turnCursor = nil
	f.c.turnMu.Unlock()
	f.c.ingestRun(ctx, f.run)
	f.c.ingestRun(ctx, f.run)

	if got := f.assistantTexts(t); strings.Join(got, ",") != "OK,apple" {
		t.Fatalf("assistant messages %v", got)
	}
}

// A channel reply must not carry the completion line or the answer to a
// memory fix (run 01M4DJFC18WKPTAYSBSEHNDSWG).
func TestIngestDropsMemoryFixAndCompletion(t *testing.T) {
	f := newIngestFixture(t, "sess-m")
	ctx := context.Background()
	line := func(typ, at, content string) string {
		return `{"type":"` + typ + `","timestamp":"2026-10-08T10:` + at + `Z","message":{"role":"` + typ + `","content":` + content + "}}\n"
	}
	head := line("user", "55:48.000", `"Which jobs are active?"`) +
		line("assistant", "56:11.000", `[{"type":"text","text":"Ten jobs are active.\n\nTREBI_RUN_COMPLETE"}]`) +
		line("user", "56:12.000", `"MEMORY.md: unchanged this run. Add a Log entry for this run.\nFix MEMORY.md, then finish the run."`)
	tail := line("assistant", "56:20.000", `[{"type":"tool_use","name":"Edit","input":{}}]`) +
		line("user", "56:21.000", `[{"type":"tool_result","content":"ok"}]`) +
		line("assistant", "56:55.000", `[{"type":"text","text":"I updated MEMORY.md.\n\nTREBI_RUN_COMPLETE"}]`) +
		line("user", "57:30.000", `"And which are off?"`) +
		line("assistant", "57:40.000", `[{"type":"text","text":"One job is off."}]`)

	// The fix answer arrives on a later tick than the fix prompt.
	if err := os.WriteFile(f.path, []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.ingestRun(ctx, f.run)
	if err := os.WriteFile(f.path, []byte(head+tail), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.ingestRun(ctx, f.run)

	if got := f.assistantTexts(t); strings.Join(got, "|") != "Ten jobs are active.|One job is off." {
		t.Fatalf("assistant messages %q", got)
	}
}
