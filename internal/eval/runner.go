package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const preflightTimeout = 90 * time.Second

const (
	preflightPromptMCP = "call the system tool with action health, then reply DONE"
	preflightPromptCLI = "trebi doctor --json"
)

// Options is process wiring for the eval runner.
type Options struct {
	EvalsDir  string
	OutDir    string
	TrebiBin  string
	SkillPath string
	Clients   []string
	Judge     string // claude|codex, optionally name:model
	TaskID    string
	Tier      string
	Trials    int
	BudgetUSD float64
	Baseline  string
	KeepHome  bool
	Surface   string // cli (default), mcp, or both
}

// Runner owns suite execution.
type Runner struct {
	opts            Options
	skillMD         []byte
	judge           *judgeRunner
	newClient       func(string) (Client, error)
	scoreAll        func(context.Context, []JudgeDim, []byte, []byte) []JudgeResult
	preflightLaunch func(context.Context, string, string) (*LaunchResult, string, error)
}

// NewRunner loads skill + rubrics. Missing files fail fast.
func NewRunner(opts Options) (*Runner, error) {
	if opts.EvalsDir == "" {
		return nil, fmt.Errorf("evals dir required")
	}
	if opts.OutDir == "" {
		opts.OutDir = "out"
	}
	bin, err := resolveTrebi(opts.TrebiBin)
	if err != nil {
		return nil, err
	}
	opts.TrebiBin = bin
	skill, err := os.ReadFile(opts.SkillPath)
	if err != nil {
		return nil, fmt.Errorf("skill: %w", err)
	}
	j, err := newJudge(opts.EvalsDir, opts.Judge)
	if err != nil {
		return nil, err
	}
	return &Runner{opts: opts, skillMD: skill, judge: j}, nil
}

// Run executes the selected tasks and writes the report.
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	tasks, err := LoadTasks(r.opts.EvalsDir, r.opts.TaskID, r.opts.Tier)
	if err != nil {
		return nil, err
	}
	tasks, err = filterSurface(tasks, r.opts.Surface)
	if err != nil {
		return nil, err
	}
	if err := r.preflightJudge(ctx, tasks); err != nil {
		return nil, err
	}
	if err := r.runClientPreflight(ctx, tasks); err != nil {
		return nil, err
	}
	rep := newReport()
	outRoot := filepath.Join(r.opts.OutDir, rep.ID)
	if err := os.MkdirAll(outRoot, 0o755); err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if err := ctx.Err(); err != nil {
			rep.Aborted = err.Error()
			break
		}
		if r.overBudget(rep) {
			rep.Aborted = fmt.Sprintf("budget cap %.4f", r.opts.BudgetUSD)
			break
		}
		r.runTask(ctx, t, rep, outRoot)
	}
	if err := ctx.Err(); err != nil && rep.Aborted == "" {
		rep.Aborted = err.Error()
	}
	rep.finish()
	if err := rep.write(outRoot); err != nil {
		return rep, err
	}
	if drops := r.baselineDrops(rep); len(drops) > 0 {
		return rep, fmt.Errorf("regression:\n  %s", strings.Join(drops, "\n  "))
	}
	return rep, nil
}

// ReplayOracle runs the reference script and grades checks only.
func (r *Runner) ReplayOracle(ctx context.Context, taskID string) (*TrialResult, error) {
	tasks, err := LoadTasks(r.opts.EvalsDir, taskID, "")
	if err != nil {
		return nil, err
	}
	t := tasks[0]
	if t.OraclePath() == "" {
		return nil, fmt.Errorf("task %s has no oracle", t.ID)
	}
	rep := newReport()
	art := filepath.Join(r.opts.OutDir, rep.ID, t.ID, "oracle", "0")
	tr := r.runOne(ctx, t, "oracle", 0, art, true, surfaceCLI)
	if !tr.Passed() {
		return tr, fmt.Errorf("oracle %s: %s", t.ID, tr.Error)
	}
	return tr, nil
}

func (r *Runner) clientFor(name string) (Client, error) {
	if r.newClient != nil {
		return r.newClient(name)
	}
	return newClient(name)
}

func (r *Runner) overBudget(rep *Report) bool {
	if r.opts.BudgetUSD <= 0 {
		return false
	}
	return rep.CostUSD+rep.JudgeUSD >= r.opts.BudgetUSD
}

