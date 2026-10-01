package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	config "github.com/trebi-ai/trebi/internal/config"
)

// Model-list sources.
const (
	// ModelSourceCLI: the harness CLI answered with its own catalog.
	ModelSourceCLI = "cli"
	// ModelSourceStatic: a list trebi ships for a harness whose CLI cannot
	// list models.
	ModelSourceStatic = "static"
	// ModelSourceNone: no list, not installed, or nothing parsed.
	// The UI keeps a free-text input then.
	ModelSourceNone = "none"
)

// ModelInfo is one model a harness CLI reports.
type ModelInfo struct {
	ID      string `json:"id"`
	Label   string `json:"label,omitempty"`
	Default bool   `json:"default,omitempty"`
}

// ModelList is the catalog a harness CLI reports.
type ModelList struct {
	Models []ModelInfo `json:"models"`
	Source string      `json:"source"`
	// Default is the model an empty trebi pin resolves to: what the CLI uses
	// when no --model flag is passed. Empty when no source names one.
	Default string `json:"default,omitempty"`
	Error   string `json:"error,omitempty"`
}

// modelListTimeout bounds one vendor CLI call.
const modelListTimeout = 6 * time.Second

// modelListArgs returns the argv for the harnesses whose CLI lists models.
// claude, codex, and copilot only have an interactive /model picker; they
// use staticModels.
func modelListArgs(name string) ([]string, bool) {
	switch name {
	case Grok, OpenCode, OpenCode2, Cursor:
		return []string{"models"}, true
	case Pi:
		return []string{"--list-models"}, true
	}
	return nil, false
}

// ListModels asks the harness CLI for its model catalog. The CLI runs in the
// same isolation env a launch uses; when that yields nothing it retries in the
// plain environment, because shared auth may live only in the user's home.
//
// Read-only: the vendor binary is never asked to write its config.
func ListModels(ctx context.Context, name string) ModelList {
	args, ok := modelListArgs(name)
	if !ok {
		list := staticModels(name)
		if list == nil {
			return emptyModelList("")
		}
		def := harnessConfigDefault(name)
		return ModelList{Models: markDefault(withDefault(list, def), def), Source: ModelSourceStatic, Default: def}
	}
	bin := modelBinary(name)
	if bin == "" {
		return emptyModelList(name + " is not installed")
	}
	var lastErr error
	for _, env := range []map[string]string{modelIsolationEnv(name), nil} {
		out, err := runModelCommand(ctx, bin, args, env)
		if models := parseModels(name, out); len(models) > 0 {
			def := resolvedDefault(ctx, name, bin, env, models)
			return ModelList{
				Models:  markDefault(models, def),
				Source:  ModelSourceCLI,
				Default: def,
			}
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return ModelList{
			Models: []ModelInfo{}, Source: ModelSourceNone,
			Default: harnessConfigDefault(name), Error: firstLine(lastErr.Error()),
		}
	}
	return ModelList{Models: []ModelInfo{}, Source: ModelSourceNone, Default: harnessConfigDefault(name)}
}

func emptyModelList(errMsg string) ModelList {
	return ModelList{Models: []ModelInfo{}, Source: ModelSourceNone, Error: errMsg}
}

// resolvedDefault is the model a run uses when trebi passes no --model flag:
// the CLI's own marker first, then the CLI's resolved config (opencode), then
// the config file the harness reads.
func resolvedDefault(ctx context.Context, name, bin string, env map[string]string, models []ModelInfo) string {
	for _, m := range models {
		if m.Default {
			return m.ID
		}
	}
	if name == OpenCode {
		if id := opencodeConfigDefault(ctx, bin, env); id != "" {
			return id
		}
	}
	return harnessConfigDefault(name)
}

// markDefault flags id in the list so the UI can label it.
func markDefault(models []ModelInfo, id string) []ModelInfo {
	if id == "" {
		return models
	}
	for i := range models {
		models[i].Default = models[i].ID == id
	}
	return models
}

// opencodeConfigDefault asks opencode for its resolved config and reads model,
// which merges the user's global config with trebi's harness file.
func opencodeConfigDefault(ctx context.Context, bin string, env map[string]string) string {
	out, _ := runModelCommand(ctx, bin, []string{"debug", "config"}, env)
	return parseOpencodeConfigModel(out)
}

func parseOpencodeConfigModel(out string) string {
	out = stripANSI(out)
	start := strings.IndexByte(out, '{')
	if start < 0 {
		return ""
	}
	var cfg struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &cfg); err != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Model)
}

