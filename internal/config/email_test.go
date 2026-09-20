package config

import (
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/secrets"
)

func TestEmailValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mod  func(*EmailConfig)
		want string
	}{
		{"valid", func(*EmailConfig) {}, ""},
		{"missing worker", func(e *EmailConfig) { e.Worker = "" }, "email.worker"},
		{"bad worker url", func(e *EmailConfig) { e.Worker = "mail.example" }, "email.worker"},
		{"bad token form", func(e *EmailConfig) { e.Token = "plain-value" }, "email.token"},
		{"empty secret name", func(e *EmailConfig) { e.Token = "secret:" }, "email.token"},
		{"missing domain", func(e *EmailConfig) { e.Domain = "" }, "email.domain"},
		{"domain with scheme", func(e *EmailConfig) { e.Domain = "https://mail.example.com" }, "email.domain"},
		{"no operators", func(e *EmailConfig) { e.Operators = nil }, "email.operators"},
		{"operator not address", func(e *EmailConfig) { e.Operators = []string{"ops"} }, "email.operators"},
		{"poll too fast", func(e *EmailConfig) { e.PollInterval = "1s" }, "email.poll_interval"},
		{"poll too slow", func(e *EmailConfig) { e.PollInterval = "11m" }, "email.poll_interval"},
		{"poll unparsable", func(e *EmailConfig) { e.PollInterval = "fast" }, "email.poll_interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := defaultConfig()
			cfg.applyDefaults()
			cfg.Email = validEmailBlock()
			tc.mod(&cfg.Email)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestEmailAbsentBlockValid(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("absent block must validate: %v", err)
	}
	if cfg.Email.Enabled() {
		t.Fatal("empty block must be disabled")
	}
}

func TestEmailPartialBlockRejected(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.applyDefaults()
	cfg.Email = EmailConfig{Domain: "mail.example.com"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "email.worker") {
		t.Fatalf("want email.worker error, got %v", err)
	}
}

func TestEmailNotifyConflict(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.applyDefaults()
	cfg.Email = validEmailBlock()
	cfg.Notify.Email = "echo notify"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "cannot both be set") {
		t.Fatalf("want conflict error, got %v", err)
	}
	// The shell template alone stays valid: legacy notify chain without the block.
	cfg.Email = EmailConfig{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("notify.email without the block must validate: %v", err)
	}
}

func TestEmailDefaults(t *testing.T) {
	t.Parallel()
	e := EmailConfig{}
	if e.Enabled() {
		t.Fatal("empty block disabled")
	}
	d, err := e.PollIntervalDuration()
	if err != nil || d != DefaultEmailPollInterval {
		t.Fatalf("poll default: %v, %v", d, err)
	}
	if !e.DMARCRequired() {
		t.Fatal("DMARC required by default")
	}
	if e.FromSenderName() != "Trebi" {
		t.Fatalf("from name: %q", e.FromSenderName())
	}
}

func TestEmailResolveToken(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })

	e := validEmailBlock()
	if _, err := e.ResolveToken(); err == nil {
		t.Fatal("missing secret must fail")
	}
	if err := secrets.Set("trebi_mail", "test-token-0000"); err != nil {
		t.Fatal(err)
	}
	v, err := e.ResolveToken()
	if err != nil || v != "test-token-0000" {
		t.Fatalf("resolve: %q, %v", v, err)
	}
}

func TestEmailSetPathAndShow(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.applyDefaults()
	set := func(k, v string) {
		t.Helper()
		if err := cfg.SetPath(k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	set("email.worker", "https://trebi-mail.example.workers.dev")
	set("email.token", "secret:trebi_mail")
	set("email.domain", "mail.example.com")
	set("email.operators", "a@example.com,b@example.com")
	set("email.poll_interval", "45s")
	set("email.require_dmarc", "false")
	set("email.from_name", "Ops")
	if !cfg.Email.Enabled() {
		t.Fatal("block must be enabled")
	}
	if d, err := cfg.Email.PollIntervalDuration(); err != nil || d != 45*time.Second {
		t.Fatalf("poll: %v, %v", d, err)
	}
	if cfg.Email.DMARCRequired() {
		t.Fatal("require_dmarc false must stick")
	}
	if cfg.Email.FromSenderName() != "Ops" {
		t.Fatalf("from name: %q", cfg.Email.FromSenderName())
	}
	by := map[string]PathEntry{}
	for _, e := range cfg.PathEntries(nil) {
		by[e.Key] = e
	}
	if by["email.worker"].Value != "https://trebi-mail.example.workers.dev" {
		t.Fatalf("show worker: %+v", by["email.worker"])
	}
	if by["email.operators"].Value != "a@example.com,b@example.com" {
		t.Fatalf("show operators: %+v", by["email.operators"])
	}
	if by["email.require_dmarc"].Value != "false" {
		t.Fatalf("show dmarc: %+v", by["email.require_dmarc"])
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func validEmailBlock() EmailConfig {
	return EmailConfig{
		Worker:    "https://trebi-mail.example.workers.dev",
		Token:     "secret:trebi_mail",
		Domain:    "mail.example.com",
		Operators: []string{"ops@example.com"},
	}
}
