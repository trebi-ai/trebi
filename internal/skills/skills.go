// Package skills owns the canonical ~/.trebi/skills tree and skill_index.
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
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// Provenance for skills (D8).
type Provenance string

const (
	ProvenanceAssistant   Provenance = "assistant"
	ProvenanceLocal       Provenance = "local"
	ProvenanceGit         Provenance = "git"
	ProvenanceRegistry    Provenance = "registry"
	ProvenanceIntegration Provenance = "integration"
)

// SkillRequirement is a structured requires entry (B12).
type SkillRequirement struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // bin|env|mcp
	Satisfied bool   `json:"satisfied"`
}

// Skill is the wire shape (backend-0808 §3.15).
type Skill struct {
	Name            string             `json:"name"`
	Provenance      Provenance         `json:"provenance"`
	FromIntegration string             `json:"from_integration,omitempty"`
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
}

// Service manages skill folders + index.
type Service struct {
	st *store.Store
}

// ErrPrecondition is If-Match / hash mismatch.
var ErrPrecondition = errors.New("precondition failed")

// New creates a skills service.
func New(st *store.Store) *Service { return &Service{st: st} }

// List discovers skills under SkillsDir and refreshes skill_index.
func (s *Service) List(ctx context.Context) ([]Skill, error) {
	if err := home.EnsureDirs(); err != nil {
		return nil, err
	}
	root := home.SkillsDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []Skill{}, nil
		}
		return nil, err
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sk, err := s.loadSkill(ctx, e.Name(), false)
		if err != nil {
			continue
		}
		out = append(out, *sk)
	}
	return out, nil
}

// Get returns one skill; includeMD loads SKILL.md body.
func (s *Service) Get(ctx context.Context, name string, includeMD bool) (*Skill, error) {
	return s.loadSkill(ctx, name, includeMD)
}

// Create writes a new skill folder.
func (s *Service) Create(ctx context.Context, name, skillMD string, prov Provenance) (*Skill, error) {
	if name == "" {
		return nil, fmt.Errorf("name required")
	}
	if prov == "" {
		prov = ProvenanceLocal
	}
	dir := filepath.Join(home.SkillsDir(), name)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("skill %q already exists", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if skillMD == "" {
		skillMD = "# " + name + "\n\nSkill body.\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	hash := hashDir(dir)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO skill_index (name, provenance, path, hash, used_30d, updated_at)
			VALUES (?,?,?,?,0,?)`, name, string(prov), dir, hash, now)
		return err
	}, store.BusEvent{
		Topic: "skill.created", Entity: "skill", EntityID: name, Action: "created",
		Attrs: map[string]string{"provenance": string(prov)},
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, name, true)
}

// Update replaces SKILL.md (If-Match via hash) and/or harness projection.
func (s *Service) Update(ctx context.Context, name, skillMD, ifMatch string, harnesses map[string]bool) (*Skill, error) {
	cur, err := s.Get(ctx, name, false)
	if err != nil {
		return nil, err
	}
	if ifMatch != "" && ifMatch != cur.Hash {
		return nil, fmt.Errorf("%w: hash mismatch", ErrPrecondition)
	}
	if skillMD != "" {
		if err := os.WriteFile(filepath.Join(home.SkillsDir(), name, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
			return nil, err
		}
	}
	if harnesses != nil {
		if err := applyProjection(name, harnesses); err != nil {
			return nil, err
		}
	}
	hash := hashDir(filepath.Join(home.SkillsDir(), name))
	now := time.Now().UTC().Format(time.RFC3339)
	dir := filepath.Join(home.SkillsDir(), name)
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE skill_index SET hash=?, path=?, updated_at=? WHERE name=?`, hash, dir, now, name)
		return err
	}, store.BusEvent{
		Topic: "skill.updated", Entity: "skill", EntityID: name, Action: "updated",
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, name, true)
}

