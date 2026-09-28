package requests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/files"
)

const maxReportBytes = 5 << 20

// ReportStore keeps one snapshot of the report of each request, so a later
// change in the working dir does not change what the person approves.
type ReportStore struct {
	dir   string
	files *files.Service
}

func NewReportStore(dir string) *ReportStore {
	return &ReportStore{dir: dir, files: files.New(maxReportBytes)}
}

// Snapshot copies rel (inside workingDir) to the folder of request id.
func (r *ReportStore) Snapshot(ctx context.Context, id, workingDir, rel string) (kind, sha string, err error) {
	kind, err = reportKind(rel)
	if err != nil {
		return "", "", err
	}
	if workingDir == "" {
		return "", "", invalid("report", "the run has no working dir")
	}
	data, _, err := r.files.Read(ctx, workingDir, rel)
	switch {
	case errors.Is(err, files.ErrTooLarge):
		return "", "", invalid("report", "is larger than %d MiB", maxReportBytes>>20)
	case errors.Is(err, files.ErrEscapesRoot), errors.Is(err, files.ErrProtectedPath):
		return "", "", invalid("report", "must be inside the working dir")
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrIsDir):
		return "", "", invalid("report", "file %s not found", rel)
	case err != nil:
		return "", "", fmt.Errorf("read report: %w", err)
	}
	dir := filepath.Join(r.dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "report."+kind), data, 0o600); err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(data)
	return kind, hex.EncodeToString(sum[:]), nil
}

// Open returns the snapshot of request id and its kind.
func (r *ReportStore) Open(id, kind string) (io.ReadCloser, error) {
	if kind == "" || strings.ContainsAny(id, `/\.`) {
		return nil, ErrNotFound
	}
	f, err := os.Open(filepath.Join(r.dir, id, "report."+kind))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (r *ReportStore) Remove(id string) error {
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return nil
	}
	return os.RemoveAll(filepath.Join(r.dir, id))
}

func reportKind(rel string) (string, error) {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".html", ".htm":
		return "html", nil
	case ".md", ".markdown":
		return "markdown", nil
	}
	return "", invalid("report", "must be an .html or .md file")
}
