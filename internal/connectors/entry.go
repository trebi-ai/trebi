package connectors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/secrets"
	"gopkg.in/yaml.v3"
)

// ErrInvalid marks an entry or manifest that failed validation. The API maps
// it to 400 validation.
var ErrInvalid = manifest.ErrInvalid

// FieldError is one validation problem on a named field.
type FieldError = manifest.FieldError

// ValidationError lists every field error of one entry or resolved manifest.
type ValidationError = manifest.ValidationError

// Entry sources (Part C.1).
const (
	FromCatalog = "catalog"
	FromInline  = "inline"
	fromFile    = "file:"
)

// Entry is one global connector entry in connectors.yaml: where its manifest
// comes from, the version and track, the setup input values, and a merge
// patch on the manifest.
type Entry struct {
	Name             string             `yaml:"-" json:"name"`
	From             string             `yaml:"from" json:"from"`
	Version          string             `yaml:"version,omitempty" json:"version,omitempty"`
	Track            manifest.Track     `yaml:"track,omitempty" json:"track,omitempty"`
	Inputs           map[string]string  `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Patch            map[string]any     `yaml:"patch,omitempty" json:"patch,omitempty"`
	Manifest         *manifest.Manifest `yaml:"manifest,omitempty" json:"manifest,omitempty"`
	InstalledSecrets []string           `yaml:"installed_secrets,omitempty" json:"installed_secrets,omitempty"`
}

// envKeyRE is an environment variable name.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// nameRE is the connector name rule of the manifest.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// clone returns a deep copy through the JSON form.
func (e Entry) clone() Entry {
	b, err := json.Marshal(e)
	if err != nil {
		return e
	}
	var out Entry
	if err := json.Unmarshal(b, &out); err != nil {
		return e
	}
	out.Name = e.Name
	return out
}

// FilePath returns the manifest path of a file: entry, or "".
func (e Entry) FilePath() string {
	p, ok := strings.CutPrefix(e.From, fromFile)
	if !ok {
		return ""
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	return p
}

// track returns the track with the default.
func (e Entry) track() manifest.Track {
	if e.Track == "" {
		return manifest.TrackPatch
	}
	return e.Track
}

// Validate checks the entry fields. The manifest is checked by Resolve.
func (e Entry) Validate() error {
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{Field: field, Message: msg}) }
	if !nameRE.MatchString(e.Name) {
		add("name", "must match "+nameRE.String())
	}
	if manifest.Reserved(e.Name) {
		add("name", "is reserved")
	}
	switch {
	case e.From == FromCatalog:
		if e.Version == "" {
			add("version", "required for from: catalog")
		}
		if e.Manifest != nil {
			add("manifest", "only for from: inline")
		}
	case e.From == FromInline:
		if e.Manifest == nil {
			add("manifest", "required for from: inline")
		}
	case strings.HasPrefix(e.From, fromFile):
		if e.FilePath() == "" {
			add("from", "file: needs a path")
		}
		if e.Manifest != nil {
			add("manifest", "only for from: inline")
		}
	default:
		add("from", "must be catalog, inline, or file:<path>")
	}
	if e.Version != "" {
		if _, err := manifest.ParseVersion(e.Version); err != nil {
			add("version", err.Error())
		}
	}
	if e.Track != "" && !e.Track.Valid() {
		add("track", "must be exact, patch, minor, or latest")
	}
	for k := range e.Inputs {
		if !envKeyRE.MatchString(k) {
			add("inputs", fmt.Sprintf("key %q must match %s", k, envKeyRE))
		}
	}
	for _, n := range e.InstalledSecrets {
		if err := secrets.CheckName(n); err != nil {
			add("installed_secrets", err.Error())
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Name: e.Name, Fields: errs}
}

// refersToSecret reports whether an input of e names the secret.
func (e Entry) refersToSecret(name string) bool {
	for _, v := range e.Inputs {
		if v == name || v == "secret:"+name {
			return true
		}
	}
	return false
}

// parseEntries decodes strict YAML data into named entries. An unknown key
// fails with the connector name and its line in the file.
func parseEntries(data []byte) (map[string]Entry, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := map[string]Entry{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	err := dec.Decode(&out)
	var te *yaml.TypeError
	switch {
	case err == nil, errors.Is(err, io.EOF):
	case errors.As(err, &te) && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode:
		// The whole-file decode keeps the file line; the top-level keys
		// name the connector that holds it.
		keys := doc.Content[0].Content
		msgs := make([]string, 0, len(te.Errors))
		for _, msg := range te.Errors {
			var line int
			if _, scanErr := fmt.Sscanf(msg, "line %d:", &line); scanErr == nil {
				for k := len(keys) - 2; k >= 0; k -= 2 {
					if keys[k].Line <= line {
						msg = fmt.Sprintf("connector %q: %s", keys[k].Value, msg)
						break
					}
				}
			}
			msgs = append(msgs, msg)
		}
		return nil, errors.New(strings.Join(msgs, "; "))
	default:
		return nil, err
	}
	for name, e := range out {
		e.Name = name
		out[name] = e
	}
	return out, nil
}

// readEntries reads and validates connectors.yaml. A missing file is an
// empty set.
func readEntries() (map[string]Entry, error) {
	path := home.ConnectorsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Entry{}, nil
		}
		return nil, err
	}
	entries, err := parseEntries(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, e := range entries {
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return entries, nil
}

// LoadManifests resolves the global layer of every entry without a Service.
// The doctor uses it. An entry that does not resolve is left out.
func LoadManifests() (map[string]manifest.Manifest, error) {
	entries, err := readEntries()
	out := map[string]manifest.Manifest{}
	for name, e := range entries {
		if r, rerr := resolveEntry(e, "", nil); rerr == nil {
			out[name] = r.Manifest
		}
	}
	return out, err
}

// sortedNames returns the map keys sorted.
func sortedNames[T any](m map[string]T) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// maskValue is the wire value of a literal that GET does not show.
const maskValue = "***"

// MaskInputs hides each literal value. A value that holds a secret reference
// stays visible, because it holds no secret.
func MaskInputs(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if v == "" || strings.HasPrefix(v, "secret:") {
			out[k] = v
			continue
		}
		out[k] = maskValue
	}
	return out
}

// keepMasked puts back the stored value of each input that comes back as the
// mask. A mask with no stored value is a validation error.
func keepMasked(name string, next, prev map[string]string) error {
	var fields []FieldError
	for k, v := range next {
		if v != maskValue {
			continue
		}
		stored, ok := prev[k]
		if !ok || stored == maskValue {
			fields = append(fields, FieldError{Field: "inputs", Message: fmt.Sprintf("%s: %q has no stored value", k, maskValue)})
			continue
		}
		next[k] = stored
	}
	if len(fields) > 0 {
		return &ValidationError{Name: name, Fields: fields}
	}
	return nil
}

func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// decodeLine parses one lines-protocol output line per the declared format.
// A json connector returns nil for a line that is not valid json.
func decodeLine(e *manifest.Events, line string) any {
	if e.Format == manifest.FormatJSON {
		// UseNumber keeps a large id exact for the id and identity paths.
		var m any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil || dec.More() {
			return nil
		}
		return m
	}
	return map[string]any{"line": line}
}

// lookupPath reads a dotted path from a decoded JSON value. A numeric
// segment indexes an array. It renders strings, numbers, and booleans for
// comparison; anything else misses.
func lookupPath(v any, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	cur := v
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return "", false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return "", false
			}
			cur = node[i]
		default:
			return "", false
		}
	}
	return identityString(cur), identityString(cur) != ""
}

// identityString renders a scalar for comparison; "" means absent or not a
// scalar.
func identityString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return ""
}

// orEmptySlice returns a non-nil slice for the wire.
func orEmptySlice[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return slices.Clone(in)
}
