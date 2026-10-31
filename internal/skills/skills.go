// Package skills owns the skill folders of all four scopes and skill_index.
package skills

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// Provenance for skills (D8).
type Provenance string

const (
	ProvenanceAssistant Provenance = "assistant"
	ProvenanceLocal     Provenance = "local"
	ProvenanceGit       Provenance = "git"
	ProvenanceRegistry  Provenance = "registry"
	ProvenanceConnector Provenance = "connector"
	ProvenanceInherited Provenance = "inherited"
	// ProvenanceAgent is a skill that an agent or a job run made.
	ProvenanceAgent Provenance = "agent"
)

// SkillRequirement is a structured requires entry (B12).
type SkillRequirement struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // bin|env|mcp
	Satisfied bool   `json:"satisfied"`
}

// Skill is the wire shape (backend-0808 §3.15).
type Skill struct {
	Ref             string             `json:"ref"`
	Name            string             `json:"name"`
	Scope           Scope              `json:"scope"`
	Owner           *Owner             `json:"owner,omitempty"`
	Status          Status             `json:"status"`
	CreatedBy       *CreatedBy         `json:"created_by,omitempty"`
	Note            string             `json:"note,omitempty"`
	Overrides       string             `json:"overrides,omitempty"`
	OverriddenBy    []string           `json:"overridden_by,omitempty"`
	Provenance      Provenance         `json:"provenance"`
	FromConnector   string             `json:"from_connector,omitempty"`
	Description     string             `json:"description,omitempty"`
	Version         string             `json:"version,omitempty"`
	UpdatedAt       time.Time          `json:"updated_at"`
	Hash            string             `json:"hash"`
	SkillMD         string             `json:"skill_md,omitempty"`
	Scripts         []string           `json:"scripts,omitempty"`
	Requires        []SkillRequirement `json:"requires,omitempty"`
	Harnesses       map[string]bool    `json:"harnesses,omitempty"`
	UpdateAvailable bool               `json:"update_available,omitempty"`
	Forked          bool               `json:"forked,omitempty"`
	Jobs            []string           `json:"jobs,omitempty"`
	Revision        int64              `json:"revision"`
	Path            string             `json:"path,omitempty"`
	Used30d         int                `json:"used_30d,omitempty"`
	// Source is set when the skill came from a git remote or a folder.
	Source *Source `json:"source,omitempty"`
	// Origins and Injected are set when the skill was also found in user harness dirs.
	Origins  []harness.SkillOrigin `json:"origins,omitempty"`
	Injected bool                  `json:"injected,omitempty"`
}

// Service manages skill folders + index.
type Service struct {
	st       *store.Store
	userHome string // empty: skip inherited discovery (tests)
	owners   Owners // nil: global skills only
}

// ErrPrecondition is If-Match / hash mismatch.
var ErrPrecondition = errors.New("precondition failed")

// ErrExists is a create for a skill name that is already on disk.
var ErrExists = errors.New("skill already exists")

// ErrInherited is a mutation against a read-only discovered user skill.
var ErrInherited = errors.New("inherited from user skill dir")

// ErrManaged is a mutation against a skill that a connector owns.
var ErrManaged = errors.New("managed skill: remove the connector instead")

// A skill name is one directory name under a skills folder.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ErrBadName is a skill name that cannot be a plain directory name.
var ErrBadName = errors.New("skill name must match " + nameRE.String())

// CheckName refuses a name that could reach outside the skills directory.
func CheckName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return nil
}

// New creates a skills service.
func New(st *store.Store) *Service { return &Service{st: st} }

// WithUserHome sets the home scanned for inherited skills. Empty skips.
func (s *Service) WithUserHome(dir string) *Service {
	s.userHome = dir
	return s
}

