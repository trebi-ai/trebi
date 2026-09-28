// Package registry is the client for connector catalogs. It searches the
// configured sources and turns one entry into a connector manifest. A source
// is the signed Trebi catalog, the official MCP Registry API, the Smithery
// API, or the Docker MCP Catalog file.
package registry

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
)

// DefaultRegistryName and DefaultRegistryURL are the built-in registry. The
// config default carries the same pair.
const (
	DefaultRegistryName = "official"
	DefaultRegistryURL  = "https://registry.modelcontextprotocol.io"
)

// requestTimeout caps one registry request.
const requestTimeout = 10 * time.Second

// maxBody caps one registry reply so a bad endpoint cannot exhaust memory.
const maxBody = 8 << 20

// Sentinel errors. The API maps each to a status code.
var (
	// ErrUnknownRegistry is a registry name not in the configured list.
	ErrUnknownRegistry = errors.New("unknown registry")
	// ErrNotFound is a server or version the registry does not know.
	ErrNotFound = errors.New("registry entry not found")
	// ErrUnsupportedPackage is a package type or source this client cannot install.
	ErrUnsupportedPackage = errors.New("unsupported package")
	// ErrUnsupportedRemote is a remote whose transport this client cannot use.
	ErrUnsupportedRemote = errors.New("unsupported remote")
	// ErrMissingInput is a required input with no value.
	ErrMissingInput = errors.New("missing required input")
	// ErrUnusedInput is a caller value that no input of the chosen source uses.
	ErrUnusedInput = errors.New("unused input")
	// ErrInvalidInput is a caller value that the chosen source cannot use.
	ErrInvalidInput = errors.New("invalid input")
	// ErrInvalidEntry is registry data that fails a local safety check.
	ErrInvalidEntry = errors.New("invalid registry entry")
)

// apiPrefixes are the registry API versions, newest first. A registry that
// returns 404 for the first gets the second.
var apiPrefixes = [...]string{"/v0.1", "/v0"} //nolint:gochecknoglobals // constant list, never mutated

// Config configures the client. A zero Config uses the official registry, a
// 10 s HTTP client, SafeClient for skill fetches, and the default logger.
type Config struct {
	Registries []config.Registry
	HTTP       *http.Client
	// SkillHTTP fetches contributed skills. Nil is SafeClient. Tests set it
	// to reach a loopback server.
	SkillHTTP *http.Client
	Log       *slog.Logger
	// CatalogKeys verify the Trebi catalog index. Nil is the embedded key.
	CatalogKeys []ed25519.PublicKey
}

// Service talks to one or more MCP registries. Build it with New; it is safe
// for concurrent use.
type Service struct {
	Registries []config.Registry
	HTTP       *http.Client
	Log        *slog.Logger

	skillHTTP   *http.Client
	catalogKeys []ed25519.PublicKey
	mu          sync.Mutex
	prefix      map[string]string       // registry URL → API prefix that answered
	docker      map[string]dockerLoaded // catalog URL → last good file
	trebi       map[string]trebiLoaded  // catalog URL → last good index
	sigErr      map[string]error        // catalog URL → last signature failure
	pages       map[string]cachedPage   // registry and query → last good page
}

// cachedPage is the last good page of one search, served stale when the
// source fails.
type cachedPage struct {
	at   time.Time
	page Page
}

