package workspaces

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/trebi-ai/trebi/internal/store"
	"gopkg.in/yaml.v3"
)

const rewriteMaxBytes = 4 << 20

// MoveOpts is the relocate-or-rebind request.
type MoveOpts struct {
	Path   string
	Mkdir  bool
	DryRun bool
}

// MoveResult is the workspace after a move (or the preview of a dry-run).
type MoveResult struct {
	Workspace
	Rewritten []string `json:"rewritten"`
	DryRun    bool     `json:"dry_run,omitempty"`
}

// Move relocates a workspace folder to dest (or rebinds the registry when the
// folder was already moved) and rewrites absolute path prefixes in text files.
func (s *Service) Move(ctx context.Context, idOrName string, opts MoveOpts) (*MoveResult, error) {
	plan, err := s.planMove(ctx, idOrName, opts)
	if err != nil {
		return nil, err
	}
	if err := ensureDestParent(plan.dest, opts.Mkdir, opts.DryRun); err != nil {
		return nil, err
	}
	if opts.DryRun {
		rewritten, err := rewriteTree(plan.scanRoot, plan.src, plan.dest, false)
		if err != nil {
			return nil, err
		}
		out := &MoveResult{Workspace: *plan.ws, Rewritten: rewritten, DryRun: true}
		out.Path = plan.dest
		return out, nil
	}
	if !plan.rebind {
		if plan.destExists {
			if err := os.Remove(plan.dest); err != nil {
				return nil, err
			}
		}
		if err := relocateDir(plan.src, plan.dest); err != nil {
			return nil, err
		}
	}
	rewritten, err := rewriteTree(plan.dest, plan.src, plan.dest, true)
	if err != nil {
		if dbErr := s.updatePath(ctx, plan.ws, plan.dest); dbErr != nil {
			return nil, fmt.Errorf("rewrite: %v; registry: %w", err, dbErr)
		}
		return nil, err
	}
	if err := s.updatePath(ctx, plan.ws, plan.dest); err != nil {
		return nil, err
	}
	plan.ws.Path = plan.dest
	if err := s.RebuildIndex(ctx, plan.ws); err != nil {
		return &MoveResult{Workspace: *plan.ws, Rewritten: rewritten}, err
	}
	fresh, err := s.Get(ctx, plan.ws.ID)
	if err != nil {
		return nil, err
	}
	return &MoveResult{Workspace: *fresh, Rewritten: rewritten}, nil
}

