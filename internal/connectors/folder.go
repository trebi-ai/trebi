package connectors

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"gopkg.in/yaml.v3"
)

// Files of a connection folder. Only Folder opens them.
const (
	recordFile = "connection.yaml"
	cursorFile = "cursor.json"
	secretsSub = "secrets"
	stateSub   = "state"
	cacheSub   = "cache"
	logsSub    = "logs"
	streamLog  = "stream.jsonl"
)

// Cursor is the connector cursor of a connection: the source id of the last
// event that the pipeline stored.
type Cursor struct {
	LastEventID string `json:"last_event_id"`
	UpdatedAt   string `json:"updated_at"`
}

// Folder owns the files of one connection folder: the record, the cursor,
// the owned secrets, and the adapter state, cache, and log.
type Folder struct{ dir string }

// NewFolder returns the folder of a connection id.
func NewFolder(id string) Folder { return Folder{dir: home.ConnectionDir(id)} }

func (f Folder) Dir() string      { return f.dir }
func (f Folder) StateDir() string { return filepath.Join(f.dir, stateSub) }
func (f Folder) CacheDir() string { return filepath.Join(f.dir, cacheSub) }
func (f Folder) LogPath() string  { return filepath.Join(f.dir, logsSub, streamLog) }

// Create makes the folder and its sub folders with their modes. It keeps
// what is there.
func (f Folder) Create() error {
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{
		{f.dir, 0o700}, {f.secretsDir(), 0o700}, {f.StateDir(), 0o700},
		{f.CacheDir(), 0o700}, {filepath.Join(f.dir, logsSub), 0o755},
	} {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return fmt.Errorf("connection folder: %w", err)
		}
	}
	return nil
}

// Remove deletes the folder and everything in it.
func (f Folder) Remove() error { return os.RemoveAll(f.dir) }

// Load reads connection.yaml strictly. The id in the file must match the
// folder name.
func (f Folder) Load() (Connection, error) {
	data, err := os.ReadFile(filepath.Join(f.dir, recordFile))
	if err != nil {
		return Connection{}, err
	}
	var c Connection
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return Connection{}, fmt.Errorf("%s: %w", recordFile, err)
	}
	if want := filepath.Base(f.dir); c.ID != want {
		return Connection{}, fmt.Errorf("%s: id %q does not match the folder %s", recordFile, c.ID, want)
	}
	if err := c.Validate(); err != nil {
		return Connection{}, fmt.Errorf("%s: %w", recordFile, err)
	}
	return c, nil
}

// Save writes connection.yaml with mode 0600, because it can hold literal
// inputs.
func (f Folder) Save(c Connection) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := f.Create(); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(f.dir, recordFile), b)
}

// Cursor reads cursor.json. A missing file is a zero cursor.
func (f Folder) Cursor() (Cursor, error) {
	var c Cursor
	b, err := os.ReadFile(filepath.Join(f.dir, cursorFile))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Cursor{}, fmt.Errorf("%s: %w", cursorFile, err)
	}
	return c, nil
}

// SetCursor writes cursor.json.
func (f Folder) SetCursor(c Cursor) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(f.dir, cursorFile), append(b, '\n'))
}

