// Package manifest owns trebi-connector.yaml: the typed manifest of one
// connector, its JSON Schema, the field rules, and the JSON merge patch that
// the global and workspace layers apply to it.
package manifest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

// Manifest is one trebi-connector.yaml. The YAML and the JSON forms use the
// same keys, so a JSON merge patch addresses the fields of the file.
type Manifest struct {
	Schema      string   `yaml:"schema" json:"schema"`
	Name        string   `yaml:"name" json:"name"`
	Title       string   `yaml:"title" json:"title"`
	Description string   `yaml:"description" json:"description"`
	Icon        string   `yaml:"icon,omitempty" json:"icon,omitempty"`
	Version     string   `yaml:"version" json:"version"`
	Publisher   string   `yaml:"publisher,omitempty" json:"publisher,omitempty"`
	Source      string   `yaml:"source,omitempty" json:"source,omitempty"`
	Homepage    string   `yaml:"homepage,omitempty" json:"homepage,omitempty"`
	License     string   `yaml:"license,omitempty" json:"license,omitempty"`
	Warning     string   `yaml:"warning,omitempty" json:"warning,omitempty"`
	Platforms   []string `yaml:"platforms,omitempty" json:"platforms,omitempty"`
	Install     *Install `yaml:"install,omitempty" json:"install,omitempty"`
	Setup       *Setup   `yaml:"setup,omitempty" json:"setup,omitempty"`
	Skill       *Skill   `yaml:"skill,omitempty" json:"skill,omitempty"`
	Actions     *Actions `yaml:"actions,omitempty" json:"actions,omitempty"`
	Events      *Events  `yaml:"events,omitempty" json:"events,omitempty"`
	Channel     *Channel `yaml:"channel,omitempty" json:"channel,omitempty"`
}

// Install lists the install methods. The first method that works wins.
type Install struct {
	Catalog        bool           `yaml:"catalog,omitempty" json:"catalog,omitempty"` // the archive that the catalog index lists for this version
	GitHubRelease  *GitHubRelease `yaml:"github_release,omitempty" json:"github_release,omitempty"`
	Build          *Build         `yaml:"build,omitempty" json:"build,omitempty"`
	Brew           string         `yaml:"brew,omitempty" json:"brew,omitempty"`
	Go             string         `yaml:"go,omitempty" json:"go,omitempty"`
	Bin            string         `yaml:"bin,omitempty" json:"bin,omitempty"`
	VersionCommand string         `yaml:"version_command,omitempty" json:"version_command,omitempty"`
	Packslip       any            `yaml:"packslip,omitempty" json:"packslip,omitempty"` // reserved for a later method
}

// GitHubRelease downloads one release asset and checks it against the
// checksums file of the same release.
type GitHubRelease struct {
	Repo      string `yaml:"repo" json:"repo"`
	Tag       string `yaml:"tag" json:"tag"`
	Asset     string `yaml:"asset" json:"asset"`
	Checksums string `yaml:"checksums,omitempty" json:"checksums,omitempty"`
}

// Build makes the program from the source in the entry folder.
type Build struct {
	Go string `yaml:"go" json:"go"` // package dir relative to the entry folder, "." for the root
}

// ModuleDir returns the folder that holds go.mod for build.go: the package
// folder or a parent folder inside the entry folder.
func (b Build) ModuleDir(entry string) (string, error) {
	root := filepath.Clean(entry)
	dir := filepath.Join(root, filepath.FromSlash(b.Go))
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		if dir == root {
			return "", fmt.Errorf("install.build: no go.mod in %s or a parent folder inside the connector folder", b.Go)
		}
		dir = filepath.Dir(dir)
	}
}

// Setup is the install form and the login methods.
type Setup struct {
	Inputs []Input       `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Login  []LoginMethod `yaml:"login,omitempty" json:"login,omitempty"`
}

// LoginMethod is one way to log in. A method with an input belongs to the
// daemon: the user pastes the value of that secret input, and the daemon
// checks it. Test is a read-only command line for that check. A method with
// oauth also belongs to the daemon: an OAuth login to actions.mcp.url. The
// adapter owns the other methods and answers auth/methods for them.
type LoginMethod struct {
	ID    string      `yaml:"id" json:"id"`
	Type  string      `yaml:"type" json:"type"`
	Label string      `yaml:"label,omitempty" json:"label,omitempty"`
	Help  string      `yaml:"help,omitempty" json:"help,omitempty"`
	Input string      `yaml:"input,omitempty" json:"input,omitempty"`
	Test  string      `yaml:"test,omitempty" json:"test,omitempty"`
	OAuth *OAuthLogin `yaml:"oauth,omitempty" json:"oauth,omitempty"`
}

// Daemon reports whether the daemon runs the method.
func (l LoginMethod) Daemon() bool { return l.Input != "" || l.OAuth != nil }

// OAuthLogin is an OAuth login to an online MCP server. Registration is
// auto, cimd, dcr, or own_app. ClientID is a known app of the server.
type OAuthLogin struct {
	Scopes       []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
	ClientID     string   `yaml:"client_id,omitempty" json:"client_id,omitempty"`
	Registration string   `yaml:"registration,omitempty" json:"registration,omitempty"`
}

// OAuth registrations of a login method.
const (
	RegistrationAuto   = "auto"
	RegistrationCIMD   = "cimd"
	RegistrationDCR    = "dcr"
	RegistrationOwnApp = "own_app"
)

// OAuthMethod returns the first OAuth login method.
func (s *Setup) OAuthMethod() (LoginMethod, bool) {
	if s == nil {
		return LoginMethod{}, false
	}
	i := slices.IndexFunc(s.Login, func(l LoginMethod) bool { return l.OAuth != nil })
	if i < 0 {
		return LoginMethod{}, false
	}
	return s.Login[i], true
}

// Login method types, in the order of the UI.
const (
	LoginExisting = "existing"
	LoginBrowser  = "browser"
	LoginPhone    = "phone"
	LoginKey      = "key"
)

// LoginTypes lists the login method types in the order of the UI.
func LoginTypes() []string { return []string{LoginExisting, LoginBrowser, LoginPhone, LoginKey} }

// KeyInputs returns the names of the inputs that a key method owns. The
// install form does not ask for them; the Login tab does.
func (s *Setup) KeyInputs() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, l := range s.Login {
		if l.Input != "" {
			out = append(out, l.Input)
		}
	}
	return out
}

// AdapterLogin reports whether the adapter owns at least one login method.
func (s *Setup) AdapterLogin() bool {
	return s != nil && slices.ContainsFunc(s.Login, func(l LoginMethod) bool { return !l.Daemon() })
}

// Input is one field of the install form. Its value becomes an env var of
// every process of the connector; a secret input is stored as a secret.
type Input struct {
	Name     string   `yaml:"name" json:"name"`
	Label    string   `yaml:"label,omitempty" json:"label,omitempty"`
	Help     string   `yaml:"help,omitempty" json:"help,omitempty"`
	URL      string   `yaml:"url,omitempty" json:"url,omitempty"`
	Secret   bool     `yaml:"secret,omitempty" json:"secret,omitempty"`
	Required bool     `yaml:"required,omitempty" json:"required,omitempty"`
	Choices  []string `yaml:"choices,omitempty" json:"choices,omitempty"`
	Default  string   `yaml:"default,omitempty" json:"default,omitempty"`
	Format   string   `yaml:"format,omitempty" json:"format,omitempty"`
}

// Skill points at the skill folder of the entry.
type Skill struct {
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`
	Compat string `yaml:"compat,omitempty" json:"compat,omitempty"`
}