// New builds a registry client.
func New(cfg Config) *Service {
	regs := cfg.Registries
	if len(regs) == 0 {
		regs = config.DefaultRegistries()
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	skill := cfg.SkillHTTP
	if skill == nil {
		skill = SafeClient(requestTimeout)
	}
	keys := cfg.CatalogKeys
	if keys == nil {
		keys = catalogKeys()
	}
	return &Service{
		Registries: regs, HTTP: hc, Log: log, skillHTTP: skill, catalogKeys: keys,
		prefix: map[string]string{}, docker: map[string]dockerLoaded{}, trebi: map[string]trebiLoaded{},
		sigErr: map[string]error{}, pages: map[string]cachedPage{},
	}
}

// Source is one registry API. Each kind maps its wire shape to Entry.
type Source interface {
	Search(ctx context.Context, q Query) (Page, error)
	Get(ctx context.Context, name, version string) (Entry, error)
}

// Query is one page request to one registry. Cursor is the value of the last
// Page.Next.
type Query struct {
	Text   string
	Cursor string
	Limit  int
}

// Page is one page from one registry. Stale marks a page from the cache
// after the source failed.
type Page struct {
	Entries []Entry
	Next    string
	Stale   bool
}

// Trust levels of an entry.
const (
	TrustOfficial  = "official"
	TrustVerified  = "verified"
	TrustCommunity = "community"
)

// SearchOpts is one catalog search. An empty Registry queries every
// configured registry in order.
type SearchOpts struct {
	Query    string
	Registry string
	Cursor   string
	Limit    int
}

// Entry is one server from one registry. Catalog is set for a Trebi
// catalog entry, which installs from its snapshot and not from Server.
type Entry struct {
	Registry string
	Server   ServerJSON
	Meta     map[string]any // the response-level _meta
	Trust    string
	Catalog  *IndexEntry
}

// SearchResult is one merged catalog page. Errors lists each registry that
// failed while the others answered. Stale lists each registry whose page
// came from the cache.
type SearchResult struct {
	Entries []Entry
	Next    string
	Errors  []RegistryError
	Stale   []string
}

// RegistryError is one registry that failed in a merged search.
type RegistryError struct {
	Registry string `json:"registry"`
	Error    string `json:"error"`
}

// Search returns the merged pages of the selected registries. The next cursor
// encodes one cursor per registry that has more. A continuation skips each
// registry that has no key in the cursor. One failing registry does not fail
// the search; it fails only when every selected registry fails.
func (s *Service) Search(ctx context.Context, o SearchOpts) (SearchResult, error) {
	regs, err := s.selectRegistries(o.Registry)
	if err != nil {
		return SearchResult{}, err
	}
	positions, asJSON := decodeCursor(o.Cursor)
	var res SearchResult
	next := map[string]string{}
	var firstErr error
	queried := 0
	for _, reg := range regs {
		pos := ""
		switch {
		case asJSON && o.Cursor != "":
			p, ok := positions[reg.Name]
			if !ok {
				continue // this registry has no more pages
			}
			pos = p
		case !asJSON && o.Registry != "":
			pos = o.Cursor // a bare cursor applies to the named registry
		}
		queried++
		page, err := s.searchCached(ctx, reg, Query{Text: o.Query, Cursor: pos, Limit: o.Limit})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			res.Errors = append(res.Errors, RegistryError{Registry: reg.Name, Error: err.Error()})
			if pos != "" {
				next[reg.Name] = pos // retry the same page next time
			}
			continue
		}
		res.Entries = append(res.Entries, page.Entries...)
		if page.Stale {
			res.Stale = append(res.Stale, reg.Name)
		}
		if page.Next != "" {
			next[reg.Name] = page.Next
		}
	}
	if queried > 0 && len(res.Errors) == queried {
		return SearchResult{}, firstErr
	}
	res.Next = encodeCursor(next)
	return res, nil
}

// searchCached runs one source search. A good page is kept for catalogTTL;
// when the source fails, the kept page comes back with Stale set, so one
// failing source does not empty the catalog.
func (s *Service) searchCached(ctx context.Context, reg config.Registry, q Query) (Page, error) {
	key := reg.Name + "\x00" + reg.URL + "\x00" + q.Text + "\x00" + q.Cursor + "\x00" + strconv.Itoa(q.Limit)
	page, err := s.Source(reg).Search(ctx, q)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if c, ok := s.pages[key]; ok && time.Since(c.at) < catalogTTL {
			s.Log.Warn("catalog search", "registry", reg.Name, "error", err)
			c.page.Stale = true
			return c.page, nil
		}
		return Page{}, err
	}
	for k, c := range s.pages {
		if time.Since(c.at) >= catalogTTL {
			delete(s.pages, k)
		}
	}
	s.pages[key] = cachedPage{at: time.Now(), page: page}
	return page, nil
}

