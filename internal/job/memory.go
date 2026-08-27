package job

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// MemoryInfo — memory sub-resource (B22 dated ledger, §3.3).
type MemoryInfo struct {
	SizeBytes int           `json:"size_bytes"`
	UpdatedAt time.Time     `json:"updated_at"`
	Hash      string        `json:"hash"`               // If-Match for writes
	Preamble  string        `json:"preamble,omitempty"` // text before the first entry
	Entries   []MemoryEntry `json:"entries"`
	Content   string        `json:"content"` // full raw text
}

// MemoryEntry — one ledger row.
type MemoryEntry struct {
	ID   string `json:"id"`   // stable short hash of (date, text)
	Date string `json:"date"` // YYYY-MM-DD
	Text string `json:"text"` // entry text, continuation lines joined
}

// Memory ledger format (B22, spec §3.3):
// An entry starts at a line `[YYYY-MM-DD] <text>`. Lines up to the next
// entry line are continuation lines of that entry (joined with "\n").
// Text before the first entry line is the read-only preamble.
// The date match is shape-only: any `[\d{4}-\d{2}-\d{2}]` prefix starts
// an entry. Calendar validity is not checked ("[2026-13-99]" parses).
var memoryEntryRe = regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2})\] ?(.*)$`)

var memoryDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ParseMemory splits ledger content into preamble and entries.
// The preamble keeps its content verbatim minus one trailing newline.
func ParseMemory(content []byte) (string, []MemoryEntry) {
	lines := strings.Split(string(content), "\n")
	var preamble []string
	entries := []MemoryEntry{}
	cur := -1 // index into entries of the open entry
	for i, line := range lines {
		// Split leaves one empty trailing element for a final newline.
		if i == len(lines)-1 && line == "" {
			break
		}
		if m := memoryEntryRe.FindStringSubmatch(line); m != nil {
			entries = append(entries, MemoryEntry{Date: m[1], Text: m[2]})
			cur = len(entries) - 1
			continue
		}
		if cur < 0 {
			preamble = append(preamble, line)
			continue
		}
		entries[cur].Text += "\n" + line
	}
	for i := range entries {
		entries[i].ID = memoryEntryID(entries[i].Date, entries[i].Text)
	}
	return strings.Join(preamble, "\n"), entries
}

// AppendMemoryEntry appends one ledger entry to the file at path.
// An empty date defaults to today (UTC). The file is created when missing.
func AppendMemoryEntry(path, date, text string) (MemoryEntry, error) {
	if date == "" {
		date = time.Now().UTC().Format("2006-01-02")
	}
	if !memoryDateRe.MatchString(date) {
		return MemoryEntry{}, fmt.Errorf("invalid date %q: want YYYY-MM-DD", date)
	}
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return MemoryEntry{}, fmt.Errorf("empty entry text")
	}
	// A continuation line that matches the entry pattern would re-parse as a
	// second entry and break the returned id. Reject it.
	for _, ln := range strings.Split(text, "\n")[1:] {
		if memoryEntryRe.MatchString(ln) {
			return MemoryEntry{}, fmt.Errorf("entry text line %q would parse as a new entry", ln)
		}
	}
	content, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return MemoryEntry{}, err
	}
	// Ensure exactly one trailing newline before the append.
	s := strings.TrimRight(string(content), "\n")
	if s != "" {
		s += "\n"
	}
	s += formatMemoryEntry(date, text)
	if err := WriteAtomic(path, []byte(s)); err != nil {
		return MemoryEntry{}, err
	}
	return MemoryEntry{ID: memoryEntryID(date, text), Date: date, Text: text}, nil
}

// RemoveMemoryEntry drops the entry with the given id and rewrites the file.
// The preamble stays verbatim; remaining entries re-serialize as ledger lines.
func RemoveMemoryEntry(path, id string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	preamble, entries := ParseMemory(content)
	kept := entries[:0]
	found := false
	for _, e := range entries {
		if e.ID == id {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		return fmt.Errorf("memory entry %q not found", id)
	}
	return WriteAtomic(path, serializeMemory(preamble, kept))
}

// LoadMemoryInfo reads the ledger file and returns its wire shape.
// It returns (nil, nil) when the file does not exist.
func LoadMemoryInfo(path string) (*MemoryInfo, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	preamble, entries := ParseMemory(content)
	return &MemoryInfo{
		SizeBytes: len(content),
		UpdatedAt: st.ModTime().UTC(),
		Hash:      ContentHash(content),
		Preamble:  preamble,
		Entries:   entries,
		Content:   string(content),
	}, nil
}

// memoryEntryID returns the stable short id for (date, text).
func memoryEntryID(date, text string) string {
	sum := sha256.Sum256([]byte(date + "\x00" + text))
	return hex.EncodeToString(sum[:4])
}

// serializeMemory renders preamble + entries back to file content.
func serializeMemory(preamble string, entries []MemoryEntry) []byte {
	var b strings.Builder
	if preamble != "" {
		b.WriteString(preamble)
		b.WriteString("\n")
	}
	for _, e := range entries {
		b.WriteString(formatMemoryEntry(e.Date, e.Text))
	}
	return []byte(b.String())
}

// formatMemoryEntry renders one entry: `[date] first-line` + continuation lines.
func formatMemoryEntry(date, text string) string {
	first, rest, multi := strings.Cut(text, "\n")
	line := "[" + date + "]"
	if first != "" {
		line += " " + first
	}
	line += "\n"
	if multi {
		line += rest + "\n"
	}
	return line
}
