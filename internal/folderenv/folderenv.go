// Package folderenv reads the .env file of a run working dir. Values never
// leave the daemon: the API, the tools, and the run record get a Summary.
package folderenv

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Name is the one file name that a working dir can hold.
const Name = ".env"

// MaxSize is the largest file that Load reads.
const MaxSize = 64 << 10

// Skip reasons.
const (
	ReasonNoEquals    = `no "="`
	ReasonBadKey      = "bad key"
	ReasonQuoteNotEnd = "quote not closed"
)

var keyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// File is the parsed .env of a working dir.
type File struct {
	Path   string
	Values map[string]string
	// Keys is in file order; a duplicate key keeps its first place and its
	// last value.
	Keys    []string
	Skipped []Skip
	// Err is why the file did not load: too large, not a regular file, or
	// a read error.
	Err string
}

// Skip is a line that Parse ignored.
type Skip struct {
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

// Summary is the value-free view of a File.
type Summary struct {
	Found   bool     `json:"found"`
	Enabled bool     `json:"enabled"`
	Path    string   `json:"path,omitempty"`
	Keys    []string `json:"keys"`
	Skipped []Skip   `json:"skipped,omitempty"`
	Error   string   `json:"error,omitempty"`
	// Overridden are the keys that a job or agent env value replaced.
	Overridden []string `json:"overridden,omitempty"`
}

// Load reads dir/.env. It returns nil when dir is empty or holds no .env.
func Load(dir string) *File {
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, Name)
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	f := &File{Path: path}
	switch {
	case err != nil:
		f.Err = err.Error()
	case !st.Mode().IsRegular():
		f.Err = "not a regular file"
	case st.Size() > MaxSize:
		f.Err = fmt.Sprintf("larger than %d KiB", MaxSize>>10)
	}
	if f.Err != "" {
		return f
	}
	data, err := os.ReadFile(path)
	if err != nil {
		f.Err = err.Error()
		return f
	}
	f.Values, f.Keys, f.Skipped = Parse(data)
	return f
}

// Summary is the view of f for one enabled state. A nil f is not found.
func (f *File) Summary(enabled bool) Summary {
	if f == nil {
		return Summary{Enabled: enabled, Keys: []string{}}
	}
	keys := f.Keys
	if keys == nil {
		keys = []string{}
	}
	return Summary{Found: true, Enabled: enabled, Path: f.Path, Keys: keys, Skipped: f.Skipped, Error: f.Err}
}

// Parse reads KEY=value lines. A quoted value can span lines until its quote
// closes. A bad line is skipped and reported; it does not stop the parse.
// There is no ${VAR} expansion.
func Parse(data []byte) (vals map[string]string, keys []string, skipped []Skip) {
	vals = map[string]string{}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			line = strings.TrimSpace(rest)
		}
		k, raw, ok := strings.Cut(line, "=")
		if !ok {
			skipped = append(skipped, Skip{Line: i + 1, Reason: ReasonNoEquals})
			continue
		}
		k = strings.TrimSpace(k)
		if !keyRE.MatchString(k) {
			skipped = append(skipped, Skip{Line: i + 1, Reason: ReasonBadKey})
			continue
		}
		v, last, ok := parseValue(raw, lines[i+1:])
		if !ok {
			skipped = append(skipped, Skip{Line: i + 1, Reason: ReasonQuoteNotEnd})
			continue
		}
		i += last
		if _, dup := vals[k]; !dup {
			keys = append(keys, k)
		}
		vals[k] = v
	}
	return vals, keys, skipped
}

// parseValue reads the text after "=". A quoted value takes more lines from
// next until the quote closes; used is how many. It is false when the quote
// does not close before the end of the file.
func parseValue(raw string, next []string) (v string, used int, ok bool) {
	v = strings.TrimLeft(raw, " \t")
	if v != "" && strings.ContainsRune(`'"`+"`", rune(v[0])) {
		q, text := v[0], v[1:]
		for used = 0; ; used++ {
			if val, ok := closeQuote(text, q); ok {
				return val, used, true
			}
			if used == len(next) {
				return "", 0, false
			}
			text += "\n" + next[used]
		}
	}
	for i := 1; i < len(raw); i++ {
		if raw[i] == '#' && (raw[i-1] == ' ' || raw[i-1] == '\t') {
			raw = raw[:i]
			break
		}
	}
	return strings.TrimSpace(raw), 0, true
}

// closeQuote returns the text before the closing quote q. Only a double
// quote reads the escapes \n, \", and \\; the other quotes are literal.
// Text after the closing quote is ignored.
func closeQuote(s string, q byte) (string, bool) {
	if q != '"' {
		end := strings.IndexByte(s, q)
		if end < 0 {
			return "", false
		}
		return s[:end], true
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			return b.String(), true
		case c == '\\' && i+1 < len(s):
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case '"', '\\':
				b.WriteByte(s[i])
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", false
}
