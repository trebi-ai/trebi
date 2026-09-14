// Package memory parses, validates, and renders the structured MEMORY.md
// file (v1): four fixed H2 sections (Context, State, Lessons, Log) with
// stamped bullet entries in Lessons and Log. The parser is line-based and
// shape-only; it never rewrites the model's entries.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Options are the effective memory options after config defaults.
type Options struct {
	MaxLogs       int  `json:"max_logs"`       // 0 = no entry cap
	RequireUpdate bool `json:"require_update"` // file must change each run
	InjectLogs    int  `json:"inject_logs"`    // Log entries in the launch prompt; 0 = none
	MaxBytes      int  `json:"max_bytes"`      // files cap for the too_large check; 0 = no byte check
}

// Entry is one stamped bullet in Lessons or Log.
type Entry struct {
	ID   string `json:"id"`
	At   string `json:"at"`   // "YYYY-MM-DD" or "YYYY-MM-DD HH:MM" as written
	Text string `json:"text"` // continuation lines joined with "\n"
	Line int    `json:"line"`
}

// Doc is the parsed memory file.
type Doc struct {
	Context string  `json:"context"`
	State   string  `json:"state"`
	Lessons []Entry `json:"lessons"`
	Log     []Entry `json:"log"`
	Legacy  bool    `json:"-"` // parsed from a B22 ledger file
}

// Issue is one validation finding. Msg carries no file or line prefix;
// FormatIssues renders the one-line form shared by the gate, API, and CLI.
type Issue struct {
	Code string `json:"code"`
	Line int    `json:"line,omitempty"`
	Msg  string `json:"msg"`
}

const (
	markerLine   = `<!-- trebi memory v1. Keep these four sections in this order. Do not add other "## " headings. -->`
	markerPrefix = "<!-- trebi memory v1"
	sectionsHint = "(sections in order: Context, State, Lessons, Log)"
	entryShape   = `"- [YYYY-MM-DD HH:MM] " or "- [YYYY-MM-DD] "`
)

// sectionNames is the fixed H2 order.
var sectionNames = []string{"Context", "State", "Lessons", "Log"} //nolint:gochecknoglobals // static data table, never mutated

// sectionComments are re-emitted by Marshal when a section is empty.
var sectionComments = map[string]string{ //nolint:gochecknoglobals // static data table, never mutated
	"Context": "<!-- Durable facts about this job: what it does, where things are, how they work. Free form. Rewrite when facts change. -->",
	"State":   "<!-- Current situation, open issues, next steps. Free form. Rewrite each run. -->",
	"Lessons": "<!-- One bullet per lesson: `- [YYYY-MM-DD HH:MM] text`. Never prune by age. -->",
	"Log":     "<!-- One bullet per event, observation, or stat: `- [YYYY-MM-DD HH:MM] text`. Prune the oldest entries when over the cap. -->",
}

// entryRe is the entry shape: date-only or date+time stamp, any text.
// Calendar validity is not checked ("[2026-13-99]" parses).
var entryRe = regexp.MustCompile(`^- \[(\d{4}-\d{2}-\d{2})(?: (\d{2}:\d{2}))?\] (.*)$`)

// legacyEntryRe is the B22 ledger line; Migrate carries it.
var legacyEntryRe = regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2})\] ?(.*)$`)

// stampRe is a bare entry stamp, without the "- " and text.
var stampRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}( \d{2}:\d{2})?$`)

// ValidStamp reports whether at fits "YYYY-MM-DD" or "YYYY-MM-DD HH:MM".
// Shape only; calendar validity is not checked.
func ValidStamp(at string) bool { return stampRe.MatchString(at) }

// Parse returns the document and its structure issues. A file with no v1
// marker and no H2 headings is a legacy B22 ledger: it returns Doc.Legacy
// and no issues. The parser is line-based; comments are ignored everywhere.
func Parse(data []byte) (*Doc, []Issue) {
	lines := splitLines(data)
	offset := 1 // line number of lines[0] after the marker is consumed
	if len(lines) > 0 && strings.HasPrefix(lines[0], markerPrefix) {
		lines = lines[1:]
		offset = 2
	} else if !hasSectionHeading(lines) {
		// No marker and no H2 headings: legacy B22 ledger.
		return &Doc{Legacy: true, Lessons: []Entry{}, Log: []Entry{}}, nil
	}
	p := &parseState{seen: map[string]bool{}, orderLines: map[string]int{}, openLesson: -1, openLog: -1}
	if offset == 1 {
		p.issues = append(p.issues, Issue{Code: "no_marker", Line: 1,
			Msg: `missing v1 marker "<!-- trebi memory v1" on line 1`})
	}
	for i, line := range lines {
		p.line(i+offset, line)
	}
	return p.finish()
}

