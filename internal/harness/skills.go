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

var skillScanTools = []string{"claude", "grok", "codex", "opencode", "cursor", "pi"}

// DiscoverSkills scans user-global harness skill dirs that EnsureBridges injects.
// userHome empty skips the scan (tests). cfg nil loads trebi config for inherit.
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
			root := filepath.Join(userHome, def.UserRel)
			entries, err := os.ReadDir(root)
			if err != nil {
				continue
			}
			injected := config.ResolveResourceInherit(tool, "skills", cfg)
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
				sk, ok := byName[name]
				if !ok {
					sk = &DiscoveredSkill{Name: name, Path: dir}
					byName[name] = sk
					order = append(order, name)
				}
				sk.Origins = append(sk.Origins, SkillOrigin{Kind: "user", Tool: tool, Path: dir, Key: name})
				sk.Harnesses = appendUnique(sk.Harnesses, tool)
				if injected {
					sk.Injected = true
				}
			}
		}
	}
	out := make([]DiscoveredSkill, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out
}

func hasSkillMD(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil && !st.IsDir()
}
