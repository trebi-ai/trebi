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
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"gopkg.in/yaml.v3"
)

// Workspace wire shape.
type Workspace struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Handle     string    `json:"handle"` // fixed name in the email address
	Path       string    `json:"path"`
	GitEnabled bool      `json:"git_enabled"`
	CreatedAt  time.Time `json:"created_at"`
	Watching   bool      `json:"watching"`
	Orphaned   bool      `json:"orphaned,omitempty"`
	Assistant  *string   `json:"assistant,omitempty"` // nickname from trebi.yaml assistant.name
	Revision   int64     `json:"revision"`
	// Contact and Notify are the workspace route layer from trebi.yaml.
	// NotifySet is true when the file holds notify, also as an empty list.
	Contact   []string      `json:"contact,omitempty"`
	Notify    []notify.Step `json:"notify,omitempty"`
	NotifySet bool          `json:"notify_set"`
	// JobCount and RunningCount are filled by list reads only.
	JobCount     int `json:"job_count"`
	RunningCount int `json:"running_count"`
}

// SenderName is the From name of mail the workspace assistant sends: the
// assistant nickname, else the workspace name.
func (w Workspace) SenderName() string {
	if w.Assistant != nil && strings.TrimSpace(*w.Assistant) != "" {
		return strings.TrimSpace(*w.Assistant)
	}
	return w.Name
}

// Service owns workspace registry rows.
type Service struct {
	st            *store.Store
	root          string // workspaces.root; empty refuses pathless create
	log           *slog.Logger
	mu            sync.Mutex
	fp            map[string]string
	jobs          map[string][]*job.Job
	budgetAlerter BudgetAlerter

	asstMu     sync.Mutex
	asstWrites map[string]string // workspace id -> trebi.yaml hash of the last API write
}

// New creates a workspace service.
func New(st *store.Store) *Service {
	return &Service{
		st: st, log: slog.Default().With("source", "workspaces"),
		fp: map[string]string{}, jobs: map[string][]*job.Job{},
	}
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
	Handle string // empty derives one from the name
	Create bool
}

// Patch is a partial workspace update. Nil fields are left unchanged. A
// Clear flag removes the key from trebi.yaml, so the layer inherits.
type Patch struct {
	Name         *string
	GitEnabled   *bool
	Contact      *[]string
	Notify       *[]notify.Step
	ClearContact bool
	ClearNotify  bool
}

func (p Patch) touchesRoute() bool {
	return p.Contact != nil || p.Notify != nil || p.ClearContact || p.ClearNotify
}

// trebiYAML is the on-disk workspace config (B26).
type trebiYAML struct {
	ID         string                       `yaml:"id"`
	Name       string                       `yaml:"name"`
	GitEnabled bool                         `yaml:"git_enabled"`
	Email      *trebiYAMLEmail              `yaml:"email,omitempty" json:"email,omitempty"`
	Assistant  *wire.Override               `yaml:"assistant,omitempty" json:"assistant,omitempty"`
	Contact    yaml.Node                    `yaml:"contact,omitempty"`
	Notify     yaml.Node                    `yaml:"notify,omitempty"`
	Connectors map[string]ConnectorOverride `yaml:"connectors,omitempty"`
}

// route parses the contact and the notify route.
func (y *trebiYAML) route() (route []notify.Step, set bool, contact []string, err error) {
	if route, err = notify.ParseSteps(&y.Notify); err != nil {
		return nil, false, nil, fmt.Errorf("trebi.yaml: %w", err)
	}
	if contact, err = notify.ParseContact(&y.Contact); err != nil {
		return nil, false, nil, fmt.Errorf("trebi.yaml: %w", err)
	}
	return route, route != nil, contact, nil
}

// applyRoute writes the route fields of p.
func (y *trebiYAML) applyRoute(p Patch) error {
	switch {
	case p.ClearContact:
		y.Contact = yaml.Node{}
	case p.Contact != nil:
		y.Contact = yaml.Node{}
		if len(*p.Contact) > 0 {
			if err := y.Contact.Encode(*p.Contact); err != nil {
				return err
			}
		}
	}
	switch {
	case p.ClearNotify:
		y.Notify = yaml.Node{}
	case p.Notify != nil:
		if err := notify.ValidateSteps(*p.Notify); err != nil {
			return err
		}
		y.Notify = yaml.Node{}
		steps := append([]notify.Step{}, *p.Notify...)
		if err := y.Notify.Encode(steps); err != nil {
			return err
		}
	}
	return nil
}

