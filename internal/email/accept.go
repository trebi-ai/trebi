package email

import (
	"context"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
)

// AcceptRule is one operator's resolved sender gate (2026-09-24 plan D.1).
// Zero value is closed.
type AcceptRule struct {
	admins    bool
	allowlist bool
	addrs     []string
	domains   []string
}

// NewAcceptRule compiles the shared frontmatter grammar into a matcher.
func NewAcceptRule(entries []job.AcceptEntry) AcceptRule {
	var r AcceptRule
	for _, entry := range entries {
		switch {
		case entry.Keyword == "admins":
			r.admins = true
		case entry.Keyword == "allowlist":
			r.allowlist = true
		case entry.Address != "":
			r.addrs = append(r.addrs, entry.Address)
		case entry.Domain != "":
			r.domains = append(r.domains, entry.Domain)
		}
	}
	return r
}

// NewAgentAcceptRule is the gate of an agent tag. Admins always pass; the
// list adds senders.
func NewAgentAcceptRule(entries []job.AcceptEntry) AcceptRule {
	r := NewAcceptRule(entries)
	r.admins = true
	return r
}

// Empty reports a closed rule: no entry at all. An explicitly empty
// email.accept list and a missing agent list are both closed.
func (r AcceptRule) Empty() bool {
	return !r.admins && !r.allowlist && len(r.addrs) == 0 && len(r.domains) == 0
}

// Accepts matches one From address. Admins and allowlist expand to their
// configured address lists at call time so config changes need no reparse.
func (r AcceptRule) Accepts(from string, admins, allowlist []string) bool {
	from = strings.ToLower(strings.TrimSpace(from))
	if from == "" {
		return false
	}
	if r.admins && addrListed(admins, from) {
		return true
	}
	if r.allowlist && addrListed(allowlist, from) {
		return true
	}
	if len(r.addrs) > 0 && addrListed(r.addrs, from) {
		return true
	}
	_, domain, found := strings.Cut(from, "@")
	return found && domain != "" && len(r.domains) > 0 && slicesContains(r.domains, domain)
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ResolveRecipients expands report.to against the configured admins and the
// workspace allowlist. It returns the concrete addresses only.
func (s *Service) ResolveRecipients(ctx context.Context, workspaceID string, entries []string) ([]string, error) {
	parsed, err := job.ParseAccept(entries)
	if err != nil {
		return nil, err
	}
	rule := NewAcceptRule(parsed)
	if rule.Empty() {
		return nil, nil
	}
	cfg, err := s.Settings()
	if err != nil {
		return nil, err
	}
	var allowlist []string
	if s.ingress != nil {
		if env, err := s.ingress.act.RouteEnvFor(ctx, workspaceID); err == nil {
			allowlist = env.Allowlist
		}
	}
	out := make([]string, 0)
	seen := map[string]bool{}
	for _, group := range [][]string{cfg.Admins, allowlist} {
		for _, addr := range group {
			addr = strings.TrimSpace(addr)
			if addr == "" || seen[strings.ToLower(addr)] || !rule.Accepts(addr, cfg.Admins, allowlist) {
				continue
			}
			seen[strings.ToLower(addr)] = true
			out = append(out, addr)
		}
	}
	return out, nil
}
