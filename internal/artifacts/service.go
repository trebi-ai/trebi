package artifacts

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

var (
	ErrNotFound  = errors.New("artifact not found")
	ErrAmbiguous = errors.New("artifact name is ambiguous")
	ErrConflict  = errors.New("artifact revision changed")
)

// StaleError is a save that would overwrite a newer version (D4).
type StaleError struct{ Latest int }

func (e *StaleError) Error() string {
	return fmt.Sprintf("version %d is newer; read it and save again with base %d", e.Latest, e.Latest)
}

// Origin kinds of a version.
const (
	OriginRun          = "run"
	OriginConversation = "conversation"
	OriginUser         = "user"
)

// Owner kinds of an artifact.
const (
	OwnerJob       = "job"
	OwnerAgent     = "agent"
	OwnerAssistant = "assistant"
)

const idPrefix = "art_"

// Config holds the limits of the store.
type Config struct {
	MaxBytes      int64
	KeepVersions  int
	KeepSnapshots time.Duration
}

const (
	DefaultMaxBytes      = 25 << 20
	DefaultKeepVersions  = 30
	DefaultKeepSnapshots = 90 * 24 * time.Hour
	orphanAge            = time.Hour
)

// ConfigFrom reads the artifacts block of cfg. A nil cfg gives the defaults.
func ConfigFrom(cfg *config.Config) Config {
	if cfg == nil {
		return Config{}.withDefaults()
	}
	a := cfg.Artifacts
	return Config{MaxBytes: a.MaxBytes, KeepVersions: a.KeepVersions, KeepSnapshots: a.KeepSnapshotsDuration()}.withDefaults()
}

func (c Config) withDefaults() Config {
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.KeepVersions <= 0 {
		c.KeepVersions = DefaultKeepVersions
	}
	if c.KeepSnapshots <= 0 {
		c.KeepSnapshots = DefaultKeepSnapshots
	}
	return c
}

// Runs reads the run of a token or a run-end hook.
type Runs interface {
	Get(ctx context.Context, id string) (*runs.Run, error)
}

// Jobs reads the owner of a job.
type Jobs interface {
	JobByID(ctx context.Context, id string) (*workspaces.JobRow, error)
}

type Service struct {
	st     *store.Store
	blobs  *Blobs
	files  *files.Service
	runs   Runs
	jobs   Jobs
	cont   Continuer
	src    Sources
	sched  Scheduler
	alerts Alerter
	limit  refreshLimiter
	cfg    Config
	now    func() time.Time
	log    *slog.Logger
}

func NewService(st *store.Store, blobs *Blobs, rs Runs, jobs Jobs, cfg Config) *Service {
	cfg = cfg.withDefaults()
	return &Service{
		st: st, blobs: blobs, files: files.New(cfg.MaxBytes), runs: rs, jobs: jobs, cfg: cfg,
		now: func() time.Time { return time.Now().UTC() },
		log: slog.Default().With("source", "artifacts"),
	}
}

// Origin is who saves a version and who owns the artifact.
type Origin struct {
	Kind           string
	RunID          string
	ConversationID string
	Author         string
	OwnerKind      string
	OwnerID        string
}

// same reports whether two origins are the same writer (D4).
func (o Origin) same(kind, runID, convID string) bool {
	if o.Kind != kind {
		return false
	}
	switch kind {
	case OriginRun:
		return o.RunID == runID
	case OriginConversation:
		return o.ConversationID == convID
	}
	return true
}