// Validate runs Parse plus the Log cap and the byte-size cap.
// It returns nil when the file is valid.
func Validate(data []byte, opt Options) []Issue {
	doc, issues := Parse(data)
	if doc.Legacy {
		issues = append(issues, Issue{Code: "no_marker", Line: 1,
			Msg: `missing v1 marker "<!-- trebi memory v1" on line 1`})
	}
	if opt.MaxLogs > 0 && len(doc.Log) > opt.MaxLogs {
		issues = append(issues, Issue{Code: "log_over_cap",
			Msg: fmt.Sprintf("Log has %d entries, cap is %d. Remove the oldest entries.", len(doc.Log), opt.MaxLogs)})
	}
	if opt.MaxBytes > 0 && len(data) > opt.MaxBytes {
		issues = append(issues, Issue{Code: "too_large",
			Msg: fmt.Sprintf("file is %d bytes, cap is %d.", len(data), opt.MaxBytes)})
	}
	return issues
}

// Skeleton returns the seeded file: marker, H1, four empty sections with
// their rule comments.
func Skeleton() []byte {
	var b strings.Builder
	b.WriteString(markerLine + "\n# Memory\n")
	for _, name := range sectionNames {
		b.WriteString("\n## " + name + "\n" + sectionComments[name] + "\n")
	}
	return []byte(b.String())
}

// Migrate converts a legacy B22 ledger to v1: preamble to Context, dated
// lines to Log with the same date, State and Lessons empty. Leading "# "
// heading lines in the preamble are dropped; v1 supplies its own H1. Input
// that is already v1 (or v1-shaped) returns unchanged. An empty file
// returns the skeleton.
func Migrate(legacy []byte) []byte {
	lines := splitLines(legacy)
	if len(lines) > 0 && (strings.HasPrefix(lines[0], markerPrefix) || hasSectionHeading(lines)) {
		return legacy
	}
	var preamble, cur []string
	doc := &Doc{Lessons: []Entry{}, Log: []Entry{}}
	flush := func() {
		if len(cur) == 0 {
			return
		}
		doc.Log = append(doc.Log, Entry{At: cur[0], Text: strings.Join(cur[1:], "\n")})
		cur = nil
	}
	for _, line := range lines {
		if m := legacyEntryRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = []string{m[1], m[2]}
			continue
		}
		if len(cur) > 0 {
			cur = append(cur, strings.TrimLeft(line, " "))
			continue
		}
		preamble = append(preamble, line)
	}
	flush()
	for len(preamble) > 0 && strings.HasPrefix(preamble[0], "# ") {
		preamble = preamble[1:]
	}
	doc.Context = strings.TrimSpace(strings.Join(preamble, "\n"))
	return doc.Marshal()
}

// Render builds the Memory prompt section: full Context and State, all
// Lessons, and the newest InjectLogs Log entries plus a count of older ones.
// Entries print newest first; empty sections print "(empty)".
func Render(doc *Doc, opt Options) string {
	if doc == nil {
		doc = &Doc{}
	}
	log := sortedEntries(doc.Log)
	shown, older := log, 0
	if opt.InjectLogs >= 0 && len(log) > opt.InjectLogs {
		shown, older = log[:opt.InjectLogs], len(log)-opt.InjectLogs
	}
	var b strings.Builder
	b.WriteString("## Context\n" + bodyOrEmpty(doc.Context) + "\n\n")
	b.WriteString("## State\n" + bodyOrEmpty(doc.State) + "\n\n")
	b.WriteString("## Lessons\n" + entriesBlock(sortedEntries(doc.Lessons), 0) + "\n\n")
	b.WriteString("## Log\n" + entriesBlock(shown, older))
	return b.String()
}

