// Package job parses and discovers workspace job files with canonical
// trebi frontmatter only (B23 — no legacy aliases).
package job

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrg/frontmatter"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/secrets"
	"gopkg.in/yaml.v3"
)

// Frontmatter is the canonical YAML block.
type Frontmatter struct {
	WorkingDir       string            `yaml:"working_dir"`
	Harness          string            `yaml:"harness"`
	Runtime          string            `yaml:"runtime"`
	Title            string            `yaml:"title"`
	Memory           any               `yaml:"memory"`
	Envset           string            `yaml:"envset"`
	Env              map[string]string `yaml:"env"` // inline env map (new); never auto-written by migration
	Session          SessionYAML       `yaml:"session"`
	Triggers         []TriggerYAML     `yaml:"triggers"`
	Disabled         bool              `yaml:"disabled"`
	FollowupPrompt   string            `yaml:"followup_prompt"`
	AssistantContext bool              `yaml:"assistant_context"`
	Name             string            `yaml:"name"` // verified then ignored for JOB.md identity
	Description      string            `yaml:"description"`
	Allowlist        []string          `yaml:"allowlist"`
	Inputs           any               `yaml:"inputs"`
	Skills           any               `yaml:"skills"`
	Notify           any               `yaml:"notify"`
}

// SessionYAML — canonical keys; idle_timeout not timeout.
type SessionYAML struct {
	MaxDuration string `yaml:"max_duration,omitempty"`
	IdleTimeout string `yaml:"idle_timeout,omitempty"`
	KeepAlive   string `yaml:"keep_alive,omitempty"`
	Nudge       string `yaml:"nudge,omitempty"`
	Resumable   string `yaml:"resumable,omitempty"`
	IfRunning   string `yaml:"if_running,omitempty"`
}

// KnobsMS is the one resolver from session strings to launch knobs (B16).
// A bad duration parses to 0; validation reports it separately.
func (s SessionYAML) KnobsMS() (maxMS, idleMS, keepMS, nudgeMS int64) {
	if d, err := time.ParseDuration(s.MaxDuration); err == nil {
		maxMS = d.Milliseconds()
	}
	if d, err := time.ParseDuration(s.IdleTimeout); err == nil {
		idleMS = d.Milliseconds()
	}
	if d, err := time.ParseDuration(s.KeepAlive); err == nil {
		keepMS = d.Milliseconds()
	}
	if d, err := time.ParseDuration(s.Nudge); err == nil {
		nudgeMS = d.Milliseconds()
	}
	return
}

// TriggerYAML — unified triggers array (B21).
// Exactly one of Cron, Every, Event is set per entry.
type TriggerYAML struct {
	Cron           string    `yaml:"cron,omitempty"`
	Every          string    `yaml:"every,omitempty"`
	Event          string    `yaml:"event,omitempty"` // connector name
	Match          string    `yaml:"match,omitempty"`
	Extract        string    `yaml:"extract,omitempty"`
	Debounce       string    `yaml:"debounce,omitempty"`
	RateLimit      string    `yaml:"rate_limit,omitempty"`
	Acknowledge    any       `yaml:"acknowledge,omitempty"`
	Typing         any       `yaml:"typing,omitempty"`
	TypingInterval string    `yaml:"typing_interval,omitempty"`
	TypingMax      string    `yaml:"typing_max,omitempty"`
	FollowupPrompt string    `yaml:"followup_prompt,omitempty"`
	When           *WhenYAML `yaml:"when,omitempty"`
	Anchor         string    `yaml:"anchor,omitempty"` // @every phase
	Timezone       string    `yaml:"timezone,omitempty"`
	Allowlist      []string  `yaml:"allowlist,omitempty"`
	Enabled        *bool     `yaml:"enabled,omitempty"`
	Disabled       bool      `yaml:"disabled,omitempty"` // keeps family index (B21)
}