// trebiYAMLEmail carries the workspace's own email additions (plan F):
// allowlist extends the operator list for inbound mail. Not a secret, so
// it travels with the folder.
type trebiYAMLEmail struct {
	Allowlist []string `yaml:"allowlist,omitempty" json:"allowlist,omitempty"`
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
	// ErrDestExists reports a move destination that already has content.
	ErrDestExists = errors.New("destination exists")
	// ErrDestInside reports a move destination inside the workspace itself.
	ErrDestInside = errors.New("destination is inside the workspace")
	// ErrSrcInsideDest reports a move destination that contains the workspace.
	ErrSrcInsideDest = errors.New("destination contains the workspace")
	// ErrSamePath reports a move to the current path.
	ErrSamePath = errors.New("destination is the current path")
	// ErrOrphaned reports a move of a history-only workspace with no folder.
	ErrOrphaned = errors.New("orphaned workspace has no folder")
	// ErrMissing reports both the current path and the destination are absent.
	ErrMissing = errors.New("workspace folder is missing")
	// ErrRebindID reports a rebind destination whose trebi.yaml id does not match.
	ErrRebindID = errors.New("destination trebi.yaml id does not match")
	// ErrHandle reports a handle outside the address grammar.
	ErrHandle = errors.New("handle must be 2 to 20 characters: a-z, 0-9, and inner dashes")
)

var handleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,18}[a-z0-9]$`)

// ValidateHandle checks the handle grammar of the email address.
func ValidateHandle(h string) error {
	if !handleRe.MatchString(h) {
		return fmt.Errorf("%w: %q", ErrHandle, h)
	}
	return nil
}

// HandleFor derives a handle from a display name: lowercase, [a-z0-9-],
// collapsed dashes, at most 20 characters. A name with no usable
// characters gives "ws".
func HandleFor(name string) string {
	var sb strings.Builder
	prevDash := true // also trims leading dashes
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			prevDash = false
		case !prevDash:
			sb.WriteByte('-')
			prevDash = true
		}
		if sb.Len() >= 20 {
			break
		}
	}
	out := strings.Trim(sb.String(), "-")
	if len(out) < 2 {
		return "ws"
	}
	return out
}

// Add registers a workspace folder, minting id into trebi.yaml if missing.
func (s *Service) Add(ctx context.Context, path, name string) (*Workspace, error) {
	return s.AddWith(ctx, AddOpts{Path: path, Name: name})
}

// AddWith registers a workspace, optionally creating the directory.
func (s *Service) AddWith(ctx context.Context, opts AddOpts) (*Workspace, error) {
	if opts.Handle != "" {
		if err := ValidateHandle(opts.Handle); err != nil {
			return nil, err
		}
	}
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

	handle := opts.Handle
	if handle == "" {
		handle = HandleFor(dispName)
	}
	// Reject id collision with a different path (copied trebi.yaml).
	var existingPath, existingHandle string
	scanErr := s.st.DB().QueryRowContext(ctx, `SELECT path, handle FROM workspaces WHERE id=?`, id).
		Scan(&existingPath, &existingHandle)
	switch {
	case scanErr == nil && existingPath != abs:
		return nil, fmt.Errorf("workspace id %s already registered at %s", id, existingPath)
	case scanErr == nil:
		// Same path: idempotent no-op, no insert or event. The handle stays.
		handle = existingHandle
	case scanErr == sql.ErrNoRows:
		_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO workspaces (id, name, handle, path, git_enabled, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?)`,
				id, dispName, handle, abs, boolInt(gitEn), nowStr, nowStr)
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
	ws := &Workspace{ID: id, Name: dispName, Handle: handle, Path: abs, GitEnabled: gitEn, CreatedAt: now, Watching: true, Revision: 1}
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

