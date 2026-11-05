package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// filledExample is a filled v2 file, byte for byte.
const filledExample = `<!-- trebi memory v2. Keep these three sections in this order. Do not add other "## " headings. -->
# Memory

## Context
Daily monitor. Sends one email when a topic's R value crosses 0.8.
Topics come from ` + "`topics.yaml`" + ` next to this file.

## Lessons
- [2026-08-02 09:00] Skip duplicate alerts within 24h.
- [2026-09-01] The API rate limits at 60 req/min. Back off 10s.

## Log
- [2026-09-13 08:00] topic B crossed 0.8 (R 0.83). Alerted.
- [2026-09-14 08:00] topic A 0.41, topic B 0.85. No alert.
  Two topics scanned in 4s.
`

// v1Example is a v1 file with the deprecated State section.
const v1Example = `<!-- trebi memory v1. Keep these four sections in this order. Do not add other "## " headings. -->
# Memory

## Context
Daily monitor.

## State
- topic A: ok (R 0.41)
- next: re-check topic B tomorrow

## Lessons
- [2026-09-01] The API rate limits at 60 req/min.

## Log
- [2026-09-14 08:00] topic A 0.41. No alert.
`

// head is the marker + H1 prefix shared by hand-built inputs.
const head = markerLine + "\n# Memory\n"

func TestParseSkeleton(t *testing.T) {
	t.Parallel()
	doc, issues := Parse(Skeleton())
	if len(issues) != 0 {
		t.Fatalf("skeleton has issues: %v", issues)
	}
	if doc.Legacy {
		t.Fatal("skeleton parsed as legacy")
	}
	if doc.Context != "" || doc.DeprecatedState != "" || len(doc.Lessons) != 0 || len(doc.Log) != 0 {
		t.Fatalf("skeleton not empty: %+v", doc)
	}
}

func TestParseFilledExample(t *testing.T) {
	t.Parallel()
	doc, issues := Parse([]byte(filledExample))
	if len(issues) != 0 {
		t.Fatalf("filled example has issues: %v", issues)
	}
	wantCtx := "Daily monitor. Sends one email when a topic's R value crosses 0.8.\nTopics come from `topics.yaml` next to this file."
	if doc.Context != wantCtx {
		t.Fatalf("Context = %q", doc.Context)
	}
	if len(doc.Lessons) != 2 || len(doc.Log) != 2 {
		t.Fatalf("counts: lessons %d log %d", len(doc.Lessons), len(doc.Log))
	}
	if doc.Lessons[0].At != "2026-08-02 09:00" || doc.Lessons[0].Line != 9 {
		t.Fatalf("lesson[0] = %+v", doc.Lessons[0])
	}
	if doc.Lessons[1].At != "2026-09-01" || doc.Lessons[1].Line != 10 {
		t.Fatalf("lesson[1] = %+v", doc.Lessons[1])
	}
	if doc.Log[0].At != "2026-09-13 08:00" || doc.Log[0].Line != 13 {
		t.Fatalf("log[0] = %+v", doc.Log[0])
	}
	if doc.Log[1].Text != "topic A 0.41, topic B 0.85. No alert.\nTwo topics scanned in 4s." {
		t.Fatalf("log[1].Text = %q", doc.Log[1].Text)
	}
	// IDs are 8-hex sha256 of section, stamp, text.
	sum := sha256.Sum256([]byte("Log\x002026-09-14 08:00\x00topic A 0.41, topic B 0.85. No alert.\nTwo topics scanned in 4s."))
	if doc.Log[1].ID != hex.EncodeToString(sum[:4]) {
		t.Fatalf("log[1].ID = %s", doc.Log[1].ID)
	}
}

func TestParseLegacyDetection(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"b22 ledger": "# Memory\n\nDated notes follow below.\n\n[2026-09-01] First.\n",
		"empty file": "",
		"only text":  "just some notes\n",
	}
	for name, in := range cases {
		doc, issues := Parse([]byte(in))
		if !doc.Legacy || len(issues) != 0 {
			t.Fatalf("%s: doc=%+v issues=%v", name, doc, issues)
		}
	}
	// No marker but H2 headings: parsed with a no_marker issue, not legacy.
	doc, issues := Parse([]byte("# Memory\n\n## Context\nx\n"))
	if doc.Legacy {
		t.Fatal("H2 file parsed as legacy")
	}
	issueByCode(t, issues, "no_marker")
}

func TestErrorCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		code string
		line int
	}{
		{"no_marker", "# Memory\n\n## Context\nx\n", "no_marker", 1},
		{"stray_heading_head", head + "# Again\n", "stray_heading", 3},
		{"stray_heading_section", head + "## Context\n# X\n", "stray_heading", 4},
		{"stray_text", head + "hello\n", "stray_text", 3},
		{"missing_section", markerLine + "\n## Context\n## Lessons\n", "missing_section", 3},
		{"unknown_section", head + "## Context\n\n## Notes\nmore\n", "unknown_section", 5},
		{"duplicate_section", head + "## Context\n\n## Log\n- [2026-09-01] a\n\n## Log\n- [2026-09-02] b\n", "duplicate_section", 8},
		{"section_order", markerLine + "\n## Context\n\n## Log\n\n## Lessons\n", "section_order", 4},
		{"bad_entry", head + "## Context\n\n## Lessons\n\n## Log\nplain text\n", "bad_entry", 8},
		{"empty_entry", head + "## Context\n\n## Lessons\n\n## Log\n- [2026-09-01] \n", "empty_entry", 8},
		{"continuation_no_entry", head + "## Context\n\n## Lessons\n\n## Log\n  orphan\n", "bad_entry", 8},
		{"state_deprecated", head + "## Context\n\n## State\nx\n\n## Lessons\n\n## Log\n", "state_deprecated", 5},
	}
	for _, tc := range cases {
		_, issues := Parse([]byte(tc.in))
		iss := issueByCode(t, issues, tc.code)
		if iss.Line != tc.line {
			t.Errorf("%s: line = %d, want %d (issues %v)", tc.name, iss.Line, tc.line, issues)
		}
	}
}

func TestVerbatimIssueMessages(t *testing.T) {
	t.Parallel()
	// The bad Log line lands on line 11.
	in := markerLine + "\n# Memory\n\n## Context\nc\n\n## Lessons\n- [2026-09-01] l\n\n## Log\nplain text\n"
	_, issues := Parse([]byte(in))
	got := FormatIssues("MEMORY.md", issues)
	want := `MEMORY.md:11: log entry must start with "- [YYYY-MM-DD HH:MM] " or "- [YYYY-MM-DD] "`
	if len(got) == 0 || got[0] != want {
		t.Fatalf("bad_entry message = %q", got)
	}

	_, issues = Parse([]byte(markerLine + "\n## Context\n## Log\n"))
	iss := issueByCode(t, issues, "missing_section")
	if iss.Msg != `missing section "## Lessons" (sections in order: Context, Lessons, Log)` || iss.Line != 3 {
		t.Fatalf("missing_section = %+v", iss)
	}

	issues = Validate([]byte(filledExample), Options{MaxLogs: 1})
	iss = issueByCode(t, issues, "log_over_cap")
	if iss.Msg != "Log has 2 entries, cap is 1. Remove the oldest entries." {
		t.Fatalf("log_over_cap = %q", iss.Msg)
	}

	unchanged := FormatIssues("MEMORY.md", []Issue{Unchanged()})
	if unchanged[0] != "MEMORY.md: unchanged this run. Add a Log entry for this run." {
		t.Fatalf("unchanged = %q", unchanged[0])
	}
}

func TestValidateCaps(t *testing.T) {
	t.Parallel()
	if issues := Validate([]byte(filledExample), Options{MaxLogs: 0}); len(issues) != 0 {
		t.Fatalf("max_logs 0 reported cap: %v", issues)
	}
	// Legacy input is never valid.
	issues := Validate([]byte("# Memory\n\n[2026-09-01] x\n"), Options{})
	issueByCode(t, issues, "no_marker")
}

func TestCommentsAndH3(t *testing.T) {
	t.Parallel()
	in := head + "<!-- a comment -->\n\n## Context\n### Subhead\nText.\n<!-- inline note -->\n\n## Lessons\n<!-- wait -->\n- [2026-09-01] lesson\n\n## Log\n- [2026-09-02] log\n<!-- trailing -->\n"
	doc, issues := Parse([]byte(in))
	if len(issues) != 0 {
		t.Fatalf("issues: %v", issues)
	}
	if doc.Context != "### Subhead\nText." {
		t.Fatalf("Context = %q", doc.Context)
	}
	if len(doc.Lessons) != 1 || len(doc.Log) != 1 {
		t.Fatalf("entries: %+v %+v", doc.Lessons, doc.Log)
	}
}

