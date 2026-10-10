package email

import (
	"bytes"
	"cmp"
	"fmt"
	"html/template"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
	"github.com/trebi-ai/trebi/internal/job"
	alert "github.com/yuin/goldmark-alert"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Theme is the house style of a rendered mail. Colors are CSS hex values.
// The dark values apply only in clients that honor prefers-color-scheme.
type Theme struct {
	Accent         string
	Text           string
	Muted          string
	Background     string
	Surface        string
	Border         string
	Code           string
	DarkText       string
	DarkBackground string
	DarkSurface    string
	DarkBorder     string
	Font           string
	Mono           string
	Width          int
}

// DefaultTheme is the Trebi house style. It avoids pure black and pure
// white, so a client that inverts colors keeps the contrast.
func DefaultTheme() Theme {
	return Theme{
		Accent: "#2563eb", Text: "#1f2328", Muted: "#59636e",
		Background: "#f3f4f6", Surface: "#fdfdfd", Border: "#d8dee4", Code: "#f1f3f5",
		DarkText: "#e6e8eb", DarkBackground: "#15171a", DarkSurface: "#1f2226", DarkBorder: "#353a40",
		Font:  "-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,Helvetica,Arial,sans-serif",
		Mono:  "ui-monospace,SFMono-Regular,Menlo,Consolas,Liberation Mono,monospace",
		Width: 600,
	}
}

// With returns t with the accent of st. An empty accent keeps the house one.
func (t Theme) With(st job.StyleYAML) Theme {
	t.Accent = cmp.Or(st.Accent, t.Accent)
	return t
}

// Layout is the frame of one mail. The purpose of the mail selects it.
type Layout string

const (
	LayoutReport  Layout = "report"  // sender header, accent bar, card, footer
	LayoutReply   Layout = "reply"   // card and footer, like a conversation
	LayoutMinimal Layout = "minimal" // one plain column; each line break stays
)

// Page is the frame around one rendered body. A zero Theme is the house style.
type Page struct {
	Layout Layout
	Title  string // the document title; mail clients do not show it
	Brand  string // the sender name in the report header
	Logo   string // https image URL in the report header; it replaces Brand
	Footer string // a small line below the body; "" omits it
	Link   string // an http(s) URL; it adds an "Open in Trebi" button below the body
	// LinkLabel replaces the "Open in Trebi" button text.
	LinkLabel string
	Theme     Theme
}

// alertColors maps a GitHub alert kind to its accent and its tint.
var alertColors = map[string][2]string{ //nolint:gochecknoglobals // static palette, never mutated
	"note":      {"#0969da", "#eef5ff"},
	"tip":       {"#1a7f37", "#eefbf1"},
	"important": {"#8250df", "#f6f0ff"},
	"warning":   {"#9a6700", "#fff8e5"},
	"caution":   {"#d1242f", "#fff0f0"},
}

// Renderer turns agent Markdown into email-safe HTML: inline styles on
// every element, one fixed-width column, and no scripts or raw HTML.
type Renderer struct {
	parser parser.Parser
	html   html.Renderer
	hard   html.Renderer // keeps each line break, for LayoutMinimal
	layout *template.Template
}

func NewRenderer() *Renderer {
	exts := html.WithExtensions(extension.GFMHTMLRenderer, alert.NewHTMLRenderer(alert.WithAlertIcons(map[string]string{})))
	return &Renderer{
		parser: parser.New(parser.WithExtensions(extension.GFMParser, alert.Parser)),
		html:   html.New(exts),
		hard:   html.New(exts, html.WithHardWraps()),
		layout: template.Must(template.New("mail").Parse(layoutHTML)),
	}
}

// ClipBytes is the HTML budget of one mail. Gmail hides the rest of a
// message larger than about 102 KB behind a link.
const ClipBytes = 90 << 10

// FullMailName is the file that holds the whole HTML of a cut mail.
const FullMailName = "full-mail.html"

// Rendered is one HTML mail. Full is set only when HTML is cut to
// ClipBytes; it then holds the whole mail.
type Rendered struct {
	HTML string
	Full string
}

// attach returns the HTML part and the files of a send. A cut mail adds
// Full as a file. With no room for one more file, the mail goes uncut.
func (r Rendered) attach(files []SendAttachment) (string, []SendAttachment) {
	if r.Full == "" {
		return r.HTML, files
	}
	if len(files) >= MaxAttachments {
		return r.Full, files
	}
	return r.HTML, append(slices.Clip(files), SendAttachment{Name: FullMailName, Type: "text/html", Content: []byte(r.Full)})
}

// Markdown renders src as one complete HTML mail. Raw HTML in src is left
// out; links with a dangerous scheme lose their target. A mail larger than
// ClipBytes keeps the top-level blocks that fit, plus a note.
func (r *Renderer) Markdown(src string, p Page) (Rendered, error) {
	if p.Theme.Width == 0 {
		p.Theme = DefaultTheme()
	}
	if p.Layout == "" {
		p.Layout = LayoutReply
	}
	hr := r.html
	if p.Layout == LayoutMinimal {
		hr = r.hard
	}
	source := []byte(src)
	var body bytes.Buffer
	if err := hr.Render(&body, source, r.parser.Parse(source)); err != nil {
		return Rendered{}, fmt.Errorf("render markdown: %w", err)
	}
	ctx := &xhtml.Node{Type: xhtml.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := xhtml.ParseFragment(&body, ctx)
	if err != nil {
		return Rendered{}, fmt.Errorf("parse rendered markdown: %w", err)
	}
	sty := newStyler(p.Theme)
	var pieces []string
	var text strings.Builder
	first := true
	for _, n := range nodes {
		var piece strings.Builder
		if block := sty.block(n); block != nil {
			first = false
			for _, b := range block {
				if err := xhtml.Render(&piece, b); err != nil {
					return Rendered{}, fmt.Errorf("write report block: %w", err)
				}
			}
			pieces = append(pieces, piece.String())
			continue
		}
		sty.style(n, false)
		if first && n.Type == xhtml.ElementNode {
			appendStyle(n, "margin-top:0;") // the first block sits on the card padding
			first = false
		}
		collectText(n, &text)
		if err := xhtml.Render(&piece, n); err != nil {
			return Rendered{}, fmt.Errorf("write rendered markdown: %w", err)
		}
		pieces = append(pieces, piece.String())
	}
	pre := preheader(text.String())
	all := strings.Join(pieces, "")
	full, err := r.page(p, all, pre)
	if err != nil || len(full) <= ClipBytes {
		return Rendered{HTML: full}, err
	}
	return r.clip(p, pieces, len(full)-len(all), pre, full)
}

// clip keeps the top-level pieces that fit in ClipBytes, with a note that
// points to the file of the full mail. frame is the size of the layout.
func (r *Renderer) clip(p Page, pieces []string, frame int, pre, full string) (Rendered, error) {
	c := alertColors["note"]
	note := `<p class="markdown-alert" style="margin:16px 0 0;padding:12px 16px;border-left:4px solid ` + c[0] + ";background:" + c[1] +
		`;border-radius:4px">This mail is too long to show in full. Open the attached file ` + FullMailName + ` to read all of it.</p>`
	room := ClipBytes - frame - len(note)
	var kept strings.Builder
	for _, piece := range pieces {
		if kept.Len()+len(piece) > room {
			break
		}
		kept.WriteString(piece)
	}
	kept.WriteString(note)
	cut, err := r.page(p, kept.String(), pre)
	if err != nil {
		return Rendered{}, err
	}
	return Rendered{HTML: cut, Full: full}, nil
}

// page puts a styled body in the layout of p.
func (r *Renderer) page(p Page, body, pre string) (string, error) {
	var out bytes.Buffer
	err := r.layout.Execute(&out, map[string]any{
		"T": p.Theme, "Page": p, "Body": template.HTML(body), //nolint:gosec // goldmark output, raw HTML disabled
		"Preheader": pre,
		"MsoOpen": template.HTML(fmt.Sprintf( //nolint:gosec // constant markup
			`<!--[if mso]><table role="presentation" width="%d" align="center" cellpadding="0" cellspacing="0"><tr><td><![endif]-->`, p.Theme.Width)),
		"MsoClose": template.HTML(`<!--[if mso]></td></tr></table><![endif]-->`),
	})
	if err != nil {
		return "", fmt.Errorf("render mail layout: %w", err)
	}
	return out.String(), nil
}

// styler sets the inline styles of one theme on rendered nodes.
type styler struct {
	t    Theme
	tags map[atom.Atom]string
}

// SanitizeHTML strips scripts, forms, and event handlers from untrusted
// mail HTML. It keeps layout, links, images, and simple inline styles.
// The UI still shows the result in a sandboxed frame.
func SanitizeHTML(in string) string {
	p := bluemonday.UGCPolicy()
	p.AllowStyles("color", "background-color", "font-size", "font-weight", "font-style",
		"font-family", "text-align", "text-decoration", "line-height", "margin", "padding",
		"border", "width", "max-width", "height").Globally()
	p.AllowAttrs("align", "valign", "bgcolor", "width", "height").Globally()
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p.Sanitize(in)
}

// PlainText converts an HTML part into its text part. Blocks become lines,
// table cells of one row share a line, and a link keeps its URL. Hidden
// elements, such as a preheader, are dropped.
func PlainText(src string) string {
	doc, err := xhtml.Parse(strings.NewReader(src))
	if err != nil {
		return ""
	}
	var t textPart
	t.walk(doc, false)
	lines := strings.Split(t.b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func newStyler(t Theme) styler {
	mono := "font-family:" + t.Mono + ";"
	return styler{t: t, tags: map[atom.Atom]string{
		atom.H1:         "margin:24px 0 12px;font-size:24px;line-height:32px;font-weight:700;",
		atom.H2:         "margin:24px 0 10px;font-size:20px;line-height:28px;font-weight:700;",
		atom.H3:         "margin:20px 0 8px;font-size:17px;line-height:24px;font-weight:600;",
		atom.H4:         "margin:16px 0 8px;font-size:16px;line-height:24px;font-weight:600;",
		atom.H5:         "margin:16px 0 8px;font-size:15px;line-height:22px;font-weight:600;",
		atom.H6:         "margin:16px 0 8px;font-size:14px;line-height:20px;font-weight:600;color:" + t.Muted + ";",
		atom.P:          "margin:0 0 16px;font-size:16px;line-height:24px;",
		atom.A:          "color:" + t.Accent + ";text-decoration:underline;",
		atom.Ul:         "margin:0 0 16px;padding:0 0 0 24px;",
		atom.Ol:         "margin:0 0 16px;padding:0 0 0 24px;",
		atom.Li:         "margin:0 0 6px;font-size:16px;line-height:24px;",
		atom.Blockquote: "margin:0 0 16px;padding:4px 16px;border-left:4px solid " + t.Border + ";color:" + t.Muted + ";",
		atom.Code:       mono + "font-size:14px;background:" + t.Code + ";padding:2px 4px;border-radius:4px;",
		atom.Pre: mono + "margin:0 0 16px;padding:12px 16px;font-size:13px;line-height:20px;background:" + t.Code +
			";border-radius:6px;white-space:pre-wrap;word-break:break-word;",
		atom.Table: "width:100%;margin:0 0 16px;border-collapse:collapse;font-size:14px;line-height:20px;",
		atom.Th:    "padding:8px 10px;text-align:left;font-weight:600;background:" + t.Code + ";border-bottom:2px solid " + t.Border + ";",
		atom.Td:    "padding:8px 10px;vertical-align:top;border-bottom:1px solid " + t.Border + ";",
		atom.Hr:    "margin:24px 0;border:0;border-top:1px solid " + t.Border + ";",
		atom.Img:   "max-width:100%;height:auto;",
		atom.Del:   "color:" + t.Muted + ";",
	}}
}

// style sets the inline style of n and its subtree. A style goldmark wrote
// (table cell alignment) stays after the house style, so it wins.
func (s styler) style(n *xhtml.Node, inPre bool) {
	if n.DataAtom == atom.Input {
		// Mail clients drop form controls, so a task box becomes a glyph.
		box := "☐"
		for _, a := range n.Attr {
			if a.Key == "checked" {
				box = "☑"
			}
		}
		*n = xhtml.Node{Type: xhtml.TextNode, Data: box, Parent: n.Parent, PrevSibling: n.PrevSibling, NextSibling: n.NextSibling}
		return
	}
	if css := s.css(n, inPre); css != "" {
		setStyle(n, css)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		s.style(c, inPre || n.DataAtom == atom.Pre)
	}
	// The last paragraph of a callout sits on its bottom padding.
	if n.DataAtom == atom.Div && hasClassPrefix(n, "markdown-alert-") {
		for c := n.LastChild; c != nil; c = c.PrevSibling {
			if c.DataAtom == atom.P {
				appendStyle(c, "margin-bottom:0;")
				break
			}
		}
	}
}

func (s styler) css(n *xhtml.Node, inPre bool) string {
	switch {
	case n.Type != xhtml.ElementNode:
		return ""
	case n.DataAtom == atom.Code && inPre:
		return "font-family:" + s.t.Mono + ";"
	case n.DataAtom == atom.Div && hasClassPrefix(n, "markdown-alert-"):
		c := alertColors[alertKind(n)]
		return "margin:0 0 16px;padding:12px 16px;border-left:4px solid " + c[0] + ";background:" + c[1] + ";border-radius:4px;"
	case n.DataAtom == atom.P && hasClassPrefix(n, "markdown-alert-title"):
		return "margin:0 0 4px;font-size:15px;line-height:22px;font-weight:600;color:" + alertColors[alertKind(n.Parent)][0] + ";"
	}
	return s.tags[n.DataAtom]
}

// block renders a top-level fenced kpi, status, or button block as report
// markup. It returns nil for any other node and for a block with no valid
// line, so that block stays a code block.
func (s styler) block(n *xhtml.Node) []*xhtml.Node {
	code := n.FirstChild
	if n.DataAtom != atom.Pre || code == nil || code.DataAtom != atom.Code || code.FirstChild == nil {
		return nil
	}
	var kind string
	for _, a := range code.Attr {
		if a.Key == "class" {
			kind = strings.TrimPrefix(a.Val, "language-")
		}
	}
	parts := map[string]int{"kpi": 3, "status": 2, "button": 1}[kind]
	if parts == 0 {
		return nil
	}
	rows := blockRows(code.FirstChild.Data, parts)
	var markup string
	switch kind {
	case "kpi":
		markup = s.kpi(rows)
	case "status":
		markup = s.status(rows)
	case "button":
		markup = s.buttons(rows)
	}
	if markup == "" {
		return nil
	}
	nodes, err := xhtml.ParseFragment(strings.NewReader(markup), &xhtml.Node{Type: xhtml.ElementNode, Data: "div", DataAtom: atom.Div})
	if err != nil {
		return nil
	}
	return nodes
}

// blockRows splits "label: field, field" lines into escaped fields, at
// most parts after the label. ", " splits the fields, so a value such as
// 1,234 stays whole. A line with no label or no first field drops.
func blockRows(text string, parts int) [][]string {
	var rows [][]string
	for line := range strings.Lines(text) {
		label, rest, ok := strings.Cut(line, ":")
		label, rest = strings.TrimSpace(label), strings.TrimSpace(rest)
		if !ok || label == "" || rest == "" {
			continue
		}
		fields := append([]string{label}, strings.SplitN(rest, ", ", parts)...)
		for i := range fields {
			fields[i] = template.HTMLEscapeString(strings.TrimSpace(fields[i]))
		}
		if fields[1] != "" {
			rows = append(rows, fields)
		}
	}
	return rows
}

// kpi renders "Label: value, change, tone" lines as number tiles, three to
// a row. A change that starts with + is good and one with - is bad; the
// tone good, bad, or neutral overrides the sign.
func (s styler) kpi(rows [][]string) string {
	var b strings.Builder
	for start := 0; start < len(rows); start += 3 {
		row := rows[start:min(start+3, len(rows))]
		b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="margin:0 0 12px;border-collapse:collapse"><tr>`)
		for i, f := range row {
			if i > 0 {
				b.WriteString(`<td width="8" style="width:8px"></td>`)
			}
			fmt.Fprintf(&b, `<td class="tb-tile" width="%d%%" style="padding:12px 14px;background:%s;border-radius:6px;vertical-align:top">`, 100/len(row), s.t.Code)
			fmt.Fprintf(&b, `<div style="font-size:12px;line-height:16px;color:%s;text-transform:uppercase;letter-spacing:0.04em">%s</div>`, s.t.Muted, f[0])
			fmt.Fprintf(&b, `<div style="font-size:24px;line-height:32px;font-weight:700">%s</div>`, f[1])
			if len(f) > 2 && f[2] != "" {
				tone := ""
				if len(f) > 3 {
					tone = strings.ToLower(f[3])
				}
				color := s.t.Muted
				switch {
				case tone == "good", tone == "" && strings.HasPrefix(f[2], "+"):
					color = alertColors["tip"][0]
				case tone == "bad", tone == "" && (strings.HasPrefix(f[2], "-") || strings.HasPrefix(f[2], "−")):
					color = alertColors["caution"][0]
				}
				fmt.Fprintf(&b, `<div style="font-size:13px;line-height:18px;font-weight:600;color:%s">%s</div>`, color, f[2])
			}
			b.WriteString(`</td>`)
		}
		b.WriteString(`</tr></table>`)
	}
	return b.String()
}

// statusTone maps the first word of a state to ok, warn, or fail.
var statusTone = map[string]string{ //nolint:gochecknoglobals // static lookup, never mutated
	"ok": "tip", "pass": "tip", "passed": "tip", "done": "tip", "success": "tip", "healthy": "tip", "up": "tip",
	"warn": "warning", "warning": "warning", "slow": "warning", "degraded": "warning", "pending": "warning", "skipped": "warning",
	"fail": "caution", "failed": "caution", "error": "caution", "down": "caution", "broken": "caution",
}

// status renders "name: state, note" lines as a list with colored dots.
func (s styler) status(rows [][]string) string {
	var b strings.Builder
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="margin:0 0 16px;border-collapse:collapse;font-size:14px;line-height:20px">`)
	cell := "padding:8px 10px 8px 0;border-bottom:1px solid " + s.t.Border + ";vertical-align:top;"
	for _, f := range rows {
		color := s.t.Muted
		if k, ok := statusTone[strings.ToLower(strings.Fields(f[1])[0])]; ok {
			color = alertColors[k][0]
		}
		note := ""
		if len(f) > 2 {
			note = f[2]
		}
		fmt.Fprintf(&b, `<tr><td width="18" style="%scolor:%s">&#9679;</td><td style="%sfont-weight:600">%s</td><td style="%sfont-weight:600;color:%s">%s</td><td style="%scolor:%s">%s</td></tr>`,
			cell, color, cell, f[0], cell, color, f[1], cell, s.t.Muted, note)
	}
	b.WriteString(`</table>`)
	return b.String()
}

// buttons renders "Label: URL" lines as one row of buttons that work in
// Outlook too. A line with a URL that is not http, https, or mailto drops.
func (s styler) buttons(rows [][]string) string {
	var b strings.Builder
	n := 0
	for _, f := range rows {
		u, err := url.Parse(xhtml.UnescapeString(f[1]))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "mailto") {
			continue
		}
		if n > 0 {
			b.WriteString(`<td width="8" style="width:8px"></td>`)
		}
		n++
		fmt.Fprintf(&b, `<td bgcolor="%s" style="border-radius:6px;background:%s"><a href="%s" target="_blank" style="display:inline-block;padding:10px 18px;font-size:15px;line-height:20px;font-weight:600;color:%s;text-decoration:none;border-radius:6px">%s</a></td>`,
			s.t.Accent, s.t.Accent, f[1], s.t.Surface, f[0])
	}
	if n == 0 {
		return ""
	}
	return `<table role="presentation" cellpadding="0" cellspacing="0" style="margin:0 0 16px"><tr>` + b.String() + `</tr></table>`
}

func setStyle(n *xhtml.Node, css string) {
	for i, a := range n.Attr {
		if a.Key == "style" {
			n.Attr[i].Val = css + a.Val
			return
		}
	}
	n.Attr = append(n.Attr, xhtml.Attribute{Key: "style", Val: css})
}

// appendStyle adds css after the inline style of n, so it wins.
func appendStyle(n *xhtml.Node, css string) {
	for i, a := range n.Attr {
		if a.Key == "style" {
			n.Attr[i].Val += css
			return
		}
	}
	n.Attr = append(n.Attr, xhtml.Attribute{Key: "style", Val: css})
}

func hasClassPrefix(n *xhtml.Node, prefix string) bool {
	if n == nil {
		return false
	}
	for _, a := range n.Attr {
		if a.Key == "class" {
			for f := range strings.FieldsSeq(a.Val) {
				if strings.HasPrefix(f, prefix) {
					return true
				}
			}
		}
	}
	return false
}

// alertKind reads "note" from class="markdown-alert markdown-alert-note".
func alertKind(n *xhtml.Node) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == "class" {
			for f := range strings.FieldsSeq(a.Val) {
				if k, ok := strings.CutPrefix(f, "markdown-alert-"); ok && k != "title" {
					return k
				}
			}
		}
	}
	return ""
}

// collectText gathers prose for the preheader. Headings, tables, and code
// make a poor preview, so it skips them.
func collectText(n *xhtml.Node, b *strings.Builder) {
	if b.Len() > 400 {
		return
	}
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Table, atom.Pre:
		return
	}
	if n.DataAtom == atom.P && hasClassPrefix(n, "markdown-alert-title") {
		return
	}
	if n.Type == xhtml.TextNode {
		b.WriteString(n.Data)
		b.WriteByte(' ')
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectText(c, b)
	}
}

