package eval

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
)

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
