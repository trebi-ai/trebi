// Package artifacts owns the deliverables that agents save: versions in
// SQLite, bytes in a content-addressed store.
package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	ErrTooLarge = errors.New("artifact is too large")
	ErrBadSHA   = errors.New("bad sha256")
	ErrNoBlob   = errors.New("blob not found")
)

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

const tempPrefix = ".trebi-write-"

// Blobs is the content-addressed byte store: <dir>/ab/<sha256>.
type Blobs struct {
	dir    string
	max    int64
	rename func(oldpath, newpath string) error
}

func NewBlobs(dir string, maxBytes int64) *Blobs {
	return &Blobs{dir: dir, max: maxBytes, rename: os.Rename}
}

// Put writes r to the store. Identical bytes share one file.
func (b *Blobs) Put(r io.Reader) (sha string, size int64, err error) {
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(b.dir, tempPrefix+"*")
	if err != nil {
		return "", 0, err
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()           // already failed
			_ = os.Remove(tmp.Name()) // best effort
		}
	}()
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, b.max+1))
	if err != nil {
		return "", 0, err
	}
	if size > b.max {
		return "", 0, ErrTooLarge
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	sha = hex.EncodeToString(h.Sum(nil))
	dst := b.path(sha)
	if _, err := os.Stat(dst); err == nil {
		done = true
		_ = os.Remove(tmp.Name()) // the same bytes are already there
		return sha, size, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	if err := b.rename(tmp.Name(), dst); err != nil {
		return "", 0, fmt.Errorf("store blob: %w", err)
	}
	done = true
	return sha, size, nil
}

// Open returns the bytes of sha and their size.
func (b *Blobs) Open(sha string) (io.ReadCloser, int64, error) {
	if !shaRE.MatchString(sha) {
		return nil, 0, ErrBadSHA
	}
	f, err := os.Open(b.path(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, ErrNoBlob
	}
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close() // already failed
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (b *Blobs) Has(sha string) bool {
	if !shaRE.MatchString(sha) {
		return false
	}
	_, err := os.Stat(b.path(sha))
	return err == nil
}

func (b *Blobs) Remove(sha string) error {
	if !shaRE.MatchString(sha) {
		return ErrBadSHA
	}
	err := os.Remove(b.path(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Walk calls fn for each stored blob. It skips temp files and other names.
func (b *Blobs) Walk(fn func(sha string, size int64, mtime time.Time) error) error {
	shards, err := os.ReadDir(b.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, shard := range shards {
		if !shard.IsDir() || strings.HasPrefix(shard.Name(), ".") {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(b.dir, shard.Name()))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !shaRE.MatchString(e.Name()) || !strings.HasPrefix(e.Name(), shard.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue // removed while we walk
			}
			if err := fn(e.Name(), info.Size(), info.ModTime()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Blobs) path(sha string) string { return filepath.Join(b.dir, sha[:2], sha) }
