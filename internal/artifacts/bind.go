package artifacts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const (
	bindTimeout = 5 * time.Second
	maxBound    = 1 << 20
)

var errBindLimit = errors.New("the template output is larger than 1 MiB")

// BindMarkdown runs a live Markdown template on the data of a snapshot
// (E4). A template error becomes a status block with the error text; the
// output up to the error stays.
func BindMarkdown(tmpl []byte, data map[string]any) []byte {
	out, err := bind(tmpl, data)
	if err != nil {
		var b bytes.Buffer
		b.Write(out)
		fmt.Fprintf(&b, "\n\n```status\nData: error, %s\n```\n", strings.ReplaceAll(err.Error(), "\n", " "))
		return b.Bytes()
	}
	return out
}

// boundWriter stops a template at the size cap or the deadline.
type boundWriter struct {
	buf      bytes.Buffer
	deadline time.Time
}

func (w *boundWriter) Write(p []byte) (int, error) {
	if time.Now().After(w.deadline) {
		return 0, errors.New("the template took longer than 5s")
	}
	if w.buf.Len()+len(p) > maxBound {
		return 0, errBindLimit
	}
	return w.buf.Write(p)
}

// BindData is the template data of a snapshot: one entry per source with
// columns, rows (records), json, text, and error, plus params.
func BindData(data map[string]SourceData, inputs map[string]string) map[string]any {
	out := make(map[string]any, len(data)+1)
	for name, d := range data {
		src := map[string]any{"columns": []string{}, "rows": []map[string]any{}, "text": d.Text, "error": d.Error}
		if d.Rows != nil {
			rows := make([]map[string]any, 0, len(d.Rows.Values))
			for _, vals := range d.Rows.Values {
				rec := make(map[string]any, len(d.Rows.Columns))
				for i, c := range d.Rows.Columns {
					if i < len(vals) {
						rec[c] = vals[i]
					}
				}
				rows = append(rows, rec)
			}
			src["columns"], src["rows"] = d.Rows.Columns, rows
		}
		if len(d.JSON) > 0 {
			var v any
			if json.Unmarshal(d.JSON, &v) == nil {
				src["json"] = v
			}
		}
		out[name] = src
	}
	params := make(map[string]any, len(inputs))
	for k, v := range inputs {
		params[k] = v
	}
	out["params"] = params
	return out
}

func bind(tmpl []byte, data map[string]any) ([]byte, error) {
	t, err := template.New("live").Option("missingkey=zero").Funcs(bindFuncs()).Parse(string(tmpl))
	if err != nil {
		return nil, err
	}
	w := &boundWriter{deadline: time.Now().Add(bindTimeout)}
	err = t.Execute(w, data)
	return bytes.ReplaceAll(w.buf.Bytes(), []byte("<no value>"), nil), err // a missing key prints nothing
}

// bindFuncs is the whole function set. call and html are replaced, so a
// template cannot call a function value or write raw HTML through them.
func bindFuncs() template.FuncMap {
	blocked := func(name string) func(...any) (string, error) {
		return func(...any) (string, error) { return "", fmt.Errorf("%s is not available", name) }
	}
	return template.FuncMap{
		"call": blocked("call"), "html": blocked("html"), "js": blocked("js"),
		"num": func(v any) string { return group(toFloat(v), -1) },
		"money": func(v any, symbol ...string) string {
			s := "$"
			if len(symbol) > 0 {
				s = symbol[0]
			}
			f := toFloat(v)
			if f < 0 {
				return "-" + s + group(-f, 2)
			}
			return s + group(f, 2)
		},
		"pct": func(v any) string { return strconv.FormatFloat(toFloat(v)*100, 'f', 1, 64) + "%" },
		"delta": func(cur, prev any) string {
			p := toFloat(prev)
			if p == 0 {
				return ""
			}
			d := (toFloat(cur) - p) / math.Abs(p) * 100
			sign := "+"
			if d < 0 {
				sign = "-"
			}
			return sign + strconv.FormatFloat(math.Abs(d), 'f', 1, 64) + "%"
		},
		"date":  bindDate,
		"first": func(rows []map[string]any) map[string]any { return pick(rows, 0) },
		"last":  func(rows []map[string]any) map[string]any { return pick(rows, len(rows)-1) },
		"sum": func(rows []map[string]any, col string) float64 {
			var t float64
			for _, r := range rows {
				t += toFloat(r[col])
			}
			return t
		},
		"table": bindTable,
		"kpi":   bindKPI,
	}
}

func pick(rows []map[string]any, i int) map[string]any {
	if i < 0 || i >= len(rows) {
		return map[string]any{}
	}
	return rows[i]
}

func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(t), ",", ""), 64) // text that is not a number is 0
		return f
	}
	return 0
}

// group writes f with thousands separators. prec -1 is the shortest form,
// with at most two decimals.
func group(f float64, prec int) string {
	auto := prec < 0
	if auto {
		prec = 2
	}
	s := strconv.FormatFloat(f, 'f', prec, 64)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if auto {
		frac = strings.TrimRight(frac, "0")
	}
	if frac != "" {
		return sign + b.String() + "." + frac
	}
	return sign + b.String()
}

// bindDate formats an RFC 3339 time or a date. The layout is a Go layout,
// default "Jan 2, 2006".
func bindDate(v any, layout ...string) string {
	s := fmt.Sprint(v)
	l := "Jan 2, 2006"
	if len(layout) > 0 {
		l = layout[0]
	}
	for _, in := range []string{time.RFC3339Nano, time.DateTime, time.DateOnly} {
		if t, err := time.Parse(in, s); err == nil {
			return t.Format(l)
		}
	}
	return s
}

// bindTable writes the rows of a source as a GFM table, at most limit rows
// (default 50).
func bindTable(src map[string]any, limit ...int) string {
	cols, _ := src["columns"].([]string)
	rows, _ := src["rows"].([]map[string]any)
	if len(cols) == 0 {
		return ""
	}
	n := 50
	if len(limit) > 0 && limit[0] > 0 {
		n = limit[0]
	}
	cell := func(v any) string {
		return strings.NewReplacer("|", `\|`, "\n", " ").Replace(fmt.Sprint(v))
	}
	var b strings.Builder
	b.WriteString("| " + strings.Join(cols, " | ") + " |\n|")
	b.WriteString(strings.Repeat(" --- |", len(cols)) + "\n")
	for _, r := range rows[:min(n, len(rows))] {
		b.WriteString("|")
		for _, c := range cols {
			v := r[c]
			if v == nil {
				v = ""
			}
			b.WriteString(" " + cell(v) + " |")
		}
		b.WriteString("\n")
	}
	if len(rows) > n {
		fmt.Fprintf(&b, "\n%d more rows are not shown.\n", len(rows)-n)
	}
	return b.String()
}

// bindKPI writes one kpi block from label, value, change triples. An empty
// change shows no change.
func bindKPI(args ...any) (string, error) {
	if len(args) == 0 || len(args)%3 != 0 {
		return "", errors.New("kpi takes label, value, change triples")
	}
	clean := strings.NewReplacer("\n", " ", ", ", " ").Replace
	var b strings.Builder
	b.WriteString("```kpi\n")
	for i := 0; i < len(args); i += 3 {
		line := clean(fmt.Sprint(args[i])) + ": " + clean(fmt.Sprint(args[i+1]))
		if c := clean(fmt.Sprint(args[i+2])); c != "" {
			line += ", " + c
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("```\n")
	return b.String(), nil
}
