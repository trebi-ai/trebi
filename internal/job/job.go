// Package job parses and discovers workspace job files with canonical
// trebi frontmatter only (B23 — no legacy aliases).
package job

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/adrg/frontmatter"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/folderenv"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/state"
	"gopkg.in/yaml.v3"
)

// HarnessSpec is the harness: frontmatter node. It accepts the plain name
// (harness: claude) and the nested override form:
//
//	harness:
//	  name: claude
//	  model: opus
//	  effort: xhigh
//
// Model and effort are job-level overrides of the harness default; empty
// values fall through to tools.<name>.model|effort in config.yaml.
type HarnessSpec struct {
	Name   string `yaml:"name"`
	Model  string `yaml:"model,omitempty"`
	Effort string `yaml:"effort,omitempty"`
}

// UnmarshalYAML accepts a scalar name or a mapping.
func (h *HarnessSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		h.Name = strings.TrimSpace(value.Value)
		h.Model, h.Effort = "", ""
		return nil
	case yaml.MappingNode:
		type raw HarnessSpec
		var r raw
		if err := value.Decode(&r); err != nil {
			return err
		}
		*h = HarnessSpec(r)
		return nil
	default:
		return fmt.Errorf("harness: want a name or a {name, model, effort} map")
	}
}

// MarshalYAML writes the plain scalar while no override is set, so existing
// files keep their one-line form.
func (h HarnessSpec) MarshalYAML() (any, error) {
	if h.Model == "" && h.Effort == "" {
		return h.Name, nil
	}
	type plain HarnessSpec
	return plain(h), nil
}

// Frontmatter is the canonical YAML block.
type Frontmatter struct {
	ID             string            `yaml:"id,omitempty"`
	WorkingDir     string            `yaml:"working_dir,omitempty"`
	EnvFile        *bool             `yaml:"env_file,omitempty"`
	Harness        HarnessSpec       `yaml:"harness"`
	Runtime        string            `yaml:"runtime,omitempty"`
	Title          string            `yaml:"title,omitempty"`
	Memory         any               `yaml:"memory,omitempty"`
	State          any               `yaml:"state,omitempty"`
	Envset         EnvsetList        `yaml:"envset,omitempty"`
	Env            map[string]string `yaml:"env,omitempty"` // inline env map (new); never auto-written by migration
	Session        SessionYAML       `yaml:"session,omitempty"`
	Triggers       []TriggerYAML     `yaml:"triggers,omitempty"`
	Disabled       bool              `yaml:"disabled,omitempty"`
	FollowupPrompt string            `yaml:"followup_prompt,omitempty"`
	Name           string            `yaml:"name,omitempty"` // verified then ignored for JOB.md identity
	Description    string            `yaml:"description,omitempty"`
	Folder         string            `yaml:"folder,omitempty"`
	Inputs         Inputs            `yaml:"inputs,omitempty"`
	Skills         any               `yaml:"skills,omitempty"`
	Connectors     any               `yaml:"connectors,omitempty"`
	Email          *EmailYAML        `yaml:"email,omitempty"`
	Kit            *KitYAML          `yaml:"kit,omitempty"`
	Budget         *BudgetYAML       `yaml:"budget,omitempty"`
	Report         *ReportYAML       `yaml:"report,omitempty"`
	Team           *TeamYAML         `yaml:"team,omitempty"`
	Artifacts      []ArtifactYAML    `yaml:"artifacts,omitempty"`
	KeepRuns       *int              `yaml:"keep_runs,omitempty"`
	// Notify is the alert route; Contact is the owner address list. See
	// internal/notify.
	Notify  yaml.Node `yaml:"notify,omitempty"`
	Contact yaml.Node `yaml:"contact,omitempty"`
}

// SessionYAML — canonical keys; idle_timeout not timeout.
type SessionYAML struct {
	MaxDuration string `yaml:"max_duration,omitempty"`
	IdleTimeout string `yaml:"idle_timeout,omitempty"`
	KeepAlive   string `yaml:"keep_alive,omitempty"`
	Nudge       string `yaml:"nudge,omitempty"`
	Resumable   string `yaml:"resumable,omitempty"`
	IfRunning   string `yaml:"if_running,omitempty"`
	// Continue is how long after a run ends a new event of the same
	// conversation continues its session. Empty is off.
	Continue string `yaml:"continue,omitempty"`
}

// ContinueMS parses session.continue: a Go duration or whole days ("7d").
func (s SessionYAML) ContinueMS() (int64, error) {
	v := strings.TrimSpace(s.Continue)
	if v == "" {
		return 0, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("continue %q: must be a duration such as 12h or 7d", v)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			return 0, fmt.Errorf("continue %q: must be a duration such as 12h or 7d", v)
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("continue %q: must not be negative", v)
	}
	return d.Milliseconds(), nil
}

