package config

import (
	"fmt"
	"net/url"
	"strings"
)

// DefaultCloudAPIURL is the hosted account endpoint. Staging is the
// default for now: production routes attach later and flip this. A
// different deployment sets cloud.api_url and nothing else (plan 2026-09-22
// Part J).
const DefaultCloudAPIURL = "https://staging-api.trebi.ai"

// CloudConfig is the hosted link block. It holds no token and no key: the
// device id lives in the store and the private key lives in the secrets
// directory.
type CloudConfig struct {
	APIURL  string `yaml:"api_url,omitempty"`
	Enabled *bool  `yaml:"enabled,omitempty"` // nil → true; false keeps a linked device offline
}

// Active reports whether the daemon may dial the hosted workers.
func (c CloudConfig) Active() bool { return c.Enabled == nil || *c.Enabled }

// APIURLOrDefault returns the configured endpoint or the default.
func (c CloudConfig) APIURLOrDefault() string {
	if u := strings.TrimSpace(c.APIURL); u != "" {
		return u
	}
	return DefaultCloudAPIURL
}

// Present reports whether the cloud block is in the file. An absent block
// keeps the hosted defaults; a worker-less email block then has nothing
// to point at and stays a config error.
func (c CloudConfig) Present() bool {
	return strings.TrimSpace(c.APIURL) != "" || c.Enabled != nil
}

// checkCloud validates the cloud block. A Worker URL and a granted cloud
// link at once is a runtime conflict that doctor reports, not a config
// error: the link state lives in the store (plan 2026-09-22 Part D).
func (e *ConfigError) checkCloud(c Config) {
	if u, err := url.Parse(strings.TrimSpace(c.Cloud.APIURLOrDefault())); err != nil ||
		u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		e.add("cloud.api_url", fmt.Sprintf("%q is not a parseable http(s) URL", c.Cloud.APIURL))
	}
}
