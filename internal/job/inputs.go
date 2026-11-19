package job

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Input types of the inputs block.
const (
	InputString  = "string"
	InputNumber  = "number"
	InputInteger = "integer"
	InputBoolean = "boolean"
)

// FormatMultiline is the format of a string input that takes more than one line.
const FormatMultiline = "multiline"

// Sources of a filled input value.
const (
	SourceGiven   = "given"
	SourceMap     = "map"
	SourceName    = "name"
	SourceDefault = "default"
	SourceLiteral = "literal"
	SourceEmpty   = "empty"
)

// inputKeys are the keys of one input entry.
var inputKeys = []string{"type", "label", "help", "required", "default", "enum", "format", "min", "max"} //nolint:gochecknoglobals // static key list

var inputNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// templateRe finds one {{name}} of a trigger input value. A name can be a
// dotted data path.
var templateRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\}\}`)

// Input is one typed value that a job asks for. The inputs block of JOB.md
// declares it; a {{placeholder}} that the block does not declare is an input
// with Declared false and type string.
type Input struct {
	Name     string   `json:"name"`
	Label    string   `json:"label,omitempty"`
	Help     string   `json:"help,omitempty"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Default  any      `json:"default,omitempty"`
	Enum     []string `json:"enum,omitempty"`
	Format   string   `json:"format,omitempty"`
	Min      *float64 `json:"min,omitempty"`
	Max      *float64 `json:"max,omitempty"`
	Declared bool     `json:"declared"`
	// faults are the problems of the entry in the file. Issues reports them.
	faults []Issue
}

// Title is the label of the input, else its name.
func (in Input) Title() string {
	if in.Label != "" {
		return in.Label
	}
	return in.Name
}

// Validate returns the first fault of the declaration.
func (in Input) Validate() error {
	if len(in.faults) > 0 {
		return errors.New(in.faults[0].Message)
	}
	return nil
}

// Coerce converts v to the type of the input and checks enum, min, and max.
// An empty value is nil. A required check is the job of the caller.
func (in Input) Coerce(v any) (any, error) {
	if blank(v) {
		return nil, nil
	}
	switch in.Type {
	case InputNumber, InputInteger:
		return in.coerceNumber(v)
	case InputBoolean:
		b, err := toBool(v)
		if err != nil {
			return nil, fmt.Errorf("%s: must be true or false", in.Title())
		}
		return b, nil
	}
	s := RenderValue(v)
	if len(in.Enum) > 0 && !slices.Contains(in.Enum, s) {
		return nil, fmt.Errorf("%s: must be one of %s", in.Title(), strings.Join(in.Enum, ", "))
	}
	return s, nil
}

func (in Input) coerceNumber(v any) (any, error) {
	n, err := toNumber(v)
	if err != nil {
		return nil, fmt.Errorf("%s: must be a number", in.Title())
	}
	if in.Type == InputInteger && n != math.Trunc(n) {
		return nil, fmt.Errorf("%s: must be a whole number", in.Title())
	}
	if in.Min != nil && n < *in.Min {
		return nil, fmt.Errorf("%s: must be at least %s", in.Title(), RenderValue(*in.Min))
	}
	if in.Max != nil && n > *in.Max {
		return nil, fmt.Errorf("%s: must be at most %s", in.Title(), RenderValue(*in.Max))
	}
	if in.Type == InputInteger {
		return int64(n), nil
	}
	return n, nil
}

func (in *Input) fault(path, code, msg string) {
	in.faults = append(in.faults, Issue{Path: path, Code: code, Severity: SeverityError, Message: msg})
}

// setKey sets one key of the entry. known is false for a key that inputs
// do not have.
func (in *Input) setKey(key string, val *yaml.Node) (known bool, err error) {
	switch key {
	case "type":
		in.Type = val.Value
	case "label":
		err = val.Decode(&in.Label)
	case "help":
		err = val.Decode(&in.Help)
	case "required":
		err = val.Decode(&in.Required)
	case "enum":
		err = val.Decode(&in.Enum)
	case "format":
		err = val.Decode(&in.Format)
	case "min":
		in.Min, err = decodeFloat(val)
	case "max":
		in.Max, err = decodeFloat(val)
	default:
		return false, nil
	}
	return true, err
}