// Actions is the "an agent can act on the system" capability.
type Actions struct {
	CLI        *CLIAction  `yaml:"cli,omitempty" json:"cli,omitempty"`
	MCP        *MCPAction  `yaml:"mcp,omitempty" json:"mcp,omitempty"`
	Operations []Operation `yaml:"operations,omitempty" json:"operations,omitempty"`
}

// CLIAction gives a run local commands; the skill teaches them.
type CLIAction struct {
	Commands []string `yaml:"commands" json:"commands"`
}

// MCPAction is one MCP server: a registry reference, a stdio command, or a
// URL. "${NAME}" in args, env, url, and headers expands from the inputs.
type MCPAction struct {
	Registry string            `yaml:"registry,omitempty" json:"registry,omitempty"`
	Name     string            `yaml:"name,omitempty" json:"name,omitempty"`
	Version  string            `yaml:"version,omitempty" json:"version,omitempty"`
	Command  string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args     []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Env      map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	URL      string            `yaml:"url,omitempty" json:"url,omitempty"`
	Headers  map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// Events is the "the system can start work" capability. The lines fields
// (format to catchup) exist only with protocol lines. Room to Text are
// dotted paths into a JSON line that fill the message fields of the event.
type Events struct {
	Protocol   string      `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	Command    string      `yaml:"command" json:"command"`
	Restart    string      `yaml:"restart,omitempty" json:"restart,omitempty"`
	Types      []EventType `yaml:"types,omitempty" json:"types,omitempty"`
	Format     string      `yaml:"format,omitempty" json:"format,omitempty"`
	Poll       Duration    `yaml:"poll,omitempty" json:"poll,omitempty"`
	ID         string      `yaml:"id,omitempty" json:"id,omitempty"`
	TS         string      `yaml:"ts,omitempty" json:"ts,omitempty"`
	Identity   string      `yaml:"identity,omitempty" json:"identity,omitempty"`
	Room       string      `yaml:"room,omitempty" json:"room,omitempty"`
	RoomName   string      `yaml:"room_name,omitempty" json:"room_name,omitempty"`
	Thread     string      `yaml:"thread,omitempty" json:"thread,omitempty"`
	Sender     string      `yaml:"sender,omitempty" json:"sender,omitempty"`
	SenderName string      `yaml:"sender_name,omitempty" json:"sender_name,omitempty"`
	Type       string      `yaml:"type,omitempty" json:"type,omitempty"`
	Text       string      `yaml:"text,omitempty" json:"text,omitempty"`
	Catchup    *Catchup    `yaml:"catchup,omitempty" json:"catchup,omitempty"`

	Subscriptions *Subscriptions `yaml:"subscriptions,omitempty" json:"subscriptions,omitempty"`
}

// FieldPath is one dotted path key of a lines events block.
type FieldPath struct{ Field, Path string }

// Paths lists the dotted path keys of a lines events block.
func (e Events) Paths() []FieldPath {
	return []FieldPath{
		{"events.id", e.ID}, {"events.ts", e.TS}, {"events.identity", e.Identity},
		{"events.room", e.Room}, {"events.room_name", e.RoomName}, {"events.thread", e.Thread},
		{"events.sender", e.Sender}, {"events.sender_name", e.SenderName},
		{"events.type", e.Type}, {"events.text", e.Text},
	}
}

// MapsMessage reports whether a lines events block maps a message field
// (room to text). Such an event gets the built-in vars.
func (e Events) MapsMessage() bool {
	return e.Room != "" || e.RoomName != "" || e.Thread != "" || e.Sender != "" ||
		e.SenderName != "" || e.Type != "" || e.Text != ""
}

// EventType is one event the connector emits. Schema is a JSON Schema file
// for the data of the event, relative to the entry folder.
type EventType struct {
	Type        string `yaml:"type" json:"type"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Schema      string `yaml:"schema,omitempty" json:"schema,omitempty"`
}

// Catchup replays the events a lines connector emitted while the daemon was
// down.
type Catchup struct {
	Command   string   `yaml:"command" json:"command"`
	MaxEvents int      `yaml:"max_events,omitempty" json:"max_events,omitempty"`
	Timeout   Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Subscriptions declares what a user can watch in the system.
type Subscriptions struct {
	Action  string              `yaml:"action" json:"action"`
	Modes   []string            `yaml:"modes" json:"modes"`
	Fields  []SubscriptionField `yaml:"fields,omitempty" json:"fields,omitempty"`
	Webhook *Webhook            `yaml:"webhook,omitempty" json:"webhook,omitempty"`
}

// SubscriptionField is one field of the watch form.
type SubscriptionField struct {
	Name     string   `yaml:"name" json:"name"`
	Label    string   `yaml:"label" json:"label"`
	Help     string   `yaml:"help,omitempty" json:"help,omitempty"`
	Choices  []string `yaml:"choices,omitempty" json:"choices,omitempty"`
	Dynamic  bool     `yaml:"dynamic,omitempty" json:"dynamic,omitempty"`
	Multiple bool     `yaml:"multiple,omitempty" json:"multiple,omitempty"`
	Required bool     `yaml:"required,omitempty" json:"required,omitempty"`
	Default  string   `yaml:"default,omitempty" json:"default,omitempty"`
	Room     bool     `yaml:"room,omitempty" json:"room,omitempty"`
}

// Webhook is what the cloud keeps for the hosted hook of a connection.
type Webhook struct {
	Headers   []string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Handshake []string `yaml:"handshake,omitempty" json:"handshake,omitempty"`
}

// HandshakeRequest is one inbound request, as the cloud sees it.
type HandshakeRequest struct {
	Method  string
	Query   url.Values
	Headers http.Header
	Body    []byte
}

// HandshakeAnswer is the reply of the cloud to a handshake.
type HandshakeAnswer struct {
	Status      int
	Headers     map[string]string
	ContentType string
	Body        string
	Forward     bool
}

// Channel is the "the system can hold a conversation" capability. Features
// and limits are what the catalog promises; initialize is what the process
// delivers.
type Channel struct {
	Features []string `yaml:"features,omitempty" json:"features,omitempty"`
	Limits   Limits   `yaml:"limits,omitempty" json:"limits,omitzero"`
}

// Limits bound one message of a channel.
type Limits struct {
	MaxText int      `yaml:"max_text,omitempty" json:"max_text,omitempty"`
	Formats []string `yaml:"formats,omitempty" json:"formats,omitempty"`
}

// FieldError is one validation problem on a named field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError lists every field error of one manifest. Layer names the
// layer that caused it (catalog, global, workspace) when a resolve fails.
type ValidationError struct {
	Name   string
	Layer  string
	Fields []FieldError
}

// Duration is a text duration ("15m") in both YAML and JSON. A manifest is a
// document, so it keeps the text form of the file.
type Duration time.Duration

// Schema, file, and enum values of trebi-connector/1.
const (
	SchemaID = "trebi-connector/1"
	FileName = "trebi-connector.yaml"

	ProtocolLines     = "lines"
	ProtocolConnector = SchemaID

	FormatJSON = "json"
	FormatLine = "line"

	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"

	PublisherTrebi     = "trebi"
	PublisherCommunity = "community"

	ModeReply = "reply"
	ModePush  = "push"

	ModeAPI    = "api"
	ModeManual = "manual"
	ModeStream = "stream"
	ModePoll   = "poll"

	// EventMessage is the event type that makes a channel conversational.
	EventMessage = "message"

	defaultSkillPath      = "skill"
	defaultMaxEvents      = 500
	defaultCatchupTimeout = 2 * time.Minute
	minPoll               = 10 * time.Second
)

// Channel features (Part D.5).
const (
	FeatureRoomsList      = "rooms.list"
	FeatureRoomsOpen      = "rooms.open"
	FeatureThreads        = "threads"
	FeatureThreadsCreate  = "threads.create"
	FeatureHistory        = "history"
	FeatureReplay         = "replay"
	FeatureTyping         = "typing"
	FeatureSeen           = "seen"
	FeatureReactions      = "reactions"
	FeatureEdit           = "edit"
	FeatureAttachmentsIn  = "attachments.in"
	FeatureAttachmentsOut = "attachments.out"
	FeatureReplies        = "replies"

	FeatureSubscriptions = "subscriptions"
	FeatureWebhooks      = "webhooks"
)

// Limits of the watch form and the hosted hook.
const (
	maxSubscriptionFields = 8
	maxActionLen          = 40
	maxWebhookHeaders     = 16
	maxHandshakeRules     = 4
	// MaxValueBytes bounds one free text value of a subscription.
	MaxValueBytes = 500
)

var (
	// ErrInvalid marks a manifest that failed validation.
	ErrInvalid = errors.New("invalid connector")

	//go:embed schema.json
	schemaJSON []byte

	nameRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	envKeyRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	loginIDRE  = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	templateRE = regexp.MustCompile(`\{([a-z]+)\}`)
	fieldRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	headerRE   = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	ruleRE     = regexp.MustCompile(`^(query_echo:[A-Za-z0-9_.]{1,64}|json_echo:[A-Za-z0-9_]{1,64}|header_echo:[a-z0-9-]{1,64}|ok:(GET|HEAD))$`)
)

// Reserved names belong to a native channel or to the harness bridge.
func Reserved(name string) bool {
	switch name {
	case "email", "webhook", "trebi":
		return true
	}
	return false
}

// SchemaJSON returns the embedded JSON Schema, byte for byte.
func SchemaJSON() []byte { return slices.Clone(schemaJSON) }

// Load reads dir/trebi-connector.yaml and validates it.
func Load(dir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return Manifest{}, err
	}
	m, err := Parse(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", filepath.Join(dir, FileName), err)
	}
	return m, m.Validate()
}

// Parse decodes one manifest strictly: an unknown key fails. It does not
// validate.
func Parse(data []byte) (Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		return Manifest{}, err
	}
	return m, nil
}

