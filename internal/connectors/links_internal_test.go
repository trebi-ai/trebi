package connectors

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

func TestCommandLinksPlan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pkg := func(typ, version string, files ...string) Resolved {
		dir := filepath.Join(root, typ, version)
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(dir, "bin", f), nil, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return Resolved{ID: typ + "-" + version, Type: typ, Version: version, From: FromCatalog, Dir: dir, Manifest: manifest.Manifest{
			Install: &manifest.Install{Bin: "tool"},
			Actions: &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"tool", "tool-extra", "../evil", "trebi"}}},
		}}
	}
	file := pkg("local", "1.0.0", "tool")
	file.From = fromFile + "/x"
	l := newCommandLinks([]Resolved{
		pkg("beta", "1.0.0", "tool"),
		pkg("alpha", "1.0.0", "tool", "tool-extra", "trebi"),
		pkg("alpha", "1.2.0", "tool"),
		file,
	})
	if got, want := l.want["tool"].path, filepath.Join(root, "alpha", "1.2.0", "bin", "tool"); got != want {
		t.Fatalf("tool = %q, want %q", got, want)
	}
	if got := l.want["tool-extra"].path; got != filepath.Join(root, "alpha", "1.0.0", "bin", "tool-extra") {
		t.Fatalf("tool-extra = %q", got)
	}
	if len(l.want) != 2 {
		t.Fatalf("want = %v", l.want)
	}
	if len(l.clashes) != 1 || l.clashes[0] != (linkClash{Command: "tool", Linked: "alpha", Skipped: "beta", ID: "beta-1.0.0"}) {
		t.Fatalf("clashes = %+v", l.clashes)
	}
}
