package config

import (
	"strings"
	"testing"
)

func TestRemoteMCPOnDefault(t *testing.T) {
	t.Parallel()
	on, off := true, false
	for _, tc := range []struct {
		val  *bool
		want bool
	}{{nil, true}, {&on, true}, {&off, false}} {
		if got := (CloudConfig{RemoteMCP: tc.val}).RemoteMCPOn(); got != tc.want {
			t.Errorf("RemoteMCPOn(%v) = %v, want %v", tc.val, got, tc.want)
		}
	}
}

func TestCloudAPIURLs(t *testing.T) {
	t.Parallel()
	empty := []string{}
	custom := []string{" https://b.example ", ""}
	for _, tc := range []struct {
		name string
		cfg  CloudConfig
		want []string
	}{
		{"default", CloudConfig{}, []string{DefaultCloudAPIURL, DefaultCloudFallbackURL}},
		{"own api_url has no default fallback", CloudConfig{APIURL: "https://a.example"}, []string{"https://a.example"}},
		{"explicit empty list", CloudConfig{FallbackURLs: &empty}, []string{DefaultCloudAPIURL}},
		{"explicit list", CloudConfig{APIURL: "https://a.example", FallbackURLs: &custom}, []string{"https://a.example", "https://b.example"}},
	} {
		got := tc.cfg.APIURLs()
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: APIURLs() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCheckCloudFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		urls []string
		bad  bool
	}{
		{"valid", []string{"https://b.example"}, false},
		{"not a URL", []string{"b.example"}, true},
		{"same as primary", []string{DefaultCloudAPIURL + "/"}, true},
	} {
		urls := tc.urls
		ce := &ConfigError{}
		ce.checkCloud(Config{Cloud: CloudConfig{FallbackURLs: &urls}})
		got := false
		for _, fe := range ce.Errors {
			got = got || fe.Field == "cloud.fallback_urls"
		}
		if got != tc.bad {
			t.Errorf("%s: error = %v, want %v (%v)", tc.name, got, tc.bad, ce.Errors)
		}
	}
}

func TestSetCloudFallbackURLs(t *testing.T) {
	t.Parallel()
	var c Config
	if _, err := c.setCloudKey("cloud.fallback_urls", "[]"); err != nil {
		t.Fatal(err)
	}
	if c.Cloud.FallbackURLs == nil || len(c.Cloud.APIURLs()) != 1 {
		t.Fatalf("[] must turn the fallback off: %v", c.Cloud.APIURLs())
	}
	if _, err := c.setCloudKey("cloud.fallback_urls", "-"); err != nil {
		t.Fatal(err)
	}
	if c.Cloud.FallbackURLs != nil {
		t.Fatalf("- must restore the default: %v", *c.Cloud.FallbackURLs)
	}
}
