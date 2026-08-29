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
	"strings"
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
	Revision   int64     `json:"revision"`
}

// Service owns workspace registry rows.
type Service struct {
	st   *store.Store
	root string // workspaces.root; empty refuses pathless create
	log  *slog.Logger
}

// New creates a workspace service.
func New(st *store.Store) *Service {
	return &Service{st: st, log: slog.Default().With("source", "workspaces")}
}

// WithRoot injects workspaces.root (already expanded). Empty refuses pathless create.
func (s *Service) WithRoot(root string) *Service {
	s.root = strings.TrimSpace(root)
	return s
}

// AddOpts is the register-workspace request.
type AddOpts struct {
	Path   string
	Name   string
	Create bool
}

// Patch is a partial workspace update. Nil fields are left unchanged.
type Patch struct {
	Name       *string
	GitEnabled *bool
}

// trebiYAML is the on-disk workspace config (B26).
type trebiYAML struct {
	ID         string `yaml:"id"`
	Name       string `yaml:"name"`
	GitEnabled bool   `yaml:"git_enabled"`
}

var (
	// ErrNotFound reports a workspace id that is not registered.
	ErrNotFound = errors.New("workspace not found")
	// ErrNested reports a target inside an existing workspace.
	ErrNested = errors.New("target is inside an existing workspace")
	// ErrContains reports a target that contains an existing workspace.
	ErrContains = errors.New("target contains an existing workspace")
	// ErrNoRoot reports pathless create with workspaces.root unset.
	ErrNoRoot = errors.New("workspaces.root is not configured")
	// ErrAncestorFile reports create under a path whose nearest ancestor is a file.
	ErrAncestorFile = errors.New("nearest existing ancestor is a file")
)

// Add registers a workspace folder, minting id into trebi.yaml if missing.
func (s *Service) Add(ctx context.Context, path, name string) (*Workspace, error) {
	return s.AddWith(ctx, AddOpts{Path: path, Name: name})
}

// AddWith registers a workspace, optionally creating the directory.
func (s *Service) AddWith(ctx context.Context, opts AddOpts) (*Workspace, error) {
	abs, err := s.resolveAddPath(opts)
	if err != nil {
		return nil, err
	}
	if err := s.prepareDir(abs, opts.Create); err != nil {
		return nil, err
	}
	if err := s.rejectNested(ctx, abs); err != nil {
		return nil, err
	}

	id, dispName, gitEn, err := ensureTrebiYAML(abs, opts.Name)
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
	ws := &Workspace{ID: id, Name: dispName, Path: abs, GitEnabled: gitEn, CreatedAt: now, Watching: true, Revision: 1}
	if err := s.RebuildIndex(ctx, ws); err != nil {
		return ws, err // still registered
	}
	return ws, nil
}

func validateWorkspaceName(name string) error {
	if name == "" {
		return fmt.Errorf("name required for pathless create")
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("invalid workspace name %q", name)
	}
	return nil
}

func nearestExisting(p string) (string, os.FileInfo, error) {
	cur := filepath.Clean(p)
	for {
		st, err := os.Lstat(cur)
		if err == nil {
			return cur, st, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", nil, fmt.Errorf("no existing ancestor for %s", p)
		}
		cur = parent
	}
}

