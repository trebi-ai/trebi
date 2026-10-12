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

// DefaultCloudFallbackURL is the workers.dev address of the same Worker.
// The daemon uses it when a network blocks DefaultCloudAPIURL. It changes
// together with DefaultCloudAPIURL.
const DefaultCloudFallbackURL = "https://trebi-cloud-api-staging.ocral.workers.dev"

// CloudConfig is the hosted link block. It holds no token and no key: the
// device id lives in the store and the private key lives in the secrets
// directory.
type CloudConfig struct {
	APIURL       string    `yaml:"api_url,omitempty"`
	FallbackURLs *[]string `yaml:"fallback_urls,omitempty"` // nil → default list; [] → no fallback
	Enabled      *bool     `yaml:"enabled,omitempty"`       // nil → true; false keeps a linked device offline
	RemoteMCP    *bool     `yaml:"remote_mcp,omitempty"`    // nil → true
	Mobile       *bool     `yaml:"mobile,omitempty"`        // nil → true
}

// RemoteMCPOn reports whether remote MCP clients may use this machine.
func (c CloudConfig) RemoteMCPOn() bool { return c.RemoteMCP == nil || *c.RemoteMCP }

// MobileOn reports whether paired phones may use this machine.
func (c CloudConfig) MobileOn() bool { return c.Mobile == nil || *c.Mobile }

// Active reports whether the daemon may dial the hosted workers.
func (c CloudConfig) Active() bool { return c.Enabled == nil || *c.Enabled }

// APIURLOrDefault returns the configured endpoint or the default.
func (c CloudConfig) APIURLOrDefault() string {
	if u := strings.TrimSpace(c.APIURL); u != "" {
		return u
	}
	return DefaultCloudAPIURL
}

// FallbackURLsOrDefault returns the configured fallback URLs. Unset, it is
// the default fallback when api_url is the default, and empty otherwise.
func (c CloudConfig) FallbackURLsOrDefault() []string {
	if c.FallbackURLs != nil {
		out := make([]string, 0, len(*c.FallbackURLs))
		for _, u := range *c.FallbackURLs {
			if u = strings.TrimSpace(u); u != "" {
				out = append(out, u)
			}
		}
		return out
	}
	if strings.TrimSpace(c.APIURL) == "" {
		return []string{DefaultCloudFallbackURL}
	}
	return []string{}
}

// APIURLs returns the primary URL first, then the fallback URLs.
func (c CloudConfig) APIURLs() []string {
	return append([]string{c.APIURLOrDefault()}, c.FallbackURLsOrDefault()...)
}

// DefaultDirectPort is the default mobile.direct.port.
const DefaultDirectPort = 7434

// MobileConfig is the phone block. cloud.mobile stays the on/off switch
// for phone access; this block holds the direct path on the same network.
type MobileConfig struct {
	Direct MobileDirect `yaml:"direct,omitempty"`
}

// MobileDirect is the direct listener for phones on the same network.
type MobileDirect struct {
	Enabled *bool `yaml:"enabled,omitempty"` // nil → true
	Port    *int  `yaml:"port,omitempty"`    // nil → DefaultDirectPort
}

// On reports whether the daemon opens the direct listener.
func (d MobileDirect) On() bool { return d.Enabled == nil || *d.Enabled }

// PortOrDefault returns the configured port or the default.
func (d MobileDirect) PortOrDefault() int {
	if d.Port == nil {
		return DefaultDirectPort
	}
	return *d.Port
}

func (e *ConfigError) checkMobile(m MobileConfig) {
	if p := m.Direct.PortOrDefault(); p < 1 || p > 65535 {
		e.add("mobile.direct.port", fmt.Sprintf("%d is not a TCP port (1 to 65535)", p))
	}
}

// checkCloud validates the cloud block. A Worker URL and a granted cloud
// link at once is a runtime conflict that doctor reports, not a config
// error: the link state lives in the store (plan 2026-09-22 Part D).
func (e *ConfigError) checkCloud(c Config) {
	if u, err := url.Parse(strings.TrimSpace(c.Cloud.APIURLOrDefault())); err != nil ||
		u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		e.add("cloud.api_url", fmt.Sprintf("%q is not a parseable http(s) URL", c.Cloud.APIURL))
	}
	primary := c.Cloud.APIURLOrDefault()
	for _, raw := range c.Cloud.FallbackURLsOrDefault() {
		if u, err := url.Parse(raw); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			e.add("cloud.fallback_urls", fmt.Sprintf("%q is not a parseable http(s) URL", raw))
		} else if strings.TrimRight(raw, "/") == strings.TrimRight(primary, "/") {
			e.add("cloud.fallback_urls", fmt.Sprintf("%q is the same as cloud.api_url", raw))
		}
	}
}