// ValidateIfRunning rejects an if_running value that is not queue, skip,
// replace, or empty (queue).
func ValidateIfRunning(v string) error {
	switch v {
	case "", "queue", "skip", "replace":
		return nil
	}
	return fmt.Errorf("if_running %q: must be queue, skip, or replace", v)
}

// ErrFolderNoOwner is the error of a folder on a job with no owner agent.
var ErrFolderNoOwner = errors.New("folder: only a job of an agent can have a folder")

// CheckFolderOwner returns ErrFolderNoOwner when a job with no owner agent has a folder.
func (j *Job) CheckFolderOwner(owner string) error {
	if j.Folder != "" && owner == "" {
		return ErrFolderNoOwner
	}
	return nil
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
	Cron      string    `yaml:"cron,omitempty"`
	Every     string    `yaml:"every,omitempty"`
	Event     string    `yaml:"event,omitempty"` // connector name
	Match     string    `yaml:"match,omitempty"`
	Extract   string    `yaml:"extract,omitempty"`
	Limit     string    `yaml:"limit,omitempty"` // N/<duration> | off; event only
	When      *WhenYAML `yaml:"when,omitempty"`
	Timezone  string    `yaml:"timezone,omitempty"`
	Allowlist []string  `yaml:"allowlist,omitempty"`
	Rooms     []string  `yaml:"rooms,omitempty"`
	Contains  textList  `yaml:"contains,omitempty"` // event only; any entry, case-insensitive
	From      string    `yaml:"from,omitempty"`     // people | bots | anyone; event only
	Replies   string    `yaml:"replies,omitempty"`  // continue | off; job event only
	Batch     string    `yaml:"batch,omitempty"`    // <duration> | off; event only
	Disabled  bool      `yaml:"disabled,omitempty"` // keeps family index (B21)
	Types     []string  `yaml:"types,omitempty"`    // event types; event only
	Manual    bool      `yaml:"manual,omitempty"`   // a person starts the run; event only
	Inputs    InputMap  `yaml:"inputs,omitempty"`   // input name to a literal or a {{ }} template

	// Removed keys. A set value is a parse error that names the fix.
	Debounce       string `yaml:"debounce,omitempty" json:"-"`
	RateLimit      string `yaml:"rate_limit,omitempty" json:"-"`
	Anchor         string `yaml:"anchor,omitempty" json:"-"`
	Enabled        *bool  `yaml:"enabled,omitempty" json:"-"`
	FollowupPrompt string `yaml:"followup_prompt,omitempty" json:"-"`
}

// textList is one string or a list of strings in YAML. One entry writes
// back as a scalar.
type textList []string

func (l *textList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*l = textList{n.Value}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*l = list
	return nil
}

func (l textList) MarshalYAML() (any, error) {
	if len(l) == 1 {
		return l[0], nil
	}
	return []string(l), nil
}

// removedKey returns the parse error of the first removed key that t sets.
func (t TriggerYAML) removedKey() error {
	switch {
	case t.Debounce != "":
		return fmt.Errorf(`trigger key "debounce" is removed; use "limit: N/<duration>" (default %s)`, DefaultLimit)
	case t.RateLimit != "":
		return fmt.Errorf(`trigger key "rate_limit" is removed; use "limit: N/<duration>" (default %s)`, DefaultLimit)
	case t.Anchor != "":
		return fmt.Errorf(`trigger key "anchor" is removed; use cron for a fixed start time`)
	case t.Enabled != nil:
		return fmt.Errorf(`trigger key "enabled" is removed; use "disabled: true"`)
	case t.FollowupPrompt != "":
		return fmt.Errorf(`trigger key "followup_prompt" is removed; set followup_prompt on the job, not on a trigger`)
	}
	return nil
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
	// ID is the static identity from the id key. Empty when IDState is not IDOK.
	ID            string
	IDState       IDState
	Name          string
	FilePath      string
	Dir           string
	WorkspaceID   string
	WorkspacePath string
	WorkingDir    string
	// EnvFile is the env_file key; nil is not set.
	EnvFile *bool
	Harness string
	// Model and Effort are job-level overrides of the harness default. Empty
	// falls through to tools.<harness>.model|effort in config.yaml.
	Model      string
	Effort     string
	Runtime    string
	Title      string
	Memory     bool
	MemoryOpts MemoryConfig
	State      map[string]state.Collection // declared state collections; nil when none
	Envset     EnvsetList
	Env        map[string]string
	Session    SessionYAML
	Triggers   []Trigger
	// Inputs is the declared inputs block in file order.
	Inputs         Inputs
	Disabled       bool
	FollowupPrompt string
	Skills         []string
	Connectors     []string
	Prompt         string
	Kind           string // job | assistant | agent
	Owner          string // owning agent index name (agents/<name>) for jobs under an agent folder
	Folder         string // group label under the owner agent; one level, no slash
	EmailAccept    []string
	EmailStyle     *StyleYAML
	AcceptSet      bool
	Accept         []AcceptEntry
	Kit            *KitYAML
	// LegacyKitMCP lists an old kit.mcp list; nil when the file has none.
	LegacyKitMCP []string
	Budget       *BudgetYAML
	Report       *ReportYAML
	Team         *TeamYAML
	Artifacts    []ArtifactYAML
	// KeepRuns overrides defaults.keep_runs; nil is not set, 0 keeps all.
	KeepRuns    *int
	ParseError  string
	Hash        string
	Description string
	// Notify is the alert route. NotifySet is true when the file holds the
	// key, also as an empty list (no notification).
	Notify    []notify.Step
	NotifySet bool
	Contact   []string
}

