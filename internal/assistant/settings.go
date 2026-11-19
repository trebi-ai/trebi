package assistant

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
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
	Settings       = wire.Settings
	Override       = wire.Override
	Effective      = wire.Effective
	Health         = wire.Health
	WorkspaceInfo  = wire.WorkspaceInfo
	NativeSettings = wire.NativeSettings

	NativeTestRequest = wire.NativeTestRequest
	NativeTestResult  = wire.NativeTestResult
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
		Mode:     a.ModeOrDefault(),
		Skills:   a.SkillsOrDefault(),
		Context:  a.Context,
		Accept:   slices.Clone(wire.DefaultAccept),
		Native:   nativeSettingsOf(a.Native),
	}
}

// nativeSettingsOf renders the read-only provider view. The key itself never
// leaves the process (plan 2026-09-26 G).
func nativeSettingsOf(n config.NativeConfig) *NativeSettings {
	src := n.KeySourceName()
	return &NativeSettings{
		Model:         n.Model,
		APIFormat:     n.APIFormat,
		BaseURL:       n.BaseURL,
		APIKeyEnv:     n.APIKeyEnv,
		APIKeyPresent: src != "" && src != config.KeySourceNone,
		APIKeySource:  src,
		Headers:       n.Headers,
		WebSearch:     n.WebSearch,
		MaxTokens:     n.MaxTokens,
		ContextTokens: n.ContextTokens,
	}
}

// Timeout defaults for the manager timers.
const (
	DefaultIdleClose         = 15 * time.Minute
	DefaultPermissionTimeout = 10 * time.Minute
	DefaultMaxLive           = 4
	DefaultStartTimeout      = 30 * time.Second
)

// ResolverTiming carries the process timers the manager reads.
type ResolverTiming struct {
	IdleClose         time.Duration
	PermissionTimeout time.Duration
	MaxLive           int
	// StartTimeout fails a new turn that shows no event.
	StartTimeout time.Duration
}

// Resolver merges the global defaults and the workspace override.
type Resolver struct {
	cfg      ConfigSource
	ws       OverrideStore
	rt       Detector
	nativeFn func() config.NativeConfig
	timing   ResolverTiming
}

// NewResolver builds a resolver over config defaults, the workspace override
// store, and the harness detector. nativeFn supplies the native provider
// config on every read, so a saved provider change lands without a restart
// (plan 2026-09-26 G).
func NewResolver(cfg ConfigSource, ws OverrideStore, rt Detector, nativeFn func() config.NativeConfig, timing ResolverTiming) *Resolver {
	if timing.IdleClose <= 0 {
		timing.IdleClose = DefaultIdleClose
	}
	if timing.PermissionTimeout <= 0 {
		timing.PermissionTimeout = DefaultPermissionTimeout
	}
	if timing.MaxLive <= 0 {
		timing.MaxLive = DefaultMaxLive
	}
	if timing.StartTimeout <= 0 {
		timing.StartTimeout = DefaultStartTimeout
	}
	if nativeFn == nil {
		nativeFn = func() config.NativeConfig { return config.NativeConfig{} }
	}
	return &Resolver{cfg: cfg, ws: ws, rt: rt, nativeFn: nativeFn, timing: timing}
}

// nativeConfig returns the current native provider config.
func (r *Resolver) nativeConfig() config.NativeConfig { return r.nativeFn() }