// opencode2ConfigDefault reads the model a V2 run uses when trebi passes no
// pin. The file is the operator's own config: V2 ignores the trebi config
// directory. V1 stores a "provider/model" string, V2 stores
// {providerID, model}.
func opencode2ConfigDefault() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Model json.RawMessage `json:"model"`
	}
	if json.Unmarshal(data, &cfg) != nil || len(cfg.Model) == 0 {
		return ""
	}
	var pinned string
	if json.Unmarshal(cfg.Model, &pinned) == nil {
		return strings.TrimSpace(pinned)
	}
	var ref struct {
		ProviderID string `json:"providerID"`
		Model      string `json:"model"`
	}
	if json.Unmarshal(cfg.Model, &ref) != nil || ref.Model == "" {
		return ""
	}
	if ref.ProviderID == "" {
		return ref.Model
	}
	return ref.ProviderID + "/" + ref.Model
}

// harnessConfigDefault reads the default model from the config a harness run
// sees. Read-only, and "" when no config names one.
func harnessConfigDefault(name string) string {
	switch name {
	case Claude:
		return jsonStringField(filepath.Join(config.HarnessDir(Claude), "settings.json"), "model")
	case Codex:
		dir := os.Getenv("CODEX_HOME")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return ""
			}
			dir = filepath.Join(home, ".codex")
		}
		return tomlStringField(filepath.Join(dir, "config.toml"), "", "model")
	case Grok:
		return tomlStringField(filepath.Join(config.HarnessDir(Grok), "config.toml"), "models", "default")
	case OpenCode:
		return jsonStringField(filepath.Join(config.HarnessDir(OpenCode), "opencode.json"), "model")
	case OpenCode2:
		// V2 reads the operator's own config, not the trebi harness home.
		return opencode2ConfigDefault()
	case Cursor:
		return cursorConfigDefault()
	case Pi:
		path := filepath.Join(config.HarnessDir(Pi), "settings.json")
		provider := jsonStringField(path, "defaultProvider")
		model := jsonStringField(path, "defaultModel")
		if model == "" {
			return ""
		}
		if provider == "" {
			return model
		}
		return provider + "/" + model
	}
	return ""
}

// jsonStringField reads one string field of a JSON object; "" on any failure.
func jsonStringField(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return ""
	}
	value, _ := doc[key].(string)
	return strings.TrimSpace(value)
}

// cursorConfigDefault reads cli-config.json, where the picked model is an
// object ({"model": {"modelId": "…"}}).
func cursorConfigDefault() string {
	data, err := os.ReadFile(filepath.Join(config.HarnessDir(Cursor), "cli-config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Model struct {
			ModelID string `json:"modelId"`
		} `json:"model"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Model.ModelID)
}

// tomlStringField reads one string key, optionally inside a table.
func tomlStringField(path, table, key string) string {
	var doc map[string]any
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		return ""
	}
	section := doc
	if table != "" {
		next, ok := doc[table].(map[string]any)
		if !ok {
			return ""
		}
		section = next
	}
	value, _ := section[key].(string)
	return strings.TrimSpace(value)
}

// modelIsolationEnv is the harness-process env a launch of name uses
// (GROK_HOME, OPENCODE_CONFIG, …), so the list matches what trebi runs see.
func modelIsolationEnv(name string) map[string]string {
	tool, ok := ByName(name)
	if !ok {
		return nil
	}
	if p, ok := tool.(interface{ launchEnv() map[string]string }); ok {
		return p.launchEnv()
	}
	return nil
}

// modelBinary resolves the vendor binary a launch of name would run: the
// trebi store pin first, then PATH.
func modelBinary(name string) string {
	if name == Cursor {
		if bin := findCursorBinary(); bin != "" {
			return bin
		}
	}
	current := config.ToolCurrentLink(name)
	for _, candidate := range []string{
		filepath.Join(current, name),
		filepath.Join(current, "bin", name),
	} {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// runModelCommand runs one listing. The returned output is kept even on a
// non-zero exit: some CLIs print the catalog and then complain about auth.
func runModelCommand(ctx context.Context, bin string, args []string, extraEnv map[string]string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, modelListTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Env = mergedEnv(extraEnv)
	out, err := cmd.Output()
	if err == nil {
		return string(out), nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return string(out), fmt.Errorf("%s: %s", err, firstLine(string(ee.Stderr)))
	}
	return string(out), err
}

// mergedEnv overlays extra onto the current environment without duplicates.
func mergedEnv(extra map[string]string) []string {
	values := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			values[k] = v
		}
	}
	for k, v := range extra {
		values[k] = v
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+values[k])
	}
	return out
}

func parseModels(name, out string) []ModelInfo {
	switch name {
	case Grok:
		return parseGrokModels(out)
	case Cursor:
		return parseCursorModels(out)
	case OpenCode, OpenCode2:
		return parseOpencodeModels(out)
	case Pi:
		return parsePiModels(out)
	}
	return nil
}

// ansiRe strips SGR color codes; CLIs may colorize even when piped.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

var (
	grokDefaultRe = regexp.MustCompile(`(?m)^Default model:\s*(\S+)`)
	grokItemRe    = regexp.MustCompile(`(?m)^[ \t]*[*-][ \t]+(\S+)(?:[ \t]+(.*))?$`)
	cursorItemRe  = regexp.MustCompile(`(?m)^([ \t]*\*)?[ \t]*(\S+)[ \t]+-[ \t]+(.+?)[ \t]*$`)
	piRowSplitRe  = regexp.MustCompile(`[ \t]{2,}`)
	modelIDRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@:+-]*$`)
)

