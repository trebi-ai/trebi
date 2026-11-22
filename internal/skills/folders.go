package skills

import (
	"encoding/binary"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// racyWindow is how long after a change a folder is not trusted: a write in
// the same mtime tick as the read must not hide behind an equal stamp.
const racyWindow = 2 * time.Second

// folderCache keeps the disk part of each skill folder, keyed by path. A
// read stats the folder tree and reloads only when the stamp moved.
type folderCache struct {
	mu      sync.Mutex
	entries map[string]folderEntry
}

type folderEntry struct {
	stamp   uint64
	newest  time.Time
	readAt  time.Time
	modTime time.Time
	md      string
	hash    string
	scripts []string
}

func newFolderCache() *folderCache {
	return &folderCache{entries: map[string]folderEntry{}}
}

// read returns SKILL.md, the content hash, the scripts, and the mtime of dir.
// It reads file content only when the folder changed since the last read.
func (c *folderCache) read(dir string) (folderEntry, error) {
	readAt := time.Now()
	stamp, newest, err := folderStamp(dir)
	if err != nil {
		c.forget(dir)
		return folderEntry{}, err
	}
	c.mu.Lock()
	e, ok := c.entries[dir]
	c.mu.Unlock()
	if ok && e.stamp == stamp && e.newest.Before(e.readAt.Add(-racyWindow)) {
		e.scripts = slices.Clone(e.scripts)
		return e, nil
	}
	st, err := os.Stat(dir)
	if err != nil {
		c.forget(dir)
		return folderEntry{}, err
	}
	md, _ := os.ReadFile(filepath.Join(dir, "SKILL.md")) // a folder with no SKILL.md reads as empty, as before
	// The stamp comes before the content, so a write during the read leaves a stale stamp and the next read reloads.
	e = folderEntry{stamp: stamp, newest: newest, readAt: readAt, modTime: st.ModTime(), md: string(md), hash: hashDir(dir), scripts: listScripts(dir)}
	c.mu.Lock()
	c.entries[dir] = e
	c.mu.Unlock()
	e.scripts = slices.Clone(e.scripts)
	return e, nil
}

// forget drops the entries of folders that a write through the service changed.
func (c *folderCache) forget(dirs ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range dirs {
		delete(c.entries, d)
	}
}

// folderStamp hashes the path, size, mode, and mtime of dir and of each entry
// in it, and returns the newest mtime. It reads no file content.
func folderStamp(dir string) (uint64, time.Time, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return 0, time.Time{}, err
	}
	h := fnv.New64a()
	var newest time.Time
	var buf [8]byte
	add := func(rel string, fi fs.FileInfo) {
		h.Write([]byte(rel))
		binary.LittleEndian.PutUint64(buf[:], uint64(fi.Size())) //nolint:gosec // size is never negative
		h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(fi.Mode()))
		h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(fi.ModTime().UnixNano())) //nolint:gosec // bit pattern only
		h.Write(buf[:])
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, as hashDir skips it
		}
		var fi fs.FileInfo
		if d.Type()&fs.ModeSymlink != 0 {
			fi, err = os.Stat(path) // hashDir reads through file links
		} else {
			fi, err = d.Info()
		}
		if err != nil {
			return nil //nolint:nilerr // a broken link or a removed entry is skipped
		}
		rel, _ := filepath.Rel(root, path)
		add(rel, fi)
		return nil
	})
	return h.Sum64(), newest, err
}
