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
func TestIngestAcrossReopen(t *testing.T) {
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
		WorkspaceID: ws.ID, JobID: "j", JobName: "j", Harness: "claude", SessionID: "sess-i", WorkingDir: wd, ConversationID: conv.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := tmux.ByName("claude")
	path, err := tool.TranscriptPath(home.HarnessDir("claude"), wd, "sess-i")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "tmux", "testdata", "transcripts", "claude", "resumed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(fixture), "\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(lines[0]+lines[1]), 0o644); err != nil {
		t.Fatal(err)
	}
	c.ingestRun(ctx, run)

	// Turn 2 appends to the file; a restart drops the in-memory cursor.
	if err := os.WriteFile(path, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	c.turnMu.Lock()
	c.turnCursor = nil
	c.turnMu.Unlock()
	c.ingestRun(ctx, run)
	c.ingestRun(ctx, run)

	rows, err := st.DB().Query(`SELECT text FROM conversation_messages WHERE conversation_id=? AND role='assistant' ORDER BY turn_index`, conv.ID)
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
	if strings.Join(got, ",") != "OK,apple" {
		t.Fatalf("assistant messages %v", got)
	}
}