// Patch applies a JSON merge patch (RFC 7386) to the JSON form of base. The
// result is decoded strictly; it is not validated.
func Patch(base Manifest, patch json.RawMessage) (Manifest, error) {
	if len(bytes.TrimSpace(patch)) == 0 || string(bytes.TrimSpace(patch)) == "null" {
		return base.Clone(), nil
	}
	doc, err := json.Marshal(base)
	if err != nil {
		return Manifest{}, err
	}
	merged, err := MergePatch(doc, patch)
	if err != nil {
		return Manifest{}, err
	}
	var out Manifest
	dec := json.NewDecoder(bytes.NewReader(merged))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return Manifest{}, fmt.Errorf("patch: %w", err)
	}
	return out, nil
}

// MergePatch applies an RFC 7386 JSON merge patch to target.
func MergePatch(target, patch []byte) ([]byte, error) {
	var t, p any
	if len(bytes.TrimSpace(target)) > 0 {
		if err := json.Unmarshal(target, &t); err != nil {
			return nil, fmt.Errorf("merge target: %w", err)
		}
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil, fmt.Errorf("merge patch: %w", err)
	}
	return json.Marshal(mergeValue(t, p))
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		if f.Field == "" {
			parts = append(parts, f.Message)
			continue
		}
		parts = append(parts, f.Field+": "+f.Message)
	}
	name := e.Name
	if name == "" {
		name = "connector"
	}
	if e.Layer != "" {
		name += " (" + e.Layer + " layer)"
	}
	return fmt.Sprintf("%s %s", name, strings.Join(parts, "; "))
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

