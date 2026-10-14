// Package uploads owns the images that a person adds to a message: the
// bytes on disk under home.UploadsDir, one row per upload in SQLite.
package uploads

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/trebi/internal/store"
)

var (
	ErrNotFound = errors.New("upload not found")
	ErrTooLarge = errors.New("upload is too large")
	ErrType     = errors.New("upload is not a PNG, JPEG, GIF, or WebP image")
)

// DefaultMaxBytes is the size limit of one upload.
const DefaultMaxBytes = 10 << 20

// Keep is how long an upload that no message used stays.
const Keep = 30 * 24 * time.Hour

// IDPrefix starts every upload id.
const IDPrefix = "upl_"

// extByMIME is the allowlist of upload types and their file extensions.
var extByMIME = map[string]string{ //nolint:gochecknoglobals // static allowlist, never mutated
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// Upload is one stored image.
type Upload struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	MIME        string    `json:"mime"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	CreatedAt   time.Time `json:"created_at"`
}

// Attachment is one upload that a message carries: its id, its type, and
// its file.
type Attachment struct {
	ID   string
	MIME string
	Path string
}

// Attachments are the images of one message.
type Attachments []Attachment

// IDs lists the upload ids.
func (a Attachments) IDs() []string {
	out := make([]string, 0, len(a))
	for _, x := range a {
		out = append(out, x.ID)
	}
	return out
}

// Wire is the prompt form for a harness that takes images.
func (a Attachments) Wire() []agentwire.Attachment {
	out := make([]agentwire.Attachment, 0, len(a))
	for _, x := range a {
		out = append(out, agentwire.Attachment{MIME: x.MIME, Path: x.Path})
	}
	return out
}

// Inline is the text form for a harness without images: the text, then one
// line per image with its file path, so the agent can read the file.
func (a Attachments) Inline(text string) string {
	if len(a) == 0 {
		return text
	}
	lines := []string{strings.TrimRight(text, "\n")}
	if lines[0] == "" {
		lines = lines[:0]
	}
	for _, x := range a {
		lines = append(lines, "[Image: "+x.Path+"]")
	}
	return strings.Join(lines, "\n")
}

// Store keeps the upload rows and files.
type Store struct {
	st  *store.Store
	dir string
	max int64
	log *slog.Logger
}

// NewStore builds the store over dir. maxBytes <= 0 is DefaultMaxBytes.
func NewStore(st *store.Store, dir string, maxBytes int64) *Store {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Store{st: st, dir: dir, max: maxBytes, log: slog.Default().With("source", "uploads")}
}

// MaxBytes is the size limit of one upload.
func (s *Store) MaxBytes() int64 { return s.max }

// Put stores the image in r for workspace ws. The type comes from the bytes,
// not from the client.
func (s *Store) Put(ctx context.Context, ws string, r io.Reader) (Upload, error) {
	br := bufio.NewReaderSize(r, 512)
	head, err := br.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return Upload{}, err
	}
	mime := http.DetectContentType(head)
	ext, ok := extByMIME[mime]
	if !ok {
		return Upload{}, fmt.Errorf("%w: got %s", ErrType, mime)
	}
	u := Upload{ID: IDPrefix + ulid.Make().String(), WorkspaceID: ws, MIME: mime, CreatedAt: time.Now().UTC()}
	dir := filepath.Join(s.dir, ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Upload{}, err
	}
	dst := filepath.Join(dir, u.ID+ext)
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Upload{}, err
	}
	h := sha256.New()
	u.Size, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(br, s.max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && u.Size > s.max {
		err = ErrTooLarge
	}
	if err != nil {
		_ = os.Remove(dst) // the write failed; the row is not written
		return Upload{}, err
	}
	u.SHA256 = hex.EncodeToString(h.Sum(nil))
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO uploads (id, workspace_id, mime, size, sha256, created_at)
		VALUES (?,?,?,?,?,?)`, u.ID, ws, u.MIME, u.Size, u.SHA256, u.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		_ = os.Remove(dst) // no row points at it
		return Upload{}, fmt.Errorf("save upload: %w", err)
	}
	return u, nil
}

// Get reads one upload of workspace ws.
func (s *Store) Get(ctx context.Context, ws, id string) (Upload, error) {
	u := Upload{ID: id, WorkspaceID: ws}
	var created string
	err := s.st.DB().QueryRowContext(ctx, `SELECT mime, size, sha256, created_at FROM uploads
		WHERE id = ? AND workspace_id = ?`, id, ws).Scan(&u.MIME, &u.Size, &u.SHA256, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Upload{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Upload{}, err
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339Nano, created) // the store writes this format
	return u, nil
}

// Open returns the bytes of one upload of workspace ws.
func (s *Store) Open(ctx context.Context, ws, id string) (*os.File, Upload, error) {
	u, err := s.Get(ctx, ws, id)
	if err != nil {
		return nil, Upload{}, err
	}
	f, err := os.Open(s.Path(u))
	if errors.Is(err, os.ErrNotExist) {
		return nil, Upload{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return f, u, err
}

// Path is the file of one upload.
func (s *Store) Path(u Upload) string {
	return filepath.Join(s.dir, u.WorkspaceID, u.ID+extByMIME[u.MIME])
}

// Attach resolves the uploads that one message carries and marks them used,
// so the prune keeps them. Each id must belong to workspace ws.
func (s *Store) Attach(ctx context.Context, ws string, ids []string) (Attachments, error) {
	out := make(Attachments, 0, len(ids))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, id := range ids {
		u, err := s.Get(ctx, ws, strings.TrimSpace(id))
		if err != nil {
			return nil, err
		}
		if _, err := s.st.DB().ExecContext(ctx, `UPDATE uploads SET attached_at = ? WHERE id = ? AND attached_at IS NULL`,
			now, u.ID); err != nil {
			return nil, err
		}
		out = append(out, Attachment{ID: u.ID, MIME: u.MIME, Path: s.Path(u)})
	}
	return out, nil
}

// Prune removes the uploads older than Keep that no message used.
func (s *Store) Prune(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, workspace_id, mime FROM uploads
		WHERE attached_at IS NULL AND created_at < ?`, now.Add(-Keep).UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	var old []Upload
	for rows.Next() {
		var u Upload
		if err := rows.Scan(&u.ID, &u.WorkspaceID, &u.MIME); err != nil {
			_ = rows.Close() // the scan error wins
			return 0, err
		}
		old = append(old, u)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	n := 0
	for _, u := range old {
		if err := os.Remove(s.Path(u)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("upload prune", "id", u.ID, "err", err)
			continue
		}
		if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, u.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
