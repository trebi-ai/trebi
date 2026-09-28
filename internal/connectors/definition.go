package connectors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/secrets"
	"gopkg.in/yaml.v3"
)

// ErrInvalid marks a definition that failed validation. The API maps it to
// 400 validation and lists the field errors in details.
var ErrInvalid = errors.New("invalid connector")

// FieldError is one validation problem on a named field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError lists every field error of one definition.
type ValidationError struct {
	Name   string
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Message)
	}
	name := e.Name
	if name == "" {
		name = "connector"
	}
	return fmt.Sprintf("%s %s", name, strings.Join(parts, "; "))
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

// Duration is a YAML text duration ("15m") with a JSON millisecond number,
// which matches the _ms wire rule.
type Duration time.Duration

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

// MarshalJSON writes milliseconds.
func (d Duration) MarshalJSON() ([]byte, error) {
	if d <= 0 {
		return []byte("null"), nil
	}
	return strconv.AppendInt(nil, int64(time.Duration(d)/time.Millisecond), 10), nil
}

// UnmarshalJSON reads milliseconds, or null.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*d = 0
		return nil
	}
	var ms int64
	if err := json.Unmarshal(b, &ms); err != nil {
		return err
	}
	*d = Duration(time.Duration(ms) * time.Millisecond)
	return nil
}

