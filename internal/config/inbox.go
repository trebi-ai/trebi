package config

import (
	"fmt"
	"strings"
	"time"
)

// DefaultInboxDoneRetention is how long a Done inbox item stays.
const DefaultInboxDoneRetention = 720 * time.Hour

// InboxConfig holds the inbox timers.
type InboxConfig struct {
	DoneRetention string `yaml:"done_retention,omitempty"` // duration; default 720h
}

// DoneRetentionDuration is how long a Done item stays before the daemon deletes it.
func (i InboxConfig) DoneRetentionDuration() time.Duration {
	return positiveDuration(i.DoneRetention, DefaultInboxDoneRetention)
}

func (e *ConfigError) checkInbox(i InboxConfig) {
	if v := strings.TrimSpace(i.DoneRetention); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			e.add("inbox.done_retention", "want a positive duration, for example 720h")
		}
	}
}

// setInboxKey handles inbox.* keys; handled=false when path is not one.
func (c *Config) setInboxKey(path, value string) (handled bool, err error) {
	if path != "inbox.done_retention" {
		return false, nil
	}
	v := strings.TrimSpace(value)
	if v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			return true, fmt.Errorf("%s: want a positive duration, for example 720h", path)
		}
	}
	c.Inbox.DoneRetention = v
	return true, nil
}

func (l *pathEntryList) addInbox(cfg *Config) {
	l.add("inbox.done_retention", cfg.Inbox.DoneRetentionDuration().String(), DefaultInboxDoneRetention.String())
}
