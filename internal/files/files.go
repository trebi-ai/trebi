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
	Dir       bool      `json:"dir,omitempty"`
}

// ListOpts filters a directory walk. AfterPath is the last path from a
// previous page (the wire name is cursor).
type ListOpts struct {
	Prefix    string
	AfterPath string
	Limit     int
	// Tree adds a row per directory, so a client sees the folder shape.
	Tree bool
}

// MoveOpts guards a move. IfMatch applies to a file source. Overwrite
// replaces a file at the target; a folder at the target is never replaced.
type MoveOpts struct {
	IfMatch   string
	Overwrite bool
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
	ErrExists        = errors.New("path exists")
	ErrNotDir        = errors.New("path is not a folder")
	ErrIntoItself    = errors.New("cannot move a folder into itself")
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
	all, err := s.walkFiles(ctx, root, prefix, opts.Tree)
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
	return os.Remove(joined)
}

// Mkdir creates a folder and its parents.
func (s *Service) Mkdir(ctx context.Context, root, rel string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	joined, cleaned, err := s.resolve(root, rel)
	if err != nil {
		return Info{}, err
	}
	if _, err := os.Lstat(joined); err == nil {
		return Info{}, fmt.Errorf("%w: %s", ErrExists, cleaned)
	} else if !os.IsNotExist(err) {
		return Info{}, err
	}
	if err := os.MkdirAll(joined, 0o755); err != nil {
		return Info{}, err
	}
	return dirInfo(joined, cleaned)
}

// Move renames a file or a folder inside one root. It creates the parent
// folders of the target.
func (s *Service) Move(ctx context.Context, root, from, to string, opts MoveOpts) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	src, srcRel, err := s.resolve(root, from)
	if err != nil {
		return Info{}, err
	}
	dst, dstRel, err := s.resolve(root, to)
	if err != nil {
		return Info{}, err
	}
	srcSt, err := os.Lstat(src)
	if err != nil {
		return Info{}, mapReadErr(err)
	}
	if err := checkMove(src, dst, srcRel, dstRel, srcSt, opts); err != nil {
		return Info{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Info{}, err
	}
	if err := os.Rename(src, dst); err != nil {
		return Info{}, err
	}
	if srcSt.IsDir() {
		return dirInfo(dst, dstRel)
	}
	return s.statJoined(dst, dstRel, true)
}

// checkMove refuses a folder move into itself, a stale file source, and a
// target that exists.
func checkMove(src, dst, srcRel, dstRel string, srcSt fs.FileInfo, opts MoveOpts) error {
	if srcSt.IsDir() && (dstRel == srcRel || strings.HasPrefix(dstRel, srcRel+"/")) {
		return fmt.Errorf("%w: %s", ErrIntoItself, srcRel)
	}
	if !srcSt.IsDir() {
		if err := checkIfMatch(src, opts.IfMatch, true); err != nil {
			return err
		}
	}
	dstSt, err := os.Lstat(dst)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// A case-only rename on a case-insensitive disk sees the source as the target.
	if !os.SameFile(srcSt, dstSt) && (dstSt.IsDir() || !opts.Overwrite) {
		return fmt.Errorf("%w: %s", ErrExists, dstRel)
	}
	return nil
}

// DirFiles returns the root-relative paths of all files under a folder.
// It walks the folders that List skips, so the result is what a delete removes.
func (s *Service) DirFiles(ctx context.Context, root, rel string) ([]string, error) {
	joined, cleaned, err := s.resolve(root, rel)
	if err != nil {
		return nil, err
	}
	return dirFiles(ctx, joined, cleaned)
}

// DeleteDir removes a folder and all its contents. It returns the count of
// removed files. A folder that holds a protected path stays; a leftover
// write temp file does not block the delete.
func (s *Service) DeleteDir(ctx context.Context, root, rel string) (int, error) {
	joined, cleaned, err := s.resolve(root, rel)
	if err != nil {
		return 0, err
	}
	paths, err := dirFiles(ctx, joined, cleaned)
	if err != nil {
		return 0, err
	}
	for _, p := range paths {
		if isProtected(p) && !strings.HasPrefix(path.Base(p), ".trebi-write-") {
			return 0, fmt.Errorf("%w: %s", ErrProtectedPath, p)
		}
	}
	if err := os.RemoveAll(joined); err != nil {
		return 0, err
	}
	return len(paths), nil
}

func dirFiles(ctx context.Context, joined, cleaned string) ([]string, error) {
	st, err := os.Lstat(joined)
	if err != nil {
		return nil, mapReadErr(err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrNotDir, cleaned)
	}
	out := []string{}
	err = filepath.WalkDir(joined, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(joined, p)
		if err != nil {
			return err
		}
		out = append(out, cleaned+"/"+filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func dirInfo(joined, cleaned string) (Info, error) {
	st, err := os.Stat(joined)
	if err != nil {
		return Info{}, err
	}
	return Info{Path: cleaned, MTime: st.ModTime().UTC(), Dir: true}, nil
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

func (s *Service) walkFiles(ctx context.Context, root, prefix string, tree bool) ([]Info, error) {
	var all []Info
	start := root
	if prefix != "" {
		cand := filepath.Join(root, filepath.FromSlash(prefix))
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			start = cand
		}
	}
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if skipListDir(d.Name()) {
				return filepath.SkipDir
			}
			if prefix != "" && p != start {
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return nil
				}
				rel = filepath.ToSlash(rel)
				if !couldMatchPrefix(rel, prefix) {
					return filepath.SkipDir
				}
			}
			if tree {
				all = appendDir(all, root, p, prefix)
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

// couldMatchPrefix reports whether a directory rel can contain or be a
// prefix of paths matching prefix. Used to SkipDir on disjoint subtrees.
func couldMatchPrefix(rel, prefix string) bool {
	if prefix == "" || rel == prefix {
		return true
	}
	return strings.HasPrefix(prefix, rel+"/") || strings.HasPrefix(rel, prefix+"/") || strings.HasPrefix(rel, prefix)
}

func mapReadErr(err error) error {
	if os.IsNotExist(err) {
		return ErrNotFound
	}
	return err
}

// appendDir adds one folder row to a tree listing.
func appendDir(all []Info, root, p, prefix string) []Info {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return all
	}
	rel = filepath.ToSlash(rel)
	if !prefixOK(rel, prefix) {
		return all
	}
	return append(all, Info{Path: rel, Dir: true})
}