// WhenYAML — gate on a cron/every entry (B25).
type WhenYAML struct {
	Run       any               `yaml:"run"`
	Extract   string            `yaml:"extract,omitempty"`
	Match     string            `yaml:"match"`
	Timeout   string            `yaml:"timeout,omitempty"`
	AllowExit []int             `yaml:"allow_exit,omitempty"`
	Cwd       string            `yaml:"cwd,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
}

// Job is the resolved in-memory definition.
type Job struct {
	Name             string
	FilePath         string
	Dir              string
	WorkspaceID      string
	WorkspacePath    string
	WorkingDir       string
	Harness          string
	Runtime          string
	Title            string
	Memory           bool
	MemoryOpts       MemoryConfig
	Envset           string
	Env              map[string]string
	Session          SessionYAML
	Triggers         []Trigger
	Disabled         bool
	FollowupPrompt   string
	AssistantContext bool
	Prompt           string
	Kind             string // job | assistant | agent
	Owner            string // owning agent index name (agents/<name>) for jobs under an agent folder
	ParseError       string
	Hash             string
	Description      string
}

// MemoryConfig is the parsed memory frontmatter. Nil pointers mean unset;
// Resolve fills them from config. Explicit zeros stay (max_logs 0 = no cap).
type MemoryConfig struct {
	Enabled       bool
	MaxLogs       *int
	RequireUpdate *bool
	InjectLogs    *int
}

// Resolve returns the effective options after config defaults.
func (m MemoryConfig) Resolve(cfg config.MemoryDefaults) memory.Options {
	opt := memory.Options{
		MaxLogs:       cfg.MaxLogs,
		RequireUpdate: cfg.RequireUpdate,
		InjectLogs:    cfg.InjectLogs,
	}
	if m.MaxLogs != nil {
		opt.MaxLogs = *m.MaxLogs
	}
	if m.RequireUpdate != nil {
		opt.RequireUpdate = *m.RequireUpdate
	}
	if m.InjectLogs != nil {
		opt.InjectLogs = *m.InjectLogs
	}
	return opt
}

// LaunchDefaults is cwd, overlap policy, env, and session knobs for a run.
type LaunchDefaults struct {
	WorkingDir    string
	IfRunning     string
	Env           map[string]string
	MaxDurationMS int64
	IdleTimeoutMS int64
	KeepAliveMS   int64
	NudgeMS       int64
}

// ResolvedWorkingDir is working_dir, else the JOB.md directory.
// Agents default to the workspace root (C.3): <ws>/agents/<name> → <ws>.
func (j *Job) ResolvedWorkingDir() string {
	if j == nil {
		return ""
	}
	if j.WorkingDir != "" {
		return j.WorkingDir
	}
	if j.Kind == "agent" {
		return filepath.Dir(filepath.Dir(j.Dir))
	}
	return j.Dir
}

// ResolvedHarness is the job harness, else "claude".
func (j *Job) ResolvedHarness() string {
	if j != nil && j.Harness != "" {
		return j.Harness
	}
	return "claude"
}

// ResolvedEnv merges envset: then inline env:. Missing envset is ignored.
func (j *Job) ResolvedEnv() map[string]string {
	if j == nil {
		return nil
	}
	env := map[string]string{}
	if j.Envset != "" {
		if sets, err := config.LoadEnvSets(); err == nil {
			if kv, ok := sets.Get(j.Envset); ok {
				for k, v := range kv {
					env[k] = v
				}
			}
		}
	}
	for k, v := range j.Env {
		env[k] = v
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// LaunchEnv is ResolvedEnv with secret-name values expanded. A missing
// secret is left as the literal name and logged as secret.referenced_missing.
func (j *Job) LaunchEnv() map[string]string {
	env, missing := secrets.ExpandRefs(j.ResolvedEnv())
	for _, name := range missing {
		slog.Default().Info("secret.referenced_missing", "name", name)
	}
	return env
}

// LaunchDefaults returns the four-line launch seam (cwd / if_running / env / knobs).
func (j *Job) LaunchDefaults() LaunchDefaults {
	maxMS, idleMS, keepMS, nudgeMS := j.Session.KnobsMS()
	return LaunchDefaults{
		WorkingDir:    j.ResolvedWorkingDir(),
		IfRunning:     j.Session.IfRunning,
		Env:           j.LaunchEnv(),
		MaxDurationMS: maxMS,
		IdleTimeoutMS: idleMS,
		KeepAliveMS:   keepMS,
		NudgeMS:       nudgeMS,
	}
}

// Trigger is a normalized trigger entry with family index.
type Trigger struct {
	Family  string // cron | every | event
	Cron    string
	Every   string
	Event   string // connector
	Match   string
	Extract string
	// Allowlist is the ingress gate (security layer 1), checked before match.
	// Entry "key:value" (key an identity key) matches only that event field;
	// a bare "value" matches ANY identity field (flat OR).
	Allowlist []string
	Debounce  time.Duration
	RateLimit string
	When      *WhenYAML
	Anchor    string
	Timezone  string
	Index     int // per-family index (B21): schedule family shares cron+every; event separate
	Disabled  bool
	Raw       TriggerYAML
}

// ParseFile reads and parses a JOB.md or TREBI.md with canonical keys only.
func ParseFile(filePath string) (*Job, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filePath, err)
	}
	return ParseBytes(filePath, data)
}

// ParseBytes parses content; filePath is used for errors and name derivation.
func ParseBytes(filePath string, data []byte) (*Job, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:8])

	if err := rejectLegacyKeys(data); err != nil {
		return parseFail(filePath, hash, err)
	}

	var fm Frontmatter
	body, err := frontmatter.Parse(bytes.NewReader(data), &fm, frontmatter.NewFormat("---", "---", yaml.Unmarshal))
	if err != nil {
		return parseFail(filePath, hash, fmt.Errorf("parsing frontmatter in %s: %w", filePath, err))
	}

	// session.timeout is legacy — detect raw.
	if strings.Contains(string(data), "session:") && hasLegacySessionTimeout(data) {
		return parseFail(filePath, hash, fmt.Errorf("legacy key session.timeout: use session.idle_timeout"))
	}

	// An agent name: that differs from the folder basename is a validation
	// error (C.2). The row stays indexed with parse_error.
	if isAgentFile(filePath) && fm.Name != "" && fm.Name != filepath.Base(filepath.Dir(filePath)) {
		return parseFail(filePath, hash, fmt.Errorf("agent name %q must equal folder name %q", fm.Name, filepath.Base(filepath.Dir(filePath))))
	}

	memCfg, err := resolveMemory(fm.Memory)
	if err != nil {
		return parseFail(filePath, hash, fmt.Errorf("memory: %w", err))
	}
	// Agents and the assistant default to memory on (C.3); jobs stay opt-in.
	if fm.Memory == nil && (isAgentFile(filePath) || isAssistantFile(filePath)) {
		memCfg.Enabled = true
	}

	triggers, err := normalizeTriggers(fm.Triggers)
	if err != nil {
		return nil, fmt.Errorf("%s: triggers: %w", filePath, err)
	}

	// A name: mismatch with the path-derived name is soft (migration reports
	// it); parse keeps the path-derived name for jobs.
	name := deriveName(filePath)
	if isAssistantFile(filePath) && fm.Name != "" {
		name = fm.Name
	}

	j := &Job{
		Name:             name,
		FilePath:         filePath,
		Dir:              filepath.Dir(filePath),
		WorkingDir:       fm.WorkingDir,
		Harness:          fm.Harness,
		Runtime:          fm.Runtime,
		Title:            fm.Title,
		Memory:           memCfg.Enabled,
		MemoryOpts:       memCfg,
		Envset:           fm.Envset,
		Env:              fm.Env,
		Session:          fm.Session,
		Triggers:         triggers,
		Disabled:         fm.Disabled,
		FollowupPrompt:   fm.FollowupPrompt,
		AssistantContext: fm.AssistantContext,
		Prompt:           string(bytes.TrimSpace(body)),
		Kind:             kindFromPath(filePath),
		Hash:             hash,
		Description:      fm.Description,
	}
	return j, nil
}

func parseFail(path, hash string, err error) (*Job, error) {
	return &Job{
		Name:       deriveName(path),
		FilePath:   path,
		Dir:        filepath.Dir(path),
		ParseError: err.Error(),
		Hash:       hash,
		Kind:       kindFromPath(path),
	}, err
}

// ContentHash returns a short content hash for If-Match.
func ContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func rejectLegacyKeys(data []byte) error {
	// Parse raw map to detect forbidden top-level keys.
	// command/schedule/on/tool must cause a loud parse error (M1/B23).
	// Note: top-level `env` as a string is legacy (use envset); map form is canonical (B24).
	block, ok := extractFM(data)
	if !ok {
		return nil
	}
	var raw map[string]any
	if err := yaml.Unmarshal(block, &raw); err != nil {
		return nil // let frontmatter.Parse report YAML errors
	}
	for _, k := range []string{"command", "schedule", "on", "tool"} {
		if _, exists := raw[k]; exists {
			return fmt.Errorf("legacy key %q not accepted; use canonical trebi frontmatter (B23)", k)
		}
	}
	// env: string is legacy (envset); env: map is canonical inline env (B24).
	if v, exists := raw["env"]; exists {
		if _, ok := v.(map[string]any); !ok {
			// yaml may decode as map[any]any
			if _, ok := v.(map[any]any); !ok {
				return fmt.Errorf("legacy key \"env\" as set name not accepted; use envset: (B24)")
			}
		}
	}
	// top-level when: moved onto trigger entries
	if _, exists := raw["when"]; exists {
		return fmt.Errorf("legacy top-level key \"when\" not accepted; place when: on a triggers entry (B25)")
	}
	return nil
}

func hasLegacySessionTimeout(data []byte) bool {
	block, ok := extractFM(data)
	if !ok {
		return false
	}
	var raw map[string]any
	if err := yaml.Unmarshal(block, &raw); err != nil {
		return false
	}
	sess, ok := raw["session"].(map[string]any)
	if !ok {
		return false
	}
	_, has := sess["timeout"]
	return has
}

func extractFM(data []byte) ([]byte, bool) {
	if !bytes.HasPrefix(data, []byte("---")) {
		return nil, false
	}
	rest := data[3:]
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	}
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return nil, false
	}
	return rest[:end], true
}

func normalizeTriggers(raw []TriggerYAML) ([]Trigger, error) {
	var out []Trigger
	// B21: two index spaces — schedule (cron+every) and event.
	schedIdx, eventIdx := 0, 0
	for _, t := range raw {
		n := 0
		if t.Cron != "" {
			n++
		}
		if t.Every != "" {
			n++
		}
		if t.Event != "" {
			n++
		}
		if n == 0 {
			return nil, fmt.Errorf("trigger entry needs cron, every, or event")
		}
		if n > 1 {
			return nil, fmt.Errorf("trigger entry must set exactly one of cron, every, event")
		}
		tr := Trigger{Raw: t, When: t.When, Match: t.Match, Extract: t.Extract, Allowlist: t.Allowlist, Anchor: t.Anchor, Timezone: t.Timezone, Disabled: t.Disabled}
		if t.Enabled != nil && !*t.Enabled {
			tr.Disabled = true
		}
		if t.Debounce != "" {
			d, err := time.ParseDuration(t.Debounce)
			if err != nil {
				return nil, fmt.Errorf("debounce: %w", err)
			}
			tr.Debounce = d
		}
		tr.RateLimit = t.RateLimit
		switch {
		case t.Cron != "":
			tr.Family = "cron"
			tr.Cron = t.Cron
			tr.Index = schedIdx
			schedIdx++
		case t.Every != "":
			tr.Family = "every"
			tr.Every = t.Every
			tr.Index = schedIdx
			schedIdx++
		case t.Event != "":
			tr.Family = "event"
			tr.Event = t.Event
			tr.Index = eventIdx
			eventIdx++
		}
		out = append(out, tr)
	}
	return out, nil
}

func deriveName(filePath string) string {
	base := filepath.Base(filePath)
	dir := filepath.Base(filepath.Dir(filePath))
	switch strings.ToLower(base) {
	case "job.md":
		return dir
	case "trebi.md":
		return dir // default; frontmatter name: overrides for assistants
	case "agent.md":
		return dir // discover rewrites to agents/<name>
	default:
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
}

func isAssistantFile(path string) bool {
	return strings.EqualFold(filepath.Base(path), "TREBI.md")
}

func kindFromPath(path string) string {
	switch strings.ToLower(filepath.Base(path)) {
	case "trebi.md":
		return "assistant"
	case "agent.md":
		return "agent"
	default:
		return "job"
	}
}

// WhenResult is the outcome of a schedule when: gate.
type WhenResult struct {
	Fire   bool
	Skip   bool
	Vars   map[string]string
	Error  string
	Stdout string
}

// EvalWhen runs the shell gate and JS match/extract (B25).
func EvalWhen(ctx context.Context, w *WhenYAML, cwd string, env map[string]string) WhenResult {
	if w == nil || strings.TrimSpace(w.Match) == "" {
		return WhenResult{Fire: true}
	}
	cctx, cancel := context.WithTimeout(ctx, w.gateTimeout())
	defer cancel()

	var stdout, stderr bytes.Buffer
	exitCode := 0
	if runCmd := w.command(); runCmd != "" {
		var skip *WhenResult
		exitCode, skip = w.runGate(cctx, runCmd, cwd, env, &stdout, &stderr)
		if skip != nil {
			return *skip
		}
	}
	event := map[string]any{
		"stdout": stdout.String(),
		"stderr": stderr.String(),
		"exit":   exitCode,
		"lines":  strings.Split(stdout.String(), "\n"),
	}
	res := dispatch.EvalMatch(cctx, w.Match, w.Extract, event)
	if res.Error != "" {
		return WhenResult{Error: res.Error, Skip: true, Stdout: stdout.String()}
	}
	if !res.Matched {
		return WhenResult{Skip: true, Stdout: stdout.String(), Vars: res.Vars}
	}
	return WhenResult{Fire: true, Vars: res.Vars, Stdout: stdout.String()}
}

// gateTimeout returns the configured timeout or the 30s default.
func (w *WhenYAML) gateTimeout() time.Duration {
	timeout := 30 * time.Second
	if w.Timeout != "" {
		if d, err := time.ParseDuration(w.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}
	return timeout
}

// command renders run: (string or list) as one shell command.
func (w *WhenYAML) command() string {
	switch v := w.Run.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, p := range v {
			parts = append(parts, fmt.Sprint(p))
		}
		return strings.Join(parts, " ")
	default:
		if w.Run != nil {
			return fmt.Sprint(w.Run)
		}
		return ""
	}
}

// runGate runs the shell command into stdout/stderr. A non-nil result means
// the gate decided to skip; otherwise the exit code is allowed.
func (w *WhenYAML) runGate(ctx context.Context, runCmd, cwd string, env map[string]string, stdout, stderr *bytes.Buffer) (int, *WhenResult) {
	cmd := exec.CommandContext(ctx, "sh", "-c", runCmd)
	if w.Cwd != "" {
		cmd.Dir = w.Cwd
	} else if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range w.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	exitCode := 0
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else if ctx.Err() != nil {
			return exitCode, &WhenResult{Error: "when: timeout", Skip: true}
		} else {
			return exitCode, &WhenResult{Error: err.Error(), Skip: true}
		}
	}
	allowed := map[int]bool{0: true}
	for _, c := range w.AllowExit {
		allowed[c] = true
	}
	if !allowed[exitCode] {
		return exitCode, &WhenResult{Skip: true, Stdout: stdout.String(), Error: fmt.Sprintf("exit %d", exitCode)}
	}
	return exitCode, nil
}

func isAgentFile(path string) bool {
	return strings.EqualFold(filepath.Base(path), "AGENT.md")
}

// resolveMemory accepts bool, a non-empty string (legacy, true), nil, or a
// map of the three option keys. Object form implies enabled.
func resolveMemory(raw any) (MemoryConfig, error) {
	switch v := raw.(type) {
	case nil:
		return MemoryConfig{}, nil
	case bool:
		return MemoryConfig{Enabled: v}, nil
	case string:
		return MemoryConfig{Enabled: strings.TrimSpace(v) != ""}, nil
	case map[string]any:
		return memoryFromMap(v)
	case map[any]any:
		conv := make(map[string]any, len(v))
		for k, val := range v {
			conv[fmt.Sprint(k)] = val
		}
		return memoryFromMap(conv)
	default:
		return MemoryConfig{}, fmt.Errorf("expected bool or map, got %T", raw)
	}
}

func memoryFromMap(m map[string]any) (MemoryConfig, error) {
	cfg := MemoryConfig{Enabled: true}
	for k, v := range m {
		switch k {
		case "max_logs", "inject_logs":
			n, err := memoryInt(k, v)
			if err != nil {
				return MemoryConfig{}, err
			}
			if k == "max_logs" {
				cfg.MaxLogs = &n
			} else {
				cfg.InjectLogs = &n
			}
		case "require_update":
			b, ok := v.(bool)
			if !ok {
				return MemoryConfig{}, fmt.Errorf("require_update: expected bool, got %T", v)
			}
			cfg.RequireUpdate = &b
		default:
			return MemoryConfig{}, fmt.Errorf("unknown key %q (known: max_logs, require_update, inject_logs)", k)
		}
	}
	return cfg, nil
}

func memoryInt(key string, v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case uint64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("%s: expected int, got %T", key, v)
	}
}
