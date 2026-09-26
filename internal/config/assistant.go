package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/secrets"
)

const (
	// AssistantKeySecret is the secrets-store name for assistant.api_key.
	AssistantKeySecret = "assistant.api_key"

	DefaultAssistantMaxTokens = 8192
	defaultOpenAIBase         = "https://api.openai.com"
	defaultAnthropicBase      = "https://api.anthropic.com"

	// Assistant session defaults (plan 2026-09-26 Part B).
	DefaultAssistantMode              = "auto_edit"
	DefaultAssistantIdleClose         = 15 * time.Minute
	DefaultAssistantPermissionTimeout = 10 * time.Minute
	DefaultAssistantMaxLive           = 4

	KeySourceNone      = "none"
	KeySourceSecret    = "secret"
	keySourceEnvPrefix = "env:"
	keySourceWellKnown = "well-known:"
)

// ErrNoAssistantKey means no API key resolved for the native assistant.
var ErrNoAssistantKey = errors.New("assistant api key not set")

// NativeConfig is the assistant.native block: the provider of the in-process
// loop, the one harness that needs no installed CLI.
type NativeConfig struct {
	APIFormat     string            `yaml:"api_format,omitempty"`
	Model         string            `yaml:"model,omitempty"`
	BaseURL       string            `yaml:"base_url,omitempty"`
	APIKeyEnv     string            `yaml:"api_key_env,omitempty"`
	APIKeyPresent bool              `yaml:"api_key_present,omitempty"`
	MaxTokens     int               `yaml:"max_tokens,omitempty"`
	ContextTokens int               `yaml:"context_tokens,omitempty"`
	Headers       map[string]string `yaml:"headers,omitempty"`
	WebSearch     *bool             `yaml:"web_search,omitempty"`
	Limits        AssistantLimits   `yaml:"limits,omitempty"`
}

// AssistantLimits are caps. Zero means unlimited / use the loop default.
type AssistantLimits struct {
	DailyTokens int `yaml:"daily_tokens,omitempty"`
	MaxTurns    int `yaml:"max_turns,omitempty"`
}

// AssistantConfig is the daemon-global assistant settings (plan 2026-09-26
// Part B). A workspace trebi.yaml block overrides each field when set.
type AssistantConfig struct {
	Harness           string       `yaml:"harness,omitempty"`            // "" = auto; native or an agent-wire harness
	Model             string       `yaml:"model,omitempty"`              // passed to the harness
	Effort            string       `yaml:"effort,omitempty"`             // low|medium|high
	Envset            string       `yaml:"envset,omitempty"`             // env set for CLI harnesses
	Mode              string       `yaml:"mode,omitempty"`               // ask|auto_edit|auto; "" = auto_edit
	Skills            []string     `yaml:"skills,omitempty"`             // skill names under ~/.trebi/skills
	Context           string       `yaml:"context,omitempty"`            // default context prompt
	IdleClose         string       `yaml:"idle_close,omitempty"`         // duration; default 15m
	PermissionTimeout string       `yaml:"permission_timeout,omitempty"` // duration; default 10m
	MaxLive           int          `yaml:"max_live,omitempty"`           // live agent processes; default 4
	APIKeyPresent     bool         `yaml:"api_key_present,omitempty"`
	Native            NativeConfig `yaml:"native,omitempty"`

	// APIKey is a legacy flat key. Load drains it into the secrets store;
	// Save never writes it.
	APIKey string `yaml:"api_key,omitempty"`

	pendingKey  string `yaml:"-"`
	clearSecret bool   `yaml:"-"`
}

// Format is openai or anthropic. Empty YAML means openai.
func (n NativeConfig) Format() string {
	if strings.EqualFold(strings.TrimSpace(n.APIFormat), "anthropic") {
		return "anthropic"
	}
	return "openai"
}

// DefaultBaseURL is the format's origin (no /v1).
func (n NativeConfig) DefaultBaseURL() string {
	if n.Format() == "anthropic" {
		return defaultAnthropicBase
	}
	return defaultOpenAIBase
}

// MaxTokensOrDefault is 8192 when max_tokens is omitted.
func (n NativeConfig) MaxTokensOrDefault() int {
	if n.MaxTokens <= 0 {
		return DefaultAssistantMaxTokens
	}
	return n.MaxTokens
}

// WebSearchEnabled defaults true.
func (n NativeConfig) WebSearchEnabled() bool {
	if n.WebSearch == nil {
		return true
	}
	return *n.WebSearch
}

// Enabled reports model set and a resolvable key.
func (n NativeConfig) Enabled() bool {
	if strings.TrimSpace(n.Model) == "" {
		return false
	}
	_, _, err := n.ResolveKey()
	return err == nil
}

// ResolveKey returns the API key and a source name that never includes the value.
// Precedence: stored secret > api_key_env > well-known env for the format.
func (n NativeConfig) ResolveKey() (key, source string, err error) {
	if n.APIKeyPresent {
		sec, gerr := secrets.Get(AssistantKeySecret)
		if gerr == nil && strings.TrimSpace(string(sec)) != "" {
			return string(sec), KeySourceSecret, nil
		}
	}
	if env := strings.TrimSpace(n.APIKeyEnv); env != "" {
		if v := os.Getenv(env); v != "" {
			return v, keySourceEnvPrefix + env, nil
		}
		return "", KeySourceNone, fmt.Errorf("%w: %s empty", ErrNoAssistantKey, env)
	}
	well := n.wellKnownEnv()
	if v := os.Getenv(well); v != "" {
		return v, keySourceWellKnown + well, nil
	}
	return "", KeySourceNone, ErrNoAssistantKey
}