// LiveNativeConfig reads the current provider config for the native driver
// and the resolver. The model is the native block's own model; an empty one
// falls back to assistant.model (annotation 2026-09-26). Both the daemon
// and the API server wire this one function, so every reader sees the same
// merge.
func LiveNativeConfig() config.NativeConfig {
	cur, err := config.Load()
	if err != nil || cur == nil {
		return config.NativeConfig{}
	}
	n := cur.Assistant.Native
	n.Model = cur.Assistant.NativeModel()
	return n
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
	if ov.Email != nil && len(ov.Email.Accept) > 0 {
		eff.Accept = ov.Email.Accept
		apply("email_accept", true)
	}
	if ov.Email != nil && ov.Email.Style != nil {
		eff.Style = *ov.Email.Style
		apply("email_style", true)
	}
	if eff.Name == "" {
		info, werr := r.ws.WorkspaceInfo(ctx, workspaceID)
		if werr != nil {
			return Effective{}, werr
		}
		eff.Name = info.Name
	}
	for _, f := range []string{"disabled", "name", "harness", "model", "effort", "mode", "skills", "context", "email_accept", "triggers"} {
		if source[f] == "" {
			source[f] = "default"
		}
	}
	resolved, health := r.resolveHarness(ctx, eff)
	// The provider block is Defaults scope only (plan 2026-09-26 G): a
	// workspace view never carries it.
	eff.Native = nil
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
	native := r.nativeConfig()
	if strings.TrimSpace(native.Model) != "" {
		if _, _, err := native.ResolveKey(); err == nil {
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
		native := r.nativeConfig()
		_, _, keyErr := native.ResolveKey()
		model := strings.TrimSpace(native.Model)
		ok := model != "" && keyErr == nil
		auth := "ok"
		detail := ""
		if model == "" {
			auth = "missing"
			detail = "no model: set one in Settings -> Assistant"
		} else if keyErr != nil {
			auth = "missing"
			detail = "no API key: set one in Settings -> Assistant"
		}
		return resolved, Health{Available: ok, Auth: auth, Detail: detail}
	}
	d, err := r.rt.Detect(ctx, agentwire.Harness(wire.BaseHarness(resolved)))
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

// HarnessStatus is the actionable posture of one harness option
// (plan 2026-09-26 F). `available` told the UI only installed or not; native
// is always installed, so an unconfigured native harness read as broken.
type HarnessStatus string

const (
	HarnessReady         HarnessStatus = "ready"
	HarnessNotInstalled  HarnessStatus = "not_installed"
	HarnessNotConfigured HarnessStatus = "not_configured"
	HarnessLoginRequired HarnessStatus = "login_required"
	HarnessUnsupported   HarnessStatus = "unsupported" // below the version floor, or no wire
)

// HarnessOption is one entry of the harness picker (plan F.1).
type HarnessOption struct {
	Name      string        `json:"name"`
	Base      string        `json:"base,omitempty"` // the harness that a variant runs
	Label     string        `json:"label"`
	Available bool          `json:"available"`
	Status    HarnessStatus `json:"status"`
	Auth      string        `json:"auth"` // ok|missing|unknown
	Detail    string        `json:"detail,omitempty"`
	Version   string        `json:"version,omitempty"`
	Native    bool          `json:"native"`
}

// HarnessOptions probes native, the fake driver, and every agent-wire CLI
// harness. Native comes first (plan F.1). The CLI probes run in parallel
// under one 3 s deadline, so a cold picker still answers fast
// (plan 2026-09-26 H).
func (r *Resolver) HarnessOptions(ctx context.Context) []HarnessOption {
	// Native answers through its registered driver: the credentials come
	// from the live config (plan 2026-09-26 I.7). The model is the native
	// block's own model, with assistant.model as the fallback.
	nativeOpt := HarnessOption{Name: HarnessNative, Label: "Native", Native: true, Auth: "unknown"}
	model := strings.TrimSpace(r.nativeConfig().Model)
	if d, err := r.rt.Detect(ctx, agentwire.Native); err != nil {
		nativeOpt.Status = HarnessNotConfigured
		nativeOpt.Detail = err.Error()
	} else {
		nativeOpt.Available = d.Supported
		nativeOpt.Auth = string(d.Auth)
		switch {
		case model == "":
			nativeOpt.Status = HarnessNotConfigured
			nativeOpt.Detail = "no model: set one in Settings -> Assistant"
		case d.Auth == "missing":
			nativeOpt.Status = HarnessNotConfigured
			nativeOpt.Detail = d.Detail
		default:
			nativeOpt.Status = HarnessReady
			nativeOpt.Version = model
		}
	}
	out := []HarnessOption{nativeOpt}

	var ks []agentwire.Harness
	for _, k := range agentwire.Harnesses() {
		if k != agentwire.Native {
			ks = append(ks, k)
		}
	}
	dets := make([]agentwire.Detection, len(ks))
	errs := make([]error, len(ks))
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range ks {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			dets[i], errs[i] = r.rt.Detect(probeCtx, ks[i])
		}()
	}
	wg.Wait()
	for i := range ks {
		opt := HarnessOption{Name: string(ks[i]), Label: harnessLabel(string(ks[i])), Auth: "unknown"}
		if err := errs[i]; err != nil {
			// The probe never answered: report unknown rather than claim
			// the harness is missing (plan 2026-09-26 H).
			opt.Detail = err.Error()
			opt.Status = HarnessUnsupported
			out = append(out, opt)
			continue
		}
		d := dets[i]
		opt.Available = d.Supported
		opt.Auth = string(d.Auth)
		opt.Detail = d.Detail
		opt.Version = d.Version
		opt.Status = harnessStatusOf(d)
		out = append(out, opt)
	}
	return append(out, variantOptions(out)...)
}

// variantOptions gives one option for each harness variant in config.yaml.
// A variant has the program of its base, and its own login, which the
// picker does not probe.
func variantOptions(opts []HarnessOption) []HarnessOption {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	var out []HarnessOption
	for _, name := range slices.Sorted(maps.Keys(cfg.VariantBases())) {
		i := slices.IndexFunc(opts, func(o HarnessOption) bool { return o.Name == cfg.HarnessBase(name) })
		if i < 0 {
			continue
		}
		opt := opts[i]
		opt.Name, opt.Base, opt.Label, opt.Auth = name, opt.Name, cmp.Or(cfg.HarnessLabel(name), name), "unknown"
		if opt.Status == HarnessLoginRequired {
			opt.Status = HarnessReady
		}
		out = append(out, opt)
	}
	return out
}

// harnessStatusOf maps one detection onto the picker posture.
func harnessStatusOf(d agentwire.Detection) HarnessStatus {
	switch {
	case d.Ready:
		return HarnessReady
	case d.Path == "":
		return HarnessNotInstalled
	case !d.Supported:
		return HarnessUnsupported
	case d.Auth == "missing":
		return HarnessLoginRequired
	default:
		return HarnessReady
	}
}

// harnessLabel renders the picker label of one harness name.
func harnessLabel(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