// parseGrokModels reads `grok models`:
//
//	Default model: grok-4.6
//
//	Available models:
//	  * grok-4.6 (default)
//	  - grok-4.5
func parseGrokModels(out string) []ModelInfo {
	out = stripANSI(out)
	defaultID := ""
	if m := grokDefaultRe.FindStringSubmatch(out); m != nil {
		defaultID = m[1]
	}
	var models []ModelInfo
	for _, m := range grokItemRe.FindAllStringSubmatch(out, -1) {
		id := strings.TrimSpace(m[1])
		if !modelIDRe.MatchString(id) {
			continue
		}
		label := strings.TrimSpace(m[2])
		isDefault := id == defaultID
		if strings.Contains(label, "(default)") {
			isDefault = true
			label = strings.TrimSpace(strings.ReplaceAll(label, "(default)", ""))
		}
		models = append(models, ModelInfo{ID: id, Label: strings.Trim(label, "()"), Default: isDefault})
	}
	return dedupeModels(models)
}

// parseCursorModels reads `agent models`:
//
//	Available models
//
//	auto - Auto (default)
//	gpt-5.3-codex-low - Codex 5.3 Low
//
// Older builds print "* id Label" lines instead; only those starred lines are
// trusted then, so an unrelated "Tip - read this" line cannot pass as a model.
func parseCursorModels(out string) []ModelInfo {
	out = stripANSI(out)
	var models []ModelInfo
	if body, ok := sectionAfter(out, "Available models"); ok {
		for _, line := range strings.Split(body, "\n") {
			m := cursorItemRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			id := m[2]
			if !modelIDRe.MatchString(id) {
				continue
			}
			label := strings.TrimSpace(m[3])
			isDefault := strings.TrimSpace(m[1]) == "*"
			if strings.Contains(label, "(default)") {
				isDefault = true
				label = strings.TrimSpace(strings.ReplaceAll(label, "(default)", ""))
			}
			models = append(models, ModelInfo{ID: id, Label: label, Default: isDefault})
		}
		return dedupeModels(models)
	}
	for _, line := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "*") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(trim, "*"))
		if len(fields) < 1 || !modelIDRe.MatchString(fields[0]) {
			continue
		}
		models = append(models, ModelInfo{
			ID:      fields[0],
			Label:   strings.Join(fields[1:], " "),
			Default: true,
		})
	}
	return dedupeModels(models)
}

// parseOpencodeModels reads `opencode models`: one provider/model per line.
func parseOpencodeModels(out string) []ModelInfo {
	out = stripANSI(out)
	var models []ModelInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.ContainsAny(line, " \t") {
			continue
		}
		if !strings.Contains(line, "/") || !modelIDRe.MatchString(line) {
			continue
		}
		models = append(models, ModelInfo{ID: line})
	}
	return dedupeModels(models)
}

// parsePiModels reads the `pi --list-models` table:
//
//	provider  model   context  max-out  thinking  images
//	xai       grok-4.3  1M       30K      yes       yes
//
// The value is provider/id: pi's --model accepts that form.
func parsePiModels(out string) []ModelInfo {
	out = stripANSI(out)
	var models []ModelInfo
	for _, line := range strings.Split(out, "\n") {
		fields := piRowSplitRe.Split(strings.TrimSpace(line), -1)
		if len(fields) < 2 {
			continue
		}
		provider, id := fields[0], fields[1]
		if provider == "provider" || id == "model" {
			continue
		}
		if !modelIDRe.MatchString(provider) || !modelIDRe.MatchString(id) {
			continue
		}
		models = append(models, ModelInfo{ID: provider + "/" + id})
	}
	return dedupeModels(models)
}

// sectionAfter returns the output after the first line equal to header and
// reports whether the header was found.
func sectionAfter(out, header string) (string, bool) {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			return strings.Join(lines[i+1:], "\n"), true
		}
	}
	return out, false
}

func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiRe.ReplaceAllString(s, "")
}

func dedupeModels(in []ModelInfo) []ModelInfo {
	seen := map[string]bool{}
	out := make([]ModelInfo, 0, len(in))
	for _, m := range in {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 200
	if len(s) > max {
		s = s[:max]
	}
	return s
}
