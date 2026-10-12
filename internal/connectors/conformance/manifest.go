package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

// manifestSuite checks the manifest, its event schemas, and its skill.
type manifestSuite struct{}

func (manifestSuite) name() string                   { return SuiteManifest }
func (manifestSuite) applies(manifest.Manifest) bool { return true }

func (manifestSuite) run(_ context.Context, r *run) []Result {
	m := r.t.Manifest
	out := []Result{row(SuiteManifest, "validate", m.Validate())}
	if r.t.Dir == "" {
		return append(out,
			Result{Suite: SuiteManifest, Check: "event schemas", Status: StatusSkip, Message: "no manifest folder"},
			Result{Suite: SuiteManifest, Check: "skill", Status: StatusSkip, Message: "no manifest folder"})
	}
	schemas := 0
	if m.Events != nil {
		for _, t := range m.Events.Types {
			if t.Schema != "" {
				schemas++
			}
		}
	}
	if schemas == 0 {
		out = append(out, Result{Suite: SuiteManifest, Check: "event schemas", Status: StatusSkip, Message: "no event schema"})
	} else {
		_, err := m.EventSchemas(r.t.Dir)
		out = append(out, row(SuiteManifest, "event schemas", err))
	}
	out = append(out, skillRow(m, r.t.Dir), compatRow(m))
	if m.Install != nil && m.Install.Build != nil {
		_, err := m.Install.Build.ModuleDir(r.t.Dir)
		out = append(out, row(SuiteManifest, "build module", err))
	}
	return out
}

// skillRow checks that SKILL.md exists. A connector with actions needs it.
func skillRow(m manifest.Manifest, dir string) Result {
	path := filepath.Join(dir, filepath.FromSlash(m.SkillPath()), "SKILL.md")
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return Result{Suite: SuiteManifest, Check: "skill", Status: StatusPass}
	case m.Actions != nil:
		return Result{Suite: SuiteManifest, Check: "skill", Status: StatusFail, Message: "the connector has actions but no " + path}
	}
	return Result{Suite: SuiteManifest, Check: "skill", Status: StatusWarn, Message: "no " + path}
}

// compatRow checks that skill.compat holds the manifest version.
func compatRow(m manifest.Manifest) Result {
	rng, err := m.SkillCompat()
	if err != nil {
		return row(SuiteManifest, "skill compat", err)
	}
	v, err := manifest.ParseVersion(m.Version)
	if err != nil {
		return row(SuiteManifest, "skill compat", err)
	}
	if !rng.Contains(v) {
		return row(SuiteManifest, "skill compat", fmt.Errorf("skill.compat %s does not hold version %s", rng, m.Version))
	}
	return row(SuiteManifest, "skill compat", nil)
}