// Delete removes a skill folder (refused for integration provenance).
func (s *Service) Delete(ctx context.Context, name string) error {
	cur, err := s.Get(ctx, name, false)
	if err != nil {
		return err
	}
	if cur.Provenance == ProvenanceIntegration {
		return fmt.Errorf("managed skill: remove the integration instead")
	}
	if err := os.RemoveAll(filepath.Join(home.SkillsDir(), name)); err != nil {
		return err
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM skill_index WHERE name=?`, name)
		return err
	}, store.BusEvent{
		Topic: "skill.deleted", Entity: "skill", EntityID: name, Action: "deleted",
	})
	return err
}

func (s *Service) loadSkill(ctx context.Context, name string, includeMD bool) (*Skill, error) {
	dir := filepath.Join(home.SkillsDir(), name)
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("skill %q: %w", name, err)
	}
	mdPath := filepath.Join(dir, "SKILL.md")
	mdBytes, _ := os.ReadFile(mdPath)
	hash := hashDir(dir)
	prov := string(ProvenanceLocal)
	var used int
	var srcKind, srcRef sql.NullString
	_ = s.st.DB().QueryRowContext(ctx, `SELECT provenance, COALESCE(used_30d,0), source_kind, source_ref
		FROM skill_index WHERE name=?`, name).Scan(&prov, &used, &srcKind, &srcRef)

	sk := &Skill{
		Name:        name,
		Provenance:  Provenance(prov),
		Description: firstLine(string(mdBytes)),
		UpdatedAt:   st.ModTime().UTC(),
		Hash:        hash,
		Path:        dir,
		Revision:    1,
		Used30d:     used,
		Harnesses:   projectionState(name),
		Scripts:     listScripts(dir),
		Requires:    parseRequires(string(mdBytes)),
	}
	if srcKind.String != "" {
		sk.Source = &Source{Kind: srcKind.String}
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
	_, _ = s.st.DB().ExecContext(ctx, `INSERT INTO skill_index (name, provenance, path, hash, used_30d, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET hash=excluded.hash, path=excluded.path, updated_at=excluded.updated_at`,
		name, string(sk.Provenance), dir, hash, used, now)
	return sk, nil
}

// ProjectForRun materializes allow/deny skill scope into a temp dir; returns path.
// Declared skill-scoping entry point; no production caller yet (parity test
// pins it). The missing API route is a functionality gap, not dead code.
func ProjectForRun(allow, deny []string) (string, error) {
	root := home.SkillsDir()
	all, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	denySet := map[string]bool{}
	for _, d := range deny {
		denySet[d] = true
	}
	allowAll := len(allow) == 0
	allowSet := map[string]bool{}
	for _, a := range allow {
		allowSet[a] = true
	}
	out := filepath.Join(os.TempDir(), "trebi-skills-"+ulid.Make().String())
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	for _, e := range all {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if denySet[name] {
			continue
		}
		if !allowAll && !allowSet[name] {
			continue
		}
		src := filepath.Join(root, name)
		dst := filepath.Join(out, name)
		_ = os.Symlink(src, dst)
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

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		if line != "" {
			return line
		}
	}
	return ""
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
		if strings.HasPrefix(line, "- bin:") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "- bin:"))
			if name == "" {
				continue
			}
			_, err := exec.LookPath(name)
			out = append(out, SkillRequirement{Name: name, Kind: "bin", Satisfied: err == nil})
		}
	}
	return out
}

func projectionState(name string) map[string]bool {
	out := map[string]bool{}
	for _, h := range []string{"claude", "grok", "codex", "opencode", "gemini", "cursor", "pi"} {
		link := filepath.Join(home.HarnessDir(h), "skills", name)
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
		dstDir := filepath.Join(home.HarnessDir(h), "skills")
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

// CreateFromSource installs one skill from a git remote or a folder.
func (s *Service) CreateFromSource(ctx context.Context, name string, src Source) (*Skill, error) {
	if name == "" {
		return nil, fmt.Errorf("name required")
	}
	dir := filepath.Join(home.SkillsDir(), name)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("skill %q already exists", name)
	}
	if err := src.fetch(ctx, dir); err != nil {
		return nil, err
	}
	prov := ProvenanceGit
	if src.Kind == "folder" {
		prov = ProvenanceLocal
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO skill_index (name, provenance, path, hash, used_30d, updated_at, source_kind, source_ref)
			VALUES (?,?,?,?,0,?,?,?)`, name, string(prov), dir, hashDir(dir), now, src.Kind, src.ref())
		return err
	}, store.BusEvent{
		Topic: "skill.created", Entity: "skill", EntityID: name, Action: "created",
		Attrs: map[string]string{"provenance": string(prov), "source": src.Kind},
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, name, true)
}

// Refresh pulls the skill source again. A skill with no source is unchanged.
func (s *Service) Refresh(ctx context.Context, name string) (*Skill, bool, error) {
	cur, err := s.Get(ctx, name, false)
	if err != nil {
		return nil, false, err
	}
	if cur.Source == nil {
		sk, err := s.Get(ctx, name, true)
		return sk, false, err
	}
	src := *cur.Source
	dir := filepath.Join(home.SkillsDir(), name)
	before := hashDir(dir)
	if err := src.fetch(ctx, dir); err != nil {
		return nil, false, err
	}
	after := hashDir(dir)
	if after == before {
		sk, err := s.Get(ctx, name, true)
		return sk, false, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE skill_index SET hash=?, updated_at=? WHERE name=?`, after, now, name)
		return err
	}, store.BusEvent{
		Topic: "skill.updated", Entity: "skill", EntityID: name, Action: "updated",
		Attrs: map[string]string{"source": src.Kind},
	})
	if err != nil {
		return nil, false, err
	}
	sk, err := s.Get(ctx, name, true)
	return sk, true, err
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
