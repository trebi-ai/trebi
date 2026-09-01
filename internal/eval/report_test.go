package eval

import (
	"os"
	"path/filepath"
	"strings"
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

// TestUngradedCounted pins that a dimension which abstained or failed to
// invoke is reported. Such a dimension defaults to pass, so without the count
// a run can silently grade less than it claims.
func TestUngradedCounted(t *testing.T) {
	t.Parallel()
	r := newReport()
	r.add(TrialResult{TaskID: "a", Client: "codex", Outcome: OutcomePass, Judge: []JudgeResult{
		{Name: "guardrails", Pass: true, Unknown: true},
		{Name: "efficiency", Pass: true, Infra: true},
		{Name: "accuracy", Pass: true},
	}})
	r.finish()
	c := r.Cells[0]
	if c.Ungraded != 2 {
		t.Fatalf("ungraded = %d, want 2 (%+v)", c.Ungraded, c)
	}
	if c.Infra != 0 {
		t.Fatalf("infra = %d; a passed trial is not an infra trial", c.Infra)
	}
	md := r.markdown()
	if !strings.Contains(md, "ungraded: guardrails, efficiency") {
		t.Fatalf("markdown missing ungraded line:\n%s", md)
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

func TestVerdictPass(t *testing.T) {
	t.Parallel()
	if !(judgeVerdict{Score: 0.9, Verdict: "pass"}).pass(1.0) {
		t.Fatal("binary min trusts verdict pass")
	}
	if (judgeVerdict{Score: 0.9, Verdict: "fail"}).pass(1.0) {
		t.Fatal("verdict fail")
	}
	if (judgeVerdict{Score: 0.4, Verdict: "pass"}).pass(0.5) {
		t.Fatal("score below min")
	}
	if !(judgeVerdict{Unknown: true, Verdict: "fail"}).pass(1.0) {
		t.Fatal("unknown does not fail")
	}
}

func TestTrialOutcomeSkipsEfficiency(t *testing.T) {
	t.Parallel()
	checks := []CheckResult{{Name: "ok", Pass: true}}
	judge := []JudgeResult{
		{Name: "guardrails", Pass: true, Min: 1},
		{Name: "efficiency", Pass: false, Min: 0.5},
	}
	if trialOutcome(checks, judge) != OutcomePass {
		t.Fatal("efficiency must not fail a green outcome")
	}
	judge[0].Pass = false
	if trialOutcome(checks, judge) != OutcomeFail {
		t.Fatal("guardrails still fails")
	}
}

func TestTrialOutcomeJudgeInfra(t *testing.T) {
	t.Parallel()
	checks := []CheckResult{{Name: "ok", Pass: true}}
	judge := []JudgeResult{
		{Name: "guardrails", Infra: true, Unknown: true, Pass: true, Min: 1},
		{Name: "user answer", Infra: true, Unknown: true, Pass: true, Min: 0.7},
	}
	if trialOutcome(checks, judge) != OutcomeInfraFailure {
		t.Fatal("all invoke failures are infra")
	}
}
