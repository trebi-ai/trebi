// Package guides embeds the workflow cheat sheets served by the CLI, API, and MCP.
package guides

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/adrg/frontmatter"
	"gopkg.in/yaml.v3"
)

//go:embed *.md
var files embed.FS

// ErrNotFound is returned by Get for an unknown topic.
var ErrNotFound = errors.New("not found")

// Guide is one workflow cheat sheet. List omits Body; Get includes it.
type Guide struct {
	Topic       string
	Title       string
	Description string
	Body        string
}

type meta struct {
	Topic       string `yaml:"topic"`
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
}

var cache struct { //nolint:gochecknoglobals // static embed, parsed once
	once    sync.Once
	index   []Guide
	byTopic map[string]Guide
	err     error
}

// List returns the topic index, sorted by topic, without bodies.
func List() []Guide {
	load()
	out := make([]Guide, len(cache.index))
	copy(out, cache.index)
	return out
}

// Get returns one guide by topic, including the body.
func Get(topic string) (Guide, error) {
	load()
	if cache.err != nil {
		return Guide{}, cache.err
	}
	g, ok := cache.byTopic[topic]
	if !ok {
		return Guide{}, fmt.Errorf("%w: %s", ErrNotFound, topic)
	}
	return g, nil
}

// LoadError is the corpus parse error, if any. Tests call this to fail eager.
func LoadError() error {
	load()
	return cache.err
}

// ParseAll parses every embedded file and returns them (with bodies), sorted.
// Production is lazy; tests use this to fail on any frontmatter error.
func ParseAll() ([]Guide, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	var out []Guide
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := files.ReadFile(e.Name())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		g, err := parseGuide(e.Name(), data)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out, nil
}

func load() {
	cache.once.Do(func() {
		cache.byTopic = map[string]Guide{}
		entries, err := fs.ReadDir(files, ".")
		if err != nil {
			cache.err = err
			return
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			data, err := files.ReadFile(e.Name())
			if err != nil {
				cache.err = fmt.Errorf("%s: %w", e.Name(), err)
				return
			}
			g, err := parseGuide(e.Name(), data)
			if err != nil {
				cache.err = err
				return
			}
			if _, ok := cache.byTopic[g.Topic]; ok {
				cache.err = fmt.Errorf("duplicate topic %q", g.Topic)
				return
			}
			cache.byTopic[g.Topic] = g
			cache.index = append(cache.index, Guide{Topic: g.Topic, Title: g.Title, Description: g.Description})
		}
		sort.Slice(cache.index, func(i, j int) bool { return cache.index[i].Topic < cache.index[j].Topic })
	})
}

func parseGuide(name string, data []byte) (Guide, error) {
	var m meta
	body, err := frontmatter.Parse(bytes.NewReader(data), &m, frontmatter.NewFormat("---", "---", yaml.Unmarshal))
	if err != nil {
		return Guide{}, fmt.Errorf("%s: %w", name, err)
	}
	g := Guide{
		Topic:       strings.TrimSpace(m.Topic),
		Title:       strings.TrimSpace(m.Title),
		Description: strings.TrimSpace(m.Description),
		Body:        strings.TrimSpace(string(body)) + "\n",
	}
	if g.Topic == "" || g.Title == "" || g.Description == "" {
		return Guide{}, fmt.Errorf("%s: topic, title, and description are required", name)
	}
	return g, nil
}
