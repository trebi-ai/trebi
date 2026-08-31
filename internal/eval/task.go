package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tier is the eval catalog grouping.
type Tier string

const (
	TierOnboarding Tier = "onboarding"
	TierOperate    Tier = "operate"
	TierDebug      Tier = "debug"
	TierGuardrail  Tier = "guardrail"
)

const (
	clientClaude      = "claude"
	clientCodex       = "codex"
	defaultHarness    = "codex"
	defaultTrials     = 3
	defaultTimeout    = 600
	defaultJudgeModel = "claude-sonnet-5"
)

// Check is one deterministic outcome grader.
type Check struct {
	Name            string `yaml:"name"`
	Cmd             string `yaml:"cmd"`
	JQ              string `yaml:"jq"`
	Exit            *int   `yaml:"exit"`
	FileExists      string `yaml:"file_exists"`
	FileAbsent      string `yaml:"file_absent"`
	FileContains    string `yaml:"file_contains"`
	FileNotContains string `yaml:"file_not_contains"`
	Text            string `yaml:"text"`
	SnapshotJQ      string `yaml:"snapshot_jq"`
	StdoutContains  string `yaml:"stdout_contains"`
	StdoutAbsent    string `yaml:"stdout_absent"`
	HomeUntouched   bool   `yaml:"home_untouched"`
	CommandsJQ      string `yaml:"commands_jq"`
	FileMatches     string `yaml:"file_matches"` // path; pattern goes in Text
	// Surfaces limits a check to some surfaces. Empty means every surface.
	Surfaces []string `yaml:"surfaces"`
}

// appliesTo reports whether this check runs on the given surface.
func (c Check) appliesTo(surface string) bool {
	if len(c.Surfaces) == 0 {
		return true
	}
	if surface == "" {
		surface = surfaceCLI
	}
	return slices.Contains(c.Surfaces, surface)
}

// JudgeDim is one isolated judge call.
type JudgeDim struct {
	Use    string   `yaml:"use"`
	Name   string   `yaml:"name"`
	Min    *float64 `yaml:"min"`
	Rubric string   `yaml:"rubric"`
}

// JudgeSpec pins the model and lists dimensions.
type JudgeSpec struct {
	Model      string     `yaml:"model"`
	Dimensions []JudgeDim `yaml:"dimensions"`
}

// Task is one YAML eval scenario.
type Task struct {
	ID            string    `yaml:"id"`
	Title         string    `yaml:"title"`
	Tier          Tier      `yaml:"tier"`
	Clients       []string  `yaml:"clients"`
	Trials        int       `yaml:"trials"`
	TimeoutS      int       `yaml:"timeout_s"`
	ServerHarness string    `yaml:"server_harness"`
	Seed          string    `yaml:"seed"`
	Prompt        string    `yaml:"prompt"`
	Checks        []Check   `yaml:"checks"`
	Judge         JudgeSpec `yaml:"judge"`
	Oracle        string    `yaml:"oracle"`
	Surfaces      []string  `yaml:"surfaces"`

	path     string
	evalsDir string
}

const (
	surfaceCLI = "cli"
	surfaceMCP = "mcp"
)

// Placeholders are expanded in checks, seeds, and file paths.
type Placeholders struct {
	Home       string
	Workspace  string
	APIURL     string
	Workdir    string
	Transcript string
	Snapshot   string
}

// LoadTask reads and validates one task YAML file.
func LoadTask(path, evalsDir string) (*Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read task %s: %w", path, err)
	}
	var t Task
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse task %s: %w", path, err)
	}
	t.path = path
	t.evalsDir = evalsDir
	t.applyDefaults()
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("task %s: %w", t.ID, err)
	}
	return &t, nil
}

// LoadTasks walks evals/tasks for YAML files, optionally filtered by id or tier.
func LoadTasks(evalsDir, id, tier string) ([]*Task, error) {
	root := filepath.Join(evalsDir, "tasks")
	var out []*Task
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		t, err := LoadTask(path, evalsDir)
		if err != nil {
			return err
		}
		if id != "" && t.ID != id {
			return nil
		}
		if tier != "" && string(t.Tier) != tier {
			return nil
		}
		out = append(out, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no tasks in %s (id=%q tier=%q)", root, id, tier)
	}
	return out, nil
}

// Validate reports schema problems.
func (t *Task) Validate() error {
	if t.ID == "" {
		return fmt.Errorf("id required")
	}
	if t.Prompt == "" {
		return fmt.Errorf("prompt required")
	}
	if err := t.validateTier(); err != nil {
		return err
	}
	if err := t.validateClients(); err != nil {
		return err
	}
	if err := t.validateSurfaces(); err != nil {
		return err
	}
	if len(t.Checks) == 0 {
		return fmt.Errorf("at least one check required")
	}
	for i, c := range t.Checks {
		if err := c.validate(); err != nil {
			return fmt.Errorf("checks[%d] %s: %w", i, c.Name, err)
		}
		for _, s := range c.Surfaces {
			if s != surfaceCLI && s != surfaceMCP {
				return fmt.Errorf("checks[%d] %s: surface %q: want cli|mcp", i, c.Name, s)
			}
		}
	}
	return nil
}

