// Package workspaces manages the workspace registry and job index rebuild.
package workspaces

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
	"gopkg.in/yaml.v3"
)

// Workspace wire shape.
type Workspace struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	GitEnabled bool      `json:"git_enabled"`
	CreatedAt  time.Time `json:"created_at"`
	Watching   bool      `json:"watching"`
	Orphaned   bool      `json:"orphaned,omitempty"`
	Assistant  *string   `json:"assistant,omitempty"` // nickname if TREBI.md present
}

// Service owns workspace registry rows.
type Service struct {
	st  *store.Store
	log *slog.Logger
}

// New creates a workspace service.
func New(st *store.Store) *Service {
	return &Service{st: st, log: slog.Default().With("logger", "workspaces")}
}

// trebiYAML is the on-disk workspace config (B26).
type trebiYAML struct {
	ID         string `yaml:"id"`
	Name       string `yaml:"name"`
	GitEnabled bool   `yaml:"git_enabled"`
}

// Add registers a workspace folder, minting id into trebi.yaml if missing.
func (s *Service) Add(ctx context.Context, path, name string) (*Workspace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", abs)
	}

	id, dispName, gitEn, err := ensureTrebiYAML(abs, name)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	// Reject id collision with a different path (copied trebi.yaml).
	var existingPath string
	scanErr := s.st.DB().QueryRowContext(ctx, `SELECT path FROM workspaces WHERE id=?`, id).Scan(&existingPath)
	switch {
	case scanErr == nil && existingPath != abs:
		return nil, fmt.Errorf("workspace id %s already registered at %s", id, existingPath)
	case scanErr == nil:
		// Same path: idempotent no-op, no insert or event.
	case scanErr == sql.ErrNoRows:
		_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO workspaces (id, name, path, git_enabled, created_at, updated_at)
				VALUES (?,?,?,?,?,?)`,
				id, dispName, abs, boolInt(gitEn), nowStr, nowStr)
			return err
		}, store.BusEvent{
			Topic: "workspace.created", Entity: "workspace", EntityID: id, Action: "created", Revision: 1,
			Attrs: map[string]string{"path": abs, "name": dispName},
		})
		if err != nil {
			return nil, err
		}
	default:
		return nil, scanErr
	}
	ws := &Workspace{ID: id, Name: dispName, Path: abs, GitEnabled: gitEn, CreatedAt: now, Watching: true}
	if err := s.RebuildIndex(ctx, ws); err != nil {
		return ws, err // still registered
	}
	return ws, nil
}

// ErrNotFound reports a workspace id that is not registered.
var ErrNotFound = errors.New("workspace not found")

// Remove deletes a workspace row and its job index, and emits workspace.deleted.
// An unknown id returns ErrNotFound and emits nothing.
func (s *Service) Remove(ctx context.Context, id string) error {
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM job_index WHERE workspace_id=?`, id); err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM workspaces WHERE id=?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	}, store.BusEvent{
		Topic: "workspace.deleted", Entity: "workspace", EntityID: id, Action: "deleted", Revision: 1,
	})
	return err
}

