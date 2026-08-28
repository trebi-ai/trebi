// Package files owns workspace-scoped file IO behind the jail.
package files

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/job"
)

// Service is the workspace file plane.
type Service struct {
	maxBytes int64
	log      *slog.Logger
}

// Info is one file's metadata. Hash is set on Stat/Read; Oversize on List.
type Info struct {
	Path      string    `json:"path"`
	SizeBytes int64     `json:"size_bytes"`
	MTime     time.Time `json:"mtime"`
	Hash      string    `json:"hash,omitempty"`
	Oversize  bool      `json:"oversize,omitempty"`
}

// ListOpts filters a directory walk. AfterPath is the last path from a
// previous page (the wire name is cursor).
type ListOpts struct {
	Prefix    string
	AfterPath string
	Limit     int
}

// StaleError is ErrStale with the current hash when the file exists.
type StaleError struct {
	Hash    string
	Missing bool
}

func (e *StaleError) Error() string {
	if e == nil {
		return ErrStale.Error()
	}
	if e.Missing {
		return "if-match mismatch: file is missing"
	}
	if e.Hash != "" {
		return "if-match mismatch; current hash: " + e.Hash
	}
	return ErrStale.Error()
}

func (e *StaleError) Unwrap() error { return ErrStale }

const (
	// DefaultMaxBytes is files.max_bytes when unset (10 MiB).
	DefaultMaxBytes  int64 = 10 << 20
	defaultListLimit       = 50
)

var (
	ErrEscapesRoot   = errors.New("path escapes workspace root")
	ErrProtectedPath = errors.New("path is protected")
	ErrTooLarge      = errors.New("file exceeds size limit")
	ErrNotFound      = errors.New("file not found")
	ErrIsDir         = errors.New("path is a directory")
	ErrStale         = errors.New("if-match mismatch")
)

// New builds a file service. maxBytes <= 0 uses DefaultMaxBytes.
func New(maxBytes int64) *Service {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Service{maxBytes: maxBytes, log: slog.Default().With("source", "files")}
}

// MaxBytes is the write/read size cap.
func (s *Service) MaxBytes() int64 { return s.maxBytes }

// List returns files under root, sorted by path, after AfterPath, up to Limit.
func (s *Service) List(ctx context.Context, root string, opts ListOpts) ([]Info, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	prefix, err := cleanListPrefix(opts.Prefix)
	if err != nil {
		return nil, "", err
	}
	root = filepath.Clean(root)
	all, err := s.walkFiles(ctx, root, prefix)
	if err != nil {
		return nil, "", err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	out := make([]Info, 0, limit)
	for _, it := range all {
		if opts.AfterPath != "" && it.Path <= opts.AfterPath {
			continue
		}
		out = append(out, it)
		if len(out) == limit {
			break
		}
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1].Path
		for _, it := range all {
			if it.Path > last {
				next = last
				break
			}
		}
	}
	return out, next, nil
}

// Stat returns metadata and content hash for one file.
func (s *Service) Stat(ctx context.Context, root, rel string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	joined, cleaned, err := s.resolve(root, rel)
	if err != nil {
		return Info{}, err
	}
	return s.statJoined(joined, cleaned, true)
}

// Read returns file bytes and metadata (including hash).
func (s *Service) Read(ctx context.Context, root, rel string) ([]byte, Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, Info{}, err
	}
	joined, cleaned, err := s.resolve(root, rel)
	if err != nil {
		return nil, Info{}, err
	}
	info, err := s.statJoined(joined, cleaned, false)
	if err != nil {
		return nil, Info{}, err
	}
	if info.SizeBytes > s.maxBytes {
		info.Oversize = true
		return nil, info, ErrTooLarge
	}
	data, err := os.ReadFile(joined)
	if err != nil {
		return nil, Info{}, mapReadErr(err)
	}
	info.Hash = job.ContentHash(data)
	return data, info, nil
}

// Write replaces (or creates) a file. created is true when the path was missing.
func (s *Service) Write(ctx context.Context, root, rel string, data []byte, ifMatch string) (Info, bool, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, false, err
	}
	if int64(len(data)) > s.maxBytes {
		return Info{}, false, ErrTooLarge
	}
	joined, cleaned, err := s.resolve(root, rel)
	if err != nil {
		return Info{}, false, err
	}
	exists, err := s.preWrite(joined)
	if err != nil {
		return Info{}, false, err
	}
	if err := checkIfMatch(joined, ifMatch, exists); err != nil {
		return Info{}, false, err
	}
	if err := os.MkdirAll(filepath.Dir(joined), 0o755); err != nil {
		return Info{}, false, err
	}
	if err := job.WriteAtomic(joined, data); err != nil {
		return Info{}, false, err
	}
	st, err := os.Stat(joined)
	if err != nil {
		return Info{}, false, err
	}
	return Info{
		Path:      cleaned,
		SizeBytes: st.Size(),
		MTime:     st.ModTime().UTC(),
		Hash:      job.ContentHash(data),
	}, !exists, nil
}

// Delete removes one file.
func (s *Service) Delete(ctx context.Context, root, rel string, ifMatch string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	joined, _, err := s.resolve(root, rel)
	if err != nil {
		return err
	}
	st, err := os.Lstat(joined)
	if err != nil {
		return mapReadErr(err)
	}
	if st.IsDir() {
		return ErrIsDir
	}
	if err := checkIfMatch(joined, ifMatch, true); err != nil {
		return err
	}
	if err := os.Remove(joined); err != nil {
		return err
	}
	pruneEmptyParents(joined, root)
	return nil
}