func TestFencedHeadingStillError(t *testing.T) {
	t.Parallel()
	// Documented limit: the parser is line-based, so "## " inside a fence
	// in Context is still a section heading.
	in := head + "## Context\n```\n## not a section\n```\n\n## Lessons\n\n## Log\n"
	_, issues := Parse([]byte(in))
	if issueByCode(t, issues, "unknown_section").Line != 5 {
		t.Fatalf("issues: %v", issues)
	}
}

func TestMigrate(t *testing.T) {
	t.Parallel()
	legacy := "# Memory\n\nDated notes follow below.\nExtra preamble line.\n\n[2026-09-01] First note.\n[2026-09-02] Second.\n  detail line.\n"
	out := Migrate([]byte(legacy))
	doc, issues := Parse(out)
	if len(issues) != 0 {
		t.Fatalf("migrated file has issues: %v (%s)", issues, out)
	}
	if doc.Context != "Dated notes follow below.\nExtra preamble line." {
		t.Fatalf("Context = %q", doc.Context)
	}
	if len(doc.Log) != 2 {
		t.Fatalf("log = %+v", doc.Log)
	}
	if doc.Log[0].At != "2026-09-01" || doc.Log[0].Text != "First note." {
		t.Fatalf("log[0] = %+v", doc.Log[0])
	}
	if doc.Log[1].At != "2026-09-02" || doc.Log[1].Text != "Second.\ndetail line." {
		t.Fatalf("log[1] = %+v", doc.Log[1])
	}
	// Idempotent on v1 input and on unmarked v1-shaped input.
	if again := Migrate(out); string(again) != string(out) {
		t.Fatal("Migrate rewrote a v1 file")
	}
	shaped := []byte("# Memory\n\n## Context\nx\n")
	if again := Migrate(shaped); string(again) != string(shaped) {
		t.Fatal("Migrate rewrote a v1-shaped file")
	}
	// Empty legacy file becomes the skeleton.
	if got := Migrate(nil); string(got) != string(Skeleton()) {
		t.Fatalf("empty migrate = %q", got)
	}
	// Migrated entry ids are the Parse ids (stable across re-parses).
	doc2, _ := Parse(out)
	if doc2.Log[0].ID != doc.Log[0].ID {
		t.Fatal("migrated id not stable")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"filled example": filledExample,
		"skeleton":       string(Skeleton()),
	} {
		doc, issues := Parse([]byte(in))
		if len(issues) != 0 {
			t.Fatalf("%s: issues %v", name, issues)
		}
		if out := string(doc.Marshal()); out != in {
			t.Errorf("%s: round trip mismatch:\n%q\n%q", name, in, out)
		}
	}
	// Trailing whitespace inside Context survives the round trip. Empty
	// sections re-emit their comments, so the input carries them.
	in := markerLine + "\n# Memory\n\n## Context\nline one  \nline two\n\n## Lessons\n" + sectionComments["Lessons"] +
		"\n\n## Log\n" + sectionComments["Log"] + "\n"
	doc, _ := Parse([]byte(in))
	if doc.Context != "line one  \nline two" {
		t.Fatalf("Context = %q", doc.Context)
	}
	if out := string(doc.Marshal()); out != in {
		t.Errorf("whitespace round trip:\n%q\n%q", in, out)
	}
}

