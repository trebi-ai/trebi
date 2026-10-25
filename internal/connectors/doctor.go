package connectors

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/procgroup"
	"github.com/trebi-ai/trebi/internal/skills"
)

const (
	// versionWait bounds one install.version_command run.
	versionWait = 5 * time.Second
	// tokenSoon is how near the end of a token that a run cannot renew
	// makes a doctor finding.
	tokenSoon = 10 * time.Minute
)

// Finding is one doctor finding of a connector (Part E.12).
type Finding struct {
	Connector string
	Severity  string // error, warn, or info
	Code      string
	Message   string
}

// Doctor checks every connection: the folder loads, the manifest resolves,
// the file source and the user secrets exist, the binaries exist, the
// installed version matches, the skill fits, the live features match the
// promise, no two connections share one account, and whether an update is
// available. Connector is the connection id.
func (s *Service) Doctor(ctx context.Context) []Finding {
	var out []Finding
	add := func(id, sev, code, format string, args ...any) {
		out = append(out, Finding{Connector: id, Severity: sev, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	s.mu.Lock()
	broken := maps.Clone(s.loadErr)
	s.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(broken)) {
		add(id, "error", "connection_invalid", "connection %s does not load; writes to it are refused: %v", id, broken[id])
	}
	var trigs map[string][]cachedTrig
	if s.ws != nil {
		trigs = s.triggerIndex(ctx)
	}
	for _, c := range s.snapshot() {
		id, name := c.ID, c.Slug
		if path := c.FilePath(); path != "" {
			if _, err := os.Stat(path); err != nil {
				add(id, "error", "connection_package_missing", "connector %q: the manifest file %s does not exist", name, path)
				continue
			}
		}
		r, err := s.Resolve(id, "")
		if err != nil {
			add(id, "error", "connector_manifest_invalid", "connector %q does not resolve: %v", name, err)
			continue
		}
		if _, missing := r.Env(); len(missing) > 0 {
			add(id, "error", "connection_secret_missing", "connector %q: the secrets %s do not exist", name, strings.Join(missing, ", "))
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
				add(id, "error", "connector_binary_missing", "connector %q: the command %q is not installed", name, bin)
			}
		}
		if m.Install != nil && m.Install.VersionCommand != "" {
			if got, ok := s.installedVersion(ctx, r); ok && !strings.Contains(got, r.Version) {
				add(id, "warn", "connector_version_mismatch", "connector %q: the installed program reports %q, not %s", name, got, r.Version)
			}
		}
		out = append(out, skipFindings(c, r, trigs[c.Slug])...)
		out = append(out, s.skillFindings(ctx, r)...)
		out = append(out, s.oauthFindings(r)...)
		if spec := m.SubscriptionSpec(); spec != nil && len(c.Subscriptions) > 0 && r.Webhook == nil &&
			!slices.Contains(spec.Modes, manifest.ModeStream) && !slices.Contains(spec.Modes, manifest.ModePoll) {
			add(id, "warn", "connector_webhook_unavailable", "%s needs Trebi Cloud for live events", cmp.Or(c.Title, m.Title))
		}
		if u := s.availableUpdate(id); u.Version != "" {
			add(id, "info", "connector_update_available", "connector %q: version %s is available (installed %s)", name, u.Version, r.Version)
		}
		if p := s.procByName(id); p != nil {
			if have := p.snapshot().features(); have != nil {
				for _, f := range m.Features() {
					if !slices.Contains(have, f) {
						add(id, "warn", "connector_features_mismatch", "connector %q promises %s, but the running program does not deliver it", name, f)
					}
				}
			}
		}
	}
	out = append(out, s.packageFindings()...)
	out = append(out, s.linkFindings()...)
	return append(out, s.clashFindings()...)
}

// skipFindings reports each live trigger that waits on a type that the
// connection skips.
func skipFindings(c Connection, r Resolved, trigs []cachedTrig) []Finding {
	declared := declaredTypes(r.Manifest)
	var out []Finding
	for _, t := range trigs {
		if !t.live() {
			continue
		}
		for _, typ := range skippedBy(c.Receive, t.tr.Match, declared) {
			msg := fmt.Sprintf("%s starts on %s from %s, and %s skips it.", t.row.Name, typ, r.Title, r.Title)
			if typ == "" {
				msg = fmt.Sprintf("%s starts on events from %s, and %s receives none.", t.row.Name, r.Title, r.Title)
			}
			out = append(out, Finding{c.ID, "warn", "connector_event_skipped", msg})
		}
	}
	return out
}

// packageFindings reports a package that does not match its pin and a link
// that the last update check could not reach.
func (s *Service) packageFindings() []Finding {
	s.mu.Lock()
	pkg, src := maps.Clone(s.pkgErr), maps.Clone(s.srcErr)
	s.mu.Unlock()
	var out []Finding
	for _, id := range slices.Sorted(maps.Keys(pkg)) {
		if errors.Is(pkg[id], ErrDigestMismatch) {
			out = append(out, Finding{id, "error", "connector_digest_mismatch",
				fmt.Sprintf("connector %q: the program files do not match the pinned version, so Trebi does not start it: %v", s.SlugOf(id), pkg[id])})
		}
	}
	for _, id := range slices.Sorted(maps.Keys(src)) {
		c, err := s.Lookup(id)
		if err != nil || c.Source == nil {
			continue
		}
		out = append(out, Finding{id, "warn", "connector_source_unreachable",
			fmt.Sprintf("connector %q: Trebi cannot open %s to look for updates. Check that the repository exists and that the GitHub login on this computer can read it.", c.Slug, c.Source.URL())})
	}
	return out
}

// oauthFindings reports a connection that needs an OAuth login, and a
// token in the run config that ends soon and cannot renew.
func (s *Service) oauthFindings(r Resolved) []Finding {
	if _, ok := s.oauthMethod(r); !ok {
		return nil
	}
	st, err := s.oauth.Status(r.ID)
	if err != nil {
		return nil
	}
	if !st.LoggedIn {
		return []Finding{{r.ID, "warn", "connector_login_required", fmt.Sprintf("%s needs a login. Jobs cannot use it until you log in.", r.Title)}}
	}
	_, set, err := s.oauthConn(r)
	if err != nil || set.Mode != OAuthHeader || st.Renewable || st.Expiry.IsZero() || st.Expiry.Sub(s.now()) >= tokenSoon {
		return nil
	}
	return []Finding{{r.ID, "warn", "connector_token_expiring", fmt.Sprintf("The token of %s ends at %s and cannot renew. Log in again before that time.", r.Title, st.Expiry.UTC().Format(time.RFC3339))}}
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
		return []Finding{{r.ID, "warn", "connector_skill_missing", fmt.Sprintf("connector %q has CLI actions but no skill at %s", r.Slug, m.SkillPath())}}
	}
	if rng, err := m.SkillCompat(); err != nil {
		out = append(out, Finding{r.ID, "warn", "connector_skill_compat", fmt.Sprintf("connector %q: skill.compat does not parse: %v", r.Slug, err)})
	} else if v, err := manifest.ParseVersion(r.Version); err == nil && !rng.Contains(v) {
		out = append(out, Finding{r.ID, "warn", "connector_skill_compat", fmt.Sprintf("connector %q: the skill fits %s, not version %s", r.Slug, rng, r.Version)})
	}
	if s.sk != nil {
		if sk, err := s.sk.Get(ctx, skills.GlobalRef(r.Type), false); err == nil && sk.FromConnector != r.Type {
			out = append(out, Finding{r.ID, "warn", "connector_skill_shadowed", fmt.Sprintf("connector %q: another skill named %q hides the connector skill", r.Slug, r.Type)})
		}
	}
	return out
}

// clashFindings reports two live connections of one type that are logged
// in to the same account.
func (s *Service) clashFindings() []Finding {
	s.mu.Lock()
	procs := maps.Clone(s.procs)
	s.mu.Unlock()
	seen := map[string]string{}
	var out []Finding
	for _, id := range slices.Sorted(maps.Keys(procs)) {
		p := procs[id]
		acct := p.snapshot().auth.Account
		if acct == nil || acct.ID == "" {
			continue
		}
		key := p.res.Type + "\x00" + acct.ID
		if other, ok := seen[key]; ok {
			out = append(out, Finding{id, "error", "connector_instance_clash",
				fmt.Sprintf("connections %s and %s of type %q use the same account; one of them misses messages", s.SlugOf(other), s.SlugOf(id), p.res.Type)})
			continue
		}
		seen[key] = id
	}
	return out
}
