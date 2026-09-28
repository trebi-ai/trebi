package config

import (
	"fmt"
	"strings"
	"time"
)

const (
	DefaultRequestsExpireAfter = 168 * time.Hour
	DefaultRequestsRetain      = 720 * time.Hour
)

// RequestsConfig holds the timers of job requests to a person.
type RequestsConfig struct {
	ExpireAfter string `yaml:"expire_after,omitempty"` // duration; default 168h
	Retain      string `yaml:"retain,omitempty"`       // duration; default 720h
}

func positiveDuration(raw string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(raw)); err == nil && d > 0 {
		return d
	}
	return def
}

// ExpireAfterDuration is how long a request waits for an answer.
func (r RequestsConfig) ExpireAfterDuration() time.Duration {
	return positiveDuration(r.ExpireAfter, DefaultRequestsExpireAfter)
}

// RetainDuration is how long a closed request keeps its report snapshot.
func (r RequestsConfig) RetainDuration() time.Duration {
	return positiveDuration(r.Retain, DefaultRequestsRetain)
}

func (e *ConfigError) checkRequests(r RequestsConfig) {
	for _, f := range []struct{ key, v string }{{"requests.expire_after", r.ExpireAfter}, {"requests.retain", r.Retain}} {
		if strings.TrimSpace(f.v) == "" {
			continue
		}
		if d, err := time.ParseDuration(strings.TrimSpace(f.v)); err != nil || d <= 0 {
			e.add(f.key, "want a positive duration, for example 168h")
		}
	}
}

// setRequestsKey handles requests.* keys; handled=false when path is not one.
func (c *Config) setRequestsKey(path, value string) (handled bool, err error) {
	var dst *string
	switch path {
	case "requests.expire_after":
		dst = &c.Requests.ExpireAfter
	case "requests.retain":
		dst = &c.Requests.Retain
	default:
		return false, nil
	}
	v := strings.TrimSpace(value)
	if v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			return true, fmt.Errorf("%s: want a positive duration, for example 168h", path)
		}
	}
	*dst = v
	return true, nil
}

func (l *pathEntryList) addRequests(cfg *Config) {
	r := cfg.Requests
	l.add("requests.expire_after", r.ExpireAfterDuration().String(), DefaultRequestsExpireAfter.String())
	l.add("requests.retain", r.RetainDuration().String(), DefaultRequestsRetain.String())
}
