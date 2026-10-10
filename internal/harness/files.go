package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
)

// MaxConfigFileBytes limits one config file write.
const MaxConfigFileBytes = 1 << 20

// BackupKeep is how long Trebi keeps a reset copy.
const BackupKeep = 30 * 24 * time.Hour

var (
	ErrUnknownFile = errors.New("unknown config file")
	ErrNotOwn      = errors.New("this file is yours: make Trebi's copy first")
	ErrFileTooBig  = errors.New("the file is larger than 1 MiB")
	ErrNoBackup    = errors.New("no backup to restore")
)

// StaleError is a write with an old etag. It carries the current file.
type StaleError struct {
	Content string `json:"content"`
	ETag    string `json:"etag"`
}

func (e *StaleError) Error() string { return "the file changed since you read it" }

// ParseError is a JSON or TOML error at a line and column (1-based).
type ParseError struct {
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Msg    string `json:"-"`
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Msg)
}

// FileRow is one config file of a harness.
type FileRow struct {
	Key                 string     `json:"key"`
	Label               string     `json:"label"`
	Format              string     `json:"format"`
	Mode                string     `json:"mode"` // yours | own
	Inherit             string     `json:"inherit"`
	UserPath            string     `json:"user_path"`
	TrebiPath           string     `json:"trebi_path"`
	UserExists          bool       `json:"user_exists"`
	UserModifiedAt      *time.Time `json:"user_modified_at,omitempty"`
	OwnCreatedAt        *time.Time `json:"own_created_at,omitempty"`
	UserChangedSinceOwn bool       `json:"user_changed_since_own"`
	BackupUntil         *time.Time `json:"backup_until,omitempty"`
}

// fileRef is one config file of one harness with its resolved paths.
type fileRef struct {
	name string
	f    ConfigFile
	user string
	dst  string
}

func (s *Service) fileRef(name, key string) (fileRef, error) {
	spec, ok := SpecFor(name)
	if !ok {
		return fileRef{}, fmt.Errorf("unknown harness %q", name)
	}
	roots := s.opts.Roots.withDefaults()
	for _, f := range spec.Files {
		if f.Key == key {
			return fileRef{
				name: name, f: f,
				user: filepath.Join(roots.UserHome, f.UserRel),
				dst:  filepath.Join(roots.HarnessBase, name, f.TrebiRel),
			}, nil
		}
	}
	return fileRef{}, fmt.Errorf("%w %q for %s", ErrUnknownFile, key, name)
}

// ownMarker is stamped when Trebi makes its copy.
func (r fileRef) ownMarker() string {
	return filepath.Join(filepath.Dir(r.dst), ".trebi-own-"+r.f.Key)
}

func (r fileRef) backupDir() string { return home.HarnessBackupDir(r.name) }

// backups lists the reset copies of the file, newest first, and deletes the
// ones older than BackupKeep.
func (r fileRef) backups() []string {
	matches, _ := filepath.Glob(filepath.Join(r.backupDir(), r.f.Key+"-*"))
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	var keep []string
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		if time.Since(fi.ModTime()) > BackupKeep {
			_ = os.Remove(m) // expired backup; a failed delete retries next list
			continue
		}
		keep = append(keep, m)
	}
	return keep
}

func (r fileRef) row(cfg *config.Config) FileRow {
	row := FileRow{
		Key: r.f.Key, Label: r.f.Label, Format: r.f.Format, Mode: "yours",
		Inherit: r.f.Inherit, UserPath: r.user, TrebiPath: r.dst,
	}
	var userMod time.Time
	if fi, err := os.Stat(r.user); err == nil {
		row.UserExists = true
		userMod = fi.ModTime().UTC()
		row.UserModifiedAt = &userMod
	}
	if config.ResourceOwn(r.name, r.f.Key, cfg) {
		row.Mode = "own"
		if fi, err := os.Stat(r.ownMarker()); err == nil {
			t := fi.ModTime().UTC()
			row.OwnCreatedAt = &t
			row.UserChangedSinceOwn = row.UserExists && userMod.After(t)
		}
	}
	if b := r.backups(); len(b) > 0 {
		if fi, err := os.Stat(b[0]); err == nil {
			t := fi.ModTime().Add(BackupKeep).UTC()
			row.BackupUntil = &t
		}
	}
	return row
}

// Files lists the config files of a harness.
func (s *Service) Files(name string) ([]FileRow, error) {
	spec, ok := SpecFor(name)
	if !ok {
		return nil, fmt.Errorf("unknown harness %q", name)
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	rows := []FileRow{}
	for _, f := range spec.Files {
		r, _ := s.fileRef(name, f.Key)
		rows = append(rows, r.row(cfg))
	}
	return rows, nil
}

// OwnCount is the number of files of a harness in "own" mode.
func OwnCount(name string, cfg *config.Config) int {
	spec, _ := SpecFor(name)
	n := 0
	for _, f := range spec.Files {
		if config.ResourceOwn(name, f.Key, cfg) {
			n++
		}
	}
	return n
}

// FileETag is the etag of file content.
func FileETag(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:16])
}

// ReadFile returns one side of a config file: "user" or "trebi". The empty
// side is the one that a run uses.
func (s *Service) ReadFile(name, key, side string) (content, etag string, err error) {
	r, err := s.fileRef(name, key)
	if err != nil {
		return "", "", err
	}
	cfg, err := config.Load()
	if err != nil {
		return "", "", err
	}
	own := config.ResourceOwn(name, key, cfg)
	path := r.user
	switch side {
	case "trebi":
		path = r.dst
	case "", "user":
		if side == "" && own {
			path = r.dst
		}
	default:
		return "", "", fmt.Errorf("side must be user or trebi")
	}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	return string(b), FileETag(b), nil
}

