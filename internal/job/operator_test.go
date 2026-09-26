package job

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAcceptGrammar(t *testing.T) {
	t.Parallel()
	got, err := ParseAccept([]string{" Operators ", "ALLOWLIST", "Alice@ACME.com", "@Acme.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := []AcceptEntry{
		{Keyword: "operators"}, {Keyword: "allowlist"},
		{Address: "alice@acme.com"}, {Domain: "acme.com"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	for _, bad := range []string{"", "*", "@", "alice", "@acme", "a b@c.com"} {
		if _, err := ParseAccept([]string{bad}); err == nil {
			t.Errorf("ParseAccept(%q) accepted", bad)
		}
	}
}

func TestOperatorFrontmatterByDocument(t *testing.T) {
	t.Parallel()
	agent := "---\ntitle: Research\nemail:\n  accept: [operators]\nkit:\n  skills: [research]\nbudget:\n  monthly_usd: 10\nreport:\n  every: 168h\n  to: [operators]\n---\n\nResearch.\n"
	j, err := ParseBytes("/ws/agents/research/AGENT.md", []byte(agent))
	if err != nil {
		t.Fatal(err)
	}
	if j.Title != "Research" || !j.AcceptSet || !reflect.DeepEqual(j.EmailAccept, []string{"operators"}) {
		t.Fatalf("identity/email = %+v", j)
	}
	if j.Kit == nil || len(j.Kit.Skills) != 1 || j.Budget == nil || j.Budget.MonthlyUSD != 10 || j.Report == nil || j.Report.Every != "168h" {
		t.Fatalf("operator payload = %+v/%+v/%+v", j.Kit, j.Budget, j.Report)
	}

	for name, doc := range map[string]string{
		"job email":     "---\nemail:\n  accept: [operators]\n---\n\nNo.\n",
		"job kit":       "---\nkit:\n  skills: [x]\n---\n\nNo.\n",
		"job budget":    "---\nbudget:\n  monthly_usd: 1\n---\n\nNo.\n",
		"job report":    "---\nreport:\n  every: 1h\n---\n\nNo.\n",
		"assistant kit": "---\nkit:\n  skills: [x]\n---\n\nNo.\n",
		"zero budget":   "---\nbudget:\n  monthly_usd: 0\n---\n\nNo.\n",
		"bad mode":      "---\nbudget:\n  monthly_usd: 1\n  on_exceed: explode\n---\n\nNo.\n",
		"both report":   "---\nreport:\n  cron: '* * * * *'\n  every: 1h\n  to: [operators]\n---\n\nNo.\n",
	} {
		j, err := ParseBytes("/ws/"+strings.Replace(name, " ", "_", 1)+"/JOB.md", []byte(doc))
		if err == nil || j == nil || j.ParseError == "" {
			t.Errorf("%s: accepted operator violation", name)
		}
	}
}

func TestLaunchDefaultsWithKit(t *testing.T) {
	t.Parallel()
	j, err := ParseBytes("/ws/agents/research/task/JOB.md", []byte(
		"---\nenvset: jobset\nenv:\n  REGION: us\n  JOB_ONLY: yes\n---\n\nRun.\n"))
	if err != nil {
		t.Fatal(err)
	}
	kit := &KitYAML{
		Envset: EnvsetList{"agents"},
		Env:    map[string]string{"REGION": "eu", "AGENT_ONLY": "1"},
		Skills: []string{"research"},
		MCP:    []string{"notion"},
	}
	env := j.LaunchDefaultsWithKit(kit).Env
	if env["AGENT_ONLY"] != "1" || env["REGION"] != "us" || env["JOB_ONLY"] != "yes" {
		t.Fatalf("merged env = %+v", env)
	}
	if env := j.LaunchDefaults().Env; env == nil || env["REGION"] != "us" || env["JOB_ONLY"] != "yes" {
		t.Fatal("plain defaults changed")
	}
}
