package eval

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cell is one task×client score.
type Cell struct {
	Task     string  `json:"task"`
	Tier     Tier    `json:"tier"`
	Client   string  `json:"client"`
	Trials   int     `json:"trials"`
	Passed   int     `json:"passed"`
	Infra    int     `json:"infra"`
	PassAt1  float64 `json:"pass_at_1"`
	PassHatK float64 `json:"pass_hat_k"`
	CostUSD  float64 `json:"cost_usd"`
	DurS     float64 `json:"duration_s"`
	Surface  string  `json:"surface,omitempty"`
}

// passHatCell hides pass^k for a single graded trial, where it restates pass@1.
func passHatCell(c Cell) string {
	if c.Trials-c.Infra <= 1 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", c.PassHatK)
}

func cellSurface(s string) string {
	if s == "" {
		return surfaceCLI
	}
	return s
}

// Report is one suite run.
type Report struct {
	ID        string        `json:"id"`
	Started   time.Time     `json:"started"`
	Finished  time.Time     `json:"finished"`
	CostUSD   float64       `json:"cost_usd"`
	JudgeUSD  float64       `json:"judge_cost_usd,omitempty"`
	TotalUSD  float64       `json:"total_cost_usd"`
	Cells     []Cell        `json:"cells"`
	Trials    []TrialResult `json:"trials"`
	BudgetUSD float64       `json:"budget_usd,omitempty"`
	Aborted   string        `json:"aborted,omitempty"`
}

func newReport() *Report {
	return &Report{
		ID:      time.Now().UTC().Format("20060102T150405Z"),
		Started: time.Now().UTC(),
	}
}

func (r *Report) add(t TrialResult) {
	r.Trials = append(r.Trials, t)
	r.CostUSD += t.CostUSD
}

func (r *Report) finish() {
	r.Finished = time.Now().UTC()
	r.Cells = r.rollup()
	r.TotalUSD = r.CostUSD + r.JudgeUSD
}

// checkpoint writes a partial report so a killed runner still has scores.
func (r *Report) checkpoint(dir string) error {
	r.Finished = time.Now().UTC()
	r.Cells = r.rollup()
	r.TotalUSD = r.CostUSD + r.JudgeUSD
	return r.write(dir)
}

func (t *TrialResult) writeFile() error {
	if t == nil || t.Dir == "" {
		return nil
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(t.Dir, "trial.json"), data, 0o644)
}

func (r *Report) rollup() []Cell {
	type key struct{ task, client, surface string }
	type acc struct {
		n, pass, infra int
		cost, dur      float64
	}
	m := map[key]*acc{}
	var order []key
	for _, t := range r.Trials {
		k := key{t.TaskID, t.Client, cellSurface(t.Surface)}
		a, ok := m[k]
		if !ok {
			a = &acc{}
			m[k] = a
			order = append(order, k)
		}
		a.n++
		a.cost += t.CostUSD
		a.dur += t.DurationS
		switch t.Outcome {
		case OutcomePass:
			a.pass++
		case OutcomeInfraFailure:
			a.infra++
		}
	}
	cells := make([]Cell, 0, len(order))
	for _, k := range order {
		a := m[k]
		agentN := a.n - a.infra
		var p1, pk float64
		if agentN > 0 {
			p1 = float64(a.pass) / float64(agentN)
			if a.pass == agentN {
				pk = 1
			}
		}
		cells = append(cells, Cell{
			Task: k.task, Client: k.client, Surface: k.surface, Trials: a.n, Passed: a.pass, Infra: a.infra,
			PassAt1: p1, PassHatK: pk, CostUSD: a.cost, DurS: a.dur,
		})
	}
	return cells
}

func (r *Report) write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	js, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), js, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(r.markdown()), 0o644)
}