func (r *Runner) scoreFor(ctx context.Context, dims []JudgeDim, transcript, snapshot []byte, surface string) []JudgeResult {
	if r.scoreAll != nil {
		return r.scoreAll(ctx, dims, transcript, snapshot)
	}
	return r.judge.scoreAll(ctx, dims, transcript, snapshot, surface)
}

func (r *Runner) runTask(ctx context.Context, t *Task, rep *Report, outRoot string) {
	clients := t.Clients
	if len(r.opts.Clients) > 0 {
		clients = r.opts.Clients
	}
	trials := t.Trials
	if r.opts.Trials > 0 {
		trials = r.opts.Trials
	}
	for _, surf := range t.runSurfaces(r.opts.Surface) {
		for _, name := range clients {
			for i := 0; i < trials; i++ {
				if err := ctx.Err(); err != nil {
					if rep.Aborted == "" {
						rep.Aborted = err.Error()
					}
					return
				}
				if r.overBudget(rep) {
					if rep.Aborted == "" {
						rep.Aborted = fmt.Sprintf("budget cap %.4f", r.opts.BudgetUSD)
					}
					return
				}
				art := filepath.Join(outRoot, t.ID, name, fmt.Sprintf("%d", i))
				if surf == surfaceMCP {
					art = filepath.Join(outRoot, t.ID, "mcp", name, fmt.Sprintf("%d", i))
				}
				tr := r.runOne(ctx, t, name, i, art, false, surf)
				if tr.Outcome == OutcomeInfraFailure {
					tr = r.runOne(ctx, t, name, i, art+"-retry", false, surf)
				}
				rep.add(*tr)
				if r.judge != nil {
					rep.JudgeUSD = r.judge.spendUSD()
				}
				fmt.Fprintf(os.Stderr, "eval %s/%s/%s#%d %s %.1fs\n", tr.TaskID, tr.Surface, tr.Client, tr.Index, tr.Outcome, tr.DurationS)
				_ = rep.checkpoint(outRoot)
			}
		}
	}
}

