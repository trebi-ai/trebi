package connectors

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/procgroup"
)

// versionWait bounds one install.version_command run.
const versionWait = 5 * time.Second

// Finding is one doctor finding of a connector (Part E.12).
type Finding struct {
	Connector string
	Severity  string // error, warn, or info
	Code      string
	Message   string
}

// Doctor checks every installed connector: the manifest resolves, the
// binaries exist, the installed version matches, the skill fits, the live
// features match the promise, no two instances share one account, and
// whether an update is available.
func (s *Service) Doctor(ctx context.Context) []Finding {
	var out []Finding
	add := func(name, sev, code, format string, args ...any) {
		out = append(out, Finding{Connector: name, Severity: sev, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	entries := s.snapshotEntries()
	for _, name := range sortedNames(entries) {
		r, err := s.Resolve(name, "")
		if err != nil {
			add(name, "error", "connector_manifest_invalid", "connector %q does not resolve: %v", name, err)
			continue
		}
		m := r.Manifest
		path := r.PathEnv(os.Getenv("PATH"))
		var bins []string
		if m.Install != nil && m.Install.Bin != "" {
			bins = append(bins, m.Install.Bin)
		}
		if m.Actions != nil && m.Actions.CLI != nil {
			bins = append(bins, m.Actions.CLI.Commands...)
		}
		for _, bin := range slices.Compact(bins) {
			if lookPath(bin, path) == "" {
				add(name, "error", "connector_binary_missing", "connector %q: the command %q is not installed", name, bin)
			}
		}
		if m.Install != nil && m.Install.VersionCommand != "" {
			if got, ok := s.installedVersion(ctx, r); ok && !strings.Contains(got, r.Version) {
				add(name, "warn", "connector_version_mismatch", "connector %q: the installed program reports %q, not %s", name, got, r.Version)
			}
		}
		out = append(out, s.skillFindings(ctx, r)...)
		if v := s.availableUpdate(name); v != "" {
			add(name, "info", "connector_update_available", "connector %q: version %s is available (installed %s)", name, v, r.Version)
		}
		if p := s.procByName(r.Instance); p != nil {
			if have := p.snapshot().features(); have != nil {
				for _, f := range m.Features() {
					if !slices.Contains(have, f) {
						add(name, "warn", "connector_features_mismatch", "connector %q promises %s, but the running program does not deliver it", name, f)
					}
				}
			}
		}
	}
	return append(out, s.clashFindings()...)
}

// installedVersion runs install.version_command and returns its first line.
func (s *Service) installedVersion(ctx context.Context, r Resolved) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, versionWait)
	defer cancel()
	env := append(os.Environ(), "PATH="+r.PathEnv(os.Getenv("PATH")))
	outb, err := procgroup.Command{Script: r.Manifest.Install.VersionCommand, Env: env}.Run(ctx, 4096)
	if err != nil {
		return "", false
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(outb)), "\n")
	return line, line != ""
}

// skillFindings checks the skill of a connector with CLI actions.
func (s *Service) skillFindings(ctx context.Context, r Resolved) []Finding {
	m := r.Manifest
	if m.Actions == nil || m.Actions.CLI == nil || r.Dir == "" {
		return nil
	}
	var out []Finding
	if r.SkillDir() == "" {
		return []Finding{{r.Name, "warn", "connector_skill_missing", fmt.Sprintf("connector %q has CLI actions but no skill at %s", r.Name, m.SkillPath())}}
	}
	if rng, err := m.SkillCompat(); err != nil {
		out = append(out, Finding{r.Name, "warn", "connector_skill_compat", fmt.Sprintf("connector %q: skill.compat does not parse: %v", r.Name, err)})
	} else if v, err := manifest.ParseVersion(r.Version); err == nil && !rng.Contains(v) {
		out = append(out, Finding{r.Name, "warn", "connector_skill_compat", fmt.Sprintf("connector %q: the skill fits %s, not version %s", r.Name, rng, r.Version)})
	}
	if s.sk != nil {
		if sk, err := s.sk.Get(ctx, r.Name, false); err == nil && sk.FromConnector != r.Name {
			out = append(out, Finding{r.Name, "warn", "connector_skill_shadowed", fmt.Sprintf("connector %q: another skill named %q hides the connector skill", r.Name, r.Name)})
		}
	}
	return out
}

// clashFindings reports two live instances of one connector that are logged
// in to the same account.
func (s *Service) clashFindings() []Finding {
	s.mu.Lock()
	procs := make(map[string]*proc, len(s.procs))
	for k, p := range s.procs {
		procs[k] = p
	}
	s.mu.Unlock()
	seen := map[string]string{}
	var out []Finding
	for _, key := range sortedNames(procs) {
		acct := procs[key].snapshot().auth.Account
		if acct == nil || acct.ID == "" {
			continue
		}
		name, _ := splitInstance(key)
		id := name + "\x00" + acct.ID
		if other, ok := seen[id]; ok {
			out = append(out, Finding{name, "error", "connector_instance_clash",
				fmt.Sprintf("instances %s and %s of connector %q use the same account; one of them misses messages", other, key, name)})
			continue
		}
		seen[id] = key
	}
	return out
}