// List discovers global skills (unioned with inherited user-home skills)
// and the skills of every workspace, agent, and job. Overrides and
// OverriddenBy are set over the full set before opts filters it.
func (s *Service) List(ctx context.Context, opts ListOpts) ([]Skill, error) {
	if err := home.EnsureDirs(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(home.SkillsDir())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || CheckName(e.Name()) != nil {
			continue
		}
		sk, err := s.load(ctx, GlobalRef(e.Name()), Owner{Kind: ScopeGlobal}, false)
		if err != nil {
			continue
		}
		out = append(out, *sk)
	}
	own := len(out)
	out = s.unionInherited(out)
	scoped, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	// Own skills first, then scoped, then inherited, so a short page keeps the skills of this Trebi.
	out = append(append(out[:own:own], scoped...), out[own:]...)
	markOverrides(out)
	kept := out[:0]
	for _, sk := range out {
		if opts.match(sk) {
			kept = append(kept, sk)
		}
	}
	return kept, nil
}

// Get returns one skill; includeMD loads SKILL.md body. Overrides and
// OverriddenBy are set.
func (s *Service) Get(ctx context.Context, r Ref, includeMD bool) (*Skill, error) {
	sk, err := s.get(ctx, r, includeMD)
	if err != nil {
		return nil, err
	}
	if all, err := s.List(ctx, ListOpts{}); err == nil {
		for _, other := range all {
			if other.Ref == sk.Ref {
				sk.Overrides, sk.OverriddenBy = other.Overrides, other.OverriddenBy
				break
			}
		}
	}
	return sk, nil
}

// Named finds the skills of one name in all scopes, global first.
func (s *Service) Named(ctx context.Context, name string) ([]Skill, error) {
	all, err := s.List(ctx, ListOpts{})
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, sk := range all {
		if sk.Name == name {
			out = append(out, sk)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Scope == ScopeGlobal && out[j].Scope != ScopeGlobal })
	return out, nil
}

// CreateOpts is one new skill. Give SkillMD, or Source to pull the bundle.
type CreateOpts struct {
	Ref        Ref
	SkillMD    string
	Provenance Provenance
	Source     *Source
	CreatedBy  *CreatedBy
	// Pending keeps the skill out of runs until Approve.
	Pending bool
	// Note is a short text from the maker, such as a request to share the
	// skill wider.
	Note string
}

// maxNote is the longest note a skill keeps.
const maxNote = 500

func (o CreateOpts) provenance() (Provenance, error) {
	prov := o.Provenance
	if prov == "" {
		prov = ProvenanceLocal
	}
	if o.Source != nil {
		prov = ProvenanceGit
		if o.Source.Kind == "folder" {
			prov = ProvenanceLocal
		}
	}
	if prov == ProvenanceInherited {
		return "", fmt.Errorf("provenance inherited is discovered, not created")
	}
	if prov == ProvenanceConnector && o.Ref.Scope != ScopeGlobal {
		return "", fmt.Errorf("a connector skill is always global")
	}
	return prov, nil
}

