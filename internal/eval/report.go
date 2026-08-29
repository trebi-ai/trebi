package eval

import (
	"encoding/json"
	"fmt"
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
}

// Report is one suite run.
type Report struct {
	ID        string        `json:"id"`
	Started   time.Time     `json:"started"`
	Finished  time.Time     `json:"finished"`
	CostUSD   float64       `json:"cost_usd"`
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
}

func (r *Report) rollup() []Cell {
	type key struct{ task, client string }
	type acc struct {
		n, pass, infra int
		cost, dur      float64
	}
	m := map[key]*acc{}
	var order []key
	for _, t := range r.Trials {
		k := key{t.TaskID, t.Client}
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
			Task: k.task, Client: k.client, Trials: a.n, Passed: a.pass, Infra: a.infra,
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
	fmt.Fprintf(&b, "cost_usd=%.4f duration=%s\n\n", r.CostUSD, r.Finished.Sub(r.Started).Truncate(time.Second))
	if r.Aborted != "" {
		b.WriteString("aborted: " + r.Aborted + "\n\n")
	}
	b.WriteString("| Task | Client | pass@1 | pass^k | trials | infra | cost |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, c := range r.Cells {
		fmt.Fprintf(&b, "| %s | %s | %.2f | %.2f | %d | %d | %.4f |\n",
			c.Task, c.Client, c.PassAt1, c.PassHatK, c.Trials, c.Infra, c.CostUSD)
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
	type key struct{ task, client string }
	got := map[key]Cell{}
	for _, c := range r.Cells {
		got[key{c.Task, c.Client}] = c
	}
	var drops []string
	for _, c := range base.Cells {
		g, ok := got[key{c.Task, c.Client}]
		if !ok {
			continue
		}
		if g.PassAt1+1e-9 < c.PassAt1 {
			drops = append(drops, fmt.Sprintf("%s/%s pass@1 %.2f -> %.2f", c.Task, c.Client, c.PassAt1, g.PassAt1))
		}
	}
	return drops
}
