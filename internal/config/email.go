package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/secrets"
)

// EmailTokenPrefix marks an email token as a secret reference. A bare token
// in config.yaml is refused; the value never belongs in the file.
const EmailTokenPrefix = "secret:"

// Poll-interval bounds for the ingress worker (plan 2026-09-19 Part F).
const (
	DefaultEmailPollInterval = 30 * time.Second
	MinEmailPollInterval     = 5 * time.Second
	MaxEmailPollInterval     = 10 * time.Minute
)

// EmailConfig is the instance-level email channel block. A present worker
// URL enables the channel; there is no enabled flag. An absent block
// disables polling, the notifier channel, and every address surface.
type EmailConfig struct {
	Worker       string   `yaml:"worker,omitempty"`
	Token        string   `yaml:"token,omitempty"` // secret:<name>, resolved at load
	Domain       string   `yaml:"domain,omitempty"`
	Operators    []string `yaml:"operators,omitempty"`
	PollInterval string   `yaml:"poll_interval,omitempty"` // empty → 30s
	RequireDMARC *bool    `yaml:"require_dmarc,omitempty"` // nil → true
	FromName     string   `yaml:"from_name,omitempty"`     // empty → Trebi
}

// Enabled reports whether the email channel is on: worker URL present.
func (e EmailConfig) Enabled() bool { return strings.TrimSpace(e.Worker) != "" }

// PollIntervalDuration parses poll_interval. Empty means the default.
func (e EmailConfig) PollIntervalDuration() (time.Duration, error) {
	s := strings.TrimSpace(e.PollInterval)
	if s == "" {
		return DefaultEmailPollInterval, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("email.poll_interval %q: %w", s, err)
	}
	return d, nil
}

// DMARCRequired reports the inbound DMARC policy. Unset means required.
func (e EmailConfig) DMARCRequired() bool { return e.RequireDMARC == nil || *e.RequireDMARC }

// FromSenderName returns the display name for outbound mail.
func (e EmailConfig) FromSenderName() string {
	if n := strings.TrimSpace(e.FromName); n != "" {
		return n
	}
	return "Trebi"
}

// ResolveToken reads the shared Worker token from the secret store. The
// config holds only the reference, the same split as the assistant key.
func (e EmailConfig) ResolveToken() (string, error) {
	ref := strings.TrimSpace(e.Token)
	if !strings.HasPrefix(ref, EmailTokenPrefix) {
		return "", fmt.Errorf("email.token must be %s<name>", EmailTokenPrefix)
	}
	name := strings.TrimSpace(strings.TrimPrefix(ref, EmailTokenPrefix))
	if name == "" {
		return "", fmt.Errorf("email.token: empty secret name")
	}
	v, err := secrets.Get(name)
	if err != nil {
		return "", fmt.Errorf("email.token: %w", err)
	}
	return string(v), nil
}

// checkEmail validates the email block. It checks the token reference form,
// not the stored secret; a missing secret is a doctor finding, not a config
// error, so the daemon still boots.
func (e *ConfigError) checkEmail(c Config) {
	ec := c.Email
	if !ec.Enabled() {
		if emailBlockSet(ec) {
			e.add("email.worker", "required when any email key is set")
		}
		return
	}
	if u, err := url.Parse(strings.TrimSpace(ec.Worker)); err != nil || u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		e.add("email.worker", fmt.Sprintf("%q is not a parseable http(s) URL", ec.Worker))
	}
	if !strings.HasPrefix(strings.TrimSpace(ec.Token), EmailTokenPrefix) ||
		strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ec.Token), EmailTokenPrefix)) == "" {
		e.add("email.token", fmt.Sprintf("must be %s<name>", EmailTokenPrefix))
	}
	if dom := strings.TrimSpace(ec.Domain); dom == "" || strings.ContainsAny(dom, "/\\:@") {
		e.add("email.domain", fmt.Sprintf("%q is not a bare hostname", ec.Domain))
	}
	e.checkEmailOperators(ec.Operators)
	e.checkEmailPollInterval(ec)
	if c.Notify.Email != "" {
		e.add("email", "notify.email and the email block cannot both be set; the native channel takes the email slot")
	}
}

func (e *ConfigError) checkEmailOperators(operators []string) {
	if len(operators) == 0 {
		e.add("email.operators", "at least one operator address is required")
	}
	for _, op := range operators {
		if !strings.Contains(strings.TrimSpace(op), "@") {
			e.add("email.operators", fmt.Sprintf("%q is not an email address", op))
		}
	}
}

func (e *ConfigError) checkEmailPollInterval(ec EmailConfig) {
	d, err := ec.PollIntervalDuration()
	if err != nil {
		e.add("email.poll_interval", strings.TrimPrefix(err.Error(), "email."))
	} else if d < MinEmailPollInterval || d > MaxEmailPollInterval {
		e.add("email.poll_interval", fmt.Sprintf("must be between %s and %s",
			MinEmailPollInterval, MaxEmailPollInterval))
	}
}

// emailBlockSet reports whether any email key besides worker is set.
func emailBlockSet(ec EmailConfig) bool {
	return ec.Token != "" || ec.Domain != "" || len(ec.Operators) > 0 ||
		ec.PollInterval != "" || ec.RequireDMARC != nil || ec.FromName != ""
}
