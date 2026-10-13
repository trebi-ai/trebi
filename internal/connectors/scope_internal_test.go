package connectors

import (
	"errors"
	"testing"

	"github.com/trebi-ai/trebi/internal/workspaces"
)

// TestVisibleIn covers the one visibility check of a connection.
func TestVisibleIn(t *testing.T) {
	t.Parallel()
	s := &Service{
		conns: map[string]Connection{
			"c_glob": {ID: "c_glob", Slug: "glob"},
			"c_word": {ID: "c_word", Slug: "word", Scope: Scope{ScopeGlobal}},
			"c_team": {ID: "c_team", Slug: "team", Scope: Scope{"ws_a", "ws_b"}},
		},
		slugs: map[string]string{"glob": "c_glob", "word": "c_word", "team": "c_team"},
		overrides: map[string]map[string]workspaces.ConnectorOverride{
			"ws_a": {"glob": {Disabled: true}, "team": {Disabled: true}},
		},
	}
	tests := []struct {
		name, ws, slug string
		want           error
	}{
		{"no scope is global", "ws_c", "glob", nil},
		{"global word", "ws_c", "word", nil},
		{"in scope", "ws_b", "team", nil},
		{"out of scope", "ws_c", "team", ErrNotInScope},
		{"disabled global", "ws_a", "glob", ErrDisabled},
		{"disabled in scope", "ws_a", "team", ErrDisabled},
		{"empty workspace sees scoped", "", "team", nil},
		{"native name", "ws_c", "email", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.hiddenIn(tt.ws, tt.slug)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("hiddenIn(%q, %q) = %v; want %v", tt.ws, tt.slug, err, tt.want)
			}
			if got := s.visibleIn(tt.ws, tt.slug); got != (tt.want == nil) {
				t.Fatalf("visibleIn = %v", got)
			}
		})
	}
}