// Definition is one connector from connectors.yaml: one external system with
// up to three optional capabilities.
type Definition struct {
	Name    string            `yaml:"-" json:"name"`
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Cwd     string            `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Actions *Actions          `yaml:"actions,omitempty" json:"actions,omitempty"`
	Events  *Events           `yaml:"events,omitempty" json:"events,omitempty"`
	Channel *Channel          `yaml:"channel,omitempty" json:"channel,omitempty"`
	Source  *Source           `yaml:"source,omitempty" json:"source,omitempty"`
}

// clone returns a deep copy: every pointer block, map, and slice is new.
func (d Definition) clone() Definition {
	d.Env = maps.Clone(d.Env)
	if d.Actions != nil {
		a := *d.Actions
		if a.MCP != nil {
			m := *a.MCP
			m.Args = slices.Clone(m.Args)
			m.Env = maps.Clone(m.Env)
			m.Headers = maps.Clone(m.Headers)
			a.MCP = &m
		}
		if a.CLI != nil {
			c := *a.CLI
			c.Commands = slices.Clone(c.Commands)
			a.CLI = &c
		}
		d.Actions = &a
	}
	if d.Events != nil {
		e := *d.Events
		if e.Catchup != nil {
			c := *e.Catchup
			e.Catchup = &c
		}
		d.Events = &e
	}
	if d.Channel != nil {
		c := *d.Channel
		d.Channel = &c
	}
	if d.Source != nil {
		src := *d.Source
		d.Source = &src
	}
	return d
}

// Actions is the "the agent can act on the system" capability.
type Actions struct {
	MCP *MCPAction `yaml:"mcp,omitempty" json:"mcp,omitempty"`
	CLI *CLIAction `yaml:"cli,omitempty" json:"cli,omitempty"`
}

// MCPAction supplies actions through an MCP server (stdio or HTTP).
type MCPAction struct {
	Command string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	URL     string            `yaml:"url,omitempty" json:"url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// fingerprint identifies the server for inherited-row merging; it uses the
// same rules as the harness MCP discovery so one server is one row.
func (m MCPAction) fingerprint() string {
	transport := "stdio"
	if m.URL != "" {
		transport = "http"
	}
	return harness.MCPFingerprint(transport, m.Command, m.Args, m.URL)
}

// CLIAction supplies actions through local commands plus a skill.
type CLIAction struct {
	Commands []string `yaml:"commands" json:"commands"`
	Skill    string   `yaml:"skill,omitempty" json:"skill,omitempty"`
}

// Events is the "the system can start work" capability.
type Events struct {
	Command  string   `yaml:"command" json:"command"`
	Format   string   `yaml:"format,omitempty" json:"format,omitempty"`   // json|line
	Restart  string   `yaml:"restart,omitempty" json:"restart,omitempty"` // always|on-failure|never
	Poll     Duration `yaml:"poll,omitempty" json:"poll_ms,omitempty"`
	ID       string   `yaml:"id,omitempty" json:"id,omitempty"`             // path to the source event id
	TS       string   `yaml:"ts,omitempty" json:"ts,omitempty"`             // path used to sort catch-up output
	Identity string   `yaml:"identity,omitempty" json:"identity,omitempty"` // path to the sender for allowlists
	Catchup  *Catchup `yaml:"catchup,omitempty" json:"catchup,omitempty"`
}

// Catchup replays events the connector emitted while the daemon was down.
type Catchup struct {
	Command   string   `yaml:"command" json:"command"`
	MaxEvents int      `yaml:"max_events,omitempty" json:"max_events,omitempty"`
	Timeout   Duration `yaml:"timeout,omitempty" json:"timeout_ms,omitempty"`
}

// Channel is the "the system can hold a conversation" capability.
type Channel struct {
	Ref            string   `yaml:"ref" json:"ref"`
	Send           string   `yaml:"send" json:"send"`
	Typing         string   `yaml:"typing,omitempty" json:"typing,omitempty"`
	TypingInterval Duration `yaml:"typing_interval,omitempty" json:"typing_interval_ms,omitempty"`
	TypingMax      Duration `yaml:"typing_max,omitempty" json:"typing_max_ms,omitempty"`
	Ack            string   `yaml:"ack,omitempty" json:"ack,omitempty"`
}

// Channel modes. A reply channel answers conversations and also pushes; a
// send-only channel (no ref, no events) only pushes.
const (
	ModeReply = "reply"
	ModePush  = "push"
)

// SendOnly reports whether the channel has no ref, so it only pushes.
func (c Channel) SendOnly() bool { return strings.TrimSpace(c.Ref) == "" }

// Modes lists what the channel does.
func (c Channel) Modes() []string {
	if c.SendOnly() {
		return []string{ModePush}
	}
	return []string{ModeReply, ModePush}
}

// Source records where a registry install came from.
type Source struct {
	Registry string   `yaml:"registry" json:"registry"`
	Name     string   `yaml:"name" json:"name"`
	Version  string   `yaml:"version" json:"version"`
	Secrets  []string `yaml:"secrets,omitempty" json:"secrets,omitempty"` // names this install wrote
}

// refersToSecret reports whether an env value or MCP header of d names the
// secret. A value resolves as a secret by its secret: reference or by its
// bare name (secrets.ExpandRefs).
func (d Definition) refersToSecret(name string) bool {
	values := []map[string]string{d.Env}
	if d.Actions != nil && d.Actions.MCP != nil {
		values = append(values, d.Actions.MCP.Env, d.Actions.MCP.Headers)
	}
	for _, m := range values {
		for _, v := range m {
			if v == name || v == "secret:"+name {
				return true
			}
		}
	}
	return false
}

// Connector name rules: lowercase, digits, dashes, at most 40 characters.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// envKeyRE is an environment variable name.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reserved names belong to a native channel, not to a subprocess. The row
// for one is synthesized by its service and is invisible to Get, so without
// GuardReserved a write would shadow it. "trebi" is the MCP server name of
// the harness bridge.
var reserved = map[string]bool{"email": true, "webhook": true, "trebi": true} //nolint:gochecknoglobals // fixed set of native names, never mutated

// Format and restart enums.
const (
	FormatJSON = "json"
	FormatLine = "line"

	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"
)

// Catch-up and typing defaults (Part B.1).
const (
	defaultMaxEvents      = 500
	defaultCatchupTimeout = 2 * time.Minute
	defaultTypingInterval = 8 * time.Second
	defaultTypingMax      = 15 * time.Minute
	minPoll               = 10 * time.Second
	minTypingInterval     = time.Second
)

// Validate checks every rule of Part B.1 and returns a joined list of field
// errors. It does not expand secret references; that happens at use time.
func (d Definition) Validate() error {
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{Field: field, Message: msg}) }

	if !nameRE.MatchString(d.Name) {
		add("name", "must match "+nameRE.String())
	}
	if reserved[d.Name] {
		add("name", "is reserved")
	}
	if d.Actions == nil && d.Events == nil && d.Channel == nil {
		add("", "at least one of actions, events, channel is required")
	}
	for k := range d.Env {
		if !envKeyRE.MatchString(k) {
			add("env", fmt.Sprintf("key %q must match %s", k, envKeyRE))
		}
	}
	if d.Actions != nil {
		if d.Actions.MCP == nil && d.Actions.CLI == nil {
			add("actions", "at least one of mcp, cli is required")
		}
		if m := d.Actions.MCP; m != nil {
			if (m.Command == "") == (m.URL == "") {
				add("actions.mcp", "exactly one of command, url is required")
			}
			if m.URL == "" && len(m.Headers) > 0 {
				add("actions.mcp.headers", "needs url")
			}
			if m.Command == "" && len(m.Env) > 0 {
				add("actions.mcp.env", "needs command")
			}
			if m.Command == "" && len(m.Args) > 0 {
				add("actions.mcp.args", "needs command")
			}
			for k := range m.Env {
				if !envKeyRE.MatchString(k) {
					add("actions.mcp.env", fmt.Sprintf("key %q must match %s", k, envKeyRE))
				}
			}
		}
		if c := d.Actions.CLI; c != nil {
			if len(c.Commands) == 0 {
				add("actions.cli.commands", "needs at least one entry")
			}
		}
	}
	if d.Source != nil {
		for _, n := range d.Source.Secrets {
			if err := secrets.CheckName(n); err != nil {
				add("source.secrets", err.Error())
			}
		}
	}
	if e := d.Events; e != nil {
		if strings.TrimSpace(e.Command) == "" {
			add("events.command", "required")
		}
		switch e.Format {
		case "", FormatJSON, FormatLine:
		default:
			add("events.format", "must be json or line")
		}
		switch e.Restart {
		case "", RestartAlways, RestartOnFailure, RestartNever:
		default:
			add("events.restart", "must be always, on-failure, or never")
		}
		if e.Poll > 0 && e.Poll.Std() < minPoll {
			add("events.poll", "must be at least 10s")
		}
		if e.Poll > 0 && e.Catchup != nil {
			add("events.catchup", "cannot be combined with poll")
		}
		if e.Format != FormatJSON {
			for _, f := range []struct{ field, path string }{
				{"events.id", e.ID}, {"events.ts", e.TS}, {"events.identity", e.Identity},
			} {
				if f.path != "" {
					add(f.field, "needs format: json")
				}
			}
			if e.Catchup != nil {
				add("events.catchup", "needs format: json")
			}
		}
		if e.Catchup != nil {
			if strings.TrimSpace(e.Catchup.Command) == "" {
				add("events.catchup.command", "required")
			}
			if e.ID == "" {
				add("events.catchup", "needs events.id")
			}
			if e.Catchup.MaxEvents < 0 {
				add("events.catchup.max_events", "must not be negative")
			}
			if t := e.Catchup.Timeout; t > 0 && t.Std() < time.Second {
				add("events.catchup.timeout", "must be at least 1s")
			}
		}
		for _, f := range []struct{ field, path string }{
			{"events.id", e.ID}, {"events.ts", e.TS}, {"events.identity", e.Identity},
		} {
			if f.path != "" && !validPath(f.path) {
				add(f.field, "must be a dotted path")
			}
		}
	}
	if c := d.Channel; c != nil {
		switch {
		case d.Events != nil || !c.SendOnly():
			// A reply channel reads its ref from the events.
			if d.Events == nil || d.Events.Format != FormatJSON {
				add("channel", "needs events with format: json")
			}
			if strings.TrimSpace(c.Ref) == "" {
				add("channel.ref", "required")
			} else if !validPath(c.Ref) {
				add("channel.ref", "must be a dotted path")
			}
		default:
			if strings.TrimSpace(c.Typing) != "" {
				add("channel.typing", "needs events; a send-only channel has no typing")
			}
			if strings.TrimSpace(c.Ack) != "" {
				add("channel.ack", "needs events; a send-only channel has no ack")
			}
		}
		if strings.TrimSpace(c.Send) == "" {
			add("channel.send", "required")
		}
		if t := c.TypingInterval; t > 0 && t.Std() < minTypingInterval {
			add("channel.typing_interval", "must be at least 1s")
		}
		if t := c.TypingMax; t < 0 {
			add("channel.typing_max", "must not be negative")
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Name: d.Name, Fields: errs}
}

