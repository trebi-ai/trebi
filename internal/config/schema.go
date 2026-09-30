package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Key types in the config schema.
const (
	TypeString   = "string"
	TypeInt      = "int"
	TypeBool     = "bool"
	TypeDuration = "duration"
	TypeList     = "list"
	TypeMap      = "map"
	TypeJSON     = "json"
)

// SecretMask replaces a secret value in the YAML view. A PATCH with this
// value keeps the stored value.
const SecretMask = "***"

// Schema describes every settable config key for an editor.
type Schema struct {
	Sections []SchemaSection `json:"sections"`
	Keys     []SchemaKey     `json:"keys"`
}

// SchemaSection is one top-level block of config.yaml.
type SchemaSection struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// SchemaKey is one dotted key. Restart is true when the daemon reads the key
// only at start.
type SchemaKey struct {
	Key         string   `json:"key"`
	Section     string   `json:"section"`
	Type        string   `json:"type"`
	Enum        []string `json:"enum,omitempty"`
	Default     string   `json:"default"`
	Description string   `json:"description"`
	Secret      bool     `json:"secret"`
	Restart     bool     `json:"restart"`
}

// keyMeta is the schema data that PathEntry does not carry.
type keyMeta struct {
	typ     string
	enum    []string
	secret  bool
	restart bool
}

// NewSchema builds the schema from the built-in key list.
func NewSchema() Schema {
	sc := Schema{Sections: schemaSections(), Keys: []SchemaKey{}}
	for _, e := range (*Config)(nil).PathEntries(nil) {
		m := schemaMeta(e.Key)
		def := e.Default
		if def == "-" {
			def = ""
		}
		sc.Keys = append(sc.Keys, SchemaKey{
			Key: e.Key, Section: strings.SplitN(e.Key, ".", 2)[0], Type: m.typ, Enum: m.enum,
			Default: def, Description: e.Description, Secret: m.secret, Restart: m.restart,
		})
	}
	return sc
}

// Key returns the schema row of key.
func (s Schema) Key(key string) (SchemaKey, bool) {
	for _, k := range s.Keys {
		if k.Key == key {
			return k, true
		}
	}
	return SchemaKey{}, false
}

// Draft is a config change that is checked but never saved.
type Draft struct {
	Set   map[string]string
	Unset []string
	YAML  *string
}

// Check applies the draft to a copy of config.yaml and validates the result.
// With YAML set, the text replaces the file. It writes nothing.
func (d Draft) Check() []FieldError {
	cfg := defaultConfig()
	data := []byte{}
	if d.YAML != nil {
		data = []byte(*d.YAML)
	} else if b, err := os.ReadFile(ConfigPath()); err == nil {
		data = b
	} else if !os.IsNotExist(err) {
		return []FieldError{{Msg: err.Error()}}
	}
	if err := decodeConfig(data, cfg); err != nil {
		return []FieldError{{Msg: err.Error()}}
	}
	var errs []FieldError
	for _, k := range d.Unset {
		if err := cfg.UnsetPath(k); err != nil {
			errs = append(errs, FieldError{Field: k, Msg: err.Error()})
		}
	}
	for k, v := range d.Set {
		if err := cfg.SetPath(k, v); err != nil {
			errs = append(errs, FieldError{Field: k, Msg: err.Error()})
		}
	}
	cfg.applyDefaults()
	err := cfg.Validate()
	var ce *ConfigError
	switch {
	case errors.As(err, &ce):
		errs = append(errs, ce.Errors...)
	case err != nil:
		errs = append(errs, FieldError{Msg: err.Error()})
	}
	return errs
}

// RedactedYAML returns config.yaml with each secret value masked, and the
// SHA-256 of the file bytes. A missing file gives empty text.
func RedactedYAML() (text, hash string, err error) {
	data, err := os.ReadFile(ConfigPath())
	if os.IsNotExist(err) {
		data, err = nil, nil
	}
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(data)
	hash = hex.EncodeToString(sum[:])
	if len(strings.TrimSpace(string(data))) == 0 {
		return string(data), hash, nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return "", "", err
	}
	if !maskSecrets(&root, "") {
		return string(data), hash, nil
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		return "", "", err
	}
	return string(out), hash, nil
}

// maskSecrets masks the scalar values of secret keys under n. It reports
// whether it changed a value.
func maskSecrets(n *yaml.Node, prefix string) bool {
	if n.Kind == yaml.DocumentNode {
		changed := false
		for _, c := range n.Content {
			changed = maskSecrets(c, prefix) || changed
		}
		return changed
	}
	if n.Kind != yaml.MappingNode {
		return false
	}
	changed := false
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if prefix != "" {
			key = prefix + "." + key
		}
		v := n.Content[i+1]
		if schemaMeta(key).secret && v.Kind == yaml.ScalarNode && v.Value != "" {
			v.Value, v.Style, v.Tag = SecretMask, 0, "!!str"
			changed = true
			continue
		}
		changed = maskSecrets(v, key) || changed
	}
	return changed
}