// write fills dir from the source, or writes SKILL.md.
func (o CreateOpts) write(ctx context.Context, dir string) error {
	if o.Source != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		return o.Source.fetch(ctx, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	md := o.SkillMD
	if md == "" {
		md = "# " + o.Ref.Name + "\n\nSkill body.\n"
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644)
}

// Create writes a new skill folder and its index row.
func (s *Service) Create(ctx context.Context, opts CreateOpts) (*Skill, error) {
	if opts.Ref.Scope == "" {
		opts.Ref.Scope = ScopeGlobal
	}
	if err := opts.Ref.Check(); err != nil {
		return nil, err
	}
	if len(opts.Note) > maxNote {
		return nil, fmt.Errorf("note is longer than %d characters", maxNote)
	}
	prov, err := opts.provenance()
	if err != nil {
		return nil, err
	}
	o, dir, err := s.owner(ctx, opts.Ref)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, opts.Ref)
	}
	if err := opts.write(ctx, dir); err != nil {
		return nil, err
	}
	var srcKind, srcRef string
	if opts.Source != nil {
		srcKind, srcRef = opts.Source.Kind, opts.Source.ref()
	}
	status := StatusActive
	if opts.Pending {
		status = StatusPending
	}
	by := opts.CreatedBy
	if by == nil {
		by = &CreatedBy{}
	}
	attrs := map[string]string{"provenance": string(prov), "scope": string(opts.Ref.Scope), "workspace": o.WorkspaceID, "status": string(status)}
	if srcKind != "" {
		attrs["source"] = srcKind
	}
	if by.ActorKind != "" {
		attrs["actor_kind"], attrs["job"], attrs["run_id"] = by.ActorKind, by.Job, by.RunID
	}
	if opts.Note != "" {
		attrs["note"] = opts.Note
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO skill_index (ref, name, scope, owner_id, workspace_id, provenance, path, hash, used_30d, updated_at,
				source_kind, source_ref, created_by_kind, created_by_job, created_by_run, status, note)
			VALUES (?,?,?,?,?,?,?,?,0,?,?,?,?,?,?,?,?)`,
			opts.Ref.String(), opts.Ref.Name, string(opts.Ref.Scope), opts.Ref.Owner, o.WorkspaceID, string(prov), dir, hashDir(dir), now,
			store.NullStr(srcKind), store.NullStr(srcRef), store.NullStr(by.ActorKind), store.NullStr(by.Job), store.NullStr(by.RunID),
			string(status), store.NullStr(opts.Note))
		return err
	}, store.BusEvent{
		Topic: "skill.created", Entity: "skill", EntityID: opts.Ref.String(), Action: "created", Attrs: attrs,
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, opts.Ref, true)
}

// Update replaces SKILL.md (If-Match via hash) and/or harness projection.
// Harness links exist only for global skills.
func (s *Service) Update(ctx context.Context, r Ref, skillMD, ifMatch string, harnesses map[string]bool) (*Skill, error) {
	cur, err := s.Get(ctx, r, false)
	if err != nil {
		return nil, err
	}
	if cur.Provenance == ProvenanceInherited {
		return nil, inheritedErr(cur)
	}
	if ifMatch != "" && ifMatch != cur.Hash {
		return nil, fmt.Errorf("%w: hash mismatch", ErrPrecondition)
	}
	if harnesses != nil && r.Scope != ScopeGlobal {
		return nil, fmt.Errorf("harness links are only for global skills; a %s skill is in the runs of its owner", r.Scope)
	}
	if skillMD != "" {
		if err := os.WriteFile(filepath.Join(cur.Path, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
			return nil, err
		}
	}
	if harnesses != nil {
		if err := applyProjection(r.Name, harnesses); err != nil {
			return nil, err
		}
	}
	if err := s.touch(ctx, cur, nil); err != nil {
		return nil, err
	}
	return s.Get(ctx, r, true)
}

// Touch records a change on disk under a skill folder: the hash and the
// time move, and skill.updated is emitted.
func (s *Service) Touch(ctx context.Context, r Ref) error {
	cur, err := s.get(ctx, r, false)
	if err != nil {
		return err
	}
	return s.touch(ctx, cur, nil)
}

// Delete removes a skill folder (refused for connector and inherited provenance).
func (s *Service) Delete(ctx context.Context, r Ref) error {
	cur, err := s.Get(ctx, r, false)
	if err != nil {
		return err
	}
	if err := guard(cur); err != nil {
		return err
	}
	if r.Scope == ScopeGlobal {
		if err := applyProjection(r.Name, allHarnesses(false)); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(cur.Path); err != nil {
		return err
	}
	attrs := map[string]string{}
	if cur.Owner != nil && cur.Owner.WorkspaceID != "" {
		attrs["workspace"] = cur.Owner.WorkspaceID
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM skill_index WHERE ref=?`, r.String())
		return err
	}, store.BusEvent{
		Topic: "skill.deleted", Entity: "skill", EntityID: r.String(), Action: "deleted", Attrs: attrs,
	})
	return err
}

