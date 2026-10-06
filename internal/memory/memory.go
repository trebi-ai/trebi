// Package memory parses, validates, and renders the structured MEMORY.md
// file (v2): three fixed H2 sections (Context, Lessons, Log) with stamped
// bullet entries in Lessons and Log. A v1 State section is deprecated: the
// parser keeps its text and reports it. The parser is line-based and
// shape-only; it never rewrites the model's entries.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Options are the effective memory options after config defaults.
type Options struct {
	MaxLogs       int  `json:"max_logs"`       // 0 = no entry cap
	RequireUpdate bool `json:"require_update"` // file must change each run
	InjectLogs    int  `json:"inject_logs"`    // Log entries in the launch prompt; 0 = none
	MaxBytes      int  `json:"max_bytes"`      // cap on len(Render) for too_large; 0 = no cap
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
	Context         string  `json:"context"`
	Lessons         []Entry `json:"lessons"`
	Log             []Entry `json:"log"`
	DeprecatedState string  `json:"deprecated_state,omitempty"` // v1 State body, kept until the session moves it
	Legacy          bool    `json:"-"`                          // parsed from a B22 ledger file
}

// Issue is one validation finding. Msg carries no file or line prefix;
// FormatIssues renders the one-line form shared by the gate, API, and CLI.
type Issue struct {
	Code string `json:"code"`
	Line int    `json:"line,omitempty"`
	Msg  string `json:"msg"`
}

const (
	markerLine   = `<!-- trebi memory v2. Keep these three sections in this order. Do not add other "## " headings. -->`
	markerPrefix = "<!-- trebi memory v"
	markerMsg    = `missing marker "<!-- trebi memory v2" on line 1`
	sectionsHint = "(sections in order: Context, Lessons, Log)"
	entryShape   = `"- [YYYY-MM-DD HH:MM] " or "- [YYYY-MM-DD] "`
	stateTitle   = "State" // deprecated v1 section
)

// sectionNames is the fixed H2 order.
var sectionNames = []string{"Context", "Lessons", "Log"} //nolint:gochecknoglobals // static data table, never mutated

// sectionComments are re-emitted by Marshal when a section is empty.
var sectionComments = map[string]string{ //nolint:gochecknoglobals // static data table, never mutated
	"Context": "<!-- Durable facts about this job: what it does, where things are, how they work. Free form. Rewrite when facts change. -->",
	"Lessons": "<!-- One bullet per lesson: `- [YYYY-MM-DD HH:MM] text`. Never prune by age. -->",
	"Log":     "<!-- One bullet per event, observation, or stat: `- [YYYY-MM-DD HH:MM] text`. End each run with a `Next:` entry for the next run. Prune the oldest entries when over the cap. -->",
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

// Parse returns the document and its structure issues. A file with no
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
		p.issues = append(p.issues, Issue{Code: "no_marker", Line: 1, Msg: markerMsg})
	}
	for i, line := range lines {
		p.line(i+offset, line)
	}
	return p.finish()
}

// Validate runs Parse plus the Log cap and the prompt-size cap.
// It returns nil when the file is valid.
func Validate(data []byte, opt Options) []Issue {
	doc, issues := Parse(data)
	if doc.Legacy {
		issues = append(issues, Issue{Code: "no_marker", Line: 1, Msg: markerMsg})
	}
	if opt.MaxLogs > 0 && len(doc.Log) > opt.MaxLogs {
		issues = append(issues, Issue{Code: "log_over_cap",
			Msg: fmt.Sprintf("Log has %d entries, cap is %d. Remove the oldest entries.", len(doc.Log), opt.MaxLogs)})
	}
	if iss, ok := doc.sizeIssue(opt); ok {
		issues = append(issues, iss)
	}
	return issues
}

// Skeleton returns the seeded file: marker, H1, three empty sections with
// their rule comments.
func Skeleton() []byte {
	var b strings.Builder
	b.WriteString(markerLine + "\n# Memory\n")
	for _, name := range sectionNames {
		b.WriteString("\n## " + name + "\n" + sectionComments[name] + "\n")
	}
	return []byte(b.String())
}

// Migrate converts a legacy B22 ledger to v2: preamble to Context, dated
// lines to Log with the same date, Lessons empty. Leading "# " heading
// lines in the preamble are dropped; v2 supplies its own H1. Input that
// already has a marker or "## " headings returns unchanged. An empty file
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

// Render builds the Memory prompt section: full Context, all Lessons, and
// the newest InjectLogs Log entries plus a count of older ones. A deprecated
// State prints after Context, so that the session sees what it must move.
// Entries print newest first; empty sections print "(empty)".
func Render(doc *Doc, opt Options) string {
	if doc == nil {
		doc = &Doc{}
	}
	shown := shownLog(doc.Log, opt)
	var b strings.Builder
	b.WriteString("## Context\n" + bodyOrEmpty(doc.Context) + "\n\n")
	if doc.DeprecatedState != "" {
		b.WriteString("## State (deprecated, move it)\n" + doc.DeprecatedState + "\n\n")
	}
	b.WriteString("## Lessons\n" + entriesBlock(sortedEntries(doc.Lessons), 0) + "\n\n")
	b.WriteString("## Log\n" + entriesBlock(shown, len(doc.Log)-len(shown)))
	return b.String()
}

// ProtocolInfo is the launch data that Protocol adds to the rules.
type ProtocolInfo struct {
	PromptBytes int  // bytes of Render output
	HasState    bool // Doc.DeprecatedState != ""
}

