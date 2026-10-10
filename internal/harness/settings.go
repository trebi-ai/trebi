package harness

import (
	"context"
	"errors"
	"fmt"
	"strconv"

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
	Updates  *string `json:"updates,omitempty"`
	Version  *string `json:"version,omitempty"`
	// Compat is {"claude": {"agents": true, "hooks": false, ...}}.
	Compat map[string]map[string]bool `json:"compat,omitempty"`
}

// ErrDefaultOff refuses to turn off the default harness for new jobs.
var ErrDefaultOff = errors.New("choose another default harness first")

// ErrNoConfigField refuses a setting on a harness with no tools.<name> field.
var ErrNoConfigField = errors.New("this harness has no settings in config.yaml")

// Apply writes p to config.yaml, applies the auth mode, and publishes
// harness.updated. Turning a harness on runs a probe.
func (s *Service) Apply(ctx context.Context, name string, p Patch) error {
	if !Known(name) {
		return fmt.Errorf("unknown harness %q", name)
	}
	if !config.IsFirstClass(name) {
		return ErrNoConfigField
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	attrs := map[string]string{}
	set := func(key, value string) error {
		if err := cfg.SetPath("tools."+name+"."+key, value); err != nil {
			return err
		}
		attrs[key] = value
		return nil
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
		if *p.Model != "" && !config.HarnessSupportsModel(name) {
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
	if len(p.Compat) > 0 {
		// Grok reads the switches from its config.toml.
		if err := EnsureBridgesOpts(name, cfg, s.opts.Roots.withDefaults(), EnsureOpts{}); err != nil {
			s.log.Warn("bridge after compat", "harness", name, "error", err)
		}
		attrs["files"] = "true"
	}
	if p.AuthMode != nil {
		if err := s.persistMode(name, *p.AuthMode); err != nil {
			return err
		}
	}
	if p.AuthMode != nil || (p.Enabled != nil && *p.Enabled) {
		// Probe publishes harness.updated when the auth state changes.
		if _, err := s.Probe(ctx, name, SourceProbe); err != nil {
			s.log.Warn("probe after patch", "harness", name, "error", err)
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