// AddConnectorSkill writes one skill a connector install contributed. The
// skill carries ProvenanceConnector and the connector name as its source, so
// the connector owns its lifetime (Part H.4).
func (s *Service) AddConnectorSkill(ctx context.Context, name, skillMD, connector string) (*Skill, error) {
	if connector == "" {
		return nil, fmt.Errorf("connector required")
	}
	r := GlobalRef(name)
	if err := r.Check(); err != nil {
		return nil, err
	}
	dir := filepath.Join(home.SkillsDir(), name)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, r)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		return nil, err
	}
	if err := s.upsertConnector(ctx, r, dir, connector, "skill.created"); err != nil {
		return nil, err
	}
	return s.Get(ctx, r, true)
}

// SyncConnectorSkill copies the skill folder of a resolved connector into
// the managed skills as ProvenanceConnector (Part C.6). A skill of the same
// name that another owner holds is ErrExists. A copy that the same connector
// owns is replaced. With root, srcDir is the connector folder itself, so only
// SKILL.md and the skill folders are copied, not the source tree. Connector
// skills are always global.
func (s *Service) SyncConnectorSkill(ctx context.Context, name, srcDir, connector string, root bool) (*Skill, error) {
	r := GlobalRef(name)
	if err := r.Check(); err != nil {
		return nil, err
	}
	dir := filepath.Join(home.SkillsDir(), name)
	var prov, ref sql.NullString
	err := s.st.DB().QueryRowContext(ctx, `SELECT provenance, source_ref FROM skill_index WHERE ref=?`, r.String()).Scan(&prov, &ref)
	switch {
	case err == nil && (prov.String != string(ProvenanceConnector) || ref.String != connector):
		return nil, fmt.Errorf("%w: %q", ErrExists, name)
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return nil, err
	case err != nil:
		if _, statErr := os.Stat(dir); statErr == nil {
			return nil, fmt.Errorf("%w: %q", ErrExists, name)
		}
	}
	exists := err == nil
	tmp := dir + ".sync"
	_ = os.RemoveAll(tmp) // a leftover of a failed sync
	if err := copySkill(tmp, srcDir, root); err != nil {
		_ = os.RemoveAll(tmp) // best effort
		return nil, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return nil, err
	}
	topic := "skill.created"
	if exists {
		topic = "skill.updated"
	}
	if err := s.upsertConnector(ctx, r, dir, connector, topic); err != nil {
		return nil, err
	}
	return s.Get(ctx, r, false)
}

// RemoveConnectorSkills removes the skills one connector contributed. It
// returns the names it removed.
func (s *Service) RemoveConnectorSkills(ctx context.Context, connector string) ([]string, error) {
	if connector == "" {
		return nil, nil
	}
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT name FROM skill_index WHERE provenance=? AND source_ref=? AND scope=?`,
		string(ProvenanceConnector), connector, string(ScopeGlobal))
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			names = append(names, name)
		}
	}
	rows.Close()
	sort.Strings(names)
	for _, name := range names {
		// A row from before the name check must not reach RemoveAll.
		if CheckName(name) == nil {
			if err := os.RemoveAll(filepath.Join(home.SkillsDir(), name)); err != nil {
				return names, err
			}
		}
		ref := GlobalRef(name).String()
		if _, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM skill_index WHERE ref=?`, ref)
			return err
		}, store.BusEvent{
			Topic: "skill.deleted", Entity: "skill", EntityID: ref, Action: "deleted",
		}); err != nil {
			return names, err
		}
	}
	return names, nil
}

// GuardMutate refuses writes against inherited user-home skills and
// skills that a connector owns.
func (s *Service) GuardMutate(ctx context.Context, r Ref) error {
	sk, err := s.Get(ctx, r, false)
	if err != nil {
		return err
	}
	return guard(sk)
}

func guard(sk *Skill) error {
	switch sk.Provenance {
	case ProvenanceInherited:
		return inheritedErr(sk)
	case ProvenanceConnector:
		return ErrManaged
	}
	return nil
}

