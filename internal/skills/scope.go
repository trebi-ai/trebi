package skills

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// Scope is who can use a skill. A run gets all four; a narrower scope
// overwrites a wider skill of the same name.
type Scope string

const (
	ScopeGlobal    Scope = "global"
	ScopeWorkspace Scope = "workspace"
	ScopeAgent     Scope = "agent"
	ScopeJob       Scope = "job"
)

// Scopes lists the scopes from narrow to wide.
func Scopes() []Scope { return []Scope{ScopeJob, ScopeAgent, ScopeWorkspace, ScopeGlobal} }

// Valid reports a known scope.
func (s Scope) Valid() bool {
	switch s {
	case ScopeGlobal, ScopeWorkspace, ScopeAgent, ScopeJob:
		return true
	}
	return false
}

// Status of a skill. A pending skill is not in runs until it is approved.
type Status string

const (
	StatusActive  Status = "active"
	StatusPending Status = "pending"
)

// ErrConflict is a skill name that is already in the target scope.
var ErrConflict = errors.New("skill name conflict")

// ErrBadRef is a ref that does not parse.
var ErrBadRef = errors.New("bad skill ref")

// ErrNoOwner is a scoped ref whose owner does not exist.
var ErrNoOwner = errors.New("skill owner not found")

// Ref is the identity of a skill: global/<name> or <scope>/<owner-id>/<name>.
// A name alone is unique only inside one scope and owner.
type Ref struct {
	Scope Scope
	Owner string
	Name  string
}

// GlobalRef is the ref of a global skill.
func GlobalRef(name string) Ref { return Ref{Scope: ScopeGlobal, Name: name} }

// ParseRef reads global/<name> or <scope>/<owner-id>/<name>.
func ParseRef(s string) (Ref, error) {
	parts := strings.Split(s, "/")
	var r Ref
	switch len(parts) {
	case 2:
		r = Ref{Scope: Scope(parts[0]), Name: parts[1]}
	case 3:
		r = Ref{Scope: Scope(parts[0]), Owner: parts[1], Name: parts[2]}
	default:
		return Ref{}, fmt.Errorf("%w: %q", ErrBadRef, s)
	}
	if err := r.Check(); err != nil {
		return Ref{}, err
	}
	return r, nil
}

func (r Ref) String() string {
	if r.Scope == ScopeGlobal {
		return "global/" + r.Name
	}
	return string(r.Scope) + "/" + r.Owner + "/" + r.Name
}