func pathInside(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if target == root {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Rename sets the display name. Prefer Patch when git_enabled is also changing.
func (s *Service) Rename(ctx context.Context, id, name string) (*Workspace, error) {
	return s.Patch(ctx, id, Patch{Name: &name})
}

// Patch writes trebi.yaml first, then the registry row and workspace.updated.
func (s *Service) Patch(ctx context.Context, id string, p Patch) (*Workspace, error) {
	if p.Name == nil && p.GitEnabled == nil {
		ws, err := s.Get(ctx, id)
		if err != nil {
			return nil, ErrNotFound
		}
		return ws, nil
	}
	cur, err := s.Get(ctx, id)
	if err != nil {
		return nil, ErrNotFound
	}
	name := cur.Name
	git := cur.GitEnabled
	if p.Name != nil {
		name = strings.TrimSpace(*p.Name)
		if name == "" {
			return nil, fmt.Errorf("name must not be empty")
		}
	}
	if p.GitEnabled != nil {
		git = *p.GitEnabled
	}
	if err := writeTrebiYAML(cur.Path, cur.ID, name, git); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var rev int64
	s.st.LockBus()
	defer s.st.UnlockBus()
	err = s.st.WithTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT revision FROM workspaces WHERE id=?`, id).Scan(&rev)
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rev++
		if _, err := tx.Exec(`UPDATE workspaces SET name=?, git_enabled=?, updated_at=?, revision=? WHERE id=?`,
			name, boolInt(git), now, rev, id); err != nil {
			return err
		}
		_, err = store.AppendInTx(tx, store.BusEvent{
			Topic: "workspace.updated", Entity: "workspace", EntityID: id, Action: "updated",
			Revision: rev,
			Attrs:    map[string]string{"name": name, "git_enabled": fmt.Sprintf("%t", git)},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Remove unregisters a workspace, deletes its folder, and emits workspace.deleted.
// An unknown id returns ErrNotFound and emits nothing.
func (s *Service) Remove(ctx context.Context, id string) error {
	ws, err := s.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
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
	if err != nil {
		return err
	}
	if err := refuseFolderDelete(ws.Path, s.root); err != nil {
		s.log.Warn("workspace unregistered; folder kept", "id", id, "path", ws.Path, "error", err)
		return nil
	}
	if err := os.RemoveAll(ws.Path); err != nil {
		return fmt.Errorf("unregistered %s but failed to delete %s: %w", id, ws.Path, err)
	}
	return nil
}

func refuseFolderDelete(path, root string) error {
	path = filepath.Clean(path)
	if path == "" || path == "." || path == string(filepath.Separator) {
		return fmt.Errorf("refusing to delete %q", path)
	}
	if root != "" && filepath.Clean(root) == path {
		return fmt.Errorf("refusing to delete workspaces.root %s", path)
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == path {
		return fmt.Errorf("refusing to delete home directory %s", path)
	}
	return nil
}

// List returns all workspaces.
func (s *Service) List(ctx context.Context) ([]Workspace, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, name, path, git_enabled, orphaned, created_at, revision FROM workspaces ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		var git, orphan int
		var created string
		if err := rows.Scan(&w.ID, &w.Name, &w.Path, &git, &orphan, &created, &w.Revision); err != nil {
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

func (s *Service) resolveAddPath(opts AddOpts) (string, error) {
	path := strings.TrimSpace(opts.Path)
	name := strings.TrimSpace(opts.Name)
	if path == "" {
		if !opts.Create {
			return "", fmt.Errorf("path required")
		}
		if s.root == "" {
			return "", ErrNoRoot
		}
		if err := validateWorkspaceName(name); err != nil {
			return "", err
		}
		path = filepath.Join(s.root, name)
	}
	return filepath.Abs(path)
}

func (s *Service) prepareDir(abs string, create bool) error {
	info, err := os.Stat(abs)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("not a directory: %s", abs)
		}
		return nil
	}
	if !create {
		return err
	}
	anc, st, err := nearestExisting(abs)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%w: %s", ErrAncestorFile, anc)
	}
	return os.MkdirAll(abs, 0o755)
}

func (s *Service) rejectNested(ctx context.Context, abs string) error {
	roots, err := s.otherWorkspaceRoots(ctx, "")
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	for _, root := range roots {
		root = filepath.Clean(root)
		if root == abs {
			continue
		}
		if pathInside(root, abs) {
			return fmt.Errorf("%w: %s", ErrNested, root)
		}
		if pathInside(abs, root) {
			return fmt.Errorf("%w: %s", ErrContains, root)
		}
	}
	return nil
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

func writeTrebiYAML(dir, id, name string, git bool) error {
	y := trebiYAML{ID: id, Name: name, GitEnabled: git}
	b, err := yaml.Marshal(&y)
	if err != nil {
		return err
	}
	return job.WriteAtomic(filepath.Join(dir, "trebi.yaml"), b)
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

const (
	wakeIndexCap = 8 * 1024
	wakeRecapCap = 8 * 1024
	truncMark    = "\n…[truncated]"
)

// WakeOpts is the extra material folded into a composed assistant wake prompt.
type WakeOpts struct {
	ExtraPrompt string
	Recap       string
	Message     string
}

// ComposeWakePrompt builds the B27 assistant prompt:
// TREBI.md body → job index → MEMORY.md → recap → triggering message / extra.
func (s *Service) ComposeWakePrompt(ctx context.Context, wsID string, opts WakeOpts) (string, error) {
	ws, err := s.Get(ctx, wsID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	writeSection(&b, "Instructions", trebiBody(ws.Path))
	writeSection(&b, "Workspace jobs", capSection(s.jobIndex(ctx, wsID), wakeIndexCap))
	writeSection(&b, "Memory", memoryBody(ws.Path))
	if recap := strings.TrimSpace(opts.Recap); recap != "" {
		writeSection(&b, "Conversation recap", capSection(recap, wakeRecapCap))
	}
	msg := strings.TrimSpace(opts.Message)
	if msg == "" {
		msg = strings.TrimSpace(opts.ExtraPrompt)
	}
	if msg != "" {
		writeSection(&b, "Current message", msg)
	}
	return strings.TrimSpace(b.String()), nil
}

func trebiBody(wsPath string) string {
	j, err := job.ParseFile(filepath.Join(wsPath, "TREBI.md"))
	if err != nil || j == nil || strings.TrimSpace(j.Prompt) == "" {
		return "(none)"
	}
	return strings.TrimSpace(j.Prompt)
}

func memoryBody(wsPath string) string {
	data, err := os.ReadFile(filepath.Join(wsPath, "MEMORY.md"))
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return "(empty)"
	}
	return strings.TrimSpace(string(data))
}

func (s *Service) jobIndex(ctx context.Context, wsID string) string {
	jobs, err := s.ListJobs(ctx, wsID)
	if err != nil || len(jobs) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, row := range jobs {
		active := "inactive"
		if row.Active {
			active = "active"
		}
		fmt.Fprintf(&b, "- %s kind=%s harness=%s %s", row.Name, row.Kind, row.Harness, active)
		if row.ParseError != "" {
			fmt.Fprintf(&b, " parse_error=%s", row.ParseError)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func writeSection(b *strings.Builder, title, body string) {
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	fmt.Fprintf(b, "# %s\n%s", title, strings.TrimSpace(body))
}

func capSection(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + truncMark
}
