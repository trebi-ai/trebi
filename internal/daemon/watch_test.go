package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