// ProtectedChange reports identity, trust, and spend keys changed from this
// parsed document to next. AGENT.md proposals mark these keys for the human
// reviewer; API scope checks use the same source of truth.
func (j *Job) ProtectedChange(next *Job) []string {
	if j == nil || next == nil {
		return nil
	}
	var out []string
	if !equalStrings(j.EmailAccept, next.EmailAccept) {
		out = append(out, "email.accept")
	}
	if !reflect.DeepEqual(j.Kit, next.Kit) {
		out = append(out, "kit")
	}
	if !equalBudget(j.Budget, next.Budget) {
		out = append(out, "budget")
	}
	if !reflect.DeepEqual(j.Report, next.Report) {
		out = append(out, "report")
	}
	if !reflect.DeepEqual(j.Team, next.Team) {
		out = append(out, "team")
	}
	if !equalStrings(j.Contact, next.Contact) {
		out = append(out, "contact")
	}
	if j.NotifySet != next.NotifySet || !reflect.DeepEqual(j.Notify, next.Notify) {
		out = append(out, "notify")
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalBudget(a, b *BudgetYAML) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// MemoryConfig is the parsed memory frontmatter. Nil pointers mean unset;
// Resolve fills them from config. Explicit zeros stay (max_logs 0 = no cap).
type MemoryConfig struct {
	Enabled       bool
	MaxLogs       *int
	RequireUpdate *bool
	InjectLogs    *int
	MaxBytes      *int
}

// Resolve returns the effective options after config defaults.
func (m MemoryConfig) Resolve(cfg config.MemoryDefaults) memory.Options {
	opt := memory.Options{
		MaxLogs:       cfg.MaxLogs,
		RequireUpdate: cfg.RequireUpdate,
		InjectLogs:    cfg.InjectLogs,
		MaxBytes:      cfg.MaxBytes,
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
	if m.MaxBytes != nil {
		opt.MaxBytes = *m.MaxBytes
	}
	return opt
}

// LaunchDefaults is cwd, overlap policy, env, and session knobs for a run.
type LaunchDefaults struct {
	WorkingDir    string
	IfRunning     string
	Env           map[string]string
	Model         string
	Effort        string
	MaxDurationMS int64
	IdleTimeoutMS int64
	KeepAliveMS   int64
	NudgeMS       int64
	ContinueMS    int64
	// FolderEnv is the .env of WorkingDir that Env holds. It is nil when the
	// folder has no .env or the load is off.
	FolderEnv *folderenv.Summary
}

// Sources of a resolved working folder.
const (
	WorkingDirFromJob     = "job"
	WorkingDirFromAgent   = "agent"
	WorkingDirFromDefault = "default"
)

// ResolvedWorkingDir is the run folder and its source: working_dir, else the
// owner agent working_dir in kit, else the JOB.md directory. Agents default
// to the workspace root (C.3): <ws>/agents/<name> → <ws>.
func (j *Job) ResolvedWorkingDir(kit *KitYAML) (dir, source string) {
	if j == nil {
		return "", ""
	}
	if j.WorkingDir != "" {
		return j.ExpandPath(j.WorkingDir), WorkingDirFromJob
	}
	if kit != nil && kit.WorkingDir != "" {
		return kit.WorkingDir, WorkingDirFromAgent
	}
	if j.Kind == "agent" {
		return filepath.Dir(filepath.Dir(j.Dir)), WorkingDirFromDefault
	}
	return j.Dir, WorkingDirFromDefault
}

// ExpandPath expands a leading ~ and resolves a relative path from the
// folder of the definition file.
func (j *Job) ExpandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	if filepath.IsAbs(p) || j.Dir == "" {
		return p
	}
	return filepath.Join(j.Dir, p)
}

// OwnerKit is the kit that owned jobs inherit: the kit block plus the
// expanded working_dir and the env_file of this agent. The agent env_file
// wins over kit.env_file. It is nil when the agent sets none of them.
func (j *Job) OwnerKit() *KitYAML {
	if j == nil || (j.Kit == nil && j.WorkingDir == "" && j.EnvFile == nil) {
		return nil
	}
	var kit KitYAML
	if j.Kit != nil {
		kit = *j.Kit
	}
	if j.WorkingDir != "" {
		kit.WorkingDir = j.ExpandPath(j.WorkingDir)
	}
	if j.EnvFile != nil {
		kit.EnvFile = j.EnvFile
	}
	return &kit
}

// ResolvedHarness is the job harness, else "claude".
func (j *Job) ResolvedHarness() string {
	if j != nil && j.Harness != "" {
		return j.Harness
	}
	return "claude"
}

// HarnessPin returns the tools.<harness> pin from config.yaml, or nil.
func (j *Job) HarnessPin() *config.ToolPin {
	c, err := config.Load()
	if err != nil || c == nil {
		return nil
	}
	return c.Tools.Pin(j.ResolvedHarness())
}

// ResolvedModel is the job model override, else the harness default, else "".
func (j *Job) ResolvedModel() string {
	if j != nil && j.Model != "" {
		return j.Model
	}
	if p := j.HarnessPin(); p != nil {
		return p.Model
	}
	return ""
}

// ResolvedEffort is the job effort override, else the harness default, else "".
func (j *Job) ResolvedEffort() string {
	if j != nil && j.Effort != "" {
		return j.Effort
	}
	if p := j.HarnessPin(); p != nil {
		return p.Effort
	}
	return ""
}

// ResolvedHarnessEnv is tools.<harness>.env (empty when unset).
func (j *Job) ResolvedHarnessEnv() map[string]string {
	if p := j.HarnessPin(); p != nil {
		return p.Env
	}
	return nil
}

// ResolvedEnv merges the envset names in list order, then inline env:. A
// later set wins on a key conflict, and inline env: wins over every set. A
// name that is not defined is ignored.
func (j *Job) ResolvedEnv() map[string]string {
	if j == nil {
		return nil
	}
	env := map[string]string{}
	if sets, err := config.LoadEnvSets(); err == nil {
		for _, name := range j.Envset {
			if kv, ok := sets.Get(name); ok {
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

// LaunchEnv is the harness-process environment: tools.<harness>.env first,
// then the job envset list + inline env (inline wins), with secret-name values
// expanded. A missing secret is left as the literal name and logged as
// secret.referenced_missing.
func (j *Job) LaunchEnv() map[string]string {
	env := map[string]string{}
	for k, v := range j.ResolvedHarnessEnv() {
		env[k] = v
	}
	for k, v := range j.ResolvedEnv() {
		env[k] = v
	}
	if len(env) == 0 {
		return nil
	}
	expanded, missing := secrets.ExpandRefs(env)
	for _, name := range missing {
		slog.Default().Info("secret.referenced_missing", "name", name)
	}
	return expanded
}

// LoadsFolderEnv reports whether a launch loads the working dir .env: the
// job env_file, else the kit env_file, else true. The assistant never does.
func (j *Job) LoadsFolderEnv(kit *KitYAML) bool {
	switch {
	case j == nil || j.Kind == "assistant":
		return false
	case j.EnvFile != nil:
		return *j.EnvFile
	case kit != nil && kit.EnvFile != nil:
		return *kit.EnvFile
	}
	return true
}

// FolderEnvSummary is the working dir .env view for the job and agent
// pages: found, on or off, and the keys that job values replace.
func (j *Job) FolderEnvSummary(kit *KitYAML) folderenv.Summary {
	if j == nil || j.Kind == "assistant" {
		return (*folderenv.File)(nil).Summary(false)
	}
	if !j.LoadsFolderEnv(kit) {
		wd, _ := j.ResolvedWorkingDir(kit)
		return folderenv.Load(wd).Summary(false)
	}
	if d := j.LaunchDefaultsWithKit(kit); d.FolderEnv != nil {
		return *d.FolderEnv
	}
	return (*folderenv.File)(nil).Summary(true)
}

// LaunchDefaults returns the four-line launch seam (cwd / if_running / env / knobs).
func (j *Job) LaunchDefaults() LaunchDefaults {
	return j.launchDefaults(nil)
}

// LaunchDefaultsWithKit returns launch defaults with the owning agent's kit
// merged under this job's own keys. A job envset list replaces the agent
// envset list as a whole; individual job env values win after both env
// value maps have been applied. The agent working_dir applies when the job
// sets none.
func (j *Job) LaunchDefaultsWithKit(kit *KitYAML) LaunchDefaults {
	if j == nil || kit == nil {
		return j.LaunchDefaults()
	}
	return j.withKit(kit).launchDefaults(kit)
}

// SecretRefs lists the secret names that the launch env names with the
// secret: prefix, with the kit merged as in LaunchDefaultsWithKit. The list
// is sorted and has no duplicates.
func (j *Job) SecretRefs(kit *KitYAML) []string {
	if j == nil {
		return nil
	}
	merged := j
	if kit != nil {
		merged = j.withKit(kit)
	}
	env := maps.Clone(merged.ResolvedHarnessEnv())
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, merged.ResolvedEnv())
	var out []string
	for _, v := range env {
		if name, ok := strings.CutPrefix(v, "secret:"); ok && name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// withKit is a copy of j with the kit envset and env merged under the job
// keys.
func (j *Job) withKit(kit *KitYAML) *Job {
	merged := *j
	merged.Envset = kit.Envset
	merged.Env = maps.Clone(kit.Env)
	if len(j.Envset) > 0 {
		merged.Envset = j.Envset
	}
	if len(j.Envset) > 0 || len(j.Env) > 0 {
		// ResolvedEnv starts from the effective envset list and applies Env.
		merged.Env = maps.Clone(j.Env)
		for k, v := range kit.Env {
			if _, exists := merged.Env[k]; !exists {
				if merged.Env == nil {
					merged.Env = map[string]string{}
				}
				merged.Env[k] = v
			}
		}
	}
	return &merged
}

// Trigger is a normalized trigger entry with family index.
type Trigger struct {
	Family  string // cron | every | event
	Cron    string
	Every   string
	Event   string // connector
	Match   string
	Extract string
	// Allowlist is the sender gate (security layer 1), checked before match.
	// Each entry is compared with the event identity (the sender id).
	Allowlist []string
	// Rooms is the room scope of an event trigger. Empty takes every room.
	// Each entry is a room ID of the connector.
	Rooms []string
	// Contains takes an event whose text contains any entry, case-insensitive.
	Contains []string
	// From is FromPeople, FromBots, or FromAnyone. Empty is FromAnyone.
	From string
	// Replies is RepliesContinue or RepliesOff. Empty is RepliesOff.
	Replies string
	// Limit is the file value of an event trigger: N/<duration>, off, or
	// empty for DefaultLimit. EffectiveLimit is the resolved value.
	Limit          string
	EffectiveLimit string
	// Batch is the file value of an event trigger: a duration, off, or
	// empty. BatchMS is the resolved window; 0 is no batch.
	Batch    string
	BatchMS  int64
	When     *WhenYAML
	Timezone string
	Index    int // per-family index (B21): schedule family shares cron+every; event separate
	Disabled bool
	// Types limits an event trigger to these event types. Empty takes all.
	Types []string
	// Manual marks an event trigger that runs only when a person starts it.
	Manual bool
	// Inputs maps an input name to a literal or a {{ }} template.
	Inputs InputMap
	Raw    TriggerYAML
}

// UnmarshalYAML reads one trigger in the canonical YAML form, as in the
// assistant block of trebi.yaml. Index stays 0; the reader numbers a list.
func (t *Trigger) UnmarshalYAML(n *yaml.Node) error {
	var raw TriggerYAML
	if err := n.Decode(&raw); err != nil {
		return err
	}
	out, err := normalizeTriggers([]TriggerYAML{raw})
	if err != nil {
		return err
	}
	if out[0].Continues() {
		return errRepliesNotJob
	}
	if out[0].BatchMS > 0 {
		return errBatchNotOperator
	}
	*t = out[0]
	return nil
}

// MarshalYAML writes the canonical form back. A trigger that did not come
// from YAML (a JSON body) is built from its fields.
func (t Trigger) MarshalYAML() (any, error) {
	if !reflect.ValueOf(t.Raw).IsZero() {
		return t.Raw, nil
	}
	raw := TriggerYAML{
		Cron: t.Cron, Every: t.Every, Event: t.Event, Match: t.Match, Extract: t.Extract,
		Limit: t.Limit, When: t.When, Timezone: t.Timezone,
		Allowlist: t.Allowlist, Rooms: t.Rooms, Contains: t.Contains, From: t.From, Replies: t.Replies,
		Batch: t.Batch, Disabled: t.Disabled, Types: t.Types, Manual: t.Manual, Inputs: t.Inputs,
	}
	return raw, nil
}

// Continues reports whether a reply to a run continues the run (replies: continue).
func (t Trigger) Continues() bool { return t.Event != "" && t.Replies == RepliesContinue }

// DefaultLimit is the limit of an event trigger that sets none.
const DefaultLimit = "15/10m"

// LimitOff is the limit value that removes the limit.
const LimitOff = "off"

// Values of the event trigger keys from and replies.
const (
	FromPeople      = "people"
	FromBots        = "bots"
	FromAnyone      = "anyone"
	RepliesContinue = "continue"
	RepliesOff      = "off"
)

var errRepliesNotJob = errors.New(`"replies" applies only to a job trigger; an agent keeps its channel conversation`)

var errBatchNotOperator = errors.New(`"batch" applies only to a job or agent trigger`)

// Limits of the batch key of an event trigger.
const (
	BatchMin = time.Minute
	BatchMax = 24 * time.Hour
	BatchOff = "off"
)

// Batched reports whether the trigger collects its events into one run per
// window (batch: <duration>).
func (t Trigger) Batched() bool { return t.Event != "" && t.BatchMS > 0 }

// BatchWindow is the batch window of the trigger; 0 is no batch.
func (t Trigger) BatchWindow() time.Duration { return time.Duration(t.BatchMS) * time.Millisecond }

// batchVars are the prompt vars of a batch run.
var batchVars = map[string]bool{"events": true, "event_count": true, "events_file": true, "connection": true} //nolint:gochecknoglobals // static set

// singleEventVars are the vars of one event; a batch run leaves them empty.
var singleEventVars = map[string]bool{ //nolint:gochecknoglobals // static set
	"text": true, "sender": true, "sender_id": true, "room": true, "room_name": true,
	"thread": true, "message_id": true, "reply_to": true,
}

// launchDefaults resolves the launch for one owner kit. The folder .env sits
// above tools.<harness>.env and below the job envset and env values. Folder
// values are literal: they are not secret references.
func (j *Job) launchDefaults(kit *KitYAML) LaunchDefaults {
	wd, _ := j.ResolvedWorkingDir(kit)
	maxMS, idleMS, keepMS, nudgeMS := j.Session.KnobsMS()
	continueMS, _ := j.Session.ContinueMS() // Parse rejects a bad value
	d := LaunchDefaults{
		WorkingDir:    wd,
		IfRunning:     j.Session.IfRunning,
		Env:           j.LaunchEnv(),
		Model:         j.ResolvedModel(),
		Effort:        j.ResolvedEffort(),
		MaxDurationMS: maxMS,
		IdleTimeoutMS: idleMS,
		KeepAliveMS:   keepMS,
		NudgeMS:       nudgeMS,
		ContinueMS:    continueMS,
	}
	if !j.LoadsFolderEnv(kit) {
		return d
	}
	f := folderenv.Load(wd)
	if f == nil {
		return d
	}
	sum := f.Summary(true)
	d.FolderEnv = &sum
	if len(f.Values) == 0 {
		return d
	}
	jobEnv := j.ResolvedEnv()
	if d.Env == nil {
		d.Env = map[string]string{}
	}
	for _, k := range f.Keys {
		if _, job := jobEnv[k]; job {
			sum.Overridden = append(sum.Overridden, k)
			continue
		}
		d.Env[k] = f.Values[k]
	}
	return d
}

// ParseBatch resolves a batch value to milliseconds: empty or off is 0.
func ParseBatch(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == BatchOff {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		return 0, fmt.Errorf(`batch %q: not a duration; use a value such as 10m, or "off"`, v)
	case d < BatchMin:
		return 0, fmt.Errorf(`batch %q: must be at least 1m`, v)
	case d > BatchMax:
		return 0, fmt.Errorf(`batch %q: must be at most 24h`, v)
	}
	return d.Milliseconds(), nil
}

// RateLimit returns the rate of an event trigger, or nil when the limit is
// off or the trigger is not an event trigger.
func (t Trigger) RateLimit() *dispatch.RateLimit {
	if t.Event == "" {
		return nil
	}
	rate, _ := parseLimit(t.Limit)
	return rate
}

// NumberTriggers sets Index on a list read one trigger at a time: the
// schedule family (cron and every) and the event family count apart.
func NumberTriggers(list []Trigger) {
	sched, event := 0, 0
	for i := range list {
		switch list[i].Family {
		case "cron", "every":
			list[i].Index = sched
			sched++
		case "event":
			list[i].Index = event
			event++
		}
	}
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
	hash := fileHash(data)

	if err := rejectLegacyKeys(data); err != nil {
		return parseFail(filePath, data, hash, err)
	}

	var fm Frontmatter
	body, err := frontmatter.Parse(bytes.NewReader(data), &fm, frontmatter.NewFormat("---", "---", yaml.Unmarshal))
	if err != nil {
		return parseFail(filePath, data, hash, fmt.Errorf("parsing frontmatter in %s: %w", filePath, err))
	}

	// session.timeout is legacy — detect raw.
	if strings.Contains(string(data), "session:") && hasLegacySessionTimeout(data) {
		return parseFail(filePath, data, hash, fmt.Errorf("legacy key session.timeout: use session.idle_timeout"))
	}

	// An agent name: that differs from the folder basename is a validation
	// error (C.2). The row stays indexed with parse_error.
	if isAgentFile(filePath) && fm.Name != "" && fm.Name != filepath.Base(filepath.Dir(filePath)) {
		return parseFail(filePath, data, hash, fmt.Errorf("agent name %q must equal folder name %q", fm.Name, filepath.Base(filepath.Dir(filePath))))
	}

	accept, rawAccept, acceptSet, err := normalizeOperatorKeys(filePath, &fm)
	if err != nil {
		return parseFail(filePath, data, hash, err)
	}
	var style *StyleYAML
	if fm.Email != nil {
		style = fm.Email.Style
	}

	memCfg, err := resolveMemory(fm.Memory)
	if err != nil {
		return parseFail(filePath, data, hash, fmt.Errorf("memory: %w", err))
	}
	// Agents default to memory on (C.3); jobs stay opt-in.
	if fm.Memory == nil && isAgentFile(filePath) {
		memCfg.Enabled = true
	}

	collections, err := state.ParseCollections(fm.State)
	if err != nil {
		return parseFail(filePath, data, hash, err)
	}

	triggers, err := normalizeTriggers(fm.Triggers)
	if err != nil {
		return parseFail(filePath, data, hash, fmt.Errorf("triggers: %w", err))
	}
	if kindFromPath(filePath) == "agent" && slices.ContainsFunc(triggers, Trigger.Continues) {
		return parseFail(filePath, data, hash, fmt.Errorf("triggers: %w", errRepliesNotJob))
	}
	if err := ValidateIfRunning(fm.Session.IfRunning); err != nil {
		return parseFail(filePath, data, hash, fmt.Errorf("session: %w", err))
	}
	if _, err := fm.Session.ContinueMS(); err != nil {
		return parseFail(filePath, data, hash, fmt.Errorf("session: %w", err))
	}
	folder, err := NormalizeFolder(fm.Folder)
	if err != nil {
		return parseFail(filePath, data, hash, fmt.Errorf("folder: %w", err))
	}
	if folder != "" && isAgentFile(filePath) {
		return parseFail(filePath, data, hash, errors.New("folder: applies only to a job"))
	}
	if fm.KeepRuns != nil && *fm.KeepRuns < 0 {
		return parseFail(filePath, data, hash, errors.New("keep_runs: must be >= 0"))
	}

	// A name: mismatch with the path-derived name is soft (migration reports
	// it); parse keeps the path-derived name for jobs.
	name := deriveName(filePath)

	route, err := notify.ParseSteps(&fm.Notify)
	if err != nil {
		return parseFail(filePath, data, hash, err)
	}
	contact, err := notify.ParseContact(&fm.Contact)
	if err != nil {
		return parseFail(filePath, data, hash, err)
	}
	artifacts, err := parseArtifacts(fm.Artifacts)
	if err != nil {
		return parseFail(filePath, data, hash, fmt.Errorf("artifacts: %w", err))
	}
	j := &Job{
		Name:           name,
		FilePath:       filePath,
		Dir:            filepath.Dir(filePath),
		WorkingDir:     fm.WorkingDir,
		EnvFile:        fm.EnvFile,
		Harness:        fm.Harness.Name,
		Model:          fm.Harness.Model,
		Effort:         fm.Harness.Effort,
		Runtime:        fm.Runtime,
		Title:          fm.Title,
		Memory:         memCfg.Enabled,
		MemoryOpts:     memCfg,
		State:          collections,
		Envset:         fm.Envset,
		Env:            fm.Env,
		Session:        fm.Session,
		Triggers:       triggers,
		Inputs:         fm.Inputs,
		Disabled:       fm.Disabled,
		FollowupPrompt: fm.FollowupPrompt,
		Skills:         stringList(fm.Skills),
		Connectors:     stringList(fm.Connectors),
		Prompt:         string(bytes.TrimSpace(body)),
		Kind:           kindFromPath(filePath),
		EmailAccept:    rawAccept,
		EmailStyle:     style,
		AcceptSet:      acceptSet,
		Accept:         accept,
		Kit:            fm.Kit,
		LegacyKitMCP:   legacyKitMCP(data),
		Budget:         fm.Budget,
		Report:         fm.Report,
		Team:           fm.Team,
		Artifacts:      artifacts,
		KeepRuns:       fm.KeepRuns,
		Hash:           hash,
		Description:    fm.Description,
		Folder:         folder,
		Notify:         route,
		NotifySet:      route != nil,
		Contact:        contact,
	}
	j.setID(fm.ID)
	return j, nil
}

func (j *Job) setID(id string) {
	switch {
	case id == "":
		j.IDState = IDMissing
	case ValidID(j.Kind, id):
		j.ID, j.IDState = id, IDOK
	default:
		j.IDState = IDInvalid
	}
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
	if _, exists := raw["allowlist"]; exists {
		return fmt.Errorf(`job key "allowlist" is removed; set "allowlist" on the event trigger`)
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
		if err := t.removedKey(); err != nil {
			return nil, err
		}
		if t.Event == "" {
			switch {
			case t.Limit != "":
				return nil, fmt.Errorf(`"limit" applies only to an event trigger`)
			case len(t.Rooms) > 0:
				return nil, fmt.Errorf(`"rooms" applies only to an event trigger`)
			case len(t.Allowlist) > 0:
				return nil, fmt.Errorf(`"allowlist" applies only to an event trigger`)
			case len(t.Contains) > 0:
				return nil, fmt.Errorf(`"contains" applies only to an event trigger`)
			case t.From != "":
				return nil, fmt.Errorf(`"from" applies only to an event trigger`)
			case t.Replies != "":
				return nil, fmt.Errorf(`"replies" applies only to an event trigger`)
			case t.Batch != "":
				return nil, fmt.Errorf(`"batch" applies only to an event trigger`)
			}
		}
		switch t.From {
		case "", FromPeople, FromBots, FromAnyone:
		default:
			return nil, fmt.Errorf(`from %q: use %s, %s, or %s`, t.From, FromPeople, FromBots, FromAnyone)
		}
		switch t.Replies {
		case "", RepliesContinue, RepliesOff:
		default:
			return nil, fmt.Errorf(`replies %q: use %s or %s`, t.Replies, RepliesContinue, RepliesOff)
		}
		tr := Trigger{
			Raw: t, When: t.When, Match: t.Match, Extract: t.Extract,
			Allowlist: uniqueEntries(t.Allowlist), Rooms: uniqueEntries(t.Rooms),
			Contains: uniqueEntries(t.Contains), From: t.From, Replies: t.Replies,
			Limit: t.Limit, Batch: t.Batch, Timezone: t.Timezone, Disabled: t.Disabled,
			Types: uniqueEntries(t.Types), Manual: t.Manual, Inputs: t.Inputs,
		}
		if t.Event != "" {
			rate, err := parseLimit(t.Limit)
			if err != nil {
				return nil, err
			}
			tr.EffectiveLimit = rate.String()
			if tr.BatchMS, err = ParseBatch(t.Batch); err != nil {
				return nil, err
			}
			if tr.BatchMS > 0 && t.Replies == RepliesContinue {
				return nil, errors.New(`"batch" cannot be used with "replies: continue"; a batch has no single conversation`)
			}
		}
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
	case "agent.md":
		return dir // discover rewrites to agents/<name>
	default:
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
}

func kindFromPath(path string) string {
	switch strings.ToLower(filepath.Base(path)) {
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
		case "max_logs", "inject_logs", "max_bytes":
			n, err := memoryInt(k, v)
			if err != nil {
				return MemoryConfig{}, err
			}
			switch k {
			case "max_logs":
				cfg.MaxLogs = &n
			case "inject_logs":
				cfg.InjectLogs = &n
			default:
				if n < 0 {
					return MemoryConfig{}, fmt.Errorf("max_bytes: must be >= 0, got %d", n)
				}
				cfg.MaxBytes = &n
			}
		case "require_update":
			b, ok := v.(bool)
			if !ok {
				return MemoryConfig{}, fmt.Errorf("require_update: expected bool, got %T", v)
			}
			cfg.RequireUpdate = &b
		default:
			return MemoryConfig{}, fmt.Errorf("unknown key %q (known: max_logs, require_update, inject_logs, max_bytes)", k)
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

// stringList normalizes YAML scalar-or-list fields used by legacy frontmatter.
func stringList(raw any) []string {
	switch value := raw.(type) {
	case []string:
		return value
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			switch v := item.(type) {
			case string:
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					out = append(out, trimmed)
				}
			case fmt.Stringer:
				if trimmed := strings.TrimSpace(v.String()); trimmed != "" {
					out = append(out, trimmed)
				}
			default:
				if text := strings.TrimSpace(fmt.Sprint(v)); text != "" && text != "<nil>" {
					out = append(out, text)
				}
			}
		}
		return out
	case string:
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return []string{trimmed}
		}
	}
	return nil
}

// parseLimit resolves a limit value: empty is DefaultLimit, off is nil.
func parseLimit(v string) (*dispatch.RateLimit, error) {
	v = strings.TrimSpace(v)
	switch v {
	case "":
		v = DefaultLimit
	case LimitOff:
		return nil, nil
	}
	rate := dispatch.ParseRateLimit(v)
	if rate == nil {
		return nil, fmt.Errorf(`limit %q: use N/<duration> such as 15/10m, or "off"`, v)
	}
	return rate, nil
}

// uniqueEntries drops empty and duplicate entries and keeps the order.
func uniqueEntries(in []string) []string {
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func fileHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// NormalizeFolder trims a folder label and checks the one-level rule.
func NormalizeFolder(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", nil
	case strings.ContainsAny(v, `/\`):
		return "", fmt.Errorf("folder %q: use one folder name; nested folders are not supported", v)
	case utf8.RuneCountInString(v) > 64:
		return "", fmt.Errorf("folder %q: use at most 64 characters", v)
	case strings.IndexFunc(v, unicode.IsControl) >= 0:
		return "", fmt.Errorf("folder %q: remove control characters", v)
	}
	return v, nil
}

func parseFail(path string, data []byte, hash string, err error) (*Job, error) {
	j := &Job{
		Name:       deriveName(path),
		FilePath:   path,
		Dir:        filepath.Dir(path),
		ParseError: err.Error(),
		Hash:       hash,
		Kind:       kindFromPath(path),
	}
	j.setID(scanID(data))
	return j, err
}
