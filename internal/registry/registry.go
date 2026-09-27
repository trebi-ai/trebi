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
	// ErrMissingEnv is a required environment variable with no value.
	ErrMissingEnv = errors.New("missing required environment variable")
	// ErrInvalidEntry is registry data that fails a local safety check.
	ErrInvalidEntry = errors.New("invalid registry entry")
)

// Config configures the client. A zero Config uses the official registry, a
// 10 s HTTP client, and the default logger.
type Config struct {
	Registries []config.Registry
	HTTP       *http.Client
	Log        *slog.Logger
}

// Service talks to one or more MCP registries.
type Service struct {
	Registries []config.Registry
	HTTP       *http.Client
	Log        *slog.Logger
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
	return &Service{Registries: regs, HTTP: hc, Log: log}
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

// Search returns the merged pages of the selected registries. The returned
// next cursor encodes one cursor per registry that has more.
func (s *Service) Search(ctx context.Context, o SearchOpts) ([]Entry, string, error) {
	regs, err := s.selectRegistries(o.Registry)
	if err != nil {
		return nil, "", err
	}
	cursors, asJSON := decodeCursor(o.Cursor)
	var entries []Entry
	next := map[string]string{}
	for _, reg := range regs {
		cur := ""
		switch {
		case asJSON:
			cur = cursors[reg.Name]
		case o.Registry != "":
			cur = o.Cursor // a bare cursor applies to the named registry
		}
		page, err := s.listPage(ctx, reg, o.Query, o.Limit, cur)
		if err != nil {
			return nil, "", err
		}
		for _, it := range page.Servers {
			entries = append(entries, Entry{Registry: reg.Name, Server: it.Server, Meta: it.Meta})
		}
		if page.Metadata.NextCursor != "" {
			next[reg.Name] = page.Metadata.NextCursor
		}
	}
	return entries, encodeCursor(next), nil
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
	path := "/v0.1/servers/" + url.PathEscape(name) + "/versions/" + url.PathEscape(v)
	var out getResponse
	if err := s.getJSON(ctx, reg, path, nil, &out); err != nil {
		return Entry{}, err
	}
	return Entry{Registry: reg.Name, Server: out.Server, Meta: out.Meta}, nil
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
	if err := s.getJSON(ctx, reg, "/v0.1/servers", q, &out); err != nil {
		return out, err
	}
	return out, nil
}

// getJSON runs one GET against a registry and decodes the reply.
func (s *Service) getJSON(ctx context.Context, reg config.Registry, path string, q url.Values, out any) error {
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

// Remote is one remote transport. Headers is the registry's KeyValueInput list.
type Remote struct {
	Type    string     `json:"type"`
	URL     string     `json:"url"`
	Headers []KeyValue `json:"headers,omitempty"`
}

// Package is one package entry.
type Package struct {
	RegistryType         string     `json:"registryType"`
	RegistryBaseURL      string     `json:"registryBaseUrl"`
	Identifier           string     `json:"identifier"`
	Version              string     `json:"version"`
	RuntimeHint          string     `json:"runtimeHint"`
	Transport            *Transport `json:"transport"`
	PackageArguments     []Argument `json:"packageArguments"`
	EnvironmentVariables []KeyValue `json:"environmentVariables"`
}

// Transport is the package transport object; only its type is read.
type Transport struct {
	Type string `json:"type"`
}

// Argument is one package argument: positional or named.
type Argument struct {
	Type      string `json:"type"` // positional | named
	Name      string `json:"name"` // named
	Value     string `json:"value"`
	ValueHint string `json:"valueHint"`
}

// KeyValue is one environment variable or header input.
type KeyValue struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description"`
	Format      string `json:"format"`
	IsRequired  bool   `json:"isRequired"`
	IsSecret    bool   `json:"isSecret"`
}