// KeySourceName is the live source label, never the key.
func (n NativeConfig) KeySourceName() string {
	_, src, _ := n.ResolveKey()
	if src == "" {
		return KeySourceNone
	}
	return src
}

func (n NativeConfig) wellKnownEnv() string {
	if n.Format() == "anthropic" {
		return "ANTHROPIC_API_KEY"
	}
	return "OPENAI_API_KEY"
}

// ModeOrDefault is auto_edit when unset.
func (a AssistantConfig) ModeOrDefault() string {
	if m := strings.TrimSpace(a.Mode); m != "" {
		return m
	}
	return DefaultAssistantMode
}

// IdleCloseDuration is how long an idle session keeps its process.
func (a AssistantConfig) IdleCloseDuration() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(a.IdleClose)); err == nil && d > 0 {
		return d
	}
	return DefaultAssistantIdleClose
}

// PermissionTimeoutDuration is how long a headless permission waits.
func (a AssistantConfig) PermissionTimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(a.PermissionTimeout)); err == nil && d > 0 {
		return d
	}
	return DefaultAssistantPermissionTimeout
}

// MaxLiveOrDefault is the live-process cap across workspaces.
func (a AssistantConfig) MaxLiveOrDefault() int {
	if a.MaxLive > 0 {
		return a.MaxLive
	}
	return DefaultAssistantMaxLive
}

// SkillsOrDefault is [trebi] when unset. An empty list means no skills.
func (a AssistantConfig) SkillsOrDefault() []string {
	if a.Skills != nil {
		return append([]string(nil), a.Skills...)
	}
	return []string{"trebi"}
}

// SetPendingKey stages an assistant API key. The next Save writes it into
// the secrets store and never into YAML (plan 2026-09-26 G).
func (a *AssistantConfig) SetPendingKey(key string) {
	if a == nil {
		return
	}
	a.pendingKey = strings.TrimSpace(key)
	a.clearSecret = false
}

// ClearKey stages the removal of the stored assistant API key. The next
// Save deletes the secret.
func (a *AssistantConfig) ClearKey() {
	if a == nil {
		return
	}
	a.clearSecret = true
	a.pendingKey = ""
}

// flushKey moves a pending key into the secrets store and clears secret
// state. It runs inside Save, before the YAML marshal.
func (a *AssistantConfig) flushKey() error {
	if a == nil {
		return nil
	}
	if a.clearSecret {
		if err := secrets.Delete(AssistantKeySecret); err != nil {
			return err
		}
		a.APIKeyPresent = false
		a.Native.APIKeyPresent = false
		a.clearSecret = false
	}
	if k := strings.TrimSpace(a.pendingKey); k != "" {
		if err := secrets.Set(AssistantKeySecret, k); err != nil {
			return err
		}
		a.pendingKey = ""
		a.APIKeyPresent = true
		a.Native.APIKeyPresent = true
	}
	return nil
}

func (e *ConfigError) checkAssistant(a AssistantConfig) {
	if m := strings.TrimSpace(a.Mode); m != "" && m != "ask" && m != "auto_edit" && m != "auto" {
		e.add("assistant.mode", fmt.Sprintf("%q not in [ask, auto_edit, auto]", a.Mode))
	}
	if h := strings.TrimSpace(a.Harness); h != "" && !knownHarness(h) {
		e.add("assistant.harness", fmt.Sprintf("%q is not a known harness", h))
	}
	switch strings.TrimSpace(a.Effort) {
	case "", "low", "medium", "high":
	default:
		e.add("assistant.effort", fmt.Sprintf("%q not in [low, medium, high]", a.Effort))
	}
	if _, err := time.ParseDuration(strings.TrimSpace(a.IdleClose)); a.IdleClose != "" && err != nil {
		e.add("assistant.idle_close", "want a duration, for example 15m")
	}
	if _, err := time.ParseDuration(strings.TrimSpace(a.PermissionTimeout)); a.PermissionTimeout != "" && err != nil {
		e.add("assistant.permission_timeout", "want a duration, for example 10m")
	}
	if a.MaxLive < 0 {
		e.add("assistant.max_live", "must be >= 0")
	}
	n := a.Native
	f := strings.ToLower(strings.TrimSpace(n.APIFormat))
	if f != "" && f != "openai" && f != "anthropic" {
		e.add("assistant.native.api_format", fmt.Sprintf("%q not in [openai, anthropic]", n.APIFormat))
	}
	if n.MaxTokens < 0 {
		e.add("assistant.native.max_tokens", "must be > 0 when set")
	}
	if n.ContextTokens < 0 {
		e.add("assistant.native.context_tokens", "must be >= 0")
	}
	if n.Limits.DailyTokens < 0 {
		e.add("assistant.native.limits.daily_tokens", "must be >= 0")
	}
	if n.Limits.MaxTurns < 0 {
		e.add("assistant.native.limits.max_turns", "must be >= 0")
	}
}

// knownHarness reports whether h names native or an agent-wire harness.
func knownHarness(h string) bool {
	if h == "native" {
		return true
	}
	for _, k := range agentwire.Harnesses() {
		if string(k) == h {
			return true
		}
	}
	return false
}