// SeedPath is the absolute seed script path, or empty.
func (t *Task) SeedPath() string {
	if t.Seed == "" {
		return ""
	}
	if filepath.IsAbs(t.Seed) {
		return t.Seed
	}
	return filepath.Join(t.evalsDir, t.Seed)
}

// OraclePath is the absolute oracle markdown path, or empty.
func (t *Task) OraclePath() string {
	if t.Oracle == "" {
		return ""
	}
	if filepath.IsAbs(t.Oracle) {
		return t.Oracle
	}
	return filepath.Join(t.evalsDir, t.Oracle)
}

func (t *Task) applyDefaults() {
	if t.Trials <= 0 {
		t.Trials = defaultTrials
	}
	if t.TimeoutS <= 0 {
		t.TimeoutS = defaultTimeout
	}
	if t.ServerHarness == "" {
		t.ServerHarness = defaultHarness
	}
	if len(t.Clients) == 0 {
		t.Clients = []string{clientClaude, clientCodex}
	}
	if t.Judge.Model == "" {
		t.Judge.Model = defaultJudgeModel
	}
	if len(t.Surfaces) == 0 {
		t.Surfaces = []string{surfaceCLI}
	}
}

func (t *Task) validateTier() error {
	switch t.Tier {
	case TierOnboarding, TierOperate, TierDebug, TierGuardrail:
		return nil
	case "":
		return fmt.Errorf("tier required")
	default:
		return fmt.Errorf("tier %q: want onboarding|operate|debug|guardrail", t.Tier)
	}
}

func (t *Task) validateSurfaces() error {
	for _, s := range t.Surfaces {
		if s != surfaceCLI && s != surfaceMCP {
			return fmt.Errorf("surface %q: want cli|mcp", s)
		}
	}
	return nil
}

func (t *Task) hasSurface(s string) bool {
	for _, x := range t.Surfaces {
		if x == s {
			return true
		}
	}
	return false
}

func (t *Task) runSurfaces(want string) []string {
	switch want {
	case surfaceMCP:
		if t.hasSurface(surfaceMCP) {
			return []string{surfaceMCP}
		}
		return nil
	case "both":
		return append([]string(nil), t.Surfaces...)
	default:
		if t.hasSurface(surfaceCLI) {
			return []string{surfaceCLI}
		}
		return nil
	}
}

func (t *Task) validateClients() error {
	for _, c := range t.Clients {
		if _, _, err := parseClientSpec(c); err != nil {
			return fmt.Errorf("client %q: want claude|codex or name:model", c)
		}
	}
	return nil
}

// kinds names every check field that selects a mode. Exactly one may be set.
func (c Check) kinds() []string {
	set := map[string]bool{
		"cmd":               c.Cmd != "",
		"file_exists":       c.FileExists != "",
		"file_absent":       c.FileAbsent != "",
		"file_contains":     c.FileContains != "",
		"file_not_contains": c.FileNotContains != "",
		"file_matches":      c.FileMatches != "",
		"snapshot_jq":       c.SnapshotJQ != "",
		"home_untouched":    c.HomeUntouched,
		"commands_jq":       c.CommandsJQ != "",
	}
	names := make([]string, 0, len(set))
	for k, on := range set {
		if on {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names
}

func (c Check) validate() error {
	if c.Name == "" {
		return fmt.Errorf("name required")
	}
	if got := c.kinds(); len(got) != 1 {
		return fmt.Errorf("need exactly one check mode, got %d %v", len(got), got)
	}
	if (c.FileContains != "" || c.FileNotContains != "" || c.FileMatches != "") && c.Text == "" {
		return fmt.Errorf("text required with file_contains/file_not_contains/file_matches")
	}
	if c.FileMatches != "" {
		if _, err := regexp.Compile(c.Text); err != nil {
			return fmt.Errorf("file_matches pattern: %w", err)
		}
	}
	return nil
}

// Expand replaces {home} {workspace} {api_url} {workdir} {transcript} {snapshot}.
func (p Placeholders) Expand(s string) string {
	repl := []string{
		"{home}", p.Home,
		"{workspace}", p.Workspace,
		"{api_url}", p.APIURL,
		"{workdir}", p.Workdir,
		"{transcript}", p.Transcript,
		"{snapshot}", p.Snapshot,
	}
	return strings.NewReplacer(repl...).Replace(s)
}

// checksFor keeps the checks that apply to one surface.
func checksFor(checks []Check, surface string) []Check {
	out := make([]Check, 0, len(checks))
	for _, c := range checks {
		if c.appliesTo(surface) {
			out = append(out, c)
		}
	}
	return out
}