// Artifact is one artifact with its newest version.
type Artifact struct {
	ID            string   `json:"id"`
	Workspace     string   `json:"workspace"`
	OwnerKind     string   `json:"owner_kind"`
	OwnerID       string   `json:"owner_id"`
	Name          string   `json:"name"`
	Title         string   `json:"title"`
	Kind          Kind     `json:"kind"`
	Live          bool     `json:"live"`
	LatestVersion int      `json:"latest_version"`
	LiveVersion   *int     `json:"live_version,omitempty"`
	KeepVersions  *int     `json:"keep_versions,omitempty"`
	Schedule      string   `json:"schedule,omitempty"`
	Deliver       *Deliver `json:"deliver,omitempty"`
	// ConversationID is the one chat of the artifact (C1).
	ConversationID string    `json:"conversation_id,omitempty"`
	Revision       int64     `json:"revision"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Latest         *Version  `json:"latest,omitempty"`
	VersionCount   int       `json:"version_count"`
	DeliveryCount  int       `json:"delivery_count"`
	chatVersion    int
}

// Version is one saved version. It never changes, except pinned.
type Version struct {
	ArtifactID     string     `json:"artifact_id"`
	N              int        `json:"n"`
	SHA256         string     `json:"sha256"`
	SizeBytes      int64      `json:"size_bytes"`
	MIME           string     `json:"mime"`
	FileName       string     `json:"file_name"`
	Note           string     `json:"note,omitempty"`
	OriginKind     string     `json:"origin_kind"`
	RunID          string     `json:"run_id,omitempty"`
	ConversationID string     `json:"conversation_id,omitempty"`
	Author         string     `json:"author,omitempty"`
	BaseN          *int       `json:"base_n,omitempty"`
	Live           *Live      `json:"live,omitempty"`
	Approval       string     `json:"approval"`
	DecidedBy      string     `json:"decided_by,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	Pinned         bool       `json:"pinned"`
	Delivered      bool       `json:"delivered"`
	CreatedAt      time.Time  `json:"created_at"`
}

// VersionRef is one version that a run saved.
type VersionRef struct {
	ArtifactID string    `json:"artifact_id"`
	Name       string    `json:"name"`
	Title      string    `json:"title"`
	Kind       Kind      `json:"kind"`
	N          int       `json:"n"`
	CreatedAt  time.Time `json:"created_at"`
}

// Delivery is one mail that carried a version.
type Delivery struct {
	EmailID     string    `json:"email_id"`
	Version     int       `json:"version"`
	SnapshotID  string    `json:"snapshot_id,omitempty"`
	Recipients  string    `json:"recipients"`
	Subject     string    `json:"subject"`
	Disposition string    `json:"disposition"`
	CreatedAt   time.Time `json:"created_at"`
}

// PutOpts is one save.
type PutOpts struct {
	Workspace  string
	ArtifactID string // optional: add a version to this artifact
	Name       string
	Title      string
	Kind       Kind // optional: detect
	FileName   string
	Content    io.Reader
	Note       string
	Base       *int
	Origin     Origin
	Live       *Live // optional: the sources of a live template
}

// OriginFor builds the origin of a caller. A nil actor is the user; the
// owner of a user save is ownerKind and ownerID, default the assistant.
func (s *Service) OriginFor(ctx context.Context, a *tools.Actor, ownerKind, ownerID string) (Origin, error) {
	if a == nil || (a.RunID == "" && a.Kind != OwnerAssistant) {
		if ownerKind == "" {
			ownerKind = OwnerAssistant
		}
		if ownerKind == OwnerAssistant {
			ownerID = ""
		}
		return Origin{Kind: OriginUser, Author: OriginUser, OwnerKind: ownerKind, OwnerID: ownerID}, nil
	}
	if a.Kind == OwnerAssistant && a.RunID == "" {
		return Origin{Kind: OriginConversation, ConversationID: a.Conversation, Author: OwnerAssistant,
			OwnerKind: OwnerAssistant}, nil
	}
	return s.runOrigin(ctx, a), nil
}

