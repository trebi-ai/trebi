package email

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"unicode/utf8"

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
		DarkText: "#e6e8eb", DarkBackground: "#15171a", DarkSurface: "#1f2226",
		Font:  "-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,Helvetica,Arial,sans-serif",
		Mono:  "ui-monospace,SFMono-Regular,Menlo,Consolas,Liberation Mono,monospace",
		Width: 600,
	}
}

// Page is the frame around one rendered body.
type Page struct {
	Title  string // a heading above the body; "" omits it
	Footer string // a small line below the card; "" omits it
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
	theme  Theme
	parser parser.Parser
	html   html.Renderer
	layout *template.Template
	styles map[atom.Atom]string
}

// NewRenderer builds a renderer for one theme.
func NewRenderer(t Theme) *Renderer {
	r := &Renderer{
		theme:  t,
		parser: parser.New(parser.WithExtensions(extension.GFMParser, alert.Parser)),
		html:   html.New(html.WithExtensions(extension.GFMHTMLRenderer, alert.NewHTMLRenderer(alert.WithAlertIcons(map[string]string{})))),
		layout: template.Must(template.New("mail").Parse(layoutHTML)),
	}
	mono := "font-family:" + t.Mono + ";"
	r.styles = map[atom.Atom]string{
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
	}
	return r
}

// Markdown renders src as one complete HTML mail. Raw HTML in src is left
// out; links with a dangerous scheme lose their target.
func (r *Renderer) Markdown(src string, p Page) (string, error) {
	source := []byte(src)
	var body bytes.Buffer
	if err := r.html.Render(&body, source, r.parser.Parse(source)); err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	ctx := &xhtml.Node{Type: xhtml.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := xhtml.ParseFragment(&body, ctx)
	if err != nil {
		return "", fmt.Errorf("parse rendered markdown: %w", err)
	}
	var styled, text strings.Builder
	for _, n := range nodes {
		r.style(n, false)
		collectText(n, &text)
		if err := xhtml.Render(&styled, n); err != nil {
			return "", fmt.Errorf("write rendered markdown: %w", err)
		}
	}
	var out bytes.Buffer
	err = r.layout.Execute(&out, map[string]any{
		"T": r.theme, "Page": p, "Body": template.HTML(styled.String()), //nolint:gosec // goldmark output, raw HTML disabled
		"Preheader": preheader(text.String()),
		"MsoOpen": template.HTML(fmt.Sprintf( //nolint:gosec // constant markup
			`<!--[if mso]><table role="presentation" width="%d" align="center" cellpadding="0" cellspacing="0"><tr><td><![endif]-->`, r.theme.Width)),
		"MsoClose": template.HTML(`<!--[if mso]></td></tr></table><![endif]-->`),
	})
	if err != nil {
		return "", fmt.Errorf("render mail layout: %w", err)
	}
	return out.String(), nil
}

// style sets the inline style of n and its subtree. A style goldmark wrote
// (table cell alignment) stays after the house style, so it wins.
func (r *Renderer) style(n *xhtml.Node, inPre bool) {
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
	if css := r.css(n, inPre); css != "" {
		setStyle(n, css)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.style(c, inPre || n.DataAtom == atom.Pre)
	}
	// The last paragraph of a callout sits on its bottom padding.
	if n.DataAtom == atom.Div && hasClassPrefix(n, "markdown-alert-") {
		for c := n.LastChild; c != nil; c = c.PrevSibling {
			if c.DataAtom == atom.P {
				for i, a := range c.Attr {
					if a.Key == "style" {
						c.Attr[i].Val += "margin-bottom:0;"
					}
				}
				break
			}
		}
	}
}

func (r *Renderer) css(n *xhtml.Node, inPre bool) string {
	switch {
	case n.Type != xhtml.ElementNode:
		return ""
	case n.DataAtom == atom.Code && inPre:
		return "font-family:" + r.theme.Mono + ";"
	case n.DataAtom == atom.Div && hasClassPrefix(n, "markdown-alert-"):
		c := alertColors[alertKind(n)]
		return "margin:0 0 16px;padding:12px 16px;border-left:4px solid " + c[0] + ";background:" + c[1] + ";border-radius:4px;"
	case n.DataAtom == atom.P && hasClassPrefix(n, "markdown-alert-title"):
		return "margin:0 0 4px;font-size:15px;line-height:22px;font-weight:600;color:" + alertColors[alertKind(n.Parent)][0] + ";"
	}
	return r.styles[n.DataAtom]
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

// layoutHTML is the one-column frame. Base styles are inline; the head
// style only adds dark mode for the clients that support it.
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
}
</style>
</head>
<body style="margin:0;padding:0;word-spacing:normal;background:{{.T.Background}}">
<div style="display:none;max-height:0;overflow:hidden;mso-hide:all">{{.Preheader}}&#8199;&#65279;&#847;&#8199;&#65279;&#847;&#8199;&#65279;&#847;</div>
<div role="article" aria-roledescription="email" aria-label="{{.Page.Title}}" lang="en" dir="ltr" class="tb-bg" style="padding:24px 12px;background:{{.T.Background}}">
{{.MsoOpen}}
<div class="tb-card" style="max-width:{{.T.Width}}px;margin:0 auto;padding:28px 32px;background:{{.T.Surface}};color:{{.T.Text}};border:1px solid {{.T.Border}};border-radius:8px;font-family:{{.T.Font}};font-size:16px;line-height:24px">
{{if .Page.Title}}<h1 style="margin:0 0 20px;font-size:24px;line-height:32px;font-weight:700">{{.Page.Title}}</h1>
{{end}}{{.Body}}
</div>
{{if .Page.Footer}}<div style="max-width:{{.T.Width}}px;margin:12px auto 0;font-family:{{.T.Font}};font-size:12px;line-height:18px;color:{{.T.Muted}};text-align:center">{{.Page.Footer}}</div>
{{end}}{{.MsoClose}}
</div>
</body>
</html>
`
