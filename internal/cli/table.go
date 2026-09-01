package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"golang.org/x/term"
)

// sizedTable wraps go-pretty so we can size columns from actual cell content
// at render time (headers alone are not enough for good auto-size).
type sizedTable struct {
	table.Writer
	out     io.Writer
	headers []string
	rows    []table.Row
}

// newTable is the shared list table (TTY colors). Columns stay natural unless wider than the terminal.
func newTable(out io.Writer, headers []string) table.Writer {
	inner := table.NewWriter()
	inner.SetOutputMirror(out)

	row := make(table.Row, len(headers))
	for i, h := range headers {
		row[i] = h
	}
	inner.AppendHeader(row)

	inner.SetStyle(table.StyleLight)
	inner.Style().Format.Header = text.FormatUpper
	inner.Style().Options.SeparateRows = false
	inner.Style().Options.SeparateHeader = true
	inner.Style().Options.DrawBorder = true

	if isTTY(out) {
		inner.Style().Color.Header = text.Colors{text.Bold, text.FgWhite}
		inner.Style().Color.Border = text.Colors{text.FgHiBlack}
		inner.Style().Color.Separator = text.Colors{text.FgHiBlack}
		inner.Style().Color.Row = text.Colors{text.Reset}
		inner.Style().Color.RowAlternate = text.Colors{text.Faint}
	}

	return &sizedTable{Writer: inner, out: out, headers: headers}
}

func (s *sizedTable) AppendRow(row table.Row, configs ...table.RowConfig) {
	// Copy so later mutations by callers cannot affect measurement.
	cp := make(table.Row, len(row))
	copy(cp, row)
	s.rows = append(s.rows, cp)
	s.Writer.AppendRow(row, configs...)
}

func (s *sizedTable) AppendRows(rows []table.Row, configs ...table.RowConfig) {
	for _, r := range rows {
		s.AppendRow(r, configs...)
	}
}

func (s *sizedTable) Render() string {
	s.applyContentFit()
	return s.Writer.Render()
}

func (s *sizedTable) applyContentFit() {
	width := terminalWidth(s.out)
	if width <= 0 || len(s.headers) == 0 {
		return
	}
	if width > 2 {
		width--
	}
	// Hard cap only — never force the table to fill the terminal (that
	// stretches Job/path columns with empty padding).
	s.Style().Size.WidthMax = width

	natural := naturalColumnWidths(s.headers, s.rows)
	maxes := fitColumnWidths(s.headers, natural, width)
	if len(maxes) == 0 {
		return
	}
	cfgs := make([]table.ColumnConfig, len(maxes))
	for i, m := range maxes {
		cfg := table.ColumnConfig{
			Number:   i + 1,
			WidthMax: m,
		}
		// Soft-wrap only when we had to shrink below natural content width.
		if m < natural[i] {
			cfg.WidthMaxEnforcer = text.WrapSoft
		}
		cfgs[i] = cfg
	}
	s.SetColumnConfigs(cfgs)
}

// naturalColumnWidths returns per-column content width: max(header, cells).
// No artificial floors — truly minimum for the data.
func naturalColumnWidths(headers []string, rows []table.Row) []int {
	n := len(headers)
	out := make([]int, n)
	for i, h := range headers {
		// Headers render uppercase.
		out[i] = text.StringWidthWithoutEscSequences(strings.ToUpper(h))
	}
	for _, row := range rows {
		for i := 0; i < n && i < len(row); i++ {
			w := cellDisplayWidth(row[i])
			if w > out[i] {
				out[i] = w
			}
		}
	}
	for i := range out {
		if out[i] < 1 {
			out[i] = 1
		}
	}
	return out
}

func cellDisplayWidth(v any) int {
	if v == nil {
		return 1
	}
	s := text.StripEscape(fmt.Sprint(v))
	if s == "" {
		return 1
	}
	return text.LongestLineLen(s)
}

