package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const memorySample = `Prefers invoices as PDF. Send before the 5th.
[2026-08-08] Sent Q3 invoice reminder to Acme; awaiting reply.
[2026-08-08] Acme billing contact changed to billing@acme.com.
    Continuation lines belong to the entry above.
[2026-08-09] Third entry.
`

func TestParseMemoryRoundTrip(t *testing.T) {
	t.Parallel()
	preamble, entries := ParseMemory([]byte(memorySample))
	if preamble != "Prefers invoices as PDF. Send before the 5th." {
		t.Fatalf("preamble = %q", preamble)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if entries[0].Text != "Sent Q3 invoice reminder to Acme; awaiting reply." {
		t.Errorf("entry 0 text = %q", entries[0].Text)
	}
	want := "Acme billing contact changed to billing@acme.com.\n    Continuation lines belong to the entry above."
	if entries[1].Text != want {
		t.Errorf("entry 1 text = %q", entries[1].Text)
	}
	if entries[2].Date != "2026-08-09" {
		t.Errorf("entry 2 date = %q", entries[2].Date)
	}
	got := string(serializeMemory(preamble, entries))
	if got != memorySample {
		t.Errorf("round trip mismatch:\n got: %q\nwant: %q", got, memorySample)
	}
}

func TestParseMemoryIDsStable(t *testing.T) {
	t.Parallel()
	_, a := ParseMemory([]byte(memorySample))
	_, b := ParseMemory([]byte(memorySample))
	for i := range a {
		if a[i].ID == "" || a[i].ID != b[i].ID {
			t.Errorf("entry %d id unstable: %q vs %q", i, a[i].ID, b[i].ID)
		}
	}
	if a[0].ID == a[1].ID {
		t.Errorf("distinct entries share id %q", a[0].ID)
	}
}

// The entry regex is anchored and shape-only: any [\d{4}-\d{2}-\d{2}]
// prefix starts an entry, calendar validity is not checked.
func TestParseMemoryDateShape(t *testing.T) {
	content := "[2026-13-99] bad calendar date still parses\n  [2026-01-01] indented is continuation\n"
	preamble, entries := ParseMemory([]byte(content))
	if preamble != "" {
		t.Errorf("preamble = %q, want empty", preamble)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Date != "2026-13-99" {
		t.Errorf("date = %q", entries[0].Date)
	}
	if !strings.Contains(entries[0].Text, "indented is continuation") {
		t.Errorf("continuation not joined: %q", entries[0].Text)
	}
}

func TestAppendMemoryEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(dir, "missing.md")
		e, err := AppendMemoryEntry(path, "2026-08-09", "first entry")
		if err != nil {
			t.Fatal(err)
		}
		if e.ID == "" || e.Date != "2026-08-09" {
			t.Errorf("entry = %+v", e)
		}
		data, _ := os.ReadFile(path)
		if string(data) != "[2026-08-09] first entry\n" {
			t.Errorf("content = %q", string(data))
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(dir, "empty.md")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := AppendMemoryEntry(path, "2026-08-09", "entry"); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if string(data) != "[2026-08-09] entry\n" {
			t.Errorf("content = %q", string(data))
		}
	})

	t.Run("no trailing newline", func(t *testing.T) {
		path := filepath.Join(dir, "notrail.md")
		if err := os.WriteFile(path, []byte("preamble text"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := AppendMemoryEntry(path, "2026-08-09", "entry"); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if string(data) != "preamble text\n[2026-08-09] entry\n" {
			t.Errorf("content = %q", string(data))
		}
	})

	t.Run("multi-line text", func(t *testing.T) {
		path := filepath.Join(dir, "multi.md")
		e, err := AppendMemoryEntry(path, "2026-08-09", "first line\n  second line")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if string(data) != "[2026-08-09] first line\n  second line\n" {
			t.Errorf("content = %q", string(data))
		}
		_, entries := ParseMemory(data)
		if len(entries) != 1 || entries[0].ID != e.ID {
			t.Errorf("re-parse id mismatch: %+v vs %+v", entries, e)
		}
	})

	t.Run("default date", func(t *testing.T) {
		path := filepath.Join(dir, "today.md")
		e, err := AppendMemoryEntry(path, "", "entry")
		if err != nil {
			t.Fatal(err)
		}
		if e.Date != time.Now().UTC().Format("2006-01-02") {
			t.Errorf("date = %q", e.Date)
		}
	})

	t.Run("invalid date", func(t *testing.T) {
		path := filepath.Join(dir, "bad.md")
		if _, err := AppendMemoryEntry(path, "aug 9", "entry"); err == nil {
			t.Error("want error for invalid date")
		}
	})
}

func TestRemoveMemoryEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(path, []byte(memorySample), 0o644); err != nil {
		t.Fatal(err)
	}
	_, entries := ParseMemory([]byte(memorySample))

	if err := RemoveMemoryEntry(path, "nope"); err == nil {
		t.Error("want error for unknown id")
	}

	if err := RemoveMemoryEntry(path, entries[1].ID); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := "Prefers invoices as PDF. Send before the 5th.\n" +
		"[2026-08-08] Sent Q3 invoice reminder to Acme; awaiting reply.\n" +
		"[2026-08-09] Third entry.\n"
	if string(data) != want {
		t.Errorf("content:\n got: %q\nwant: %q", string(data), want)
	}
}

func TestLoadMemoryInfo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	info, err := LoadMemoryInfo(filepath.Join(dir, "absent.md"))
	if err != nil || info != nil {
		t.Fatalf("missing file: info=%v err=%v, want nil, nil", info, err)
	}

	path := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(path, []byte(memorySample), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = LoadMemoryInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.SizeBytes != len(memorySample) {
		t.Errorf("size = %d, want %d", info.SizeBytes, len(memorySample))
	}
	if info.Hash != ContentHash([]byte(memorySample)) {
		t.Errorf("hash = %q", info.Hash)
	}
	if info.Content != memorySample {
		t.Errorf("content = %q", info.Content)
	}
	if len(info.Entries) != 3 || info.Preamble == "" {
		t.Errorf("entries = %d, preamble = %q", len(info.Entries), info.Preamble)
	}
	if info.UpdatedAt.IsZero() {
		t.Error("updated_at is zero")
	}
}

func TestAppendMemoryEntryRejectsEntryLikeContinuation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "MEMORY.md")

	// A continuation line that parses as a new entry must be rejected.
	if _, err := AppendMemoryEntry(path, "2026-08-09", "first line\n[2026-08-10] phantom"); err == nil {
		t.Fatal("want error for entry-like continuation line")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("rejected append must not create the file")
	}

	// A normal multi-line entry still appends and round-trips as one entry.
	e, err := AppendMemoryEntry(path, "2026-08-09", "first line\n  indented continuation")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, entries := ParseMemory(data)
	if len(entries) != 1 || entries[0].ID != e.ID {
		t.Fatalf("entries = %v, want one entry with id %s", entries, e.ID)
	}
}
