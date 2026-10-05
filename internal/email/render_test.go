package email

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

func TestRendererMarkdown(t *testing.T) {
	t.Parallel()
	r := NewRenderer()
	for _, tc := range []struct {
		name, src string
		want, not []string
	}{
		{
			name: "inline styles",
			src:  "## Summary\n\nRuns are **up 12%**. See [the board](https://example.com).",
			want: []string{`<h2 style="`, `<strong>up 12%</strong>`, `<a href="https://example.com" style="`, "color:#2563eb"},
		},
		{
			name: "alert",
			src:  "> [!WARNING]\n> Two jobs failed.",
			want: []string{"border-left:4px solid", "Warning", "Two jobs failed."},
			not:  []string{"<svg"},
		},
		{
			name: "table keeps alignment",
			src:  "| Job | Runs |\n|---|--:|\n| digest | 42 |",
			want: []string{"<table style=", "text-align:right", "border:1px solid #d8dee4"},
		},
		{
			name: "task list",
			src:  "- [x] backups\n- [ ] rotate key",
			want: []string{"☑ backups", "☐ rotate key"},
			not:  []string{"<input"},
		},
		{
			name: "code block uses mono only inside pre",
			src:  "```sh\ntrebi runs list\n```",
			want: []string{"<pre style=", `<code class="language-sh" style="font-family:`},
		},
		{
			name: "raw html omitted",
			src:  "Hi <script>alert(1)</script> there.",
			not:  []string{"<script>alert"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := r.Markdown(tc.src, Page{})
			if err != nil {
				t.Fatal(err)
			}
			out := got.HTML
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q", w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(out, n) {
					t.Errorf("unexpected %q", n)
				}
			}
		})
	}
}

func TestRendererLayout(t *testing.T) {
	t.Parallel()
	body := "## Totals\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nAll jobs " + strings.Repeat("ran well and ", 20) + "finished."
	got, err := NewRenderer().Markdown(body, Page{Title: "Weekly <report>", Footer: "Sent by Trebi"})
	if err != nil {
		t.Fatal(err)
	}
	out := got.HTML
	for _, w := range []string{
		"<!DOCTYPE html>", `name="color-scheme"`, "prefers-color-scheme:dark",
		"<!--[if mso]>", "max-width:600px", "Weekly &lt;report&gt;", "Sent by Trebi",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q", w)
		}
	}
	// The preheader skips headings and tables and stops at 140 characters.
	start := strings.Index(out, "All jobs ran well")
	if start < 0 || start > strings.Index(out, "<h2") {
		t.Fatalf("preheader does not lead with the first paragraph")
	}
	if !strings.Contains(out[start:start+200], "…") {
		t.Errorf("preheader is not cut")
	}
}

func TestRendererLayouts(t *testing.T) {
	t.Parallel()
	r := NewRenderer()
	accent := DefaultTheme().With(job.StyleYAML{Accent: "#0f766e"})
	for _, tc := range []struct {
		name      string
		page      Page
		want, not []string
	}{
		{
			name: "report with brand",
			page: Page{Layout: LayoutReport, Brand: "Growth", Theme: accent},
			want: []string{">Growth</span>", "border-top:4px solid #0f766e", "color:#0f766e;text-decoration:underline"},
		},
		{
			name: "report with logo",
			page: Page{Layout: LayoutReport, Brand: "Growth", Logo: "https://example.com/l.png"},
			want: []string{`<img src="https://example.com/l.png" alt="Growth"`},
			not:  []string{">Growth</span>"},
		},
		{
			name: "reply has no header",
			page: Page{Layout: LayoutReply, Brand: "Growth"},
			want: []string{"border:1px solid"},
			not:  []string{"Growth", "border-top:4px"},
		},
		{
			name: "minimal keeps line breaks",
			page: Page{Layout: LayoutMinimal, Footer: "Acme"},
			want: []string{"<br/>", "padding:0 4px;background:#fdfdfd", "Acme"},
			not:  []string{"border:1px solid"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := r.Markdown("Line one\nline two. [Open](https://example.com)", tc.page)
			if err != nil {
				t.Fatal(err)
			}
			out := got.HTML
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q", w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(out, n) {
					t.Errorf("unexpected %q", n)
				}
			}
		})
	}
}