// Check refuses an unknown scope, a bad name, and an owner that does not
// fit the scope.
func (r Ref) Check() error {
	if !r.Scope.Valid() {
		return fmt.Errorf("%w: scope must be global, workspace, agent, or job", ErrBadRef)
	}
	if err := CheckName(r.Name); err != nil {
		return err
	}
	if (r.Scope == ScopeGlobal) != (r.Owner == "") {
		return fmt.Errorf("%w: a %s skill needs an owner only when it is not global", ErrBadRef, r.Scope)
	}
	if strings.ContainsAny(r.Owner, `/\`) || r.Owner == "." || r.Owner == ".." {
		return fmt.Errorf("%w: owner %q", ErrBadRef, r.Owner)
	}
	return nil
}

// Owner is the workspace, agent, or job that holds a scoped skill. Dir is
// the folder whose skills/ subfolder holds the skills.
type Owner struct {
	Kind        Scope  `json:"kind"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	// AgentID is the agent that owns a job.
	AgentID string `json:"agent_id,omitempty"`
	Dir     string `json:"-"`
}

// Owners finds the folders of scoped skills. The workspaces service
// implements it.
type Owners interface {
	SkillOwner(ctx context.Context, scope Scope, id string) (Owner, error)
	SkillOwners(ctx context.Context) ([]Owner, error)
}

// CreatedBy names the caller that made a skill.
type CreatedBy struct {
	ActorKind string `json:"actor_kind"`
	Job       string `json:"job,omitempty"`
	RunID     string `json:"run_id,omitempty"`
}

// WithOwners sets the source of scoped skill folders. Nil lists global
// skills only.
func (s *Service) WithOwners(o Owners) *Service {
	s.owners = o
	return s
}

// owner returns the owner of a ref and the folder of the skill.
func (s *Service) owner(ctx context.Context, r Ref) (Owner, string, error) {
	if r.Scope == ScopeGlobal {
		return Owner{Kind: ScopeGlobal}, filepath.Join(home.SkillsDir(), r.Name), nil
	}
	if s.owners == nil {
		return Owner{}, "", fmt.Errorf("%w: %s", ErrNoOwner, r)
	}
	o, err := s.owners.SkillOwner(ctx, r.Scope, r.Owner)
	if err != nil {
		return Owner{}, "", fmt.Errorf("%w: %s %s: %w", ErrNoOwner, r.Scope, r.Owner, err)
	}
	return o, filepath.Join(o.Dir, "skills", r.Name), nil
}

// Values of ListOpts.Archived. Empty is the same as ArchivedFalse.
const (
	ArchivedFalse = "false"
	ArchivedTrue  = "true"
	ArchivedAll   = "all"
)

// ListOpts filters List. Empty fields match all, except Archived: empty hides
// the archived skills.
type ListOpts struct {
	Scope       Scope
	WorkspaceID string
	OwnerID     string
	// Archived is false (default), true, or all.
	Archived string
}

func (o ListOpts) match(sk Skill) bool {
	switch o.Archived {
	case ArchivedAll:
	case ArchivedTrue:
		if sk.ArchivedAt == "" {
			return false
		}
	default:
		if sk.ArchivedAt != "" {
			return false
		}
	}
	if o.Scope != "" && sk.Scope != o.Scope {
		return false
	}
	if o.OwnerID != "" && (sk.Owner == nil || sk.Owner.ID != o.OwnerID) {
		return false
	}
	if o.WorkspaceID != "" && (sk.Owner == nil || sk.Owner.WorkspaceID != o.WorkspaceID) {
		return false
	}
	return true
}

// scoped lists the skills of every workspace, agent, and job.
func (s *Service) scoped(ctx context.Context) ([]Skill, error) {
	if s.owners == nil {
		return nil, nil
	}
	owners, err := s.owners.SkillOwners(ctx)
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, o := range owners {
		entries, err := os.ReadDir(filepath.Join(o.Dir, "skills"))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || CheckName(e.Name()) != nil {
				continue
			}
			if _, err := os.Stat(filepath.Join(o.Dir, "skills", e.Name(), "SKILL.md")); err != nil {
				continue
			}
			sk, err := s.load(ctx, Ref{Scope: o.Kind, Owner: o.ID, Name: e.Name()}, o, false)
			if err != nil {
				continue
			}
			out = append(out, *sk)
		}
	}
	return out, nil
}

// wider returns the refs that a skill of this owner overwrites, narrow
// first: the owner agent of a job, the workspace, then global.
func (o Owner) wider(name string) []Ref {
	var out []Ref
	switch o.Kind {
	case ScopeJob:
		if o.AgentID != "" {
			out = append(out, Ref{Scope: ScopeAgent, Owner: o.AgentID, Name: name})
		}
		out = append(out, Ref{Scope: ScopeWorkspace, Owner: o.WorkspaceID, Name: name})
	case ScopeAgent:
		out = append(out, Ref{Scope: ScopeWorkspace, Owner: o.WorkspaceID, Name: name})
	case ScopeGlobal:
		return nil
	}
	return append(out, GlobalRef(name))
}

// markOverrides sets Overrides and OverriddenBy across one full list.
func markOverrides(list []Skill) {
	at := make(map[string]int, len(list))
	for i, sk := range list {
		at[sk.Ref] = i
	}
	for i := range list {
		if list[i].Owner == nil {
			continue
		}
		if list[i].ArchivedAt != "" {
			continue
		}
		for _, w := range list[i].Owner.wider(list[i].Name) {
			j, ok := at[w.String()]
			if !ok || list[j].ArchivedAt != "" {
				continue
			}
			list[i].Overrides = w.String()
			list[j].OverriddenBy = append(list[j].OverriddenBy, list[i].Ref)
			break
		}
	}
}

// RunScope names the owners of one run. Resolve fills the agent and the
// workspace of a job.
type RunScope struct {
	WorkspaceID string
	AgentID     string
	JobID       string
}

// Resolved is one skill in the effective set of a run.
type Resolved struct {
	Skill
	Used          bool   `json:"used"`
	OverwrittenBy string `json:"overwritten_by,omitempty"`
}

