package assistant

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/files"
)

// snapshots keeps the content of each file an assistant edited, so the UI
// can revert one file of one turn (plan 2026-09-26 D.6). It also implements
// agentwire.FileSystem, so an ACP agent's writes go through it.
type snapshots struct {
	// CurrentTurn reports the turn an edit belongs to. An ACP write names no
	// turn, so the session supplies it.
	CurrentTurn func() int

	base string // the workspace root; relative tool paths resolve against it

	mu    sync.Mutex
	byKey map[string][]byte // "<turn>:<abs path>" -> content before the first edit
	order []string
}

func newSnapshots(base string) *snapshots {
	return &snapshots{byKey: map[string][]byte{}, base: base}
}

// Capture records the current content of each path, once per turn.
// Unreadable paths are skipped: Revert then reports "no snapshot".
func (s *snapshots) Capture(turn int, paths []string) {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		abs := s.absPath(path)
		key := snapshotKey(turn, abs)

		s.mu.Lock()
		_, seen := s.byKey[key]
		s.mu.Unlock()
		if seen {
			continue
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			continue // new file, or unreadable: nothing to restore
		}
		s.mu.Lock()
		if _, seen := s.byKey[key]; !seen {
			s.byKey[key] = content
			s.order = append(s.order, key)
		}
		s.mu.Unlock()
	}
}

// Revert writes the snapshot back and returns the restored file info.
func (s *snapshots) Revert(_ context.Context, turn int, path string) (files.Info, error) {
	abs := s.absPath(path)
	key := snapshotKey(turn, abs)

	s.mu.Lock()
	content, ok := s.byKey[key]
	s.mu.Unlock()
	if !ok {
		return files.Info{}, os.ErrNotExist
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		return files.Info{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return files.Info{}, err
	}
	return files.Info{
		Path:      abs,
		SizeBytes: info.Size(),
		MTime:     info.ModTime(),
	}, nil
}

// ReadTextFile implements agentwire.FileSystem.
func (s *snapshots) ReadTextFile(_ context.Context, path string) (string, error) {
	b, err := os.ReadFile(s.absPath(path))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WriteTextFile implements agentwire.FileSystem: the previous content is
// snapshotted first, then the write lands.
func (s *snapshots) WriteTextFile(_ context.Context, path, content string) error {
	abs := s.absPath(path)
	turn := 0
	if s.CurrentTurn != nil {
		turn = s.CurrentTurn()
	}
	s.Capture(turn, []string{abs})
	return os.WriteFile(abs, []byte(content), 0o644)
}

var _ agentwire.FileSystem = (*snapshots)(nil)

func snapshotKey(turn int, abs string) string {
	return strconv.Itoa(turn) + ":" + abs
}

// absPath anchors a relative tool path to the workspace base; absolute
// paths pass through.
func (s *snapshots) absPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(s.base, path)
}

// sortPaths is a tiny helper for deterministic popovers.
func sortPaths(paths []string) { sort.Strings(paths) }
