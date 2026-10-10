package manifest

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Operation is one named, typed action that the daemon runs for an agent or
// a live source. A run never gets the binary or the secrets of an operation.
type Operation struct {
	Name        string            `yaml:"name" json:"name"`
	Label       string            `yaml:"label" json:"label"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	ReadOnly    bool              `yaml:"readonly" json:"readonly"`
	Default     OpState           `yaml:"default,omitempty" json:"default,omitempty"`
	Command     []string          `yaml:"command" json:"command"`
	Stdin       string            `yaml:"stdin,omitempty" json:"stdin,omitempty"`
	Output      string            `yaml:"output,omitempty" json:"output,omitempty"`
	Input       *InputSchema      `yaml:"input,omitempty" json:"input,omitempty"`
	Expand      *Expand           `yaml:"expand,omitempty" json:"expand,omitempty"`
	Env         map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Timeout     Duration          `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// OpState is the state of one operation: on, ask, or off.
type OpState string

// InputSchema is the JSON Schema subset of an operation input: an object of
// scalar, enum, string array, and string map fields.
type InputSchema struct {
	Type       string                `yaml:"type" json:"type"`
	Properties map[string]InputField `yaml:"properties,omitempty" json:"properties,omitempty"`
	Required   []string              `yaml:"required,omitempty" json:"required,omitempty"`
}

// InputField is one property of an operation input.
type InputField struct {
	Type        string      `yaml:"type" json:"type"`
	Description string      `yaml:"description,omitempty" json:"description,omitempty"`
	Enum        []string    `yaml:"enum,omitempty" json:"enum,omitempty"`
	MaxLength   int         `yaml:"maxLength,omitempty" json:"maxLength,omitempty"`
	Minimum     *float64    `yaml:"minimum,omitempty" json:"minimum,omitempty"`
	Maximum     *float64    `yaml:"maximum,omitempty" json:"maximum,omitempty"`
	Items       *InputField `yaml:"items,omitempty" json:"items,omitempty"`
}

// Expand turns a map input into repeated argv elements, one group per key.
type Expand struct {
	Field string   `yaml:"field" json:"field"`
	Each  []string `yaml:"each" json:"each"`
}

// InputError lists the problems of one operation input.
type InputError struct {
	Operation string
	Fields    []FieldError
}

// Operation states and formats.
const (
	OpOn  OpState = "on"
	OpAsk OpState = "ask"
	OpOff OpState = "off"

	StdinJSON = "json"
	StdinNone = "none"

	OutputJSON = "json"
	OutputCSV  = "csv"
	OutputText = "text"

	DefaultOpTimeout = 30 * time.Second
	MaxOpTimeout     = 120 * time.Second
)

var (
	opNameRE      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	placeholderRE = regexp.MustCompile(`^\{input\.([a-z][a-z0-9_]{0,31})\}$`)
	expandKeyRE   = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,31}$`)
	braceRE       = regexp.MustCompile(`\{[^{}]*\}`)
)

// OpStates lists the operation states.
func OpStates() []OpState { return []OpState{OpOn, OpAsk, OpOff} }

// Valid reports whether s is a known state.
func (s OpState) Valid() bool { return slices.Contains(OpStates(), s) }

func (e *InputError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Message)
	}
	return fmt.Sprintf("operation %s: invalid input: %s", e.Operation, strings.Join(parts, "; "))
}

// DefaultState is the manifest default: on for a read-only operation, else
// off.
func (o Operation) DefaultState() OpState {
	if o.Default != "" {
		return o.Default
	}
	if o.ReadOnly {
		return OpOn
	}
	return OpOff
}

// EffectiveTimeout returns the timeout, or the default.
func (o Operation) EffectiveTimeout() time.Duration {
	if o.Timeout <= 0 {
		return DefaultOpTimeout
	}
	return o.Timeout.Std()
}

// OutputFormat returns the output format, or json.
func (o Operation) OutputFormat() string {
	if o.Output == "" {
		return OutputJSON
	}
	return o.Output
}

// StdinFormat returns the stdin format, or json.
func (o Operation) StdinFormat() string {
	if o.Stdin == "" {
		return StdinJSON
	}
	return o.Stdin
}

// Normalize checks in against the input schema and returns the typed values.
// A string for a number or a boolean field is parsed, so that a CLI k=v input
// works.
func (o Operation) Normalize(in map[string]any) (map[string]any, error) {
	out := map[string]any{}
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{Field: "input." + field, Message: msg}) }
	var props map[string]InputField
	var required []string
	if o.Input != nil {
		props, required = o.Input.Properties, o.Input.Required
	}
	for k, v := range in {
		f, ok := props[k]
		if !ok {
			add(k, "unknown field")
			continue
		}
		val, msg := f.normalize(v)
		if msg != "" {
			add(k, msg)
			continue
		}
		out[k] = val
	}
	for _, k := range required {
		if _, ok := in[k]; !ok {
			add(k, "required")
		}
	}
	if len(errs) > 0 {
		slices.SortFunc(errs, func(a, b FieldError) int { return strings.Compare(a.Field, b.Field) })
		return nil, &InputError{Operation: o.Name, Fields: errs}
	}
	return out, nil
}

// Argv checks in and builds the argv list. A placeholder is one whole argv
// element, so an input value never splits into more arguments.
func (o Operation) Argv(in map[string]any) ([]string, error) {
	vals, err := o.Normalize(in)
	if err != nil {
		return nil, err
	}
	argv := make([]string, 0, len(o.Command))
	for _, el := range o.Command {
		m := placeholderRE.FindStringSubmatch(el)
		if m == nil {
			argv = append(argv, el)
			continue
		}
		v, ok := vals[m[1]]
		if !ok {
			return nil, &InputError{Operation: o.Name, Fields: []FieldError{{Field: "input." + m[1], Message: "required"}}}
		}
		argv = append(argv, scalarText(v))
	}
	if o.Expand != nil {
		mp, _ := vals[o.Expand.Field].(map[string]any)
		keys := make([]string, 0, len(mp))
		for k := range mp {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			val := scalarText(mp[k])
			for _, el := range o.Expand.Each {
				argv = append(argv, strings.NewReplacer("{key}", k, "{value}", val).Replace(el))
			}
		}
	}
	return argv, nil
}

// StdinBytes returns the bytes for the stdin of the process, or nil.
func (o Operation) StdinBytes(in map[string]any) ([]byte, error) {
	if o.StdinFormat() != StdinJSON {
		return nil, nil
	}
	vals, err := o.Normalize(in)
	if err != nil {
		return nil, err
	}
	return json.Marshal(vals)
}

// Operation returns the operation with the name, or false.
func (m Manifest) Operation(name string) (Operation, bool) {
	if m.Actions == nil {
		return Operation{}, false
	}
	for _, op := range m.Actions.Operations {
		if op.Name == name {
			return op, true
		}
	}
	return Operation{}, false
}

// Operations returns the operations of the manifest.
func (m Manifest) Operations() []Operation {
	if m.Actions == nil {
		return nil
	}
	return m.Actions.Operations
}

// OperationBinaries returns the first argv element of each operation.
func (m Manifest) OperationBinaries() []string {
	var out []string
	for _, op := range m.Operations() {
		if len(op.Command) > 0 && !slices.Contains(out, op.Command[0]) {
			out = append(out, op.Command[0])
		}
	}
	return out
}

func (f InputField) normalize(v any) (any, string) {
	switch f.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, "must be a string"
		}
		if len(f.Enum) > 0 && !slices.Contains(f.Enum, s) {
			return nil, fmt.Sprintf("%q is not one of %s", s, strings.Join(f.Enum, ", "))
		}
		if f.MaxLength > 0 && len(s) > f.MaxLength {
			return nil, fmt.Sprintf("must be at most %d characters", f.MaxLength)
		}
		return s, ""
	case "integer", "number":
		n, ok := number(v)
		if !ok {
			return nil, "must be a number"
		}
		if f.Type == "integer" && n != math.Trunc(n) {
			return nil, "must be an integer"
		}
		if f.Minimum != nil && n < *f.Minimum {
			return nil, fmt.Sprintf("must be at least %v", *f.Minimum)
		}
		if f.Maximum != nil && n > *f.Maximum {
			return nil, fmt.Sprintf("must be at most %v", *f.Maximum)
		}
		if f.Type == "integer" {
			return int64(n), ""
		}
		return n, ""
	case "boolean":
		switch t := v.(type) {
		case bool:
			return t, ""
		case string:
			if b, err := strconv.ParseBool(t); err == nil {
				return b, ""
			}
		}
		return nil, "must be true or false"
	case "array":
		arr, ok := v.([]any)
		if !ok {
			if ss, ok2 := v.([]string); ok2 {
				for _, s := range ss {
					arr = append(arr, s)
				}
			} else {
				return nil, "must be a list"
			}
		}
		out := make([]any, 0, len(arr))
		item := InputField{Type: "string"}
		if f.Items != nil {
			item = *f.Items
		}
		for i, x := range arr {
			val, msg := item.normalize(x)
			if msg != "" {
				return nil, fmt.Sprintf("item %d %s", i, msg)
			}
			out = append(out, val)
		}
		return out, ""
	case "object":
		mp, ok := v.(map[string]any)
		if !ok {
			if ms, ok2 := v.(map[string]string); ok2 {
				mp = map[string]any{}
				for k, s := range ms {
					mp[k] = s
				}
			} else {
				return nil, "must be an object"
			}
		}
		out := map[string]any{}
		for k, x := range mp {
			if !expandKeyRE.MatchString(k) {
				return nil, fmt.Sprintf("key %q must match %s", k, expandKeyRE)
			}
			switch x.(type) {
			case string, bool, float64, int, int64, json.Number:
			default:
				return nil, fmt.Sprintf("key %q must have a text, number, or boolean value", k)
			}
			out[k] = x
		}
		return out, ""
	}
	return nil, "unknown type " + strconv.Quote(f.Type)
}

func number(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

func scalarText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	}
	return fmt.Sprint(v)
}

// operations checks actions.operations.
func (v *validator) operations(a *Actions) {
	seen := map[string]bool{}
	var cliBins []string
	if a.CLI != nil {
		for _, c := range a.CLI.Commands {
			cliBins = append(cliBins, filepath.Base(c))
		}
	}
	for i, op := range a.Operations {
		base := fmt.Sprintf("actions.operations[%d]", i)
		if !opNameRE.MatchString(op.Name) {
			v.add(base+".name", "must match "+opNameRE.String())
		}
		if seen[op.Name] {
			v.add(base+".name", "duplicate "+op.Name)
		}
		seen[op.Name] = true
		if strings.TrimSpace(op.Label) == "" {
			v.add(base+".label", "required")
		}
		if op.Default != "" && !op.Default.Valid() {
			v.add(base+".default", "must be on, ask, or off")
		}
		switch op.Stdin {
		case "", StdinJSON, StdinNone:
		default:
			v.add(base+".stdin", "must be json or none")
		}
		switch op.Output {
		case "", OutputJSON, OutputCSV, OutputText:
		default:
			v.add(base+".output", "must be json, csv, or text")
		}
		if op.Timeout < 0 || op.Timeout.Std() > MaxOpTimeout {
			v.add(base+".timeout", "must be at most 120s")
		}
		for k := range op.Env {
			if !envKeyRE.MatchString(k) {
				v.add(base+".env", fmt.Sprintf("key %q must match %s", k, envKeyRE))
			}
		}
		v.operationInput(base, op)
		v.operationCommand(base, op)
		if len(op.Command) > 0 && slices.Contains(cliBins, filepath.Base(op.Command[0])) {
			v.add(base+".command", "an operation binary must not also be a run command")
		}
	}
}

func (v *validator) operationInput(base string, op Operation) {
	in := op.Input
	if in == nil {
		if op.Expand != nil {
			v.add(base+".expand.field", "needs an input field of type object")
		}
		return
	}
	if in.Type != "object" {
		v.add(base+".input.type", "must be object")
	}
	for name, f := range in.Properties {
		field := base + ".input.properties." + name
		if !opNameRE.MatchString(name) {
			v.add(field, "name must match "+opNameRE.String())
		}
		switch f.Type {
		case "string", "integer", "number", "boolean", "object":
		case "array":
			if f.Items != nil && f.Items.Type != "string" {
				v.add(field+".items.type", "must be string")
			}
		default:
			v.add(field+".type", "must be string, integer, number, boolean, array, or object")
		}
		if len(f.Enum) > 0 && f.Type != "string" {
			v.add(field+".enum", "needs type string")
		}
	}
	for _, r := range in.Required {
		if _, ok := in.Properties[r]; !ok {
			v.add(base+".input.required", "unknown field "+strconv.Quote(r))
		}
	}
	if e := op.Expand; e != nil {
		if f, ok := in.Properties[e.Field]; !ok || f.Type != "object" {
			v.add(base+".expand.field", "needs an input field of type object")
		}
		if len(e.Each) == 0 {
			v.add(base+".expand.each", "needs at least one entry")
		}
		for _, el := range e.Each {
			for _, tok := range braceRE.FindAllString(el, -1) {
				if tok != "{key}" && tok != "{value}" {
					v.add(base+".expand.each", "unknown placeholder "+tok)
				}
			}
		}
	}
}

func (v *validator) operationCommand(base string, op Operation) {
	if len(op.Command) == 0 || strings.TrimSpace(op.Command[0]) == "" {
		v.add(base+".command", "needs a program")
		return
	}
	if strings.Contains(op.Command[0], "{") {
		v.add(base+".command", "the program must not be a placeholder")
	}
	for i, el := range op.Command[1:] {
		field := fmt.Sprintf("%s.command[%d]", base, i+1)
		m := placeholderRE.FindStringSubmatch(el)
		if m == nil {
			if strings.Contains(el, "{input.") {
				v.add(field, "a placeholder must be a whole argv element")
			}
			continue
		}
		var f InputField
		var ok bool
		if op.Input != nil {
			f, ok = op.Input.Properties[m[1]]
		}
		switch {
		case !ok:
			v.add(field, "unknown input field "+strconv.Quote(m[1]))
		case f.Type == "array" || f.Type == "object":
			v.add(field, "a placeholder needs a text, number, or boolean field")
		case !slices.Contains(op.Input.Required, m[1]):
			v.add(field, "a placeholder field must be required")
		}
	}
}