func inheritedErr(sk *Skill) error {
	path := inheritedOriginPath(sk)
	if path != "" {
		return fmt.Errorf("%w: inherited from %s; edit there", ErrInherited, path)
	}
	return fmt.Errorf("%w; edit the origin file", ErrInherited)
}

func inheritedOriginPath(sk *Skill) string {
	if sk == nil {
		return ""
	}
	if len(sk.Origins) > 0 {
		return sk.Origins[0].Path
	}
	return sk.Path
}

func (s *Service) unionInherited(out []Skill) []Skill {
	discovered := harness.DiscoverSkills(s.userHome, nil)
	byName := map[string]int{}
	for i, sk := range out {
		byName[sk.Name] = i
	}
	for _, ds := range discovered {
		if i, ok := byName[ds.Name]; ok {
			out[i].Origins = ds.Origins
			out[i].Injected = ds.Injected
			continue
		}
		out = append(out, *s.skillFromDiscovered(ds, false))
	}
	return out
}

func (s *Service) mergeInheritedMeta(sk *Skill) {
	if ds := s.inheritedByName(sk.Name); ds != nil {
		sk.Origins = ds.Origins
		sk.Injected = ds.Injected
	}
}

func (s *Service) inheritedByName(name string) *harness.DiscoveredSkill {
	for _, ds := range harness.DiscoverSkills(s.userHome, nil) {
		if ds.Name == name {
			return &ds
		}
	}
	return nil
}

// harnessNames are the harnesses that read projected global skills.
var harnessNames = []string{"claude", "grok", "codex", "opencode", "opencode2", "gemini", "cursor", "pi"} //nolint:gochecknoglobals // fixed list, never mutated

func (s *Service) skillFromDiscovered(ds harness.DiscoveredSkill, includeMD bool) *Skill {
	mdBytes := harness.ReadSkillMD(ds.Path, s.userHome)
	updated := time.Now().UTC()
	if st, err := os.Stat(ds.Path); err == nil {
		updated = st.ModTime().UTC()
	}
	harnesses := allHarnesses(false)
	for _, h := range ds.Harnesses {
		harnesses[h] = true
	}
	sk := &Skill{
		Ref:         GlobalRef(ds.Name).String(),
		Name:        ds.Name,
		Scope:       ScopeGlobal,
		Status:      StatusActive,
		Provenance:  ProvenanceInherited,
		Description: skillDescription(string(mdBytes)),
		Version:     skillVersion(string(mdBytes)),
		UpdatedAt:   updated,
		Hash:        hashDir(ds.Path),
		Path:        ds.Path,
		Revision:    1,
		Harnesses:   harnesses,
		Scripts:     listScripts(ds.Path),
		Requires:    parseRequires(string(mdBytes)),
		Origins:     ds.Origins,
		Injected:    ds.Injected,
	}
	if includeMD {
		sk.SkillMD = string(mdBytes)
	}
	return sk
}

// ProjectForRun links the effective skills of one run into a temp dir and
// returns its path. allow keeps only the named skills (empty keeps all);
// deny drops names. An overwritten skill is never linked.
func (s *Service) ProjectForRun(ctx context.Context, rs RunScope, allow, deny []string) (string, error) {
	resolved, err := s.Resolve(ctx, rs)
	if err != nil {
		return "", err
	}
	out := filepath.Join(os.TempDir(), "trebi-skills-"+ulid.Make().String())
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	for _, r := range resolved {
		if !r.Used || slices.Contains(deny, r.Name) || (len(allow) > 0 && !slices.Contains(allow, r.Name)) {
			continue
		}
		if err := os.Symlink(r.Path, filepath.Join(out, r.Name)); err != nil {
			return "", err
		}
	}
	return out, nil
}