func (r *Runner) runOne(ctx context.Context, t *Task, clientName string, idx int, art string, oracle bool, surface string) *TrialResult {
	tr := &TrialResult{TaskID: t.ID, Client: clientName, Index: idx, Dir: art, Outcome: OutcomeFail, Surface: surface}
	if err := os.MkdirAll(art, 0o755); err != nil {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	home := filepath.Join(art, "home")
	workdir := filepath.Join(art, "work")
	env := newTrialEnv(home, workdir, art, r.opts.TrebiBin)
	start := time.Now()
	defer func() {
		tr.DurationS = time.Since(start).Seconds()
		env.stop(context.Background())
		_ = tr.writeFile()
	}()
	dctx, cancel := context.WithTimeout(ctx, time.Duration(t.TimeoutS)*time.Second+judgeBudget(len(t.Judge.Dimensions))+time.Minute)
	defer cancel()
	if err := env.startDaemon(dctx, t.ServerHarness, env.workdir); err != nil {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	if err := env.configure(dctx, t.ServerHarness); err != nil {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	p := env.placeholders(workdir)
	if err := env.seed(dctx, t.SeedPath(), p); err != nil {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	p.Workspace = env.workspacePath(dctx)
	if err := env.preflight(dctx, t.ServerHarness); err != nil {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	if err := env.snapshotHome(); err != nil {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	g := newGrader(env, p)
	return r.runAfterSeed(ctx, dctx, t, env, g, p, tr, clientName, surface, oracle)
}

func (r *Runner) runAfterSeed(ctx, dctx context.Context, t *Task, env *trialEnv, g *Grader, p Placeholders, tr *TrialResult, clientName, surface string, oracle bool) *TrialResult {
	if oracle {
		return r.finishOracle(dctx, t, env, g, p, tr)
	}
	if guardSeedChecks(dctx, t, g, tr) {
		return tr
	}
	return r.finishClient(ctx, dctx, t, env, g, p, tr, clientName, surface)
}

func (r *Runner) finishOracle(ctx context.Context, t *Task, env *trialEnv, g *Grader, p Placeholders, tr *TrialResult) *TrialResult {
	script, err := oracleScript(t.OraclePath())
	if err != nil {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", "set -euo pipefail\n"+p.Expand(script))
	cmd.Env = append(env.environ(),
		"TREBI_BIN="+env.bin,
		"EVAL_WORKDIR="+env.workdir,
		"EVAL_HOME="+env.home,
		"EVAL_SEEDS="+filepath.Join(r.opts.EvalsDir, "seeds"),
	)
	cmd.Dir = env.workdir
	if out, err := cmd.CombinedOutput(); err != nil {
		tr.Error = fmt.Sprintf("oracle: %v\n%s", err, out)
		return tr
	}
	p.Workspace = env.workspacePath(ctx)
	g.p = p
	if err := os.WriteFile(p.Transcript, []byte("[]\n"), 0o644); err != nil {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	_ = g.collectSnapshot(ctx)
	tr.Checks = g.runAll(ctx, skipOracleChecks(checksFor(t.Checks, surfaceCLI)))
	tr.Outcome = checksOutcome(tr.Checks)
	return tr
}

func (r *Runner) finishClient(ctx, dctx context.Context, t *Task, env *trialEnv, g *Grader, p Placeholders, tr *TrialResult, clientName, surface string) *TrialResult {
	cl, err := r.clientFor(clientName)
	if err != nil {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	opts, err := r.launchOpts(env, p, t, clientName, surface)
	if err != nil {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	tctx, cancel := context.WithTimeout(ctx, time.Duration(t.TimeoutS)*time.Second)
	launch, err := cl.Launch(tctx, opts)
	cancel()
	if launch != nil {
		tr.CostUSD = launch.CostUSD
	}
	if surface == surfaceMCP {
		tr.ClientSkills = clientSkillFingerprint(opts.ConfigHome)
	}
	if err != nil && (launch == nil || len(launch.Raw) == 0) {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	if surface == surfaceMCP && launch != nil {
		if viol := mcpIsolationViolations(launch.Steps, env.apiURL); len(viol) > 0 {
			tr.Outcome = OutcomeFail
			tr.Error = "mcp isolation: " + strings.Join(viol, "; ")
			return tr
		}
	}
	if deadClientGate(tr, launch) {
		return tr
	}
	return r.gradeLaunch(ctx, dctx, t, env, g, p, tr)
}

func (r *Runner) launchOpts(env *trialEnv, p Placeholders, t *Task, clientName, surface string) (LaunchOpts, error) {
	opts := LaunchOpts{
		Workdir:    env.workdir,
		Prompt:     t.Prompt,
		Env:        env.environ(),
		Timeout:    time.Duration(t.TimeoutS) * time.Second,
		SkillMD:    r.skillMD,
		Transcript: p.Transcript,
		Surface:    surface,
	}
	if surface != surfaceMCP {
		return opts, nil
	}
	cfgHome := filepath.Join(env.artDir, "client-home")
	if err := os.MkdirAll(cfgHome, 0o755); err != nil {
		return opts, err
	}
	if err := seedClientHome(cfgHome, clientName); err != nil {
		return opts, err
	}
	opts.MCPURL = strings.TrimRight(env.apiURL, "/") + "/mcp"
	opts.MCPToken = env.token
	opts.ConfigHome = cfgHome
	opts.Env = env.mcpEnviron(cfgHome)
	return opts, nil
}

func (r *Runner) gradeLaunch(ctx, dctx context.Context, t *Task, env *trialEnv, g *Grader, p Placeholders, tr *TrialResult) *TrialResult {
	p.Workspace = env.workspacePath(dctx)
	g.p = p
	_ = g.collectSnapshot(dctx)
	tr.Checks = g.runAll(dctx, checksFor(t.Checks, tr.Surface))
	jctx, jcancel := context.WithTimeout(ctx, judgeBudget(len(t.Judge.Dimensions)))
	defer jcancel()
	tr.Judge = r.scoreFor(jctx, t.Judge.Dimensions, readFile(p.Transcript), readFile(p.Snapshot), tr.Surface)
	tr.Outcome = trialOutcome(tr.Checks, tr.Judge)
	if tr.Outcome != OutcomePass {
		// A timeout note is already here; keep it in front of the detail.
		tr.Error = strings.TrimPrefix(tr.Error+"; "+failSummary(tr), "; ")
	}
	return tr
}

func (r *Runner) preflightJudge(ctx context.Context, tasks []*Task) error {
	if r.scoreAll != nil {
		return nil
	}
	for _, t := range tasks {
		if len(t.Judge.Dimensions) == 0 {
			continue
		}
		return r.judge.preflight(ctx)
	}
	return nil
}

func (r *Runner) runClientPreflight(ctx context.Context, tasks []*Task) error {
	launchFn := r.preflightLaunch
	if launchFn == nil {
		launchFn = r.livePreflightLaunch
	}
	for _, pair := range preflightCombos(r.opts, tasks) {
		client, surface := pair[0], pair[1]
		launch, cfgPath, err := launchFn(ctx, client, surface)
		if err != nil && launch == nil {
			return fmt.Errorf("preflight: %s/%s: %w", client, surface, err)
		}
		if err := preflightVerdict(client, surface, launch, cfgPath); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "preflight: %s/%s ok\n", client, surface)
	}
	return nil
}

func (r *Runner) livePreflightLaunch(ctx context.Context, clientName, surface string) (*LaunchResult, string, error) {
	art, err := os.MkdirTemp("", "trebi-eval-preflight-*")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(art)
	env := newTrialEnv(filepath.Join(art, "home"), filepath.Join(art, "work"), art, r.opts.TrebiBin)
	defer env.stop(context.Background())
	dctx, cancel := context.WithTimeout(ctx, preflightTimeout+time.Minute)
	defer cancel()
	if err := env.startDaemon(dctx, defaultHarness, env.workdir); err != nil {
		return nil, "", err
	}
	if err := env.configure(dctx, defaultHarness); err != nil {
		return nil, "", err
	}
	return r.launchPreflightClient(ctx, env, clientName, surface)
}

func (r *Runner) launchPreflightClient(ctx context.Context, env *trialEnv, clientName, surface string) (*LaunchResult, string, error) {
	cl, err := r.clientFor(clientName)
	if err != nil {
		return nil, "", err
	}
	prompt := preflightPromptCLI
	if surface == surfaceMCP {
		prompt = preflightPromptMCP
	}
	t := &Task{Prompt: prompt, TimeoutS: int(preflightTimeout / time.Second)}
	p := env.placeholders(env.workdir)
	p.Transcript = filepath.Join(env.artDir, "transcript.json")
	opts, err := r.launchOpts(env, p, t, clientName, surface)
	if err != nil {
		return nil, "", err
	}
	cfgPath := ""
	if surface == surfaceMCP {
		cfgPath = filepath.Join(opts.ConfigHome, "config.toml")
		name, _, _ := parseClientSpec(clientName)
		if name == clientClaude {
			cfgPath = filepath.Join(opts.ConfigHome, "mcp.json")
		}
	}
	tctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	launch, err := cl.Launch(tctx, opts)
	return launch, cfgPath, err
}

func (r *Runner) baselineDrops(rep *Report) []string {
	if r.opts.Baseline == "" {
		r.opts.Baseline = filepath.Join(r.opts.EvalsDir, "baseline.json")
	}
	b, err := loadBaseline(r.opts.Baseline)
	if err != nil || b == nil {
		return nil
	}
	return rep.regress(b)
}

func skipOracleChecks(checks []Check) []Check {
	out := make([]Check, 0, len(checks))
	for _, c := range checks {
		if c.CommandsJQ != "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

func filterSurface(tasks []*Task, surface string) ([]*Task, error) {
	if surface != surfaceMCP {
		return tasks, nil
	}
	out := make([]*Task, 0, len(tasks))
	for _, t := range tasks {
		if t.hasSurface(surfaceMCP) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no tasks support surface mcp")
	}
	return out, nil
}

func guardSeedChecks(ctx context.Context, t *Task, g *Grader, tr *TrialResult) bool {
	pre := skipOracleChecks(checksFor(t.Checks, tr.Surface))
	if len(pre) == 0 {
		return false
	}
	for _, c := range g.runAll(ctx, pre) {
		if !c.Pass {
			return false
		}
	}
	tr.Outcome = OutcomeInfraFailure
	tr.Error = "checks green on seed state"
	return true
}

// deadClientGate stops a trial whose client never produced an answer. A client
// that made tool calls but ran out of time is graded normally: its work is on
// the daemon and a retry would burn the same wall. A client that reported a
// fault instead is infra, and retries: it was cut off, not slow.
func deadClientGate(tr *TrialResult, launch *LaunchResult) bool {
	if launch != nil && hasAssistantStep(launch.Steps) {
		return false
	}
	if launch != nil && toolStepCount(launch.Steps) > 0 && len(launch.Errors) == 0 {
		tr.Error = clientErrorSummary(launch)
		return false
	}
	tr.Outcome = OutcomeInfraFailure
	tr.Error = clientErrorSummary(launch)
	return true
}

func preflightVerdict(client, surface string, launch *LaunchResult, cfgPath string) error {
	if launch == nil || !hasAssistantStep(launch.Steps) {
		return fmt.Errorf("preflight: %s/%s cannot reach its model: %s", client, surface, clientErrorSummary(launch))
	}
	if surface == surfaceMCP && !mcpHandshakeOK(launch.Steps) {
		if cfgPath == "" {
			cfgPath = "(unknown config path)"
		}
		return fmt.Errorf("preflight: %s/%s MCP handshake failed (no mcp__trebi* tool call); config %s", client, surface, cfgPath)
	}
	return nil
}

func preflightCombos(opts Options, tasks []*Task) [][2]string {
	clients := opts.Clients
	if len(clients) == 0 {
		seen := map[string]bool{}
		for _, t := range tasks {
			for _, c := range t.Clients {
				if seen[c] {
					continue
				}
				seen[c] = true
				clients = append(clients, c)
			}
		}
	}
	surfaces := []string{surfaceCLI}
	switch opts.Surface {
	case surfaceMCP:
		surfaces = []string{surfaceMCP}
	case "both":
		surfaces = []string{surfaceCLI, surfaceMCP}
	}
	var out [][2]string
	for _, s := range surfaces {
		for _, c := range clients {
			out = append(out, [2]string{c, s})
		}
	}
	return out
}

func checksOutcome(checks []CheckResult) Outcome {
	if len(checks) == 0 {
		return OutcomeFail
	}
	for _, c := range checks {
		if c.Infra {
			return OutcomeInfraFailure
		}
		if !c.Pass {
			return OutcomeFail
		}
	}
	return OutcomePass
}

func trialOutcome(checks []CheckResult, judge []JudgeResult) Outcome {
	o := checksOutcome(checks)
	if o != OutcomePass {
		return o
	}
	n, nInfra := 0, 0
	for _, j := range judge {
		if j.Name == "efficiency" {
			continue
		}
		n++
		if j.Infra {
			nInfra++
			continue
		}
		if !j.Pass && !j.Unknown {
			return OutcomeFail
		}
	}
	if n > 0 && nInfra == n {
		return OutcomeInfraFailure
	}
	return OutcomePass
}

func failSummary(tr *TrialResult) string {
	var parts []string
	for _, c := range tr.Checks {
		if !c.Pass {
			parts = append(parts, "check "+c.Name+": "+c.Detail)
		}
	}
	for _, j := range tr.Judge {
		if j.Infra {
			parts = append(parts, "judge "+j.Name+" infra")
			continue
		}
		if !j.Pass && !j.Unknown {
			parts = append(parts, "judge "+j.Name)
		}
	}
	return strings.Join(parts, "; ")
}

func oracleScript(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := string(data)
	var blocks []string
	rest := s
	for {
		i := strings.Index(rest, "```bash")
		if i < 0 {
			break
		}
		rest = rest[i+len("```bash"):]
		if len(rest) > 0 && rest[0] == '\n' {
			rest = rest[1:]
		}
		j := strings.Index(rest, "```")
		if j < 0 {
			blocks = append(blocks, rest)
			break
		}
		blocks = append(blocks, rest[:j])
		rest = rest[j+3:]
	}
	if len(blocks) == 0 {
		return s, nil
	}
	return strings.Join(blocks, "\n"), nil
}

func resolveTrebi(bin string) (string, error) {
	if bin != "" {
		return filepath.Abs(bin)
	}
	if env := os.Getenv("TREBI_BIN"); env != "" {
		return filepath.Abs(env)
	}
	p, err := exec.LookPath("trebi")
	if err != nil {
		return "", fmt.Errorf("trebi binary not found (set TREBI_BIN)")
	}
	return p, nil
}

func readFile(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}