// checkShape checks that the keys fit the type.
func (in *Input) checkShape(path string) {
	switch in.Type {
	case InputString, InputNumber, InputInteger, InputBoolean:
	default:
		in.fault(path+".type", CodeInputType, fmt.Sprintf("input %q: type %q is not known; use string, number, integer, or boolean", in.Name, in.Type))
		in.Type = InputString
	}
	if len(in.Enum) > 0 && in.Type != InputString {
		in.fault(path+".enum", CodeInputInvalid, fmt.Sprintf("input %q: choices apply only to a string input", in.Name))
	}
	if in.Format != "" && (in.Format != FormatMultiline || in.Type != InputString) {
		in.fault(path+".format", CodeInputInvalid, fmt.Sprintf("input %q: format must be %q on a string input", in.Name, FormatMultiline))
	}
	if (in.Min != nil || in.Max != nil) && in.Type != InputNumber && in.Type != InputInteger {
		in.fault(path, CodeInputInvalid, fmt.Sprintf("input %q: min and max apply only to a number input", in.Name))
	}
}

// Inputs is the inputs block in file order.
type Inputs []Input

// UnmarshalYAML reads a mapping of input name to its entry. It keeps the
// file order. A fault in one entry stays on the entry for Issues.
func (l *Inputs) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("inputs: want a map of input name to its keys")
	}
	out := make(Inputs, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, parseInput(n.Content[i].Value, n.Content[i+1]))
	}
	*l = out
	return nil
}

// MarshalYAML writes the block back as a mapping in order.
func (l Inputs) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode, Style: 0}
	for _, in := range l {
		val := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
		add := func(k string, v any) {
			var vn yaml.Node
			if err := vn.Encode(v); err == nil {
				val.Content = append(val.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, &vn)
			}
		}
		add("type", in.Type)
		if in.Label != "" {
			add("label", in.Label)
		}
		if in.Help != "" {
			add("help", in.Help)
		}
		if in.Required {
			add("required", true)
		}
		if len(in.Enum) > 0 {
			add("enum", in.Enum)
		}
		if in.Default != nil {
			add("default", in.Default)
		}
		if in.Format != "" {
			add("format", in.Format)
		}
		if in.Min != nil {
			add("min", *in.Min)
		}
		if in.Max != nil {
			add("max", *in.Max)
		}
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: in.Name}, val)
	}
	return node, nil
}

// Get returns the declared input of a name.
func (l Inputs) Get(name string) (Input, bool) {
	for _, in := range l {
		if in.Name == name {
			return in, true
		}
	}
	return Input{}, false
}

// InputValue is one entry of the inputs map of a trigger.
type InputValue struct {
	Name  string
	Value string
}

// IsTemplate reports whether the value reads the event ({{ }}).
func (v InputValue) IsTemplate() bool { return strings.Contains(v.Value, "{{") }

// InputMap is the inputs map of a trigger in file order.
type InputMap []InputValue

// Get returns the value of a name.
func (m InputMap) Get(name string) (InputValue, bool) {
	for _, v := range m {
		if v.Name == name {
			return v, true
		}
	}
	return InputValue{}, false
}

// UnmarshalYAML reads a mapping of input name to a value in order.
func (m *InputMap) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("inputs: want a map of input name to a value")
	}
	out := make(InputMap, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		val := n.Content[i+1]
		if val.Kind != yaml.ScalarNode {
			return fmt.Errorf("inputs.%s: want a text value", n.Content[i].Value)
		}
		out = append(out, InputValue{Name: n.Content[i].Value, Value: val.Value})
	}
	*m = out
	return nil
}

// MarshalYAML writes the map as a flow mapping in order.
func (m InputMap) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
	for _, v := range m {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: v.Name},
			&yaml.Node{Kind: yaml.ScalarNode, Value: v.Value})
	}
	return node, nil
}

// MarshalJSON writes an object in order.
func (m InputMap) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, v := range m {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(v.Name)
		val, _ := json.Marshal(v.Value)
		b.Write(k)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON reads an object in order. A value that is not a string
// keeps its JSON text.
func (m *InputMap) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		*m = nil
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("inputs: want an object of input name to a value")
	}
	out := InputMap{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		var raw any
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		out = append(out, InputValue{Name: kt.(string), Value: RenderValue(raw)})
	}
	*m = out
	return nil
}

// EventVars is the event side of a fill: the built-in and extract vars, and
// the decoded event.
type EventVars struct {
	Vars    map[string]string
	Payload any
}

