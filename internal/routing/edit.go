package routing

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"gopkg.in/yaml.v3"
)

// Patch is a change of a routes file.
type Patch struct {
	Mode         *string
	Notes        *string
	Unsure       *string
	NoFit        *string
	SureAt       *float64
	Routes       []map[string]any // merge by "id"; no "id" adds a route at the end; a nil value removes a key
	RemoveRoutes []string         // route ids
	Rules        *[]Rule          // replaces the whole list; order is meaning
}

// ValidationError is a patch that leaves the file with a fault.
type ValidationError struct{ Problems []Problem }

func (e *ValidationError) Error() string {
	if len(e.Problems) == 0 {
		return "the routes file is not valid"
	}
	return e.Problems[0].Path + ": " + e.Problems[0].Message
}

// AssignIDs writes id: as the first key of each route that has none. Every
// other byte of the file stays.
func AssignIDs(data []byte) (out []byte, changed bool, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("routes file: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return data, false, nil
	}
	_, routes := mapGet(doc.Content[0], "routes")
	if routes == nil || routes.Kind != yaml.SequenceNode {
		return data, false, nil
	}
	lineStart := []int{0}
	for i, b := range data {
		if b == '\n' {
			lineStart = append(lineStart, i+1)
		}
	}
	eol := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		eol = "\r\n"
	}
	var ins []idInsert
	for _, item := range routes.Content {
		if at, text, ok := idInsertion(item, data, lineStart, eol); ok {
			ins = append(ins, idInsert{at, text})
		}
	}
	if len(ins) == 0 {
		return data, false, nil
	}
	out = slices.Clone(data)
	for i := len(ins) - 1; i >= 0; i-- {
		out = append(out[:ins[i].at], append([]byte(ins[i].text), out[ins[i].at:]...)...)
	}
	return out, true, nil
}

// Edit applies a patch to the bytes of a file with an edit of the YAML tree,
// so the comments and the key order stay. It returns a *ValidationError when
// the result has a fault.
func Edit(data []byte, p Patch) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, invalid("", "the file is not valid YAML: "+err.Error())
	}
	if len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, invalid("", "the file must be a map with the keys mode, routes, and rules")
	}
	for key, v := range map[string]*string{"mode": p.Mode, "notes": p.Notes, "unsure": p.Unsure, "no_fit": p.NoFit} {
		if v != nil {
			setTop(root, key, strNode(*v))
		}
	}
	if p.SureAt != nil {
		setTop(root, "sure_at", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: fmt.Sprint(*p.SureAt)})
	}
	if err := editRoutes(root, p); err != nil {
		return nil, err
	}
	if p.Rules != nil {
		setRules(root, *p.Rules)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode routes file: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode routes file: %w", err)
	}
	if f := Parse("", "", buf.Bytes()); len(f.Problems) > 0 {
		return nil, &ValidationError{Problems: f.Problems}
	}
	return buf.Bytes(), nil
}

// Marshal writes a new file in the canonical key order.
func Marshal(f File) ([]byte, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mode := f.Mode
	if mode == "" {
		mode = ModeOff
	}
	mapSet(root, "mode", strNode(mode))
	if f.Notes != "" {
		mapSet(root, "notes", strNode(f.Notes))
	}
	if len(f.Routes) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, r := range f.Routes {
			var n yaml.Node
			if err := n.Encode(r.asYAML()); err != nil {
				return nil, fmt.Errorf("encode route: %w", err)
			}
			seq.Content = append(seq.Content, &n)
		}
		mapSet(root, "routes", seq)
	}
	setRules(root, f.Rules)
	if f.Unsure != "" && f.Unsure != UnsureAsk {
		mapSet(root, "unsure", strNode(f.Unsure))
	}
	if f.NoFit != "" && f.NoFit != NoFitDrop {
		mapSet(root, "no_fit", strNode(f.NoFit))
	}
	if f.SureAt != 0 && f.SureAt != DefaultSureAt {
		mapSet(root, "sure_at", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: fmt.Sprint(f.SureAt)})
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("encode routes file: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode routes file: %w", err)
	}
	return buf.Bytes(), nil
}

// asYAML is the file form of a route. Keys of a schedule stay out.
func (r Route) asYAML() routeYAML {
	f := r.Filter
	f.Cron, f.Every, f.Event, f.When, f.Timezone, f.Disabled, f.Manual = "", "", "", nil, "", false, false
	f.Inputs = r.Inputs
	gets := r.Gets
	if gets == "" {
		gets = GetsEveryMessage
	}
	return routeYAML{ID: r.ID, Agent: r.Agent, Job: r.Job, Assistant: r.Assistant, Gets: gets, UseFor: r.UseFor, TriggerYAML: f}
}

func invalid(path, msg string) *ValidationError {
	return &ValidationError{Problems: []Problem{{Code: CodeInvalidRoute, Severity: job.SeverityError, Path: path, Message: msg}}}
}

// byteOffset is the byte position of the n-th character of line.
func byteOffset(line []byte, n int) int {
	i := 0
	for ; n > 0 && i < len(line); n-- {
		i++
		for i < len(line) && line[i]&0xC0 == 0x80 {
			i++
		}
	}
	return i
}