// Secret reads the owned secret of one input.
func (f Folder) Secret(input string) (string, error) {
	if !envKeyRE.MatchString(input) {
		return "", fmt.Errorf("secret input %q must match %s", input, envKeyRE)
	}
	b, err := os.ReadFile(filepath.Join(f.secretsDir(), input))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SetSecret writes the owned secret of one input with mode 0600.
func (f Folder) SetSecret(input, value string) error {
	if !envKeyRE.MatchString(input) {
		return fmt.Errorf("secret input %q must match %s", input, envKeyRE)
	}
	if err := os.MkdirAll(f.secretsDir(), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(f.secretsDir(), input), []byte(value))
}

// DeleteSecret removes the owned secret of one input. A missing one is not
// an error.
func (f Folder) DeleteSecret(input string) error {
	if !envKeyRE.MatchString(input) {
		return fmt.Errorf("secret input %q must match %s", input, envKeyRE)
	}
	if err := os.Remove(filepath.Join(f.secretsDir(), input)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Secrets lists the inputs that have an owned secret, sorted.
func (f Folder) Secrets() []string {
	ents, err := os.ReadDir(f.secretsDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() && envKeyRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func (f Folder) secretsDir() string { return filepath.Join(f.dir, secretsSub) }

// writeFileAtomic writes a file with mode 0600 through a temp file and a
// rename, so a crash never leaves half a file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name) // best effort; the write already failed
		return fmt.Errorf("write %s: %w", filepath.Base(path), werr)
	}
	return nil
}

// syncSecrets writes the owned secrets and deletes the stale ones. With keep
// set, the inputs were replaced: an owned secret that is not in keep and not
// written again goes. Without keep, a secret: reference in inputs replaces
// the owned value.
func (f Folder) syncSecrets(owned, inputs map[string]string, keep map[string]bool) error {
	for name, v := range owned {
		if err := f.SetSecret(name, v); err != nil {
			return err
		}
	}
	for _, name := range f.Secrets() {
		_, written := owned[name]
		_, ref := inputs[name]
		if written || (keep == nil && !ref) || keep[name] {
			continue
		}
		if err := f.DeleteSecret(name); err != nil {
			return err
		}
	}
	return nil
}

// Limits of the stream log.
const (
	logCap       = 10 << 20 // the log rotates to one old file at this size
	logLineCap   = 64 << 10 // a longer line is cut
	logTailBytes = 2 << 20  // Tail reads at most this end part of the log
)

// logRow is one line of the stream log.
type logRow struct {
	T      string `json:"t"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// AppendLog appends one row to the stream log. At logCap it moves the log to
// stream.jsonl.1 first. The caller serializes the calls.
func (f Folder) AppendLog(at time.Time, stream, line string) error {
	if len(line) > logLineCap {
		line = line[:logLineCap] + " [cut]"
	}
	b, err := json.Marshal(logRow{T: at.UTC().Format(time.RFC3339Nano), Stream: stream, Line: line})
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := f.LogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil && st.Size()+int64(len(b)) > logCap {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	lf, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = lf.Write(b)
	return errors.Join(err, lf.Close())
}

// OpenLog returns a reader of the stream log. The log can be absent.
func (f Folder) OpenLog() *LogReader { return &LogReader{path: f.LogPath()} }

// LogReader reads a stream log with one file handle. Tail gives the last
// lines, and then Next gives each line that the writer adds. It follows a
// rotation.
type LogReader struct {
	path    string
	f       *os.File
	r       *bufio.Reader
	partial []byte
}

// Tail returns the last n whole lines from the end part of the log, and
// moves the reader to the end.
func (l *LogReader) Tail(n int) ([]string, error) {
	if l.f != nil {
		return nil, errors.New("tail of a log that is open")
	}
	if ok, err := l.open(); !ok {
		return nil, err
	}
	st, err := l.f.Stat()
	if err != nil {
		return nil, err
	}
	start := max(0, st.Size()-logTailBytes)
	buf := make([]byte, st.Size()-start)
	got, err := l.f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	buf = buf[:got]
	if _, err := l.f.Seek(start+int64(got), io.SeekStart); err != nil {
		return nil, err
	}
	if start > 0 {
		i := bytes.IndexByte(buf, '\n')
		buf = buf[i+1:] // a cut first line; i is -1 when there is no newline
	}
	end := bytes.LastIndexByte(buf, '\n')
	l.partial = append(l.partial[:0], buf[end+1:]...)
	if end < 0 {
		return nil, nil
	}
	lines := strings.Split(string(buf[:end]), "\n")
	return lines[max(0, len(lines)-n):], nil
}

// Next returns the whole lines that the writer added since the last call.
// After a rotation it reads the rest of the old file, then the new file.
func (l *LogReader) Next() ([]string, error) {
	if l.f == nil {
		if ok, err := l.open(); !ok {
			return nil, err
		}
	}
	lines, err := l.read()
	if err != nil {
		return lines, err
	}
	cur, err := os.Stat(l.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return lines, err
	}
	old, serr := l.f.Stat()
	if serr != nil {
		return lines, serr
	}
	if cur != nil && os.SameFile(cur, old) {
		return lines, nil
	}
	l.Close()
	if cur == nil {
		return lines, nil
	}
	if ok, err := l.open(); !ok {
		return lines, err
	}
	more, err := l.read()
	return append(lines, more...), err
}

// Close closes the file handle.
func (l *LogReader) Close() {
	if l.f != nil {
		_ = l.f.Close() // read only
		l.f, l.r, l.partial = nil, nil, nil
	}
}

// open opens the log. An absent log gives false and no error.
func (l *LogReader) open() (bool, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	l.f, l.r = f, bufio.NewReader(f)
	return true, nil
}

// read reads the whole lines up to the end of the open file. It keeps a
// last line with no newline for the next call.
func (l *LogReader) read() ([]string, error) {
	var lines []string
	for {
		b, err := l.r.ReadSlice('\n')
		l.partial = append(l.partial, b...)
		switch {
		case err == nil:
			lines = append(lines, string(l.partial[:len(l.partial)-1]))
			l.partial = l.partial[:0]
		case errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			return lines, nil
		default:
			return lines, err
		}
	}
}
