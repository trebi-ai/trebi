package tmux

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
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

func TestParseTranscriptUnsupported(t *testing.T) {
	t.Parallel()
	_, err := codexTool{}.ParseTranscript(bytes.NewReader(nil))
	if !errors.Is(err, ErrTranscriptUnsupported) {
		t.Fatalf("codex %v", err)
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
