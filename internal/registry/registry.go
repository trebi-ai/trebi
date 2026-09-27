// Package registry is the client for MCP registries that implement the
// official MCP Registry API. It searches the configured registries and turns
// one server entry into a connector definition (Part H).
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
var apiPrefixes = [...]string{"/v0.1", "/v0"}

// Config configures the client. A zero Config uses the official registry, a
// 10 s HTTP client, SafeClient for skill fetches, and the default logger.
type Config struct {
	Registries []config.Registry
	HTTP       *http.Client
	// SkillHTTP fetches contributed skills. Nil is SafeClient. Tests set it
	// to reach a loopback server.
	SkillHTTP *http.Client
	Log       *slog.Logger
}

// Service talks to one or more MCP registries. Build it with New; it is safe
// for concurrent use.
type Service struct {
	Registries []config.Registry
	HTTP       *http.Client
	Log        *slog.Logger

	skillHTTP *http.Client
	mu        sync.Mutex
	prefix    map[string]string // registry URL → API prefix that answered
}

// New builds a registry client.
func New(cfg Config) *Service {
	regs := cfg.Registries
	if len(regs) == 0 {
		regs = []config.Registry{{Name: DefaultRegistryName, URL: DefaultRegistryURL}}
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
	return &Service{Registries: regs, HTTP: hc, Log: log, skillHTTP: skill, prefix: map[string]string{}}
}

// SearchOpts is one catalog search. An empty Registry queries every
// configured registry in order.
type SearchOpts struct {
	Query    string
	Registry string
	Cursor   string
	Limit    int
}

// Entry is one server from one registry.
type Entry struct {
	Registry string
	Server   ServerJSON
	Meta     map[string]any // the response-level _meta
}

// SearchResult is one merged catalog page. Errors lists each registry that
// failed while the others answered.
type SearchResult struct {
	Entries []Entry
	Next    string
	Errors  []RegistryError
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
		page, err := s.listPage(ctx, reg, o.Query, o.Limit, pos)
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
		for _, it := range page.Servers {
			res.Entries = append(res.Entries, Entry{Registry: reg.Name, Server: it.Server, Meta: it.Meta})
		}
		if page.Metadata.NextCursor != "" {
			next[reg.Name] = page.Metadata.NextCursor
		}
	}
	if queried > 0 && len(res.Errors) == queried {
		return SearchResult{}, firstErr
	}
	res.Next = encodeCursor(next)
	return res, nil
}

// Get reads one server version. An empty registry uses the default; an empty
// version asks the registry for the latest.
func (s *Service) Get(ctx context.Context, registryName, name, version string) (Entry, error) {
	if strings.TrimSpace(name) == "" {
		return Entry{}, fmt.Errorf("%w: name required", ErrNotFound)
	}
	reg, err := s.pick(registryName)
	if err != nil {
		return Entry{}, err
	}
	v := strings.TrimSpace(version)
	if v == "" {
		v = "latest"
	}
	path := "/servers/" + url.PathEscape(name) + "/versions/" + url.PathEscape(v)
	var out getResponse
	if err := s.getJSON(ctx, reg, path, nil, &out); err != nil {
		return Entry{}, err
	}
	return Entry{Registry: reg.Name, Server: out.Server, Meta: out.Meta}, nil
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

func (s *Service) listPage(ctx context.Context, reg config.Registry, query string, limit int, cursor string) (listResponse, error) {
	q := url.Values{}
	if strings.TrimSpace(query) != "" {
		q.Set("search", query)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		if limit > 100 {
			limit = 100
		}
		q.Set("limit", strconv.Itoa(limit))
	}
	var out listResponse
	if err := s.getJSON(ctx, reg, "/servers", q, &out); err != nil {
		return out, err
	}
	return out, nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("registry %s: %w", reg.Name, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("registry %s: %w", reg.Name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("registry %s: %w", reg.Name, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s %s", ErrNotFound, reg.Name, path)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("registry %s: %s: %s", reg.Name, resp.Status, snippet(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("registry %s: decode %s: %w", reg.Name, path, err)
	}
	return nil
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