// Std returns the underlying duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// MarshalYAML writes the text form; a zero duration writes as empty.
func (d Duration) MarshalYAML() (any, error) {
	if d <= 0 {
		return "", nil
	}
	return time.Duration(d).String(), nil
}

// UnmarshalYAML reads the text form.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	return d.parse(s)
}

// MarshalJSON writes the text form; a zero duration writes as "".
func (d Duration) MarshalJSON() ([]byte, error) {
	if d <= 0 {
		return []byte(`""`), nil
	}
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads the text form.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	return d.parse(s)
}

func (d *Duration) parse(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		*d = 0
		return nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(dur)
	return nil
}

// Clone returns a deep copy through the JSON form.
func (m Manifest) Clone() Manifest {
	b, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out Manifest
	if err := json.Unmarshal(b, &out); err != nil {
		return m
	}
	return out
}

// Capabilities lists the blocks present, in the fixed order.
func (m Manifest) Capabilities() []string {
	out := make([]string, 0, 3)
	if m.Actions != nil {
		out = append(out, "actions")
	}
	if m.Events != nil {
		out = append(out, "events")
	}
	if m.Channel != nil {
		out = append(out, "channel")
	}
	return out
}

// NeedsAccount reports whether a connection holds an account of its own: a
// secret input or a login method.
func (m Manifest) NeedsAccount() bool {
	if m.Setup == nil {
		return false
	}
	return len(m.Setup.Login) > 0 || slices.ContainsFunc(m.Setup.Inputs, func(in Input) bool { return in.Secret })
}

// Modes lists what the channel does: a channel with a message event type
// holds conversations and pushes; any other channel only pushes.
func (m Manifest) Modes() []string {
	if m.Channel == nil {
		return nil
	}
	if m.HasEventType(EventMessage) {
		return []string{ModeReply, ModePush}
	}
	return []string{ModePush}
}

// HasEventType reports whether events.types declares t.
func (m Manifest) HasEventType(t string) bool {
	if m.Events == nil {
		return false
	}
	for _, et := range m.Events.Types {
		if et.Type == t {
			return true
		}
	}
	return false
}

// AcceptsType reports whether the events block takes type t. A block with no
// types takes every type.
func (m Manifest) AcceptsType(t string) bool {
	return m.Events != nil && (len(m.Events.Types) == 0 || m.HasEventType(t))
}

// Protocol returns the events protocol, or "" with no events block.
func (m Manifest) Protocol() string {
	if m.Events == nil {
		return ""
	}
	if m.Events.Protocol == "" {
		return ProtocolLines
	}
	return m.Events.Protocol
}

// SkillPath returns the skill folder relative to the entry folder.
func (m Manifest) SkillPath() string {
	if m.Skill != nil && m.Skill.Path != "" {
		return m.Skill.Path
	}
	return defaultSkillPath
}

// SkillCompat returns the software range the skill fits. With no compat, the
// skill fits this manifest version only.
func (m Manifest) SkillCompat() (Range, error) {
	if m.Skill == nil || strings.TrimSpace(m.Skill.Compat) == "" {
		v, err := ParseVersion(m.Version)
		if err != nil {
			return Range{}, err
		}
		return ExactRange(v), nil
	}
	return ParseRange(m.Skill.Compat)
}

// EventSchemas compiles the data schema of each event type. A schema path
// is relative to dir, the entry folder.
func (m Manifest) EventSchemas(dir string) (map[string]*jsonschema.Resolved, error) {
	out := map[string]*jsonschema.Resolved{}
	if m.Events == nil {
		return out, nil
	}
	for _, t := range m.Events.Types {
		if t.Schema == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(t.Schema)))
		if err != nil {
			return nil, fmt.Errorf("event %s schema: %w", t.Type, err)
		}
		var sch jsonschema.Schema
		if err := json.Unmarshal(data, &sch); err != nil {
			return nil, fmt.Errorf("event %s schema: %w", t.Type, err)
		}
		res, err := sch.Resolve(nil)
		if err != nil {
			return nil, fmt.Errorf("event %s schema: %w", t.Type, err)
		}
		out[t.Type] = res
	}
	return out, nil
}