// OwnFile makes Trebi's copy. start is "user" (the user file, without the
// hooks for a copied file), "empty", or "backup" (the newest reset copy).
func (s *Service) OwnFile(ctx context.Context, name, key, start string) error {
	r, err := s.fileRef(name, key)
	if err != nil {
		return err
	}
	var content []byte
	switch start {
	case "", "user":
		b, err := os.ReadFile(r.user)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		content = r.dropHooks(b)
	case "empty":
		if r.f.Format == "json" {
			content = []byte("{}\n")
		}
	case "backup":
		b := r.backups()
		if len(b) == 0 {
			return ErrNoBackup
		}
		if content, err = os.ReadFile(b[0]); err != nil {
			return err
		}
	default:
		return fmt.Errorf("start must be user, empty, or backup")
	}
	if fi, err := os.Lstat(r.dst); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(r.dst); err != nil {
			return err
		}
	}
	if err := WriteFileIfChanged(r.dst, content, 0644); err != nil {
		return err
	}
	if err := os.WriteFile(r.ownMarker(), nil, 0644); err != nil {
		return err
	}
	if err := s.setFileMode(name, key, "own"); err != nil {
		return err
	}
	return s.PublishUpdated(ctx, name, map[string]string{"files": "true"})
}

// dropHooks removes the hooks from a copied JSON file, as the bridge does.
func (r fileRef) dropHooks(b []byte) []byte {
	if r.f.Inherit != "copied" || r.f.Format != "json" || len(bytes.TrimSpace(b)) == 0 {
		return b
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	if _, ok := m["hooks"]; !ok {
		return b
	}
	delete(m, "hooks")
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return b
	}
	return append(out, '\n')
}

// WriteFile replaces Trebi's copy. An empty ifMatch skips the etag check.
func (s *Service) WriteFile(ctx context.Context, name, key, content, ifMatch string) (string, error) {
	r, err := s.fileRef(name, key)
	if err != nil {
		return "", err
	}
	if len(content) > MaxConfigFileBytes {
		return "", ErrFileTooBig
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	if !config.ResourceOwn(name, key, cfg) {
		return "", ErrNotOwn
	}
	cur, err := os.ReadFile(r.dst)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if ifMatch != "" && ifMatch != FileETag(cur) {
		return "", &StaleError{Content: string(cur), ETag: FileETag(cur)}
	}
	if err := checkSyntax(r.f.Format, content); err != nil {
		return "", err
	}
	if err := WriteFileIfChanged(r.dst, []byte(content), 0644); err != nil {
		return "", err
	}
	if err := s.PublishUpdated(ctx, name, map[string]string{"files": "true"}); err != nil {
		return "", err
	}
	return FileETag([]byte(content)), nil
}

// ResetFile goes back to the user file. It moves Trebi's copy to the backup
// folder and applies the bridge again.
func (s *Service) ResetFile(ctx context.Context, name, key string) error {
	r, err := s.fileRef(name, key)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(r.dst); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		if err := os.MkdirAll(r.backupDir(), 0755); err != nil {
			return err
		}
		to := filepath.Join(r.backupDir(), r.f.Key+"-"+time.Now().UTC().Format("20060102T150405.000Z"))
		if err := os.Rename(r.dst, to); err != nil {
			return err
		}
	}
	_ = os.Remove(r.ownMarker()) // a missing marker is fine
	if err := s.setFileMode(name, key, "inherit"); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := EnsureBridgesOpts(name, cfg, s.opts.Roots.withDefaults(), EnsureOpts{}); err != nil {
		s.log.Warn("bridge after reset", "harness", name, "error", err)
	}
	return s.PublishUpdated(ctx, name, map[string]string{"files": "true"})
}

func (s *Service) setFileMode(name, key, mode string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.SetPath("tools."+name+".bridge.resources."+key, mode); err != nil {
		return err
	}
	return cfg.Save()
}

// checkSyntax parses JSON and TOML. A markdown file is free text.
func checkSyntax(format, content string) error {
	switch format {
	case "json":
		if strings.TrimSpace(content) == "" {
			return nil
		}
		var v any
		err := json.Unmarshal([]byte(content), &v)
		var se *json.SyntaxError
		if errors.As(err, &se) {
			line, col := lineCol(content, int(se.Offset))
			return &ParseError{Line: line, Column: col, Msg: se.Error()}
		}
		if err != nil {
			return &ParseError{Line: 1, Column: 1, Msg: err.Error()}
		}
	case "toml":
		var v map[string]any
		_, err := toml.Decode(content, &v)
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return &ParseError{Line: pe.Position.Line, Column: pe.Position.Col, Msg: pe.Message}
		}
		if err != nil {
			return &ParseError{Line: 1, Column: 1, Msg: err.Error()}
		}
	}
	return nil
}

// lineCol maps a byte offset to a 1-based line and column.
func lineCol(s string, off int) (line, col int) {
	if off > len(s) {
		off = len(s)
	}
	before := s[:off]
	line = strings.Count(before, "\n") + 1
	col = off - strings.LastIndex(before, "\n")
	return line, col
}
