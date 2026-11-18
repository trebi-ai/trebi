package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
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
	tool, _ := agentcli.ByName("claude")
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
	fixture, err := os.ReadFile(filepath.Join("..", "agentcli", "testdata", "transcripts", "claude", "resumed.jsonl"))
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
		line("user", "56:12.000", `"MEMORY.md: unchanged this run. Add a Log entry for this run.\nFix MEMORY.md. Trebi checks the file again when you finish. Then finish the run."`)
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

func TestIngestDropsSyntheticReply(t *testing.T) {
	f := newIngestFixture(t, "sess-s")
	lines := ingestLine("user", "00:01.000", "", "", `"Q1"`) +
		ingestLine("assistant", "00:02.000", "<synthetic>", "", `[{"type":"text","text":"No response requested."}]`) +
		ingestLine("user", "00:03.000", "", "", `"Q2"`) +
		ingestLine("assistant", "00:04.000", "claude-sonnet-4-5", "", `[{"type":"text","text":"A2"}]`)
	if err := os.WriteFile(f.path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.ingestRun(context.Background(), f.run)

	if got := f.assistantTexts(t); strings.Join(got, "|") != "A2" {
		t.Fatalf("assistant messages %q", got)
	}
	var idx int
	if err := f.st.DB().QueryRow(`SELECT turn_index FROM conversation_messages WHERE conversation_id=? AND text='A2'`, f.conv).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 3 {
		t.Fatalf("turn_index %d want 3", idx)
	}
}

// The memory fix came before the model answered the prompt, so the fix turn
// holds the real reply.
func TestIngestKeepsReplyAfterEarlyMemoryFix(t *testing.T) {
	f := newIngestFixture(t, "sess-e")
	ctx := context.Background()
	head := ingestLine("user", "00:01.000", "", "task-notification", `"Task finished"`) +
		ingestLine("user", "00:02.000", "", "", `"What about the credits?"`) +
		ingestLine("assistant", "00:03.000", "<synthetic>", "", `[{"type":"text","text":"No response requested."}]`) +
		ingestLine("user", "00:04.000", "", "", ingestMemoryFix)
	tail := ingestLine("assistant", "00:05.000", "claude-sonnet-4-5", "", `[{"type":"tool_use","name":"Edit","input":{}}]`) +
		ingestLine("user", "00:06.000", "", "", `[{"type":"tool_result","content":"ok"}]`) +
		ingestLine("assistant", "00:07.000", "claude-sonnet-4-5", "", `[{"type":"text","text":"You are right to ask...\n\nTREBI_RUN_COMPLETE"}]`) +
		ingestLine("assistant", "00:08.000", "claude-sonnet-4-5", "", `[{"type":"text","text":"I updated MEMORY.md."}]`)
	if err := os.WriteFile(f.path, []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.ingestRun(ctx, f.run)
	if err := os.WriteFile(f.path, []byte(head+tail), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.ingestRun(ctx, f.run)

	if got := f.assistantTexts(t); strings.Join(got, "|") != "You are right to ask...|I updated MEMORY.md." {
		t.Fatalf("assistant messages %q", got)
	}
}

// A tool turn answers the prompt, so a later memory fix is housekeeping.
func TestIngestHidesFixAfterToolTurn(t *testing.T) {
	f := newIngestFixture(t, "sess-t")
	lines := ingestLine("user", "00:01.000", "", "", `"Log it"`) +
		ingestLine("assistant", "00:02.000", "claude-sonnet-4-5", "", `[{"type":"tool_use","name":"Edit","input":{}}]`) +
		ingestLine("user", "00:03.000", "", "", `[{"type":"tool_result","content":"ok"}]`) +
		ingestLine("user", "00:04.000", "", "", ingestMemoryFix) +
		ingestLine("assistant", "00:05.000", "claude-sonnet-4-5", "", `[{"type":"text","text":"Log entry added."}]`)
	if err := os.WriteFile(f.path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.ingestRun(context.Background(), f.run)

	if got := f.assistantTexts(t); len(got) != 0 {
		t.Fatalf("assistant messages %q", got)
	}
}

// TestWatchFollowsMovedJobFolder: a job folder moved by hand names no
// definition file in its events, so the watcher rebuilds the workspace.
func TestWatchFollowsMovedJobFolder(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	wsPath := filepath.Join(dir, "ws")
	files := map[string]string{
		"agents/a/AGENT.md": "---\ndescription: A.\n---\n\nA.\n",
		"agents/b/AGENT.md": "---\ndescription: B.\n---\n\nB.\n",
		"agents/a/x/JOB.md": "---\nharness: claude\n---\n\nX.\n",
	}
	for rel, body := range files {
		p := filepath.Join(wsPath, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cp := New(st, Options{Shadow: true})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "watch-move")
	if err != nil {
		t.Fatal(err)
	}
	before, err := cp.Workspaces.JobByName(ctx, ws.ID, "agents/a/x")
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	// The poll rebuild is 30s, so a change seen inside 10s comes from the
	// watcher. Edit the job until the index sees it, so the watcher is armed.
	job := filepath.Join(wsPath, "agents/a/x/JOB.md")
	armed := false
	for i := 0; i < 20 && !armed; i++ {
		if err := os.WriteFile(job, fmt.Appendf(nil, "---\nharness: claude\n---\n\nX %d.\n", i), 0o644); err != nil {
			t.Fatal(err)
		}
		for range 10 {
			if row, err := cp.Workspaces.JobByName(ctx, ws.ID, "agents/a/x"); err == nil && row.Hash != before.Hash {
				armed = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !armed {
		t.Fatal("watcher did not arm")
	}
	if err := os.Rename(filepath.Join(wsPath, "agents/a/x"), filepath.Join(wsPath, "agents/b/x")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if row, err := cp.Workspaces.JobByName(ctx, ws.ID, "agents/b/x"); err == nil {
			if row.ID != before.ID {
				t.Fatalf("id %s, want %s", row.ID, before.ID)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("index did not follow the moved folder")
}

// TestAgentArchiveEditRecomputesOwnedJobs: an edit of AGENT.md that adds
// archived_at marks the jobs of the agent as archived, and a second edit
// that removes it restores them.
func TestAgentArchiveEditRecomputesOwnedJobs(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	wsPath := filepath.Join(dir, "ws")
	files := map[string]string{
		"agents/a/AGENT.md": "---\ndescription: A.\n---\n\nA.\n",
		"agents/a/x/JOB.md": "---\nharness: claude\n---\n\nX.\n",
	}
	for rel, body := range files {
		p := filepath.Join(wsPath, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cp := New(st, Options{Shadow: true})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "watch-archive")
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	agent := filepath.Join(wsPath, "agents/a/AGENT.md")
	waitArchived := func(want bool, body func(i int) string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for i := 0; time.Now().Before(deadline); i++ {
			// Rewrite until the watcher is armed and sees the change.
			if err := os.WriteFile(agent, []byte(body(i)), 0o644); err != nil {
				t.Fatal(err)
			}
			for range 10 {
				if row, err := cp.Workspaces.JobByName(ctx, ws.ID, "agents/a/x"); err == nil && row.Archived() == want {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		t.Fatalf("job archived state did not become %v", want)
	}
	waitArchived(true, func(i int) string {
		return fmt.Sprintf("---\ndescription: A %d.\narchived_at: 2026-11-01T10:00:00Z\n---\n\nA.\n", i)
	})
	waitArchived(false, func(i int) string {
		return fmt.Sprintf("---\ndescription: A %d.\n---\n\nA.\n", i)
	})
}

// TestWatchSkipsRunPausedByRestart: a run that a restart parked sends no
// partial reply. The resume after boot ends the exchange. The plane has no
// services, so any work past the check panics.
func TestWatchSkipsRunPausedByRestart(t *testing.T) {
	c := &ControlPlane{}
	c.replyIfExchangeEnded(context.Background(), &runs.Run{
		ID: "r1", ConversationID: "c1", Status: runs.StatusParked, EndReason: runs.EndRestart,
	})
}

// TestIngestRunSendsLiveBlocks: each text block of a running turn becomes its
// own outbox row at ingest, before the exchange ends, and the end sweep adds
// none. After a restart that lost the live call, the sweep adds the rows.
func TestIngestRunSendsLiveBlocks(t *testing.T) {
	f := newIngestFixture(t, "sess-live")
	ctx := context.Background()
	chat, err := f.c.Connectors.Create(ctx, connectors.Connection{Type: "chat", Slug: "chat", From: connectors.FromInline, Manifest: &manifest.Manifest{
		Schema: manifest.SchemaID, Name: "chat", Title: "Chat", Description: "Test chat", Version: "1.0.0",
		Events: &manifest.Events{
			Protocol: manifest.ProtocolConnector, Command: "sleep 60",
			Types: []manifest.EventType{{Type: "message"}},
		},
		Channel: &manifest.Channel{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.c.Conversations.AppendUser(ctx, f.conv, conversations.UserTurn{
		Text: "status?", Address: conversations.Address{Connector: "chat", Instance: chat.ID, Room: "u1"},
	}); err != nil {
		t.Fatal(err)
	}
	line := func(typ, at, content string) string {
		return `{"type":"` + typ + `","timestamp":"2026-11-09T10:` + at + `Z","message":{"role":"` + typ + `","content":` + content + "}}\n"
	}
	transcript := line("user", "00:00.000", `"status?"`) +
		line("assistant", "00:01.000", `[{"type":"text","text":"_Checking…_"}]`) +
		line("assistant", "00:02.000", `[{"type":"tool_use","name":"Bash","input":{}}]`) +
		line("user", "00:03.000", `[{"type":"tool_result","content":"ok"}]`) +
		line("assistant", "00:04.000", `[{"type":"text","text":"All good."}]`)
	if err := os.WriteFile(f.path, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	bodies := func() []string {
		rows, err := f.st.DB().Query(`SELECT body FROM outbox WHERE kind='channel' AND conversation_id=? ORDER BY id`, f.conv)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var b string
			if err := rows.Scan(&b); err != nil {
				t.Fatal(err)
			}
			got = append(got, b)
		}
		return got
	}

	// Restart case: the rows are stored, but the live call is lost.
	live := f.c.Channels
	f.c.Channels = nil
	f.c.ingestRun(ctx, f.run)
	f.c.Channels = live
	if got := bodies(); len(got) != 0 {
		t.Fatalf("rows with no channels = %q", got)
	}
	if err := f.c.Channels.SendReplies(ctx, f.conv, f.run.ID); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(bodies(), "|"); got != "_Checking…_|All good." {
		t.Fatalf("after sweep %q", got)
	}

	// Live case: a new block on the next tick goes out at once.
	more := transcript + line("user", "01:00.000", `"and now?"`) +
		line("assistant", "01:01.000", `[{"type":"text","text":"Still good."}]`)
	if _, _, err := f.c.Conversations.AppendUser(ctx, f.conv, conversations.UserTurn{
		Text: "and now?", Address: conversations.Address{Connector: "chat", Instance: chat.ID, Room: "u1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, []byte(more), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.ingestRun(ctx, f.run)
	if got := strings.Join(bodies(), "|"); got != "_Checking…_|All good.|Still good." {
		t.Fatalf("live rows %q", got)
	}
	if err := f.c.Channels.SendReplies(ctx, f.conv, f.run.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(bodies()); n != 3 {
		t.Fatalf("sweep added rows: %d", n)
	}
}

// ingestLine builds one claude transcript line. model and origin may be empty.
func ingestLine(typ, at, model, origin, content string) string {
	msg := `"role":"` + typ + `"`
	if model != "" {
		msg = `"model":"` + model + `",` + msg
	}
	top := ""
	if origin != "" {
		top = `"origin":{"kind":"` + origin + `"},`
	}
	return `{"type":"` + typ + `",` + top + `"timestamp":"2026-10-08T10:` + at + `Z","message":{` + msg + `,"content":` + content + "}}\n"
}

const ingestMemoryFix = `"MEMORY.md: unchanged this run. Add a Log entry for this run.\nFix MEMORY.md. Trebi checks the file again when you finish. Then finish the run."`
