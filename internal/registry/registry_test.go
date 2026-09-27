package registry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
)

// getFixtures maps a decoded name@version to a testdata get reply.
var getFixtures = map[string]string{
	"ai.adtest/adtest-mcp@1.1.0":  "get-adtest.json",
	"ai.adtest/adtest-mcp@latest": "get-adtest.json",
	"medium-ops@0.1.0":            "get-mediumops.json",
	"acme/remote@2.0.0":           "get-remote.json",
	"acme/docker@1.0.0":           "get-oci.json",
	"acme/cargo@1.0.0":            "get-unsupported.json",
	"acme/contributed@1.0.0":      "get-contributed.json",
}

// recorder captures the last list request so a test can assert its query.
type recorder struct {
	lastQuery url.Values
}

// fixtureServer serves the testdata fixtures. It records the last list query.
func fixtureServer(t *testing.T, rec *recorder) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v0.1/servers", func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			rec.lastQuery = r.URL.Query()
		}
		serveFixture(t, w, "list.json")
	})
	mux.HandleFunc("/v0.1/servers/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.EscapedPath(), "/v0.1/servers/")
		nameEsc, verEsc, ok := strings.Cut(rest, "/versions/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		name, _ := url.PathUnescape(nameEsc)
		ver, _ := url.PathUnescape(verEsc)
		file, ok := getFixtures[name+"@"+ver]
		if !ok {
			http.NotFound(w, r)
			return
		}
		serveFixture(t, w, file)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func serveFixture(t *testing.T, w http.ResponseWriter, name string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Errorf("read fixture %s: %v", name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// testService builds a client over one or two named registries at the fixture
// server.
func testService(t *testing.T, srv *httptest.Server) *Service {
	t.Helper()
	return New(Config{Registries: []config.Registry{
		{Name: "official", URL: srv.URL},
		{Name: "second", URL: srv.URL},
	}})
}

func TestSearchMergesRegistries(t *testing.T) {
	srv := fixtureServer(t, nil)
	svc := testService(t, srv)

	entries, next, err := svc.Search(context.Background(), SearchOpts{Query: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(entries))
	}
	if entries[0].Registry != "official" || entries[2].Registry != "second" {
		t.Fatalf("registry tags = %q %q", entries[0].Registry, entries[2].Registry)
	}
	if entries[0].Server.Name != "ai.adtest/adtest-mcp" {
		t.Fatalf("name = %q", entries[0].Server.Name)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(next), &decoded); err != nil {
		t.Fatalf("next %q: %v", next, err)
	}
	if decoded["official"] != "CUR2" || decoded["second"] != "CUR2" {
		t.Fatalf("next = %v", decoded)
	}
}

func TestSearchPassesQueryParams(t *testing.T) {
	rec := &recorder{}
	srv := fixtureServer(t, rec)
	svc := New(Config{Registries: []config.Registry{{Name: "official", URL: srv.URL}}})

	if _, _, err := svc.Search(context.Background(), SearchOpts{Query: "github", Registry: "official", Limit: 500}); err != nil {
		t.Fatal(err)
	}
	if rec.lastQuery.Get("search") != "github" {
		t.Fatalf("search = %q", rec.lastQuery.Get("search"))
	}
	if rec.lastQuery.Get("limit") != "100" {
		t.Fatalf("limit = %q, want clamp to 100", rec.lastQuery.Get("limit"))
	}

	// A bare cursor applies to the one named registry.
	if _, _, err := svc.Search(context.Background(), SearchOpts{Registry: "official", Cursor: "CUR2", Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if rec.lastQuery.Get("cursor") != "CUR2" || rec.lastQuery.Get("limit") != "5" {
		t.Fatalf("cursor = %q limit = %q", rec.lastQuery.Get("cursor"), rec.lastQuery.Get("limit"))
	}
}

func TestSearchUnknownRegistry(t *testing.T) {
	srv := fixtureServer(t, nil)
	svc := testService(t, srv)
	_, _, err := svc.Search(context.Background(), SearchOpts{Registry: "nope"})
	if !errors.Is(err, ErrUnknownRegistry) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetByVersion(t *testing.T) {
	srv := fixtureServer(t, nil)
	svc := testService(t, srv)
	entry, err := svc.Get(context.Background(), "official", "ai.adtest/adtest-mcp", "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Registry != "official" {
		t.Fatalf("registry = %q", entry.Registry)
	}
	if entry.Server.Version != "1.1.0" || len(entry.Server.Packages) != 1 {
		t.Fatalf("server = %+v", entry.Server)
	}
	if entry.Server.Packages[0].RegistryType != "npm" {
		t.Fatalf("registryType = %q", entry.Server.Packages[0].RegistryType)
	}
}

func TestGetLatestAndDefaultRegistry(t *testing.T) {
	srv := fixtureServer(t, nil)
	svc := testService(t, srv)
	entry, err := svc.Get(context.Background(), "", "ai.adtest/adtest-mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Registry != "official" || entry.Server.Version != "1.1.0" {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestGetNotFound(t *testing.T) {
	srv := fixtureServer(t, nil)
	svc := testService(t, srv)
	_, err := svc.Get(context.Background(), "official", "acme/missing", "1.0.0")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetUnknownRegistry(t *testing.T) {
	srv := fixtureServer(t, nil)
	svc := testService(t, srv)
	_, err := svc.Get(context.Background(), "nope", "ai.adtest/adtest-mcp", "1.1.0")
	if !errors.Is(err, ErrUnknownRegistry) {
		t.Fatalf("err = %v", err)
	}
}