// Get reads one server version; an empty version asks for the latest. An
// empty registry asks each registry in order, so a Trebi entry wins over an
// MCP registry entry with the same name.
func (s *Service) Get(ctx context.Context, registryName, name, version string) (Entry, error) {
	if strings.TrimSpace(name) == "" {
		return Entry{}, fmt.Errorf("%w: name required", ErrNotFound)
	}
	if strings.TrimSpace(registryName) != "" {
		reg, err := s.pick(registryName)
		if err != nil {
			return Entry{}, err
		}
		return s.Source(reg).Get(ctx, name, version)
	}
	var firstErr error
	for _, reg := range s.Registries {
		e, err := s.Source(reg).Get(ctx, name, version)
		if err == nil {
			return e, nil
		}
		if firstErr == nil || errors.Is(firstErr, ErrNotFound) && !errors.Is(err, ErrNotFound) {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return Entry{}, firstErr
}

// Source returns the API client for reg by its kind.
func (s *Service) Source(reg config.Registry) Source {
	switch reg.KindOrDefault() {
	case config.RegistryTrebi:
		return trebiCatalog{s: s, reg: reg}
	case config.RegistrySmithery:
		return smithery{s: s, reg: reg}
	case config.RegistryDocker:
		return dockerCatalog{s: s, reg: reg}
	default:
		return mcpRegistry{s: s, reg: reg}
	}
}

// FetchSkill reads the SKILL.md body of a contributed skill through the skill
// client, which is SafeClient unless a test replaces it.
func (s *Service) FetchSkill(ctx context.Context, raw string) (string, error) {
	body, err := readCapped(ctx, s.skillHTTP, raw, maxSkillBody)
	if err != nil {
		return "", fmt.Errorf("skill url: %w", err)
	}
	return string(body), nil
}

// selectRegistries returns the named registry, or every registry in order.
func (s *Service) selectRegistries(name string) ([]config.Registry, error) {
	if strings.TrimSpace(name) == "" {
		return s.Registries, nil
	}
	reg, err := s.pick(name)
	if err != nil {
		return nil, err
	}
	return []config.Registry{reg}, nil
}

// pick resolves one registry by name. An empty name is the default registry:
// the built-in "official" when present, else the first configured.
func (s *Service) pick(name string) (config.Registry, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		for _, r := range s.Registries {
			if r.Name == DefaultRegistryName {
				return r, nil
			}
		}
		if len(s.Registries) == 0 {
			return config.Registry{Name: DefaultRegistryName, URL: DefaultRegistryURL}, nil
		}
		return s.Registries[0], nil
	}
	for _, r := range s.Registries {
		if r.Name == name {
			return r, nil
		}
	}
	return config.Registry{}, fmt.Errorf("%w: %q", ErrUnknownRegistry, name)
}

// getJSON runs one GET against a registry and decodes the reply. The path has
// no API version. The first call tries /v0.1, then /v0 on a 404, and keeps
// the version that answered for the next calls.
func (s *Service) getJSON(ctx context.Context, reg config.Registry, path string, q url.Values, out any) error {
	s.mu.Lock()
	known, ok := s.prefix[reg.URL]
	s.mu.Unlock()
	if ok {
		return s.fetchJSON(ctx, reg, known+path, q, out)
	}
	var firstErr error
	for _, p := range apiPrefixes {
		err := s.fetchJSON(ctx, reg, p+path, q, out)
		if err == nil {
			s.mu.Lock()
			s.prefix[reg.URL] = p
			s.mu.Unlock()
			return nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if !errors.Is(err, ErrNotFound) {
			break
		}
	}
	return firstErr
}

// fetchJSON runs one GET on one full API path.
func (s *Service) fetchJSON(ctx context.Context, reg config.Registry, path string, q url.Values, out any) error {
	endpoint := strings.TrimRight(reg.URL, "/") + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	body, err := s.fetch(ctx, reg, endpoint, "application/json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("registry %s: decode %s: %w", reg.Name, path, err)
	}
	return nil
}

// fetch runs one GET and returns the body, capped at maxBody.
func (s *Service) fetch(ctx context.Context, reg config.Registry, endpoint, accept string) ([]byte, error) {
	return s.fetchCapped(ctx, reg, endpoint, accept, maxBody)
}

// fetchCapped runs one GET and returns the body, capped at limit.
func (s *Service) fetchCapped(ctx context.Context, reg config.Registry, endpoint, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", reg.Name, err)
	}
	req.Header.Set("Accept", accept)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", reg.Name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", reg.Name, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s %s", ErrNotFound, reg.Name, strings.TrimPrefix(endpoint, strings.TrimRight(reg.URL, "/")))
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("registry %s: %s: %s", reg.Name, resp.Status, snippet(body))
	}
	return body, nil
}

