package registry

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"gopkg.in/yaml.v3"
)

// dockerTTL is how long the client keeps a Docker catalog file.
const dockerTTL = 10 * time.Minute

// dockerTemplateRE matches a {{name}} value in the Docker catalog.
var dockerTemplateRE = regexp.MustCompile(`\{\{\s*([^{}\s]+)\s*\}\}`)

// envRefRE matches a ${NAME} value in a Docker remote header.
var envRefRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// dockerCatalog reads the Docker MCP Catalog. The registry URL is the
// catalog file; search filters it locally. Docker curates the catalog, so
// every entry is verified. A "poci" entry has no install source.
type dockerCatalog struct {
	s   *Service
	reg config.Registry
}

// dockerLoaded is one cached catalog file.
type dockerLoaded struct {
	at      time.Time
	ids     []string
	servers map[string]dockerServer
}

// Search matches the text in the id, title, and description. The cursor is
// an offset.
func (d dockerCatalog) Search(ctx context.Context, q Query) (Page, error) {
	cat, err := d.load(ctx)
	if err != nil {
		return Page{}, err
	}
	text := strings.ToLower(strings.TrimSpace(q.Text))
	var hits []string
	for _, id := range cat.ids {
		s := cat.servers[id]
		if text == "" || strings.Contains(strings.ToLower(id+"\n"+s.Title+"\n"+s.Description), text) {
			hits = append(hits, id)
		}
	}
	from, _ := strconv.Atoi(q.Cursor)
	from = max(0, min(from, len(hits)))
	limit := q.Limit
	if limit <= 0 {
		limit = 30
	}
	to := min(from+limit, len(hits))
	page := Page{}
	for _, id := range hits[from:to] {
		page.Entries = append(page.Entries, d.entry(id, cat.servers[id]))
	}
	if to < len(hits) {
		page.Next = strconv.Itoa(to)
	}
	return page, nil
}

// Get reads one server by id. version is ignored: the image is pinned by
// digest.
func (d dockerCatalog) Get(ctx context.Context, name, _ string) (Entry, error) {
	cat, err := d.load(ctx)
	if err != nil {
		return Entry{}, err
	}
	s, ok := cat.servers[name]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %s %s", ErrNotFound, d.reg.Name, name)
	}
	return d.entry(name, s), nil
}

// load returns the catalog, from the cache while it is fresh.
func (d dockerCatalog) load(ctx context.Context) (dockerLoaded, error) {
	d.s.mu.Lock()
	cached, ok := d.s.docker[d.reg.URL]
	d.s.mu.Unlock()
	if ok && time.Since(cached.at) < dockerTTL {
		return cached, nil
	}
	body, err := d.s.fetch(ctx, d.reg, d.reg.URL, "application/yaml, text/yaml, */*")
	if err != nil {
		return dockerLoaded{}, err
	}
	var file struct {
		Registry map[string]dockerServer `yaml:"registry"`
	}
	if err := yaml.Unmarshal(body, &file); err != nil {
		return dockerLoaded{}, fmt.Errorf("registry %s: decode catalog: %w", d.reg.Name, err)
	}
	loaded := dockerLoaded{at: time.Now(), servers: file.Registry}
	for id := range file.Registry {
		loaded.ids = append(loaded.ids, id)
	}
	slices.Sort(loaded.ids)
	d.s.mu.Lock()
	d.s.docker[d.reg.URL] = loaded
	d.s.mu.Unlock()
	return loaded, nil
}

// entry maps one catalog server. A server is an OCI stdio package; a remote
// is an HTTP remote. Secrets become secret env inputs, and {{name}} or
// ${NAME} values become required inputs.
func (d dockerCatalog) entry(id string, s dockerServer) Entry {
	srv := ServerJSON{Name: id, Title: s.Title, Description: s.Description}
	if s.Upstream != "" {
		srv.Repository = &Repository{URL: s.Upstream, Source: "github"}
	}
	secretEnv := map[string]bool{}
	for _, sec := range s.Secrets {
		secretEnv[sec.Env] = true
	}
	switch s.Type {
	case "server":
		if s.Image == "" {
			break
		}
		p := Package{RegistryType: "oci", Identifier: s.Image, Transport: &Transport{Type: "stdio"}}
		for _, sec := range s.Secrets {
			p.EnvironmentVariables = append(p.EnvironmentVariables, KeyValue{Name: sec.Env, Input: Input{
				Description: sec.Description, Placeholder: sec.Example, IsRequired: true, IsSecret: true,
			}})
		}
		for _, e := range s.Env {
			p.EnvironmentVariables = append(p.EnvironmentVariables, KeyValue{Name: e.Name, Input: dockerInput(e.Value, dockerTemplateRE, secretEnv)})
		}
		for _, arg := range s.Command {
			p.PackageArguments = append(p.PackageArguments, Argument{Type: "positional", Input: dockerInput(arg, dockerTemplateRE, secretEnv)})
		}
		srv.Packages = []Package{p}
	case "remote":
		if s.Remote.URL == "" {
			break
		}
		rm := Remote{Type: s.Remote.TransportType, URL: s.Remote.URL}
		for _, k := range slices.Sorted(maps.Keys(s.Remote.Headers)) {
			rm.Headers = append(rm.Headers, KeyValue{Name: k, Input: dockerInput(s.Remote.Headers[k], envRefRE, secretEnv)})
		}
		srv.Remotes = []Remote{rm}
	}
	return Entry{Registry: d.reg.Name, Server: srv, Trust: TrustVerified}
}

// dockerInput turns a catalog value into a registry Input. Each match of re
// becomes a {name} variable that the caller fills.
func dockerInput(value string, re *regexp.Regexp, secretEnv map[string]bool) Input {
	if !re.MatchString(value) {
		return Input{Value: value}
	}
	in := Input{Variables: map[string]Input{}}
	in.Value = re.ReplaceAllStringFunc(value, func(m string) string {
		name := re.FindStringSubmatch(m)[1]
		in.Variables[name] = Input{IsRequired: true, IsSecret: secretEnv[name], Description: name}
		return "{" + name + "}"
	})
	return in
}

// dockerServer is one entry of the catalog file.
type dockerServer struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	Type        string `yaml:"type"`
	Image       string `yaml:"image"`
	Upstream    string `yaml:"upstream"`
	Remote      struct {
		TransportType string            `yaml:"transport_type"`
		URL           string            `yaml:"url"`
		Headers       map[string]string `yaml:"headers"`
	} `yaml:"remote"`
	Secrets []struct {
		Name        string `yaml:"name"`
		Env         string `yaml:"env"`
		Example     string `yaml:"example"`
		Description string `yaml:"description"`
	} `yaml:"secrets"`
	Env []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
	Command []string `yaml:"command"`
}
