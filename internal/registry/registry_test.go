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
	"sync/atomic"
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

	res, err := svc.Search(context.Background(), SearchOpts{Query: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	entries, next := res.Entries, res.Next
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

	if _, err := svc.Search(context.Background(), SearchOpts{Query: "github", Registry: "official", Limit: 500}); err != nil {
		t.Fatal(err)
	}
	if rec.lastQuery.Get("search") != "github" {
		t.Fatalf("search = %q", rec.lastQuery.Get("search"))
	}
	if rec.lastQuery.Get("limit") != "100" {
		t.Fatalf("limit = %q, want clamp to 100", rec.lastQuery.Get("limit"))
	}

	// A bare cursor applies to the one named registry.
	if _, err := svc.Search(context.Background(), SearchOpts{Registry: "official", Cursor: "CUR2", Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if rec.lastQuery.Get("cursor") != "CUR2" || rec.lastQuery.Get("limit") != "5" {
		t.Fatalf("cursor = %q limit = %q", rec.lastQuery.Get("cursor"), rec.lastQuery.Get("limit"))
	}
}

func TestSearchUnknownRegistry(t *testing.T) {
	srv := fixtureServer(t, nil)
	svc := testService(t, srv)
	_, err := svc.Search(context.Background(), SearchOpts{Registry: "nope"})
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

// countingServer serves list.json and counts the list requests.
func countingServer(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if status != http.StatusOK {
			http.Error(w, "down", status)
			return
		}
		serveFixture(t, w, "list.json")
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// TestSearchSkipsRegistryWithoutPosition proves a continuation reads only
// the registries that still have a key in the merged cursor.
func TestSearchSkipsRegistryWithoutPosition(t *testing.T) {
	a, aN := countingServer(t, http.StatusOK)
	b, bN := countingServer(t, http.StatusOK)
	svc := New(Config{Registries: []config.Registry{{Name: "a", URL: a.URL}, {Name: "b", URL: b.URL}}})
	res, err := svc.Search(context.Background(), SearchOpts{Cursor: `{"a":"CUR2"}`})
	if err != nil {
		t.Fatal(err)
	}
	if aN.Load() != 1 || bN.Load() != 0 {
		t.Fatalf("requests a=%d b=%d", aN.Load(), bN.Load())
	}
	for _, e := range res.Entries {
		if e.Registry != "a" {
			t.Fatalf("entry from %s", e.Registry)
		}
	}
}

// TestSearchPartialFailure proves one failing registry adds a registry error
// and the others still answer; all failing is an error.
func TestSearchPartialFailure(t *testing.T) {
	good, _ := countingServer(t, http.StatusOK)
	bad, _ := countingServer(t, http.StatusInternalServerError)
	svc := New(Config{Registries: []config.Registry{{Name: "good", URL: good.URL}, {Name: "bad", URL: bad.URL}}})
	res, err := svc.Search(context.Background(), SearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) == 0 || len(res.Errors) != 1 || res.Errors[0].Registry != "bad" || res.Errors[0].Error == "" {
		t.Fatalf("res = %+v", res)
	}
	if _, err := svc.Search(context.Background(), SearchOpts{Registry: "bad"}); err == nil {
		t.Fatal("a search where every registry fails must fail")
	}
}

// TestAPIVersionFallback proves a registry that has only /v0 answers, and
// that the client keeps /v0 for that registry after the first call.
func TestAPIVersionFallback(t *testing.T) {
	var v01 atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v0.1/", func(w http.ResponseWriter, r *http.Request) {
		v01.Add(1)
		http.NotFound(w, r)
	})
	mux.HandleFunc("/v0/servers", func(w http.ResponseWriter, _ *http.Request) { serveFixture(t, w, "list.json") })
	mux.HandleFunc("/v0/servers/", func(w http.ResponseWriter, _ *http.Request) { serveFixture(t, w, "get-adtest.json") })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	svc := New(Config{Registries: []config.Registry{{Name: "old", URL: srv.URL}}})
	res, err := svc.Search(context.Background(), SearchOpts{})
	if err != nil || len(res.Entries) == 0 {
		t.Fatalf("search: %v %+v", err, res)
	}
	if _, err := svc.Get(context.Background(), "old", "ai.adtest/adtest-mcp", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if v01.Load() != 1 {
		t.Fatalf("/v0.1 requests = %d, want 1", v01.Load())
	}
}

func TestFetchSkillCapsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size := maxSkillBody
		if r.URL.Path == "/big" {
			size++
		}
		_, _ = w.Write([]byte(strings.Repeat("x", size)))
	}))
	t.Cleanup(srv.Close)
	svc := New(Config{SkillHTTP: srv.Client()})
	if body, err := svc.FetchSkill(context.Background(), srv.URL+"/ok"); err != nil || len(body) != maxSkillBody {
		t.Fatalf("ok: len %d err %v", len(body), err)
	}
	if _, err := svc.FetchSkill(context.Background(), srv.URL+"/big"); err == nil {
		t.Fatal("a body over the cap must be an error")
	}
}
