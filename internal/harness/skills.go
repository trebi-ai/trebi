package harness

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/config"
)

// SkillOrigin is one user-home directory a discovered skill was found in.
type SkillOrigin struct {
	Kind string `json:"kind"` // user
	Tool string `json:"tool"`
	Path string `json:"path"`
	Key  string `json:"key"`
}

// DiscoveredSkill is one logical skill after unioning duplicate names.
type DiscoveredSkill struct {
	Name      string        `json:"name"`
	Path      string        `json:"path"`
	Origins   []SkillOrigin `json:"origins"`
	Harnesses []string      `json:"harnesses"`
	Injected  bool          `json:"injected"`
}

var skillScanTools = []string{"claude", "grok", "codex", "opencode", "opencode2", "cursor", "pi"}

const agentsSkillsRel = ".agents/skills"

// DiscoverSkills scans user-global harness skill dirs that EnsureBridges injects
// and the shared Agent Skills dir (~/.agents/skills). A SKILL.md that is a
// single `@path` line is followed to that file; Path is the directory that
// holds the real document. userHome empty skips the scan (tests). cfg nil
// loads trebi config for inherit.
func DiscoverSkills(userHome string, cfg *config.Config) []DiscoveredSkill {
	if userHome == "" {
		return nil
	}
	if cfg == nil {
		cfg, _ = config.Load()
	}
	byName := map[string]*DiscoveredSkill{}
	var order []string
	for _, tool := range skillScanTools {
		for _, def := range Catalog(tool) {
			if def.Key != "skills" || def.UserRel == "" {
				continue
			}
			injected := config.ResolveResourceInherit(tool, "skills", cfg)
			scanSkillRoot(filepath.Join(userHome, def.UserRel), tool, userHome, injected, true, byName, &order)
		}
	}
	scanSkillRoot(filepath.Join(userHome, agentsSkillsRel), "agents", userHome, false, false, byName, &order)
	out := make([]DiscoveredSkill, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out
}

func scanSkillRoot(
	root, tool, userHome string,
	injected, asHarness bool,
	byName map[string]*DiscoveredSkill,
	order *[]string,
) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(root, name)
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() || !hasSkillMD(dir) {
			continue
		}
		resolved := ResolveSkillDir(dir, userHome)
		sk, ok := byName[name]
		if !ok {
			sk = &DiscoveredSkill{Name: name, Path: resolved}
			byName[name] = sk
			*order = append(*order, name)
		} else if skillMDIsPointer(sk.Path) && !skillMDIsPointer(resolved) {
			sk.Path = resolved
		}
		sk.Origins = append(sk.Origins, SkillOrigin{Kind: "user", Tool: tool, Path: dir, Key: name})
		if asHarness {
			sk.Harnesses = appendUnique(sk.Harnesses, tool)
		}
		if injected {
			sk.Injected = true
		}
	}
}

func hasSkillMD(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil && !st.IsDir()
}

func skillMDIsPointer(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return false
	}
	return skillPointerTarget(b) != ""
}

func skillPointerTarget(md []byte) string {
	s := strings.TrimSpace(string(md))
	if !strings.HasPrefix(s, "@") || strings.Contains(s, "\n") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(s, "@"))
}

// ResolveSkillDir returns dir, or the directory a one-line @path SKILL.md points at.
func ResolveSkillDir(dir, userHome string) string {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return dir
	}
	tgt := skillPointerTarget(b)
	if tgt == "" {
		return dir
	}
	resolved, _, ok := resolveSkillPointer(dir, tgt, userHome)
	if !ok {
		return dir
	}
	return resolved
}

// ReadSkillMD reads SKILL.md, following a one-line @path pointer under userHome.
func ReadSkillMD(dir, userHome string) []byte {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return nil
	}
	for i := 0; i < 4; i++ {
		tgt := skillPointerTarget(b)
		if tgt == "" {
			return b
		}
		nextDir, mdPath, ok := resolveSkillPointer(dir, tgt, userHome)
		if !ok {
			return b
		}
		next, err := os.ReadFile(mdPath)
		if err != nil {
			return b
		}
		dir, b = nextDir, next
	}
	return b
}

func resolveSkillPointer(fromDir, target, userHome string) (dir, mdPath string, ok bool) {
	if userHome == "" || target == "" {
		return "", "", false
	}
	if strings.HasPrefix(target, "~/") {
		target = filepath.Join(userHome, strings.TrimPrefix(target, "~/"))
	} else if !filepath.IsAbs(target) {
		target = filepath.Join(fromDir, target)
	}
	target = filepath.Clean(target)
	rel, err := filepath.Rel(filepath.Clean(userHome), target)
	if err != nil || !filepath.IsLocal(rel) {
		return "", "", false
	}
	st, err := os.Stat(target)
	if err != nil || st.IsDir() {
		return "", "", false
	}
	return filepath.Dir(target), target, true
}
