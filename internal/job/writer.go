package job

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

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

// patchScalars are the top-level frontmatter fields a patch may set.
var patchScalars = []string{"harness", "title", "envset", "description", "working_dir", "followup_prompt"} //nolint:gochecknoglobals // static field list, never mutated

// patchSessionFields map a dotted session key to its frontmatter field.
var patchSessionFields = map[string]string{ //nolint:gochecknoglobals // static field table, never mutated
	"session.max_duration": "max_duration",
	"session.idle_timeout": "idle_timeout",
	"session.keep_alive":   "keep_alive",
	"session.nudge":        "nudge",
	"session.resumable":    "resumable",
	"session.if_running":   "if_running",
}

// MergePatchFrontmatter applies a JSON-merge-patch of known scalar frontmatter fields.
// Supports: disabled, harness, title, envset, description, working_dir,
// followup_prompt, and session.* scalars. A nested "session" map is flattened
// to session.idle_timeout etc. via line rewrite. An unknown key is an error:
// a silent success would tell the caller a change landed when it did not.
func MergePatchFrontmatter(path string, patch map[string]any) error {
	if len(patch) == 0 {
		return nil
	}
	flattenSessionPatch(patch)
	if err := checkPatchKeys(patch); err != nil {
		return err
	}
	if err := applyDisabledPatch(path, patch); err != nil {
		return err
	}
	if err := applyScalarPatch(path, patch); err != nil {
		return err
	}
	return applySessionPatch(path, patch)
}

// checkPatchKeys rejects a key the writer does not honor.
func checkPatchKeys(patch map[string]any) error {
	known := map[string]bool{"disabled": true}
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
	valid := append([]string{"disabled"}, patchScalars...)
	valid = append(valid, "session")
	return fmt.Errorf("unknown patch field(s) %s; valid fields are %s",
		strings.Join(unknown, ", "), strings.Join(valid, ", "))
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
		if v, ok := patch[k]; ok && v != nil {
			if err := setScalarField(path, k, fmt.Sprint(v)); err != nil {
				return err
			}
		}
	}
	return nil
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
		if strings.TrimRight(line, " \t") == "---" {
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
	Content     string
	Prompt      string
	Harness     string
	Description string
	Cron        string
	Every       string
	// On is one event trigger: the connector name plus its optional gates.
	On      *EventSpec
	Session SessionYAML
	Env     map[string]string
}

// EventSpec is the on: field of a Spec.
type EventSpec struct {
	Event   string
	Match   string
	Extract string
}

// Render composes JOB.md text. It returns Content unchanged when set.
func (s Spec) Render() (string, error) {
	if strings.TrimSpace(s.Content) != "" {
		return s.Content, nil
	}
	if strings.TrimSpace(s.Prompt) == "" {
		return "", errors.New("prompt or content is required")
	}
	fm := Frontmatter{
		Harness:     s.Harness,
		Description: s.Description,
		Session:     s.Session,
		Env:         s.Env,
		Triggers:    s.triggers(),
	}
	if fm.Harness == "" {
		fm.Harness = "claude"
	}
	block, err := yaml.Marshal(fm)
	if err != nil {
		return "", err
	}
	return "---\n" + string(block) + "---\n\n" + strings.TrimSpace(s.Prompt) + "\n", nil
}

func (s Spec) triggers() []TriggerYAML {
	var out []TriggerYAML
	if s.Cron != "" {
		out = append(out, TriggerYAML{Cron: s.Cron})
	}
	if s.Every != "" {
		out = append(out, TriggerYAML{Every: s.Every})
	}
	if s.On != nil && s.On.Event != "" {
		out = append(out, TriggerYAML{Event: s.On.Event, Match: s.On.Match, Extract: s.On.Extract})
	}
	return out
}
