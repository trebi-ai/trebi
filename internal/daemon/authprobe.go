package daemon

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

const defaultAuthProbeInterval = 5 * time.Minute

func (c *ControlPlane) authProbe(ctx context.Context) {
	interval := c.Opts.AuthProbeInterval
	if interval <= 0 {
		interval = defaultAuthProbeInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var expiry *time.Timer
	// probed records the expiry each harness was last woken for. A probe that
	// cannot refresh the credential leaves auth_expires_at unchanged, so
	// without this the zero-duration timer re-arms on the same past date and
	// spins. Each expiry value earns one immediate probe; the ticker retries.
	probed := map[string]time.Time{}
	armExpiry := func() {
		name, at, err := c.Harness.EarliestExpiry(ctx)
		if expiry != nil {
			expiry.Stop()
			expiry = nil
		}
		if err != nil || at.IsZero() {
			return
		}
		d := time.Until(at.Add(harness.ExpiryGrace))
		if d < 0 {
			if last, ok := probed[name]; ok && last.Equal(at) {
				return
			}
			d = 0
		}
		expiry = time.NewTimer(d)
		c.log.Debug("auth expiry timer", "harness", name, "in", d)
	}
	armExpiry()
	c.probeBlocked(ctx)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		c.log.Warn("auth cred watcher unavailable", "error", err)
		w = nil
	} else {
		defer w.Close()
		c.syncCredWatch(w)
	}

	for {
		var expC <-chan time.Time
		if expiry != nil {
			expC = expiry.C
		}
		var events <-chan fsnotify.Event
		if w != nil {
			events = w.Events
		}
		select {
		case <-ctx.Done():
			if expiry != nil {
				expiry.Stop()
			}
			return
		case <-ticker.C:
			clear(probed)
			c.probeBlocked(ctx)
			if w != nil {
				c.syncCredWatch(w)
			}
			armExpiry()
		case <-expC:
			if name, at, err := c.Harness.EarliestExpiry(ctx); err == nil && !at.IsZero() {
				probed[name] = at
			}
			c.probeExpiring(ctx)
			armExpiry()
		case ev, ok := <-events:
			if !ok {
				w = nil
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			for _, name := range configFileHarnesses(ev.Name) {
				// Keeps user_changed_since_own and an open file dialog current.
				if err := c.Harness.PublishUpdated(ctx, name, map[string]string{"files": "true"}); err != nil {
					c.log.Warn("config-file watch", "harness", name, "error", err)
				}
			}
			for _, name := range c.credFileHarnesses(ev.Name) {
				if _, err := c.Harness.Probe(ctx, name, harness.SourceProbe); err != nil {
					c.log.Warn("cred-watch probe", "harness", name, "error", err)
				}
				armExpiry()
			}
		}
	}
}

// harnessOn reads config.yaml per call, so a harness that the user turns
// off stops at once. A config that does not load keeps each harness on.
func harnessOn(name string) bool {
	cfg, err := config.Load()
	if err != nil {
		return true
	}
	return cfg.HarnessEnabled(name)
}

func (c *ControlPlane) probeBlocked(ctx context.Context) {
	names, err := c.Harness.BlockedNames(ctx)
	if err != nil {
		c.log.Warn("list blocked harnesses", "error", err)
		return
	}
	for _, n := range names {
		if !harnessOn(n) {
			continue
		}
		if _, err := c.Harness.Probe(ctx, n, harness.SourceProbe); err != nil {
			c.log.Warn("auth probe", "harness", n, "error", err)
		}
	}
}

func (c *ControlPlane) probeExpiring(ctx context.Context) {
	name, at, err := c.Harness.EarliestExpiry(ctx)
	if err != nil || name == "" {
		return
	}
	if time.Now().Before(at.Add(harness.ExpiryGrace)) {
		return
	}
	if _, err := c.Harness.Probe(ctx, name, harness.SourceProbe); err != nil {
		c.log.Warn("expiry probe", "harness", name, "error", err)
	}
}

func (c *ControlPlane) syncCredWatch(w *fsnotify.Watcher) {
	userHome, _ := os.UserHomeDir()
	seen := map[string]struct{}{}
	for _, n := range harness.Names() {
		caps := c.Harness.Caps(n)
		for _, f := range []string{caps.CredFile, caps.SharedCredFile} {
			if f == "" {
				continue
			}
			dir := filepath.Dir(f)
			if fi, err := os.Stat(f); err == nil && fi.IsDir() {
				dir = f // credential location is a directory (copilot)
			}
			// Never watch the user home: kqueue opens every entry, which trips
			// macOS TCC prompts for Desktop/Documents/Downloads.
			if userHome != "" && filepath.Clean(dir) == filepath.Clean(userHome) {
				continue
			}
			if _, ok := seen[dir]; ok {
				continue
			}
			seen[dir] = struct{}{}
			// Do not make a vendor folder in the user home.
			if f == caps.SharedCredFile {
				if _, err := os.Stat(dir); err != nil {
					continue
				}
			} else if err := os.MkdirAll(dir, 0o755); err != nil {
				continue
			}
			_ = w.Add(dir)
		}
	}
	// The user config files (D10). Only folders that exist, never the home.
	for _, spec := range harness.Specs() {
		for _, f := range spec.Files {
			dir := filepath.Dir(filepath.Join(userHome, f.UserRel))
			if userHome == "" || filepath.Clean(dir) == filepath.Clean(userHome) {
				continue
			}
			if _, ok := seen[dir]; ok {
				continue
			}
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			seen[dir] = struct{}{}
			_ = w.Add(dir)
		}
	}
}

// configFileHarnesses returns the harnesses that have path as a user config
// file.
func configFileHarnesses(path string) []string {
	userHome, _ := os.UserHomeDir()
	if userHome == "" {
		return nil
	}
	path = filepath.Clean(path)
	var out []string
	for _, spec := range harness.Specs() {
		for _, f := range spec.Files {
			if filepath.Join(userHome, f.UserRel) == path {
				out = append(out, spec.Name)
				break
			}
		}
	}
	return out
}

// credFileHarnesses returns the harnesses that are on and whose credential
// file is path. A shared user file can serve two harnesses (opencode,
// opencode2).
func (c *ControlPlane) credFileHarnesses(path string) []string {
	path = filepath.Clean(path)
	var out []string
	for _, n := range harness.Names() {
		if !harnessOn(n) {
			continue
		}
		caps := c.Harness.Caps(n)
		for _, f := range []string{caps.CredFile, caps.SharedCredFile} {
			if f == "" {
				continue
			}
			f = filepath.Clean(f)
			if f == path || filepath.Dir(path) == f {
				out = append(out, n)
				break
			}
		}
	}
	return out
}