// Features returns the promised features: the channel features, then
// subscriptions and webhooks.
func (m Manifest) Features() []string {
	var out []string
	if m.Channel != nil {
		out = slices.Clone(m.Channel.Features)
	}
	if m.Events != nil && m.Events.Subscriptions != nil {
		out = append(out, FeatureSubscriptions)
		if m.Events.Subscriptions.Webhook != nil {
			out = append(out, FeatureWebhooks)
		}
	}
	return out
}

// SubscriptionSpec returns the events.subscriptions block, or nil.
func (m Manifest) SubscriptionSpec() *Subscriptions {
	if m.Events == nil {
		return nil
	}
	return m.Events.Subscriptions
}

// WithDefaults fills the declared defaults. Call it after Validate.
func (m Manifest) WithDefaults() Manifest {
	m = m.Clone()
	if e := m.Events; e != nil {
		if e.Protocol == "" {
			e.Protocol = ProtocolLines
		}
		if e.Restart == "" {
			e.Restart = RestartOnFailure
		}
		if e.Protocol == ProtocolLines && e.Format == "" {
			e.Format = FormatLine
		}
		if e.Catchup != nil {
			if e.Catchup.MaxEvents == 0 {
				e.Catchup.MaxEvents = defaultMaxEvents
			}
			if e.Catchup.Timeout == 0 {
				e.Catchup.Timeout = Duration(defaultCatchupTimeout)
			}
		}
	}
	return m
}

// Validate checks the JSON Schema and the field rules of Part B.2 and
// returns every field error.
func (m Manifest) Validate() error {
	v := &validator{}
	v.schema(m)
	v.identity(m)
	v.install(m)
	v.setup(m)
	v.skill(m)
	v.actions(m)
	v.events(m)
	v.channel(m)
	if len(v.errs) == 0 {
		return nil
	}
	return &ValidationError{Name: m.Name, Fields: v.errs}
}

// ExpandTemplate replaces {version}, {os}, and {arch} in an install field.
func ExpandTemplate(s, version, goos, goarch string) string {
	return templateRE.ReplaceAllStringFunc(s, func(k string) string {
		switch k {
		case "{version}":
			return version
		case "{os}":
			return goos
		case "{arch}":
			return goarch
		}
		return k
	})
}

// validator collects the field errors of one manifest.
type validator struct{ errs []FieldError }

func (v *validator) add(field, msg string) {
	v.errs = append(v.errs, FieldError{Field: field, Message: msg})
}

// schema runs the embedded JSON Schema on the JSON form.
func (v *validator) schema(m Manifest) {
	res, err := compiledSchema()
	if err != nil {
		v.add("", "schema: "+err.Error())
		return
	}
	b, err := json.Marshal(m)
	if err != nil {
		v.add("", err.Error())
		return
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		v.add("", err.Error())
		return
	}
	pruneEmpty(doc)
	if err := res.Validate(doc); err != nil {
		v.add(schemaError(err.Error()))
	}
}

func (v *validator) identity(m Manifest) {
	if m.Schema != SchemaID {
		v.add("schema", "must be "+SchemaID)
	}
	if !nameRE.MatchString(m.Name) {
		v.add("name", "must match "+nameRE.String())
	}
	if Reserved(m.Name) {
		v.add("name", "is reserved")
	}
	for _, f := range []struct{ field, val string }{{"title", m.Title}, {"description", m.Description}} {
		if strings.TrimSpace(f.val) == "" {
			v.add(f.field, "required")
		}
	}
	if _, err := ParseVersion(m.Version); err != nil {
		v.add("version", err.Error())
	}
	if m.Actions == nil && m.Events == nil && m.Channel == nil {
		v.add("", "at least one of actions, events, channel is required")
	}
	if m.Icon != "" && !relativePath(m.Icon) {
		v.add("icon", "must be a path in the entry folder")
	}
	switch m.Publisher {
	case "", PublisherTrebi, PublisherCommunity:
	default:
		v.add("publisher", "must be trebi or community")
	}
}

func (v *validator) install(m Manifest) {
	in := m.Install
	if in == nil {
		return
	}
	check := func(field, s string) {
		for _, k := range templateRE.FindAllString(s, -1) {
			if k != "{version}" && k != "{os}" && k != "{arch}" {
				v.add(field, "unknown template "+k)
			}
		}
	}
	if gh := in.GitHubRelease; gh != nil {
		if gh.Repo == "" || gh.Tag == "" || gh.Asset == "" {
			v.add("install.github_release", "needs repo, tag, and asset")
		}
		check("install.github_release.tag", gh.Tag)
		check("install.github_release.asset", gh.Asset)
	}
	check("install.go", in.Go)
	if b := in.Build; b != nil && !relativePath(b.Go) {
		v.add("install.build.go", "must be a path in the entry folder")
	}
	if in.Packslip != nil {
		v.add("install.packslip", "is reserved")
	}
	if (in.Catalog || in.GitHubRelease != nil || in.Build != nil || in.Brew != "" || in.Go != "") && in.Bin == "" {
		v.add("install.bin", "required with an install method")
	}
}