// Protocol returns the rules paragraph for enabled launches.
func Protocol(opt Options) string {
	capRule := "Log has no entry cap."
	if opt.MaxLogs > 0 {
		capRule = fmt.Sprintf("Keep Log under %d entries; prune the oldest.", opt.MaxLogs)
	}
	return "MEMORY.md is next to your job file and is shown above. " +
		"Keep its four sections in order: Context, State, Lessons, Log. " +
		"Context and State are free form. " +
		"Lessons and Log take one bullet per entry in the form `- [YYYY-MM-DD HH:MM] text` (`- [YYYY-MM-DD] text` is accepted). " +
		"Rewrite State each run. Add Log entries for events, observations, and stats. " +
		capRule + " Never prune Lessons by age. Update the file before you print the completion line."
}

// Hash is the files ContentHash of the file bytes.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// Info is the wire payload of one MEMORY.md: the raw file, its parsed doc,
// validation issues, and the options that shaped them.
type Info struct {
	SizeBytes int64     `json:"size_bytes"`
	UpdatedAt time.Time `json:"updated_at"`
	Hash      string    `json:"hash"`
	Content   string    `json:"content"`
	Doc       *Doc      `json:"doc"`
	Issues    []Issue   `json:"issues"`
	Legacy    bool      `json:"legacy"`
	Options   Options   `json:"options"`
}

// Inspect builds Info from file bytes validated with opt. A missing file
// passes Skeleton() bytes, a zero time, and an empty hash.
func Inspect(data []byte, opt Options, at time.Time) *Info {
	doc, _ := Parse(data)
	return &Info{
		SizeBytes: int64(len(data)), UpdatedAt: at, Hash: Hash(data),
		Content: string(data), Doc: doc, Issues: Validate(data, opt),
		Legacy: doc.Legacy, Options: opt,
	}
}

// Unchanged is the freshness issue for a file not rewritten this run.
func Unchanged() Issue {
	return Issue{Code: "unchanged", Msg: "unchanged this run. Update State, and Log if anything happened."}
}

// FormatIssues renders issues one per line: "MEMORY.md:14: msg", or
// "MEMORY.md: msg" when the issue carries no line.
func FormatIssues(file string, issues []Issue) []string {
	out := make([]string, 0, len(issues))
	for _, iss := range issues {
		if iss.Line > 0 {
			out = append(out, fmt.Sprintf("%s:%d: %s", file, iss.Line, iss.Msg))
		} else {
			out = append(out, fmt.Sprintf("%s: %s", file, iss.Msg))
		}
	}
	return out
}

// Marshal re-emits the file: Context and State verbatim, Lessons and Log
// from entries, skeleton comments only for empty sections.
func (d *Doc) Marshal() []byte {
	var b strings.Builder
	b.WriteString(markerLine + "\n# Memory\n")
	b.WriteString("\n## Context\n" + bodyOrComment(d.Context, "Context"))
	b.WriteString("\n## State\n" + bodyOrComment(d.State, "State"))
	b.WriteString("\n## Lessons\n" + entriesOrComment(d.Lessons, "Lessons"))
	b.WriteString("\n## Log\n" + entriesOrComment(d.Log, "Log"))
	return []byte(b.String())
}

// SetContext replaces the Context body.
func (d *Doc) SetContext(body string) { d.Context = strings.TrimRight(body, "\n") }

// SetState replaces the State body.
func (d *Doc) SetState(body string) { d.State = strings.TrimRight(body, "\n") }

// AddLesson appends a lesson; an empty at stamps now UTC to the minute.
func (d *Doc) AddLesson(at, text string) Entry { return d.addEntry("Lessons", &d.Lessons, at, text) }

// AddLog appends a log entry; an empty at stamps now UTC to the minute.
func (d *Doc) AddLog(at, text string) Entry { return d.addEntry("Log", &d.Log, at, text) }

// Remove drops the entry with id from Lessons or Log. Unknown: false.
func (d *Doc) Remove(section, id string) bool {
	switch strings.ToLower(strings.TrimSpace(section)) {
	case "lessons":
		return removeEntry(&d.Lessons, id)
	case "log":
		return removeEntry(&d.Log, id)
	}
	return false
}

