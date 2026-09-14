package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// filledExample is the plan's filled file, byte for byte.
const filledExample = `<!-- trebi memory v1. Keep these four sections in this order. Do not add other "## " headings. -->
# Memory

## Context
Daily monitor. Sends one email when a topic's R value crosses 0.8.
Topics come from ` + "`topics.yaml`" + ` next to this file.

## State
- topic A: ok (R 0.41)
- topic B: warn since 2026-09-13, alerted once
- next: re-check topic B tomorrow, no new alert unless R > 0.9

## Lessons
- [2026-08-02 09:00] Skip duplicate alerts within 24h.
- [2026-09-01] The API rate limits at 60 req/min. Back off 10s.

## Log
- [2026-09-13 08:00] topic B crossed 0.8 (R 0.83). Alerted.
- [2026-09-14 08:00] topic A 0.41, topic B 0.85. No alert.
  Two topics scanned in 4s.
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
	if doc.Context != "" || doc.State != "" || len(doc.Lessons) != 0 || len(doc.Log) != 0 {
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
	wantState := "- topic A: ok (R 0.41)\n- topic B: warn since 2026-09-13, alerted once\n- next: re-check topic B tomorrow, no new alert unless R > 0.9"
	if doc.State != wantState {
		t.Fatalf("State = %q", doc.State)
	}
	if len(doc.Lessons) != 2 || len(doc.Log) != 2 {
		t.Fatalf("counts: lessons %d log %d", len(doc.Lessons), len(doc.Log))
	}
	if doc.Lessons[0].At != "2026-08-02 09:00" || doc.Lessons[0].Line != 14 {
		t.Fatalf("lesson[0] = %+v", doc.Lessons[0])
	}
	if doc.Lessons[1].At != "2026-09-01" || doc.Lessons[1].Line != 15 {
		t.Fatalf("lesson[1] = %+v", doc.Lessons[1])
	}
	if doc.Log[0].At != "2026-09-13 08:00" || doc.Log[0].Line != 18 {
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
		{"section_order", markerLine + "\n## Context\n\n## Log\n\n## State\n\n## Lessons\n", "section_order", 4},
		{"bad_entry", head + "## Context\n\n## State\n\n## Lessons\n\n## Log\nplain text\n", "bad_entry", 10},
		{"empty_entry", head + "## Context\n\n## State\n\n## Lessons\n\n## Log\n- [2026-09-01] \n", "empty_entry", 10},
		{"continuation_no_entry", head + "## Context\n\n## State\n\n## Lessons\n\n## Log\n  orphan\n", "bad_entry", 10},
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
	// Plan example position: the bad Log line lands on line 14.
	in := markerLine + "\n# Memory\n\n## Context\nc\n\n## State\ns\n\n## Lessons\n- [2026-09-01] l\n\n## Log\nplain text\n"
	_, issues := Parse([]byte(in))
	got := FormatIssues("MEMORY.md", issues)
	want := `MEMORY.md:14: log entry must start with "- [YYYY-MM-DD HH:MM] " or "- [YYYY-MM-DD] "`
	if len(got) == 0 || got[0] != want {
		t.Fatalf("bad_entry message = %q", got)
	}

	_, issues = Parse([]byte(markerLine + "\n## Context\n## Lessons\n"))
	iss := issueByCode(t, issues, "missing_section")
	if iss.Msg != `missing section "## State" (sections in order: Context, State, Lessons, Log)` || iss.Line != 3 {
		t.Fatalf("missing_section = %+v", iss)
	}

	issues = Validate([]byte(filledExample), Options{MaxLogs: 1})
	iss = issueByCode(t, issues, "log_over_cap")
	if iss.Msg != "Log has 2 entries, cap is 1. Remove the oldest entries." {
		t.Fatalf("log_over_cap = %q", iss.Msg)
	}

	unchanged := FormatIssues("MEMORY.md", []Issue{Unchanged()})
	if unchanged[0] != "MEMORY.md: unchanged this run. Update State, and Log if anything happened." {
		t.Fatalf("unchanged = %q", unchanged[0])
	}
}

func TestValidateCaps(t *testing.T) {
	t.Parallel()
	if issues := Validate([]byte(filledExample), Options{MaxLogs: 0}); len(issues) != 0 {
		t.Fatalf("max_logs 0 reported cap: %v", issues)
	}
	issues := Validate([]byte(filledExample), Options{MaxBytes: 10})
	wantMsg := fmt.Sprintf("file is %d bytes, cap is 10.", len(filledExample))
	if issueByCode(t, issues, "too_large").Msg != wantMsg {
		t.Fatalf("too_large message = %v", issues)
	}
	// Legacy input is never valid.
	issues = Validate([]byte("# Memory\n\n[2026-09-01] x\n"), Options{})
	issueByCode(t, issues, "no_marker")
}

func TestCommentsAndH3(t *testing.T) {
	t.Parallel()
	in := head + "<!-- a comment -->\n\n## Context\n### Subhead\nText.\n<!-- inline note -->\n\n## State\n\n## Lessons\n<!-- wait -->\n- [2026-09-01] lesson\n\n## Log\n- [2026-09-02] log\n<!-- trailing -->\n"
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
	in := head + "## Context\n```\n## not a section\n```\n\n## State\n\n## Lessons\n\n## Log\n"
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
	in := markerLine + "\n# Memory\n\n## Context\nline one  \nline two\n\n## State\n" +
		sectionComments["State"] + "\n\n## Lessons\n" + sectionComments["Lessons"] +
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
	if !strings.Contains(out, "## State\n- topic A: ok (R 0.41)") {
		t.Fatalf("state missing:\n%s", out)
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
	if strings.Count(empty, "(empty)") != 4 {
		t.Fatalf("empty render:\n%s", empty)
	}
}

func TestProtocol(t *testing.T) {
	t.Parallel()
	if p := Protocol(Options{MaxLogs: 100}); !strings.Contains(p, "Keep Log under 100 entries; prune the oldest.") {
		t.Fatalf("capped protocol: %q", p)
	}
	if p := Protocol(Options{}); !strings.Contains(p, "Log has no entry cap.") {
		t.Fatalf("uncapped protocol: %q", p)
	}
	if !strings.Contains(Protocol(Options{}), "Update the file before you print the completion line.") {
		t.Fatal("protocol lacks the completion-line instruction")
	}
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