func (v *validator) setup(m Manifest) {
	s := m.Setup
	if s == nil {
		return
	}
	seen := map[string]bool{}
	for i, in := range s.Inputs {
		field := fmt.Sprintf("setup.inputs[%d]", i)
		if !envKeyRE.MatchString(in.Name) {
			v.add(field+".name", "must match "+envKeyRE.String())
		}
		if seen[in.Name] {
			v.add(field+".name", "duplicate "+in.Name)
		}
		seen[in.Name] = true
		if len(in.Choices) > 0 && in.Default != "" && !slices.Contains(in.Choices, in.Default) {
			v.add(field+".default", "must be one of choices")
		}
	}
	ids := map[string]bool{}
	for i, l := range s.Login {
		field := fmt.Sprintf("setup.login[%d]", i)
		if !loginIDRE.MatchString(l.ID) {
			v.add(field+".id", "must match "+loginIDRE.String())
		}
		if ids[l.ID] {
			v.add(field+".id", "duplicate "+l.ID)
		}
		ids[l.ID] = true
		if !slices.Contains(LoginTypes(), l.Type) {
			v.add(field+".type", "unknown type "+strconv.Quote(l.Type))
		}
		if l.OAuth != nil {
			v.oauthLogin(m, field, l)
			continue
		}
		if l.Input == "" {
			if l.Test != "" {
				v.add(field+".test", "is allowed only with input")
			}
			if m.Protocol() != ProtocolConnector {
				v.add(field, "a method with no input needs events.protocol: "+ProtocolConnector)
			}
			continue
		}
		if l.Type != LoginKey {
			v.add(field+".input", "is allowed only with type: "+LoginKey)
		}
		if !slices.ContainsFunc(s.Inputs, func(in Input) bool { return in.Name == l.Input && in.Secret }) {
			v.add(field+".input", "must name a secret input of setup.inputs")
		}
	}
}

// oauthLogin checks a method with oauth: type browser, no input or test,
// a known registration, and an MCP URL to log in to.
func (v *validator) oauthLogin(m Manifest, field string, l LoginMethod) {
	if l.Type != LoginBrowser {
		v.add(field+".oauth", "is allowed only with type: "+LoginBrowser)
	}
	if l.Input != "" || l.Test != "" {
		v.add(field+".oauth", "is not allowed with input or test")
	}
	if r := l.OAuth.Registration; r != "" && !slices.Contains([]string{RegistrationAuto, RegistrationCIMD, RegistrationDCR, RegistrationOwnApp}, r) {
		v.add(field+".oauth.registration", "unknown registration "+strconv.Quote(r))
	}
	if m.Actions == nil || m.Actions.MCP == nil || m.Actions.MCP.URL == "" {
		v.add(field+".oauth", "needs actions.mcp.url")
	}
}

func (v *validator) skill(m Manifest) {
	if m.Skill == nil {
		return
	}
	if m.Skill.Path != "" && !relativePath(m.Skill.Path) {
		v.add("skill.path", "must be a path in the entry folder")
	}
	if m.Skill.Compat != "" {
		if _, err := ParseRange(m.Skill.Compat); err != nil {
			v.add("skill.compat", err.Error())
		}
	}
}

func (v *validator) actions(m Manifest) {
	a := m.Actions
	if a == nil {
		return
	}
	if a.MCP == nil && a.CLI == nil && len(a.Operations) == 0 {
		v.add("actions", "at least one of mcp, cli, operations is required")
	}
	if c := a.CLI; c != nil && len(c.Commands) == 0 {
		v.add("actions.cli.commands", "needs at least one entry")
	}
	if a.MCP != nil {
		v.mcp(*a.MCP)
	}
	v.operations(a)
}

func (v *validator) mcp(mc MCPAction) {
	forms := 0
	for _, set := range []bool{mc.Name != "" || mc.Registry != "", mc.Command != "", mc.URL != ""} {
		if set {
			forms++
		}
	}
	if forms != 1 {
		v.add("actions.mcp", "exactly one of name (a registry reference), command, url is required")
	}
	if mc.Registry != "" && mc.Name == "" {
		v.add("actions.mcp.name", "required with registry")
	}
	if mc.URL == "" && len(mc.Headers) > 0 {
		v.add("actions.mcp.headers", "needs url")
	}
	if mc.Command == "" && len(mc.Env) > 0 {
		v.add("actions.mcp.env", "needs command")
	}
	if mc.Command == "" && len(mc.Args) > 0 {
		v.add("actions.mcp.args", "needs command")
	}
	for k := range mc.Env {
		if !envKeyRE.MatchString(k) {
			v.add("actions.mcp.env", fmt.Sprintf("key %q must match %s", k, envKeyRE))
		}
	}
}

func (v *validator) events(m Manifest) {
	e := m.Events
	if e == nil {
		return
	}
	if strings.TrimSpace(e.Command) == "" {
		v.add("events.command", "required")
	}
	switch e.Restart {
	case "", RestartAlways, RestartOnFailure, RestartNever:
	default:
		v.add("events.restart", "must be always, on-failure, or never")
	}
	types := map[string]bool{}
	for i, t := range e.Types {
		if types[t.Type] {
			v.add(fmt.Sprintf("events.types[%d].type", i), "duplicate "+t.Type)
		}
		types[t.Type] = true
		if t.Schema != "" && !relativePath(t.Schema) {
			v.add(fmt.Sprintf("events.types[%d].schema", i), "must be a path in the entry folder")
		}
	}
	if e.Subscriptions != nil {
		if m.Protocol() != ProtocolConnector {
			v.add("events.subscriptions", "needs events.protocol: "+ProtocolConnector)
		}
		v.subscriptions(e.Subscriptions)
	}
	switch m.Protocol() {
	case ProtocolConnector:
		v.linesOnly(*e)
	case ProtocolLines:
		v.lines(*e)
	default:
		v.add("events.protocol", "must be lines or "+ProtocolConnector)
	}
}

// subscriptions checks the cross-field rules of the watch form. The JSON
// Schema holds the patterns and the sizes.
func (v *validator) subscriptions(s *Subscriptions) {
	const base = "events.subscriptions"
	if strings.TrimSpace(s.Action) == "" {
		v.add(base+".action", "required")
	} else if len(s.Action) > maxActionLen {
		v.add(base+".action", fmt.Sprintf("must be at most %d characters", maxActionLen))
	}
	if len(s.Modes) == 0 {
		v.add(base+".modes", "needs at least one mode")
	}
	seenMode := map[string]bool{}
	for _, mode := range s.Modes {
		if !slices.Contains(SubscriptionModes(), mode) {
			v.add(base+".modes", "unknown mode "+strconv.Quote(mode))
		}
		if seenMode[mode] {
			v.add(base+".modes", "duplicate "+mode)
		}
		seenMode[mode] = true
	}
	if (seenMode[ModeAPI] || seenMode[ModeManual]) && s.Webhook == nil {
		v.add(base+".webhook", "required with mode api or manual")
	}
	v.subscriptionFields(base, s.Fields)
	if s.Webhook != nil {
		v.webhook(base+".webhook", *s.Webhook)
	}
}