func TestRendererBlocks(t *testing.T) {
	t.Parallel()
	r := NewRenderer()
	for _, tc := range []struct {
		name, src string
		want, not []string
	}{
		{
			name: "kpi tiles",
			src:  "```kpi\nRuns: 1,234, +12%\nCost: $3.20, -4%, good\nFailed: 2, +1, bad\nSkipped: 0\n```",
			want: []string{
				`class="tb-tile"`, ">1,234<", "color:#1a7f37\">+12%", "color:#1a7f37\">-4%", "color:#d1242f\">+1",
				`width="33%"`, `width="100%" style="padding`,
			},
			not: []string{"<pre", "```"},
		},
		{
			name: "status list",
			src:  "```status\ndigest: ok\nsync: failed, token expired, retry at 3\nbackup: unknown\n```",
			want: []string{"●", ">digest<", "color:#1a7f37\">ok<", "color:#d1242f\">failed<", "token expired, retry at 3", "color:#59636e\">unknown<"},
		},
		{
			name: "buttons drop unsafe links",
			src:  "```button\nOpen the run: https://x.io/r?a=1&b=2\nBad: javascript:alert(1)\n```",
			want: []string{`href="https://x.io/r?a=1&amp;b=2"`, ">Open the run</a>", `bgcolor="#2563eb"`},
			not:  []string{"javascript", ">Bad<"},
		},
		{
			name: "markup in a value is text",
			src:  "```kpi\n<b>Runs</b>: <script>x</script>\n```",
			want: []string{"&lt;b&gt;Runs&lt;/b&gt;", "&lt;script&gt;"},
			not:  []string{"<script>", "<b>"},
		},
		{
			name: "no valid line keeps the code block",
			src:  "```kpi\njust words\n```",
			want: []string{"<pre", "just words"},
			not:  []string{"tb-tile\" style"},
		},
		{
			name: "other languages stay code",
			src:  "```sh\nrun: now\n```",
			want: []string{"<pre", "run: now"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := r.Markdown(tc.src, Page{Layout: LayoutReport})
			if err != nil {
				t.Fatal(err)
			}
			out := got.HTML
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q", w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(out, n) {
					t.Errorf("unexpected %q", n)
				}
			}
		})
	}
}

func TestRendererClip(t *testing.T) {
	t.Parallel()
	r := NewRenderer()
	para := "Run " + strings.Repeat("ok and ", 60) + "done.\n\n"
	short, err := r.Markdown(para, Page{Layout: LayoutReport})
	if err != nil {
		t.Fatal(err)
	}
	if short.Full != "" {
		t.Fatalf("a short mail is cut")
	}
	got, err := r.Markdown("# Top\n\n"+strings.Repeat(para, 400)+"Last line.", Page{Layout: LayoutReport})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.HTML) > ClipBytes || got.Full == "" || len(got.Full) <= ClipBytes {
		t.Fatalf("cut %d, full %d", len(got.HTML), len(got.Full))
	}
	for _, w := range []string{"<!DOCTYPE html>", ">Top</h1>", "too long to show in full", FullMailName, "</html>"} {
		if !strings.Contains(got.HTML, w) {
			t.Errorf("cut mail misses %q", w)
		}
	}
	if strings.Contains(got.HTML, "Last line.") || !strings.Contains(got.Full, "Last line.") {
		t.Errorf("the cut is not at the budget")
	}
	if strings.Count(got.HTML, "<p")-strings.Count(got.HTML, "</p>") != 0 {
		t.Errorf("the cut splits a block")
	}

	html, files := got.attach(nil)
	if html != got.HTML || len(files) != 1 || files[0].Name != FullMailName || string(files[0].Content) != got.Full {
		t.Errorf("attach with room: %d files", len(files))
	}
	full := make([]SendAttachment, MaxAttachments)
	if html, files = got.attach(full); html != got.Full || len(files) != MaxAttachments {
		t.Errorf("attach with no room sends the cut mail")
	}
}

func TestPlainText(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ in, want string }{
		"paragraphs": {"<p>One  line.</p><p>Two\n lines.</p>", "One line.\n\nTwo lines."},
		"head and hidden": {`<html><head><title>T</title><style>p{}</style></head><body>` +
			`<div style="display: none">preview&zwnj;&nbsp;</div><p>Body</p><script>x()</script></body></html>`, "Body"},
		"inline": {"<p>A <b>bold</b> word.<br>Next line</p>", "A bold word.\nNext line"},
		"lists":  {"<ul><li>a</li><li>b</li></ul><ol><li>x</li><li>y</li></ol>", "- a\n- b\n\n1. x\n2. y"},
		"links":  {`<p><a href="https://x.io/r">report</a> <a href="https://x.io">https://x.io</a> <a href="#top">top</a></p>`, "report (https://x.io/r) https://x.io top"},
		"table rows": {`<table><tr><td>Mon</td><td><table><tr><td style="width:40%"></td></tr></table></td><td>66 bpm</td></tr>` +
			`<tr><td>Tue</td><td></td><td>65 bpm</td></tr></table>`, "Mon 66 bpm\nTue 65 bpm"},
		"image alt": {`<p><img src="a.png" alt="Logo"> Weekly</p>`, "Logo Weekly"},
		"pre":       {"<pre>a\n  b</pre>", "a\n  b"},
		"no text":   {`<img src="a.png">`, ""},
	} {
		if got := PlainText(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}
