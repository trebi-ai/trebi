package daemon

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
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
	armExpiry := func() {
		name, at, err := harness.EarliestExpiry(ctx, c.Store)
		if expiry != nil {
			expiry.Stop()
			expiry = nil
		}
		if err != nil || at.IsZero() {
			return
		}
		d := time.Until(at.Add(harness.ExpiryGrace))
		if d < 0 {
			d = 0
		}
		expiry = time.NewTimer(d)
		c.log.Debug("auth expiry timer", "harness", name, "in", d)
	}
	armExpiry()

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
			c.probeBlocked(ctx)
			if w != nil {
				c.syncCredWatch(w)
			}
			armExpiry()
		case <-expC:
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
			if name := credFileHarness(ev.Name); name != "" {
				if _, err := c.Harness.Probe(ctx, name, harness.SourceProbe); err != nil {
					c.log.Warn("cred-watch probe", "harness", name, "error", err)
				}
				armExpiry()
			}
		}
	}
}

func (c *ControlPlane) probeBlocked(ctx context.Context) {
	names, err := harness.BlockedNames(ctx, c.Store)
	if err != nil {
		c.log.Warn("list blocked harnesses", "error", err)
		return
	}
	for _, n := range names {
		if _, err := c.Harness.Probe(ctx, n, harness.SourceProbe); err != nil {
			c.log.Warn("auth probe", "harness", n, "error", err)
		}
	}
}

func (c *ControlPlane) probeExpiring(ctx context.Context) {
	name, at, err := harness.EarliestExpiry(ctx, c.Store)
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
	seen := map[string]struct{}{}
	for _, n := range harness.Names() {
		caps := c.Harness.Caps(n)
		if caps.CredFile == "" {
			continue
		}
		dir := filepath.Dir(caps.CredFile)
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		_ = w.Add(dir)
	}
}

func credFileHarness(path string) string {
	base := filepath.Base(path)
	switch base {
	case ".credentials.json":
		return "claude"
	case "oauth_creds.json":
		return "gemini"
	case "auth.json":
		// disambiguate by parent
		parent := filepath.Base(filepath.Dir(path))
		switch parent {
		case "codex":
			return "codex"
		case "grok":
			return "grok"
		case "pi", "agent":
			return "pi"
		case "opencode":
			return "opencode"
		}
		for _, n := range []string{"codex", "grok", "pi", "opencode"} {
			if filepath.Base(filepath.Dir(path)) == n {
				return n
			}
		}
	}
	return ""
}