// List returns all workspaces.
func (s *Service) List(ctx context.Context) ([]Workspace, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, name, path, git_enabled, orphaned, created_at FROM workspaces ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		var git, orphan int
		var created string
		if err := rows.Scan(&w.ID, &w.Name, &w.Path, &git, &orphan, &created); err != nil {
			return nil, err
		}
		w.GitEnabled = git == 1
		w.Orphaned = orphan == 1
		w.CreatedAt, _ = time.Parse(time.RFC3339, created)
		w.Watching = !w.Orphaned
		// Assistant nickname from TREBI.md if present.
		if _, err := os.Stat(filepath.Join(w.Path, "TREBI.md")); err == nil {
			n := w.Name
			w.Assistant = &n
			if a, err := job.ParseFile(filepath.Join(w.Path, "TREBI.md")); err == nil && a.Name != "" {
				w.Assistant = &a.Name
			}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Get returns one workspace by id.
func (s *Service) Get(ctx context.Context, id string) (*Workspace, error) {
	list, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

// GetByName finds by display name.
func (s *Service) GetByName(ctx context.Context, name string) (*Workspace, error) {
	list, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

// indexRow is one desired or existing job_index row for the diff.
type indexRow struct {
	name, path, kind, harness, hash, parseError string
	active                                      bool
}

func rowFromJob(j *job.Job, kind string) indexRow {
	return indexRow{
		name: j.Name, path: j.FilePath, kind: kind, harness: j.Harness,
		hash: j.Hash, parseError: j.ParseError, active: kind == "assistant" || !j.Disabled,
	}
}

// RebuildIndex rescans job files into job_index (diff-aware, B5).
// It emits job.created/job.updated/job.deleted and assistant.updated bus
// events for changes. Bus errors never fail the rebuild.
func (s *Service) RebuildIndex(ctx context.Context, ws *Workspace) error {
	otherRoots, err := s.otherWorkspaceRoots(ctx, ws.ID)
	if err != nil {
		return err
	}
	jobs, assistant, err := job.Discover(ws.Path, otherRoots...)
	if err != nil {
		return err
	}
	existing, err := s.existingIndexRows(ctx, ws.ID)
	if err != nil {
		return err
	}
	desired := map[string]indexRow{}
	for _, j := range jobs {
		desired[j.Name] = rowFromJob(j, "job")
	}
	if assistant != nil {
		desired[assistant.Name] = rowFromJob(assistant, "assistant")
	}
	if err := s.writeIndexDiff(ctx, ws.ID, existing, desired); err != nil {
		return err
	}
	s.emitIndexDiff(ctx, ws.ID, existing, desired)
	return nil
}

// ListJobs returns job_index rows for a workspace (empty workspace = all).
func (s *Service) ListJobs(ctx context.Context, workspaceID string) ([]JobRow, error) {
	q := `SELECT workspace_id, name, path, kind, active, COALESCE(harness,''), COALESCE(hash,''), COALESCE(parse_error,'') FROM job_index`
	var args []any
	if workspaceID != "" {
		q += ` WHERE workspace_id=?`
		args = append(args, workspaceID)
	}
	q += ` ORDER BY name`
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		var r JobRow
		var active int
		if err := rows.Scan(&r.WorkspaceID, &r.Name, &r.Path, &r.Kind, &active, &r.Harness, &r.Hash, &r.ParseError); err != nil {
			return nil, err
		}
		r.Active = active == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// existingIndexRows loads current job_index rows by name.
func (s *Service) existingIndexRows(ctx context.Context, wsID string) (map[string]indexRow, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT name, path, kind, active, COALESCE(harness,''), COALESCE(hash,''), COALESCE(parse_error,'')
		FROM job_index WHERE workspace_id=?`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	existing := map[string]indexRow{}
	for rows.Next() {
		var r indexRow
		var active int
		if err := rows.Scan(&r.name, &r.path, &r.kind, &active, &r.harness, &r.hash, &r.parseError); err != nil {
			return nil, err
		}
		r.active = active == 1
		existing[r.name] = r
	}
	return existing, rows.Err()
}

// writeIndexDiff upserts changed rows and deletes stale ones in one tx.
func (s *Service) writeIndexDiff(ctx context.Context, wsID string, existing, desired map[string]indexRow) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.st.WithTx(ctx, func(tx *sql.Tx) error {
		for name, want := range desired {
			if have, ok := existing[name]; ok && have == want {
				continue // unchanged: no row churn
			}
			active := 0
			if want.active {
				active = 1
			}
			var pe *string
			if want.parseError != "" {
				pe = &want.parseError
			}
			if _, err := tx.Exec(`INSERT INTO job_index (workspace_id, name, path, kind, active, harness, hash, parse_error, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?)
				ON CONFLICT(workspace_id, name) DO UPDATE SET
					path=excluded.path, kind=excluded.kind, active=excluded.active,
					harness=excluded.harness, hash=excluded.hash,
					parse_error=excluded.parse_error, updated_at=excluded.updated_at`,
				wsID, name, want.path, want.kind, active, want.harness, want.hash, pe, now); err != nil {
				return err
			}
		}
		for name := range existing {
			if _, ok := desired[name]; !ok {
				if _, err := tx.Exec(`DELETE FROM job_index WHERE workspace_id=? AND name=?`, wsID, name); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// emitIndexDiff appends bus events for index changes (best-effort).
func (s *Service) emitIndexDiff(ctx context.Context, wsID string, existing, desired map[string]indexRow) {
	emit := func(ev store.BusEvent) {
		if _, err := s.st.Append(ctx, ev); err != nil {
			s.log.Warn("job index bus append failed", "workspace", wsID, "topic", ev.Topic, "error", err)
		}
	}
	jobEvent := func(action, name string) store.BusEvent {
		return store.BusEvent{
			Topic: "job." + action, Entity: "job", EntityID: name, Action: action,
			Attrs: map[string]string{"workspace": wsID, "job": name},
		}
	}
	for name, want := range desired {
		have, ok := existing[name]
		if want.kind == "assistant" {
			if !ok || have.hash != want.hash {
				emit(store.BusEvent{
					Topic: "assistant.updated", Entity: "assistant", EntityID: name, Action: "updated",
					Attrs: map[string]string{"workspace": wsID, "name": name},
				})
			}
			continue
		}
		switch {
		case !ok:
			emit(jobEvent("created", name))
		case have.hash != want.hash:
			emit(jobEvent("updated", name))
		}
	}
	for name, have := range existing {
		if _, ok := desired[name]; ok {
			continue
		}
		if have.kind == "assistant" {
			emit(store.BusEvent{
				Topic: "assistant.updated", Entity: "assistant", EntityID: name, Action: "updated",
				Attrs: map[string]string{"workspace": wsID, "name": name},
			})
			continue
		}
		emit(jobEvent("deleted", name))
	}
}

// otherWorkspaceRoots returns paths of all registered workspaces except id.
func (s *Service) otherWorkspaceRoots(ctx context.Context, id string) ([]string, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT path FROM workspaces WHERE id<>?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// JobRow is a job_index entry.
type JobRow struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Active      bool   `json:"active"`
	Harness     string `json:"harness"`
	Hash        string `json:"hash"`
	ParseError  string `json:"parse_error,omitempty"`
}

func ensureTrebiYAML(dir, name string) (id, dispName string, git bool, err error) {
	path := filepath.Join(dir, "trebi.yaml")
	data, readErr := os.ReadFile(path)
	if readErr == nil {
		var y trebiYAML
		if err := yaml.Unmarshal(data, &y); err != nil {
			return "", "", false, err
		}
		if y.ID == "" {
			return "", "", false, fmt.Errorf("trebi.yaml missing id")
		}
		if y.Name == "" {
			y.Name = filepath.Base(dir)
		}
		return y.ID, y.Name, y.GitEnabled, nil
	}
	if !os.IsNotExist(readErr) {
		return "", "", false, readErr
	}
	id = mintID()
	if name == "" {
		name = filepath.Base(dir)
	}
	y := trebiYAML{ID: id, Name: name, GitEnabled: false}
	b, err := yaml.Marshal(&y)
	if err != nil {
		return "", "", false, err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", "", false, err
	}
	return id, name, false, nil
}

func mintID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "ws_" + hex.EncodeToString(b[:])
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