// RewritePathPrefix replaces old absolute path with new in s, only at a path
// boundary (next rune is a separator, a non-name character, or end).
func RewritePathPrefix(s, old, new string) (string, bool) {
	old = strings.TrimRight(filepath.Clean(old), `/\`)
	new = strings.TrimRight(filepath.Clean(new), `/\`)
	if old == "" || old == new || !strings.Contains(s, old) {
		return s, false
	}
	var b strings.Builder
	b.Grow(len(s))
	changed := false
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], old) && pathBoundary(s, i+len(old)) {
			b.WriteString(new)
			i += len(old)
			changed = true
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	if !changed {
		return s, false
	}
	return b.String(), true
}

type movePlan struct {
	ws         *Workspace
	src, dest  string
	rebind     bool
	destExists bool
	scanRoot   string
}

func (s *Service) planMove(ctx context.Context, idOrName string, opts MoveOpts) (*movePlan, error) {
	ws, err := s.Resolve(ctx, idOrName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if ws.Orphaned {
		return nil, ErrOrphaned
	}
	dest := strings.TrimSpace(opts.Path)
	if dest == "" {
		return nil, fmt.Errorf("path required")
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	dest = filepath.Clean(dest)
	src := filepath.Clean(ws.Path)
	if dest == src {
		return nil, ErrSamePath
	}
	if PathInside(src, dest) {
		return nil, ErrDestInside
	}
	if PathInside(dest, src) {
		return nil, ErrSrcInsideDest
	}
	if err := s.rejectMoveDest(ctx, ws.ID, dest); err != nil {
		return nil, err
	}
	srcExists, destExists, destInfo, err := statMoveEnds(src, dest)
	if err != nil {
		return nil, err
	}
	rebind, err := classifyMove(ws.ID, dest, srcExists, destExists, destInfo)
	if err != nil {
		return nil, err
	}
	scanRoot := src
	if rebind {
		scanRoot = dest
	}
	return &movePlan{ws: ws, src: src, dest: dest, rebind: rebind, destExists: destExists, scanRoot: scanRoot}, nil
}

func statMoveEnds(src, dest string) (srcExists, destExists bool, destInfo os.FileInfo, err error) {
	srcInfo, srcErr := os.Lstat(src)
	destInfo, destErr := os.Lstat(dest)
	if srcErr != nil && !os.IsNotExist(srcErr) {
		return false, false, nil, srcErr
	}
	if destErr != nil && !os.IsNotExist(destErr) {
		return false, false, nil, destErr
	}
	srcExists, destExists = srcErr == nil, destErr == nil
	if srcExists && srcInfo.Mode()&os.ModeSymlink == 0 && !srcInfo.IsDir() {
		return false, false, nil, fmt.Errorf("workspace path is not a directory: %s", src)
	}
	return srcExists, destExists, destInfo, nil
}

func classifyMove(id, dest string, srcExists, destExists bool, destInfo os.FileInfo) (rebind bool, err error) {
	switch {
	case !srcExists && !destExists:
		return false, ErrMissing
	case !srcExists && destExists:
		got, _, _, err := readTrebiYAML(dest)
		if err != nil {
			return false, fmt.Errorf("%w: %v", ErrRebindID, err)
		}
		if got != id {
			return false, fmt.Errorf("%w: found %s", ErrRebindID, got)
		}
		return true, nil
	case srcExists && destExists:
		if destInfo.Mode()&os.ModeSymlink != 0 || !destInfo.IsDir() {
			return false, ErrDestExists
		}
		empty, err := dirEmpty(dest)
		if err != nil {
			return false, err
		}
		if !empty {
			return false, ErrDestExists
		}
	}
	return false, nil
}

func ensureDestParent(dest string, mkdir, dryRun bool) error {
	parent := filepath.Dir(dest)
	_, err := os.Stat(parent)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if !mkdir {
		return fmt.Errorf("destination parent %s does not exist (pass mkdir)", parent)
	}
	if dryRun {
		return nil
	}
	return os.MkdirAll(parent, 0o755)
}

func (s *Service) rejectMoveDest(ctx context.Context, selfID, dest string) error {
	roots, err := s.otherWorkspaceRoots(ctx, selfID)
	if err != nil {
		return err
	}
	dest = filepath.Clean(dest)
	for _, root := range roots {
		root = filepath.Clean(root)
		if root == dest {
			return fmt.Errorf("%w: %s", ErrDestExists, root)
		}
		if PathInside(root, dest) {
			return fmt.Errorf("%w: %s", ErrNested, root)
		}
		if PathInside(dest, root) {
			return fmt.Errorf("%w: %s", ErrContains, root)
		}
	}
	return nil
}

func (s *Service) updatePath(ctx context.Context, ws *Workspace, dest string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	var rev int64
	s.st.LockBus()
	defer s.st.UnlockBus()
	return s.st.WithTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT revision FROM workspaces WHERE id=?`, ws.ID).Scan(&rev)
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rev++
		if _, err := tx.Exec(`UPDATE workspaces SET path=?, updated_at=?, revision=? WHERE id=?`,
			dest, now, rev, ws.ID); err != nil {
			return err
		}
		_, err = store.AppendInTx(tx, store.BusEvent{
			Topic: "workspace.updated", Entity: "workspace", EntityID: ws.ID, Action: "updated",
			Revision: rev,
			Attrs:    map[string]string{"path": dest, "name": ws.Name},
		})
		return err
	})
}

func readTrebiYAML(dir string) (id, name string, git bool, err error) {
	data, err := os.ReadFile(filepath.Join(dir, "trebi.yaml"))
	if err != nil {
		return "", "", false, err
	}
	var y trebiYAML
	if err := yaml.Unmarshal(data, &y); err != nil {
		return "", "", false, err
	}
	if y.ID == "" {
		return "", "", false, fmt.Errorf("trebi.yaml missing id")
	}
	return y.ID, y.Name, y.GitEnabled, nil
}

func dirEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err != nil && err != io.EOF {
		return false, err
	}
	return len(names) == 0, nil
}

func relocateDir(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	} else if !isCrossDevice(err) {
		return err
	}
	if err := copyTree(src, dest); err != nil {
		_ = os.RemoveAll(dest)
		return err
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("copied to %s but failed to remove %s: %w", dest, src, err)
	}
	return nil
}

func isCrossDevice(err error) bool {
	var link *os.LinkError
	if errors.As(err, &link) {
		return link.Err == syscall.EXDEV
	}
	return errors.Is(err, syscall.EXDEV)
}

func copyTree(src, dest string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func rewriteTree(root, oldPrefix, newPrefix string, write bool) ([]string, error) {
	out := []string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipRewriteDir(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > rewriteMaxBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !isText(data) {
			return nil
		}
		next, ok := RewritePathPrefix(string(data), oldPrefix, newPrefix)
		if !ok {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		out = append(out, filepath.ToSlash(rel))
		if write {
			return os.WriteFile(path, []byte(next), info.Mode().Perm())
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}

func skipRewriteDir(root, path, name string) bool {
	if path == root {
		return false
	}
	if name == ".git" || name == "node_modules" || name == ".trebi" {
		return true
	}
	return strings.HasPrefix(name, ".")
}

func isText(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	return utf8.Valid(data)
}

func pathBoundary(s string, end int) bool {
	if end >= len(s) {
		return true
	}
	c := s[end]
	if c == '/' || c == '\\' {
		return true
	}
	return !isPathNameByte(c)
}

func isPathNameByte(c byte) bool {
	return c == '-' || c == '_' || c == '.' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
