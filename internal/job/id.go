package job

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	IDPrefixJob   = "jb_"
	IDPrefixAgent = "agt_"
)

var idPattern = regexp.MustCompile(`^(jb|agt)_[0-9a-f]{10}$`)

// IDState tells how the parser found the id key.
type IDState int

const (
	IDOK IDState = iota
	IDMissing
	IDInvalid
)

// NewID mints a random ID for kind "job" or "agent".
func NewID(kind string) string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return idPrefix(kind) + hex.EncodeToString(b[:])
}

// ValidID reports whether id has the shape of kind.
func ValidID(kind, id string) bool {
	return idPattern.MatchString(id) && strings.HasPrefix(id, idPrefix(kind))
}

// LooksLikeID reports whether s has the shape of any ID.
func LooksLikeID(s string) bool { return idPattern.MatchString(s) }

// SetID writes id as the first frontmatter key and removes an old top-level
// id line. A file with no frontmatter gets a new block. All other bytes stay.
func SetID(path, id string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := WithID(data, id)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return WriteAtomic(path, out)
}

// WithID returns data with id as the first frontmatter key. See SetID.
func WithID(data []byte, id string) ([]byte, error) {
	eol := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		eol = "\r\n"
	}
	line := "id: " + id + eol
	start, end, err := locateFrontmatter(data)
	if err != nil {
		if len(data) >= 3 && string(data[:3]) == "---" {
			return nil, err
		}
		return append([]byte("---"+eol+line+"---"+eol), data...), nil
	}
	var block []byte
	for _, l := range bytes.SplitAfter(data[start:end], []byte("\n")) {
		if bytes.HasPrefix(l, []byte("id:")) {
			continue
		}
		block = append(block, l...)
	}
	out := make([]byte, 0, len(data)+len(line))
	out = append(out, data[:start]...)
	out = append(out, line...)
	out = append(out, block...)
	return append(out, data[end:]...), nil
}

// AssignID writes id into the file of j and updates ID, IDState, and Hash.
func (j *Job) AssignID(id string) error {
	data, err := os.ReadFile(j.FilePath)
	if err != nil {
		return err
	}
	out, err := WithID(data, id)
	if err != nil {
		return err
	}
	if err := WriteAtomic(j.FilePath, out); err != nil {
		return err
	}
	j.ID, j.IDState, j.Hash = id, IDOK, fileHash(out)
	return nil
}

func idPrefix(kind string) string {
	if kind == "agent" {
		return IDPrefixAgent
	}
	return IDPrefixJob
}

// scanID reads the top-level id key from the frontmatter as text. It works
// when the YAML does not parse.
func scanID(data []byte) string {
	block, ok := extractFM(data)
	if !ok {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(block))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "id:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}