// mcpRegistry is a registry that speaks the official MCP Registry API.
type mcpRegistry struct {
	s   *Service
	reg config.Registry
}

func (m mcpRegistry) Search(ctx context.Context, q Query) (Page, error) {
	v := url.Values{}
	if strings.TrimSpace(q.Text) != "" {
		v.Set("search", q.Text)
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(min(q.Limit, 100)))
	}
	var out listResponse
	if err := m.s.getJSON(ctx, m.reg, "/servers", v, &out); err != nil {
		return Page{}, err
	}
	page := Page{Next: out.Metadata.NextCursor}
	for _, it := range out.Servers {
		page.Entries = append(page.Entries, Entry{Registry: m.reg.Name, Server: it.Server, Meta: it.Meta, Trust: m.trust()})
	}
	return page, nil
}

// Get reads one version; an empty version asks for the latest.
func (m mcpRegistry) Get(ctx context.Context, name, version string) (Entry, error) {
	v := strings.TrimSpace(version)
	if v == "" {
		v = "latest"
	}
	path := "/servers/" + url.PathEscape(name) + "/versions/" + url.PathEscape(v)
	var out getResponse
	if err := m.s.getJSON(ctx, m.reg, path, nil, &out); err != nil {
		return Entry{}, err
	}
	return Entry{Registry: m.reg.Name, Server: out.Server, Meta: out.Meta, Trust: m.trust()}, nil
}

// trust is official for the official registry and community for any other.
func (m mcpRegistry) trust() string {
	if strings.TrimRight(m.reg.URL, "/") == DefaultRegistryURL {
		return TrustOfficial
	}
	return TrustCommunity
}

// snippet shortens a body for an error message.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// decodeCursor parses the encoded per-registry cursor. A non-JSON value is a
// bare cursor (asJSON false).
func decodeCursor(raw string) (map[string]string, bool) {
	if raw == "" {
		return map[string]string{}, true
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, false
	}
	return m, true
}

