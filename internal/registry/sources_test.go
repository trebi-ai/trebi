package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
)

func TestSmitherySource(t *testing.T) {
	rec := &recorder{}
	svc := kindService(kindServer(t, rec))
	ctx := context.Background()
	res, err := svc.Search(ctx, SearchOpts{Registry: "smithery", Query: "github", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rec.lastQuery.Get("q") != "github" || rec.lastQuery.Get("pageSize") != "2" {
		t.Fatalf("query = %v", rec.lastQuery)
	}
	if len(res.Entries) != 2 || res.Entries[0].Server.Name != "github" || res.Entries[0].Trust != TrustVerified {
		t.Fatalf("entries = %+v", res.Entries)
	}
	if res.Next != `{"smithery":"2"}` {
		t.Fatalf("next = %q", res.Next)
	}
	if _, err := svc.Search(ctx, SearchOpts{Registry: "smithery", Cursor: res.Next}); err != nil || rec.lastQuery.Get("page") != "2" {
		t.Fatalf("page 2: %v %v", err, rec.lastQuery)
	}

	e, err := svc.Get(ctx, "smithery", "deepwiki", "")
	if err != nil {
		t.Fatal(err)
	}
	def, _, err := e.Definition(InstallOpts{Package: -1, Remote: -1})
	if err != nil {
		t.Fatal(err)
	}
	if def.Actions.MCP.URL != "https://deepwiki.run.tools" {
		t.Fatalf("mcp = %+v", def.Actions.MCP)
	}
	if _, err := svc.Get(ctx, "smithery", "nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestDockerSource(t *testing.T) {
	svc := kindService(kindServer(t, &recorder{}))
	ctx := context.Background()
	all, err := svc.Search(ctx, SearchOpts{Registry: "docker", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Entries) != 2 || all.Entries[0].Server.Name != "brave" || all.Next != `{"docker":"2"}` || all.Entries[0].Trust != TrustVerified {
		t.Fatalf("page 1 = %+v next %q", all.Entries, all.Next)
	}
	hit, err := svc.Search(ctx, SearchOpts{Registry: "docker", Query: "DOCUMENTATION"})
	if err != nil || len(hit.Entries) != 1 || hit.Entries[0].Server.Name != "context7" {
		t.Fatalf("search = %+v %v", hit.Entries, err)
	}

	brave, err := svc.Get(ctx, "docker", "brave", "")
	if err != nil {
		t.Fatal(err)
	}
	def, secrets, err := brave.Definition(InstallOpts{Package: -1, Remote: -1, Env: map[string]string{"BRAVE_API_KEY": "k"}})
	if err != nil {
		t.Fatal(err)
	}
	m := def.Actions.MCP
	if m.Command != "docker" || m.Env["BRAVE_API_KEY"] != "secret:brave_BRAVE_API_KEY" || m.Env["BRAVE_MCP_TRANSPORT"] != "stdio" || len(secrets) != 1 {
		t.Fatalf("brave = %+v secrets %+v", m, secrets)
	}

	hub, _ := svc.Get(ctx, "docker", "dockerhub", "")
	def, _, err = hub.Definition(InstallOpts{Package: -1, Remote: -1, Env: map[string]string{"HUB_PAT_TOKEN": "t", "dockerhub.username": "me"}})
	if err != nil {
		t.Fatal(err)
	}
	if args := def.Actions.MCP.Args; !reflect.DeepEqual(args[len(args)-2:], []string{"--transport=stdio", "--username=me"}) {
		t.Fatalf("args = %v", args)
	}

	c7, _ := svc.Get(ctx, "docker", "context7", "")
	def, _, err = c7.Definition(InstallOpts{Package: -1, Remote: -1, Env: map[string]string{"CONTEXT7_API_KEY": "secret:c7"}})
	if err != nil {
		t.Fatal(err)
	}
	if def.Actions.MCP.URL != "https://mcp.context7.com/mcp" || def.Actions.MCP.Headers["CONTEXT7_API_KEY"] != "secret:c7" {
		t.Fatalf("context7 = %+v", def.Actions.MCP)
	}

	curl, _ := svc.Get(ctx, "docker", "curl", "")
	if _, _, err := curl.Definition(InstallOpts{Package: -1, Remote: -1}); !errors.Is(err, ErrUnsupportedPackage) {
		t.Fatalf("poci = %v", err)
	}
	if _, err := svc.Get(ctx, "docker", "nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestOfficialTrust(t *testing.T) {
	m := mcpRegistry{reg: config.Registry{URL: DefaultRegistryURL + "/"}}
	if m.trust() != TrustOfficial {
		t.Fatal("official registry is not official")
	}
	u, _ := url.Parse("https://registry.example.com")
	if (mcpRegistry{reg: config.Registry{URL: u.String()}}).trust() != TrustCommunity {
		t.Fatal("private registry is not community")
	}
}

// kindServer serves the smithery and docker fixtures and records the last
// Smithery list query.
func kindServer(t *testing.T, rec *recorder) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	serve := func(w http.ResponseWriter, name string) {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
		}
		_, _ = w.Write(b)
	}
	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		rec.lastQuery = r.URL.Query()
		serve(w, "smithery/list.json")
	})
	mux.HandleFunc("/servers/deepwiki", func(w http.ResponseWriter, r *http.Request) { serve(w, "smithery/get-deepwiki.json") })
	mux.HandleFunc("/catalog.yaml", func(w http.ResponseWriter, r *http.Request) { serve(w, "docker/catalog.yaml") })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func kindService(srv *httptest.Server) *Service {
	return New(Config{Registries: []config.Registry{
		{Name: "smithery", URL: srv.URL, Kind: config.RegistrySmithery},
		{Name: "docker", URL: srv.URL + "/catalog.yaml", Kind: config.RegistryDocker},
	}})
}
