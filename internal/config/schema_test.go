package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

func TestSchemaCoversEveryKey(t *testing.T) {
	sc := NewSchema()
	ids := map[string]bool{}
	for _, s := range sc.Sections {
		ids[s.ID] = true
	}
	types := []string{TypeString, TypeInt, TypeBool, TypeDuration, TypeList, TypeMap, TypeJSON}
	for _, k := range sc.Keys {
		if !ids[k.Section] {
			t.Errorf("%s: section %q not listed", k.Key, k.Section)
		}
		if !slices.Contains(types, k.Type) {
			t.Errorf("%s: type %q", k.Key, k.Type)
		}
	}
	if k, _ := sc.Key("tools.claude.effort"); !slices.Contains(k.Enum, "max") {
		t.Fatalf("tools.claude.effort enum = %v", k.Enum)
	}
}

func TestDraftCheck(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("defaults:\n  command: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		d    Draft
		want int
	}{
		{"good set", Draft{Set: map[string]string{"notify.route": `[{"via":"email"}]`}}, 0},
		{"bad set", Draft{Set: map[string]string{"harness.runtime": "pane"}}, 1},
		{"bad unset", Draft{Unset: []string{"nope.key"}}, 1},
		{"removed key in yaml", Draft{YAML: ptr("notify:\n  email: true\n")}, 1},
	}
	for _, c := range cases {
		if got := c.d.Check(); len(got) != c.want {
			t.Errorf("%s: errors = %v", c.name, got)
		}
	}
}

func ptr(s string) *string { return &s }

// TestToolApproval pins tools.<name>.approval: ask or auto, auto by default.
func TestToolApproval(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	cfg := defaultConfig()
	if got := cfg.HarnessApproval("grok"); got != "auto" {
		t.Fatalf("default approval = %q", got)
	}
	if err := cfg.SetPath("tools.grok.approval", "maybe"); err == nil {
		t.Fatal("bad approval accepted")
	}
	if err := cfg.SetPath("tools.grok.approval", "ask"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.HarnessApproval("grok"); got != "ask" {
		t.Fatalf("approval = %q", got)
	}
	if k, _ := NewSchema().Key("tools.grok.approval"); !slices.Equal(k.Enum, []string{"ask", "auto"}) {
		t.Fatalf("schema enum = %v", k.Enum)
	}
	cfg.Tools.Pin("grok").Approval = "always"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "tools.grok.approval") {
		t.Fatalf("validate = %v", err)
	}
	if HarnessAsksApproval("grok") || !HarnessAsksApproval("omp") {
		t.Fatal("only grok cannot ask")
	}
}
