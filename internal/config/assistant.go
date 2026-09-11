package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/trebi-ai/trebi/internal/secrets"
)

const (
	// AssistantKeySecret is the secrets-store name for assistant.api_key.
	AssistantKeySecret = "assistant.api_key"

	DefaultAssistantMaxTokens = 8192
	defaultOpenAIBase         = "https://api.openai.com"
	defaultAnthropicBase      = "https://api.anthropic.com"

	KeySourceNone      = "none"
	KeySourceSecret    = "secret"
	keySourceEnvPrefix = "env:"
	keySourceWellKnown = "well-known:"
)

// ErrNoAssistantKey means no API key resolved for the native assistant.
var ErrNoAssistantKey = errors.New("assistant api key not set")

// AssistantConfig is the daemon-global native assistant block.
type AssistantConfig struct {
	Model         string            `yaml:"model,omitempty"`
	APIFormat     string            `yaml:"api_format,omitempty"`
	BaseURL       string            `yaml:"base_url,omitempty"`
	APIKey        string            `yaml:"api_key,omitempty"`
	APIKeyEnv     string            `yaml:"api_key_env,omitempty"`
	APIKeyPresent bool              `yaml:"api_key_present,omitempty"`
	MaxTokens     int               `yaml:"max_tokens,omitempty"`
	ContextTokens int               `yaml:"context_tokens,omitempty"`
	Headers       map[string]string `yaml:"headers,omitempty"`
	SystemExtra   string            `yaml:"system_extra,omitempty"`
	WebSearch     *bool             `yaml:"web_search,omitempty"`
	Limits        AssistantLimits   `yaml:"limits,omitempty"`

	pendingKey  string `yaml:"-"`
	clearSecret bool   `yaml:"-"`
}

// AssistantLimits are phase-2/4 caps. Zero means unlimited / use the loop default.
type AssistantLimits struct {
	DailyTokens int `yaml:"daily_tokens,omitempty"`
	MaxTurns    int `yaml:"max_turns,omitempty"`
}

// Format is openai or anthropic. Empty YAML means openai.
func (a AssistantConfig) Format() string {
	if strings.EqualFold(strings.TrimSpace(a.APIFormat), "anthropic") {
		return "anthropic"
	}
	return "openai"
}

// DefaultBaseURL is the format's origin (no /v1).
func (a AssistantConfig) DefaultBaseURL() string {
	if a.Format() == "anthropic" {
		return defaultAnthropicBase
	}
	return defaultOpenAIBase
}

// MaxTokensOrDefault is 8192 when max_tokens is omitted.
func (a AssistantConfig) MaxTokensOrDefault() int {
	if a.MaxTokens <= 0 {
		return DefaultAssistantMaxTokens
	}
	return a.MaxTokens
}

// WebSearchEnabled defaults true.
func (a AssistantConfig) WebSearchEnabled() bool {
	if a.WebSearch == nil {
		return true
	}
	return *a.WebSearch
}

// Enabled reports model set and a resolvable key.
func (a AssistantConfig) Enabled() bool {
	if strings.TrimSpace(a.Model) == "" {
		return false
	}
	_, _, err := a.ResolveKey()
	return err == nil
}

// ResolveKey returns the API key and a source name that never includes the value.
// Precedence: stored secret > api_key_env > well-known env for the format.
func (a AssistantConfig) ResolveKey() (key, source string, err error) {
	if k := strings.TrimSpace(a.pendingKey); k != "" {
		return k, KeySourceSecret, nil
	}
	if a.APIKeyPresent {
		sec, gerr := secrets.Get(AssistantKeySecret)
		if gerr == nil && strings.TrimSpace(string(sec)) != "" {
			return string(sec), KeySourceSecret, nil
		}
	}
	if env := strings.TrimSpace(a.APIKeyEnv); env != "" {
		if v := os.Getenv(env); v != "" {
			return v, keySourceEnvPrefix + env, nil
		}
		return "", KeySourceNone, fmt.Errorf("%w: %s empty", ErrNoAssistantKey, env)
	}
	well := a.wellKnownEnv()
	if v := os.Getenv(well); v != "" {
		return v, keySourceWellKnown + well, nil
	}
	return "", KeySourceNone, ErrNoAssistantKey
}

// KeySourceName is the live source label, never the key.
func (a AssistantConfig) KeySourceName() string {
	_, src, _ := a.ResolveKey()
	if src == "" {
		return KeySourceNone
	}
	return src
}

func (a AssistantConfig) wellKnownEnv() string {
	if a.Format() == "anthropic" {
		return "ANTHROPIC_API_KEY"
	}
	return "OPENAI_API_KEY"
}

// Route picks the assistant runtime. jobRuntime is TREBI.md runtime:.
func (a AssistantConfig) Route(jobRuntime string) (runtime, reason, auth string) {
	if strings.EqualFold(strings.TrimSpace(jobRuntime), "tui") {
		return "tui", "TREBI.md runtime: tui", a.authStatus()
	}
	if strings.TrimSpace(a.Model) == "" {
		return "tui", "native off: no model", "off"
	}
	if !a.Enabled() {
		return "native", "native off: no key", "missing"
	}
	return "native", "config: assistant.model", "ok"
}

func (a AssistantConfig) authStatus() string {
	if strings.TrimSpace(a.Model) == "" {
		return "off"
	}
	if a.Enabled() {
		return "ok"
	}
	return "missing"
}

func (a *AssistantConfig) flushKey() error {
	if a == nil {
		return nil
	}
	if a.clearSecret {
		if err := secrets.Delete(AssistantKeySecret); err != nil {
			return err
		}
		a.APIKeyPresent = false
		a.clearSecret = false
	}
	if k := strings.TrimSpace(a.pendingKey); k != "" {
		if err := secrets.Set(AssistantKeySecret, k); err != nil {
			return err
		}
		a.pendingKey = ""
		a.APIKeyPresent = true
	}
	a.APIKey = ""
	return nil
}

func (e *ConfigError) checkAssistant(a AssistantConfig) {
	f := strings.ToLower(strings.TrimSpace(a.APIFormat))
	if f != "" && f != "openai" && f != "anthropic" {
		e.add("assistant.api_format", fmt.Sprintf("%q not in [openai, anthropic]", a.APIFormat))
	}
	if a.MaxTokens < 0 {
		e.add("assistant.max_tokens", "must be > 0 when set")
	}
	if a.ContextTokens < 0 {
		e.add("assistant.context_tokens", "must be >= 0")
	}
	if a.Limits.DailyTokens < 0 {
		e.add("assistant.limits.daily_tokens", "must be >= 0")
	}
	if a.Limits.MaxTurns < 0 {
		e.add("assistant.limits.max_turns", "must be >= 0")
	}
}