// preheader is the inbox preview line: the first words of the body.
func preheader(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 140 {
		return s
	}
	cut := 140
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

// textPart collects the text of an HTML tree. breaks and space are the
// separator due before the next word; it is written only when a word comes.
type textPart struct {
	b      strings.Builder
	breaks int
	space  bool
}

// dropInvisible removes the format characters that mail templates use as
// padding, such as the zero-width joiners after a preheader.
func dropInvisible(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\u034f', '\ufeff', '\u00ad':
			return -1
		}
		return r
	}, s)
}

func (t *textPart) brk(n int) {
	t.breaks = max(t.breaks, n)
}

func (t *textPart) write(s string) {
	if t.b.Len() > 0 {
		switch {
		case t.breaks > 0:
			t.b.WriteString(strings.Repeat("\n", t.breaks))
		case t.space:
			t.b.WriteByte(' ')
		}
	}
	t.breaks, t.space = 0, false
	t.b.WriteString(s)
}

// words writes inline text with its whitespace collapsed.
func (t *textPart) words(s string) {
	s = dropInvisible(s)
	if s == "" {
		return
	}
	lead := strings.TrimLeftFunc(s, unicode.IsSpace) != s
	trail := strings.TrimRightFunc(s, unicode.IsSpace) != s
	f := strings.Fields(s)
	if len(f) == 0 {
		t.space = true
		return
	}
	t.space = t.space || lead
	t.write(strings.Join(f, " "))
	t.space = trail
}

