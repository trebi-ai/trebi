package connectors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

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