// Put saves the next version. It writes the blob first; a failed
// transaction leaves an orphan blob that Prune removes.
func (s *Service) Put(ctx context.Context, o PutOpts) (Version, error) {
	if o.Workspace == "" {
		return Version{}, invalid("workspace", "is required")
	}
	if o.ArtifactID == "" && !job.ValidArtifactName(o.Name) {
		return Version{}, invalid("name", "must match [a-z0-9][a-z0-9-]{0,62}")
	}
	if o.Kind != "" && !o.Kind.Valid() {
		return Version{}, invalid("kind", "unknown kind %q", o.Kind)
	}
	if o.Content == nil {
		return Version{}, invalid("content", "is required")
	}
	if o.Live != nil {
		if err := s.checkSources(ctx, o.Workspace, o.Live); err != nil {
			return Version{}, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(o.Content, s.cfg.MaxBytes+1))
	if err != nil {
		return Version{}, err
	}
	if int64(len(data)) > s.cfg.MaxBytes {
		return Version{}, ErrTooLarge
	}
	kind, mimeType := DetectKind(o.FileName, data[:min(len(data), 4096)])
	if o.Kind != "" && o.Kind != kind {
		kind, mimeType = o.Kind, o.Kind.MIME()
	}
	if errs := kind.Validate(data); len(errs) > 0 {
		return Version{}, &InvalidError{Fields: errs}
	}
	sha, size, err := s.blobs.Put(bytes.NewReader(data))
	if err != nil {
		return Version{}, err
	}
	return s.insert(ctx, o, kind, mimeType, sha, size)
}

// PutFromFile saves rel inside workingDir. The files jail rejects a path
// outside the folder.
func (s *Service) PutFromFile(ctx context.Context, o PutOpts, workingDir, rel string) (Version, error) {
	if workingDir == "" {
		return Version{}, invalid("from_path", "the run has no working dir")
	}
	if filepath.IsAbs(rel) {
		r, err := filepath.Rel(workingDir, rel)
		if err != nil || strings.HasPrefix(r, "..") {
			return Version{}, invalid("from_path", "must be inside the working dir")
		}
		rel = r
	}
	data, _, err := s.files.Read(ctx, workingDir, rel)
	switch {
	case errors.Is(err, files.ErrTooLarge):
		return Version{}, ErrTooLarge
	case errors.Is(err, files.ErrEscapesRoot), errors.Is(err, files.ErrProtectedPath):
		return Version{}, invalid("from_path", "must be inside the working dir")
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrIsDir):
		return Version{}, invalid("from_path", "file %s not found", rel)
	case err != nil:
		return Version{}, fmt.Errorf("read %s: %w", rel, err)
	}
	o.Content = bytes.NewReader(data)
	if o.FileName == "" {
		o.FileName = filepath.Base(rel)
	}
	return s.Put(ctx, o)
}

const artifactSelect = `SELECT id, workspace_id, owner_kind, owner_id, name, title, kind, live, latest_version, live_version,
  keep_versions, schedule, deliver_json, conversation_id, chat_version, revision, created_at, updated_at FROM artifacts`

type scanner interface{ Scan(dest ...any) error }

func scanArtifact(row scanner) (Artifact, error) {
	var a Artifact
	var kind, deliver, created, updated string
	var liveV, keep sql.NullInt64
	if err := row.Scan(&a.ID, &a.Workspace, &a.OwnerKind, &a.OwnerID, &a.Name, &a.Title, &kind, &a.Live, &a.LatestVersion,
		&liveV, &keep, &a.Schedule, &deliver, &a.ConversationID, &a.chatVersion, &a.Revision, &created, &updated); err != nil {
		return Artifact{}, err
	}
	if deliver != "" {
		_ = json.Unmarshal([]byte(deliver), &a.Deliver) // the service writes this JSON
	}
	a.Kind = Kind(kind)
	a.LiveVersion, a.KeepVersions = intPtr(liveV), intPtr(keep)
	a.CreatedAt, a.UpdatedAt = parseTime(created), parseTime(updated)
	return a, nil
}

