package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPassAt1AndHatK(t *testing.T) {
	t.Parallel()
	r := newReport()
	r.add(TrialResult{TaskID: "a", Client: "codex", Outcome: OutcomePass})
	r.add(TrialResult{TaskID: "a", Client: "codex", Outcome: OutcomeFail})
	r.add(TrialResult{TaskID: "a", Client: "codex", Outcome: OutcomeInfraFailure})
	r.finish()
	if len(r.Cells) != 1 {
		t.Fatalf("%+v", r.Cells)
	}
	c := r.Cells[0]
	if c.Passed != 1 || c.Infra != 1 {
		t.Fatalf("%+v", c)
	}
	if c.PassAt1 != 0.5 {
		t.Fatalf("pass@1 %v", c.PassAt1)
	}
	if c.PassHatK != 0 {
		t.Fatalf("pass^k %v", c.PassHatK)
	}
}

func TestPassHatKAllGreen(t *testing.T) {
	t.Parallel()
	r := newReport()
	r.add(TrialResult{TaskID: "a", Client: "claude", Outcome: OutcomePass})
	r.add(TrialResult{TaskID: "a", Client: "claude", Outcome: OutcomePass})
	r.finish()
	if r.Cells[0].PassHatK != 1 {
		t.Fatalf("%+v", r.Cells[0])
	}
}

func TestRegress(t *testing.T) {
	t.Parallel()
	r := newReport()
	r.add(TrialResult{TaskID: "a", Client: "codex", Outcome: OutcomeFail})
	r.finish()
	base := &Baseline{Cells: []Cell{{Task: "a", Client: "codex", PassAt1: 1}}}
	drops := r.regress(base)
	if len(drops) != 1 {
		t.Fatalf("%v", drops)
	}
}

func TestReportWrite(t *testing.T) {
	t.Parallel()
	r := newReport()
	r.add(TrialResult{TaskID: "a", Client: "codex", Outcome: OutcomePass, CostUSD: 0.01})
	r.finish()
	dir := t.TempDir()
	if err := r.write(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "report.json")); err != nil {
		t.Fatal(err)
	}
}

func TestReportCheckpointAndTrialFile(t *testing.T) {
	t.Parallel()
	r := newReport()
	tr := TrialResult{TaskID: "a", Client: "codex", Outcome: OutcomePass, Dir: t.TempDir()}
	r.add(tr)
	dir := t.TempDir()
	if err := r.checkpoint(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "report.md")); err != nil {
		t.Fatal(err)
	}
	if err := tr.writeFile(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tr.Dir, "trial.json")); err != nil {
		t.Fatal(err)
	}
}

func TestReviewAgreement(t *testing.T) {
	t.Parallel()
	ttrue, ffalse := true, false
	items := []ReviewItem{
		{JudgePass: &ttrue, HumanPass: &ttrue},
		{JudgePass: &ttrue, HumanPass: &ffalse},
	}
	rate, agree, total, err := agreement(items)
	if err != nil || total != 2 || agree != 1 || rate != 0.5 {
		t.Fatalf("%v %d %d %v", rate, agree, total, err)
	}
}

func TestParseRubricsAndVerdict(t *testing.T) {
	t.Parallel()
	md := "## guardrails\n\n**min:** 1.0\n\nNever curl.\n\n## efficiency\n\nUse jq.\n"
	r := parseRubrics(md)
	if r["guardrails"].Min != 1.0 || r["efficiency"].Min != 0.7 {
		t.Fatalf("%+v", r)
	}
	v, err := parseVerdict([]byte("here\n```json\n{\"score\":0.9,\"verdict\":\"pass\",\"evidence\":\"line 1\",\"unknown\":false}\n```"))
	if err != nil || v.Score != 0.9 || v.Unknown {
		t.Fatalf("%+v %v", v, err)
	}
}