func (d *Doc) addEntry(section string, list *[]Entry, at, text string) Entry {
	if at == "" {
		at = time.Now().UTC().Format("2006-01-02 15:04")
	}
	e := Entry{At: at, Text: text, ID: entryID(section, at, text)}
	*list = append(*list, e)
	return e
}

func removeEntry(list *[]Entry, id string) bool {
	for i, e := range *list {
		if e.ID == id {
			*list = append((*list)[:i], (*list)[i+1:]...)
			return true
		}
	}
	return false
}

// entryID is the stable short id: 8-hex sha256 of section, stamp, text.
// Stable across reorders and re-parses.
func entryID(section, at, text string) string {
	sum := sha256.Sum256([]byte(section + "\x00" + at + "\x00" + text))
	return hex.EncodeToString(sum[:4])
}

// parseState walks the file once.
type parseState struct {
	issues     []Issue
	section    string // "", section name, or "unknown"
	ctx, state []string
	lessons    []Entry
	log        []Entry
	seen       map[string]bool
	order      []string
	orderLines map[string]int
	h1         bool
	openLesson int // index of the entry open for continuations; -1 none
	openLog    int
}

func (p *parseState) line(n int, line string) {
	if strings.HasPrefix(line, "## ") {
		p.heading(n, strings.TrimSpace(line[3:]))
		return
	}
	switch p.section {
	case "":
		p.headLine(n, line)
	case "Context":
		p.bodyLine(n, line, &p.ctx)
	case "State":
		p.bodyLine(n, line, &p.state)
	case "Lessons":
		p.entryLine(n, line, "Lessons", &p.lessons, &p.openLesson)
	case "Log":
		p.entryLine(n, line, "Log", &p.log, &p.openLog)
	}
	// Content of an unknown section is skipped; the heading was reported.
}

func (p *parseState) headLine(n int, line string) {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "", strings.HasPrefix(trimmed, "<!--"):
		return
	case strings.HasPrefix(line, "# "):
		if p.h1 {
			p.issues = append(p.issues, Issue{Code: "stray_heading", Line: n,
				Msg: `only one "# " heading is allowed`})
			return
		}
		p.h1 = true
	default:
		p.issues = append(p.issues, Issue{Code: "stray_text", Line: n,
			Msg: `text before "## Context" must be blank, a comment, or "# Memory"`})
	}
}

func (p *parseState) bodyLine(n int, line string, buf *[]string) {
	if strings.HasPrefix(line, "# ") {
		p.issues = append(p.issues, Issue{Code: "stray_heading", Line: n,
			Msg: `stray "# " heading inside a section`})
		return
	}
	if strings.HasPrefix(strings.TrimSpace(line), "<!--") {
		return
	}
	*buf = append(*buf, line)
}

func (p *parseState) entryLine(n int, line, section string, list *[]Entry, open *int) {
	if strings.HasPrefix(strings.TrimSpace(line), "<!--") {
		return
	}
	if strings.HasPrefix(line, "  ") {
		if *open < 0 {
			p.badEntry(n, section)
			return
		}
		e := &(*list)[*open]
		e.Text += "\n" + strings.TrimLeft(line, " ")
		return
	}
	if m := entryRe.FindStringSubmatch(line); m != nil {
		at := m[1]
		if m[2] != "" {
			at += " " + m[2]
		}
		if m[3] == "" {
			p.issues = append(p.issues, Issue{Code: "empty_entry", Line: n,
				Msg: "entry text after the stamp is empty"})
		}
		*list = append(*list, Entry{At: at, Text: m[3], Line: n})
		*open = len(*list) - 1
		return
	}
	if strings.TrimSpace(line) == "" {
		return
	}
	p.badEntry(n, section)
}

func (p *parseState) badEntry(n int, section string) {
	p.issues = append(p.issues, Issue{Code: "bad_entry", Line: n,
		Msg: fmt.Sprintf("%s entry must start with %s", strings.ToLower(section), entryShape)})
}