// subscriptionFields checks the fields of the watch form.
func (v *validator) subscriptionFields(base string, fields []SubscriptionField) {
	if len(fields) > maxSubscriptionFields {
		v.add(base+".fields", fmt.Sprintf("must have at most %d items", maxSubscriptionFields))
	}
	seen := map[string]bool{}
	rooms := 0
	for i, f := range fields {
		field := fmt.Sprintf("%s.fields[%d]", base, i)
		if !fieldRE.MatchString(f.Name) {
			v.add(field+".name", "must match "+fieldRE.String())
		}
		if seen[f.Name] {
			v.add(field+".name", "duplicate "+f.Name)
		}
		seen[f.Name] = true
		if strings.TrimSpace(f.Label) == "" {
			v.add(field+".label", "required")
		}
		if len(f.Choices) > 0 && f.Dynamic {
			v.add(field+".dynamic", "cannot be combined with choices")
		}
		if len(f.Choices) > 0 && f.Default != "" && !slices.Contains(f.Choices, f.Default) {
			v.add(field+".default", "must be one of choices")
		}
		if f.Room {
			rooms++
			if f.Multiple {
				v.add(field+".multiple", "a room field takes one value")
			}
		}
	}
	if rooms > 1 {
		v.add(base+".fields", "at most one field has room: true")
	}
}

// webhook checks the headers and the handshake rules of the hook.
func (v *validator) webhook(base string, w Webhook) {
	if len(w.Headers) > maxWebhookHeaders {
		v.add(base+".headers", fmt.Sprintf("must have at most %d items", maxWebhookHeaders))
	}
	for i, h := range w.Headers {
		if msg := headerProblem(h); msg != "" {
			v.add(fmt.Sprintf("%s.headers[%d]", base, i), msg)
		}
	}
	if len(w.Handshake) > maxHandshakeRules {
		v.add(base+".handshake", fmt.Sprintf("must have at most %d rules", maxHandshakeRules))
	}
	for i, r := range w.Handshake {
		if !ruleRE.MatchString(r) {
			v.add(fmt.Sprintf("%s.handshake[%d]", base, i), "unknown rule "+strconv.Quote(r))
		}
	}
}

// headerProblem returns why the cloud refuses to forward a header, or "".
func headerProblem(h string) string {
	if !headerRE.MatchString(h) {
		return "must match " + headerRE.String()
	}
	switch h {
	case "authorization", "cookie", "set-cookie", "proxy-authorization":
		return "is not forwarded"
	}
	if strings.HasPrefix(h, "cf-") || strings.HasPrefix(h, "x-forwarded-") {
		return "is not forwarded"
	}
	return ""
}

// SubscriptionModes lists the subscription modes.
func SubscriptionModes() []string { return []string{ModeAPI, ModeManual, ModeStream, ModePoll} }

// Field returns the field with the name, or false.
func (s Subscriptions) Field(name string) (SubscriptionField, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return SubscriptionField{}, false
}

// RoomField returns the room field, or false.
func (s Subscriptions) RoomField() (SubscriptionField, bool) {
	for _, f := range s.Fields {
		if f.Room {
			return f, true
		}
	}
	return SubscriptionField{}, false
}

// Check returns the problems of the values and the event types of one
// subscription. The fields are values.<name> and types.
func (s Subscriptions) Check(values map[string][]string, types []string, eventTypes []EventType) []FieldError {
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{Field: field, Message: msg}) }
	for name, vals := range values {
		f, ok := s.Field(name)
		if !ok {
			add("values."+name, "unknown field")
			continue
		}
		if !f.Multiple && len(vals) > 1 {
			add("values."+name, "takes one value")
		}
		for _, val := range vals {
			switch {
			case len(f.Choices) > 0 && !slices.Contains(f.Choices, val):
				add("values."+name, fmt.Sprintf("%q is not one of %s", val, strings.Join(f.Choices, ", ")))
			case len(val) > MaxValueBytes:
				add("values."+name, fmt.Sprintf("must be at most %d bytes", MaxValueBytes))
			}
		}
	}
	for _, f := range s.Fields {
		if f.Required && len(values[f.Name]) == 0 {
			add("values."+f.Name, "required")
		}
	}
	for _, t := range types {
		if !slices.ContainsFunc(eventTypes, func(et EventType) bool { return et.Type == t }) {
			add("types", "unknown event type "+strconv.Quote(t))
		}
	}
	return errs
}

// WithDefaults returns values with the default of each empty field that has
// one. The adapter then sees the full form.
func (s Subscriptions) WithDefaults(values map[string][]string) map[string][]string {
	out := make(map[string][]string, len(values))
	for k, v := range values {
		out[k] = slices.Clone(v)
	}
	for _, f := range s.Fields {
		if len(out[f.Name]) == 0 && f.Default != "" {
			out[f.Name] = []string{f.Default}
		}
	}
	return out
}