func (r *Report) markdown() string {
	var b strings.Builder
	b.WriteString("# trebi eval " + r.ID + "\n\n")
	fmt.Fprintf(&b, "cost_usd=%.4f judge_usd=%.4f total_usd=%.4f duration=%s\n\n",
		r.CostUSD, r.JudgeUSD, r.CostUSD+r.JudgeUSD, r.Finished.Sub(r.Started).Truncate(time.Second))
	if r.Aborted != "" {
		b.WriteString("aborted: " + r.Aborted + "\n\n")
	}
	b.WriteString("| Task | Surface | Client | pass@1 | pass^k | trials | infra | cost |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, c := range r.Cells {
		fmt.Fprintf(&b, "| %s | %s | %s | %.2f | %s | %d | %d | %.4f |\n",
			c.Task, cellSurface(c.Surface), c.Client, c.PassAt1, passHatCell(c), c.Trials, c.Infra, c.CostUSD)
	}
	b.WriteString("\n## Trials\n\n")
	for _, t := range r.Trials {
		fmt.Fprintf(&b, "- %s/%s#%d **%s** cost=%.4f dir=`%s`\n",
			t.TaskID, t.Client, t.Index, t.Outcome, t.CostUSD, t.Dir)
		if t.Error != "" {
			b.WriteString("  - error: " + t.Error + "\n")
		}
	}
	return b.String()
}

// Baseline is the checked-in pass@1 matrix.
type Baseline struct {
	Cells []Cell `json:"cells"`
}

func loadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *Report) regress(base *Baseline) []string {
	if base == nil || len(base.Cells) == 0 {
		return nil
	}
	type key struct{ task, client, surface string }
	got := map[key]Cell{}
	for _, c := range r.Cells {
		got[key{c.Task, c.Client, cellSurface(c.Surface)}] = c
	}
	var drops []string
	for _, c := range base.Cells {
		g, ok := got[key{c.Task, c.Client, cellSurface(c.Surface)}]
		if !ok {
			continue
		}
		if g.PassAt1+1e-9 < c.PassAt1 {
			drops = append(drops, fmt.Sprintf("%s/%s pass@1 %.2f -> %.2f", c.Task, c.Client, c.PassAt1, g.PassAt1))
		}
	}
	return drops
}

// ReviewItem is one sampled trial×dimension for a human label.
type ReviewItem struct {
	Trial      string `json:"trial"`
	Task       string `json:"task"`
	Client     string `json:"client"`
	Dimension  string `json:"dimension"`
	JudgePass  *bool  `json:"judge_pass"`
	HumanPass  *bool  `json:"human_pass"`
	Transcript string `json:"transcript"`
}

const reviewAgreeMin = 0.8

func sampleReview(reportDir string, n int) ([]ReviewItem, error) {
	data, err := os.ReadFile(filepath.Join(reportDir, "report.json"))
	if err != nil {
		return nil, err
	}
	var rep Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, err
	}
	var pool []ReviewItem
	for _, t := range rep.Trials {
		for _, j := range t.Judge {
			jp := j.Pass
			pool = append(pool, ReviewItem{
				Trial: t.Dir, Task: t.TaskID, Client: t.Client,
				Dimension: j.Name, JudgePass: &jp,
				Transcript: filepath.Join(t.Dir, "transcript.json"),
			})
		}
	}
	if len(pool) == 0 {
		return nil, fmt.Errorf("no judge verdicts in %s", reportDir)
	}
	if n <= 0 || n > len(pool) {
		n = len(pool)
		if n > 10 {
			n = 10
		}
	}
	rand.Shuffle(len(pool), func(i, k int) { pool[i], pool[k] = pool[k], pool[i] })
	return pool[:n], nil
}

func writeReviewQueue(path string, items []ReviewItem) error {
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func agreement(labeled []ReviewItem) (float64, int, int, error) {
	var agree, total int
	for _, it := range labeled {
		if it.HumanPass == nil || it.JudgePass == nil {
			continue
		}
		total++
		if *it.HumanPass == *it.JudgePass {
			agree++
		}
	}
	if total == 0 {
		return 0, 0, 0, fmt.Errorf("no labeled items")
	}
	return float64(agree) / float64(total), agree, total, nil
}

func loadReview(path string) ([]ReviewItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []ReviewItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func reviewReport(rate float64, agree, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "judge/human agreement: %d/%d = %.2f (min %.2f)\n", agree, total, rate, reviewAgreeMin)
	if rate+1e-9 < reviewAgreeMin {
		b.WriteString("below threshold: block rubric changes until calibrated\n")
	}
	return b.String()
}
