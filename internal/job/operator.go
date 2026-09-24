package job

import (
	"fmt"
	"net/mail"
	"strings"
)

// EmailYAML is the inbound-mail gate shared by agents and the workspace
// assistant (2026-09-24 plan B.1/B.2).
type EmailYAML struct {
	Accept []string `yaml:"accept"`
}

// KitYAML is an agent's reusable run configuration. Owned jobs may inherit
// it; job-level values take precedence (plan A.10 and Part J).
type KitYAML struct {
	Envset string            `yaml:"envset,omitempty"`
	Env    map[string]string `yaml:"env,omitempty"`
	Skills []string          `yaml:"skills,omitempty"`
	MCP    []string          `yaml:"mcp,omitempty"`
}

// BudgetYAML is the optional monthly spend guardrail.
type BudgetYAML struct {
	MonthlyUSD float64 `yaml:"monthly_usd,omitempty"`
	OnExceed   string  `yaml:"on_exceed,omitempty"` // pause | alert; empty is pause
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

// DefaultAssistantAccept preserves the behavior of a TREBI.md without an
// email block. Agents do not get this default: an absent list is closed.
var DefaultAssistantAccept = []AcceptEntry{{Keyword: "operators"}, {Keyword: "allowlist"}}

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
		case "operators":
			out = append(out, AcceptEntry{Keyword: "operators"})
			continue
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
			return nil, fmt.Errorf("email.accept[%d]: want operators, allowlist, an address, or @domain", i)
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
	if kind == "assistant" && (fm.Kit != nil || fm.Budget != nil || fm.Report != nil) {
		return nil, nil, false, fmt.Errorf("kit, budget, and report are agent keys and are not allowed in TREBI.md")
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
		if kind == "assistant" {
			return DefaultAssistantAccept, nil, false, nil
		}
		return nil, nil, false, nil
	}
	accept, err = ParseAccept(fm.Email.Accept)
	if err != nil {
		return nil, fm.Email.Accept, true, err
	}
	return accept, fm.Email.Accept, true, nil
}