func (t *textPart) walk(n *xhtml.Node, inPre bool) {
	switch n.Type {
	case xhtml.TextNode:
		if inPre {
			t.write(dropInvisible(n.Data))
		} else {
			t.words(n.Data)
		}
		return
	case xhtml.ElementNode:
		if hidden(n) {
			return
		}
	case xhtml.DocumentNode:
	default:
		return
	}
	gap, done := t.open(n)
	if done {
		return
	}
	breaks, space := t.breaks, t.space
	t.brk(gap)
	start := t.b.Len()
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		t.walk(c, inPre || n.DataAtom == atom.Pre)
	}
	if t.b.Len() == start {
		// An empty block, such as a chart bar, adds no break.
		t.breaks, t.space = breaks, space
		return
	}
	t.close(n, gap, strings.TrimSpace(t.b.String()[start:]))
}

// close ends an element that wrote text. A link adds its URL after the
// label, unless the label is the URL.
func (t *textPart) close(n *xhtml.Node, gap int, label string) {
	if n.DataAtom == atom.A {
		href := strings.TrimSpace(attr(n, "href"))
		if (strings.HasPrefix(href, "http") || strings.HasPrefix(href, "mailto:")) &&
			label != href && label != strings.TrimPrefix(href, "mailto:") {
			t.space = true
			t.write("(" + href + ")")
		}
	}
	t.brk(gap)
	if n.DataAtom == atom.Td || n.DataAtom == atom.Th {
		t.space = true
	}
}

