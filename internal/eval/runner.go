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

// Options is process wiring for the eval runner.
type Options struct {
	EvalsDir  string
	OutDir    string
	TrebiBin  string
	SkillPath string
	Clients   []string
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
	opts    Options
	skillMD []byte
	judge   *judgeRunner
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
	j, err := newJudge(opts.EvalsDir, "")
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
	for _, t := range tasks {
		if len(t.Judge.Dimensions) == 0 {
			continue
		}
		if err := r.judge.preflight(ctx); err != nil {
			return nil, err
		}
		break
	}
	rep := newReport()
	outRoot := filepath.Join(r.opts.OutDir, rep.ID)
	if err := os.MkdirAll(outRoot, 0o755); err != nil {
		return nil, err
	}
	if r.opts.Surface == surfaceMCP {
		filtered := tasks[:0]
		for _, t := range tasks {
			if t.hasSurface(surfaceMCP) {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
		if len(tasks) == 0 {
			return nil, fmt.Errorf("no tasks support surface mcp")
		}
	}
	for _, t := range tasks {
		if err := ctx.Err(); err != nil {
			rep.Aborted = err.Error()
			break
		}
		if r.opts.BudgetUSD > 0 && rep.CostUSD >= r.opts.BudgetUSD {
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
				if r.opts.BudgetUSD > 0 && rep.CostUSD >= r.opts.BudgetUSD {
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
	if oracle {
		return r.finishOracle(dctx, t, env, g, p, tr)
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
	tr.Checks = g.runAll(ctx, skipOracleChecks(t.Checks))
	tr.Outcome = checksOutcome(tr.Checks)
	return tr
}

func (r *Runner) finishClient(ctx, dctx context.Context, t *Task, env *trialEnv, g *Grader, p Placeholders, tr *TrialResult, clientName, surface string) *TrialResult {
	cl, err := newClient(clientName)
	if err != nil {
		tr.Outcome = OutcomeInfraFailure
		tr.Error = err.Error()
		return tr
	}
	opts := LaunchOpts{
		Workdir:    env.workdir,
		Prompt:     t.Prompt,
		Env:        env.environ(),
		Timeout:    time.Duration(t.TimeoutS) * time.Second,
		SkillMD:    r.skillMD,
		Transcript: p.Transcript,
		Surface:    surface,
	}
	if surface == surfaceMCP {
		cfgHome := filepath.Join(env.artDir, "client-home")
		if err := os.MkdirAll(cfgHome, 0o755); err != nil {
			tr.Outcome = OutcomeInfraFailure
			tr.Error = err.Error()
			return tr
		}
		opts.SkillMD = nil
		opts.MCPURL = strings.TrimRight(env.apiURL, "/") + "/mcp"
		opts.MCPToken = env.token
		opts.ConfigHome = cfgHome
		opts.Env = env.mcpEnviron(cfgHome)
	}
	tctx, cancel := context.WithTimeout(ctx, time.Duration(t.TimeoutS)*time.Second)
	launch, err := cl.Launch(tctx, opts)
	cancel()
	if launch != nil {
		tr.CostUSD = launch.CostUSD
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
	p.Workspace = env.workspacePath(dctx)
	g.p = p
	_ = g.collectSnapshot(dctx)
	tr.Checks = g.runAll(dctx, t.Checks)
	jctx, jcancel := context.WithTimeout(ctx, judgeBudget(len(t.Judge.Dimensions)))
	defer jcancel()
	tr.Judge = r.judge.scoreAll(jctx, t.Judge.Dimensions, readFile(p.Transcript), readFile(p.Snapshot))
	tr.Outcome = trialOutcome(tr.Checks, tr.Judge)
	if tr.Outcome != OutcomePass && tr.Error == "" {
		tr.Error = failSummary(tr)
	}
	return tr
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
