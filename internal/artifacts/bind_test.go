package artifacts

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
)

func TestBindMarkdown(t *testing.T) {
	t.Parallel()
	data := BindData(map[string]SourceData{
		"sales": {Rows: &connectors.Rows{Columns: []string{"day", "total"}, Values: [][]string{{"2026-10-05", "1234567.5"}, {"2026-10-06", "-20"}}}},
		"bad":   {Error: "timeout"},
	}, map[string]string{"period": "7d"})
	cases := []struct{ tmpl, want string }{
		{`{{ num 1234567.50 }}`, "1,234,567.5"},
		{`{{ money -1234.5 }}`, "-$1,234.50"},
		{`{{ money 3 "€" }}`, "€3.00"},
		{`{{ pct 0.123 }}`, "12.3%"},
		{`{{ delta 110 100 }} {{ delta 90 100 }} [{{ delta 1 0 }}]`, "+10.0% -10.0% []"},
		{`{{ date "2026-10-05" }} {{ date "2026-10-05T10:00:00Z" "Jan 2" }}`, "Oct 5, 2026 Oct 5"},
		{`{{ (first .sales.rows).day }} {{ (last .sales.rows).total }}`, "2026-10-05 -20"},
		{`{{ num (sum .sales.rows "total") }}`, "1,234,547.5"},
		{`{{ .params.period }} {{ .bad.error }} [{{ .missing }}]`, "7d timeout []"},
		{`{{ table .sales 1 }}`, "| day | total |\n| --- | --- |\n| 2026-10-05 | 1234567.5 |\n\n1 more rows are not shown.\n"},
		{`{{ kpi "Revenue" "$1.2M" "+4%" "Orders" 12 "" }}`, "```kpi\nRevenue: $1.2M, +4%\nOrders: 12\n```\n"},
	}
	for _, c := range cases {
		if got := string(BindMarkdown([]byte(c.tmpl), data)); got != c.want {
			t.Errorf("%s\n got %q\nwant %q", c.tmpl, got, c.want)
		}
	}
}

func TestBindLimits(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{`{{ call .x }}`, `{{ html "<b>" }}`, `{{ js "x" }}`, `{{ kpi "a" }}`, `{{ .x`} {
		got := string(BindMarkdown([]byte(tmpl), map[string]any{"x": func() string { return "ran" }}))
		if !strings.Contains(got, "```status\nData: error,") || strings.Contains(got, "ran") {
			t.Errorf("%s: %q", tmpl, got)
		}
	}
	big := `{{ range .rows }}` + strings.Repeat("x", 1024) + `{{ end }}`
	rows := make([]int, 2048)
	got := BindMarkdown([]byte(big), map[string]any{"rows": rows})
	if len(got) > maxBound+200 || !strings.Contains(string(got), "larger than 1 MiB") {
		t.Fatalf("cap: %d bytes", len(got))
	}
}
