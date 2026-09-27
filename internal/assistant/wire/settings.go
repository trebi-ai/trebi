package wire

import (
	"context"
	"fmt"
	"strings"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/job"
)

// Permission modes.
const (
	ModeAsk      = "ask"
	ModeAutoEdit = "auto_edit"
	ModeAuto     = "auto"
)

// HarnessNative names the in-process driver that needs no installed CLI.
const HarnessNative = "native"

// SkillLister names the known skills for Validate.
type SkillLister interface {
	SkillNames(ctx context.Context) ([]string, error)
}

// Validate checks mode, harness name, effort, and that each skill exists.
func (s Settings) Validate(ctx context.Context, known SkillLister) error {
	switch strings.TrimSpace(s.Mode) {
	case "", ModeAsk, ModeAutoEdit, ModeAuto:
	default:
		return fmt.Errorf("assistant.mode: %q not in [ask, auto_edit, auto]", s.Mode)
	}
	switch strings.TrimSpace(s.Effort) {
	case "", "low", "medium", "high":
	default:
		return fmt.Errorf("assistant.effort: %q not in [low, medium, high]", s.Effort)
	}
	if h := strings.TrimSpace(s.Harness); h != "" && !KnownHarness(h) {
		return fmt.Errorf("assistant.harness: %q is not a known harness", s.Harness)
	}
	if known != nil {
		names, err := known.SkillNames(ctx)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, n := range names {
			have[n] = true
		}
		for _, sk := range s.Skills {
			if !have[sk] {
				return fmt.Errorf("assistant.skills: %q is not a known skill", sk)
			}
		}
	}
	return nil
}

// KnownHarness reports whether h names native or an agent-wire harness.
func KnownHarness(h string) bool {
	if h == HarnessNative {
		return true
	}
	for _, k := range agentwire.Harnesses() {
		if string(k) == h {
			return true
		}
	}
	return false
}

// DefaultAccept preserves the behavior of an absent email block: operators
// and the allowlist may write.
var DefaultAccept = []string{"operators", "allowlist"}

// Settings are the effective assistant settings for one workspace (plan
// 2026-09-26 Part B). Both the assistant manager and the workspace
// trebi.yaml loader use them.
type Settings struct {
	Disabled bool          `json:"disabled"`
	Name     string        `json:"name"`
	Harness  string        `json:"harness"`
	Model    string        `json:"model"`
	Effort   string        `json:"effort"`
	Envset   string        `json:"envset"`
	Mode     string        `json:"mode"`
	Skills   []string      `json:"skills"`
	Context  string        `json:"context"`
	Accept   []string      `json:"email_accept"`
	Triggers []job.Trigger `json:"triggers,omitempty"`
	// Native is the provider block of the Defaults scope only. A workspace
	// view never fills it and a workspace PUT must reject it
	// (plan 2026-09-26 G).
	Native *NativeSettings `json:"native,omitempty"`
}

// NativeSettings is the native provider configuration. The model lives in
// the block: once native is configured it survives harness switches, and
// an empty model falls back to Settings.Model. APIKey is write-only: the
// API accepts it and never returns it. APIKeyPresent and APIKeySource are
// read-only (plan 2026-09-26 G).
type NativeSettings struct {
	Model         string            `json:"model,omitempty"`          // empty inherits Settings.Model
	APIFormat     string            `json:"api_format"`               // anthropic | openai
	BaseURL       string            `json:"base_url,omitempty"`       //
	APIKey        string            `json:"api_key,omitempty"`        // write-only; never returned
	APIKeyEnv     string            `json:"api_key_env,omitempty"`    // read-only on /v1; set in config.yaml
	APIKeyPresent bool              `json:"api_key_present"`          // read-only
	APIKeySource  string            `json:"api_key_source"`           // read-only: secret | env:<NAME> | none
	Headers       map[string]string `json:"headers,omitempty"`        //
	WebSearch     *bool             `json:"web_search,omitempty"`     // nil keeps the daemon default (true)
	MaxTokens     int               `json:"max_tokens,omitempty"`     //
	ContextTokens int               `json:"context_tokens,omitempty"` //
}

// NativeTestRequest is the body of POST /v1/assistant/native/test: the
// unsaved native form values (plan 2026-09-26 B).
type NativeTestRequest struct {
	Model  string         `json:"model"`
	Native NativeSettings `json:"native"`
}

// NativeTestResult is one provider test. Error is verbatim provider text.
// The key value never appears in any field.
type NativeTestResult struct {
	OK           bool   `json:"ok"`
	Model        string `json:"model"`
	LatencyMS    int64  `json:"latency_ms"`
	Reply        string `json:"reply,omitempty"`
	KeySource    string `json:"key_source"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	Code         string `json:"code,omitempty"` // no_model | no_key | timeout | provider
	Error        string `json:"error,omitempty"`
}

// Override is the trebi.yaml assistant block. A nil field means "inherit"
// the global default. For Skills an empty list means "no skills".
type Override struct {
	Disabled *bool     `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	Name     *string   `yaml:"name,omitempty" json:"name,omitempty"`
	Harness  *string   `yaml:"harness,omitempty" json:"harness,omitempty"`
	Model    *string   `yaml:"model,omitempty" json:"model,omitempty"`
	Effort   *string   `yaml:"effort,omitempty" json:"effort,omitempty"`
	Envset   *string   `yaml:"envset,omitempty" json:"envset,omitempty"`
	Mode     *string   `yaml:"mode,omitempty" json:"mode,omitempty"`
	Skills   *[]string `yaml:"skills,omitempty" json:"skills,omitempty"`
	Context  *string   `yaml:"context,omitempty" json:"context,omitempty"`
	Email    *struct {
		Accept []string `yaml:"accept" json:"accept"`
	} `yaml:"email,omitempty" json:"email,omitempty"`
	Triggers []job.Trigger `yaml:"triggers,omitempty" json:"triggers,omitempty"`
}

// Effective is Settings plus where each field came from.
type Effective struct {
	Settings
	Source   map[string]string `json:"source"`           // field -> "default"|"workspace"
	Resolved string            `json:"resolved_harness"` // the harness a new session uses now
	Health   Health            `json:"health"`
}

// Health is the resolved harness's availability on this machine.
type Health struct {
	Available bool   `json:"available"`
	Auth      string `json:"auth"` // ok|missing|unknown
	Detail    string `json:"detail,omitempty"`
}

// WorkspaceInfo is the workspace identity the composer and the launcher need.
type WorkspaceInfo struct {
	ID   string
	Name string
	Path string
}