func TestAddRemove(t *testing.T) {
	t.Parallel()
	doc := &Doc{}
	stampRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$`)
	lesson := doc.AddLesson("", "back off on 429")
	if !stampRe.MatchString(lesson.At) {
		t.Fatalf("AddLesson stamped %q", lesson.At)
	}
	entry := doc.AddLog("", "scanned 2 topics")
	if !stampRe.MatchString(entry.At) {
		t.Fatalf("AddLog stamped %q", entry.At)
	}
	if len(doc.Lessons) != 1 || len(doc.Log) != 1 {
		t.Fatalf("counts: %+v", doc)
	}
	if !doc.Remove("log", entry.ID) {
		t.Fatal("Remove log by id failed")
	}
	if len(doc.Log) != 0 {
		t.Fatalf("log after remove: %+v", doc.Log)
	}
	if doc.Remove("log", "00000000") {
		t.Fatal("Remove unknown id returned true")
	}
	if doc.Remove("context", lesson.ID) {
		t.Fatal("Remove context returned true")
	}
	// A doc built by hand marshals with comments in empty sections only.
	out := string(doc.Marshal())
	if !strings.Contains(out, "## Log\n"+sectionComments["Log"]) {
		t.Fatalf("empty Log kept no comment:\n%s", out)
	}
	if !strings.Contains(out, "- ["+lesson.At+"] back off on 429") {
		t.Fatalf("lesson entry missing:\n%s", out)
	}
}

func TestRender(t *testing.T) {
	t.Parallel()
	doc, _ := Parse([]byte(filledExample))
	out := Render(doc, Options{InjectLogs: 1})
	if !strings.HasPrefix(out, "## Context\nDaily monitor.") {
		t.Fatalf("prefix:\n%s", out)
	}
	if strings.Contains(out, "## State") {
		t.Fatalf("v2 render has State:\n%s", out)
	}
	// Both lessons render; InjectLogs does not cap Lessons.
	if !strings.Contains(out, "[2026-08-02 09:00]") || !strings.Contains(out, "[2026-09-01]") {
		t.Fatalf("lessons capped or missing:\n%s", out)
	}
	// Log is newest first, capped, with the older count.
	logIdx := strings.Index(out, "## Log\n")
	logBlock := out[logIdx:]
	if !strings.HasPrefix(logBlock, "## Log\n- [2026-09-14 08:00]") {
		t.Fatalf("log not newest first:\n%s", logBlock)
	}
	if !strings.Contains(logBlock, "(1 older entries in MEMORY.md)") {
		t.Fatalf("older count missing:\n%s", logBlock)
	}
	if strings.Contains(logBlock, "2026-09-13 08:00") {
		t.Fatalf("capped entry shown:\n%s", logBlock)
	}
	// Empty doc renders placeholders.
	empty := Render(&Doc{}, Options{InjectLogs: 20})
	if strings.Count(empty, "(empty)") != 3 {
		t.Fatalf("empty render:\n%s", empty)
	}
}

func TestProtocol(t *testing.T) {
	t.Parallel()
	if p := Protocol(Options{MaxLogs: 100}, ProtocolInfo{}); !strings.Contains(p, "Keep Log under 100 entries; prune the oldest.") {
		t.Fatalf("capped protocol: %q", p)
	}
	base := Protocol(Options{}, ProtocolInfo{})
	for _, want := range []string{
		"Log has no entry cap.",
		"Keep its three sections in order: Context, Lessons, Log.",
		"End each run with a Log entry that starts with `Next:`.",
		"Keep records in `trebi state`, not in memory.",
		"Update the file before you print the completion line.",
	} {
		if !strings.Contains(base, want) {
			t.Fatalf("protocol lacks %q: %q", want, base)
		}
	}
	if strings.Contains(base, "deprecated") || strings.Contains(base, "Trim it") {
		t.Fatalf("plain protocol has a notice: %q", base)
	}
	cases := []struct {
		name string
		opt  Options
		info ProtocolInfo
		want []string
		not  []string
	}{
		{"state", Options{}, ProtocolInfo{HasState: true}, []string{"This file has a deprecated State section. Move it this run."}, []string{"Trim it"}},
		{"over cap", Options{MaxBytes: 16384}, ProtocolInfo{PromptBytes: 41 * 1024}, []string{"Memory is 41 KB in the prompt; the cap is 16 KB. Trim it this run."}, []string{"deprecated"}},
		{"under cap", Options{MaxBytes: 16384}, ProtocolInfo{PromptBytes: 100}, nil, []string{"Trim it"}},
		{"cap off", Options{}, ProtocolInfo{PromptBytes: 1 << 20}, nil, []string{"Trim it"}},
	}
	for _, tc := range cases {
		p := Protocol(tc.opt, tc.info)
		for _, w := range tc.want {
			if !strings.Contains(p, w) {
				t.Errorf("%s: lacks %q: %q", tc.name, w, p)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(p, n) {
				t.Errorf("%s: has %q: %q", tc.name, n, p)
			}
		}
	}
	doc, _ := Parse([]byte(v1Example))
	opt := Options{InjectLogs: 20}
	if got := NewProtocolInfo(doc, opt); got.PromptBytes != len(Render(doc, opt)) || !got.HasState {
		t.Fatalf("NewProtocolInfo = %+v", got)
	}
}

func TestDeprecatedState(t *testing.T) {
	t.Parallel()
	doc, issues := Parse([]byte(v1Example))
	if len(issues) != 1 || issues[0].Code != "state_deprecated" || issues[0].Line != 7 {
		t.Fatalf("issues = %v", issues)
	}
	if !strings.Contains(issues[0].Msg, "`trebi state`") || !strings.Contains(issues[0].Msg, `"Next:"`) {
		t.Fatalf("message = %q", issues[0].Msg)
	}
	if doc.DeprecatedState != "- topic A: ok (R 0.41)\n- next: re-check topic B tomorrow" {
		t.Fatalf("DeprecatedState = %q", doc.DeprecatedState)
	}
	// A structured edit keeps the State text until the session moves it.
	doc.AddLog("2026-09-15 08:00", "Next: check topic B.")
	again, issues := Parse(doc.Marshal())
	if again.DeprecatedState != doc.DeprecatedState || len(again.Log) != 2 {
		t.Fatalf("round trip lost data: %+v", again)
	}
	issueByCode(t, issues, "state_deprecated")
	// Render shows the block that the session must move.
	out := Render(doc, Options{InjectLogs: 20})
	if !strings.Contains(out, "## State (deprecated, move it)\n- topic A: ok (R 0.41)") {
		t.Fatalf("render:\n%s", out)
	}
	// A blank State is dropped on the next write.
	blank, _ := Parse([]byte(head + "## Context\n\n## State\n\n## Lessons\n\n## Log\n"))
	if blank.DeprecatedState != "" || strings.Contains(string(blank.Marshal()), "## State") {
		t.Fatalf("blank State kept: %q", blank.Marshal())
	}
}

func TestV1MarkerPasses(t *testing.T) {
	t.Parallel()
	v1 := strings.Replace(filledExample, markerLine,
		`<!-- trebi memory v1. Keep these four sections in this order. Do not add other "## " headings. -->`, 1)
	if issues := Validate([]byte(v1), Options{}); len(issues) != 0 {
		t.Fatalf("v1 marker: %v", issues)
	}
	doc, _ := Parse([]byte(v1))
	if !strings.HasPrefix(string(doc.Marshal()), markerLine) {
		t.Fatal("Marshal does not write the v2 marker")
	}
}

func TestPromptSizeCap(t *testing.T) {
	t.Parallel()
	doc := &Doc{Context: strings.Repeat("c", 2000), DeprecatedState: strings.Repeat("s", 3000)}
	for i := range 100 {
		doc.AddLog(fmt.Sprintf("2026-09-%02d 08:%02d", 1+i/60, i%60), strings.Repeat("l", 200))
	}
	doc.AddLesson("2026-09-01", strings.Repeat("x", 1000))
	data := doc.Marshal()
	opt := Options{InjectLogs: 20, MaxBytes: 1024}
	iss := issueByCode(t, Validate(data, opt), "too_large")
	parsed, _ := Parse(data)
	total := len(Render(parsed, opt))
	if total > len(data)/2 {
		t.Fatalf("render measured the whole Log: %d of %d", total, len(data))
	}
	want := fmt.Sprintf("memory is %d KB in the prompt (Context 2 KB, State 3 KB, Lessons 1 KB, Log 5 KB); the cap is 1 KB. "+
		"Trim it to 1 KB or less, so that the next runs have room. "+
		"Trim it in this order: 1. Log: remove the oldest entries. "+
		"2. Lessons: merge duplicates, make each lesson one short rule, remove lessons that Context or the job file now covers. "+
		"3. Context: move instructions to the job file. Move records to `trebi state`.", (total+1023)/1024)
	if iss.Msg != want {
		t.Fatalf("too_large:\n%s\nwant:\n%s", iss.Msg, want)
	}
	// Twenty Log entries in the prompt fit a cap that the whole file does not.
	opt.MaxBytes = total
	if issues := Validate(data, opt); hasCode(issues, "too_large") {
		t.Fatalf("cap at render size failed: %v", issues)
	}
	if len(data) <= total {
		t.Fatal("file is not larger than the render")
	}
	opt.MaxBytes = 0
	if hasCode(Validate(data, opt), "too_large") {
		t.Fatal("max_bytes 0 reported too_large")
	}
	// No State: the message has no State part.
	doc.DeprecatedState = ""
	iss = issueByCode(t, Validate(doc.Marshal(), Options{InjectLogs: 20, MaxBytes: 1024}), "too_large")
	if strings.Contains(iss.Msg, "State") {
		t.Fatalf("State in message: %q", iss.Msg)
	}
}

func TestTrim(t *testing.T) {
	t.Parallel()
	logDoc := func(n int, lesson string) *Doc {
		doc := &Doc{Context: "ctx"}
		doc.AddLesson("2026-09-01", lesson)
		for i := range n {
			doc.AddLog(fmt.Sprintf("2026-09-%02d 08:00", 1+i), strings.Repeat("l", 100))
		}
		return doc
	}
	t.Run("entry cap", func(t *testing.T) {
		t.Parallel()
		opt := Options{MaxLogs: 3, InjectLogs: 20}
		out, n := Trim(logDoc(5, "x").Marshal(), opt)
		if n != 2 {
			t.Fatalf("removed %d, want 2", n)
		}
		doc, _ := Parse(out)
		if len(doc.Log) != 3 || doc.Log[0].At != "2026-09-03 08:00" {
			t.Fatalf("log %+v", doc.Log)
		}
		if Validate(out, opt) != nil {
			t.Fatalf("issues after trim: %v", Validate(out, opt))
		}
	})
	t.Run("size", func(t *testing.T) {
		t.Parallel()
		data := logDoc(10, "x").Marshal()
		parsed, _ := Parse(data)
		opt := Options{InjectLogs: 20, MaxBytes: len(Render(parsed, Options{InjectLogs: 20})) - 150}
		out, n := Trim(data, opt)
		if n != 2 {
			t.Fatalf("removed %d, want 2", n)
		}
		if Validate(out, opt) != nil {
			t.Fatalf("issues after trim: %v", Validate(out, opt))
		}
	})
	t.Run("log cannot fix size", func(t *testing.T) {
		t.Parallel()
		data := logDoc(10, strings.Repeat("x", 4000)).Marshal()
		out, n := Trim(data, Options{InjectLogs: 20, MaxBytes: 2048})
		if n != 0 || string(out) != string(data) {
			t.Fatalf("trimmed %d entries for a size that Log cannot fix", n)
		}
	})
	t.Run("keeps newest entry", func(t *testing.T) {
		t.Parallel()
		data := logDoc(3, "x").Marshal()
		parsed, _ := Parse(data)
		one := &Doc{Context: parsed.Context, Lessons: parsed.Lessons, Log: parsed.Log[2:]}
		opt := Options{InjectLogs: 20, MaxBytes: len(Render(one, Options{InjectLogs: 20}))}
		out, n := Trim(data, opt)
		doc, _ := Parse(out)
		if n != 2 || len(doc.Log) != 1 || doc.Log[0].At != "2026-09-03 08:00" {
			t.Fatalf("n %d log %+v", n, doc.Log)
		}
	})
	t.Run("damaged structure", func(t *testing.T) {
		t.Parallel()
		data := []byte(head + "\n## Context\nx\n\n## Lessons\nnot an entry\n\n## Log\n- [2026-09-01] a\n- [2026-09-02] b\n")
		out, n := Trim(data, Options{MaxLogs: 1})
		if n != 0 || string(out) != string(data) {
			t.Fatalf("trimmed a damaged file: %d", n)
		}
	})
}

func TestHash(t *testing.T) {
	t.Parallel()
	data := []byte("hello memory")
	sum := sha256.Sum256(data)
	if got := Hash(data); got != hex.EncodeToString(sum[:16]) {
		t.Fatalf("Hash = %s", got)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(filledExample))
	f.Add(Skeleton())
	f.Add([]byte("# Memory\n\n[2026-09-01] x\n"))
	f.Add([]byte("## Log\n- [2026-09-01]\n  cont\n\x00\xff"))
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, _ := Parse(data)
		_ = Validate(data, Options{MaxLogs: 5, MaxBytes: 4096})
		mig := Migrate(data)
		_, _ = Parse(mig)
		_ = Render(doc, Options{InjectLogs: 2})
		_ = doc.Marshal()
	})
}

func issueByCode(t *testing.T, issues []Issue, code string) Issue {
	t.Helper()
	for _, iss := range issues {
		if iss.Code == code {
			return iss
		}
	}
	t.Fatalf("issue %q not found in %v", code, issues)
	return Issue{}
}

func hasCode(issues []Issue, code string) bool {
	for _, iss := range issues {
		if iss.Code == code {
			return true
		}
	}
	return false
}