func hashDir(dir string) string {
	h := sha256.New()
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		h.Write([]byte(rel))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func frontmatter(md string) (meta map[string]string, body string) {
	meta = map[string]string{}
	body = md
	trimmed := strings.TrimSpace(md)
	if !strings.HasPrefix(trimmed, "---") {
		return meta, md
	}
	rest := strings.TrimPrefix(trimmed, "---")
	parts := strings.SplitN(rest, "\n---", 2)
	if len(parts) != 2 {
		return meta, md
	}
	for _, line := range strings.Split(parts[0], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key != "" {
			meta[key] = val
		}
	}
	return meta, parts[1]
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		if line != "" {
			return line
		}
	}
	return ""
}

func skillDescription(md string) string {
	meta, body := frontmatter(md)
	if d := strings.TrimSpace(meta["description"]); d != "" {
		return d
	}
	return firstLine(body)
}

func skillVersion(md string) string {
	meta, _ := frontmatter(md)
	return strings.TrimSpace(meta["version"])
}

func listScripts(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".sh") || strings.HasSuffix(path, ".py") || strings.HasSuffix(path, ".js") {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func parseRequires(md string) []SkillRequirement {
	var out []SkillRequirement
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		kind, name, ok := strings.Cut(strings.TrimPrefix(line, "- "), ":")
		if !ok || !strings.HasPrefix(line, "- ") {
			continue
		}
		kind = strings.TrimSpace(kind)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		switch kind {
		case "bin":
			_, err := exec.LookPath(name)
			out = append(out, SkillRequirement{Name: name, Kind: "bin", Satisfied: err == nil})
		case "env":
			satisfied := os.Getenv(name) != ""
			if !satisfied {
				if _, err := os.Stat(filepath.Join(home.SecretsDir(), name)); err == nil {
					satisfied = true
				}
			}
			out = append(out, SkillRequirement{Name: name, Kind: "env", Satisfied: satisfied})
		case "mcp":
			out = append(out, SkillRequirement{Name: name, Kind: "mcp", Satisfied: false})
		}
	}
	return out
}

// harnessSkillsDir returns the skills folder in one harness home. The
// agent-wire home layout owns the name.
func harnessSkillsDir(h string) string {
	for _, f := range agentwire.HomeFiles(agentwire.Harness(h)) {
		if f.Key == "skills" && f.Home != "" {
			return f.Home
		}
	}
	return "skills"
}

func projectionState(name string) map[string]bool {
	out := map[string]bool{}
	for _, h := range harnessNames {
		link := filepath.Join(home.HarnessDir(h), harnessSkillsDir(h), name)
		if _, err := os.Lstat(link); err == nil {
			out[h] = true
		} else {
			out[h] = false
		}
	}
	return out
}

func applyProjection(name string, harnesses map[string]bool) error {
	src := filepath.Join(home.SkillsDir(), name)
	for h, on := range harnesses {
		dstDir := filepath.Join(home.HarnessDir(h), harnessSkillsDir(h))
		dst := filepath.Join(dstDir, name)
		if on {
			_ = os.MkdirAll(dstDir, 0o755)
			_ = os.Remove(dst)
			if err := os.Symlink(src, dst); err != nil {
				return err
			}
		} else {
			_ = os.Remove(dst)
		}
	}
	return nil
}

// Source names where a skill comes from, so a refresh can pull it again.
type Source struct {
	// Kind is git or folder.
	Kind string `json:"kind"`
	// URL is the git remote; Path is the folder. Give one.
	URL  string `json:"url,omitempty"`
	Path string `json:"path,omitempty"`
}

// ref returns the single source location, whichever field carries it.
func (src Source) ref() string {
	if src.URL != "" {
		return src.URL
	}
	return src.Path
}

// validate checks the kind and that exactly one location is set.
func (src Source) validate() error {
	switch src.Kind {
	case "git":
		if src.URL == "" {
			return fmt.Errorf("source kind git needs url")
		}
	case "folder":
		if src.Path == "" {
			return fmt.Errorf("source kind folder needs path")
		}
	default:
		return fmt.Errorf("source kind must be git or folder")
	}
	return nil
}

