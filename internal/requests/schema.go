package requests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
)

// Schema is the flat form schema of a form request: one level of
// properties, each a string, number, integer, or boolean.
type Schema struct {
	Properties map[string]Field `json:"properties"`
	Required   []string         `json:"required,omitempty"`
}

type Field struct {
	Type        string   `json:"type"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Format      string   `json:"format,omitempty"` // multiline, for a string
}

const maxFields = 30

// ParseSchema reads a flat schema. Any keyword outside the flat subset is
// refused, so a nested schema never reaches a person.
func ParseSchema(raw json.RawMessage) (Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Schema{}, invalid("schema", "is required for a form request")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return Schema{}, invalid("schema", "must be a JSON object")
	}
	for k := range top {
		switch k {
		case "type", "properties", "required", "title", "description", "$schema":
		default:
			return Schema{}, invalid("schema", "keyword %q is not supported", k)
		}
	}
	if t, ok := top["type"]; ok && string(t) != `"object"` {
		return Schema{}, invalid("schema.type", "must be object")
	}
	var s Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return Schema{}, invalid("schema", "%v", err)
	}
	if len(s.Properties) == 0 {
		return Schema{}, invalid("schema.properties", "needs at least one field")
	}
	if len(s.Properties) > maxFields {
		return Schema{}, invalid("schema.properties", "has more than %d fields", maxFields)
	}
	var props map[string]map[string]json.RawMessage
	if err := json.Unmarshal(top["properties"], &props); err != nil {
		return Schema{}, invalid("schema.properties", "must be an object of objects")
	}
	for name, keys := range props {
		if err := s.Properties[name].validate(name, keys); err != nil {
			return Schema{}, err
		}
	}
	for _, r := range s.Required {
		if _, ok := s.Properties[r]; !ok {
			return Schema{}, invalid("schema.required", "%q is not a property", r)
		}
	}
	return s, nil
}

// validate checks one property: its keywords and its type.
func (f Field) validate(name string, keys map[string]json.RawMessage) error {
	for k := range keys {
		switch k {
		case "type", "title", "description", "enum", "format":
		default:
			return invalid("schema.properties."+name, "keyword %q is not supported", k)
		}
	}
	switch f.Type {
	case "string":
		if f.Format != "" && f.Format != "multiline" {
			return invalid("schema.properties."+name+".format", "must be multiline")
		}
	case "number", "integer", "boolean":
		if len(f.Enum) > 0 || f.Format != "" {
			return invalid("schema.properties."+name, "enum and format are only for a string")
		}
	default:
		return invalid("schema.properties."+name+".type", "must be string, number, integer, or boolean")
	}
	return nil
}

// Names returns the field names in sorted order.
func (s Schema) Names() []string {
	return slices.Sorted(maps.Keys(s.Properties))
}

// ValidateValues checks one submit against the schema.
func (s Schema) ValidateValues(raw json.RawMessage) error {
	values := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &values); err != nil {
			return invalid("values", "must be a JSON object")
		}
	}
	for _, r := range s.Required {
		v, ok := values[r]
		if !ok || v == nil || v == "" {
			return invalid("values."+r, "is required")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		f, ok := s.Properties[name]
		if !ok {
			return invalid("values."+name, "is not a field of the form")
		}
		if err := f.check(values[name]); err != nil {
			return invalid("values."+name, "%v", err)
		}
	}
	return nil
}

func (f Field) check(v any) error {
	if v == nil {
		return nil
	}
	switch f.Type {
	case "string":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("must be a string")
		}
		if len(f.Enum) > 0 && str != "" && !slices.Contains(f.Enum, str) {
			return fmt.Errorf("%q is not one of the choices", str)
		}
	case "number":
		if _, ok := v.(float64); !ok {
			return fmt.Errorf("must be a number")
		}
	case "integer":
		n, ok := v.(float64)
		if !ok || n != math.Trunc(n) {
			return fmt.Errorf("must be a whole number")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("must be true or false")
		}
	}
	return nil
}