func schemaSections() []SchemaSection {
	return []SchemaSection{
		{ID: "defaults", Title: "Defaults", Description: "Defaults for jobs and runs"},
		{ID: "api", Title: "API", Description: "HTTP bind address and TLS"},
		{ID: "terminal", Title: "Terminal", Description: "macOS terminal for run attach"},
		{ID: "tools", Title: "Tools", Description: "Harness pins and resource bridge"},
		{ID: "harness", Title: "Harness", Description: "Run runtime"},
		{ID: "notify", Title: "Notifications", Description: "Global alert route and escalation delays"},
		{ID: "files", Title: "Files", Description: "Workspace file limits"},
		{ID: "workspaces", Title: "Workspaces", Description: "Workspace root directory"},
		{ID: "assistant", Title: "Assistant", Description: "Workspace assistant settings"},
		{ID: "email", Title: "Email", Description: "Mail channel through the trebi-mail Worker"},
		{ID: "cloud", Title: "Cloud", Description: "Hosted account link"},
		{ID: "connectors", Title: "Connectors", Description: "MCP registries for the connector catalog"},
		{ID: "requests", Title: "Requests", Description: "How long a job waits for a person's answer"},
	}
}

// schemaMeta returns the type, enum, secret, and restart data of key.
func schemaMeta(key string) keyMeta {
	if m, ok := schemaMetas()[key]; ok {
		return m
	}
	parts := strings.Split(key, ".")
	if len(parts) >= 3 && parts[0] == "tools" && parts[1] != "bridge" {
		tool, rest := parts[1], strings.Join(parts[2:], ".")
		switch {
		case rest == "auth":
			return keyMeta{typ: TypeString, enum: []string{"shared", "separate"}}
		case rest == "runtime":
			return keyMeta{typ: TypeString, enum: []string{"cli", "tui"}}
		case rest == "effort":
			return keyMeta{typ: TypeString, enum: HarnessEffortLevels(tool)}
		case rest == "bridge.inherit":
			return keyMeta{typ: TypeBool}
		case strings.HasPrefix(rest, "bridge.resources."):
			return keyMeta{typ: TypeString, enum: []string{"inherit", "off"}}
		}
	}
	return keyMeta{typ: TypeString}
}

func schemaMetas() map[string]keyMeta {
	dur := keyMeta{typ: TypeDuration}
	restart := func(m keyMeta) keyMeta { m.restart = true; return m }
	return map[string]keyMeta{
		"defaults.session.timeout":             dur,
		"defaults.session.max_duration":        dur,
		"defaults.session.keep_alive":          dur,
		"defaults.session.nudge":               dur,
		"defaults.session.if_running":          {typ: TypeString, enum: []string{"queue", "skip", "replace"}},
		"api.host":                             restart(keyMeta{typ: TypeString}),
		"api.port":                             restart(keyMeta{typ: TypeInt}),
		"api.tls.cert":                         restart(keyMeta{typ: TypeString}),
		"api.tls.key":                          restart(keyMeta{typ: TypeString}),
		"api.tls.insecure":                     restart(keyMeta{typ: TypeBool}),
		"terminal":                             {typ: TypeString, enum: []string{"Terminal", "iTerm", "Ghostty"}},
		"tools.fallback":                       {typ: TypeString, enum: []string{"path", "require"}},
		"tools.bridge.inherit":                 {typ: TypeBool},
		"tools.bridge.only":                    {typ: TypeList},
		"tools.bridge.never":                   {typ: TypeList},
		"harness.runtime":                      {typ: TypeString, enum: []string{"cli", "tui"}},
		"notify.route":                         {typ: TypeJSON},
		"notify.waiting_after":                 restart(dur),
		"notify.approval_after":                restart(dur),
		"notify.usage_warn_at":                 restart(keyMeta{typ: TypeInt}),
		"files.max_bytes":                      restart(keyMeta{typ: TypeInt}),
		"requests.expire_after":                dur,
		"requests.retain":                      dur,
		"assistant.effort":                     {typ: TypeString, enum: []string{"low", "medium", "high"}},
		"assistant.mode":                       {typ: TypeString, enum: []string{"ask", "auto_edit", "auto"}},
		"assistant.idle_close":                 dur,
		"assistant.permission_timeout":         dur,
		"assistant.max_live":                   restart(keyMeta{typ: TypeInt}),
		"assistant.native.api_format":          {typ: TypeString, enum: []string{"openai", "anthropic"}},
		"assistant.api_key":                    {typ: TypeString, secret: true},
		"assistant.native.max_tokens":          {typ: TypeInt},
		"assistant.native.context_tokens":      {typ: TypeInt},
		"assistant.native.web_search":          {typ: TypeBool},
		"assistant.native.limits.daily_tokens": {typ: TypeInt},
		"assistant.native.limits.max_turns":    {typ: TypeInt},
		"email.worker":                         restart(keyMeta{typ: TypeString}),
		"email.token":                          restart(keyMeta{typ: TypeString}),
		"email.domain":                         restart(keyMeta{typ: TypeString}),
		"email.admins":                         restart(keyMeta{typ: TypeList}),
		"email.poll_interval":                  restart(dur),
		"email.require_dmarc":                  restart(keyMeta{typ: TypeBool}),
		"email.from_name":                      restart(keyMeta{typ: TypeString}),
		"cloud.api_url":                        restart(keyMeta{typ: TypeString}),
		"cloud.enabled":                        restart(keyMeta{typ: TypeBool}),
		"connectors.registries":                {typ: TypeJSON},
	}
}