// open writes what an element shows before its children: a break, a list
// mark, or the alt text of an image. done means the children are skipped.
func (t *textPart) open(n *xhtml.Node) (gap int, done bool) {
	switch n.DataAtom {
	case atom.Head, atom.Style, atom.Script, atom.Title, atom.Template, atom.Noscript:
		return 0, true
	case atom.Br:
		t.brk(1)
		return 0, true
	case atom.Hr:
		t.brk(2)
		t.write("---")
		t.brk(2)
		return 0, true
	case atom.Img:
		if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
			t.space = true
			t.write(alt)
			t.space = true
		}
		return 0, true
	case atom.P, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Ul, atom.Ol,
		atom.Table, atom.Blockquote, atom.Pre:
		gap = 2
	case atom.Div, atom.Tr, atom.Section, atom.Header, atom.Footer, atom.Article, atom.Center, atom.Dt, atom.Dd:
		gap = 1
	case atom.Td, atom.Th:
		t.space = true
	case atom.Li:
		t.brk(1)
		t.write(listMark(n))
		t.space = true
	}
	return gap, false
}

// listMark is "-" for a bullet and "3." for the third item of an ordered list.
func listMark(li *xhtml.Node) string {
	if li.Parent == nil || li.Parent.DataAtom != atom.Ol {
		return "-"
	}
	i := 1
	for s := li.PrevSibling; s != nil; s = s.PrevSibling {
		if s.DataAtom == atom.Li {
			i++
		}
	}
	return fmt.Sprintf("%d.", i)
}

