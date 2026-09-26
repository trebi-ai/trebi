package assistant

import (
	"context"
	"strings"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/config"
)

// Permission modes and the native harness name.
const (
	ModeAsk       = wire.ModeAsk
	ModeAutoEdit  = wire.ModeAutoEdit
	ModeAuto      = wire.ModeAuto
	HarnessNative = wire.HarnessNative
)

// Settings, Override, Effective, Health, and WorkspaceInfo live in the wire
// leaf package so the workspaces service can use them without an import
// cycle (plan 2026-09-26 Part B).
type (
	Settings      = wire.Settings
	Override      = wire.Override
	Effective     = wire.Effective
	Health        = wire.Health
	WorkspaceInfo = wire.WorkspaceInfo
)

// OverrideStore reads and writes the workspace assistant override. It is
// implemented by *workspaces.Service.
type OverrideStore interface {
	AssistantOverride(ctx context.Context, id string) (Override, string, error)
	SetAssistantOverride(ctx context.Context, id string, o Override, ifMatch string) (string, error)
	WorkspaceInfo(ctx context.Context, id string) (WorkspaceInfo, error)
}

// ConfigSource supplies the global defaults. It is implemented over
// *config.Config by DefaultsFromConfig.
type ConfigSource interface {
	AssistantDefaults() Settings
}

// Detector probes a harness on this machine. *agentwire.Runtime implements it.
type Detector interface {
	Detect(ctx context.Context, h agentwire.Harness) (agentwire.Detection, error)
}

// DefaultsFromConfig adapts *config.Config to ConfigSource.
func DefaultsFromConfig(cfg *config.Config) ConfigSource {
	return configSourceFunc(func() Settings { return SettingsFromConfig(cfg) })
}

type configSourceFunc func() Settings

func (f configSourceFunc) AssistantDefaults() Settings { return f() }

// SettingsFromConfig renders the global defaults from config.yaml.
func SettingsFromConfig(cfg *config.Config) Settings {
	a := cfg.Assistant
	return Settings{
		Disabled: false,
		Name:     "",
		Harness:  strings.TrimSpace(a.Harness),
		Model:    a.Model,
		Effort:   strings.TrimSpace(a.Effort),
		Envset:   strings.TrimSpace(a.Envset),
		Mode:     a.ModeOrDefault(),
		Skills:   a.SkillsOrDefault(),
		Context:  a.Context,
		Accept:   []string{"operators", "allowlist"},
	}
}

// Timeout defaults for the manager timers.
const (
	DefaultIdleClose         = 15 * time.Minute
	DefaultPermissionTimeout = 10 * time.Minute
	DefaultMaxLive           = 4
)

// ResolverTiming carries the process timers the manager reads.
type ResolverTiming struct {
	IdleClose         time.Duration
	PermissionTimeout time.Duration
	MaxLive           int
}

// Resolver merges the global defaults and the workspace override.
type Resolver struct {
	cfg    ConfigSource
	ws     OverrideStore
	rt     Detector
	native config.NativeConfig
	timing ResolverTiming
}

// NewResolver builds a resolver over config defaults, the workspace override
// store, and the harness detector.
func NewResolver(cfg ConfigSource, ws OverrideStore, rt Detector, native config.NativeConfig, timing ResolverTiming) *Resolver {
	if timing.IdleClose <= 0 {
		timing.IdleClose = DefaultIdleClose
	}
	if timing.PermissionTimeout <= 0 {
		timing.PermissionTimeout = DefaultPermissionTimeout
	}
	if timing.MaxLive <= 0 {
		timing.MaxLive = DefaultMaxLive
	}
	return &Resolver{cfg: cfg, ws: ws, rt: rt, native: native, timing: timing}
}

// Timing exposes the process timers.
func (r *Resolver) Timing() ResolverTiming { return r.timing }

// Defaults returns the global settings.
func (r *Resolver) Defaults(_ context.Context) (Settings, error) {
	return r.cfg.AssistantDefaults(), nil
}