// Get returns one artifact with its newest version and counts.
func (s *Service) Get(ctx context.Context, id string) (Artifact, error) {
	a, err := scanArtifact(s.st.DB().QueryRowContext(ctx, artifactSelect+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	if v, err := s.Version(ctx, id, a.LatestVersion); err == nil {
		a.Latest = &v
	}
	db := s.st.DB()
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_versions WHERE artifact_id=?`, id).Scan(&a.VersionCount); err != nil {
		return Artifact{}, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM email_messages WHERE artifact_id=?`, id).Scan(&a.DeliveryCount); err != nil {
		return Artifact{}, err
	}
	return a, nil
}

// Resolve finds ref (an id, a name, or name@n) in workspace ws. A name
// looks in the artifacts of owner first, then in the workspace. n is 0 for
// the newest version.
func (s *Service) Resolve(ctx context.Context, ws string, owner Origin, ref string) (Artifact, int, error) {
	ref = strings.TrimSpace(ref)
	n := 0
	if at := strings.LastIndex(ref, "@"); at > 0 {
		v, err := strconv.Atoi(ref[at+1:])
		if err != nil || v < 1 {
			return Artifact{}, 0, invalid("ref", "version after @ must be a number from 1")
		}
		ref, n = ref[:at], v
	}
	if strings.HasPrefix(ref, idPrefix) {
		a, err := s.Get(ctx, ref)
		if err != nil {
			return Artifact{}, 0, err
		}
		if ws != "" && a.Workspace != ws {
			return Artifact{}, 0, ErrNotFound
		}
		return a, n, nil
	}
	var ids []string
	if owner.OwnerKind != "" {
		if err := s.ids(ctx, &ids, `SELECT id FROM artifacts WHERE workspace_id=? AND owner_kind=? AND owner_id=? AND name=?`,
			ws, owner.OwnerKind, owner.OwnerID, ref); err != nil {
			return Artifact{}, 0, err
		}
	}
	if len(ids) == 0 {
		if err := s.ids(ctx, &ids, `SELECT id FROM artifacts WHERE workspace_id=? AND name=?`, ws, ref); err != nil {
			return Artifact{}, 0, err
		}
	}
	switch len(ids) {
	case 0:
		return Artifact{}, 0, ErrNotFound
	case 1:
		a, err := s.Get(ctx, ids[0])
		return a, n, err
	}
	return Artifact{}, 0, fmt.Errorf("%w: %d artifacts are named %s; use the id", ErrAmbiguous, len(ids), ref)
}

// ListFilter filters List. Cursor is the next_cursor of the last page.
// Conversation matches the chat of an artifact and the versions that a
// conversation saved.
type ListFilter struct {
	Workspace    string
	Conversation string
	OwnerKind    string
	OwnerID      string
	Kind         Kind
	Live         *bool
	Query        string
	Cursor       string
	Limit        int
}

// ErrBadCursor is a list cursor that List did not make.
var ErrBadCursor = errors.New("bad artifact list cursor")

// List returns artifacts, newest update first.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Artifact, string, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	where, args, err := f.where()
	if err != nil {
		return nil, "", err
	}
	q := artifactSelect + where + " ORDER BY updated_at DESC, id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = base64.RawURLEncoding.EncodeToString([]byte(fmtTime(last.UpdatedAt) + "|" + last.ID))
	}
	return out, next, nil
}

func decodeCursor(c string) (at, id string, err error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", "", ErrBadCursor
	}
	at, id, ok := strings.Cut(string(b), "|")
	if !ok || !strings.HasPrefix(id, idPrefix) {
		return "", "", ErrBadCursor
	}
	return at, id, nil
}

const versionSelect = `SELECT v.artifact_id, v.n, v.sha256, v.size_bytes, v.mime, v.file_name, v.note, v.origin_kind, v.run_id,
  v.conversation_id, v.author, v.base_n, v.sources_json, v.params_json, v.approval, v.decided_by, v.decided_at, v.pinned, v.created_at,
  EXISTS (SELECT 1 FROM email_messages e WHERE e.artifact_id = v.artifact_id AND e.artifact_version = v.n)
  FROM artifact_versions v`

func scanVersion(row scanner) (Version, error) {
	var v Version
	var base sql.NullInt64
	var sources, params, decided, created string
	if err := row.Scan(&v.ArtifactID, &v.N, &v.SHA256, &v.SizeBytes, &v.MIME, &v.FileName, &v.Note, &v.OriginKind, &v.RunID,
		&v.ConversationID, &v.Author, &base, &sources, &params, &v.Approval, &v.DecidedBy, &decided, &v.Pinned, &created,
		&v.Delivered); err != nil {
		return Version{}, err
	}
	v.BaseN = intPtr(base)
	v.Live = decodeLive(sources, params)
	if decided != "" {
		t := parseTime(decided)
		v.DecidedAt = &t
	}
	v.CreatedAt = parseTime(created)
	return v, nil
}

