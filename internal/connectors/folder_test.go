package connectors

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAppendLogRows proves each row is valid JSON for control bytes and
// bad UTF-8, and a long line is cut.
func TestAppendLogRows(t *testing.T) {
	t.Parallel()
	f := Folder{dir: t.TempDir()}
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	lines := []string{"\x1b[31mred\x1b[0m", "bell \a", "bad \xff", strings.Repeat("x", logLineCap+10)}
	for _, l := range lines {
		if err := f.AppendLog(at, "stderr", l); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(f.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(rows) != len(lines) {
		t.Fatalf("rows = %d", len(rows))
	}
	for i, raw := range rows {
		var row logRow
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			t.Fatalf("row %d: %v: %s", i, err, raw)
		}
		if row.Stream != "stderr" || row.T != "2026-10-17T12:00:00Z" {
			t.Fatalf("row %d = %+v", i, row)
		}
	}
	var first, last logRow
	_ = json.Unmarshal([]byte(rows[0]), &first) //nolint:errcheck // checked above
	_ = json.Unmarshal([]byte(rows[3]), &last)  //nolint:errcheck // checked above
	if first.Line != lines[0] || len(last.Line) != logLineCap+len(" [cut]") {
		t.Fatalf("first %q, last length %d", first.Line, len(last.Line))
	}
}

// TestAppendLogRotates proves the log moves to one old file at logCap.
func TestAppendLogRotates(t *testing.T) {
	t.Parallel()
	f := Folder{dir: t.TempDir()}
	if err := f.AppendLog(time.Now(), "stdout", "old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(f.LogPath(), logCap); err != nil {
		t.Fatal(err)
	}
	if err := f.AppendLog(time.Now(), "stdout", "new"); err != nil {
		t.Fatal(err)
	}
	cur, err := os.ReadFile(f.LogPath())
	if err != nil || !strings.Contains(string(cur), `"line":"new"`) || len(cur) > 200 {
		t.Fatalf("current log %d bytes, %v", len(cur), err)
	}
	if st, err := os.Stat(f.LogPath() + ".1"); err != nil || st.Size() != logCap {
		t.Fatalf("old log: %v", err)
	}
}

// TestLogReader proves Tail gives the last lines, Next gives only whole new
// lines, and Next follows a rotation without a lost line.
func TestLogReader(t *testing.T) {
	t.Parallel()
	f := Folder{dir: t.TempDir()}
	lr := f.OpenLog()
	defer lr.Close()
	if got, err := lr.Next(); err != nil || got != nil {
		t.Fatalf("no log: %v %v", got, err)
	}
	lr.Close()
	if err := os.MkdirAll(f.Dir()+"/logs", 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := range 5 {
		b.WriteString("l" + strconv.Itoa(i) + "\n")
	}
	b.WriteString("part")
	if err := os.WriteFile(f.LogPath(), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := lr.Tail(2)
	if err != nil || !slices.Equal(got, []string{"l3", "l4"}) {
		t.Fatalf("tail = %v, %v", got, err)
	}
	add := func(path, s string) {
		t.Helper()
		af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = af.WriteString(s) //nolint:errcheck // the read below checks it
		_ = af.Close()
	}
	add(f.LogPath(), "ial\nl6")
	if got, err := lr.Next(); err != nil || !slices.Equal(got, []string{"partial"}) {
		t.Fatalf("next = %v, %v", got, err)
	}
	add(f.LogPath(), "\n")
	if err := os.Rename(f.LogPath(), f.LogPath()+".1"); err != nil {
		t.Fatal(err)
	}
	add(f.LogPath(), "l7\n")
	if got, err := lr.Next(); err != nil || !slices.Equal(got, []string{"l6", "l7"}) {
		t.Fatalf("next after rotation = %v, %v", got, err)
	}
}