func (p *parseState) heading(n int, title string) {
	if !knownSection(title) {
		p.section = "unknown"
		p.issues = append(p.issues, Issue{Code: "unknown_section", Line: n,
			Msg: fmt.Sprintf("unknown section %q %s", title, sectionsHint)})
		return
	}
	if p.seen[title] {
		p.section = title // merge content into the first occurrence
		p.issues = append(p.issues, Issue{Code: "duplicate_section", Line: n,
			Msg: fmt.Sprintf("duplicate section %q", "## "+title)})
		return
	}
	p.seen[title] = true
	p.order = append(p.order, title)
	p.orderLines[title] = n
	p.section = title
}

func (p *parseState) finish() (*Doc, []Issue) {
	p.checkOrder()
	p.checkMissing()
	for i := range p.lessons {
		p.lessons[i].ID = entryID("Lessons", p.lessons[i].At, p.lessons[i].Text)
	}
	for i := range p.log {
		p.log[i].ID = entryID("Log", p.log[i].At, p.log[i].Text)
	}
	if p.lessons == nil {
		p.lessons = []Entry{}
	}
	if p.log == nil {
		p.log = []Entry{}
	}
	return &Doc{
		Context: trimBody(p.ctx), State: trimBody(p.state),
		Lessons: p.lessons, Log: p.log,
	}, p.issues
}

// checkOrder reports one section_order at the first heading that breaks the
// canonical order.
func (p *parseState) checkOrder() {
	var expected []string
	for _, name := range sectionNames {
		if p.seen[name] {
			expected = append(expected, name)
		}
	}
	for i, name := range p.order {
		if i >= len(expected) || expected[i] != name {
			p.issues = append(p.issues, Issue{Code: "section_order", Line: p.orderLines[name],
				Msg: fmt.Sprintf("section %q is out of order %s", "## "+name, sectionsHint)})
			return
		}
	}
}

// checkMissing reports each absent section at the next present heading.
func (p *parseState) checkMissing() {
	for idx, name := range sectionNames {
		if p.seen[name] {
			continue
		}
		p.issues = append(p.issues, Issue{Code: "missing_section", Line: p.missingLine(idx),
			Msg: fmt.Sprintf("missing section %q %s", "## "+name, sectionsHint)})
	}
}

func (p *parseState) missingLine(idx int) int {
	for j := idx + 1; j < len(sectionNames); j++ {
		if n, ok := p.orderLines[sectionNames[j]]; ok {
			return n
		}
	}
	for j := len(sectionNames) - 1; j >= 0; j-- {
		if n, ok := p.orderLines[sectionNames[j]]; ok {
			return n
		}
	}
	return 1
}

func knownSection(title string) bool {
	for _, name := range sectionNames {
		if title == name {
			return true
		}
	}
	return false
}

func hasSectionHeading(lines []string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			return true
		}
	}
	return false
}

// splitLines drops the one empty element after a final newline.
func splitLines(data []byte) []string {
	s := strings.Split(string(data), "\n")
	if len(s) > 0 && s[len(s)-1] == "" {
		s = s[:len(s)-1]
	}
	return s
}

// trimBody joins captured lines and drops trailing blank lines; interior
// lines stay verbatim including trailing whitespace.
func trimBody(lines []string) string {
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func bodyOrComment(body, section string) string {
	if strings.TrimSpace(body) == "" {
		return sectionComments[section] + "\n"
	}
	return body + "\n"
}

func entriesOrComment(entries []Entry, section string) string {
	if len(entries) == 0 {
		return sectionComments[section] + "\n"
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(formatEntry(e))
	}
	return b.String()
}

// formatEntry renders one entry: stamp line plus two-space continuations.
func formatEntry(e Entry) string {
	text := strings.Split(e.Text, "\n")
	var b strings.Builder
	b.WriteString("- [" + e.At + "] " + text[0] + "\n")
	for _, line := range text[1:] {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

func bodyOrEmpty(body string) string {
	if strings.TrimSpace(body) == "" {
		return "(empty)"
	}
	return body
}

func entriesBlock(entries []Entry, older int) string {
	if len(entries) == 0 && older == 0 {
		return "(empty)"
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(formatEntry(e))
	}
	if older > 0 {
		fmt.Fprintf(&b, "(%d older entries in MEMORY.md)\n", older)
	}
	return strings.TrimRight(b.String(), "\n")
}

// sortedEntries returns a newest-first copy (stamp strings sort chronologically).
func sortedEntries(entries []Entry) []Entry {
	out := append([]Entry{}, entries...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}