// fitColumnWidths sets WidthMax for termWidth; shrinks only when natural width overflows.
func fitColumnWidths(headers []string, natural []int, termWidth int) []int {
	n := len(headers)
	if n == 0 || termWidth <= 0 || len(natural) != n {
		return nil
	}
	frame := 2 + (n - 1) + 2*n // borders + seps + padding
	budget := termWidth - frame
	if budget < n {
		budget = n
	}

	// Header floor: never shrink a column below its header label.
	floor := make([]int, n)
	wrap := make([]bool, n)
	for i, h := range headers {
		floor[i] = text.StringWidthWithoutEscSequences(strings.ToUpper(h))
		if floor[i] < 1 {
			floor[i] = 1
		}
		wrap[i] = isWrappableColumn(h)
	}

	maxes := make([]int, n)
	copy(maxes, natural)
	for i := range maxes {
		if maxes[i] < floor[i] {
			maxes[i] = floor[i]
		}
	}

	sum := 0
	for _, v := range maxes {
		sum += v
	}
	if sum <= budget {
		// Fits on one line — keep content minimums, no stretch.
		return maxes
	}
	// Over budget: shrink wrappable columns first (job/path/notes), protect
	// id/status/model/tokens when possible.
	overflow := sum - budget
	overflow = shrinkLongestFirst(maxes, floor, wrap, true, overflow)
	if overflow > 0 {
		shrinkLongestFirst(maxes, floor, wrap, false, overflow)
	}
	return maxes
}

// shrinkLongestFirst removes overflow by decrementing the widest eligible column.
// If onlyWrap, only wrappable columns are shrunk.
func shrinkLongestFirst(maxes, floor []int, wrap []bool, onlyWrap bool, overflow int) int {
	for overflow > 0 {
		best := -1
		for i := range maxes {
			if onlyWrap && !wrap[i] {
				continue
			}
			if maxes[i] <= floor[i] {
				continue
			}
			if best < 0 || maxes[i] > maxes[best] {
				best = i
			}
		}
		if best < 0 {
			return overflow
		}
		maxes[best]--
		overflow--
	}
	return 0
}

// isWrappableColumn marks columns that may soft-wrap under a tight budget.
// IDs, status, model, tokens, billed, etc. are protected when possible.
func isWrappableColumn(header string) bool {
	h := strings.ToLower(strings.TrimSpace(header))
	switch h {
	case "job", "name", "path", "conversation", "trigger", "detail", "note",
		"notes", "window", "value", "description", "plan / note", "plan/note",
		"last err", "reason", "message", "error", "started", "when", "duration",
		"integration", "workspace", "harness", "title", "spec", "block",
		"origin", "timezone", "next", "last":
		return true
	}
	if strings.Contains(h, "note") || strings.Contains(h, "detail") ||
		strings.Contains(h, "description") || strings.Contains(h, "message") {
		return true
	}
	return false
}

// terminalWidth returns the width of out when it is a TTY, or COLUMNS, or 0.
func terminalWidth(out io.Writer) int {
	if f, ok := out.(*os.File); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
			return w
		}
	}
	if c := strings.TrimSpace(os.Getenv("COLUMNS")); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// renderTable writes the table to its configured output.
func renderTable(t table.Writer) {
	t.Render()
}

// colorStatus colors a run/queue status when w is a TTY.
func colorStatus(out io.Writer, s string) string {
	if !isTTY(out) {
		return s
	}
	switch s {
	case "running", "resumed", "ok", "pending", "claimed":
		return text.FgGreen.Sprint(s)
	case "complete", "done":
		return text.FgHiGreen.Sprint(s)
	case "failed", "timed_out", "FAIL", "error":
		return text.FgRed.Sprint(s)
	case "exited", "stopped", "blocked":
		return text.FgYellow.Sprint(s)
	case "skipped", "unknown", "debounced", "rate_limited":
		return text.FgHiBlack.Sprint(s)
	case "waiting":
		return text.FgYellow.Sprint(s)
	case "idle":
		return text.FgCyan.Sprint(s)
	case "active", "true":
		return text.FgGreen.Sprint(s)
	case "false":
		return text.FgHiBlack.Sprint(s)
	}
	return s
}

// mapStr reads a string field from a JSON object map.
func mapStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		s := fmt.Sprint(v)
		if s == "" || s == "<nil>" {
			continue
		}
		return s
	}
	return ""
}

// dash empty cells as "-" for table readability.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