func editRoutes(root *yaml.Node, p Patch) error {
	if len(p.Routes) == 0 && len(p.RemoveRoutes) == 0 {
		return nil
	}
	_, seq := mapGet(root, "routes")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setTop(root, "routes", seq)
	}
	for _, id := range p.RemoveRoutes {
		seq.Content = slices.DeleteFunc(seq.Content, func(n *yaml.Node) bool { return keyValue(n, "id") == id })
	}
	mode := ModeOff
	if _, m := mapGet(root, "mode"); m != nil && m.Value == ModeOn {
		mode = ModeOn
	}
	for _, patch := range p.Routes {
		id, _ := patch["id"].(string)
		if id == "" {
			n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			patch = mergeNew(patch, mode)
			if err := mergeRoute(n, patch); err != nil {
				return err
			}
			seq.Content = append(seq.Content, n)
			continue
		}
		i := slices.IndexFunc(seq.Content, func(n *yaml.Node) bool { return keyValue(n, "id") == id })
		if i < 0 {
			return invalid("routes["+id+"]", "no route has this id")
		}
		if err := mergeRoute(seq.Content[i], patch); err != nil {
			return err
		}
	}
	if len(seq.Content) == 0 {
		mapDel(root, "routes")
	}
	return nil
}

// mergeNew gives a new route its id and its default gets.
func mergeNew(patch map[string]any, mode string) map[string]any {
	out := map[string]any{"id": NewID()}
	for k, v := range patch {
		out[k] = v
	}
	if _, ok := out["gets"]; !ok {
		out["gets"] = GetsOff
		if mode == ModeOn {
			out["gets"] = GetsChosen
		}
	}
	return out
}

// mergeRoute sets the keys of patch on a route mapping. A nil value removes
// the key. A new target key replaces the old one.
func mergeRoute(n *yaml.Node, patch map[string]any) error {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return keyRank(routeKeys, a) - keyRank(routeKeys, b) })
	for _, k := range keys {
		v := patch[k]
		if v == nil {
			mapDel(n, k)
			continue
		}
		if k == "agent" || k == "job" || k == "assistant" {
			for _, other := range []string{"agent", "job", "assistant"} {
				if other != k {
					mapDel(n, other)
				}
			}
		}
		var val yaml.Node
		if s, ok := v.(string); ok {
			val = *strNode(s)
		} else if err := val.Encode(v); err != nil {
			return invalid("", fmt.Sprintf("key %s: %v", k, err))
		}
		mapSet(n, k, &val)
	}
	return nil
}

func keyRank(order []string, k string) int {
	if i := slices.Index(order, k); i >= 0 {
		return i
	}
	return len(order)
}

func setRules(root *yaml.Node, rules []Rule) {
	if len(rules) == 0 {
		mapDel(root, "rules")
		return
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, r := range rules {
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapSet(m, r.Field, strNode(r.Value))
		mapSet(m, "to", strNode(r.To))
		seq.Content = append(seq.Content, m)
	}
	setTop(root, "rules", seq)
}

// setTop sets a top-level key. A new key goes before the first key that
// follows it in the canonical order.
func setTop(root *yaml.Node, key string, val *yaml.Node) {
	if k, _ := mapGet(root, key); k != nil {
		mapSet(root, key, val)
		return
	}
	at := len(root.Content)
	for i := 0; i+1 < len(root.Content); i += 2 {
		if keyRank(topKeys, root.Content[i].Value) > keyRank(topKeys, key) {
			at = i
			break
		}
	}
	kn := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	root.Content = slices.Insert(root.Content, at, kn, val)
}

func strNode(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

func mapGet(m *yaml.Node, key string) (k, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// mapSet replaces the value of a key and keeps its comments; a new key goes last.
func mapSet(m *yaml.Node, key string, val *yaml.Node) {
	if k, old := mapGet(m, key); k != nil {
		val.HeadComment, val.LineComment, val.FootComment = old.HeadComment, old.LineComment, old.FootComment
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i] == k {
				m.Content[i+1] = val
			}
		}
		return
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

func mapDel(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = slices.Delete(m.Content, i, i+2)
			return
		}
	}
}

// idInsert is the text that AssignIDs puts at a byte offset.
type idInsert struct {
	at   int
	text string
}

// idInsertion gives the offset and the text of the id: key of a route item
// that has none.
func idInsertion(item *yaml.Node, data []byte, lineStart []int, eol string) (at int, text string, ok bool) {
	if item.Kind != yaml.MappingNode || len(item.Content) == 0 {
		return 0, "", false
	}
	if k, _ := mapGet(item, "id"); k != nil {
		return 0, "", false
	}
	first := item.Content[0]
	if first.Line < 1 || first.Line > len(lineStart) {
		return 0, "", false
	}
	at = lineStart[first.Line-1] + byteOffset(data[lineStart[first.Line-1]:], first.Column-1)
	if item.Style&yaml.FlowStyle != 0 {
		return at, "id: " + NewID() + ", ", true
	}
	return at, "id: " + NewID() + eol + strings.Repeat(" ", first.Column-1), true
}