// Render fills the {{ }} parts of a template. A name is a var, then a data
// path. A name that the event does not have renders empty.
func (ev EventVars) Render(tmpl string) string {
	return templateRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		name := templateRe.FindStringSubmatch(m)[1]
		if v, ok := ev.Vars[name]; ok {
			return v
		}
		if v, ok := ev.field(name); ok {
			return RenderValue(v)
		}
		return ""
	})
}

// field reads a dotted path from the event. A path that starts with data.
// reads the data object; when the event has no data object, the path reads
// the top level.
func (ev EventVars) field(path string) (any, bool) {
	if v, ok := lookupValue(ev.Payload, path); ok {
		return v, true
	}
	if rest, ok := strings.CutPrefix(path, "data."); ok {
		if m, isMap := ev.Payload.(map[string]any); isMap {
			if _, hasData := m["data"]; !hasData {
				return lookupValue(ev.Payload, rest)
			}
		}
	}
	return nil, false
}

// Filled is the result of one fill.
type Filled struct {
	Values  map[string]any    `json:"values"`
	Sources map[string]string `json:"sources"`
	// Missing lists the required inputs that are empty, in input order.
	Missing []string `json:"missing"`
	// Errors maps an input name to its type error.
	Errors map[string]string `json:"errors,omitempty"`
}

// Text is the prompt text of each input. An empty input is "", so that its
// placeholder does not stay in the prompt.
func (f Filled) Text() map[string]string {
	out := make(map[string]string, len(f.Values))
	for k, v := range f.Values {
		out[k] = RenderValue(v)
	}
	return out
}

// FilledValue is the wire form of one filled input.
type FilledValue struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
}

// Wire is the wire form of the fill: {name: {value, source}}.
func (f Filled) Wire() map[string]FilledValue {
	out := make(map[string]FilledValue, len(f.Values))
	for k, v := range f.Values {
		out[k] = FilledValue{Value: v, Source: f.Sources[k]}
	}
	return out
}

// Declared is the prompt text of the declared inputs. An undeclared
// placeholder keeps the event var of the same name.
func (f Filled) Declared(list Inputs) map[string]string {
	text := f.Text()
	var out map[string]string
	for _, in := range list {
		if v, ok := text[in.Name]; ok {
			if out == nil {
				out = map[string]string{}
			}
			out[in.Name] = v
		}
	}
	return out
}

// Problem is the reason that a fill cannot start a run with no person: the
// type errors and the empty required inputs, by title. Empty means none.
func (f Filled) Problem(list Inputs) string {
	var parts []string
	for _, in := range list {
		if msg, ok := f.Errors[in.Name]; ok {
			parts = append(parts, msg)
		}
	}
	var empty []string
	for _, name := range f.Missing {
		if in, ok := list.Get(name); ok {
			if _, bad := f.Errors[name]; !bad {
				empty = append(empty, in.Title())
			}
		}
	}
	switch len(empty) {
	case 0:
	case 1:
		parts = append(parts, empty[0]+" is empty")
	default:
		parts = append(parts, strings.Join(empty, ", ")+" are empty")
	}
	return strings.Join(parts, "; ")
}

// Fill fills the inputs of the job for one trigger. The order is: a given
// value, the map entry of the trigger, the event field of the same name,
// the default. ev is nil for a run with no event. Each value goes through
// Coerce.
func (j *Job) Fill(tr *Trigger, ev *EventVars, given map[string]any) Filled {
	f := Filled{Values: map[string]any{}, Sources: map[string]string{}}
	for _, in := range j.InputList(tr != nil && tr.Family == "event") {
		v, src := j.pick(in, tr, ev, given)
		c, err := in.Coerce(v)
		if err != nil {
			if f.Errors == nil {
				f.Errors = map[string]string{}
			}
			f.Errors[in.Name] = err.Error()
			c = nil
		}
		if c == nil {
			src = SourceEmpty
			if in.Required {
				f.Missing = append(f.Missing, in.Name)
			}
		}
		f.Values[in.Name], f.Sources[in.Name] = c, src
	}
	return f
}

// InputList is the declared inputs in file order, then each {{placeholder}}
// of the prompt that the block does not declare, as a string input with
// Declared false. Event vars of a routed job are not inputs.
func (j *Job) InputList(routed bool) Inputs {
	out := slices.Clone(j.Inputs)
	if out == nil {
		out = Inputs{}
	}
	for _, k := range j.undeclaredPlaceholders(routed) {
		out = append(out, Input{Name: k, Type: InputString})
	}
	return out
}

