package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const judgeDimTimeout = 90 * time.Second

// Rubric is one named judge dimension loaded from rubrics/common.md.
type Rubric struct {
	Name string
	Min  float64
	Text string
}

type judgeRunner struct {
	harness string // claude | codex
	model   string
	rubrics map[string]Rubric

	// invokeFn overrides the harness call. Tests set it; production leaves it
	// nil and invoke dispatches on harness.
	invokeFn func(context.Context, string) ([]byte, error)

	mu      sync.Mutex
	costUSD float64
}

func newJudge(evalsDir, spec string) (*judgeRunner, error) {
	harness, model, err := parseJudgeSpec(spec)
	if err != nil {
		return nil, err
	}
	r := &judgeRunner{harness: harness, model: model, rubrics: map[string]Rubric{}}
	path := filepath.Join(evalsDir, "rubrics", "common.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return r, nil
	}
	r.rubrics = parseRubrics(string(data))
	return r, nil
}

// parseJudgeSpec accepts the same name[:model] shape as --client.
// Empty spec keeps the pinned Claude default.
func parseJudgeSpec(spec string) (name, model string, err error) {
	if spec == "" {
		return clientClaude, defaultJudgeModel, nil
	}
	name, model, err = parseClientSpec(spec)
	if err != nil {
		return "", "", fmt.Errorf("judge %q: want claude|codex or name:model", spec)
	}
	if name == clientClaude && model == "" {
		model = defaultJudgeModel
	}
	return name, model, nil
}

func (j *judgeRunner) label() string {
	h := j.harness
	if h == "" {
		h = clientClaude
	}
	return clientLabel(h, j.model)
}

// resolve fills a dimension from the rubric file. A surface section
// (`## guardrails (mcp)`) wins over the shared one.
func (j *judgeRunner) resolve(d JudgeDim, surface string) (JudgeDim, float64) {
	min := 0.7
	if d.Use != "" {
		r, ok := j.rubricFor(d.Use, surface)
		if ok {
			if d.Name == "" {
				d.Name = d.Use
			}
			if d.Rubric == "" {
				d.Rubric = r.Text
			}
			min = r.Min
		} else if d.Name == "" {
			d.Name = d.Use
		}
	}
	if d.Min != nil {
		min = *d.Min
	}
	return d, min
}

func (j *judgeRunner) rubricFor(use, surface string) (Rubric, bool) {
	if surface != "" && surface != surfaceCLI {
		if r, ok := j.rubrics[use+" ("+surface+")"]; ok {
			return r, true
		}
	}
	r, ok := j.rubrics[use]
	return r, ok
}

// preflight makes one cheap judge call. It fails fast when the judge CLI
// cannot answer (auth expired, quota exhausted), before any trial spends money.
func (j *judgeRunner) preflight(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, judgeDimTimeout)
	defer cancel()
	out, err := j.invoke(ctx, `Reply with JSON only: {"ok":true}`)
	if err != nil {
		return fmt.Errorf("judge preflight (%s): %w", j.label(), err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return fmt.Errorf("judge preflight (%s): empty reply", j.label())
	}
	return nil
}

func judgeBudget(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n)*judgeDimTimeout + 30*time.Second
}

func (j *judgeRunner) scoreAll(ctx context.Context, dims []JudgeDim, transcript, snapshot []byte, surface string) []JudgeResult {
	out := make([]JudgeResult, 0, len(dims))
	for _, d := range dims {
		out = append(out, j.scoreRetry(ctx, d, transcript, snapshot, surface))
	}
	return out
}

// scoreRetry grades one dimension, retrying an invoke failure once. A judge
// timeout or a revoked token would otherwise abstain, and an abstention counts
// as pass: a transient fault must not quietly drop a dimension.
func (j *judgeRunner) scoreRetry(ctx context.Context, d JudgeDim, transcript, snapshot []byte, surface string) JudgeResult {
	var res JudgeResult
	for attempt := range 2 {
		if attempt > 0 && ctx.Err() != nil {
			break
		}
		dctx, cancel := context.WithTimeout(ctx, judgeDimTimeout)
		res = j.score(dctx, d, transcript, snapshot, surface)
		cancel()
		if !res.Infra {
			return res
		}
	}
	return res
}

func (j *judgeRunner) score(ctx context.Context, d JudgeDim, transcript, snapshot []byte, surface string) JudgeResult {
	d, min := j.resolve(d, surface)
	res := JudgeResult{Name: d.Name, Min: min}
	v, err := j.call(ctx, d, min, transcript, snapshot, surface)
	if err != nil {
		res.Unknown = true
		res.Infra = true
		res.Pass = true // invoke failure is infra, not an agent fail
		res.Evidence = err.Error()
		return res
	}
	res.Score = v.Score
	res.Verdict = v.Verdict
	res.Evidence = v.Evidence
	res.Unknown = v.Unknown
	res.Pass = verdictPass(v, min)
	return res
}

