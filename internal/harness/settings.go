package harness

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/store"
)

// Patch is a change to the settings of one harness. A nil field stays. An
// empty Model or Effort clears the value.
type Patch struct {
	Enabled  *bool   `json:"enabled,omitempty"`
	AuthMode *string `json:"auth_mode,omitempty"`
	Model    *string `json:"model,omitempty"`
	Effort   *string `json:"effort,omitempty"`
	Approval *string `json:"approval,omitempty"`
	Updates  *string `json:"updates,omitempty"`
	Version  *string `json:"version,omitempty"`
	// Label is the display name of a variant.
	Label *string `json:"label,omitempty"`
	// Env changes the env: {KEY: {value, secret} | null}.
	Env map[string]*EnvChange `json:"env,omitempty"`
	// Compat is {"claude": {"agents": true, "hooks": false, ...}}.
	Compat map[string]map[string]bool `json:"compat,omitempty"`
}

// ErrDefaultOff refuses to turn off the default harness for new jobs.
var ErrDefaultOff = errors.New("choose another default harness first")

// ErrNoConfigField refuses a setting on a harness with no tools.<name> field.
var ErrNoConfigField = errors.New("this harness has no settings in config.yaml")

// ErrVariantProgram refuses an update setting on a variant: a variant runs
// the program of its base.
var ErrVariantProgram = errors.New("a variant uses the program of its base harness")

// Apply writes p to config.yaml, applies the auth mode, and publishes
// harness.updated. Turning a harness on runs a probe.
func (s *Service) Apply(ctx context.Context, name string, p Patch) error {
	if !Known(name) {
		return fmt.Errorf("unknown harness %q", name)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	base := cfg.HarnessBase(name)
	if !config.IsFirstClass(base) {
		return ErrNoConfigField
	}
	if base != name && (p.Updates != nil || p.Version != nil) {
		return ErrVariantProgram
	}
	attrs := map[string]string{}
	set := func(key, value string) error {
		if err := cfg.SetPath("tools."+name+"."+key, value); err != nil {
			return err
		}
		attrs[key] = value
		return nil
	}
	if p.Label != nil {
		if base == name {
			return fmt.Errorf("%w: only a harness that you made has a label", ErrInvalid)
		}
		if err := set("label", strings.TrimSpace(*p.Label)); err != nil {
			return err
		}
	}
	if p.Enabled != nil {
		if !*p.Enabled && cfg.Defaults.Command == name {
			return ErrDefaultOff
		}
		if err := set("enabled", strconv.FormatBool(*p.Enabled)); err != nil {
			return err
		}
	}
	if p.Model != nil {
		if *p.Model != "" && !config.HarnessSupportsModel(base) {
			return fmt.Errorf("%s takes no model", name)
		}
		if err := set("model", *p.Model); err != nil {
			return err
		}
	}
	if p.Effort != nil {
		if err := set("effort", *p.Effort); err != nil {
			return err
		}
	}
	if p.Approval != nil {
		if err := set("approval", *p.Approval); err != nil {
			return err
		}
	}
	if p.Updates != nil {
		if err := set("updates", *p.Updates); err != nil {
			return err
		}
	}
	if p.Version != nil {
		if err := set("version", *p.Version); err != nil {
			return err
		}
	}
	for vendor, keys := range p.Compat {
		if vendor != "claude" {
			return fmt.Errorf("compat: unknown vendor %q", vendor)
		}
		for k, v := range keys {
			if err := set("compat.claude."+k, strconv.FormatBool(v)); err != nil {
				return err
			}
		}
	}
	if p.AuthMode != nil && *p.AuthMode != AuthModeShared && *p.AuthMode != AuthModeSeparate {
		return fmt.Errorf("auth mode must be shared or separate")
	}
	if len(attrs) > 0 {
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	if p.Env != nil {
		if err := s.SetEnv(ctx, name, p.Env); err != nil {
			return err
		}
	}
	if p.AuthMode != nil {
		if err := s.persistMode(ctx, name, *p.AuthMode); err != nil {
			return err
		}
	}
	if p.Updates != nil || p.Version != nil {
		mode, version := updatesFromConfig(cfg, name)
		if err := s.SetUpdates(ctx, name, string(mode), version); err != nil && !errors.Is(err, ErrNoVisor) {
			return err
		}
	}
	if p.AuthMode != nil || p.Env != nil || (p.Enabled != nil && *p.Enabled) {
		// Probe publishes harness.updated when the auth state changes. The
		// variants of a base that turns on start to work too.
		probe := []string{name}
		if p.Enabled != nil && *p.Enabled {
			for v, b := range cfg.VariantBases() {
				if b == name {
					probe = append(probe, v)
				}
			}
		}
		for _, n := range probe {
			if _, err := s.Probe(ctx, n, SourceProbe); err != nil {
				s.log.Warn("probe after patch", "harness", n, "error", err)
			}
		}
	}
	if len(attrs) > 0 {
		return s.PublishUpdated(ctx, name, attrs)
	}
	return nil
}

// PublishUpdated appends harness.updated with attrs and the harness name.
func (s *Service) PublishUpdated(ctx context.Context, name string, attrs map[string]string) error {
	a := map[string]string{"harness": name}
	for k, v := range attrs {
		a[k] = v
	}
	_, err := s.st.Append(ctx, store.BusEvent{
		Topic: "harness.updated", Entity: "harness", EntityID: name, Action: "updated", Attrs: a,
	})
	return err
}
