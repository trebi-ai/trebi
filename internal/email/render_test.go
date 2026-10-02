package email

import (
	"strings"
	"testing"
)

func TestRendererMarkdown(t *testing.T) {
	t.Parallel()
	r := NewRenderer(DefaultTheme())
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
			out, err := r.Markdown(tc.src, Page{})
			if err != nil {
				t.Fatal(err)
			}
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
	out, err := NewRenderer(DefaultTheme()).Markdown(body, Page{Title: "Weekly <report>", Footer: "Sent by Trebi"})
	if err != nil {
		t.Fatal(err)
	}
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
