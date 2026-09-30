package connectors

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

// ErrInvalid marks a connection or manifest that failed validation. The API
// maps it to 400 validation.
var ErrInvalid = manifest.ErrInvalid

// FieldError is one validation problem on a named field.
type FieldError = manifest.FieldError

// ValidationError lists every field error of one connection or resolved
// manifest.
type ValidationError = manifest.ValidationError

// Connection sources.
const (
	FromCatalog = "catalog"
	FromInline  = "inline"
	fromFile    = "file:"
)

// Native connections have no folder. Their id, slug, and type are the same
// fixed word.
const (
	NativeEmail   = "email"
	NativeWebhook = "webhook"
)

// idPrefix starts every connection id.
const idPrefix = "con_"

// maxTitle caps the display name of a connection.
const maxTitle = 60

// Connection is one account that the user adds: a connector type from the
// catalog, a file, or an inline manifest, with its own inputs. The id never
// changes. Config files refer to the slug; DB rows hold the id.
type Connection struct {
	ID       string             `yaml:"id" json:"id"`
	Type     string             `yaml:"type" json:"type"`
	Slug     string             `yaml:"slug" json:"slug"`
	Title    string             `yaml:"title,omitempty" json:"title,omitempty"`
	From     string             `yaml:"from" json:"from"`
	Version  string             `yaml:"version,omitempty" json:"version,omitempty"`
	Digest   string             `yaml:"digest,omitempty" json:"digest,omitempty"`
	Track    manifest.Track     `yaml:"track,omitempty" json:"track,omitempty"`
	Inputs   map[string]string  `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Patch    map[string]any     `yaml:"patch,omitempty" json:"patch,omitempty"`
	Manifest *manifest.Manifest `yaml:"manifest,omitempty" json:"manifest,omitempty"`
}

var (
	// idRE is con_ plus a lowercase ULID.
	idRE = regexp.MustCompile(`^con_[0-9a-hjkmnp-tv-z]{26}$`)
	// nameRE is the connector type rule of the manifest, and the slug rule.
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	// envKeyRE is an environment variable name.
	envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// NewConnectionID returns a new con_ id.
func NewConnectionID() string {
	return idPrefix + strings.ToLower(ulid.Make().String())
}

// IsConnectionID reports whether ref has the form of a connection id.
func IsConnectionID(ref string) bool { return idRE.MatchString(ref) }

// CheckSlug checks the slug rule.
func CheckSlug(slug string) error {
	switch {
	case !nameRE.MatchString(slug):
		return fmt.Errorf("must match %s", nameRE)
	case strings.HasPrefix(slug, idPrefix):
		return fmt.Errorf("must not start with %s", idPrefix)
	case manifest.Reserved(slug):
		return fmt.Errorf("%q is reserved", slug)
	}
	return nil
}

// FilePath returns the manifest path of a file: connection, or "".
func (c Connection) FilePath() string {
	p, ok := strings.CutPrefix(c.From, fromFile)
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

// DisplayTitle is the title, else the fallback (the manifest title).
func (c Connection) DisplayTitle(fallback string) string {
	if c.Title != "" {
		return c.Title
	}
	return fallback
}

// Validate checks the record fields. Resolve checks the manifest.
func (c Connection) Validate() error {
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{Field: field, Message: msg}) }
	if !idRE.MatchString(c.ID) {
		add("id", "must match "+idRE.String())
	}
	if !nameRE.MatchString(c.Type) {
		add("type", "must match "+nameRE.String())
	} else if manifest.Reserved(c.Type) {
		add("type", "is reserved")
	}
	if err := CheckSlug(c.Slug); err != nil {
		add("slug", err.Error())
	}
	c.validateSource(add)
	if c.Track != "" && !c.Track.Valid() {
		add("track", "must be exact, patch, minor, or latest")
	}
	for k := range c.Inputs {
		if !envKeyRE.MatchString(k) {
			add("inputs", fmt.Sprintf("key %q must match %s", k, envKeyRE))
		}
	}
	if utf8.RuneCountInString(c.Title) > maxTitle || strings.ContainsFunc(c.Title, unicode.IsControl) {
		add("title", fmt.Sprintf("must be at most %d characters on one line", maxTitle))
	}
	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Name: c.ref(), Fields: errs}
}

// validateSource checks from, version, manifest, and digest.
func (c Connection) validateSource(add func(field, msg string)) {
	switch {
	case c.From == FromCatalog:
		if c.Version == "" {
			add("version", "required for from: catalog")
		}
		if c.Manifest != nil {
			add("manifest", "only for from: inline")
		}
	case c.From == FromInline:
		if c.Manifest == nil {
			add("manifest", "required for from: inline")
		}
	case strings.HasPrefix(c.From, fromFile):
		if c.FilePath() == "" {
			add("from", "file: needs a path")
		}
		if c.Manifest != nil {
			add("manifest", "only for from: inline")
		}
	default:
		add("from", "must be catalog, inline, or file:<path>")
	}
	if c.Digest != "" && c.From != FromCatalog {
		add("digest", "only for from: catalog")
	}
	if c.Version != "" {
		if _, err := manifest.ParseVersion(c.Version); err != nil {
			add("version", err.Error())
		}
	}
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

// clone returns a deep copy through the JSON form.
func (c Connection) clone() Connection {
	b, err := json.Marshal(c)
	if err != nil {
		return c
	}
	var out Connection
	if err := json.Unmarshal(b, &out); err != nil {
		return c
	}
	return out
}

// track returns the track with the default.
func (c Connection) track() manifest.Track {
	if c.Track == "" {
		return manifest.TrackPatch
	}
	return c.Track
}

// ref names the connection in a message: the slug, else the id.
func (c Connection) ref() string {
	if c.Slug != "" {
		return c.Slug
	}
	return c.ID
}

// keepMasked puts back the stored value of each input that comes back as the
// mask. A mask with no stored value is a validation error.
func keepMasked(ref string, next, prev map[string]string) error {
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
		return &ValidationError{Name: ref, Fields: fields}
	}
	return nil
}
