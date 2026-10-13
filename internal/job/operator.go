package job

import (
	"cmp"
	"fmt"
	"net/mail"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// EmailYAML is the inbound-mail gate shared by agents and the workspace
// assistant (2026-09-24 plan B.1/B.2).
type EmailYAML struct {
	Accept []string   `yaml:"accept"`
	Style  *StyleYAML `yaml:"style,omitempty"`
}

// StyleYAML changes the house style of the HTML mail. An empty field keeps
// the house value.
type StyleYAML struct {
	Accent string `yaml:"accent,omitempty" json:"accent,omitempty"` // #rgb or #rrggbb
	Logo   string `yaml:"logo,omitempty" json:"logo,omitempty"`     // https URL of an image
	Footer string `yaml:"footer,omitempty" json:"footer,omitempty"` // one line of plain text
}

var hexColor = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}){1,2}$`)

func (s StyleYAML) Validate() error {
	if s.Accent != "" && !hexColor.MatchString(s.Accent) {
		return fmt.Errorf("email.style.accent %q must be a color such as #2563eb", s.Accent)
	}
	if s.Logo != "" {
		u, err := url.Parse(s.Logo)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("email.style.logo must be an https URL")
		}
	}
	if len(s.Footer) > 200 || strings.ContainsAny(s.Footer, "\r\n") {
		return fmt.Errorf("email.style.footer must be one line of at most 200 characters")
	}
	return nil
}

// Merge returns s with each empty field taken from base.
func (s *StyleYAML) Merge(base StyleYAML) StyleYAML {
	if s == nil {
		return base
	}
	return StyleYAML{
		Accent: cmp.Or(s.Accent, base.Accent),
		Logo:   cmp.Or(s.Logo, base.Logo),
		Footer: cmp.Or(s.Footer, base.Footer),
	}
}

// EmailConnector is the built-in connector name for native email. A kit or
// job lists it next to managed connectors; no connection row backs it.
const EmailConnector = "email"

// KitYAML is an agent's reusable run configuration. Owned jobs may inherit
// it; job-level values take precedence (plan A.10 and Part J). Connectors
// merge: a job adds to the kit list.
type KitYAML struct {
	Envset     EnvsetList        `yaml:"envset,omitempty" json:"envset,omitempty"`
	Env        map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Skills     []string          `yaml:"skills,omitempty" json:"skills,omitempty"`
	Connectors []string          `yaml:"connectors,omitempty" json:"connectors,omitempty"`
	// WorkingDir is the expanded working_dir of the owner agent. Job.OwnerKit
	// sets it; it is not a kit key.
	WorkingDir string `yaml:"-" json:"-"`
}

// BudgetYAML is the optional monthly spend guardrail.
type BudgetYAML struct {
	MonthlyUSD float64 `yaml:"monthly_usd,omitempty"`
	OnExceed   string  `yaml:"on_exceed,omitempty"` // pause | alert; empty is pause
}

// TeamYAML limits which agents an agent may ask. A nil CanAsk is every
// agent of the workspace; Chains false keeps the agent out of all chains.
type TeamYAML struct {
	CanAsk []string `yaml:"can_ask,omitempty" json:"can_ask,omitempty"`
	Chains *bool    `yaml:"chains,omitempty" json:"chains,omitempty"`
}

// InChains reports whether the agent asks and takes asks. The default is true.
func (t *TeamYAML) InChains() bool {
	return t == nil || t.Chains == nil || *t.Chains
}

// Allows reports whether the agent may ask the agent with this short name.
func (t *TeamYAML) Allows(name string) bool {
	return t == nil || t.CanAsk == nil || slices.Contains(t.CanAsk, name)
}

// ArtifactYAML is one file that the run saves as an artifact version when
// it completes.
type ArtifactYAML struct {
	Path  string `yaml:"path" json:"path"`
	Name  string `yaml:"name,omitempty" json:"name,omitempty"`
	Title string `yaml:"title,omitempty" json:"title,omitempty"`
}

// ReportYAML configures the optional recurring portfolio report.
type ReportYAML struct {
	Cron  string   `yaml:"cron,omitempty"`
	Every string   `yaml:"every,omitempty"`
	To    []string `yaml:"to,omitempty"`
}

// AcceptEntry is one parsed accept line: a keyword, an exact address, or a
// whole domain. Exactly one field is populated.
type AcceptEntry struct {
	Keyword string
	Address string
	Domain  string
}

// ParseAccept validates and normalizes an email.accept list. Wildcards are
// intentionally not supported (plan A.5).
func ParseAccept(lines []string) ([]AcceptEntry, error) {
	out := make([]AcceptEntry, 0, len(lines))
	for i, raw := range lines {
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil, fmt.Errorf("email.accept[%d]: empty entry", i)
		}
		switch strings.ToLower(value) {
		case "admins":
			out = append(out, AcceptEntry{Keyword: "admins"})
			continue
		case "operators":
			return nil, fmt.Errorf(`email.accept[%d]: unknown accept token "operators"; use "admins"`, i)
		case "allowlist":
			out = append(out, AcceptEntry{Keyword: "allowlist"})
			continue
		case "*":
			return nil, fmt.Errorf("email.accept[%d]: wildcards are not supported", i)
		}
		value = strings.ToLower(value)
		if strings.HasPrefix(value, "@") {
			domain := strings.TrimPrefix(value, "@")
			if err := validDomain(domain); err != nil {
				return nil, fmt.Errorf("email.accept[%d]: %w", i, err)
			}
			out = append(out, AcceptEntry{Domain: domain})
			continue
		}
		addr, err := mail.ParseAddress(value)
		if err != nil || !strings.EqualFold(addr.Address, value) || strings.ContainsAny(addr.Address, " \t") {
			return nil, fmt.Errorf("email.accept[%d]: want admins, allowlist, an address, or @domain", i)
		}
		out = append(out, AcceptEntry{Address: strings.ToLower(addr.Address)})
	}
	return out, nil
}

func validDomain(domain string) error {
	if domain == "" || strings.ContainsAny(domain, " \t@*") {
		return fmt.Errorf("invalid domain %q", domain)
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return fmt.Errorf("invalid domain %q", domain)
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("invalid domain %q", domain)
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return fmt.Errorf("invalid domain %q", domain)
			}
		}
	}
	return nil
}

// normalizeOperatorKeys parses email.accept and rejects operator-only keys
// where they do not belong. Job files cannot carry any operator key.
func normalizeOperatorKeys(path string, fm *Frontmatter) (accept []AcceptEntry, raw []string, acceptSet bool, err error) {
	kind := kindFromPath(path)
	if kind == "job" && (fm.Email != nil || fm.Kit != nil || fm.Budget != nil || fm.Report != nil) {
		return nil, nil, false, fmt.Errorf("operator keys email, kit, budget, and report are not allowed in JOB.md")
	}
	if fm.Budget != nil {
		if fm.Budget.MonthlyUSD <= 0 {
			return nil, nil, false, fmt.Errorf("budget.monthly_usd must be greater than zero")
		}
		if fm.Budget.OnExceed == "" {
			fm.Budget.OnExceed = "pause"
		}
		if fm.Budget.OnExceed != "pause" && fm.Budget.OnExceed != "alert" {
			return nil, nil, false, fmt.Errorf("budget.on_exceed must be pause or alert")
		}
	}
	if fm.Report != nil {
		if (fm.Report.Cron == "") == (fm.Report.Every == "") {
			return nil, nil, false, fmt.Errorf("report: set exactly one of cron or every")
		}
		if _, err := ParseAccept(fm.Report.To); err != nil {
			return nil, nil, false, fmt.Errorf("report.to: %w", err)
		}
	}
	if fm.Email == nil {
		return nil, nil, false, nil
	}
	if fm.Email.Style != nil {
		if err := fm.Email.Style.Validate(); err != nil {
			return nil, fm.Email.Accept, true, err
		}
	}
	accept, err = ParseAccept(fm.Email.Accept)
	if err != nil {
		return nil, fm.Email.Accept, true, err
	}
	return accept, fm.Email.Accept, true, nil
}

// legacyKitMCP returns the names of an old kit.mcp list in the raw
// frontmatter. The decode is not strict, so the doctor reports the list
// (kit_mcp_removed) instead of a silent drop.
func legacyKitMCP(data []byte) []string {
	block, ok := extractFM(data)
	if !ok {
		return nil
	}
	var raw struct {
		Kit struct {
			MCP any `yaml:"mcp"`
		} `yaml:"kit"`
	}
	if err := yaml.Unmarshal(block, &raw); err != nil || raw.Kit.MCP == nil {
		return nil
	}
	items, ok := raw.Kit.MCP.([]any)
	if !ok {
		items = []any{raw.Kit.MCP}
	}
	names := []string{}
	for _, it := range items {
		switch v := it.(type) {
		case string:
			names = append(names, v)
		case map[string]any:
			if n, ok := v["name"].(string); ok {
				names = append(names, n)
			}
		}
	}
	return names
}

var artifactNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidArtifactName reports whether name is an artifact slug.
func ValidArtifactName(name string) bool { return artifactNameRE.MatchString(name) }

// ArtifactSlug turns a file stem or a title into an artifact name.
func ArtifactSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	return out
}

// parseArtifacts checks the artifacts entries and fills the default name and
// title.
func parseArtifacts(in []ArtifactYAML) ([]ArtifactYAML, error) {
	out := make([]ArtifactYAML, 0, len(in))
	seen := map[string]bool{}
	for i, a := range in {
		p := strings.TrimSpace(a.Path)
		clean := path.Clean(strings.ReplaceAll(p, "\\", "/"))
		switch {
		case p == "":
			return nil, fmt.Errorf("entry %d: path is required", i+1)
		case path.IsAbs(clean) || strings.HasPrefix(p, "~") || clean == ".." || strings.HasPrefix(clean, "../"):
			return nil, fmt.Errorf("entry %d: path %q must be inside the working dir", i+1, p)
		}
		if a.Name == "" {
			a.Name = ArtifactSlug(strings.TrimSuffix(path.Base(clean), path.Ext(clean)))
		}
		if !ValidArtifactName(a.Name) {
			return nil, fmt.Errorf("entry %d: name %q must match %s", i+1, a.Name, artifactNameRE)
		}
		if seen[a.Name] {
			return nil, fmt.Errorf("entry %d: duplicate name %q", i+1, a.Name)
		}
		seen[a.Name] = true
		if a.Title == "" {
			a.Title = a.Name
		}
		out = append(out, ArtifactYAML{Path: clean, Name: a.Name, Title: a.Title})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