func verdictPass(v judgeVerdict, min float64) bool {
	if v.Unknown {
		return true
	}
	if min >= 1.0 && v.Verdict == "pass" {
		return true
	}
	return v.Score+1e-9 >= min && v.Verdict != "fail"
}

type judgeVerdict struct {
	Score    float64 `json:"score"`
	Verdict  string  `json:"verdict"`
	Evidence string  `json:"evidence"`
	Unknown  bool    `json:"unknown"`
}

func (j *judgeRunner) call(ctx context.Context, d JudgeDim, min float64, transcript, snapshot []byte, surface string) (judgeVerdict, error) {
	prompt := judgePrompt(d, transcript, snapshot, surface)
	raw, err := j.invoke(ctx, prompt)
	if err != nil {
		return judgeVerdict{}, err
	}
	v, err := parseVerdict(raw)
	if err != nil {
		raw, err2 := j.invoke(ctx, prompt+"\n\nYour previous reply was not valid JSON. Reply with JSON only.")
		if err2 != nil {
			return judgeVerdict{}, err
		}
		v, err = parseVerdict(raw)
		if err != nil {
			return judgeVerdict{}, err
		}
	}
	return j.confirmFail(ctx, prompt, min, v), nil
}

func (j *judgeRunner) confirmFail(ctx context.Context, prompt string, min float64, v judgeVerdict) judgeVerdict {
	if v.Unknown || v.Verdict != "fail" || min < 1.0 {
		return v
	}
	raw, err := j.invoke(ctx, prompt+"\n\nA previous grader voted fail with this evidence: "+v.Evidence+". Independently re-grade; reply JSON only.")
	if err != nil {
		v.Unknown = true
		return v
	}
	v2, err := parseVerdict(raw)
	if err != nil || v2.Verdict != "fail" {
		v.Unknown = true
	}
	return v
}

// invoke runs one judge call. The JSON envelope carries the grading spend.
func (j *judgeRunner) invoke(ctx context.Context, prompt string) ([]byte, error) {
	if j.invokeFn != nil {
		return j.invokeFn(ctx, prompt)
	}
	if j.harness == clientCodex {
		return j.invokeCodex(ctx, prompt)
	}
	return j.invokeClaude(ctx, prompt)
}

func (j *judgeRunner) invokeClaude(ctx context.Context, prompt string) ([]byte, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return nil, fmt.Errorf("judge: claude not on PATH")
	}
	args := []string{"-p", prompt, "--output-format", "json", "--dangerously-skip-permissions"}
	if j.model != "" {
		args = append(args, "--model", j.model)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			// The claude CLI writes some errors (quota, auth) to stdout.
			detail = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("judge invoke: %w\n%s", err, detail)
	}
	text, cost := parseJudgeEnvelope(stdout.Bytes())
	j.addCost(cost)
	return text, nil
}

func (j *judgeRunner) invokeCodex(ctx context.Context, prompt string) ([]byte, error) {
	if _, err := exec.LookPath("codex"); err != nil {
		return nil, fmt.Errorf("judge: codex not on PATH")
	}
	workdir, err := os.MkdirTemp("", "trebi-eval-judge-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workdir)
	scratch := filepath.Join(workdir, "home")
	if err := j.seedCodexHome(scratch); err != nil {
		return nil, err
	}
	defer func() {
		if werr := writeBackCodexAuth(scratch); werr != nil {
			fmt.Fprintf(os.Stderr, "eval: skip auth write-back: %v\n", werr)
		}
	}()
	cmd := exec.Command("codex", codexJudgeArgs(j.model)...)
	cmd.Dir = workdir
	cmd.Env = append(filterEnv(os.Environ(), "CODEX_HOME", "HOME"),
		"CODEX_HOME="+scratch,
		"HOME="+scratch,
	)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = runWithGroup(ctx, cmd)
	res := parseCodexJSON(stdout.Bytes())
	j.addCost(res.CostUSD)
	text, rerr := judgeReplyFromCodex(res)
	if rerr == nil {
		return text, nil
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("judge invoke: %w\n%s", err, detail)
	}
	return nil, rerr
}

func (j *judgeRunner) seedCodexHome(dir string) error {
	if err := copyCodexAuth(dir); err != nil {
		return fmt.Errorf("judge: copy codex auth: %w", err)
	}
	if j.model != "" {
		return nil
	}
	carried := carryCodexModelConfig()
	if carried == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.toml"), []byte(carried+"\n"), 0o600)
}

func (j *judgeRunner) addCost(usd float64) {
	j.mu.Lock()
	j.costUSD += usd
	j.mu.Unlock()
}

func codexJudgeArgs(model string) []string {
	args := []string{
		"exec", "--json",
		"--ephemeral",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
		"--color", "never",
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	return append(args, "-")
}

func judgeReplyFromCodex(res *LaunchResult) ([]byte, error) {
	if res == nil {
		return nil, fmt.Errorf("judge invoke: empty reply")
	}
	for i := len(res.Steps) - 1; i >= 0; i-- {
		if res.Steps[i].Role == "assistant" && strings.TrimSpace(res.Steps[i].Content) != "" {
			return []byte(res.Steps[i].Content), nil
		}
	}
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("judge invoke: %s", strings.Join(res.Errors, "; "))
	}
	return nil, fmt.Errorf("judge invoke: empty reply")
}

// spendUSD is the grading spend so far.
func (j *judgeRunner) spendUSD() float64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.costUSD
}

