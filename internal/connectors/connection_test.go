package connectors_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"gopkg.in/yaml.v3"
)

// TestValueForms proves a Value reads a string and a list and writes one
// string for one item.
func TestValueForms(t *testing.T) {
	t.Parallel()
	var sub connectors.Subscription
	if err := yaml.Unmarshal([]byte("id: sub_x\nvalues: {team: ENG, resources: [Issue, Comment]}\n"), &sub); err != nil {
		t.Fatal(err)
	}
	want := map[string]connectors.Value{"team": {"ENG"}, "resources": {"Issue", "Comment"}}
	if !reflect.DeepEqual(sub.Values, want) {
		t.Fatalf("yaml values = %+v", sub.Values)
	}
	out, err := yaml.Marshal(sub)
	if err != nil || !strings.Contains(string(out), "team: ENG") {
		t.Fatalf("yaml = %s, %v", out, err)
	}
	b, err := json.Marshal(sub.Values)
	if err != nil || string(b) != `{"resources":["Issue","Comment"],"team":"ENG"}` {
		t.Fatalf("json = %s, %v", b, err)
	}
	var back map[string]connectors.Value
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, want) {
		t.Fatalf("json back = %+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"a":1}`), &back); err == nil {
		t.Fatal("a number decoded")
	}
}

// TestPatchMakesSubscriptionIDs proves a subscription with no id gets one
// and a bad id fails validation.
func TestPatchMakesSubscriptionIDs(t *testing.T) {
	c := connectors.Connection{ID: connectors.NewConnectionID(), Type: "x", Slug: "x", From: connectors.FromInline}
	c.Subscriptions = []connectors.Subscription{{ID: "bad"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "subscriptions[0].id") {
		t.Fatalf("bad id: %v", err)
	}
	if id := connectors.NewSubscriptionID(); !strings.HasPrefix(id, "sub_") || len(id) != 30 {
		t.Fatalf("id = %q", id)
	}
}

// TestValidateGitHubSource proves the record rules of a link connection.
func TestValidateGitHubSource(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("3f", 20)
	src := &connectors.GitHubSource{Repo: "flarco/cli-tools", Path: "vitally-cli", Ref: "main", Commit: sha}
	base := connectors.Connection{ID: connectors.NewConnectionID(), Type: "vitally", Slug: "vitally", From: connectors.FromGitHub,
		Source: src, Version: "0.1.0", Digest: "sha256:9c1e"}
	cases := []struct {
		name  string
		edit  func(c *connectors.Connection)
		field string // "" is valid
	}{
		{"valid", func(*connectors.Connection) {}, ""},
		{"no source", func(c *connectors.Connection) { c.Source = nil }, "source"},
		{"short commit", func(c *connectors.Connection) { c.Source = &connectors.GitHubSource{Repo: src.Repo, Commit: "3f2a"} }, "source.commit"},
		{"bad repo", func(c *connectors.Connection) { c.Source = &connectors.GitHubSource{Repo: "nope", Commit: sha} }, "source"},
		{"no version", func(c *connectors.Connection) { c.Version = "" }, "version"},
		{"no digest", func(c *connectors.Connection) { c.Digest = "" }, "digest"},
		{"track", func(c *connectors.Connection) { c.Track = "minor" }, "track"},
		{"manifest", func(c *connectors.Connection) { c.Manifest = &manifest.Manifest{} }, "manifest"},
		{"source on catalog", func(c *connectors.Connection) { c.From = connectors.FromCatalog }, "source"},
		{"digest on inline", func(c *connectors.Connection) {
			c.From, c.Source, c.Manifest = connectors.FromInline, nil, &manifest.Manifest{}
		}, "digest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := base
			tc.edit(&c)
			err := c.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("valid record: %v", err)
				}
				return
			}
			var ve *connectors.ValidationError
			if !errors.As(err, &ve) || !slices.ContainsFunc(ve.Fields, func(f connectors.FieldError) bool { return f.Field == tc.field }) {
				t.Fatalf("want a %s error, got %v", tc.field, err)
			}
		})
	}
}