// PathInside reports whether target is strictly inside root (not equal to it).
func PathInside(root, target string) bool {
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
	if p.Name == nil && p.GitEnabled == nil && !p.touchesRoute() {
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
	if err := writeTrebiYAML(cur.Path, cur.ID, name, git, p); err != nil {
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

// SetHandle replaces the handle. Only a new email address calls it: the
// handle is fixed while the address lives.
func (s *Service) SetHandle(ctx context.Context, id, handle string) (*Workspace, error) {
	if err := ValidateHandle(handle); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	s.st.LockBus()
	defer s.st.UnlockBus()
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		var rev int64
		err := tx.QueryRow(`SELECT revision FROM workspaces WHERE id=?`, id).Scan(&rev)
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rev++
		if _, err := tx.Exec(`UPDATE workspaces SET handle=?, updated_at=?, revision=? WHERE id=?`,
			handle, now, rev, id); err != nil {
			return err
		}
		_, err = store.AppendInTx(tx, store.BusEvent{
			Topic: "workspace.updated", Entity: "workspace", EntityID: id, Action: "updated",
			Revision: rev, Attrs: map[string]string{"handle": handle},
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

const workspaceCols = `id, name, handle, path, git_enabled, orphaned, created_at, revision`

func scanWorkspace(sc interface{ Scan(...any) error }) (Workspace, error) {
	var w Workspace
	var git, orphan int
	var created string
	if err := sc.Scan(&w.ID, &w.Name, &w.Handle, &w.Path, &git, &orphan, &created, &w.Revision); err != nil {
		return Workspace{}, err
	}
	w.GitEnabled = git == 1
	w.Orphaned = orphan == 1
	w.CreatedAt, _ = time.Parse(time.RFC3339, created)
	w.Watching = !w.Orphaned
	return w, nil
}

func (s *Service) decorateWorkspaces(ctx context.Context, list []Workspace) ([]Workspace, error) {
	counts, err := s.jobCounts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].JobCount = counts[list[i].ID]
		if route, set, contact, rerr := s.Route(&list[i]); rerr == nil {
			list[i].Notify, list[i].NotifySet, list[i].Contact = route, set, contact
		}
		ov, _, oerr := s.AssistantOverride(ctx, list[i].ID)
		if oerr != nil || ov.Name == nil || *ov.Name == "" {
			continue
		}
		name := *ov.Name
		list[i].Assistant = &name
	}
	return list, nil
}

// List returns all workspaces.
func (s *Service) List(ctx context.Context) ([]Workspace, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+workspaceCols+` FROM workspaces ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.decorateWorkspaces(ctx, out)
}

// Get returns one workspace by id.
func (s *Service) Get(ctx context.Context, id string) (*Workspace, error) {
	row := s.st.DB().QueryRowContext(ctx, `SELECT `+workspaceCols+` FROM workspaces WHERE id=?`, id)
	w, err := scanWorkspace(row)
	if err != nil {
		return nil, err
	}
	out, err := s.decorateWorkspaces(ctx, []Workspace{w})
	if err != nil {
		return nil, err
	}
	return &out[0], nil
}

// GetByName finds by display name (case-insensitive).
func (s *Service) GetByName(ctx context.Context, name string) (*Workspace, error) {
	row := s.st.DB().QueryRowContext(ctx, `SELECT `+workspaceCols+` FROM workspaces WHERE LOWER(name)=LOWER(?) ORDER BY name LIMIT 1`, name)
	w, err := scanWorkspace(row)
	if err != nil {
		return nil, err
	}
	out, err := s.decorateWorkspaces(ctx, []Workspace{w})
	if err != nil {
		return nil, err
	}
	return &out[0], nil
}

// Resolve finds a workspace by id or display name (case-insensitive name).
func (s *Service) Resolve(ctx context.Context, idOrName string) (*Workspace, error) {
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		return nil, sql.ErrNoRows
	}
	if ws, err := s.Get(ctx, idOrName); err == nil {
		return ws, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return s.GetByName(ctx, idOrName)
}

// indexRow is one desired or existing live job_index row for the diff.
type indexRow struct {
	id, name, path, kind, harness, hash, parseError, ownerID string
	active                                                   bool
}

// RebuildIndex rescans job files into job_index (diff-aware, B5).
// It emits job.*/assistant.*/agent.* bus events for changes.
// Bus errors never fail the rebuild.
func (s *Service) RebuildIndex(ctx context.Context, ws *Workspace) error {
	_, err := s.RebuildIndexJobs(ctx, ws)
	return err
}

// RebuildIndexJobs is RebuildIndex plus the parsed jobs (index names already set).
func (s *Service) RebuildIndexJobs(ctx context.Context, ws *Workspace) ([]*job.Job, error) {
	otherRoots, err := s.otherWorkspaceRoots(ctx, ws.ID)
	if err != nil {
		return nil, err
	}
	fp, err := job.IndexFingerprint(ws.Path, otherRoots...)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.fp[ws.ID] == fp {
		cached := s.jobs[ws.ID]
		s.mu.Unlock()
		return cached, nil
	}
	s.mu.Unlock()
	disc, err := job.Discover(ws.Path, otherRoots...)
	if err != nil {
		return nil, err
	}
	existing, err := s.existingIndexRows(ctx, ws.ID)
	if err != nil {
		return nil, err
	}
	jobs := make([]*job.Job, 0, len(disc.Jobs)+len(disc.Agents))
	jobs = append(jobs, disc.Jobs...)
	jobs = append(jobs, disc.Agents...)
	if err := s.ensureIDs(ctx, ws.ID, jobs, existing); err != nil {
		return nil, err
	}
	desired := desiredRows(jobs, idsByName(existing))
	if err := s.writeIndexDiff(ctx, ws.ID, existing, desired); err != nil {
		return nil, err
	}
	s.emitIndexDiff(ctx, ws.ID, existing, desired)
	s.mu.Lock()
	s.fp[ws.ID] = fp
	s.jobs[ws.ID] = jobs
	s.mu.Unlock()
	return jobs, nil
}

// RebuildIndexPath updates one index row for a changed definition file.
func (s *Service) RebuildIndexPath(ctx context.Context, ws *Workspace, path string) (*job.Job, error) {
	otherRoots, err := s.otherWorkspaceRoots(ctx, ws.ID)
	if err != nil {
		return nil, err
	}
	j, err := job.DiscoverPath(ws.Path, path, otherRoots...)
	if err != nil {
		return nil, err
	}
	existing, err := s.existingIndexRows(ctx, ws.ID)
	if err != nil {
		return nil, err
	}
	desired := maps.Clone(existing)
	for id, row := range existing {
		if row.path == path {
			delete(desired, id)
		}
	}
	if j != nil {
		if err := s.ensureIDs(ctx, ws.ID, []*job.Job{j}, existing); err != nil {
			return nil, err
		}
		maps.Copy(desired, desiredRows([]*job.Job{j}, idsByName(existing)))
	}
	if err := s.writeIndexDiff(ctx, ws.ID, existing, desired); err != nil {
		return nil, err
	}
	s.emitIndexDiff(ctx, ws.ID, existing, desired)
	s.mu.Lock()
	delete(s.fp, ws.ID)
	delete(s.jobs, ws.ID)
	s.mu.Unlock()
	if j != nil && j.ParseError != "" {
		return nil, nil
	}
	return j, nil
}

// ListJobs returns the live job_index rows for a workspace (empty workspace = all).
func (s *Service) ListJobs(ctx context.Context, workspaceID string) ([]JobRow, error) {
	q := jobRowSelect + ` WHERE j.retired_at IS NULL`
	var args []any
	if workspaceID != "" {
		q += ` AND j.workspace_id=?`
		args = append(args, workspaceID)
	}
	q += ` ORDER BY j.name`
	return s.queryJobRows(ctx, q, args...)
}

// JobByID returns the job_index row of id, also a retired one.
func (s *Service) JobByID(ctx context.Context, id string) (*JobRow, error) {
	rows, err := s.queryJobRows(ctx, jobRowSelect+` WHERE j.id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	return &rows[0], nil
}

// JobByName returns the live job_index row of name in a workspace.
func (s *Service) JobByName(ctx context.Context, workspaceID, name string) (*JobRow, error) {
	rows, err := s.queryJobRows(ctx, jobRowSelect+` WHERE j.workspace_id=? AND j.name=? AND j.retired_at IS NULL`, workspaceID, name)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	return &rows[0], nil
}

// ResolveJob finds a job or agent row by ID or by name. An ID matches in
// any workspace; a name needs workspaceID.
func (s *Service) ResolveJob(ctx context.Context, workspaceID, ref string) (*JobRow, error) {
	ref = strings.TrimSpace(ref)
	if job.LooksLikeID(ref) {
		row, err := s.JobByID(ctx, ref)
		if err == nil && (workspaceID == "" || row.WorkspaceID == workspaceID) {
			return row, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return s.JobByName(ctx, workspaceID, ref)
}

const jobRowSelect = `SELECT j.id, j.workspace_id, j.name, j.path, j.kind, j.active, COALESCE(j.harness,''), COALESCE(j.hash,''),
	COALESCE(j.parse_error,''), j.owner_id, COALESCE(o.name,''), COALESCE(j.retired_at,'')
	FROM job_index j LEFT JOIN job_index o ON o.id = j.owner_id AND j.owner_id <> ''`

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
		if PathInside(root, abs) {
			return fmt.Errorf("%w: %s", ErrNested, root)
		}
		if PathInside(abs, root) {
			return fmt.Errorf("%w: %s", ErrContains, root)
		}
	}
	return nil
}

// existingIndexRows loads the live job_index rows of a workspace by ID.
func (s *Service) existingIndexRows(ctx context.Context, wsID string) (map[string]indexRow, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, name, path, kind, active, COALESCE(harness,''), COALESCE(hash,''), COALESCE(parse_error,''), owner_id
		FROM job_index WHERE workspace_id=? AND retired_at IS NULL`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	existing := map[string]indexRow{}
	for rows.Next() {
		var r indexRow
		var active int
		if err := rows.Scan(&r.id, &r.name, &r.path, &r.kind, &active, &r.harness, &r.hash, &r.parseError, &r.ownerID); err != nil {
			return nil, err
		}
		r.active = active == 1
		existing[r.id] = r
	}
	return existing, rows.Err()
}

// writeIndexDiff upserts changed rows and retires stale ones in one tx.
func (s *Service) writeIndexDiff(ctx context.Context, wsID string, existing, desired map[string]indexRow) error {
	return s.st.WithTx(ctx, func(tx *sql.Tx) error {
		return writeIndexDiffTx(ctx, tx, wsID, existing, desired)
	})
}

func writeIndexDiffTx(ctx context.Context, tx *sql.Tx, wsID string, existing, desired map[string]indexRow) error {
	now := time.Now().UTC().Format(time.RFC3339)
	for id := range existing {
		if _, ok := desired[id]; !ok {
			if _, err := tx.ExecContext(ctx, `UPDATE job_index SET retired_at=?, updated_at=? WHERE id=?`, now, now, id); err != nil {
				return err
			}
		}
	}
	// A rename can take the name of another changed row. Free the old
	// names first, so the unique name index never sees two live rows.
	for id, want := range desired {
		if have, ok := existing[id]; ok && have.name != want.name {
			if _, err := tx.ExecContext(ctx, `UPDATE job_index SET name=id WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	for id, want := range desired {
		if have, ok := existing[id]; ok && have == want {
			continue // unchanged: no row churn
		}
		active := 0
		if want.active {
			active = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_index (id, workspace_id, name, path, kind, active, harness, hash, parse_error, owner_id, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET
				workspace_id=excluded.workspace_id, name=excluded.name, path=excluded.path, kind=excluded.kind,
				active=excluded.active, harness=excluded.harness, hash=excluded.hash, parse_error=excluded.parse_error,
				owner_id=excluded.owner_id, updated_at=excluded.updated_at, retired_at=NULL`,
			id, wsID, want.name, want.path, want.kind, active, want.harness, want.hash, store.NullStr(want.parseError), want.ownerID, now); err != nil {
			return err
		}
	}
	return nil
}

// IndexEvent is the bus event of one job or agent index change. A job
// event names the job; an agent event names the agent by its short name.
func IndexEvent(wsID, kind, action, id, name string) store.BusEvent {
	if kind == "agent" {
		return store.BusEvent{
			Topic: "agent." + action, Entity: "agent", EntityID: id, Action: action,
			Attrs: map[string]string{"workspace": wsID, "agent_id": id, "agent_name": strings.TrimPrefix(name, "agents/")},
		}
	}
	return store.BusEvent{
		Topic: "job." + action, Entity: "job", EntityID: id, Action: action,
		Attrs: map[string]string{"workspace": wsID, "job_id": id, "job_name": name},
	}
}

// emitIndexDiff appends bus events for index changes (best-effort).
func (s *Service) emitIndexDiff(ctx context.Context, wsID string, existing, desired map[string]indexRow) {
	emit := func(ev store.BusEvent) {
		if _, err := s.st.Append(ctx, ev); err != nil {
			s.log.Warn("job index bus append failed", "workspace", wsID, "topic", ev.Topic, "error", err)
		}
	}
	for id, want := range desired {
		have, ok := existing[id]
		switch {
		case !ok:
			emit(IndexEvent(wsID, want.kind, "created", id, want.name))
		case have != want:
			emit(IndexEvent(wsID, want.kind, "updated", id, want.name))
		}
	}
	for id, have := range existing {
		if _, ok := desired[id]; !ok {
			emit(IndexEvent(wsID, have.kind, "deleted", id, have.name))
		}
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
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Active      bool   `json:"active"`
	Harness     string `json:"harness"`
	Hash        string `json:"hash"`
	ParseError  string `json:"parse_error,omitempty"`
	// OwnerID and OwnerName name the agent of a child job.
	OwnerID   string `json:"owner_id,omitempty"`
	OwnerName string `json:"owner_name,omitempty"`
	RetiredAt string `json:"retired_at,omitempty"`
}

// OwnerIndex returns the index name of the owner agent (agents/<name>), or "".
func (r JobRow) OwnerIndex() string {
	if r.OwnerName == "" {
		return ""
	}
	return "agents/" + r.OwnerName
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

func writeTrebiYAML(dir, id, name string, git bool, p Patch) error {
	y := trebiYAML{ID: id, Name: name, GitEnabled: git}
	// Patch never edits the email, assistant, or connectors blocks; carry them forward.
	if data, err := os.ReadFile(filepath.Join(dir, "trebi.yaml")); err == nil {
		var old trebiYAML
		if yaml.Unmarshal(data, &old) == nil {
			y.Email = old.Email
			y.Assistant = old.Assistant
			y.Contact = old.Contact
			y.Notify = old.Notify
			y.Connectors = old.Connectors
		}
	}
	if err := y.applyRoute(p); err != nil {
		return err
	}
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

// Route reads the contact and the notify route of trebi.yaml. set is true
// when the file holds notify, also as an empty list.
func (s *Service) Route(ws *Workspace) (route []notify.Step, set bool, contact []string, err error) {
	data, err := os.ReadFile(filepath.Join(ws.Path, "trebi.yaml"))
	if os.IsNotExist(err) {
		return nil, false, nil, nil
	}
	if err != nil {
		return nil, false, nil, err
	}
	var y trebiYAML
	if err := yaml.Unmarshal(data, &y); err != nil {
		return nil, false, nil, fmt.Errorf("trebi.yaml: %w", err)
	}
	return y.route()
}

// EmailAllowlist reads the workspace's own inbound allowlist extension from
// trebi.yaml (plan F). Not a secret; it travels with the folder. A missing
// file or block means no extension.
func (s *Service) EmailAllowlist(ws *Workspace) []string {
	if ws == nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(ws.Path, "trebi.yaml"))
	if err != nil {
		return nil
	}
	var y trebiYAML
	if yaml.Unmarshal(data, &y) != nil || y.Email == nil {
		return nil
	}
	return y.Email.Allowlist
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

// ComposeWakePrompt builds the agent wake prompt (B27, Part F). The
// assistant is a session now, so only agent launches compose a wake.
// It satisfies runs.WakeComposer — the one launch-path seam.
func (s *Service) ComposeWakePrompt(ctx context.Context, scope runs.WakeScope, opts runs.WakeOpts) (string, error) {
	if scope.Kind != "agent" {
		return "", fmt.Errorf("wake compose is agent-only; the assistant is a session")
	}
	ws, err := s.Get(ctx, scope.WorkspaceID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	s.writeAgentWake(ctx, &b, ws, scope, opts)
	return strings.TrimSpace(b.String()), nil
}

// MemoryLaunchFor resolves the gate-time MEMORY.md seam for a stored run
// (B22 §3.3). The lookup is kind-strict, like the launch finders. Nil means
// memory is off or the job file is gone. It satisfies runs.MemoryResolver.
func (s *Service) MemoryLaunchFor(ctx context.Context, run *runs.Run) *runs.MemoryLaunch {
	if run == nil || run.Workspace == "" {
		return nil
	}
	ws, err := s.Get(ctx, run.Workspace)
	if err != nil || ws == nil {
		return nil
	}
	row, err := s.JobByID(ctx, run.JobID)
	if err != nil || row == nil || row.RetiredAt != "" || row.WorkspaceID != ws.ID {
		return nil
	}
	j, _ := job.ParseFile(row.Path)
	return runs.NewMemoryLaunch(j, ws.Path)
}

// writeAgentWake renders the agent sections: instructions, owned jobs, and
// the agent's own memory, then the conversation tail (recap, current message,
// and untrusted block). {{agent_jobs}} moves the jobs block inline.
func (s *Service) writeAgentWake(ctx context.Context, b *strings.Builder, ws *Workspace, scope runs.WakeScope, opts runs.WakeOpts) {
	rows, _ := s.ListJobs(ctx, scope.WorkspaceID)
	var agent *JobRow
	for i := range rows {
		if rows[i].Kind == "agent" && rows[i].Name == scope.Name {
			agent = &rows[i]
		}
	}
	body := opts.Body
	if agent != nil {
		if strings.TrimSpace(body) == "" {
			if j, err := job.ParseFile(agent.Path); err == nil && j != nil {
				body = j.Prompt
			}
		}
		// A fire-time re-parse is raw, so expand here too (idempotent).
		body = job.SubstituteAgentVars(body, filepath.Dir(agent.Path), ws.Path)
	}
	jobs := capSection(ownedIndex(rows, scope.Name), wakeIndexCap)
	writeSection(b, "Instructions", body)
	if !strings.Contains(body, "{{agent_jobs}}") {
		writeSection(b, "Your jobs", jobs)
	} else {
		b.Reset()
		writeSection(b, "Instructions", strings.ReplaceAll(body, "{{agent_jobs}}", jobs))
	}
	short := strings.TrimPrefix(scope.Name, "agents/")
	writeSection(b, "What you can do", strings.Join([]string{
		"- You can read the whole workspace.",
		"- You can create and change jobs only under agents/" + short + "/. Use `trebi job create <name> --agent " + short + "` or the MCP `job` tool.",
		"- When a human asks for repeated work, propose a job in the conversation. Create it after the human agrees.",
		"- You cannot change your own `email.accept`, `kit`, or `budget`.",
		"- To pass a problem up, use `agent escalate`.",
	}, "\n"))
	if m := wakeMemory(opts.Memory); m != "" {
		writeSection(b, "Memory", m)
	}
	writeWakeTail(b, opts)
}

// writeWakeTail renders the chat-only sections of an assistant wake.
func writeWakeTail(b *strings.Builder, opts runs.WakeOpts) {
	if recap := strings.TrimSpace(opts.Recap); recap != "" {
		writeSection(b, "Conversation recap", capSection(recap, wakeRecapCap))
	}
	msg := strings.TrimSpace(opts.Message)
	if msg == "" {
		msg = strings.TrimSpace(opts.ExtraPrompt)
	}
	if msg != "" {
		writeSection(b, "Current message", msg)
	}
	writeUntrusted(b, opts.Untrusted)
}

// writeUntrusted renders the inbound-mail block (plan A.11). The fence is
// data from an external party, never instructions.
func writeUntrusted(b *strings.Builder, u *runs.UntrustedMessage) {
	if u == nil {
		return
	}
	writeSection(b, "Inbound email (untrusted data)", u.Fence())
}

// wakeMemory renders the Memory wake section from the launch seam. A missing
// file renders the empty placeholders; a nil seam omits the section.
func wakeMemory(mem *runs.MemoryLaunch) string {
	if mem == nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(mem.Root, mem.Rel))
	if err != nil {
		data = nil
	}
	doc, _ := memory.Parse(data)
	return memory.Render(doc, mem.Opt)
}

// portfolioLine renders one index row for a wake rollup section (Part F):
// every rollup reads alike.
func portfolioLine(r JobRow) string {
	active := "inactive"
	if r.Active {
		active = "active"
	}
	line := fmt.Sprintf("- %s kind=%s harness=%s %s", r.Name, r.Kind, r.Harness, active)
	if r.ParseError != "" {
		line += fmt.Sprintf(" parse_error=%s", r.ParseError)
	}
	return line
}

// unownedIndex lists workspace jobs with no owning agent (F.1).
func (s *Service) unownedIndex(ctx context.Context, wsID string) string {
	rows, _ := s.ListJobs(ctx, wsID)
	var b strings.Builder
	for _, row := range rows {
		if row.Kind == "job" && row.OwnerID == "" {
			b.WriteString(portfolioLine(row) + "\n")
		}
	}
	return linesBlock(b.String())
}

// agentRollup lists one agent line with children, last run, and open work
// (G.1). It uses direct rollup queries because alerts/conversations cannot be
// imported by workspaces without a dependency cycle.
func (s *Service) agentRollup(ctx context.Context, wsID string) string {
	rows, _ := s.ListJobs(ctx, wsID)
	agents := make([]JobRow, 0, len(rows))
	for _, row := range rows {
		if row.Kind == "agent" {
			agents = append(agents, row)
		}
	}
	children := make(map[string]int, len(agents))
	status := make(map[string]string, len(agents))
	conversations := make(map[string]int, len(agents))
	items := make(map[string]int, len(agents))

	rows2, err := s.st.DB().QueryContext(ctx, `SELECT owner_id, COUNT(*) FROM job_index
		WHERE workspace_id=? AND owner_id<>'' AND retired_at IS NULL GROUP BY owner_id`, wsID)
	if err == nil {
		for rows2.Next() {
			var owner string
			var count int
			if rows2.Scan(&owner, &count) == nil {
				children[owner] = count
			}
		}
		rows2.Close()
	}

	runRows, err := s.st.DB().QueryContext(ctx, `SELECT lane, value FROM (
			SELECT CASE WHEN j.kind='agent' THEN j.id ELSE j.owner_id END lane,
			       COALESCE(r.outcome,r.status) value,
			       row_number() OVER (PARTITION BY CASE WHEN j.kind='agent' THEN j.id ELSE j.owner_id END ORDER BY r.id DESC) rn
			FROM runs r
			JOIN job_index j ON j.id=r.job_id
			WHERE r.workspace_id=?
		) WHERE rn=1 AND lane<>''`, wsID)
	if err == nil {
		for runRows.Next() {
			var lane, value string
			if runRows.Scan(&lane, &value) == nil {
				status[lane] = value
			}
		}
		runRows.Close()
	}

	convRows, err := s.st.DB().QueryContext(ctx, `SELECT operator_id, COUNT(*) FROM conversations
		WHERE workspace_id=? AND operator_id<>'' GROUP BY operator_id`, wsID)
	if err == nil {
		for convRows.Next() {
			var owner string
			var count int
			if convRows.Scan(&owner, &count) == nil {
				conversations[owner] = count
			}
		}
		convRows.Close()
	}

	itemRows, err := s.st.DB().QueryContext(ctx, `SELECT CASE WHEN j.kind='agent' THEN j.id ELSE j.owner_id END lane, COUNT(*)
		FROM inbox_items i JOIN job_index j ON j.id=i.job_id
		WHERE i.workspace_id=? AND i.status<>'resolved' GROUP BY 1`, wsID)
	if err == nil {
		for itemRows.Next() {
			var owner string
			var count int
			if itemRows.Scan(&owner, &count) == nil {
				items[owner] = count
			}
		}
		itemRows.Close()
	}

	var b strings.Builder
	for _, row := range agents {
		fmt.Fprintf(&b, "%s children=%d last_status=%s conversations=%d open_items=%d\n",
			portfolioLine(row), children[row.ID], status[row.ID], conversations[row.ID], items[row.ID])
	}
	return linesBlock(b.String())
}

// ownedIndex lists the portfolio lines of one agent's jobs (F.2).
func ownedIndex(rows []JobRow, owner string) string {
	var b strings.Builder
	for _, row := range rows {
		if row.OwnerIndex() == owner {
			b.WriteString(portfolioLine(row) + "\n")
		}
	}
	return linesBlock(b.String())
}

// linesBlock trims a rendered list; an empty list reads "(none)".
func linesBlock(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(none)"
	}
	return s
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

// jobCounts returns the number of indexed jobs per workspace.
func (s *Service) jobCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT workspace_id, COUNT(*) FROM job_index WHERE kind='job' AND retired_at IS NULL GROUP BY workspace_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func rowFromJob(j *job.Job, kind, ownerID string) indexRow {
	return indexRow{
		id: j.ID, name: j.Name, path: j.FilePath, kind: kind, harness: j.Harness,
		hash: j.Hash, parseError: j.ParseError, active: !j.Disabled, ownerID: ownerID,
	}
}

// idsByName maps the index name of each row to its ID.
func idsByName(rows map[string]indexRow) map[string]string {
	out := make(map[string]string, len(rows))
	for id, row := range rows {
		out[row.name] = id
	}
	return out
}

// desiredRows keys the discovered jobs and agents by ID. byName resolves
// the owner agent of a child job that is not in jobs, so a child keeps the
// ID of a deleted owner.
func desiredRows(jobs []*job.Job, byName map[string]string) map[string]indexRow {
	names := make(map[string]string, len(byName)+len(jobs))
	maps.Copy(names, byName)
	for _, j := range jobs {
		names[j.Name] = j.ID
	}
	out := make(map[string]indexRow, len(jobs))
	for _, j := range jobs {
		kind := j.Kind
		if kind == "" {
			kind = "job"
		}
		owner := ""
		if j.Owner != "" {
			owner = names[j.Owner]
		}
		out[j.ID] = rowFromJob(j, kind, owner)
	}
	return out
}

func (s *Service) queryJobRows(ctx context.Context, q string, args ...any) ([]JobRow, error) {
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		var r JobRow
		var active int
		var ownerName string
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.Name, &r.Path, &r.Kind, &active, &r.Harness, &r.Hash, &r.ParseError,
			&r.OwnerID, &ownerName, &r.RetiredAt); err != nil {
			return nil, err
		}
		r.Active = active == 1
		r.OwnerName = strings.TrimPrefix(ownerName, "agents/")
		out = append(out, r)
	}
	return out, rows.Err()
}

// idProblem returns why j needs a new ID, or "" when its ID is good.
func (s *Service) idProblem(ctx context.Context, j *job.Job, claimed map[string]string) (string, error) {
	switch j.IDState {
	case job.IDMissing:
		return "missing", nil
	case job.IDInvalid:
		return "invalid", nil
	}
	if p, ok := claimed[j.ID]; ok {
		if p == j.FilePath {
			return "", nil
		}
		return "duplicate", nil
	}
	// A live row with the ID at a path that still exists is a copy.
	p, err := s.indexPath(ctx, j.ID)
	if err != nil {
		return "", err
	}
	if p != "" && p != j.FilePath {
		if _, err := os.Stat(p); err == nil {
			return "duplicate", nil
		}
	}
	return "", nil
}

// indexPath returns the path of the live job_index row of id, or "".
func (s *Service) indexPath(ctx context.Context, id string) (string, error) {
	var p string
	err := s.st.DB().QueryRowContext(ctx, `SELECT path FROM job_index WHERE id=? AND retired_at IS NULL`, id).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return p, err
}

func (s *Service) mintJobID(ctx context.Context, j *job.Job, reuse string, existing map[string]indexRow, claimed map[string]string) (string, error) {
	// The index row of the same name and path keeps its ID when it is free.
	if row, ok := existing[reuse]; ok && row.path == j.FilePath {
		if _, taken := claimed[reuse]; !taken {
			return reuse, nil
		}
	}
	for {
		id := job.NewID(j.Kind)
		if _, ok := claimed[id]; ok {
			continue
		}
		var n int
		if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM job_index WHERE id=?`, id).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return id, nil
		}
	}
}

// ensureIDs gives each job and agent a valid, unique ID (plan 2026-10-06
// A6). It writes a new ID into the file. A file that does not parse gets
// an ID in the index only; the write waits until the file parses.
func (s *Service) ensureIDs(ctx context.Context, wsID string, jobs []*job.Job, existing map[string]indexRow) error {
	sorted := slices.Clone(jobs)
	slices.SortFunc(sorted, func(a, b *job.Job) int { return strings.Compare(a.FilePath, b.FilePath) })
	byName := make(map[string]string, len(existing))
	for id, r := range existing {
		byName[r.name] = id
	}
	claimed := map[string]string{} // id -> path
	// The path that has the ID in job_index keeps it.
	for _, j := range sorted {
		if j.IDState != job.IDOK {
			continue
		}
		if p, err := s.indexPath(ctx, j.ID); err != nil {
			return err
		} else if p == j.FilePath {
			claimed[j.ID] = j.FilePath
		}
	}
	for _, j := range sorted {
		reason, err := s.idProblem(ctx, j, claimed)
		if err != nil {
			return err
		}
		if reason == "" {
			claimed[j.ID] = j.FilePath
			continue
		}
		reuse := ""
		if reason != "duplicate" {
			reuse = byName[j.Name]
		}
		id, err := s.mintJobID(ctx, j, reuse, existing, claimed)
		if err != nil {
			return err
		}
		claimed[id] = j.FilePath
		if j.ParseError != "" {
			j.ID = id
			continue
		}
		if err := j.AssignID(id); err != nil {
			// The index keeps the ID; the next rebuild reuses it by name and path.
			s.log.Warn("job id write failed", "path", j.FilePath, "err", err)
			j.ID = id
			continue
		}
		if _, err := s.st.Append(ctx, store.BusEvent{
			Topic: "job.id_assigned", Entity: j.Kind, EntityID: id, Action: "updated",
			Attrs: map[string]string{"workspace": wsID, "name": j.Name, "id": id, "reason": reason},
		}); err != nil {
			s.log.Warn("job id bus append failed", "workspace", wsID, "error", err)
		}
	}
	return nil
}