// Versions lists the versions of id, newest first.
func (s *Service) Versions(ctx context.Context, id string) ([]Version, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.st.DB().QueryContext(ctx, versionSelect+` WHERE v.artifact_id=? ORDER BY v.n DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Version returns version n of id.
func (s *Service) Version(ctx context.Context, id string, n int) (Version, error) {
	v, err := scanVersion(s.st.DB().QueryRowContext(ctx, versionSelect+` WHERE v.artifact_id=? AND v.n=?`, id, n))
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	return v, err
}

// Open returns the bytes of version n of id. n 0 is the newest version.
func (s *Service) Open(ctx context.Context, id string, n int) (io.ReadCloser, Version, error) {
	if n == 0 {
		a, err := s.Get(ctx, id)
		if err != nil {
			return nil, Version{}, err
		}
		n = a.LatestVersion
	}
	v, err := s.Version(ctx, id, n)
	if err != nil {
		return nil, Version{}, err
	}
	rc, _, err := s.blobs.Open(v.SHA256)
	if errors.Is(err, ErrNoBlob) {
		return nil, Version{}, fmt.Errorf("%w: the bytes of version %d are gone", ErrNotFound, n)
	}
	return rc, v, err
}

// Read returns the bytes of version n of id, with the artifact.
func (s *Service) Read(ctx context.Context, id string, n int) ([]byte, Artifact, Version, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return nil, Artifact{}, Version{}, err
	}
	rc, v, err := s.Open(ctx, id, cmp.Or(n, a.LatestVersion))
	if err != nil {
		return nil, Artifact{}, Version{}, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	return data, a, v, err
}

// Body reads the version that a mail carries. An email draft gives its
// body without the header block.
func (s *Service) Body(ctx context.Context, ref email.ArtifactRef) (email.ArtifactBody, error) {
	data, a, v, err := s.Read(ctx, ref.ID, ref.Version)
	if err != nil {
		return email.ArtifactBody{}, err
	}
	b := email.ArtifactBody{Kind: string(a.Kind), Name: a.Name, Title: a.Title, FileName: v.FileName, MIME: v.MIME, Content: data}
	if a.Kind == KindEmail {
		d, _ := ParseDraft(data)
		b.Content = []byte(d.Body)
	}
	if v.Live != nil {
		return s.liveBody(ctx, b, a, ref.SnapshotID)
	}
	return b, nil
}

// Patch is a change of the artifact fields. Nil keeps a field.
type Patch struct {
	Title        *string `json:"title"`
	KeepVersions *int    `json:"keep_versions"`
}

// Update applies p. ifRevision 0 skips the revision check.
func (s *Service) Update(ctx context.Context, id string, p Patch, ifRevision int64) (Artifact, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return Artifact{}, err
	}
	if ifRevision != 0 && ifRevision != a.Revision {
		return Artifact{}, ErrConflict
	}
	if p.Title != nil {
		t := strings.TrimSpace(*p.Title)
		if t == "" {
			return Artifact{}, invalid("title", "is required")
		}
		a.Title = t
	}
	if p.KeepVersions != nil {
		if *p.KeepVersions < 1 {
			return Artifact{}, invalid("keep_versions", "must be 1 or more")
		}
		a.KeepVersions = p.KeepVersions
	}
	a.Revision++
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE artifacts SET title=?, keep_versions=?, revision=?, updated_at=? WHERE id=? AND revision=?`,
			a.Title, nullInt(a.KeepVersions), a.Revision, fmtTime(s.now()), id, a.Revision-1)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrConflict
		}
		return nil
	}, s.event("artifact.updated", "updated", &a, a.LatestVersion))
	if err != nil {
		return Artifact{}, err
	}
	return s.Get(ctx, id)
}

// Pin keeps version n from prune.
func (s *Service) Pin(ctx context.Context, id string, n int, on bool) error {
	a, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.Version(ctx, id, n); err != nil {
		return err
	}
	a.Revision++
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE artifact_versions SET pinned=? WHERE artifact_id=? AND n=?`, on, id, n); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE artifacts SET revision=? WHERE id=?`, a.Revision, id)
		return err
	}, s.event("artifact.updated", "updated", &a, n))
	return err
}

