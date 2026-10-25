package connectors

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"gopkg.in/yaml.v3"
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
	FromGitHub  = "github"
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
	Source   *GitHubSource      `yaml:"source,omitempty" json:"source,omitempty"`
	Track    manifest.Track     `yaml:"track,omitempty" json:"track,omitempty"`
	Inputs   map[string]string  `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Patch    map[string]any     `yaml:"patch,omitempty" json:"patch,omitempty"`
	Manifest *manifest.Manifest `yaml:"manifest,omitempty" json:"manifest,omitempty"`
	// Subscriptions are the things the user watches. The adapter makes the
	// platform match this list.
	Subscriptions []Subscription `yaml:"subscriptions,omitempty" json:"subscriptions,omitempty"`
	// Operations overrides the manifest state of an operation (D14).
	Operations map[string]manifest.OpState `yaml:"operations,omitempty" json:"operations,omitempty"`
	// Scope lists the workspace ids that see the connection. Empty is global.
	Scope Scope `yaml:"scope,omitempty" json:"scope,omitempty"`
	// OAuth is set for an online MCP server with an OAuth login.
	OAuth *OAuthSettings `yaml:"oauth,omitempty" json:"oauth,omitempty"`
	// Receive selects the events that Trebi keeps. Nil keeps all.
	Receive *EventFilter `yaml:"receive,omitempty" json:"receive,omitempty"`
}

// OAuthSettings are the login settings of an online MCP server. Empty
// fields take the manifest value, then the default.
type OAuthSettings struct {
	Mode         string   `yaml:"mode,omitempty" json:"mode,omitempty"`                 // proxy | header
	Return       string   `yaml:"return,omitempty" json:"return,omitempty"`             // auto | local | cloud
	Registration string   `yaml:"registration,omitempty" json:"registration,omitempty"` // auto | cimd | dcr | own_app
	ClientID     string   `yaml:"client_id,omitempty" json:"client_id,omitempty"`       // the own app; its secret is a reserved secret
	Scopes       []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
}

// OAuth run modes and return pages.
const (
	OAuthProxy  = "proxy"
	OAuthHeader = "header"
	ReturnAuto  = "auto"
	ReturnLocal = "local"
	ReturnCloud = "cloud"
)

// Validate checks the closed values.
func (o OAuthSettings) Validate() error {
	var errs []FieldError
	if o.Mode != "" && o.Mode != OAuthProxy && o.Mode != OAuthHeader {
		errs = append(errs, FieldError{Field: "oauth.mode", Message: "must be proxy or header"})
	}
	if o.Return != "" && !slices.Contains([]string{ReturnAuto, ReturnLocal, ReturnCloud}, o.Return) {
		errs = append(errs, FieldError{Field: "oauth.return", Message: "must be auto, local, or cloud"})
	}
	regs := []string{manifest.RegistrationAuto, manifest.RegistrationCIMD, manifest.RegistrationDCR, manifest.RegistrationOwnApp}
	if o.Registration != "" && !slices.Contains(regs, o.Registration) {
		errs = append(errs, FieldError{Field: "oauth.registration", Message: "must be auto, cimd, dcr, or own_app"})
	}
	if len(errs) > 0 {
		return &ValidationError{Fields: errs}
	}
	return nil
}

// ScopeGlobal is the scope value that every workspace sees.
const ScopeGlobal = "global"

// Scope is the list of workspace ids that see a connection, or [global].
// The file form of a global scope is the scalar global. Only visibility
// changes: one process and one login serve every workspace in the scope.
type Scope []string

// wsIDRE is the form of a workspace id.
var wsIDRE = regexp.MustCompile(`^ws_[A-Za-z0-9_-]+$`)

// IsGlobal reports whether every workspace sees the connection.
func (sc Scope) IsGlobal() bool {
	return len(sc) == 0 || (len(sc) == 1 && sc[0] == ScopeGlobal)
}

// Includes reports whether the workspace sees the connection. An empty
// workspace (a global caller) sees every connection.
func (sc Scope) Includes(workspaceID string) bool {
	return workspaceID == "" || sc.IsGlobal() || slices.Contains(sc, workspaceID)
}

// Wire is the row form: [global], or the workspace ids.
func (sc Scope) Wire() []string {
	if sc.IsGlobal() {
		return []string{ScopeGlobal}
	}
	return slices.Clone(sc)
}

// UnmarshalYAML reads the scalar global or a list.
func (sc *Scope) UnmarshalYAML(n *yaml.Node) error {
	var v Value
	if err := v.UnmarshalYAML(n); err != nil {
		return err
	}
	*sc = Scope(v)
	return nil
}

// MarshalYAML writes a global scope as the scalar global.
func (sc Scope) MarshalYAML() (any, error) {
	if sc.IsGlobal() {
		return ScopeGlobal, nil
	}
	return []string(sc), nil
}

// UnmarshalJSON reads "global" or a list of strings.
func (sc *Scope) UnmarshalJSON(b []byte) error {
	var v Value
	if err := v.UnmarshalJSON(b); err != nil {
		return err
	}
	*sc = Scope(v)
	return nil
}

// normal returns nil for a global scope, else the list.
func (sc Scope) normal() Scope {
	if sc.IsGlobal() {
		return nil
	}
	return sc
}

// check returns the problems of the list.
func (sc Scope) check() []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range sc {
		switch {
		case v == ScopeGlobal && len(sc) > 1:
			out = append(out, "global must be the only value")
		case v != ScopeGlobal && !wsIDRE.MatchString(v):
			out = append(out, fmt.Sprintf("%q must be global or a workspace id (ws_...)", v))
		case seen[v]:
			out = append(out, fmt.Sprintf("%q is a duplicate", v))
		}
		seen[v] = true
	}
	return out
}

// Subscription is one thing that the user watches.
type Subscription struct {
	ID     string           `yaml:"id" json:"id"`
	Values map[string]Value `yaml:"values,omitempty" json:"values,omitempty"`
	Types  []string         `yaml:"types,omitempty" json:"types,omitempty"`
}

// Value is one string or a list. It reads both forms and writes one string
// when it holds one item.
type Value []string

// UnmarshalYAML reads a scalar or a sequence.
func (v *Value) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*v = Value{n.Value}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*v = list
	return nil
}

// MarshalYAML writes one item as a scalar.
func (v Value) MarshalYAML() (any, error) {
	if len(v) == 1 {
		return v[0], nil
	}
	return []string(v), nil
}

// UnmarshalJSON reads a string or a list of strings.
func (v *Value) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*v = Value{one}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("must be a string or a list of strings")
	}
	*v = list
	return nil
}

// MarshalJSON writes one item as a string.
func (v Value) MarshalJSON() ([]byte, error) {
	if len(v) == 1 {
		return json.Marshal(v[0])
	}
	return json.Marshal([]string(v))
}

// subIDRE is sub_ plus a lowercase ULID.
var subIDRE = regexp.MustCompile(`^sub_[0-9a-hjkmnp-tv-z]{26}$`)

// NewSubscriptionID returns a new sub_ id.
func NewSubscriptionID() string {
	return "sub_" + strings.ToLower(ulid.Make().String())
}

// values returns the values as plain lists.
func (sub Subscription) values() map[string][]string {
	out := make(map[string][]string, len(sub.Values))
	for k, v := range sub.Values {
		out[k] = []string(v)
	}
	return out
}

// Wire returns the protocol form with the field defaults filled in.
func (sub Subscription) Wire(spec *manifest.Subscriptions) protocol.Subscription {
	values := sub.values()
	if spec != nil {
		values = spec.WithDefaults(values)
	}
	return protocol.Subscription{ID: sub.ID, Values: values, Types: slices.Clone(sub.Types)}
}

// key identifies the watched thing: the sorted values and types.
func (sub Subscription) key() string {
	types := slices.Clone(sub.Types)
	slices.Sort(types)
	b, _ := json.Marshal([]any{sub.values(), types}) //nolint:errcheck // maps of strings always encode
	return string(b)
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
	c.validateSubscriptions(add)
	for _, msg := range c.Scope.check() {
		add("scope", msg)
	}
	for name, st := range c.Operations {
		if !st.Valid() {
			add("operations", fmt.Sprintf("%s must be on, ask, or off", name))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Name: c.ref(), Fields: errs}
}

// validateSubscriptions checks the ids and the value sizes. The manifest
// rules are in commit.
func (c Connection) validateSubscriptions(add func(field, msg string)) {
	seen := map[string]bool{}
	for i, sub := range c.Subscriptions {
		at := fmt.Sprintf("subscriptions[%d]", i)
		if !subIDRE.MatchString(sub.ID) {
			add(at+".id", "must match "+subIDRE.String())
		} else if seen[sub.ID] {
			add(at+".id", "is a duplicate")
		}
		seen[sub.ID] = true
		for name, v := range sub.Values {
			for _, one := range v {
				if len(one) > manifest.MaxValueBytes {
					add(at+".values."+name, fmt.Sprintf("must be at most %d bytes", manifest.MaxValueBytes))
					break
				}
			}
		}
	}
}

// checkSubscriptions applies the form rules of the manifest to the list.
func (c Connection) checkSubscriptions(m manifest.Manifest) error {
	if len(c.Subscriptions) == 0 {
		return nil
	}
	spec := m.SubscriptionSpec()
	if spec == nil {
		return &ValidationError{Name: c.ref(), Fields: []FieldError{{Field: "subscriptions", Message: "the connector has no subscriptions"}}}
	}
	var errs []FieldError
	seen := map[string]int{}
	for i, sub := range c.Subscriptions {
		at := fmt.Sprintf("subscriptions[%d].", i)
		for _, fe := range spec.Check(sub.values(), sub.Types, m.Events.Types) {
			errs = append(errs, FieldError{Field: at + fe.Field, Message: fe.Message})
		}
		if j, ok := seen[sub.key()]; ok {
			errs = append(errs, FieldError{Field: at + "values", Message: fmt.Sprintf("watches the same thing as subscriptions[%d]", j)})
		}
		seen[sub.key()] = i
	}
	if len(errs) > 0 {
		return &ValidationError{Name: c.ref(), Fields: errs}
	}
	return nil
}

// validateSource checks from, version, manifest, source, and digest.
func (c Connection) validateSource(add func(field, msg string)) {
	switch {
	case c.From == FromCatalog:
		if c.Version == "" {
			add("version", "required for from: catalog")
		}
	case c.From == FromGitHub:
		c.validateGitHub(add)
	case c.From == FromInline:
		if c.Manifest == nil {
			add("manifest", "required for from: inline")
		}
	case strings.HasPrefix(c.From, fromFile):
		if c.FilePath() == "" {
			add("from", "file: needs a path")
		}
	default:
		add("from", "must be catalog, github, inline, or file:<path>")
	}
	if c.Manifest != nil && c.From != FromInline {
		add("manifest", "only for from: inline")
	}
	if c.Source != nil && c.From != FromGitHub {
		add("source", "only for from: github")
	}
	if c.Digest != "" && c.From != FromCatalog && c.From != FromGitHub {
		add("digest", "only for from: catalog or github")
	}
	if c.Version != "" {
		if _, err := manifest.ParseVersion(c.Version); err != nil {
			add("version", err.Error())
		}
	}
}

// validateGitHub checks the pin of a link connection: the commit, the
// version, and the digest. A link has no track (decision 4).
func (c Connection) validateGitHub(add func(field, msg string)) {
	switch {
	case c.Source == nil:
		add("source", "required for from: github")
	case !ghCommitRE.MatchString(c.Source.Commit):
		add("source.commit", "must be 40 hex characters")
	default:
		if err := c.Source.Validate(); err != nil {
			add("source", err.Error())
		}
	}
	if c.Version == "" {
		add("version", "required for from: github")
	}
	if c.Digest == "" {
		add("digest", "required for from: github")
	}
	if c.Track != "" {
		add("track", "not for from: github; a link stays on its commit")
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

// WithDefaultScope gives a new connection with no scope the scope of the
// workspace of the caller when the connector needs an account: a secret
// input or a login. Any other connection stays global.
func (c Connection) WithDefaultScope(workspaceID string) Connection {
	if len(c.Scope) > 0 || workspaceID == "" {
		return c
	}
	if m, _, err := loadBase(c); err == nil && m.NeedsAccount() {
		c.Scope = Scope{workspaceID}
	}
	return c
}

// hiddenIn returns ErrDisabled or ErrNotInScope when the workspace cannot use
// c. An empty workspace (a global caller) sees every connection.
func (c Connection) hiddenIn(workspaceID string, disabled bool) error {
	switch {
	case workspaceID == "":
		return nil
	case disabled:
		return fmt.Errorf("%w: %q in %s", ErrDisabled, c.Slug, workspaceID)
	case !c.Scope.Includes(workspaceID):
		return fmt.Errorf("%w: %q in %s", ErrNotInScope, c.Slug, workspaceID)
	}
	return nil
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

// packageVersion is the folder name of the package: the version, plus the
// commit for a link, so that a link package never shares a catalog folder.
func (c Connection) packageVersion() string {
	if c.From == FromGitHub && c.Source != nil {
		return linkPackage(c.Version, c.Source.Commit)
	}
	return c.Version
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