// hidden reports an element that a mail client does not show.
func hidden(n *xhtml.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "hidden" {
			return true
		}
	}
	css := strings.ReplaceAll(strings.ToLower(attr(n, "style")), " ", "")
	return strings.Contains(css, "display:none") || strings.Contains(css, "mso-hide:all")
}

func attr(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// layoutHTML is the frame of all three layouts. Base styles are inline;
// the head style only adds dark mode for the clients that support it.
const layoutHTML = `<!DOCTYPE html>
<html lang="en" dir="ltr" xmlns:o="urn:schemas-microsoft-com:office:office">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="x-apple-disable-message-reformatting">
<meta name="format-detection" content="telephone=no,date=no,address=no,email=no,url=no">
<meta name="color-scheme" content="light dark">
<meta name="supported-color-schemes" content="light dark">
<title>{{.Page.Title}}</title>
<style>
:root{color-scheme:light dark;supported-color-schemes:light dark}
@media (prefers-color-scheme:dark){
.tb-bg{background:{{.T.DarkBackground}} !important}
.tb-card{background:{{.T.DarkSurface}} !important;color:{{.T.DarkText}} !important;border-color:{{.T.DarkSurface}} !important}
.tb-tile,.tb-card th,.tb-card pre,.tb-card code,.tb-card .markdown-alert{background:{{.T.DarkBackground}} !important}
.tb-card td,.tb-card th,.tb-card hr,.tb-card blockquote{border-color:{{.T.DarkBorder}} !important}
}
</style>
</head>
{{- $min := eq .Page.Layout "minimal"}}{{$bg := .T.Background}}{{if $min}}{{$bg = .T.Surface}}{{end}}
<body class="tb-bg" style="margin:0;padding:0;word-spacing:normal;background:{{$bg}}">
<div style="display:none;max-height:0;overflow:hidden;mso-hide:all">{{.Preheader}}&#8199;&#65279;&#847;&#8199;&#65279;&#847;&#8199;&#65279;&#847;</div>
<div role="article" aria-roledescription="email" aria-label="{{.Page.Title}}" lang="en" dir="ltr" class="tb-bg" style="padding:24px 12px;background:{{$bg}}">
{{.MsoOpen}}
{{- if eq .Page.Layout "report"}}{{if or .Page.Logo .Page.Brand}}
<div style="max-width:{{.T.Width}}px;margin:0 auto;padding:0 4px 12px;font-family:{{.T.Font}}">{{if .Page.Logo}}<img src="{{.Page.Logo}}" alt="{{.Page.Brand}}" height="32" style="display:block;height:32px;width:auto;border:0">{{else}}<span style="font-size:15px;line-height:20px;font-weight:700;color:{{.T.Accent}}">{{.Page.Brand}}</span>{{end}}</div>
{{- end}}{{end}}
{{if $min}}<div class="tb-card" style="max-width:{{.T.Width}}px;margin:0 auto;padding:0 4px;background:{{.T.Surface}};color:{{.T.Text}};font-family:{{.T.Font}};font-size:16px;line-height:24px">
{{else}}<div class="tb-card" style="max-width:{{.T.Width}}px;margin:0 auto;padding:28px 32px;background:{{.T.Surface}};color:{{.T.Text}};border:1px solid {{.T.Border}};{{if eq .Page.Layout "report"}}border-top:4px solid {{.T.Accent}};{{end}}border-radius:8px;font-family:{{.T.Font}};font-size:16px;line-height:24px">
{{end}}{{.Body}}
{{if .Page.Link}}<p style="margin:20px 0 0"><a href="{{.Page.Link}}" style="display:inline-block;padding:10px 18px;border-radius:6px;background:{{.T.Accent}};color:#ffffff;font-weight:600;text-decoration:none">{{or .Page.LinkLabel "Open in Trebi"}}</a></p>
{{end}}</div>
{{if .Page.Footer}}<div style="max-width:{{.T.Width}}px;margin:12px auto 0;font-family:{{.T.Font}};font-size:12px;line-height:18px;color:{{.T.Muted}};text-align:{{if $min}}left;padding:0 4px{{else}}center{{end}}">{{.Page.Footer}}</div>
{{end}}{{.MsoClose}}
</div>
</body>
</html>
`