// Delete removes the artifact and its versions. Prune removes the bytes.
func (s *Service) Delete(ctx context.Context, id string) error {
	a, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if s.sched != nil && a.Schedule != "" {
		if err := s.sched.DeleteArtifact(ctx, id); err != nil {
			return err
		}
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id=?`, id)
		return err
	}, s.event("artifact.deleted", "deleted", &a, a.LatestVersion))
	return err
}

// ByRun lists the versions that runID saved.
func (s *Service) ByRun(ctx context.Context, runID string) ([]VersionRef, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT a.id, a.name, a.title, a.kind, v.n, v.created_at
  FROM artifact_versions v JOIN artifacts a ON a.id = v.artifact_id WHERE v.run_id=? ORDER BY v.created_at, a.name`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VersionRef{}
	for rows.Next() {
		var r VersionRef
		var kind, created string
		if err := rows.Scan(&r.ArtifactID, &r.Name, &r.Title, &kind, &r.N, &created); err != nil {
			return nil, err
		}
		r.Kind, r.CreatedAt = Kind(kind), parseTime(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Deliveries lists the mail that carried a version of id, newest first.
func (s *Service) Deliveries(ctx context.Context, id string) ([]Delivery, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, COALESCE(artifact_version, 0), snapshot_id, recipients, subject, disposition, created_at
  FROM email_messages WHERE artifact_id=? ORDER BY created_at DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		var created string
		if err := rows.Scan(&d.EmailID, &d.Version, &d.SnapshotID, &d.Recipients, &d.Subject, &d.Disposition, &created); err != nil {
			return nil, err
		}
		d.CreatedAt = parseTime(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// PruneStats counts what one Prune removed.
type PruneStats struct {
	Versions int
	Blobs    int
}

// Prune keeps the newest keep_versions of each artifact, plus pinned,
// delivered, and live versions. Then it removes each blob that no row names
// and that is older than one hour.
func (s *Service) Prune(ctx context.Context, now time.Time) (PruneStats, error) {
	var st PruneStats
	res, err := s.st.DB().ExecContext(ctx, `DELETE FROM artifact_versions WHERE pinned = 0
  AND n <= (SELECT a.latest_version - COALESCE(a.keep_versions, ?) FROM artifacts a WHERE a.id = artifact_versions.artifact_id)
  AND n <> (SELECT COALESCE(a.live_version, 0) FROM artifacts a WHERE a.id = artifact_versions.artifact_id)
  AND NOT EXISTS (SELECT 1 FROM email_messages e WHERE e.artifact_id = artifact_versions.artifact_id AND e.artifact_version = artifact_versions.n)`,
		s.cfg.KeepVersions)
	if err != nil {
		return st, err
	}
	n, _ := res.RowsAffected()
	st.Versions = int(n)
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM artifact_snapshots WHERE pinned = 0 AND created_at < ?
  AND NOT EXISTS (SELECT 1 FROM email_messages e WHERE e.snapshot_id = artifact_snapshots.id)`,
		fmtTime(now.Add(-s.cfg.KeepSnapshots))); err != nil {
		return st, err
	}
	used := map[string]bool{}
	var shas []string
	if err := s.ids(ctx, &shas, `SELECT sha256 FROM artifact_versions UNION SELECT data_sha256 FROM artifact_snapshots
  UNION SELECT raw_sha256 FROM decisions`); err != nil {
		return st, err
	}
	for _, sha := range shas {
		used[sha] = true
	}
	err = s.blobs.Walk(func(sha string, _ int64, mtime time.Time) error {
		if used[sha] || now.Sub(mtime) < orphanAge {
			return nil
		}
		if err := s.blobs.Remove(sha); err != nil {
			return err
		}
		st.Blobs++
		return nil
	})
	return st, err
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s) // the store writes this format
	return t
}

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func intPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func (s *Service) insert(ctx context.Context, o PutOpts, kind Kind, mimeType, sha string, size int64) (Version, error) {
	now := s.now()
	ts := fmtTime(now)
	s.st.LockBus()
	defer s.st.UnlockBus()
	var v Version
	var a *Artifact
	var prevLive *Live
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.current(ctx, tx, o)
		if err != nil {
			return err
		}
		spec, err := liveSpec(ctx, tx, cur, o.Live, kind)
		if err != nil {
			return err
		}
		action := "updated"
		if cur == nil {
			action = "created"
			a = &Artifact{
				ID: idPrefix + strings.ToLower(ulid.Make().String()), Workspace: o.Workspace,
				OwnerKind: o.Origin.OwnerKind, OwnerID: o.Origin.OwnerID, Name: o.Name,
				Title: cmp.Or(o.Title, o.Name), Kind: kind, Revision: 1, LatestVersion: 1,
			}
		} else {
			if err := s.checkStale(ctx, tx, cur, o); err != nil {
				return err
			}
			a = cur
			a.LatestVersion++
			a.Revision++
			a.Title = cmp.Or(o.Title, a.Title)
			a.Kind = kind
		}
		approval, prev, err := approve(ctx, tx, a, spec, o.Origin)
		if err != nil {
			return err
		}
		prevLive = prev
		if err := writeArtifactRow(ctx, tx, a, cur == nil, ts); err != nil {
			return err
		}
		fileName := filepath.Base(o.FileName)
		if o.FileName == "" {
			fileName = a.Name + kind.Ext()
		}
		v = Version{
			ArtifactID: a.ID, N: a.LatestVersion, SHA256: sha, SizeBytes: size, MIME: mimeType, FileName: fileName,
			Note: o.Note, OriginKind: o.Origin.Kind, RunID: o.Origin.RunID, ConversationID: o.Origin.ConversationID,
			Author: o.Origin.Author, BaseN: o.Base, Live: spec, Approval: approval, CreatedAt: now,
		}
		sources, params := encodeLive(spec)
		if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_versions (artifact_id, n, sha256, size_bytes, mime, file_name, note,
  origin_kind, run_id, conversation_id, author, base_n, sources_json, params_json, approval, created_at)
  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			v.ArtifactID, v.N, v.SHA256, v.SizeBytes, v.MIME, v.FileName, v.Note, v.OriginKind, v.RunID,
			v.ConversationID, v.Author, nullInt(v.BaseN), sources, params, approval, ts); err != nil {
			return err
		}
		ev := s.event("artifact."+action, action, a, v.N)
		attrs, _ := ev.Attrs.(map[string]any)
		attrs["run_id"], attrs["conversation_id"] = v.RunID, v.ConversationID
		_, err = store.AppendInTx(tx, ev)
		return err
	})
	if err != nil {
		return Version{}, err
	}
	s.st.NotifyBus()
	if v.Approval == ApprovalPending {
		s.askApproval(ctx, a, v.N, prevLive, v.Live)
	}
	return v, nil
}

// writeArtifactRow inserts a new artifact or moves an artifact to its next
// version. A concurrent save of the same version fails as stale.
func writeArtifactRow(ctx context.Context, tx *sql.Tx, a *Artifact, create bool, ts string) error {
	if create {
		_, err := tx.ExecContext(ctx, `INSERT INTO artifacts (id, workspace_id, owner_kind, owner_id, name, title, kind,
  live, latest_version, live_version, revision, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,1,?,1,?,?)`,
			a.ID, a.Workspace, a.OwnerKind, a.OwnerID, a.Name, a.Title, string(a.Kind), a.Live, nullInt(a.LiveVersion), ts, ts)
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE artifacts SET latest_version=?, title=?, kind=?, live=?, live_version=?, revision=?,
  updated_at=? WHERE id=? AND latest_version=?`, a.LatestVersion, a.Title, string(a.Kind), a.Live, nullInt(a.LiveVersion),
		a.Revision, ts, a.ID, a.LatestVersion-1)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &StaleError{Latest: a.LatestVersion}
	}
	return nil
}

// current finds the artifact that o adds a version to, or nil.
func (s *Service) current(ctx context.Context, tx *sql.Tx, o PutOpts) (*Artifact, error) {
	var row *sql.Row
	if o.ArtifactID != "" {
		row = tx.QueryRowContext(ctx, artifactSelect+` WHERE id=? AND workspace_id=?`, o.ArtifactID, o.Workspace)
	} else {
		row = tx.QueryRowContext(ctx, artifactSelect+` WHERE workspace_id=? AND owner_kind=? AND owner_id=? AND name=?`,
			o.Workspace, o.Origin.OwnerKind, o.Origin.OwnerID, o.Name)
	}
	a, err := scanArtifact(row)
	if errors.Is(err, sql.ErrNoRows) {
		if o.ArtifactID != "" {
			return nil, ErrNotFound
		}
		return nil, nil
	}
	return &a, err
}

// checkStale applies D4.
func (s *Service) checkStale(ctx context.Context, tx *sql.Tx, a *Artifact, o PutOpts) error {
	if o.Base != nil {
		if *o.Base != a.LatestVersion {
			return &StaleError{Latest: a.LatestVersion}
		}
		return nil
	}
	var kind, runID, convID string
	if err := tx.QueryRowContext(ctx, `SELECT origin_kind, run_id, conversation_id FROM artifact_versions WHERE artifact_id=? AND n=?`,
		a.ID, a.LatestVersion).Scan(&kind, &runID, &convID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // the newest version was pruned
		}
		return err
	}
	if !o.Origin.same(kind, runID, convID) {
		return &StaleError{Latest: a.LatestVersion}
	}
	return nil
}

func (s *Service) event(topic, action string, a *Artifact, n int) store.BusEvent {
	return store.BusEvent{
		Topic: topic, Entity: "artifact", EntityID: a.ID, Action: action, Revision: a.Revision,
		Attrs: map[string]any{
			"workspace": a.Workspace, "artifact_id": a.ID, "name": a.Name, "version": n, "kind": string(a.Kind),
			"live": a.Live,
		},
	}
}

func (s *Service) ids(ctx context.Context, out *[]string, q string, args ...any) error {
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		*out = append(*out, id)
	}
	return rows.Err()
}

// runOrigin is the origin of a run. The run of an agent job belongs to the
// agent. A run inside a conversation saves with a conversation origin (C6).
func (s *Service) runOrigin(ctx context.Context, a *tools.Actor) Origin {
	o := Origin{Kind: OriginRun, RunID: a.RunID, Author: a.Job, OwnerKind: OwnerJob, OwnerID: a.JobID}
	if s.runs != nil {
		if run, err := s.runs.Get(ctx, a.RunID); err == nil && run.ConversationID != "" {
			o.Kind, o.ConversationID = OriginConversation, run.ConversationID
		}
	}
	switch a.Kind {
	case OwnerAgent:
		o.OwnerKind = OwnerAgent
	case OwnerAssistant:
		o.OwnerKind, o.OwnerID, o.Author = OwnerAssistant, "", OwnerAssistant
	default:
		if s.jobs != nil && a.JobID != "" {
			if row, err := s.jobs.JobByID(ctx, a.JobID); err == nil && row.OwnerID != "" {
				o.OwnerKind, o.OwnerID = OwnerAgent, row.OwnerID
			}
		}
	}
	return o
}

// where is the SQL filter of f.
func (f ListFilter) where() (string, []any, error) {
	q := ` WHERE 1=1`
	var args []any
	add := func(cond string, v any) {
		q += " AND " + cond
		args = append(args, v)
	}
	if f.Workspace != "" {
		add("workspace_id=?", f.Workspace)
	}
	if f.OwnerKind != "" {
		add("owner_kind=?", f.OwnerKind)
		if f.OwnerKind != OwnerAssistant && f.OwnerID != "" {
			add("owner_id=?", f.OwnerID)
		}
	}
	if f.Conversation != "" {
		q += " AND (conversation_id=? OR EXISTS (SELECT 1 FROM artifact_versions v WHERE v.artifact_id = artifacts.id AND v.conversation_id=?))"
		args = append(args, f.Conversation, f.Conversation)
	}
	if f.Kind != "" {
		add("kind=?", string(f.Kind))
	}
	if f.Live != nil {
		add("live=?", *f.Live)
	}
	if f.Query != "" {
		like := "%" + strings.ToLower(f.Query) + "%"
		q += " AND (LOWER(name) LIKE ? OR LOWER(title) LIKE ?)"
		args = append(args, like, like)
	}
	if f.Cursor != "" {
		at, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return "", nil, err
		}
		q += " AND (updated_at < ? OR (updated_at = ? AND id < ?))"
		args = append(args, at, at, id)
	}
	return q, args, nil
}
