package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
)

// catalogServer serves a signed index and one snapshot. Tamper changes the
// index after it is signed; down makes every request fail.
type catalogServer struct {
	mu     sync.Mutex
	key    ed25519.PrivateKey
	idx    Index
	snap   []byte
	tamper bool
	down   bool
}

func (c *catalogServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	body := func() []byte {
		c.mu.Lock()
		defer c.mu.Unlock()
		b, err := json.Marshal(c.idx)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	mux.HandleFunc("/v1/index.json", func(w http.ResponseWriter, _ *http.Request) {
		b := body()
		c.mu.Lock()
		if c.tamper {
			b = append(b, ' ')
		}
		c.mu.Unlock()
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/v1/index.json.sig", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(c.key, body()))))
	})
	mux.HandleFunc("/v1/snapshots/demo/1.1.0.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(c.snap)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		down := c.down
		c.mu.Unlock()
		if down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func newCatalogService(t *testing.T) (*Service, *catalogServer) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := []byte("snapshot bytes")
	sum := sha256.Sum256(snap)
	cs := &catalogServer{key: key, snap: snap}
	srv := httptest.NewServer(cs.handler(t))
	t.Cleanup(srv.Close)
	cs.idx = Index{GeneratedAt: "2026-09-28T00:00:00Z", Entries: []IndexEntry{
		{Name: "demo", Title: "Demo", Description: "A demo connector", Publisher: "trebi", Capabilities: []string{"actions"}, Latest: "1.1.0",
			Versions: []IndexVersion{
				{Version: "1.0.0", Digest: "sha256:00", URL: srv.URL + "/v1/snapshots/demo/1.0.0.tar.gz"},
				{Version: "1.1.0", Digest: "sha256:" + hex.EncodeToString(sum[:]), URL: srv.URL + "/v1/snapshots/demo/1.1.0.tar.gz"},
			}},
		{Name: "other", Title: "Other", Description: "Community entry", Publisher: "community", Latest: "0.1.0",
			Versions: []IndexVersion{{Version: "0.1.0"}}},
	}}
	svc := New(Config{
		Registries:  []config.Registry{{Name: "trebi", URL: srv.URL + "/v1", Kind: config.RegistryTrebi}},
		CatalogKeys: []ed25519.PublicKey{pub},
	})
	return svc, cs
}

func TestTrebiCatalog(t *testing.T) {
	ctx := context.Background()
	svc, cs := newCatalogService(t)

	res, err := svc.Search(ctx, SearchOpts{Query: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Catalog == nil || res.Entries[0].Trust != TrustOfficial || res.Entries[0].Server.Version != "1.1.0" {
		t.Fatalf("search = %+v", res.Entries)
	}
	page, err := svc.Search(ctx, SearchOpts{Limit: 1})
	if err != nil || len(page.Entries) != 1 || page.Next != `{"trebi":"1"}` {
		t.Fatalf("page = %+v %v", page, err)
	}
	if e, err := svc.Get(ctx, "", "other", ""); err != nil || e.Trust != TrustVerified {
		t.Fatalf("other = %+v %v", e, err)
	}
	if _, err := svc.Get(ctx, "", "demo", "9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version = %v", err)
	}
	vers, err := svc.Versions(ctx, "demo")
	if err != nil || len(vers) != 2 {
		t.Fatalf("versions = %+v %v", vers, err)
	}
	if b, err := svc.Snapshot(ctx, "demo", "1.1.0"); err != nil || string(b) != "snapshot bytes" {
		t.Fatalf("snapshot = %q %v", b, err)
	}
	if _, err := svc.Snapshot(ctx, "demo", "1.0.0"); err == nil {
		t.Fatal("a snapshot that misses its digest must fail")
	}

	// A bad signature keeps the last good index and reports the problem.
	svc.mu.Lock()
	for k, v := range svc.trebi {
		v.at = v.at.Add(-2 * catalogTTL)
		svc.trebi[k] = v
	}
	svc.mu.Unlock()
	cs.mu.Lock()
	cs.tamper = true
	cs.mu.Unlock()
	res, err = svc.Search(ctx, SearchOpts{Query: "demo"})
	if err != nil || len(res.Entries) != 1 || len(res.Stale) != 1 {
		t.Fatalf("stale search = %+v %v", res, err)
	}
	if p := svc.CatalogProblems(); !errors.Is(p["trebi"], ErrBadSignature) {
		t.Fatalf("problems = %v", p)
	}
}

func TestTrebiCatalogRefusesUnsigned(t *testing.T) {
	svc, cs := newCatalogService(t)
	cs.tamper = true
	if _, err := svc.Search(context.Background(), SearchOpts{}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v", err)
	}
}

// TestSearchServesStalePage proves a failing source serves its last good
// page with the stale mark.
func TestSearchServesStalePage(t *testing.T) {
	ctx := context.Background()
	svc, cs := newCatalogService(t)
	if _, err := svc.Search(ctx, SearchOpts{Query: "demo"}); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	svc.trebi = map[string]trebiLoaded{}
	svc.mu.Unlock()
	cs.mu.Lock()
	cs.down = true
	cs.mu.Unlock()
	res, err := svc.Search(ctx, SearchOpts{Query: "demo"})
	if err != nil || len(res.Entries) != 1 || len(res.Stale) != 1 || res.Stale[0] != "trebi" {
		t.Fatalf("res = %+v %v", res, err)
	}
	if _, err := svc.Search(ctx, SearchOpts{Query: "never searched"}); err == nil {
		t.Fatal("a query with no kept page must fail")
	}
}

func TestEmbeddedCatalogKey(t *testing.T) {
	if len(catalogKeys()) != 1 {
		t.Fatalf("embedded keys = %d", len(catalogKeys()))
	}
}