// pruneEmptyParents removes now-empty directories from the deleted file's
// parent up to (never including) the jail root.
func pruneEmptyParents(joined, root string) {
	root = filepath.Clean(root)
	dir := filepath.Dir(joined)
	for {
		if dir == root || !underRoot(root, dir) {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

// CheckIfMatch enforces If-Match against the current file. An empty ifMatch
// proceeds. Used by document PATCH paths that are not full replaces.
func (s *Service) CheckIfMatch(ctx context.Context, root, rel, ifMatch string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	joined, _, err := s.resolve(root, rel)
	if err != nil {
		return err
	}
	_, statErr := os.Lstat(joined)
	exists := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	return checkIfMatch(joined, ifMatch, exists)
}

func (s *Service) resolve(root, rel string) (joined, cleaned string, err error) {
	cleaned, err = cleanRel(rel)
	if err != nil {
		return "", "", err
	}
	if isProtected(cleaned) {
		return "", "", fmt.Errorf("%w: %s", ErrProtectedPath, cleaned)
	}
	joined, err = joinRoot(root, cleaned)
	return joined, cleaned, err
}

func (s *Service) preWrite(joined string) (exists bool, err error) {
	st, err := os.Lstat(joined)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if st.IsDir() {
		return true, ErrIsDir
	}
	return true, nil
}

func (s *Service) statJoined(joined, cleaned string, hash bool) (Info, error) {
	st, err := os.Lstat(joined)
	if err != nil {
		return Info{}, mapReadErr(err)
	}
	if st.IsDir() {
		return Info{}, ErrIsDir
	}
	info := Info{
		Path:      cleaned,
		SizeBytes: st.Size(),
		MTime:     st.ModTime().UTC(),
		Oversize:  st.Size() > s.maxBytes,
	}
	if hash && !info.Oversize {
		data, err := os.ReadFile(joined)
		if err != nil {
			return Info{}, err
		}
		info.Hash = job.ContentHash(data)
	}
	return info, nil
}

func (s *Service) walkFiles(ctx context.Context, root, prefix string) ([]Info, error) {
	var all []Info
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && skipListDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if skipListFile(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !prefixOK(rel, prefix) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		item := Info{Path: rel, SizeBytes: fi.Size(), MTime: fi.ModTime().UTC()}
		if fi.Size() > s.maxBytes {
			item.Oversize = true
		}
		all = append(all, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if all == nil {
		all = []Info{}
	}
	return all, nil
}

func checkIfMatch(joined, ifMatch string, exists bool) error {
	ifMatch = strings.Trim(strings.TrimSpace(ifMatch), `"`)
	if ifMatch == "" {
		return nil
	}
	if !exists {
		return &StaleError{Missing: true}
	}
	if ifMatch == "*" {
		return nil
	}
	data, err := os.ReadFile(joined)
	if err != nil {
		if os.IsNotExist(err) {
			return &StaleError{Missing: true}
		}
		return err
	}
	hash := job.ContentHash(data)
	if hash != ifMatch {
		return &StaleError{Hash: hash}
	}
	return nil
}

func cleanRel(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("%w: empty path", ErrEscapesRoot)
	}
	if strings.Contains(rel, `\`) || path.IsAbs(rel) {
		return "", fmt.Errorf("%w: %s", ErrEscapesRoot, rel)
	}
	cleaned := path.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: %s", ErrEscapesRoot, rel)
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." || seg == "" {
			return "", fmt.Errorf("%w: %s", ErrEscapesRoot, rel)
		}
	}
	return cleaned, nil
}

func joinRoot(root, cleaned string) (string, error) {
	root = filepath.Clean(root)
	joined := filepath.Join(root, filepath.FromSlash(cleaned))
	if !underRoot(root, joined) {
		return "", fmt.Errorf("%w: %s", ErrEscapesRoot, cleaned)
	}
	if err := evalJail(root, joined); err != nil {
		if errors.Is(err, ErrEscapesRoot) {
			return "", fmt.Errorf("%w: %s", ErrEscapesRoot, cleaned)
		}
		return "", err
	}
	return joined, nil
}

func underRoot(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	sep := string(filepath.Separator)
	if target == root {
		return true
	}
	if !strings.HasPrefix(target, root+sep) {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+sep)
}

func evalJail(root, joined string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	ancestor := existingAncestor(joined)
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return ErrEscapesRoot
	}
	if !underRoot(resolvedRoot, resolved) {
		return ErrEscapesRoot
	}
	return nil
}

func existingAncestor(p string) string {
	for {
		if _, err := os.Lstat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

func isProtected(rel string) bool {
	if rel == "trebi.yaml" {
		return true
	}
	if strings.HasPrefix(path.Base(rel), ".trebi-write-") {
		return true
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".git" {
			return true
		}
	}
	return false
}

func skipListDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == ".trebi"
}

func skipListFile(name string) bool {
	return strings.HasPrefix(name, ".trebi-write-")
}

func cleanListPrefix(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", nil
	}
	// Trailing slash is a directory filter; Clean would drop it.
	trimmed := strings.Trim(prefix, "/")
	if trimmed == "" {
		return "", nil
	}
	cleaned, err := cleanRel(trimmed)
	if err != nil {
		return "", err
	}
	return cleaned, nil
}

func prefixOK(rel, prefix string) bool {
	if prefix == "" {
		return true
	}
	return rel == prefix || strings.HasPrefix(rel, prefix+"/")
}

func mapReadErr(err error) error {
	if os.IsNotExist(err) {
		return ErrNotFound
	}
	return err
}