// encodeCursor renders the per-registry cursors as a JSON object. No cursor
// at all encodes as "".
func encodeCursor(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// ---- Registry wire shapes (only the fields this plan reads) ----

// listResponse is GET /v0.1/servers.
type listResponse struct {
	Servers []struct {
		Server ServerJSON     `json:"server"`
		Meta   map[string]any `json:"_meta"`
	} `json:"servers"`
	Metadata struct {
		NextCursor string `json:"nextCursor"`
		Count      int    `json:"count"`
	} `json:"metadata"`
}

// getResponse is GET /v0.1/servers/{name}/versions/{version}.
type getResponse struct {
	Server ServerJSON     `json:"server"`
	Meta   map[string]any `json:"_meta"`
}

// ServerJSON is one server.json entry. Unknown fields are ignored.
type ServerJSON struct {
	Schema      string         `json:"$schema"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Title       string         `json:"title"`
	Version     string         `json:"version"`
	Remotes     []Remote       `json:"remotes"`
	Packages    []Package      `json:"packages"`
	Repository  *Repository    `json:"repository"`
	Meta        map[string]any `json:"_meta"`
}

// Repository is the optional source metadata.
type Repository struct {
	URL    string `json:"url"`
	Source string `json:"source"`
	ID     string `json:"id,omitempty"`
}

// Remote is one remote transport. Headers is the registry's KeyValueInput
// list; Variables fill {placeholders} in the URL.
type Remote struct {
	Type      string           `json:"type"`
	URL       string           `json:"url"`
	Headers   []KeyValue       `json:"headers,omitempty"`
	Variables map[string]Input `json:"variables,omitempty"`
}

// Package is one package entry.
type Package struct {
	RegistryType         string     `json:"registryType"`
	RegistryBaseURL      string     `json:"registryBaseUrl"`
	Identifier           string     `json:"identifier"`
	Version              string     `json:"version"`
	RuntimeHint          string     `json:"runtimeHint"`
	Transport            *Transport `json:"transport"`
	RuntimeArguments     []Argument `json:"runtimeArguments,omitempty"`
	PackageArguments     []Argument `json:"packageArguments"`
	EnvironmentVariables []KeyValue `json:"environmentVariables"`
}

// Transport is the package transport object; only its type is read.
type Transport struct {
	Type string `json:"type"`
}

// Input is the registry Input shape. An env variable, a header, an argument,
// and a template variable all embed it. Value may hold {name} placeholders
// that Variables fill.
type Input struct {
	Value       string           `json:"value"`
	Default     string           `json:"default,omitempty"`
	Description string           `json:"description"`
	Format      string           `json:"format"`
	Placeholder string           `json:"placeholder,omitempty"`
	Choices     []string         `json:"choices,omitempty"`
	IsRequired  bool             `json:"isRequired"`
	IsSecret    bool             `json:"isSecret"`
	Variables   map[string]Input `json:"variables,omitempty"`
}

// Argument is one package or runtime argument: positional or named.
type Argument struct {
	Type       string `json:"type"` // positional | named
	Name       string `json:"name"` // named
	ValueHint  string `json:"valueHint"`
	IsRepeated bool   `json:"isRepeated,omitempty"`
	Input
}

// KeyValue is one environment variable or header input.
type KeyValue struct {
	Name string `json:"name"`
	Input
}

// Live is a registry client that follows connectors.registries in the
// config. A change takes effect on the next read, so a local registry can
// start after the daemon does. The client is swapped atomically.
type Live struct {
	cur   atomic.Pointer[Service]
	fixed bool
	log   *slog.Logger
}

// NewLive builds a Live client. A non-nil fixed client never swaps; tests
// use it.
func NewLive(fixed *Service) *Live {
	l := &Live{fixed: fixed != nil, log: slog.Default().With("source", "registry")}
	if fixed == nil {
		fixed = New(Config{Registries: liveRegistries(nil), Log: l.log})
	}
	l.cur.Store(fixed)
	return l
}

// liveRegistries reads the configured list, or keeps last on a bad config.
func liveRegistries(last []config.Registry) []config.Registry {
	cfg, err := config.Load()
	if err != nil {
		return last
	}
	if len(cfg.Connectors.Registries) == 0 {
		return config.DefaultRegistries()
	}
	return cfg.Connectors.Registries
}

// Current returns the client for the current config.
func (l *Live) Current() *Service {
	cur := l.cur.Load()
	if l.fixed {
		return cur
	}
	regs := liveRegistries(cur.Registries)
	if slices.Equal(regs, cur.Registries) {
		return cur
	}
	next := New(Config{Registries: regs, Log: l.log})
	if l.cur.CompareAndSwap(cur, next) {
		return next
	}
	return l.cur.Load()
}

// Versions lists the catalog versions of one connector.
func (l *Live) Versions(ctx context.Context, name string) ([]IndexVersion, error) {
	return l.Current().Versions(ctx, name)
}

// Snapshot downloads one checked snapshot.
func (l *Live) Snapshot(ctx context.Context, name, version string) ([]byte, error) {
	return l.Current().Snapshot(ctx, name, version)
}
