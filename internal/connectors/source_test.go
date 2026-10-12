package connectors_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/ghfake"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestCheckSource(t *testing.T) {
	_, svc, gh := linkStack(t)
	ctx := context.Background()
	type row struct{ group, status, field string }
	cases := []struct {
		fixture string
		can     bool
		want    []row
		found   []string
	}{
		{fixture: "buildable", can: true, want: []row{{"manifest", "pass", ""}, {"valid", "pass", ""}, {"skill", "pass", ""}, {"program", "pass", ""}, {"inputs", "pass", ""}}},
		{fixture: "nomanifest", want: []row{{"manifest", "fail", ""}}, found: []string{"alpha", "group/beta"}},
		{fixture: "badkeys", want: []row{{"valid", "fail", "colour"}, {"valid", "fail", "title"}}},
		{fixture: "noschema", want: []row{{"events", "fail", ""}}},
		{fixture: "compat", can: true, want: []row{{"skill", "warn", ""}}},
		{fixture: "catalogonly", want: []row{{"program", "fail", "install"}}},
		{fixture: "nogomod", want: []row{{"program", "fail", ""}}},
	}
	for _, tc := range cases {
		gh.Push("acme/tools", "main", sourceFiles(t, tc.fixture, tc.fixture+"/"))
		chk, err := svc.CheckSource(ctx, "https://github.com/acme/tools/tree/main/"+tc.fixture)
		if err != nil {
			t.Fatalf("%s: %v", tc.fixture, err)
		}
		if chk.CanInstall != tc.can {
			t.Errorf("%s: can_install = %v, results %+v", tc.fixture, chk.CanInstall, chk.Results)
		}
		for _, w := range tc.want {
			if !slices.ContainsFunc(chk.Results, func(r connectors.SourceResult) bool {
				return r.Group == w.group && r.Status == w.status && (w.field == "" || r.Field == w.field)
			}) {
				t.Errorf("%s: no %+v row in %+v", tc.fixture, w, chk.Results)
			}
		}
		if tc.found != nil && !slices.Equal(chk.Found, tc.found) {
			t.Errorf("%s: found = %v", tc.fixture, chk.Found)
		}
		if _, err := os.Stat(home.ConnectorStagingDir(chk.ID)); err != nil {
			t.Errorf("%s: no staging folder: %v", tc.fixture, err)
		}
	}
	gh.Push("acme/tools", "main", sourceFiles(t, "buildable", "vitally/"))
	chk, err := svc.CheckSource(ctx, "acme/tools/vitally@main")
	if err != nil {
		t.Fatal(err)
	}
	if chk.Entry == nil || chk.Entry.Name != "vitally" || chk.Runs.Program != "vitally" || chk.Runs.Method != "build" ||
		!slices.Contains(chk.Runs.Commands, "vitally") || !slices.Contains(chk.Files, "main.go") || len(chk.Source.Commit) != 40 {
		t.Fatalf("check = %+v", chk)
	}
}

// linkStack is a service whose links read a fake GitHub.
func linkStack(t *testing.T) (*store.Store, *connectors.Service, *ghfake.Server) {
	t.Helper()
	st, _, svc := openStack(t)
	gh := ghfake.New()
	t.Cleanup(gh.Close)
	svc.WithInstall(connectors.InstallConfig{GitHubAPI: gh.API.URL})
	return st, svc, gh
}

// sourceFiles reads one folder of testdata/sources into a fake tree under
// prefix.
func sourceFiles(t *testing.T, name, prefix string) map[string]ghfake.File {
	t.Helper()
	root := filepath.Join("testdata", "sources", name)
	out := map[string]ghfake.File{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[prefix+filepath.ToSlash(rel)] = ghfake.File{Body: string(b)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