// Effective merges defaults and the workspace override and resolves the
// harness a new session would use now.
func (r *Resolver) Effective(ctx context.Context, workspaceID string) (Effective, error) {
	base, err := r.Defaults(ctx)
	if err != nil {
		return Effective{}, err
	}
	ov, _, err := r.ws.AssistantOverride(ctx, workspaceID)
	if err != nil {
		return Effective{}, err
	}
	eff := base
	eff.Disabled = false
	source := map[string]string{}
	apply := func(field string, set bool) {
		if set {
			source[field] = "workspace"
		}
	}
	if ov.Disabled != nil {
		eff.Disabled = *ov.Disabled
		apply("disabled", true)
	}
	if ov.Name != nil {
		eff.Name = *ov.Name
		apply("name", true)
	}
	if ov.Harness != nil {
		eff.Harness = *ov.Harness
		apply("harness", true)
	}
	if ov.Model != nil {
		eff.Model = *ov.Model
		apply("model", true)
	}
	if ov.Effort != nil {
		eff.Effort = *ov.Effort
		apply("effort", true)
	}
	if ov.Envset != nil {
		eff.Envset = *ov.Envset
		apply("envset", true)
	}
	if ov.Mode != nil {
		eff.Mode = *ov.Mode
		apply("mode", true)
	}
	if eff.Mode == "" {
		eff.Mode = ModeAutoEdit
	}
	if ov.Skills != nil {
		eff.Skills = append([]string(nil), *ov.Skills...)
		apply("skills", true)
	}
	if ov.Context != nil {
		eff.Context = *ov.Context
		apply("context", true)
	}
	if ov.Email != nil {
		eff.Accept = ov.Email.Accept
		apply("email_accept", true)
	}
	if len(ov.Triggers) > 0 {
		eff.Triggers = ov.Triggers
		apply("triggers", true)
	}
	if eff.Name == "" {
		info, werr := r.ws.WorkspaceInfo(ctx, workspaceID)
		if werr != nil {
			return Effective{}, werr
		}
		eff.Name = info.Name
	}
	for _, f := range []string{"disabled", "name", "harness", "model", "effort", "envset", "mode", "skills", "context", "email_accept", "triggers"} {
		if source[f] == "" {
			source[f] = "default"
		}
	}
	resolved, health := r.resolveHarness(ctx, eff)
	return Effective{Settings: eff, Source: source, Resolved: resolved, Health: health}, nil
}

// resolve picks the harness a new session uses now: the effective harness,
// or the first supported CLI harness, then native when it has model and key.
func (r *Resolver) resolve(ctx context.Context, eff Settings) string {
	h := strings.TrimSpace(eff.Harness)
	if h != "" {
		return h
	}
	for _, k := range agentwire.Harnesses() {
		if k == agentwire.Fake {
			continue
		}
		d, err := r.rt.Detect(ctx, k)
		if err == nil && d.Supported {
			return string(k)
		}
	}
	if strings.TrimSpace(r.native.Model) != "" {
		if _, _, err := r.native.ResolveKey(); err == nil {
			return HarnessNative
		}
	}
	return ""
}

func (r *Resolver) resolveHarness(ctx context.Context, eff Settings) (string, Health) {
	resolved := r.resolve(ctx, eff)
	if resolved == "" {
		return "", Health{Available: false, Auth: "unknown", Detail: "no supported harness and no native model/key"}
	}
	if resolved == HarnessNative {
		_, _, keyErr := r.native.ResolveKey()
		model := strings.TrimSpace(r.native.Model)
		ok := model != "" && keyErr == nil
		auth := "ok"
		detail := ""
		if model == "" {
			auth = "missing"
			detail = "assistant.native.model is not set"
		} else if keyErr != nil {
			auth = "missing"
			detail = "no API key: set one in Settings or assistant.native.api_key_env"
		}
		return resolved, Health{Available: ok, Auth: auth, Detail: detail}
	}
	d, err := r.rt.Detect(ctx, agentwire.Harness(resolved))
	if err != nil {
		return resolved, Health{Available: false, Auth: "unknown", Detail: err.Error()}
	}
	detail := d.Detail
	if detail == "" && d.Version != "" {
		detail = "Uses " + resolved + " " + firstToken(d.Version)
	}
	return resolved, Health{Available: d.Supported, Auth: string(d.Auth), Detail: detail}
}

func firstToken(s string) string {
	if i := strings.IndexAny(s, " \t"); i > 0 {
		return s[:i]
	}
	return s
}

// HarnessOption is one entry of the harness picker (plan F.1).
type HarnessOption struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Available bool   `json:"available"`
	Auth      string `json:"auth"` // ok|missing|unknown
	Detail    string `json:"detail,omitempty"`
	Version   string `json:"version,omitempty"`
	Native    bool   `json:"native"`
}

// HarnessOptions probes native and every agent-wire harness except the
// fake test driver. Native comes first (plan F.1).
func (r *Resolver) HarnessOptions(ctx context.Context) []HarnessOption {
	out := []HarnessOption{{
		Name: HarnessNative, Label: "Native", Native: true,
		Auth: "unknown",
	}}
	model := strings.TrimSpace(r.native.Model)
	keyErr := error(nil)
	if model != "" {
		_, _, keyErr = r.native.ResolveKey()
	}
	out[0].Available = model != "" && keyErr == nil
	switch {
	case model == "":
		out[0].Auth = "missing"
		out[0].Detail = "assistant.native.model is not set"
	case keyErr != nil:
		out[0].Auth = "missing"
		out[0].Detail = "no API key: set one in Settings or assistant.native.api_key_env"
	default:
		out[0].Auth = "ok"
		out[0].Version = model
	}
	for _, k := range agentwire.Harnesses() {
		if k == agentwire.Fake {
			continue
		}
		opt := HarnessOption{Name: string(k), Label: harnessLabel(string(k)), Auth: "unknown"}
		if d, err := r.rt.Detect(ctx, k); err == nil {
			opt.Available = d.Supported
			opt.Auth = string(d.Auth)
			opt.Detail = d.Detail
			opt.Version = d.Version
		}
		out = append(out, opt)
	}
	return out
}

// harnessLabel renders the picker label of one harness name.
func harnessLabel(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
