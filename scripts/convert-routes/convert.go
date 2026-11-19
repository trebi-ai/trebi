package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"gopkg.in/yaml.v3"
)

// workspaceRow is one workspace of trebi.db.
type workspaceRow struct {
	ID, Name, Path string
}

// target is the owner of an event trigger.
type target struct {
	kind string // agent | job | assistant
	name string // short name: "sling-cs", "sling-cs/email-support"
}

// source is one file that can hold event triggers.
type source struct {
	path        string
	rel         string
	frontmatter bool   // JOB.md or AGENT.md; false is trebi.yaml
	owner       target // valid when bad is empty
	bad         string // why no route can name the owner
}

// eventEntry is one event entry of a triggers list.
type eventEntry struct {
	node       *yaml.Node
	connection string
	line       int // 1-based line in the file
}

// plannedRoute is one converted trigger, for the order and the report.
type plannedRoute struct {
	connection string
	index      string
	route      routing.Route
	file       string
	line       int
}

// unroutable is an event trigger that no route can hold.
type unroutable struct {
	file       string
	line       int
	connection string
	reason     string
}

// fileOp is one file that the plan writes.
type fileOp struct {
	path    string
	rel     string
	before  []byte
	existed bool
	after   []byte
	mode    fs.FileMode
	source  bool // a JOB.md or AGENT.md that the program changes
}

// plan is the full conversion of one workspace. Building it writes nothing.
type plan struct {
	ws         workspaceRow
	files      []fileOp
	routes     []plannedRoute
	unroutable []unroutable
	problems   []string
	removed    map[string]int // source file to the count of removed entries
}

const (
	kindAgent     = "agent"
	kindJob       = "job"
	kindAssistant = "assistant"
)

// routeKeys are the keys of an event trigger that a route keeps.
var routeKeys = []string{"match", "extract", "limit", "allowlist", "rooms", "contains", "from", "replies", "chat", "batch", "types", "inputs"} //nolint:gochecknoglobals // static list

func (t target) index() string {
	if t.kind == kindAssistant {
		return routing.Assistant
	}
	return t.name
}

func (p plan) blocked(drop bool) bool {
	return len(p.problems) > 0 || (len(p.unroutable) > 0 && !drop)
}

func (p plan) empty() bool { return len(p.files) == 0 }

// convertWorkspace builds the plan of one workspace. It writes nothing.
func convertWorkspace(ws workspaceRow, skipRoots map[string]bool) (plan, error) {
	p := plan{ws: ws, removed: map[string]int{}}
	sources, err := findSources(ws, skipRoots)
	if err != nil {
		return p, err
	}
	var sourceOps []fileOp
	for _, s := range sources {
		data, err := os.ReadFile(s.path)
		if err != nil {
			return p, fmt.Errorf("read %s: %w", s.path, err)
		}
		entries, err := eventEntries(data, s.frontmatter)
		if err != nil {
			p.problems = append(p.problems, fmt.Sprintf("%s: %v", s.path, err))
			continue
		}
		if len(entries) == 0 {
			continue
		}
		p.addEntries(s, entries)
		out, n, err := stripEventTriggers(data, s.frontmatter)
		if err == nil {
			_, err = eventEntries(out, s.frontmatter)
		}
		if err != nil {
			p.problems = append(p.problems, fmt.Sprintf("%s: %v", s.path, err))
			continue
		}
		p.removed[s.path] = n
		sourceOps = append(sourceOps, fileOp{path: s.path, rel: s.rel, before: data, existed: true, after: out, mode: fileMode(s.path), source: true})
	}
	if err := p.addRoutesFiles(); err != nil {
		return p, err
	}
	p.files = append(p.files, sourceOps...)
	return p, nil
}