// Answer applies the handshake rules. The echo rules go in the listed order
// and the first match answers; the ok rules apply only when no echo rule
// matched. A request that no rule answers is a normal delivery and gets nil,
// unless its method cannot carry one: then the answer is 405.
func (w Webhook) Answer(r HandshakeRequest) *HandshakeAnswer {
	for _, rule := range w.Handshake {
		kind, arg, _ := strings.Cut(rule, ":")
		switch kind {
		case "query_echo":
			if r.Query.Has(arg) {
				return &HandshakeAnswer{Status: http.StatusOK, ContentType: "text/plain", Body: r.Query.Get(arg), Forward: true}
			}
		case "json_echo":
			if val, ok := jsonEcho(r.Body, arg); ok {
				body, _ := json.Marshal(map[string]string{arg: val}) //nolint:errchkjson // a string map always encodes
				return &HandshakeAnswer{Status: http.StatusOK, ContentType: "application/json", Body: string(body), Forward: true}
			}
		case "header_echo":
			if vals := r.Headers.Values(arg); len(vals) > 0 {
				return &HandshakeAnswer{Status: http.StatusOK, Headers: map[string]string{arg: vals[0]}, Forward: true}
			}
		}
	}
	method := strings.ToUpper(r.Method)
	for _, rule := range w.Handshake {
		if rule == "ok:"+method {
			return &HandshakeAnswer{Status: http.StatusOK}
		}
	}
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return nil
	}
	return &HandshakeAnswer{Status: http.StatusMethodNotAllowed}
}

// jsonEcho reads field from a body that is a JSON object with only field,
// type, and token. An event with more keys is not a handshake.
func jsonEcho(body []byte, field string) (string, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return "", false
	}
	raw, ok := obj[field]
	if !ok {
		return "", false
	}
	for k := range obj {
		if k != field && k != "type" && k != "token" {
			return "", false
		}
	}
	var val string
	if json.Unmarshal(raw, &val) != nil {
		return "", false
	}
	return val, true
}

// linesOnly rejects the lines fields on a trebi-connector/1 events block.
func (v *validator) linesOnly(e Events) {
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"events.format", e.Format != ""}, {"events.poll", e.Poll != 0}, {"events.catchup", e.Catchup != nil},
	} {
		if f.set {
			v.add(f.field, "only with protocol: lines")
		}
	}
	for _, f := range e.Paths() {
		if f.Path != "" {
			v.add(f.Field, "only with protocol: lines")
		}
	}
}

// lines checks the fields of a lines connector.
func (v *validator) lines(e Events) {
	switch e.Format {
	case "", FormatJSON, FormatLine:
	default:
		v.add("events.format", "must be json or line")
	}
	if e.Poll > 0 && e.Poll.Std() < minPoll {
		v.add("events.poll", "must be at least 10s")
	}
	if e.Poll > 0 && e.Catchup != nil {
		v.add("events.catchup", "cannot be combined with poll")
	}
	paths := e.Paths()
	if e.Format != FormatJSON {
		for _, f := range paths {
			if f.Path != "" {
				v.add(f.Field, "needs format: json")
			}
		}
		if e.Catchup != nil {
			v.add("events.catchup", "needs format: json")
		}
	}
	for _, f := range paths {
		if f.Path != "" && !ValidPath(f.Path) {
			v.add(f.Field, "must be a dotted path")
		}
	}
	if e.Catchup != nil {
		v.catchup(e)
	}
}

func (v *validator) catchup(e Events) {
	c := e.Catchup
	if strings.TrimSpace(c.Command) == "" {
		v.add("events.catchup.command", "required")
	}
	if e.ID == "" {
		v.add("events.catchup", "needs events.id")
	}
	if c.MaxEvents < 0 {
		v.add("events.catchup.max_events", "must not be negative")
	}
	if c.Timeout > 0 && c.Timeout.Std() < time.Second {
		v.add("events.catchup.timeout", "must be at least 1s")
	}
}

func (v *validator) channel(m Manifest) {
	c := m.Channel
	if c == nil {
		return
	}
	if m.Protocol() != ProtocolConnector {
		v.add("channel", "needs events.protocol: "+ProtocolConnector)
	}
	for _, f := range c.Features {
		if !slices.Contains(ChannelFeatures(), f) {
			v.add("channel.features", "unknown feature "+strconv.Quote(f))
		}
	}
	if c.Limits.MaxText < 0 {
		v.add("channel.limits.max_text", "must not be negative")
	}
}

// ChannelFeatures lists every channel feature (Part D.5).
func ChannelFeatures() []string {
	return []string{
		FeatureRoomsList, FeatureRoomsOpen, FeatureThreads, FeatureThreadsCreate, FeatureHistory,
		FeatureReplay, FeatureTyping, FeatureSeen, FeatureReactions, FeatureEdit,
		FeatureAttachmentsIn, FeatureAttachmentsOut, FeatureReplies,
	}
}

// ValidPath reports whether path is a dotted JSON path.
func ValidPath(path string) bool {
	if path == "" {
		return false
	}
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return false
		}
		for _, r := range seg {
			if r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}

// relativePath reports whether p stays inside the entry folder.
func relativePath(p string) bool {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "~") {
		return false
	}
	clean := filepath.Clean(p)
	return clean != ".." && !strings.HasPrefix(clean, "../")
}

// pruneEmpty drops the empty strings, slices, and objects that the Go zero
// values leave in the JSON form, so the schema sees only the set fields.
func pruneEmpty(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		if arr, ok := v.([]any); ok {
			for _, x := range arr {
				pruneEmpty(x)
			}
		}
		return
	}
	for k, x := range m {
		pruneEmpty(x)
		switch t := x.(type) {
		case string:
			if t == "" {
				delete(m, k)
			}
		case map[string]any:
			if len(t) == 0 && k != "actions" {
				delete(m, k)
			}
		case []any:
			if len(t) == 0 {
				delete(m, k)
			}
		}
	}
}

// schemaError splits a jsonschema-go error into a dotted field and the last
// message: "validating /properties/channel/properties/limits: enum: …"
// becomes ("channel.limits", "enum: …").
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

func mergeValue(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = mergeValue(tm[k], v)
	}
	return tm
}
