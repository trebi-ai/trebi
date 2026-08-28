package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ConfigError is one or more field-level validation problems.
type ConfigError struct {
	Path   string // config file basename or absolute
	Errors []FieldError
}

// FieldError names a field path and reason.
type FieldError struct {
	Field string
	Msg   string
}

func (e *ConfigError) Error() string {
	if e == nil || len(e.Errors) == 0 {
		return "invalid config"
	}
	path := e.Path
	if path == "" {
		path = "config.yaml"
	}
	var b strings.Builder
	for i, fe := range e.Errors {
		if i > 0 {
			b.WriteByte('\n')
		}
		if fe.Field != "" {
			fmt.Fprintf(&b, "%s: %s: %s", path, fe.Field, fe.Msg)
		} else {
			fmt.Fprintf(&b, "%s: %s", path, fe.Msg)
		}
	}
	return b.String()
}

func (e *ConfigError) add(field, msg string) {
	e.Errors = append(e.Errors, FieldError{Field: field, Msg: msg})
}

// durationRe matches job frontmatter: s/m/h only.
var durationRe = regexp.MustCompile(`^(\d+(?:h|m|s))+$`)

// Validate checks enums, durations, and bridge resource keys/modes.
// Returns *ConfigError listing all problems when possible.
func Validate(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	ce := &ConfigError{Path: "config.yaml"}
	if base := ConfigPath(); base != "" {
		ce.Path = base
		// Prefer basename for stable tests when under BaseDir.
		ce.Path = "config.yaml"
	}

	// Terminal
	if cfg.Terminal != "" && !validTerminal(cfg.Terminal) {
		ce.add("terminal", fmt.Sprintf("%q not in [Terminal, iTerm, Ghostty]", cfg.Terminal))
	}

	// defaults.session
	s := cfg.Defaults.Session
	validateDurationField(ce, "defaults.session.timeout", s.Timeout, true)
	validateDurationField(ce, "defaults.session.max_duration", s.MaxDuration, true)
	validateDurationField(ce, "defaults.session.keep_alive", s.KeepAlive, true)
	validateNudgeField(ce, "defaults.session.nudge", s.Nudge)
	if s.IfRunning != "" && !validIfRunningMode(strings.ToLower(strings.TrimSpace(s.IfRunning))) {
		ce.add("defaults.session.if_running",
			fmt.Sprintf("%q not in [skip, replace, coalesce, allow]", s.IfRunning))
	}

	// notify.waiting_after: empty → default 5m; 0/off disables escalate
	validateNudgeField(ce, "notify.waiting_after", cfg.Notify.WaitingAfter)

	if cfg.Files.MaxBytes < 0 {
		ce.add("files.max_bytes", "must be >= 0")
	}

	// tools.fallback
	if cfg.Tools.Fallback != "" && !validFallbackMode(cfg.Tools.Fallback) {
		ce.add("tools.fallback", fmt.Sprintf("%q not in [require, path]", cfg.Tools.Fallback))
	}

	// global bridge
	validateBridge(ce, "tools.bridge", cfg.Tools.Bridge, "")

	for _, name := range FirstClassTools() {
		p := cfg.Tools.Pin(name)
		if p == nil {
			continue
		}
		prefix := "tools." + name
		if p.Auth != "" && !validAuthMode(p.Auth) {
			ce.add(prefix+".auth", fmt.Sprintf("%q not in [shared, separate]", p.Auth))
		}
		validateBridge(ce, prefix+".bridge", p.Bridge, name)
	}

	if len(ce.Errors) == 0 {
		return nil
	}
	return ce
}

// NormalizeBridgeMode maps aliases to inherit|false. Invalid returns empty and false.
func NormalizeBridgeMode(raw string) (mode string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "inherit", "link":
		return "inherit", true
	case "false", "off", "none":
		return "false", true
	default:
		return "", false
	}
}

// IsInherit reports whether a normalized or raw mode means inherit.
func IsInherit(raw string) bool {
	m, ok := NormalizeBridgeMode(raw)
	return ok && m == "inherit"
}

func validateBridge(ce *ConfigError, prefix string, b *BridgeConfig, tool string) {
	if b == nil {
		return
	}
	for _, k := range b.Only {
		if tool != "" {
			if !knownResource(tool, k) {
				ce.add(prefix+".only", fmt.Sprintf("unknown resource %q (known: %s)", k, strings.Join(KnownBridgeResources()[tool], ", ")))
			}
		} else if !knownResourceAny(k) {
			ce.add(prefix+".only", fmt.Sprintf("unknown resource %q", k))
		}
	}
	for _, k := range b.Never {
		if tool != "" {
			if !knownResource(tool, k) {
				ce.add(prefix+".never", fmt.Sprintf("unknown resource %q (known: %s)", k, strings.Join(KnownBridgeResources()[tool], ", ")))
			}
		} else if !knownResourceAny(k) {
			ce.add(prefix+".never", fmt.Sprintf("unknown resource %q", k))
		}
	}
	for k, mode := range b.Resources {
		field := prefix + ".resources." + k
		if tool != "" {
			if !knownResource(tool, k) {
				ce.add(field, fmt.Sprintf("unknown resource (known: %s)", strings.Join(KnownBridgeResources()[tool], ", ")))
				continue
			}
		} else if !knownResourceAny(k) {
			ce.add(field, "unknown resource")
			continue
		}
		if !validBridgeModeValue(mode) {
			ce.add(field, fmt.Sprintf("%q not in [true, false, inherit, link, off, none]", mode))
		}
	}
}

func knownResource(tool, key string) bool {
	for _, k := range KnownBridgeResources()[tool] {
		if k == key {
			return true
		}
	}
	return false
}

func knownResourceAny(key string) bool {
	for _, keys := range KnownBridgeResources() {
		for _, k := range keys {
			if k == key {
				return true
			}
		}
	}
	return false
}

func validateDurationField(ce *ConfigError, field, raw string, allowZeroOff bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return
	}
	if allowZeroOff && (s == "0" || strings.EqualFold(s, "off")) {
		return
	}
	if !durationRe.MatchString(s) {
		ce.add(field, fmt.Sprintf("invalid duration %q (use s/m/h, e.g. 30m, 1h)", s))
		return
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		ce.add(field, fmt.Sprintf("invalid duration %q: %v", s, err))
		return
	}
	if d <= 0 {
		ce.add(field, fmt.Sprintf("duration %q must be positive", s))
	}
}

func validateNudgeField(ce *ConfigError, field, raw string) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "0" || strings.EqualFold(s, "off") {
		return
	}
	validateDurationField(ce, field, s, false)
}

func validTerminal(v string) bool {
	switch v {
	case "Terminal", "iTerm", "Ghostty":
		return true
	}
	return false
}

func validFallbackMode(v string) bool {
	return v == "require" || v == "path"
}

// validAuthMode accepts empty (default shared).
func validAuthMode(v string) bool {
	return v == "" || v == "shared" || v == "separate"
}

// validIfRunningMode accepts empty (default policy).
func validIfRunningMode(v string) bool {
	switch v {
	case "", "skip", "replace", "coalesce", "allow":
		return true
	}
	return false
}

// validBridgeModeValue accepts a bridge mode alias or empty.
func validBridgeModeValue(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	_, ok := NormalizeBridgeMode(raw)
	return ok
}