// addEntries turns the event entries of one source into routes.
func (p *plan) addEntries(s source, entries []eventEntry) {
	for _, e := range entries {
		var reason string
		switch {
		case s.bad != "":
			reason = s.bad
		case !validConnection(e.connection):
			reason = fmt.Sprintf("event %q is not a connection name", e.connection)
		default:
			r, err := routeFromTrigger(s.owner, e.node)
			if err == nil {
				p.routes = append(p.routes, plannedRoute{connection: e.connection, index: s.owner.index(), route: r, file: s.path, line: e.line})
				continue
			}
			reason = err.Error()
		}
		p.unroutable = append(p.unroutable, unroutable{file: s.path, line: e.line, connection: e.connection, reason: reason})
	}
}

// addRoutesFiles orders the routes of each connection and builds each file.
func (p *plan) addRoutesFiles() error {
	byConn := map[string][]plannedRoute{}
	for _, r := range p.routes {
		byConn[r.connection] = append(byConn[r.connection], r)
	}
	conns := make([]string, 0, len(byConn))
	for c := range byConn {
		conns = append(conns, c)
	}
	sort.Strings(conns)
	var ops []fileOp
	for _, c := range conns {
		list := byConn[c]
		sort.SliceStable(list, func(i, j int) bool { return list[i].index < list[j].index })
		routes := make([]routing.Route, len(list))
		for i, r := range list {
			routes[i] = r.route
		}
		op, err := p.routesOp(c, routes)
		if err != nil {
			return err
		}
		if op != nil {
			ops = append(ops, *op)
		}
	}
	p.files = append(p.files, ops...)
	return nil
}

// routesOp builds the routes file of one connection. It appends to a file
// that exists. A nil op with no error means the plan has a problem.
func (p *plan) routesOp(connection string, routes []routing.Route) (*fileOp, error) {
	path := filepath.Join(p.ws.Path, routing.Dir, connection+".yaml")
	op := fileOp{path: path, rel: filepath.Join(routing.Dir, connection+".yaml"), mode: 0o644}
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		op.before, op.existed, op.mode = old, true, fileMode(path)
		if probs := routing.Parse(connection, path, old).Problems; len(probs) > 0 {
			p.problems = append(p.problems, fmt.Sprintf("%s: the existing file has a fault: %s", path, probs[0].Message))
			return nil, nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	fresh, err := routing.Marshal(routing.File{Mode: routing.ModeOff, Routes: routes})
	if err != nil {
		return nil, err
	}
	op.after = fresh
	if op.existed && len(bytes.TrimSpace(old)) > 0 {
		if op.after, err = appendRoutes(old, fresh); err != nil {
			p.problems = append(p.problems, fmt.Sprintf("%s: %v", path, err))
			return nil, nil
		}
	}
	if probs := routing.Parse(connection, path, op.after).Problems; len(probs) > 0 {
		for _, pr := range probs {
			p.problems = append(p.problems, fmt.Sprintf("%s: %s: %s", path, pr.Code, pr.Message))
		}
		return nil, nil
	}
	return &op, nil
}

func fileMode(path string) fs.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o644
}