// Resolve returns the effective skill set of one run, narrow first: job,
// agent, workspace, then global (inherited skills count as global). The
// first skill of a name is used; a wider one of the same name is
// overwritten. Pending and archived skills are left out, so a wider skill of
// the same name takes the name.
func (s *Service) Resolve(ctx context.Context, rs RunScope) ([]Resolved, error) {
	if rs.JobID != "" && s.owners != nil {
		if o, err := s.owners.SkillOwner(ctx, ScopeJob, rs.JobID); err == nil {
			rs.WorkspaceID = cmpOr(rs.WorkspaceID, o.WorkspaceID)
			rs.AgentID = cmpOr(rs.AgentID, o.AgentID)
		}
	}
	all, err := s.List(ctx, ListOpts{Archived: ArchivedAll})
	if err != nil {
		return nil, err
	}
	owners := map[Scope]string{ScopeJob: rs.JobID, ScopeAgent: rs.AgentID, ScopeWorkspace: rs.WorkspaceID}
	var out []Resolved
	used := map[string]string{}
	for _, scope := range Scopes() {
		var tier []Skill
		for _, sk := range all {
			if sk.Scope != scope || sk.Status == StatusPending || sk.ArchivedAt != "" {
				continue
			}
			if scope != ScopeGlobal && (owners[scope] == "" || sk.Owner == nil || sk.Owner.ID != owners[scope]) {
				continue
			}
			tier = append(tier, sk)
		}
		sort.Slice(tier, func(i, j int) bool { return tier[i].Name < tier[j].Name })
		for _, sk := range tier {
			r := Resolved{Skill: sk}
			if by, ok := used[sk.Name]; ok {
				r.OverwrittenBy = by
			} else {
				r.Used = true
				used[sk.Name] = sk.Ref
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// ArchivedFor returns the archived skills that a job or agent asks for: the
// skills of its job and agent scopes, and the global skills in names. Workspace
// skills stay out, because every job of the workspace would list them.
func (s *Service) ArchivedFor(ctx context.Context, rs RunScope, names []string) ([]Skill, error) {
	if rs.JobID != "" && s.owners != nil {
		if o, err := s.owners.SkillOwner(ctx, ScopeJob, rs.JobID); err == nil {
			rs.AgentID = cmpOr(rs.AgentID, o.AgentID)
		}
	}
	all, err := s.List(ctx, ListOpts{Archived: ArchivedTrue})
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, sk := range all {
		switch {
		case sk.Scope == ScopeJob && rs.JobID != "" && sk.Owner != nil && sk.Owner.ID == rs.JobID,
			sk.Scope == ScopeAgent && rs.AgentID != "" && sk.Owner != nil && sk.Owner.ID == rs.AgentID,
			sk.Scope == ScopeGlobal && slices.Contains(names, sk.Name):
			out = append(out, sk)
		}
	}
	return out, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Target is the scope and owner of a move.
type Target struct {
	Scope Scope
	Owner string
}

// MoveOpts handles a name that is already at the target.
type MoveOpts struct {
	// Replace deletes the skill at the target first.
	Replace bool
	// Rename gives the skill a new name at the target.
	Rename string
}

// Move puts a skill in another scope or under another owner. The folder
// moves in one step on the same disk, else it is copied and then removed.
// Files keep their content. A name at the target is ErrConflict unless
// opts sets Replace or Rename.
func (s *Service) Move(ctx context.Context, from Ref, to Target, opts MoveOpts) (*Skill, error) {
	cur, err := s.Get(ctx, from, false)
	if err != nil {
		return nil, err
	}
	if err := guard(cur); err != nil {
		return nil, err
	}
	dst := Ref{Scope: to.Scope, Owner: to.Owner, Name: cmpOr(opts.Rename, from.Name)}
	if err := dst.Check(); err != nil {
		return nil, err
	}
	if dst == from {
		return nil, fmt.Errorf("%w: the skill is already at %s", ErrConflict, dst)
	}
	_, srcDir, err := s.owner(ctx, from)
	if err != nil {
		return nil, err
	}
	dstOwner, dstDir, err := s.owner(ctx, dst)
	if err != nil {
		return nil, err
	}
	replaced, err := s.clearTarget(ctx, dst, dstDir, opts.Replace)
	if err != nil {
		return nil, err
	}
	if from.Scope == ScopeGlobal {
		if err := applyProjection(from.Name, allHarnesses(false)); err != nil {
			return nil, err
		}
	}
	if err := moveDir(srcDir, dstDir); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	attrs := map[string]string{"from": from.String(), "to": dst.String(), "workspace": dstOwner.WorkspaceID}
	if replaced {
		attrs["replaced"] = "true"
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM skill_index WHERE ref=?`, dst.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO skill_index (ref, name, scope, owner_id, workspace_id, provenance, path, hash, used_30d, updated_at,
				source_kind, source_ref, created_by_kind, created_by_job, created_by_run, status, note, archived_at)
			SELECT ?, ?, ?, ?, ?, provenance, ?, hash, used_30d, ?, source_kind, source_ref, created_by_kind, created_by_job, created_by_run, status, note, archived_at
			FROM skill_index WHERE ref=?`,
			dst.String(), dst.Name, string(dst.Scope), dst.Owner, dstOwner.WorkspaceID, dstDir, now, from.String()); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM skill_index WHERE ref=?`, from.String())
		return err
	}, store.BusEvent{Topic: "skill.moved", Entity: "skill", EntityID: dst.String(), Action: "moved", Attrs: attrs})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, dst, true)
}

// moveDir renames src to dst, or copies and removes it across disks.
// clearTarget removes the skill at dst when replace is set. It reports if a skill was there.
func (s *Service) clearTarget(ctx context.Context, dst Ref, dir string, replace bool) (bool, error) {
	if _, err := os.Stat(dir); err != nil {
		return false, nil //nolint:nilerr // no folder means nothing to replace
	}
	if !replace {
		return false, fmt.Errorf("%w: %s exists", ErrConflict, dst)
	}
	if old, err := s.Get(ctx, dst, false); err == nil {
		if err := guard(old); err != nil {
			return false, err
		}
	}
	return true, os.RemoveAll(dir)
}

func moveDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	err := os.Rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		_ = os.RemoveAll(dst) // a partial copy must not stay at the target
		return err
	}
	return os.RemoveAll(src)
}

// Approve makes a pending skill active, so runs use it.
func (s *Service) Approve(ctx context.Context, r Ref) (*Skill, error) {
	cur, err := s.Get(ctx, r, false)
	if err != nil {
		return nil, err
	}
	if cur.Status != StatusPending {
		return cur, nil
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE skill_index SET status=? WHERE ref=?`, string(StatusActive), r.String())
		return err
	}, store.BusEvent{
		Topic: "skill.updated", Entity: "skill", EntityID: r.String(), Action: "updated",
		Attrs: map[string]string{"status": string(StatusActive)},
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, r, true)
}

func allHarnesses(on bool) map[string]bool {
	out := map[string]bool{}
	for _, h := range harnessNames {
		out[h] = on
	}
	return out
}

// RefFor maps a path in the skills folder of a workspace, agent, or job to
// the ref of its skill. The narrowest owner folder wins.
func (s *Service) RefFor(ctx context.Context, path string) (Ref, bool) {
	if s.owners == nil {
		return Ref{}, false
	}
	owners, err := s.owners.SkillOwners(ctx)
	if err != nil {
		return Ref{}, false
	}
	best, bestLen := Ref{}, -1
	for _, o := range owners {
		root := filepath.Join(o.Dir, "skills") + string(filepath.Separator)
		if !strings.HasPrefix(path, root) || len(root) <= bestLen {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(path, root), string(filepath.Separator))
		if CheckName(name) != nil {
			continue
		}
		best, bestLen = Ref{Scope: o.Kind, Owner: o.ID, Name: name}, len(root)
	}
	return best, bestLen >= 0
}

// WatchDirs lists the skills folders of every owner and the skill folders
// in them, for a file watcher.
func (s *Service) WatchDirs(ctx context.Context) []string {
	if s.owners == nil {
		return nil
	}
	owners, err := s.owners.SkillOwners(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, o := range owners {
		root := filepath.Join(o.Dir, "skills")
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		out = append(out, root)
		for _, e := range entries {
			if e.IsDir() {
				out = append(out, filepath.Join(root, e.Name()))
			}
		}
	}
	return out
}

// Reload records a change that a file watcher saw. A folder that is gone
// loses its row and emits skill.deleted; any other folder emits
// skill.updated.
func (s *Service) Reload(ctx context.Context, r Ref) error {
	o, dir, err := s.owner(ctx, r)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
		return s.Touch(ctx, r)
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM skill_index WHERE ref=?`, r.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errNoRow
		}
		return nil
	}, store.BusEvent{
		Topic: "skill.deleted", Entity: "skill", EntityID: r.String(), Action: "deleted",
		Attrs: map[string]string{"workspace": o.WorkspaceID},
	})
	if errors.Is(err, errNoRow) {
		return nil
	}
	return err
}

// errNoRow rolls back a delete that found no row, so no event is written.
var errNoRow = errors.New("no skill row")