// NewProtocolInfo measures doc for Protocol.
func NewProtocolInfo(doc *Doc, opt Options) ProtocolInfo {
	if doc == nil {
		doc = &Doc{}
	}
	return ProtocolInfo{PromptBytes: len(Render(doc, opt)), HasState: doc.DeprecatedState != ""}
}

// Protocol returns the rules paragraph for enabled launches.
func Protocol(opt Options, p ProtocolInfo) string {
	capRule := "Log has no entry cap."
	if opt.MaxLogs > 0 {
		capRule = fmt.Sprintf("Keep Log under %d entries; prune the oldest.", opt.MaxLogs)
	}
	out := "MEMORY.md is next to your job file and is shown above. " +
		"Keep its three sections in order: Context, Lessons, Log. " +
		"Context is free form. " +
		"Lessons and Log take one bullet per entry in the form `- [YYYY-MM-DD HH:MM] text` (`- [YYYY-MM-DD] text` is accepted). " +
		"Add Log entries for events, observations, and stats. " +
		"End each run with a Log entry that starts with `Next:`. " +
		"Keep records in `trebi state`, not in memory. " +
		capRule + " Never prune Lessons by age. Update the file before you print the completion line."
	if p.HasState {
		out += " This file has a deprecated State section. Move it this run."
	}
	if opt.MaxBytes > 0 && p.PromptBytes > opt.MaxBytes {
		out += fmt.Sprintf(" Memory is %s in the prompt; the cap is %s. Trim it this run.", kb(p.PromptBytes), kb(opt.MaxBytes))
	}
	return out
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
	return Issue{Code: "unchanged", Msg: "unchanged this run. Add a Log entry for this run."}
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

// Marshal re-emits the file: Context verbatim, Lessons and Log from
// entries, skeleton comments only for empty sections. A deprecated State
// stays after Context, so that no edit loses it before the session moves it.
func (d *Doc) Marshal() []byte {
	var b strings.Builder
	b.WriteString(markerLine + "\n# Memory\n")
	b.WriteString("\n## Context\n" + bodyOrComment(d.Context, "Context"))
	if d.DeprecatedState != "" {
		b.WriteString("\n## " + stateTitle + "\n" + d.DeprecatedState + "\n")
	}
	b.WriteString("\n## Lessons\n" + entriesOrComment(d.Lessons, "Lessons"))
	b.WriteString("\n## Log\n" + entriesOrComment(d.Log, "Log"))
	return []byte(b.String())
}

// SetContext replaces the Context body.
func (d *Doc) SetContext(body string) { d.Context = strings.TrimRight(body, "\n") }

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

// sizeIssue reports too_large when the Render output is over opt.MaxBytes.
// The message gives the size of each section and the trim order.
func (d *Doc) sizeIssue(opt Options) (Issue, bool) {
	if opt.MaxBytes <= 0 {
		return Issue{}, false
	}
	total := len(Render(d, opt))
	if total <= opt.MaxBytes {
		return Issue{}, false
	}
	parts := []string{"Context " + kb(len(d.Context))}
	if d.DeprecatedState != "" {
		parts = append(parts, "State "+kb(len(d.DeprecatedState)))
	}
	parts = append(parts, "Lessons "+kb(entriesSize(d.Lessons)), "Log "+kb(entriesSize(shownLog(d.Log, opt))))
	return Issue{Code: "too_large", Msg: fmt.Sprintf("memory is %s in the prompt (%s); the cap is %s. "+
		"Trim it in this order: 1. Log: remove the oldest entries. "+
		"2. Lessons: merge duplicates, make each lesson one short rule, remove lessons that Context or the job file now covers. "+
		"3. Context: move instructions to the job file. Move records to `trebi state`.",
		kb(total), strings.Join(parts, ", "), kb(opt.MaxBytes))}, true
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
	case stateTitle:
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
	if title == stateTitle {
		p.section = stateTitle
		p.issues = append(p.issues, Issue{Code: "state_deprecated", Line: n, Msg: "the State section is deprecated. " +
			"Move records to `trebi state` and the handoff to a Log entry that starts with \"Next:\". Then remove the section."})
		return
	}
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
		Context: trimBody(p.ctx), DeprecatedState: p.stateBody(),
		Lessons: p.lessons, Log: p.log,
	}, p.issues
}

// stateBody is the deprecated State text; a blank section is "".
func (p *parseState) stateBody() string {
	body := strings.TrimLeft(trimBody(p.state), "\n")
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return body
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

// kb renders a byte count in whole KB, rounded up so that a non-empty
// section never shows 0 KB.
func kb(n int) string { return strconv.Itoa((n+1023)/1024) + " KB" }

// entriesSize is the byte size of entries as Render writes them.
func entriesSize(entries []Entry) int {
	n := 0
	for _, e := range entries {
		n += len(formatEntry(e))
	}
	return n
}

// shownLog returns the newest opt.InjectLogs Log entries, newest first.
func shownLog(log []Entry, opt Options) []Entry {
	sorted := sortedEntries(log)
	if opt.InjectLogs >= 0 && len(sorted) > opt.InjectLogs {
		return sorted[:opt.InjectLogs]
	}
	return sorted
}

// sortedEntries returns a newest-first copy (stamp strings sort chronologically).
func sortedEntries(entries []Entry) []Entry {
	out := append([]Entry{}, entries...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}