// appendRoutes adds the routes of fresh after the routes of old. Comments and
// other keys of old stay.
func appendRoutes(old, fresh []byte) ([]byte, error) {
	var oldDoc, freshDoc yaml.Node
	if err := yaml.Unmarshal(old, &oldDoc); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(fresh, &freshDoc); err != nil {
		return nil, err
	}
	root := oldDoc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("the file must be a map")
	}
	add := mapValue(freshDoc.Content[0], "routes")
	if seq := mapValue(root, "routes"); seq != nil && seq.Kind == yaml.SequenceNode {
		seq.Content = append(seq.Content, add.Content...)
	} else {
		at := len(root.Content)
		for i := 0; i+1 < len(root.Content); i += 2 {
			if slices.Contains([]string{"rules", "unsure", "no_fit", "sure_at"}, root.Content[i].Value) {
				at = i
				break
			}
		}
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "routes"}
		root.Content = slices.Insert(root.Content, at, key, add)
	}
	return encodeYAML(&oldDoc)
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func encodeYAML(n *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func validConnection(c string) bool {
	return c != "" && !strings.HasPrefix(c, ".") && !strings.ContainsAny(c, `/\ `)
}

// routeFromTrigger maps one event trigger node to a route of the owner.
func routeFromTrigger(owner target, raw *yaml.Node) (routing.Route, error) {
	if raw.Kind != yaml.MappingNode {
		return routing.Route{}, errors.New("the trigger is not a map")
	}
	var manual, disabled bool
	for i := 0; i+1 < len(raw.Content); i += 2 {
		k, v := raw.Content[i].Value, raw.Content[i+1]
		switch k {
		case "event":
		case "manual":
			if err := v.Decode(&manual); err != nil {
				return routing.Route{}, fmt.Errorf("manual: %w", err)
			}
		case "disabled":
			if err := v.Decode(&disabled); err != nil {
				return routing.Route{}, fmt.Errorf("disabled: %w", err)
			}
		default:
			if !slices.Contains(routeKeys, k) {
				return routing.Route{}, fmt.Errorf("the key %q has no place in a route", k)
			}
		}
	}
	var y job.TriggerYAML
	if err := raw.Decode(&y); err != nil {
		return routing.Route{}, err
	}
	r := routing.Route{Gets: routing.GetsEveryMessage, Inputs: y.Inputs}
	y.Event, y.Manual, y.Disabled, y.Inputs = "", false, false, nil
	r.Filter = y
	switch owner.kind {
	case kindAgent:
		r.Agent = owner.name
	case kindJob:
		r.Job = owner.name
	default:
		r.Assistant = true
	}
	switch {
	case disabled:
		r.Gets = routing.GetsOff
	case manual:
		r.Gets = routing.GetsByHand
	}
	return r, nil
}

// findSources lists the files of a workspace that can hold event triggers.
func findSources(ws workspaceRow, skipRoots map[string]bool) ([]source, error) {
	var out []source
	if _, err := os.Stat(filepath.Join(ws.Path, "trebi.yaml")); err == nil {
		out = append(out, source{path: filepath.Join(ws.Path, "trebi.yaml"), rel: "trebi.yaml", owner: target{kind: kindAssistant, name: routing.Assistant}})
	}
	err := filepath.WalkDir(ws.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if skipDir(ws.Path, path, d.Name(), skipRoots) {
				return filepath.SkipDir
			}
			return nil
		}
		isAgent := strings.EqualFold(d.Name(), "AGENT.md")
		if !isAgent && !strings.EqualFold(d.Name(), "JOB.md") {
			return nil
		}
		dir := filepath.Dir(path)
		if !isAgent && fileExists(filepath.Join(dir, "AGENT.md")) {
			return nil // the daemon does not index this JOB.md
		}
		rel, _ := filepath.Rel(ws.Path, path)
		s := source{path: path, rel: rel, frontmatter: true}
		s.owner, s.bad = ownerOf(filepath.ToSlash(filepath.Dir(rel)), isAgent)
		out = append(out, s)
		return nil
	})
	return out, err
}