// fetch copies the source into dir. An existing dir is replaced.
func (src Source) fetch(ctx context.Context, dir string) error {
	if err := src.validate(); err != nil {
		return err
	}
	tmp := dir + ".fetch"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := src.fetchInto(ctx, tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if _, err := os.Stat(filepath.Join(tmp, "SKILL.md")); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("source has no SKILL.md")
	}
	if err := os.RemoveAll(dir); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	return os.Rename(tmp, dir)
}

func (src Source) fetchInto(ctx context.Context, tmp string) error {
	if src.Kind == "git" {
		out, err := exec.CommandContext(ctx, "git", "clone", "--depth", "1", src.URL, tmp).CombinedOutput()
		if err != nil {
			return fmt.Errorf("git clone: %s", strings.TrimSpace(string(out)))
		}
		return os.RemoveAll(filepath.Join(tmp, ".git"))
	}
	return copyTree(src.Path, tmp)
}

// Refresh pulls the skill source again. A skill with no source is unchanged.
func (s *Service) Refresh(ctx context.Context, r Ref) (*Skill, bool, error) {
	cur, err := s.Get(ctx, r, false)
	if err != nil {
		return nil, false, err
	}
	if err := guard(cur); err != nil {
		return nil, false, err
	}
	if cur.Source == nil {
		sk, err := s.Get(ctx, r, true)
		return sk, false, err
	}
	src := *cur.Source
	before := cur.Hash
	if err := src.fetch(ctx, cur.Path); err != nil {
		return nil, false, err
	}
	if hashDir(cur.Path) == before {
		sk, err := s.Get(ctx, r, true)
		return sk, false, err
	}
	if err := s.touch(ctx, cur, map[string]string{"source": src.Kind}); err != nil {
		return nil, false, err
	}
	sk, err := s.Get(ctx, r, true)
	return sk, true, err
}

// get returns one skill without the override fields.
func (s *Service) get(ctx context.Context, r Ref, includeMD bool) (*Skill, error) {
	if err := r.Check(); err != nil {
		return nil, err
	}
	o, _, err := s.owner(ctx, r)
	if err != nil {
		return nil, err
	}
	sk, err := s.load(ctx, r, o, includeMD)
	if err == nil {
		if r.Scope == ScopeGlobal {
			s.mergeInheritedMeta(sk)
		}
		return sk, nil
	}
	if r.Scope == ScopeGlobal {
		if ds := s.inheritedByName(r.Name); ds != nil {
			return s.skillFromDiscovered(*ds, includeMD), nil
		}
	}
	return nil, err
}

