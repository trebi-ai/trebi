package cli

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

func TestNaturalColumnWidths(t *testing.T) {
	t.Parallel()
	headers := []string{"ID", "Job", "Status", "Model"}
	rows := []table.Row{
		{"01KYHX4SKDKPAKNSSTZYZMND03", "agents/sling-cmo/jobs/seo/hacker-news-monitor", "complete", "glm"},
		{"01KYHVPYDNNSV9HAABHZ2DT564", "sling-cmo", "skipped", "opus"},
	}
	nat := naturalColumnWidths(headers, rows)
	if nat[0] != 26 {
		t.Fatalf("id natural=%d want 26", nat[0])
	}
	if nat[2] != len("complete") {
		t.Fatalf("status natural=%d", nat[2])
	}
	if nat[3] != len("MODEL") {
		t.Fatalf("model natural=%d want %d", nat[3], len("MODEL"))
	}
	if nat[1] < len("agents/sling-cmo/jobs/seo/hacker-news-monitor") {
		t.Fatalf("job natural=%d", nat[1])
	}
}

func TestFitStaysMinimalWhenFits(t *testing.T) {
	t.Parallel()
	headers := []string{"ID", "Job", "Workspace", "Status", "Model", "Tokens", "Billed", "Conversation"}
	// Naturals already include header floors (as naturalColumnWidths would).
	natural := []int{26, 45, 9, 8, 5, 6, 6, 12}
	maxes := fitColumnWidths(headers, natural, 200)
	// Wide terminal: every column stays at content minimum — no stretch.
	for i := range natural {
		if maxes[i] != natural[i] {
			t.Fatalf("col %d (%s): got %d want natural %d (maxes=%v)",
				i, headers[i], maxes[i], natural[i], maxes)
		}
	}
}

func TestFitShrinksWrappableFirst(t *testing.T) {
	t.Parallel()
	headers := []string{"Status", "Job", "Model"}
	natural := []int{8, 80, 5} // model header MODEL=5
	term := 2 + 2 + 2*3 + 8 + 20 + 5
	maxes := fitColumnWidths(headers, natural, term)
	if maxes[0] != 8 {
		t.Fatalf("status should stay natural, got %d", maxes[0])
	}
	if maxes[2] != 5 {
		t.Fatalf("model should stay natural, got %d", maxes[2])
	}
	if maxes[1] >= 80 {
		t.Fatalf("job should shrink, got %d", maxes[1])
	}
}

func TestFitProtectsIDOverJob(t *testing.T) {
	t.Parallel()
	headers := []string{"ID", "Job", "Status"}
	natural := []int{26, 60, 8}
	// Tight: only room for id + status + short job.
	term := 2 + 2 + 2*3 + 26 + 15 + 8
	maxes := fitColumnWidths(headers, natural, term)
	if maxes[0] != 26 {
		t.Fatalf("id should be protected, got %d (maxes=%v)", maxes[0], maxes)
	}
	if maxes[1] >= 60 {
		t.Fatalf("job should shrink, got %d", maxes[1])
	}
}

func TestTerminalWidthCOLUMNS(t *testing.T) {
	t.Setenv("COLUMNS", "88")
	if w := terminalWidth(&bytes.Buffer{}); w != 88 {
		t.Fatalf("COLUMNS fallback: got %d", w)
	}
	t.Setenv("COLUMNS", "")
	if w := terminalWidth(&bytes.Buffer{}); w != 0 {
		t.Fatalf("no size: got %d", w)
	}
}

func TestHistoryLikeTableMinimalNoStretch(t *testing.T) {
	const width = 200
	t.Setenv("COLUMNS", strconv.Itoa(width))

	var buf bytes.Buffer
	tbl := newTable(&buf, []string{"ID", "Job", "Workspace", "Status", "Started", "Duration", "Model", "Tokens", "Billed", "Trigger", "Conversation"})
	jobPath := "agents/sling-cmo/jobs/seo/hacker-news-monitor"
	tbl.AppendRow([]any{
		"01KYHX4SKDKPAKNSSTZYZMND03",
		jobPath,
		"sling", "complete", "2026-07-27 10:45:02 -03", "3m08s", "glm", "1.5M", "161.1K", "-", "-",
	})
	tbl.AppendRow([]any{
		"01KYHVPYDNNSV9HAABHZ2DT564",
		"sling-cmo",
		"sling", "skipped", "2026-07-27 10:40:01 -03", "<1s", "-", "-", "-", "-", "-",
	})
	renderTable(tbl)
	out := buf.String()
	t.Logf("\n%s", out)

	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		w := text.StringWidthWithoutEscSequences(text.StripEscape(line))
		if w > width {
			t.Fatalf("line %d width %d > %d", i, w, width)
		}
	}

	// Wide terminal + content fits → one-line job path.
	if !strings.Contains(out, jobPath) {
		t.Fatalf("job path should fit on one line:\n%s", out)
	}
	if !strings.Contains(out, "01KYHX4SKDKPAKNSSTZYZMND03") {
		t.Fatalf("ULID wrapped or truncated:\n%s", out)
	}
	// Table must stay content-tight — not stretch to the full terminal (199).
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("empty")
	}
	topW := text.StringWidthWithoutEscSequences(lines[0])
	if topW >= width-2 {
		t.Fatalf("table stretched to terminal: width %d (COLUMNS=%d):\n%s", topW, width, out)
	}
	// Workspace stays content-min (header WORKSPACE=9).
	if strings.Contains(out, "sling          ") {
		t.Fatalf("workspace column bloated:\n%s", out)
	}
}

func TestHistoryLikeTableWrapsWhenNarrow(t *testing.T) {
	const width = 80
	t.Setenv("COLUMNS", strconv.Itoa(width))

	var buf bytes.Buffer
	tbl := newTable(&buf, []string{"ID", "Job", "Status", "Model"})
	tbl.AppendRow([]any{
		"01KYHX4SKDKPAKNSSTZYZMND03",
		"agents/sling-cmo/jobs/seo/hacker-news-monitor",
		"complete", "glm",
	})
	renderTable(tbl)
	out := buf.String()
	t.Logf("\n%s", out)

	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		w := text.StringWidthWithoutEscSequences(text.StripEscape(line))
		if w > width {
			t.Fatalf("line %d width %d > %d:\n%s", i, w, width, line)
		}
	}
	// Job is wrappable; ID should stay intact when possible.
	if !strings.Contains(out, "01KYHX4SKDKPAKNSSTZYZMND03") {
		t.Fatalf("ID should be protected:\n%s", out)
	}
}

func TestTableNoWidthLimitWhenUnknown(t *testing.T) {
	t.Setenv("COLUMNS", "")
	var buf bytes.Buffer
	tbl := newTable(&buf, []string{"A", "B"})
	st := tbl.(*sizedTable)
	st.applyContentFit()
	if st.Style().Size.WidthMax != 0 || st.Style().Size.WidthMin != 0 {
		t.Fatalf("WidthMin/Max=%d/%d want 0", st.Style().Size.WidthMin, st.Style().Size.WidthMax)
	}
}