// ownerOf names the target of a file by the folder of the file.
func ownerOf(dir string, isAgent bool) (target, string) {
	parts := strings.Split(dir, "/")
	inAgents := len(parts) >= 2 && parts[0] == "agents" && parts[1] != ""
	switch {
	case isAgent && inAgents && len(parts) == 2:
		return target{kind: kindAgent, name: parts[1]}, ""
	case isAgent:
		return target{}, "the AGENT.md is not at agents/<name>/AGENT.md"
	case inAgents && len(parts) == 3:
		return target{kind: kindJob, name: parts[1] + "/" + parts[2]}, ""
	case inAgents:
		return target{}, "the job folder is nested; a route names <agent>/<job>"
	}
	return target{}, "the job is outside an agent folder"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// skipDir mirrors the walk of the daemon: it skips nested workspaces and the
// folders that hold no job files.
func skipDir(root, path, name string, skipRoots map[string]bool) bool {
	if name == ".git" || name == "node_modules" || name == ".trebi" {
		return true
	}
	if path == root {
		return false
	}
	if skipRoots[filepath.Clean(path)] || fileExists(filepath.Join(path, "trebi.yaml")) {
		return true
	}
	if name == job.SkillsDir {
		parent := filepath.Dir(path)
		if parent == root || fileExists(filepath.Join(parent, "JOB.md")) || fileExists(filepath.Join(parent, "AGENT.md")) {
			return !fileExists(filepath.Join(path, "JOB.md")) && !fileExists(filepath.Join(path, "AGENT.md"))
		}
	}
	return false
}

// triggerPath is the key path of the triggers list. A JOB.md or AGENT.md
// holds it in the frontmatter; trebi.yaml holds it in the assistant block.
func triggerPath(frontmatter bool) []string {
	if frontmatter {
		return []string{"triggers"}
	}
	return []string{"assistant", "triggers"}
}

// keyVal is one key and its value node.
type keyVal struct{ key, val *yaml.Node }

// locate follows a key path from the root map. It returns nil when a key is
// missing or a value has the wrong kind.
func locate(doc *yaml.Node, path []string) []keyVal {
	if len(doc.Content) == 0 {
		return nil
	}
	cur := doc.Content[0]
	var chain []keyVal
	for i, k := range path {
		if cur.Kind != yaml.MappingNode {
			return nil
		}
		var found bool
		for j := 0; j+1 < len(cur.Content); j += 2 {
			if cur.Content[j].Value == k {
				chain = append(chain, keyVal{cur.Content[j], cur.Content[j+1]})
				cur, found = cur.Content[j+1], true
				break
			}
		}
		if !found || (i == len(path)-1 && cur.Kind != yaml.SequenceNode) {
			return nil
		}
	}
	return chain
}

// splitFrontmatter cuts a file into the opening delimiter line, the YAML
// block, and the rest from the closing delimiter.
func splitFrontmatter(data []byte) (head, block, tail []byte, ok bool) {
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) == 0 || !bytes.HasPrefix(lines[0], []byte("---")) || len(bytes.TrimSpace(lines[0])) != 3 {
		return nil, nil, nil, false
	}
	for i := 1; i < len(lines); i++ {
		if bytes.HasPrefix(lines[i], []byte("---")) {
			return lines[0], bytes.Join(lines[1:i], nil), bytes.Join(lines[i:], nil), true
		}
	}
	return nil, nil, nil, false
}

// eventEntries lists the event entries of the triggers list of a file.
func eventEntries(data []byte, frontmatter bool) ([]eventEntry, error) {
	base := 0
	if frontmatter {
		head, block, _, ok := splitFrontmatter(data)
		if !ok {
			return nil, nil
		}
		data, base = block, bytes.Count(head, []byte("\n"))
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("the YAML does not parse: %w", err)
	}
	chain := locate(&doc, triggerPath(frontmatter))
	if chain == nil {
		return nil, nil
	}
	var out []eventEntry
	for _, item := range chain[len(chain)-1].val.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if v := mapValue(item, "event"); v != nil {
			out = append(out, eventEntry{node: item, connection: v.Value, line: base + item.Line})
		}
	}
	return out, nil
}

// stripEventTriggers removes the event entries of the triggers list and
// returns the count. It edits the lines of the file, so the comments, the
// other keys, and the schedules stay byte for byte. For frontmatter true the
// list is the triggers key of a JOB.md or AGENT.md; for false it is
// assistant.triggers of a trebi.yaml.
func stripEventTriggers(data []byte, frontmatter bool) ([]byte, int, error) {
	if !frontmatter {
		return stripBlock(data, triggerPath(false))
	}
	head, block, tail, ok := splitFrontmatter(data)
	if !ok {
		return data, 0, nil
	}
	out, n, err := stripBlock(block, triggerPath(true))
	if err != nil || n == 0 {
		return data, 0, err
	}
	return bytes.Join([][]byte{head, out, tail}, nil), n, nil
}

// lineSpan is a range of lines [from, to) of a YAML block.
type lineSpan struct {
	from, to int
	replace  []byte // text that takes the place of the lines
}

