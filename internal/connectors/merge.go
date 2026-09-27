package connectors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// maskValue is the wire value of a literal that GET does not show.
const maskValue = "***"

// maskValues hides each literal value. A value that holds a secret reference
// stays visible, because it holds no secret.
func maskValues(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if v == "" || strings.Contains(v, "secret:") {
			out[k] = v
			continue
		}
		out[k] = maskValue
	}
	return out
}

// keepMasked puts back the stored value of each env value and MCP header that
// comes back as the mask. A mask with no stored value is a validation error.
func (d *Definition) keepMasked(prev Definition) error {
	var fields []FieldError
	restore := func(field string, next, old map[string]string) {
		for k, v := range next {
			if v != maskValue {
				continue
			}
			stored, ok := old[k]
			if !ok || stored == maskValue {
				fields = append(fields, FieldError{Field: field, Message: fmt.Sprintf("%s: %q has no stored value", k, maskValue)})
				continue
			}
			next[k] = stored
		}
	}
	d.Env = maps.Clone(d.Env)
	restore("env", d.Env, prev.Env)
	if d.Actions != nil && d.Actions.MCP != nil {
		var oldEnv, oldHeaders map[string]string
		if prev.Actions != nil && prev.Actions.MCP != nil {
			oldEnv, oldHeaders = prev.Actions.MCP.Env, prev.Actions.MCP.Headers
		}
		m := *d.Actions.MCP
		m.Env, m.Headers = maps.Clone(m.Env), maps.Clone(m.Headers)
		restore("actions.mcp.env", m.Env, oldEnv)
		restore("actions.mcp.headers", m.Headers, oldHeaders)
		a := *d.Actions
		a.MCP = &m
		d.Actions = &a
	}
	if len(fields) > 0 {
		return &ValidationError{Name: d.Name, Fields: fields}
	}
	return nil
}

// actionsMerge is the actions_merge body of a PATCH. An absent sub-block
// stays, a null sub-block goes, and an object merges field by field.
type actionsMerge struct {
	MCP json.RawMessage `json:"mcp"`
	CLI json.RawMessage `json:"cli"`
}

// mcpMerge holds the MCP fields that a merge sets. A nil field stays.
type mcpMerge struct {
	Command *string            `json:"command"`
	Args    *[]string          `json:"args"`
	Env     *map[string]string `json:"env"`
	URL     *string            `json:"url"`
	Headers *map[string]string `json:"headers"`
}

// cliMerge holds the CLI fields that a merge sets. A nil field stays.
type cliMerge struct {
	Commands *[]string `json:"commands"`
	Skill    *string   `json:"skill"`
}

// apply returns a copy of a with the merge done. The result is nil when no
// sub-block is left.
func (m actionsMerge) apply(name string, a *Actions) (*Actions, error) {
	var out Actions
	if a != nil {
		out = *a
	}
	if len(m.MCP) > 0 {
		if isNull(m.MCP) {
			out.MCP = nil
		} else {
			var mm mcpMerge
			if err := decodeStrict(m.MCP, &mm); err != nil {
				return nil, &ValidationError{Name: name, Fields: []FieldError{{Field: "actions_merge.mcp", Message: err.Error()}}}
			}
			out.MCP = mm.apply(out.MCP)
		}
	}
	if len(m.CLI) > 0 {
		if isNull(m.CLI) {
			out.CLI = nil
		} else {
			var cm cliMerge
			if err := decodeStrict(m.CLI, &cm); err != nil {
				return nil, &ValidationError{Name: name, Fields: []FieldError{{Field: "actions_merge.cli", Message: err.Error()}}}
			}
			out.CLI = cm.apply(out.CLI)
		}
	}
	if out.MCP == nil && out.CLI == nil {
		return nil, nil
	}
	return &out, nil
}

// apply returns a merged copy of prev. A new command clears the HTTP fields,
// and a new url clears the stdio fields, so the transport switches cleanly.
func (m mcpMerge) apply(prev *MCPAction) *MCPAction {
	var out MCPAction
	if prev != nil {
		out = *prev
	}
	if m.Command != nil && *m.Command != "" {
		out.URL, out.Headers = "", nil
	}
	if m.URL != nil && *m.URL != "" {
		out.Command, out.Args, out.Env = "", nil, nil
	}
	if m.Command != nil {
		out.Command = *m.Command
	}
	if m.Args != nil {
		out.Args = *m.Args
	}
	if m.Env != nil {
		out.Env = *m.Env
	}
	if m.URL != nil {
		out.URL = *m.URL
	}
	if m.Headers != nil {
		out.Headers = *m.Headers
	}
	return &out
}

// apply returns a merged copy of prev.
func (m cliMerge) apply(prev *CLIAction) *CLIAction {
	var out CLIAction
	if prev != nil {
		out = *prev
	}
	if m.Commands != nil {
		out.Commands = *m.Commands
	}
	if m.Skill != nil {
		out.Skill = *m.Skill
	}
	return &out
}

func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