func (s *Service) touch(ctx context.Context, cur *Skill, attrs map[string]string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	ws := ""
	if cur.Owner != nil {
		ws = cur.Owner.WorkspaceID
	}
	if ws != "" {
		if attrs == nil {
			attrs = map[string]string{}
		}
		attrs["workspace"] = ws
	}
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE skill_index SET hash=?, path=?, updated_at=? WHERE ref=?`, hashDir(cur.Path), cur.Path, now, cur.Ref)
		return err
	}, store.BusEvent{
		Topic: "skill.updated", Entity: "skill", EntityID: cur.Ref, Action: "updated", Attrs: attrs,
	})
	return err
}

// load reads one skill folder and upserts its index row. A folder with no
// row (a file copy, a git pull) gets a local active row.
func (s *Service) load(ctx context.Context, r Ref, o Owner, includeMD bool) (*Skill, error) {
	dir := filepath.Join(home.SkillsDir(), r.Name)
	if r.Scope != ScopeGlobal {
		dir = filepath.Join(o.Dir, "skills", r.Name)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("skill %s: %w", r, err)
	}
	mdBytes, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	hash := hashDir(dir)
	prov := string(ProvenanceLocal)
	status := string(StatusActive)
	var used int
	var srcKind, srcRef, byKind, byJob, byRun, note sql.NullString
	_ = s.st.DB().QueryRowContext(ctx, `SELECT provenance, COALESCE(used_30d,0), source_kind, source_ref,
			created_by_kind, created_by_job, created_by_run, status, note
		FROM skill_index WHERE ref=?`, r.String()).Scan(&prov, &used, &srcKind, &srcRef, &byKind, &byJob, &byRun, &status, &note)

	sk := &Skill{
		Ref:         r.String(),
		Name:        r.Name,
		Scope:       r.Scope,
		Status:      Status(status),
		Note:        note.String,
		Provenance:  Provenance(prov),
		Description: skillDescription(string(mdBytes)),
		Version:     skillVersion(string(mdBytes)),
		UpdatedAt:   st.ModTime().UTC(),
		Hash:        hash,
		Path:        dir,
		Revision:    1,
		Used30d:     used,
		Scripts:     listScripts(dir),
		Requires:    parseRequires(string(mdBytes)),
	}
	if r.Scope == ScopeGlobal {
		sk.Harnesses = projectionState(r.Name)
	} else {
		owner := o
		sk.Owner = &owner
	}
	if byKind.String != "" {
		sk.CreatedBy = &CreatedBy{ActorKind: byKind.String, Job: byJob.String, RunID: byRun.String}
	}
	if srcKind.String != "" {
		sk.Source = &Source{Kind: srcKind.String}
		if srcKind.String == "connector" {
			sk.FromConnector = srcRef.String
		}
		if srcKind.String == "git" {
			sk.Source.URL = srcRef.String
		} else {
			sk.Source.Path = srcRef.String
		}
	}
	if includeMD {
		sk.SkillMD = string(mdBytes)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = s.st.DB().ExecContext(ctx, `INSERT INTO skill_index (ref, name, scope, owner_id, workspace_id, provenance, path, hash, used_30d, updated_at, status)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(ref) DO UPDATE SET hash=excluded.hash, path=excluded.path, updated_at=excluded.updated_at`,
		r.String(), r.Name, string(r.Scope), r.Owner, o.WorkspaceID, string(sk.Provenance), dir, hash, used, now, status) // the index is a cache of the folder
	return sk, nil
}

// upsertConnector writes the index row of a connector skill and emits topic.
func (s *Service) upsertConnector(ctx context.Context, r Ref, dir, connector, topic string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO skill_index (ref, name, scope, owner_id, workspace_id, provenance, path, hash, used_30d, updated_at, source_kind, source_ref, status)
			VALUES (?,?,?,'','',?,?,?,0,?,?,?,?)
			ON CONFLICT(ref) DO UPDATE SET path=excluded.path, hash=excluded.hash, updated_at=excluded.updated_at`,
			r.String(), r.Name, string(ScopeGlobal), string(ProvenanceConnector), dir, hashDir(dir), now, "connector", connector, string(StatusActive))
		return err
	}, store.BusEvent{
		Topic: topic, Entity: "skill", EntityID: r.String(), Action: strings.TrimPrefix(topic, "skill."),
		Attrs: map[string]string{"provenance": string(ProvenanceConnector)},
	})
	return err
}

// copyTree copies src into dst, skipping .git.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
}

// skillFolders are the folders of a skill next to SKILL.md.
var skillFolders = []string{"references", "scripts", "assets"} //nolint:gochecknoglobals // fixed list, never mutated

// copySkill copies a skill folder to dst. With root, it copies only SKILL.md
// and the skillFolders that exist.
func copySkill(dst, src string, root bool) error {
	if !root {
		return os.CopyFS(dst, os.DirFS(src))
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	body, err := os.ReadFile(filepath.Join(src, "SKILL.md"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dst, "SKILL.md"), body, 0o600); err != nil {
		return err
	}
	for _, sub := range skillFolders {
		if fi, err := os.Stat(filepath.Join(src, sub)); err != nil || !fi.IsDir() {
			continue
		}
		if err := os.CopyFS(filepath.Join(dst, sub), os.DirFS(filepath.Join(src, sub))); err != nil {
			return err
		}
	}
	return nil
}
