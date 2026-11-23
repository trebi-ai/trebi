package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/notify"
	"gopkg.in/yaml.v3"
)

// WriteAtomic writes data to path via temp+rename.
func WriteAtomic(path string, data []byte) error {
	dir := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir = path[:i]
	}
	tmp, err := os.CreateTemp(dir, ".trebi-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// SetDisabled toggles disabled: in frontmatter via line-level mutate (preserves comments).
func SetDisabled(path string, disabled bool) error {
	val := "false"
	if disabled {
		val = "true"
	}
	return setScalarField(path, "disabled", val)
}

// SetArchived writes archived_at, or removes the key when at is zero. An
// archive keeps the first time when the key is already there.
func SetArchived(path string, at time.Time) error {
	if at.IsZero() {
		return setTopLevelField(path, "archived_at", "")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if scanKey(data, "archived_at") != "" {
		return nil
	}
	return setTopLevelField(path, "archived_at", at.UTC().Format(time.RFC3339))
}

// KeepArchived writes prev back when the file at path lost its archived_at
// after a full-text write. An empty prev does nothing.
func KeepArchived(path, prev string) error {
	if prev == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if scanKey(data, "archived_at") != "" {
		return nil
	}
	return setTopLevelField(path, "archived_at", prev)
}

// SetName writes the optional identity `name:` field. Callers own the
// file/folder consistency check.
func SetName(path, name string) error {
	return setScalarField(path, "name", strconv.Quote(name))
}

// patchScalars are the top-level frontmatter fields a patch may set.
// harness, model, and effort are handled together by applyHarnessPatch because
// they share one YAML node that can be a scalar or a map.
var patchScalars = []string{"title", "connectors", "description", "working_dir", "followup_prompt"} //nolint:gochecknoglobals // static field list, never mutated

// patchHarnessFields are keys of the nested harness: node.
var patchHarnessFields = map[string]bool{"harness": true, "model": true, "effort": true} //nolint:gochecknoglobals // static key set, never mutated

// patchSessionFields map a dotted session key to its frontmatter field.
var patchSessionFields = map[string]string{ //nolint:gochecknoglobals // static field table, never mutated
	"session.max_duration": "max_duration",
	"session.idle_timeout": "idle_timeout",
	"session.keep_alive":   "keep_alive",
	"session.nudge":        "nudge",
	"session.resumable":    "resumable",
	"session.if_running":   "if_running",
	"session.continue":     "continue",
}

// MergePatchFrontmatter applies a JSON-merge-patch of known scalar frontmatter fields.
// Supports: disabled, harness, title, connectors, description,
// working_dir, env_file, keep_runs, followup_prompt, folder, and session.* scalars; connectors is
// always a list. A nested "session" map is flattened
// to session.idle_timeout etc. via line rewrite. An unknown key is an error:
// a silent success would tell the caller a change landed when it did not.
func MergePatchFrontmatter(path string, patch map[string]any) error {
	if len(patch) == 0 {
		return nil
	}
	flattenSessionPatch(patch)
	flattenTeamPatch(patch)
	if err := checkPatchKeys(patch); err != nil {
		return err
	}
	folder, folderSet, err := folderPatch(path, patch)
	if err != nil {
		return err
	}
	if err := applyDisabledPatch(path, patch); err != nil {
		return err
	}
	if err := applyTeamPatch(path, patch); err != nil {
		return err
	}
	if err := applyEnvFilePatch(path, patch); err != nil {
		return err
	}
	if err := applyKeepRunsPatch(path, patch); err != nil {
		return err
	}
	if err := applyScalarPatch(path, patch); err != nil {
		return err
	}
	if err := applyHarnessPatch(path, patch); err != nil {
		return err
	}
	if err := applyRoutePatch(path, patch); err != nil {
		return err
	}
	if err := applyEmailAcceptPatch(path, patch); err != nil {
		return err
	}
	if err := applyBlockPatch(path, patch); err != nil {
		return err
	}
	if folderSet {
		if err := setTopLevelField(path, "folder", folder); err != nil {
			return err
		}
	}
	return applySessionPatch(path, patch)
}

// checkPatchKeys rejects a key the writer does not honor.
func checkPatchKeys(patch map[string]any) error {
	known := map[string]bool{"disabled": true, "harness": true, "contact": true, "notify": true, "email_accept": true, "folder": true, "env_file": true, "keep_runs": true, "inputs": true, "triggers": true, "team.handoff": true}
	for k := range patchHarnessFields {
		known[k] = true
	}
	for _, k := range patchScalars {
		known[k] = true
	}
	for k := range patchSessionFields {
		known[k] = true
	}
	unknown := make([]string, 0, len(patch))
	for k := range patch {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	valid := append([]string{"disabled", "harness", "model", "effort"}, patchScalars...)
	valid = append(valid, "contact", "notify", "email_accept", "folder", "env_file", "inputs", "triggers", "session", "team.handoff")
	return fmt.Errorf("unknown patch field(s) %s; valid fields are %s",
		strings.Join(unknown, ", "), strings.Join(valid, ", "))
}

// folderPatch checks the folder value before any write and returns it as
// YAML text. An empty or null value removes the key.
func folderPatch(path string, patch map[string]any) (text string, set bool, err error) {
	v, ok := patch["folder"]
	if !ok {
		return "", false, nil
	}
	s, isStr := v.(string)
	if v != nil && !isStr {
		return "", false, errors.New("folder: must be a string")
	}
	folder, err := NormalizeFolder(s)
	if err != nil {
		return "", false, fmt.Errorf("folder: %w", err)
	}
	if folder == "" {
		return "", true, nil
	}
	if isAgentFile(path) {
		return "", false, errors.New("folder: applies only to a job")
	}
	out, err := yaml.Marshal(folder)
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(out)), true, nil
}

// flattenSessionPatch lifts a nested session map into dotted session.* keys.
func flattenSessionPatch(patch map[string]any) {
	if sess, ok := patch["session"].(map[string]any); ok {
		for k, v := range sess {
			patch["session."+k] = v
		}
		delete(patch, "session")
	}
}

// flattenTeamPatch lifts a nested team map into dotted team.* keys.
func flattenTeamPatch(patch map[string]any) {
	if team, ok := patch["team"].(map[string]any); ok {
		for k, v := range team {
			patch["team."+k] = v
		}
		delete(patch, "team")
	}
}

// applyTeamPatch writes team.handoff as true or false. A null value removes
// the key, so the trigger of the run decides. A job has no other team key.
func applyTeamPatch(path string, patch map[string]any) error {
	v, ok := patch["team.handoff"]
	if !ok {
		return nil
	}
	if isAgentFile(path) {
		return errors.New("team.handoff: applies only to a job")
	}
	text := ""
	switch b := v.(type) {
	case nil:
	case bool:
		text = "\n  handoff: " + strconv.FormatBool(b)
	default:
		return errors.New("team.handoff: must be true, false or null")
	}
	delete(patch, "team.handoff")
	return setTopLevelField(path, "team", text)
}

// applyEnvFilePatch writes env_file as true or false. A null value removes
// the key, so the launch takes the kit value or the default.
func applyEnvFilePatch(path string, patch map[string]any) error {
	v, ok := patch["env_file"]
	if !ok {
		return nil
	}
	text := ""
	switch b := v.(type) {
	case nil:
	case bool:
		text = strconv.FormatBool(b)
	case string:
		if b != "true" && b != "false" {
			return errors.New("env_file: must be true, false, or null")
		}
		text = b
	default:
		return errors.New("env_file: must be true, false, or null")
	}
	delete(patch, "env_file")
	return setTopLevelField(path, "env_file", text)
}

// applyKeepRunsPatch sets keep_runs (a whole number, 0 keeps all); null
// removes the key, so defaults.keep_runs applies.
func applyKeepRunsPatch(path string, patch map[string]any) error {
	v, ok := patch["keep_runs"]
	if !ok {
		return nil
	}
	text := ""
	switch n := v.(type) {
	case nil:
	case float64:
		if n < 0 || n != math.Trunc(n) {
			return errors.New("keep_runs: must be a whole number >= 0, or null")
		}
		text = strconv.FormatInt(int64(n), 10)
	case int:
		if n < 0 {
			return errors.New("keep_runs: must be a whole number >= 0, or null")
		}
		text = strconv.Itoa(n)
	default:
		return errors.New("keep_runs: must be a whole number >= 0, or null")
	}
	delete(patch, "keep_runs")
	return setTopLevelField(path, "keep_runs", text)
}

// applyDisabledPatch handles the bool-typed disabled key and removes it from patch.
func applyDisabledPatch(path string, patch map[string]any) error {
	v, ok := patch["disabled"]
	if !ok {
		return nil
	}
	switch b := v.(type) {
	case bool:
		if err := SetDisabled(path, b); err != nil {
			return err
		}
	case string:
		if err := SetDisabled(path, b == "true" || b == "1"); err != nil {
			return err
		}
	}
	delete(patch, "disabled")
	return nil
}

// applyScalarPatch writes the known top-level scalar fields present in patch.
func applyScalarPatch(path string, patch map[string]any) error {
	for _, k := range patchScalars {
		v, ok := patch[k]
		if !ok || v == nil {
			continue
		}
		set := setScalarField
		if patchListFields[k] {
			set = setTopLevelField
		}
		if err := set(path, k, patchFieldText(k, v)); err != nil {
			return err
		}
	}
	return nil
}

// patchListFields can hold a list. A patch replaces only the top-level key,
// with its block items, so kit.connectors stays.
var patchListFields = map[string]bool{"connectors": true} //nolint:gochecknoglobals // static key set, never mutated

// patchFieldText renders one patch value as frontmatter text. connectors is
// a flow sequence. Every other field is a plain scalar.
func patchFieldText(key string, v any) string {
	if key == "connectors" {
		return "[" + strings.Join(stringList(v), ", ") + "]"
	}
	return fmt.Sprint(v)
}

// applyBlockPatch replaces the inputs block and the triggers list. inputs is
// a list of input entries in order; triggers is the full list. A null value
// removes the key. A value that does not parse is an error before any write.
func applyBlockPatch(path string, patch map[string]any) error {
	blocks := map[string]string{}
	if v, ok := patch["inputs"]; ok {
		text, err := inputsPatchText(v)
		if err != nil {
			return fmt.Errorf("inputs: %w", err)
		}
		blocks["inputs"] = text
	}
	if v, ok := patch["triggers"]; ok {
		text, err := triggersPatchText(v)
		if err != nil {
			return fmt.Errorf("triggers: %w", err)
		}
		blocks["triggers"] = text
	}
	for _, k := range []string{"inputs", "triggers"} {
		text, ok := blocks[k]
		if !ok {
			continue
		}
		if err := setTopLevelField(path, k, text); err != nil {
			return err
		}
	}
	return nil
}

func inputsPatchText(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var list Inputs
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", errors.New("want a list of inputs with name, type, and label")
	}
	if len(list) == 0 {
		return "", nil
	}
	for _, in := range list {
		if in.Name == "" {
			return "", errors.New("each input needs a name")
		}
	}
	block, err := yaml.Marshal(list)
	if err != nil {
		return "", err
	}
	var check Inputs
	if err := yaml.Unmarshal(block, &check); err != nil {
		return "", err
	}
	for _, in := range check {
		if err := in.Validate(); err != nil {
			return "", err
		}
	}
	return blockText(block), nil
}

func triggersPatchText(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	// The YAML keys of a trigger are the patch keys, so a YAML round trip
	// decodes the JSON value.
	src, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	var list []TriggerYAML
	if err := yaml.Unmarshal(src, &list); err != nil {
		return "", errors.New("want a list of triggers")
	}
	if len(list) == 0 {
		return "", nil
	}
	if _, err := normalizeTriggers(list); err != nil {
		return "", err
	}
	block, err := yaml.Marshal(list)
	if err != nil {
		return "", err
	}
	return blockText(block), nil
}

// blockText indents a YAML block to sit under a top-level key.
func blockText(block []byte) string {
	lines := strings.Split(strings.TrimRight(string(block), "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return "\n" + strings.Join(lines, "\n")
}

// applyRoutePatch writes contact and notify as flow sequences. A null value
// removes the key, so the file inherits the route or the contact.
func applyRoutePatch(path string, patch map[string]any) error {
	for _, k := range []string{"contact", "notify"} {
		v, ok := patch[k]
		if !ok {
			continue
		}
		text := ""
		if v != nil {
			var err error
			if text, err = routeFieldText(k, v); err != nil {
				return err
			}
		}
		if err := setTopLevelField(path, k, text); err != nil {
			return err
		}
	}
	return nil
}

// applyEmailAcceptPatch replaces the accept list under the email: node of an
// operator document. Other email keys stay.
func applyEmailAcceptPatch(path string, patch map[string]any) error {
	v, ok := patch["email_accept"]
	if !ok {
		return nil
	}
	raw, _ := v.([]any)
	if s, isStrs := v.([]string); isStrs {
		raw = make([]any, len(s))
		for i, e := range s {
			raw[i] = e
		}
	}
	if v != nil && raw == nil {
		return fmt.Errorf("email_accept must be a list of strings")
	}
	items := make([]string, 0, len(raw))
	lines := make([]string, 0, len(raw))
	for _, e := range raw {
		str, isStr := e.(string)
		if !isStr {
			return fmt.Errorf("email_accept must be a list of strings")
		}
		lines = append(lines, str)
		items = append(items, strconv.Quote(str))
	}
	if _, err := ParseAccept(lines); err != nil {
		return fmt.Errorf("email_accept: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmStart, fmEnd, err := locateFrontmatter(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var fm []string
	if body := strings.TrimRight(string(data[fmStart:fmEnd]), "\n"); body != "" {
		fm = strings.Split(body, "\n")
	}
	fm, ok = setEmailAccept(fm, "  accept: ["+strings.Join(items, ", ")+"]")
	if !ok {
		return fmt.Errorf("%s: email is a one-line map; write it as a block to change accept", path)
	}
	out := make([]byte, 0, len(data)+64)
	out = append(out, data[:fmStart]...)
	out = append(out, strings.Join(fm, "\n")+"\n"...)
	out = append(out, data[fmEnd:]...)
	return WriteAtomic(path, out)
}

// setEmailAccept replaces the accept child of the email: block, with its
// list lines, or adds it. A one-line email: map is not changed.
func setEmailAccept(fm []string, entry string) ([]string, bool) {
	parent := slices.IndexFunc(fm, func(l string) bool { return l == "email:" || strings.HasPrefix(l, "email: ") })
	if parent < 0 {
		return append(fm, "email:", entry), true
	}
	if strings.TrimSpace(strings.TrimPrefix(fm[parent], "email:")) != "" {
		return fm, false
	}
	end := childEnd(fm, parent+1, 0)
	for i := parent + 1; i < end; i++ {
		trim := strings.TrimLeft(fm[i], " \t")
		if trim == "accept:" || strings.HasPrefix(trim, "accept: ") {
			return slices.Replace(fm, i, childEnd(fm[:end], i+1, len(fm[i])-len(trim)), entry), true
		}
	}
	return slices.Insert(fm, parent+1, entry), true
}

// childEnd returns the index after the lines from i on that are blank, list
// items, or indented deeper than indent.
func childEnd(fm []string, i, indent int) int {
	for ; i < len(fm); i++ {
		trim := strings.TrimLeft(fm[i], " \t")
		if trim != "" && len(fm[i])-len(trim) <= indent && !strings.HasPrefix(trim, "-") {
			break
		}
	}
	return i
}

// routeFieldText validates a contact or notify value and renders it.
func routeFieldText(key string, v any) (string, error) {
	var items []string
	if key == "contact" {
		contact, err := notify.ParseContact(v)
		if err != nil {
			return "", err
		}
		for _, c := range contact {
			items = append(items, strconv.Quote(c))
		}
		return "[" + strings.Join(items, ", ") + "]", nil
	}
	steps, err := notify.ParseSteps(v)
	if err != nil {
		return "", err
	}
	for _, st := range steps {
		item := "{via: " + strconv.Quote(st.Via)
		if st.To != "" {
			item += ", to: " + strconv.Quote(st.To)
		}
		items = append(items, item+"}")
	}
	return "[" + strings.Join(items, ", ") + "]", nil
}

// applySessionPatch writes session.* keys under the session: block.
func applySessionPatch(path string, patch map[string]any) error {
	for patchKey, field := range patchSessionFields {
		v, ok := patch[patchKey]
		if !ok || v == nil {
			continue
		}
		if err := setSessionField(path, field, fmt.Sprint(v)); err != nil {
			return err
		}
	}
	return nil
}

// applyHarnessPatch writes harness/model/effort into the one harness: node.
// The node stays a plain scalar while no override is set, so files that only
// name a harness keep their one-line form. A null or empty value clears the
// field; with no field left, the node goes and the owner agent value applies.
func applyHarnessPatch(path string, patch map[string]any) error {
	field := func(key string) *string {
		v, ok := patch[key]
		if !ok {
			return nil
		}
		s := ""
		if v != nil {
			s = fmt.Sprint(v)
		}
		return &s
	}
	name, model, effort := field("harness"), field("model"), field("effort")
	if name == nil && model == nil && effort == nil {
		return nil
	}
	return setHarnessSpec(path, name, model, effort)
}

// harnessSpecValue is the mutable scalar view of the harness node.
type harnessSpecValue struct {
	Name   string
	Model  string
	Effort string
}

func setHarnessSpec(path string, name, model, effort *string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmStart, fmEnd, err := locateFrontmatter(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	fmContent := string(data[fmStart:fmEnd])
	var fmLines []string
	if fmContent != "" {
		fmLines = strings.Split(strings.TrimRight(fmContent, "\n"), "\n")
	}

	idx, children := -1, 0
	var cur harnessSpecValue
	for i, line := range fmLines {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "harness:") {
			continue
		}
		idx = i
		raw := strings.TrimSpace(strings.TrimPrefix(trim, "harness:"))
		if raw != "" {
			cur = parseHarnessScalar(raw)
			break
		}
		// Block form: consume indented child lines.
		for j := i + 1; j < len(fmLines); j++ {
			child := fmLines[j]
			if child == "" {
				break
			}
			if child[0] != ' ' && child[0] != '\t' {
				break
			}
			children++
			k, v, ok := strings.Cut(strings.TrimSpace(child), ":")
			if !ok {
				continue
			}
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			switch strings.TrimSpace(k) {
			case "name":
				cur.Name = v
			case "model":
				cur.Model = v
			case "effort":
				cur.Effort = v
			}
		}
		break
	}

	apply := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	apply(&cur.Name, name)
	apply(&cur.Model, model)
	apply(&cur.Effort, effort)

	var block []string
	if cur != (harnessSpecValue{}) {
		block = renderHarnessSpec(cur)
	}
	if idx < 0 {
		fmLines = append(block, fmLines...)
	} else {
		tail := append([]string{}, fmLines[idx+1+children:]...)
		fmLines = append(fmLines[:idx], block...)
		fmLines = append(fmLines, tail...)
	}

	newFm := strings.Join(fmLines, "\n") + "\n"
	out := make([]byte, 0, len(data)+64)
	out = append(out, data[:fmStart]...)
	out = append(out, newFm...)
	out = append(out, data[fmEnd:]...)
	return WriteAtomic(path, out)
}

// parseHarnessScalar reads a scalar or flow-map harness value.
func parseHarnessScalar(raw string) harnessSpecValue {
	if strings.HasPrefix(raw, "{") {
		var spec HarnessSpec
		if err := yaml.Unmarshal([]byte(raw), &spec); err == nil {
			return harnessSpecValue{Name: spec.Name, Model: spec.Model, Effort: spec.Effort}
		}
	}
	return harnessSpecValue{Name: strings.Trim(raw, `"'`)}
}

// renderHarnessSpec returns the harness node lines: a scalar when only the
// name is set, otherwise a two-space-indented block.
func renderHarnessSpec(v harnessSpecValue) []string {
	if v.Model == "" && v.Effort == "" {
		return []string{"harness: " + v.Name}
	}
	out := []string{"harness:"}
	if v.Name != "" {
		out = append(out, "  name: "+v.Name)
	}
	if v.Model != "" {
		out = append(out, "  model: "+v.Model)
	}
	if v.Effort != "" {
		out = append(out, "  effort: "+v.Effort)
	}
	return out
}

// setScalarField writes one top-level scalar field.
func setScalarField(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmStart, fmEnd, err := locateFrontmatter(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	fmContent := string(data[fmStart:fmEnd])
	var fmLines []string
	if fmContent != "" {
		fmLines = strings.Split(strings.TrimRight(fmContent, "\n"), "\n")
	}
	newLines := setFieldInBlock(fmLines, key, value)
	var newFm string
	if len(newLines) > 0 {
		newFm = strings.Join(newLines, "\n") + "\n"
	}
	out := make([]byte, 0, len(data)+64)
	out = append(out, data[:fmStart]...)
	out = append(out, newFm...)
	out = append(out, data[fmEnd:]...)
	return WriteAtomic(path, out)
}

func locateFrontmatter(data []byte) (start, end int, err error) {
	if len(data) < 4 || string(data[0:3]) != "---" {
		return 0, 0, fmt.Errorf("no frontmatter")
	}
	nl := strings.IndexByte(string(data), '\n')
	if nl < 0 {
		return 0, 0, fmt.Errorf("malformed frontmatter")
	}
	start = nl + 1
	rest := data[start:]
	idx := 0
	for idx < len(rest) {
		lineEnd := idx
		for lineEnd < len(rest) && rest[lineEnd] != '\n' {
			lineEnd++
		}
		line := string(rest[idx:lineEnd])
		if strings.TrimRight(line, " \t\r") == "---" {
			return start, start + idx, nil
		}
		if lineEnd >= len(rest) {
			break
		}
		idx = lineEnd + 1
	}
	return 0, 0, fmt.Errorf("unclosed frontmatter")
}

func setFieldInBlock(fm []string, key, value string) []string {
	prefix := key + ":"
	for i, line := range fm {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, prefix) && (len(trim) == len(prefix) || trim[len(prefix)] == ' ') {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			fm[i] = indent + key + ": " + value
			return fm
		}
	}
	return append(fm, key+": "+value)
}

// setTopLevelField sets a top-level key and drops the block items under it.
// An empty value removes the key.
func setTopLevelField(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmStart, fmEnd, err := locateFrontmatter(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var fmLines []string
	if fm := strings.TrimRight(string(data[fmStart:fmEnd]), "\n"); fm != "" {
		fmLines = strings.Split(fm, "\n")
	}
	newLines := setTopLevelInBlock(fmLines, key, value)
	out := make([]byte, 0, len(data)+64)
	out = append(out, data[:fmStart]...)
	out = append(out, strings.Join(newLines, "\n")+"\n"...)
	out = append(out, data[fmEnd:]...)
	return WriteAtomic(path, out)
}

func setTopLevelInBlock(fm []string, key, value string) []string {
	prefix := key + ":"
	for i, line := range fm {
		if !strings.HasPrefix(line, prefix) || (len(line) > len(prefix) && line[len(prefix)] != ' ') {
			continue
		}
		end := i + 1
		for end < len(fm) && (fm[end] == "" || fm[end][0] == ' ' || fm[end][0] == '\t' || fm[end][0] == '-') {
			end++
		}
		out := append([]string{}, fm[:i]...)
		if value != "" {
			out = append(out, topLevelLine(key, value))
		}
		return append(out, fm[end:]...)
	}
	if value == "" {
		return fm
	}
	return append(fm, topLevelLine(key, value))
}

// topLevelLine is the text of key and value. A value that starts with a
// line break is a block under the key.
func topLevelLine(key, value string) string {
	if strings.HasPrefix(value, "\n") {
		return key + ":" + value
	}
	return key + ": " + value
}

// setSessionField sets a key under the session: mapping in frontmatter.
func setSessionField(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmStart, fmEnd, err := locateFrontmatter(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	fmLines := strings.Split(strings.TrimRight(string(data[fmStart:fmEnd]), "\n"), "\n")
	// Find session: block
	sessIdx := -1
	for i, line := range fmLines {
		if strings.TrimSpace(line) == "session:" || strings.HasPrefix(strings.TrimSpace(line), "session:") {
			sessIdx = i
			break
		}
	}
	entry := "  " + key + ": " + value
	if sessIdx < 0 {
		fmLines = append(fmLines, "session:", entry)
	} else {
		fmLines = setInSessionBlock(fmLines, sessIdx, key, entry)
	}
	newFm := strings.Join(fmLines, "\n") + "\n"
	out := make([]byte, 0, len(data)+64)
	out = append(out, data[:fmStart]...)
	out = append(out, newFm...)
	out = append(out, data[fmEnd:]...)
	return WriteAtomic(path, out)
}

// setInSessionBlock replaces key in the session child lines or inserts entry
// after the last child.
func setInSessionBlock(fmLines []string, sessIdx int, key, entry string) []string {
	prefix := key + ":"
	insertAt := sessIdx + 1
	for i := sessIdx + 1; i < len(fmLines); i++ {
		trim := strings.TrimSpace(fmLines[i])
		if trim == "" {
			insertAt = i + 1
			continue
		}
		// next top-level key (no indent)
		if len(fmLines[i]) > 0 && fmLines[i][0] != ' ' && fmLines[i][0] != '\t' {
			break
		}
		insertAt = i + 1
		if strings.HasPrefix(trim, prefix) && (len(trim) == len(prefix) || trim[len(prefix)] == ' ') {
			fmLines[i] = entry
			return fmLines
		}
	}
	// insert after session header / last child
	tail := append([]string{}, fmLines[insertAt:]...)
	fmLines = append(fmLines[:insertAt], entry)
	return append(fmLines, tail...)
}

// Spec is a structured job definition. The server composes the front matter,
// so a client does not write YAML by hand. Content, when set, wins over every
// other field.
type Spec struct {
	// ID is written as the first key. Render adds it to Content too when
	// Content has no valid id.
	ID          string
	Content     string
	Title       string
	Prompt      string
	Harness     string
	Model       string
	Effort      string
	Description string
	Folder      string
	EmailAccept []string
	Cron        string
	Every       string
	// Inputs is the inputs block in order.
	Inputs  Inputs
	Session SessionYAML
	Env     map[string]string
	Contact []string
	// Notify nil leaves the key out; an empty list writes notify: [].
	Notify []notify.Step
}

// Render composes JOB.md text. It returns Content unchanged when set.
func (s Spec) Render() (string, error) {
	folder, err := NormalizeFolder(s.Folder)
	if err != nil {
		return "", fmt.Errorf("folder: %w", err)
	}
	if strings.TrimSpace(s.Content) != "" {
		if folder != "" {
			return "", errors.New("folder: set the folder key in the content")
		}
		if s.ID == "" || LooksLikeID(scanID([]byte(s.Content))) {
			return s.Content, nil
		}
		out, err := WithID([]byte(s.Content), s.ID)
		return string(out), err
	}
	if strings.TrimSpace(s.Prompt) == "" {
		return "", errors.New("prompt or content is required")
	}
	fm := Frontmatter{
		ID:          s.ID,
		Title:       s.Title,
		Harness:     HarnessSpec{Name: s.Harness, Model: s.Model, Effort: s.Effort},
		Description: s.Description,
		Folder:      folder,
		Email:       emailSpec(s.EmailAccept),
		Session:     s.Session,
		Env:         s.Env,
		Triggers:    s.triggers(),
		Inputs:      s.Inputs,
	}
	if len(s.Contact) > 0 {
		if err := fm.Contact.Encode(s.Contact); err != nil {
			return "", err
		}
	}
	if s.Notify != nil {
		if err := notify.ValidateSteps(s.Notify); err != nil {
			return "", err
		}
		if err := fm.Notify.Encode(s.Notify); err != nil {
			return "", err
		}
	}
	block, err := yaml.Marshal(fm)
	if err != nil {
		return "", err
	}
	return "---\n" + string(block) + "---\n\n" + strings.TrimSpace(s.Prompt) + "\n", nil
}

func emailSpec(accept []string) *EmailYAML {
	if accept == nil {
		return nil
	}
	return &EmailYAML{Accept: accept}
}

func (s Spec) triggers() []TriggerYAML {
	var out []TriggerYAML
	if s.Cron != "" {
		out = append(out, TriggerYAML{Cron: s.Cron})
	}
	if s.Every != "" {
		out = append(out, TriggerYAML{Every: s.Every})
	}
	return out
}