func stripBlock(block []byte, path []string) ([]byte, int, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(block, &doc); err != nil {
		return nil, 0, fmt.Errorf("the YAML does not parse: %w", err)
	}
	chain := locate(&doc, path)
	if chain == nil {
		return block, 0, nil
	}
	last := chain[len(chain)-1]
	var drop, keep []*yaml.Node
	for _, item := range last.val.Content {
		if item.Kind == yaml.MappingNode && mapValue(item, "event") != nil {
			drop = append(drop, item)
		} else {
			keep = append(keep, item)
		}
	}
	if len(drop) == 0 {
		return block, 0, nil
	}
	lines := bytes.SplitAfter(block, []byte("\n"))
	spans, err := triggerSpans(lines, chain, drop, keep)
	if err != nil {
		return nil, 0, err
	}
	var out []byte
	at := 0
	for _, s := range spans {
		out = append(out, bytes.Join(lines[at:s.from], nil)...)
		out = append(out, s.replace...)
		at = s.to
	}
	out = append(out, bytes.Join(lines[at:], nil)...)
	return out, len(drop), nil
}

// triggerSpans finds the lines to cut. When the list ends up empty, the cut
// takes the key too, and the assistant key when it holds nothing else.
func triggerSpans(lines [][]byte, chain []keyVal, drop, keep []*yaml.Node) ([]lineSpan, error) {
	last := chain[len(chain)-1]
	keyLine := last.key.Line - 1
	if last.val.Style&yaml.FlowStyle != 0 {
		end := continuation(lines, keyLine, last.key.Column-1)
		if len(keep) > 0 {
			text, err := flowText(last, keep)
			return []lineSpan{{from: keyLine, to: end, replace: text}}, err
		}
		return []lineSpan{wholeSpan(lines, chain, keyLine, end)}, nil
	}
	var spans []lineSpan
	var end int
	for _, item := range drop {
		from := item.Line - 1
		if !strings.HasPrefix(strings.TrimLeft(string(lines[from]), " \t"), "-") {
			return nil, fmt.Errorf("line %d: a trigger entry must start with a dash", item.Line)
		}
		end = continuation(lines, from, indentOf(lines[from]))
		spans = append(spans, lineSpan{from: from, to: end})
	}
	if len(keep) == 0 {
		return []lineSpan{wholeSpan(lines, chain, keyLine, end)}, nil
	}
	return spans, nil
}

// wholeSpan cuts from the key of the list to end. A parent block that holds
// only this key goes too.
func wholeSpan(_ [][]byte, chain []keyVal, keyLine, end int) lineSpan {
	if len(chain) > 1 && len(chain[len(chain)-2].val.Content) == 2 {
		keyLine = chain[len(chain)-2].key.Line - 1
	}
	return lineSpan{from: keyLine, to: end}
}

// continuation returns the line after the last line that belongs to the node
// that starts at line from with the given indent.
func continuation(lines [][]byte, from, indent int) int {
	end := from + 1
	for i := from + 1; i < len(lines); i++ {
		switch {
		case len(bytes.TrimSpace(lines[i])) == 0:
		case indentOf(lines[i]) > indent:
			end = i + 1
		default:
			return end
		}
	}
	return end
}

func indentOf(line []byte) int {
	return len(line) - len(bytes.TrimLeft(line, " "))
}

// flowText writes a flow list again with the entries that stay.
func flowText(kv keyVal, keep []*yaml.Node) ([]byte, error) {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: kv.val.Style, Content: keep}
	key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kv.key.Value}
	enc, err := encodeYAML(&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{key, seq}})
	if err != nil {
		return nil, err
	}
	pad := strings.Repeat(" ", kv.key.Column-1)
	var out bytes.Buffer
	for _, l := range bytes.SplitAfter(enc, []byte("\n")) {
		if len(l) > 0 {
			out.WriteString(pad)
			out.Write(l)
		}
	}
	return out.Bytes(), nil
}