// Capabilities lists the blocks present, in the fixed order.
func (d Definition) Capabilities() []string {
	out := make([]string, 0, 3)
	if d.Actions != nil {
		out = append(out, "actions")
	}
	if d.Events != nil {
		out = append(out, "events")
	}
	if d.Channel != nil {
		out = append(out, "channel")
	}
	return out
}

// withDefaults fills the declared defaults. Call it after Validate.
func (d Definition) withDefaults() Definition {
	if d.Events != nil {
		e := *d.Events
		if e.Format == "" {
			e.Format = FormatLine
		}
		if e.Restart == "" {
			e.Restart = RestartOnFailure
		}
		if e.Catchup != nil {
			cu := *e.Catchup
			if cu.MaxEvents == 0 {
				cu.MaxEvents = defaultMaxEvents
			}
			if cu.Timeout == 0 {
				cu.Timeout = Duration(defaultCatchupTimeout)
			}
			e.Catchup = &cu
		}
		d.Events = &e
	}
	if d.Channel != nil {
		c := *d.Channel
		if c.TypingInterval == 0 {
			c.TypingInterval = Duration(defaultTypingInterval)
		}
		if c.TypingMax == 0 {
			c.TypingMax = Duration(defaultTypingMax)
		}
		d.Channel = &c
	}
	return d
}

// decode parses one output line per the declared format. A json connector
// returns nil for a line that is not valid json.
func (e Events) decode(line string) any {
	if e.Format == FormatJSON {
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

// validPath reports whether path is a dotted JSON path.
func validPath(path string) bool {
	if path == "" {
		return false
	}
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return false
		}
		allDigits := true
		for _, r := range seg {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			continue
		}
		for _, r := range seg {
			if !(r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
				return false
			}
		}
	}
	return true
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
// scalar. It is the identityString of the old allowlist plus string handling
// for bool.
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

// parseDefinitionData decodes strict YAML/JSON data into named definitions.
// An unknown key fails with the connector name and its line in the file.
func parseDefinitionData(data []byte) (map[string]Definition, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := map[string]Definition{}
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
	for name, d := range out {
		d.Name = name
		out[name] = d
	}
	return out, nil
}

// sortedNames returns the map keys sorted.
func sortedNames(defs map[string]Definition) []string {
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LoadDefinitions reads and validates connectors.yaml without a Service. A
// missing file is an empty set. The doctor uses it to check kit names before
// a connector ever runs.
func LoadDefinitions() (map[string]Definition, error) {
	return readDefinitions()
}
