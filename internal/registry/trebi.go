package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

// catalogPub is the base64 ed25519 key that signs the Trebi catalog index.
//
//go:embed catalog.pub
var catalogPub string

// catalogTTL is how long a fetched index or search page stays fresh.
const catalogTTL = 10 * time.Minute

// maxSnapshot caps one snapshot download.
const maxSnapshot = 32 << 20

// maxBinary caps one program archive download.
const maxBinary = 256 << 20

// ErrBadSignature is an index whose signature no trusted key verifies.
var ErrBadSignature = errors.New("catalog signature is not valid")

// Index is the signed index.json of the Trebi catalog.
type Index struct {
	GeneratedAt string       `json:"generated_at"`
	Entries     []IndexEntry `json:"entries"`
}

// IndexEntry is one connector of the Trebi catalog.
type IndexEntry struct {
	Name         string         `json:"name"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	Publisher    string         `json:"publisher"`
	Capabilities []string       `json:"capabilities"`
	IconURL      string         `json:"icon_url,omitempty"`
	Runs         string         `json:"runs,omitempty"`  // online: an MCP URL and nothing to install
	Login        string         `json:"login,omitempty"` // the label of the first login method
	Latest       string         `json:"latest"`
	Versions     []IndexVersion `json:"versions"`
}

// Where an entry runs.
const (
	RunsLocal  = "local"
	RunsOnline = "online"
)

// IndexVersion is one immutable snapshot of a catalog connector.
type IndexVersion struct {
	Version     string        `json:"version"`
	Digest      string        `json:"digest"`
	URL         string        `json:"url"`
	PublishedAt string        `json:"published_at"`
	Yanked      bool          `json:"yanked,omitempty"`
	Binaries    []IndexBinary `json:"binaries,omitempty"`
}

// IndexBinary is the program archive of one version for one platform. An
// entry with install.catalog has one for each of its platforms.
type IndexBinary struct {
	Platform string `json:"platform"` // "<os>/<arch>"
	URL      string `json:"url"`
	Digest   string `json:"digest"`
}

// version returns the named version of e.
func (e IndexEntry) version(v string) (IndexVersion, bool) {
	i := slices.IndexFunc(e.Versions, func(x IndexVersion) bool { return x.Version == v })
	if i < 0 {
		return IndexVersion{}, false
	}
	return e.Versions[i], true
}

// catalogKeys parses the embedded key. A key change ships both keys for one
// release (Part M.3), so the file may hold more than one line.
func catalogKeys() []ed25519.PublicKey {
	var out []ed25519.PublicKey
	for _, line := range strings.Fields(catalogPub) {
		raw, err := base64.StdEncoding.DecodeString(line)
		if err == nil && len(raw) == ed25519.PublicKeySize {
			out = append(out, ed25519.PublicKey(raw))
		}
	}
	return out
}

// trebiLoaded is the last good index of one catalog URL.
type trebiLoaded struct {
	at  time.Time
	idx Index
}

// trebiCatalog reads the signed index of the Trebi catalog. The registry URL
// is the v1 folder, for example https://catalog.trebi.ai/v1.
type trebiCatalog struct {
	s   *Service
	reg config.Registry
}

// Search filters the index locally. The cursor is an offset.
func (t trebiCatalog) Search(ctx context.Context, q Query) (Page, error) {
	idx, stale, err := t.load(ctx)
	if err != nil {
		return Page{}, err
	}
	text := strings.ToLower(strings.TrimSpace(q.Text))
	var hits []IndexEntry
	for _, e := range idx.Entries {
		if text == "" || strings.Contains(strings.ToLower(e.Name+"\n"+e.Title+"\n"+e.Description), text) {
			hits = append(hits, e)
		}
	}
	start, _ := strconv.Atoi(q.Cursor)
	start = min(max(start, 0), len(hits))
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	end := min(start+limit, len(hits))
	page := Page{Stale: stale}
	for _, e := range hits[start:end] {
		page.Entries = append(page.Entries, t.entry(e, e.Latest))
	}
	if end < len(hits) {
		page.Next = strconv.Itoa(end)
	}
	return page, nil
}

// Get reads one entry at one version; an empty version is the latest.
func (t trebiCatalog) Get(ctx context.Context, name, version string) (Entry, error) {
	idx, _, err := t.load(ctx)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range idx.Entries {
		if e.Name != name {
			continue
		}
		v := strings.TrimPrefix(strings.TrimSpace(version), "v")
		if v == "" || v == "latest" {
			v = e.Latest
		}
		if _, ok := e.version(v); !ok {
			return Entry{}, fmt.Errorf("%w: %s %s@%s", ErrNotFound, t.reg.Name, name, v)
		}
		return t.entry(e, v), nil
	}
	return Entry{}, fmt.Errorf("%w: %s %s", ErrNotFound, t.reg.Name, name)
}

// entry maps one index entry. A Trebi entry is official; any other
// publisher that Trebi reviewed is verified.
func (t trebiCatalog) entry(e IndexEntry, version string) Entry {
	trust := TrustVerified
	if e.Publisher == manifest.PublisherTrebi {
		trust = TrustOfficial
	}
	ce := e
	return Entry{
		Registry: t.reg.Name, Trust: trust, Catalog: &ce,
		Server: ServerJSON{Name: e.Name, Title: e.Title, Description: e.Description, Version: version},
	}
}

// load returns the index, from the cache while it is fresh. A failed fetch
// or a bad signature serves the last good index with stale set.
func (t trebiCatalog) load(ctx context.Context) (Index, bool, error) {
	t.s.mu.Lock()
	cached, ok := t.s.trebi[t.reg.URL]
	t.s.mu.Unlock()
	if ok && time.Since(cached.at) < catalogTTL {
		return cached.idx, false, nil
	}
	idx, err := t.fetchIndex(ctx)
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if errors.Is(err, ErrBadSignature) {
		t.s.sigErr[t.reg.URL] = err
	} else if err == nil {
		delete(t.s.sigErr, t.reg.URL)
	}
	if err != nil {
		if ok {
			t.s.Log.Warn("catalog index", "registry", t.reg.Name, "error", err)
			return cached.idx, true, nil
		}
		return Index{}, false, err
	}
	t.s.trebi[t.reg.URL] = trebiLoaded{at: time.Now(), idx: idx}
	return idx, false, nil
}

// fetchIndex downloads index.json and its signature and checks it.
func (t trebiCatalog) fetchIndex(ctx context.Context) (Index, error) {
	base := strings.TrimRight(t.reg.URL, "/")
	body, err := t.s.fetch(ctx, t.reg, base+"/index.json", "application/json")
	if err != nil {
		return Index{}, err
	}
	sig, err := t.s.fetch(ctx, t.reg, base+"/index.json.sig", "text/plain")
	if err != nil {
		return Index{}, fmt.Errorf("registry %s: signature: %w", t.reg.Name, err)
	}
	if !t.s.verify(body, sig) {
		return Index{}, fmt.Errorf("registry %s: %w", t.reg.Name, ErrBadSignature)
	}
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return Index{}, fmt.Errorf("registry %s: decode index: %w", t.reg.Name, err)
	}
	return idx, nil
}

// verify reports whether a trusted key signed data.
func (s *Service) verify(data, sig []byte) bool {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return false
	}
	for _, k := range s.catalogKeys {
		if ed25519.Verify(k, data, raw) {
			return true
		}
	}
	return false
}

// trebiRegistry returns the first configured Trebi catalog.
func (s *Service) trebiRegistry() (trebiCatalog, error) {
	for _, r := range s.Registries {
		if r.KindOrDefault() == config.RegistryTrebi {
			return trebiCatalog{s: s, reg: r}, nil
		}
	}
	return trebiCatalog{}, fmt.Errorf("%w: no trebi catalog is configured", ErrUnknownRegistry)
}

// Versions lists the catalog versions of one connector, oldest first.
func (s *Service) Versions(ctx context.Context, name string) ([]IndexVersion, error) {
	e, err := s.catalogEntry(ctx, name)
	if err != nil {
		return nil, err
	}
	return slices.Clone(e.Versions), nil
}

func (s *Service) catalogEntry(ctx context.Context, name string) (IndexEntry, error) {
	t, err := s.trebiRegistry()
	if err != nil {
		return IndexEntry{}, err
	}
	en, err := t.Get(ctx, name, "")
	if err != nil {
		return IndexEntry{}, err
	}
	return *en.Catalog, nil
}

// Snapshot downloads one snapshot and checks it against the digest of the
// signed index.
func (s *Service) Snapshot(ctx context.Context, name, version string) ([]byte, error) {
	e, err := s.catalogEntry(ctx, name)
	if err != nil {
		return nil, err
	}
	v, ok := e.version(version)
	if !ok {
		return nil, fmt.Errorf("%w: %s@%s", ErrNotFound, name, version)
	}
	t, _ := s.trebiRegistry() // catalogEntry found it
	body, err := s.fetchCapped(ctx, t.reg, v.URL, "application/gzip", maxSnapshot)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != v.Digest {
		return nil, fmt.Errorf("%w: %s@%s has digest %s, the index says %s", ErrInvalidEntry, name, version, got, v.Digest)
	}
	return body, nil
}

// Binary downloads the program archive of one version for one platform and
// checks it against the digest of the signed index.
func (s *Service) Binary(ctx context.Context, name, version, platform string) ([]byte, error) {
	e, err := s.catalogEntry(ctx, name)
	if err != nil {
		return nil, err
	}
	v, ok := e.version(version)
	if !ok {
		return nil, fmt.Errorf("%w: %s@%s", ErrNotFound, name, version)
	}
	i := slices.IndexFunc(v.Binaries, func(b IndexBinary) bool { return b.Platform == platform })
	if i < 0 {
		return nil, fmt.Errorf("%w: %s@%s has no program for %s", ErrNotFound, name, version, platform)
	}
	b := v.Binaries[i]
	t, _ := s.trebiRegistry() // catalogEntry found it
	body, err := s.fetchCapped(ctx, t.reg, b.URL, "application/gzip", maxBinary)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != b.Digest {
		return nil, fmt.Errorf("%w: the %s program of %s@%s has digest %s, the index says %s", ErrInvalidEntry, platform, name, version, got, b.Digest)
	}
	return body, nil
}

// CatalogProblems returns the signature error of each Trebi catalog whose
// last index did not verify.
func (s *Service) CatalogProblems() map[string]error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]error{}
	for _, r := range s.Registries {
		if err := s.sigErr[r.URL]; err != nil {
			out[r.Name] = err
		}
	}
	return out
}