// parseJudgeEnvelope reads the reply text and cost from `claude -p
// --output-format json`. Other bodies pass through with no cost attributed.
func parseJudgeEnvelope(raw []byte) ([]byte, float64) {
	var env struct {
		Result  string  `json:"result"`
		IsError bool    `json:"is_error"`
		Cost    float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &env); err != nil || env.Result == "" {
		return raw, 0
	}
	return []byte(env.Result), env.Cost
}

func judgePrompt(d JudgeDim, transcript, snapshot []byte, surface string) string {
	var b strings.Builder
	if surface == surfaceMCP {
		b.WriteString("You score one dimension of an LLM agent transcript that operated trebi via its MCP server.\n")
	} else {
		b.WriteString("You score one dimension of an LLM agent transcript that operated trebi via its CLI.\n")
	}
	b.WriteString("Reply with JSON only: {\"score\":0.0-1.0,\"verdict\":\"pass\"|\"fail\",\"evidence\":\"cite lines\",\"unknown\":false}.\n")
	b.WriteString("If you cannot tell, set unknown=true. Cite evidence. Do not grade other dimensions.\n")
	if surface == surfaceMCP {
		b.WriteString("The agent calls MCP tools shaped {\"action\":\"<name>\",\"input\":{…}}. The trebi binary is not on PATH.\n")
		b.WriteString("Reading a tool's docs action, or system guide / guides, is required onboarding, not wander.\n")
		b.WriteString("Tool replies are already JSON, so --json and jq do not apply and their absence is never waste.\n")
		b.WriteString("A rule that names a CLI-only construct (curl to /v1, unbounded -f, --limit / --lines flags, hand-started service daemon, killing the daemon PID) can still be broken through the bash tool. If the transcript shows no bash tool use at all, that rule did not apply; say so rather than counting it as satisfied.\n")
	} else {
		b.WriteString("The SKILL first-checks (service status, doctor, workspace current) are required, not wander.\n")
		b.WriteString("Looking up --help after an error is recovery, not waste. Polling runs show / conversation messages while waiting is expected.\n")
		b.WriteString("runs show / job show / skill show already emit JSON; --json on those is not an invented flag.\n")
		b.WriteString("service logs uses --lines / -n (and --grep), not --limit. bus and integration logs use --limit. Unbounded -f is the violation.\n")
	}
	b.WriteString("trebi job run --test applies only when this session created or edited that JOB.md, not rules fire, assistant say, or a pre-existing job.\n")
	b.WriteString("For binary dimensions (min 1.0): score 1.0 and verdict pass if no rule that applied was broken; score 0.0 and verdict fail if one was; otherwise unknown=true. Never score 0.9 against min 1.0.\n")
	b.WriteString("Your verdict must follow from your evidence. If your evidence concludes the rubric is satisfied, the verdict is pass. Deterministic checks already verified the end state; grade only what the transcript shows, and do not re-derive end-state facts from file paths.\n\n")
	b.WriteString("Dimension: " + d.Name + "\n\nRubric:\n" + d.Rubric + "\n\n")
	b.WriteString("Transcript:\n")
	b.Write(truncate(transcript, 80_000))
	b.WriteString("\n\nState snapshot:\n")
	b.Write(truncate(snapshot, 40_000))
	return b.String()
}

func parseVerdict(raw []byte) (judgeVerdict, error) {
	s := strings.TrimSpace(string(raw))
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var v judgeVerdict
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return v, fmt.Errorf("verdict json: %w (%s)", err, truncate([]byte(s), 200))
	}
	return v, nil
}

func parseRubrics(md string) map[string]Rubric {
	out := map[string]Rubric{}
	var cur Rubric
	flush := func() {
		if cur.Name == "" {
			return
		}
		cur.Text = strings.TrimSpace(cur.Text)
		if cur.Min == 0 {
			cur.Min = 0.7
		}
		out[cur.Name] = cur
		cur = Rubric{}
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "## ") {
			flush()
			cur.Name = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "**min:**") {
			var min float64
			if _, err := fmt.Sscanf(strings.TrimSpace(line), "**min:** %f", &min); err == nil {
				cur.Min = min
			}
			continue
		}
		if cur.Name != "" {
			cur.Text += line + "\n"
		}
	}
	flush()
	return out
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}
