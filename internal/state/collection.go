package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// Collection is the frontmatter declaration of one collection.
type Collection struct {
	Description string         `yaml:"description" json:"description,omitempty"`
	TTL         string         `yaml:"ttl"         json:"ttl,omitempty"` // "90d", "2y"
	Schema      map[string]any `yaml:"schema"      json:"schema,omitempty"`
	resolved    *jsonschema.Resolved
}

var collectionRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)

// ParseCollections validates names, TTLs, and schemas. job.ParseBytes calls it
// with the decoded `state:` value. A nil value gives a nil map.
func ParseCollections(raw any) (map[string]Collection, error) {
	if raw == nil {
		return nil, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("state: must be a map of collection names")
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string]Collection, len(m))
	for _, name := range names {
		c, err := parseCollection(name, m[name])
		if err != nil {
			return nil, fmt.Errorf("state: collection %q: %w", name, err)
		}
		out[name] = c
	}
	return out, nil
}

func parseCollection(name string, raw json.RawMessage) (Collection, error) {
	if !collectionRE.MatchString(name) {
		return Collection{}, fmt.Errorf("name must match %s", collectionRE)
	}
	var c Collection
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return Collection{}, fmt.Errorf("fields are description, ttl, and schema: %w", err)
		}
	}
	if c.TTL != "" {
		if _, ok := parseSpan(c.TTL); !ok {
			return Collection{}, fmt.Errorf("ttl %q: use a number and h, d, w, mo, or y (for example 90d or 2y)", c.TTL)
		}
	}
	if err := c.resolve(); err != nil {
		return Collection{}, err
	}
	return c, nil
}

// resolve compiles the schema once. A schema type other than object is an error.
func (c *Collection) resolve() error {
	if c.Schema == nil || c.resolved != nil {
		return nil
	}
	if t, ok := c.Schema["type"]; ok && t != "object" {
		return fmt.Errorf("schema: type must be object")
	}
	b, err := json.Marshal(c.Schema)
	if err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	var sch jsonschema.Schema
	if err := json.Unmarshal(b, &sch); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	res, err := sch.Resolve(nil)
	if err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	c.resolved = res
	return nil
}

// check validates a decoded value against the schema. It returns the field
// path ("value.topic") and the message of the first failure.
func (c *Collection) check(v any) (path, msg string, ok bool) {
	if c.Schema == nil {
		return "", "", true
	}
	if err := c.resolve(); err != nil {
		return "value", err.Error(), false
	}
	err := c.resolved.Validate(v)
	if err == nil {
		return "", "", true
	}
	path, msg = schemaError(err.Error())
	if path == "" {
		return "value", msg, false
	}
	return "value." + path, msg, false
}

// properties returns the schema property names, sorted.
func (c *Collection) properties() []string {
	props, _ := c.Schema["properties"].(map[string]any)
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// schemaError turns a jsonschema-go message into a dotted field path and a text.
func schemaError(msg string) (field, text string) {
	const marker = "validating /properties/"
	i := strings.LastIndex(msg, marker)
	if i < 0 {
		return "", msg
	}
	path, text, _ := strings.Cut(msg[i+len("validating "):], ": ")
	var segs []string
	for _, s := range strings.Split(path, "/") {
		switch s {
		case "", "properties", "items", "additionalProperties":
			continue
		}
		segs = append(segs, s)
	}
	return strings.Join(segs, "."), text
}