// TriggerAt returns the event trigger of an index.
func (j *Job) TriggerAt(family string, index int) (Trigger, bool) {
	for _, t := range j.Triggers {
		if t.Index == index && (t.Family == family || (family == "schedule" && (t.Family == "cron" || t.Family == "every"))) {
			return t, true
		}
	}
	return Trigger{}, false
}

// pick finds the raw value of one input and its source.
func (j *Job) pick(in Input, tr *Trigger, ev *EventVars, given map[string]any) (any, string) {
	if v, ok := given[in.Name]; ok && !blank(v) {
		return v, SourceGiven
	}
	if tr != nil {
		if mv, ok := tr.Inputs.Get(in.Name); ok {
			if !mv.IsTemplate() {
				return mv.Value, SourceLiteral
			}
			if ev != nil {
				if s := ev.Render(mv.Value); strings.TrimSpace(s) != "" {
					return s, SourceMap
				}
			}
		}
	}
	if ev != nil {
		if v, ok := ev.field("data." + in.Name); ok && !blank(v) {
			return v, SourceName
		}
	}
	if in.Default != nil {
		return in.Default, SourceDefault
	}
	return nil, SourceEmpty
}

// undeclaredPlaceholders lists the prompt placeholders that are not
// declared inputs and not vars that a route fills (event is true when a
// route names the job).
func (j *Job) undeclaredPlaceholders(event bool) []string {
	var out []string
	for _, k := range DiscoverInputKeys(j.Prompt) {
		if _, ok := j.Inputs.Get(k); ok {
			continue
		}
		if event && (singleEventVars[k] || batchVars[k]) {
			continue
		}
		out = append(out, k)
	}
	return out
}

// RenderValue is the prompt text of a value. A list joins with ", ". An
// object renders as JSON.
func RenderValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, RenderValue(e))
		}
		return strings.Join(parts, ", ")
	case []string:
		return strings.Join(x, ", ")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// TemplateNames lists the {{ }} names of a template in order.
func TemplateNames(tmpl string) []string {
	var out []string
	for _, m := range templateRe.FindAllStringSubmatch(tmpl, -1) {
		out = append(out, m[1])
	}
	return out
}

// parseInput reads one entry. A bad key or value becomes a fault; the entry
// keeps the parts that parse.
func parseInput(name string, n *yaml.Node) Input {
	in := Input{Name: name, Type: InputString, Declared: true}
	path := "inputs." + name
	if !inputNameRe.MatchString(name) {
		in.fault(path, CodeInputInvalid, fmt.Sprintf("input %q: use letters, digits, and _ only", name))
	}
	if n.Kind != yaml.MappingNode {
		in.fault(path, CodeInputInvalid, fmt.Sprintf("input %q: want a map of keys such as type and label", name))
		return in
	}
	var def *yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]
		if key == "default" {
			def = val
			continue
		}
		known, err := in.setKey(key, val)
		switch {
		case !known:
			in.fault(path+"."+key, CodeInputInvalid, fmt.Sprintf("input %q: unknown key %q (known: %s)", name, key, strings.Join(inputKeys, ", ")))
		case err != nil:
			in.fault(path+"."+key, CodeInputInvalid, fmt.Sprintf("input %q: %s has a bad value", name, key))
		}
	}
	in.checkShape(path)
	if def != nil {
		var raw any
		if err := def.Decode(&raw); err == nil {
			if v, err := in.Coerce(raw); err != nil {
				in.fault(path+".default", CodeInputInvalid, fmt.Sprintf("input %q: default: %v", name, err))
			} else {
				in.Default = v
			}
		}
	}
	return in
}

func decodeFloat(n *yaml.Node) (*float64, error) {
	var f float64
	if err := n.Decode(&f); err != nil {
		return nil, err
	}
	return &f, nil
}

func toNumber(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case json.Number:
		return x.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(x), 64)
	}
	return 0, fmt.Errorf("not a number: %T", v)
}

func toBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "on", "1":
			return true, nil
		case "false", "no", "off", "0":
			return false, nil
		}
	}
	return false, fmt.Errorf("not a boolean: %v", v)
}

func lookupValue(v any, path string) (any, bool) {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// templateFault reports a {{ that does not close or a name that is not valid.
func templateFault(tmpl string) string {
	rest := templateRe.ReplaceAllString(tmpl, "")
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		return "a {{ }} part does not parse; use {{name}} or {{data.field}}"
	}
	return ""
}

func blank(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	}
	return false
}
