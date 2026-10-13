package job

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseAcceptGrammar(t *testing.T) {
	t.Parallel()
	got, err := ParseAccept([]string{" Admins ", "ALLOWLIST", "Alice@ACME.com", "@Acme.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := []AcceptEntry{
		{Keyword: "admins"}, {Keyword: "allowlist"},
		{Address: "alice@acme.com"}, {Domain: "acme.com"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	for _, bad := range []string{"", "*", "@", "alice", "@acme", "a b@c.com", "operators"} {
		if _, err := ParseAccept([]string{bad}); err == nil {
			t.Errorf("ParseAccept(%q) accepted", bad)
		}
	}
}

func TestOperatorFrontmatterByDocument(t *testing.T) {
	t.Parallel()
	agent := "---\ntitle: Research\nemail:\n  accept: [admins]\nkit:\n  skills: [research]\nbudget:\n  monthly_usd: 10\nreport:\n  every: 168h\n  to: [admins]\n---\n\nResearch.\n"
	j, err := ParseBytes("/ws/agents/research/AGENT.md", []byte(agent))
	if err != nil {
		t.Fatal(err)
	}
	if j.Title != "Research" || !j.AcceptSet || !reflect.DeepEqual(j.EmailAccept, []string{"admins"}) {
		t.Fatalf("identity/email = %+v", j)
	}
	if j.Kit == nil || len(j.Kit.Skills) != 1 || j.Budget == nil || j.Budget.MonthlyUSD != 10 || j.Report == nil || j.Report.Every != "168h" {
		t.Fatalf("operator payload = %+v/%+v/%+v", j.Kit, j.Budget, j.Report)
	}

	for name, doc := range map[string]string{
		"job email":     "---\nemail:\n  accept: [admins]\n---\n\nNo.\n",
		"job kit":       "---\nkit:\n  skills: [x]\n---\n\nNo.\n",
		"job budget":    "---\nbudget:\n  monthly_usd: 1\n---\n\nNo.\n",
		"job report":    "---\nreport:\n  every: 1h\n---\n\nNo.\n",
		"assistant kit": "---\nkit:\n  skills: [x]\n---\n\nNo.\n",
		"zero budget":   "---\nbudget:\n  monthly_usd: 0\n---\n\nNo.\n",
		"bad mode":      "---\nbudget:\n  monthly_usd: 1\n  on_exceed: explode\n---\n\nNo.\n",
		"both report":   "---\nreport:\n  cron: '* * * * *'\n  every: 1h\n  to: [admins]\n---\n\nNo.\n",
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
		Envset:     EnvsetList{"agents"},
		Env:        map[string]string{"REGION": "eu", "AGENT_ONLY": "1"},
		Skills:     []string{"research"},
		Connectors: []string{"notion"},
	}
	env := j.LaunchDefaultsWithKit(kit).Env
	if env["AGENT_ONLY"] != "1" || env["REGION"] != "us" || env["JOB_ONLY"] != "yes" {
		t.Fatalf("merged env = %+v", env)
	}
	if env := j.LaunchDefaults().Env; env == nil || env["REGION"] != "us" || env["JOB_ONLY"] != "yes" {
		t.Fatal("plain defaults changed")
	}
}

func TestResolvedWorkingDirOwner(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	const jobPath = "/ws/agents/acme/task/JOB.md"
	const agentPath = "/ws/agents/acme/AGENT.md"
	cases := []struct {
		name, jobDir, agentDir, want, source string
	}{
		{"job set", "/srv/job", "/srv/agent", "/srv/job", WorkingDirFromJob},
		{"job unset, agent set", "", "/srv/agent", "/srv/agent", WorkingDirFromAgent},
		{"both unset", "", "", "/ws/agents/acme/task", WorkingDirFromDefault},
		{"agent relative", "", "../../clients/acme", "/ws/clients/acme", WorkingDirFromAgent},
		{"agent tilde", "", "~/BTD/acme", filepath.Join(home, "BTD/acme"), WorkingDirFromAgent},
		{"job relative", "data", "/srv/agent", "/ws/agents/acme/task/data", WorkingDirFromJob},
		{"job tilde", "~/work", "", filepath.Join(home, "work"), WorkingDirFromJob},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agentDoc, jobDoc := "---\n", "---\n"
			if tc.agentDir != "" {
				agentDoc += "working_dir: " + tc.agentDir + "\n"
			}
			if tc.jobDir != "" {
				jobDoc += "working_dir: " + tc.jobDir + "\n"
			}
			agent, err := ParseBytes(agentPath, []byte(agentDoc+"---\n\nLead.\n"))
			if err != nil {
				t.Fatal(err)
			}
			j, err := ParseBytes(jobPath, []byte(jobDoc+"---\n\nRun.\n"))
			if err != nil {
				t.Fatal(err)
			}
			kit := agent.OwnerKit()
			if got, src := j.ResolvedWorkingDir(kit); got != tc.want || src != tc.source {
				t.Fatalf("ResolvedWorkingDir = %q, %q; want %q, %q", got, src, tc.want, tc.source)
			}
			if got := j.LaunchDefaultsWithKit(kit).WorkingDir; got != tc.want {
				t.Fatalf("LaunchDefaultsWithKit.WorkingDir = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestEmailStyle(t *testing.T) {
	t.Parallel()
	doc := "---\nemail:\n  style:\n    accent: \"#0f766e\"\n    logo: https://acme.com/logo.png\n    footer: Acme Ops\n---\n\nGrow.\n"
	j, err := ParseBytes("/ws/agents/growth/AGENT.md", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := StyleYAML{Accent: "#0f766e", Logo: "https://acme.com/logo.png", Footer: "Acme Ops"}
	if j.EmailStyle == nil || *j.EmailStyle != want {
		t.Fatalf("style = %+v", j.EmailStyle)
	}
	if got := (&StyleYAML{Footer: "Mine"}).Merge(want); got.Footer != "Mine" || got.Accent != want.Accent {
		t.Errorf("merge = %+v", got)
	}
	if got := (*StyleYAML)(nil).Merge(want); got != want {
		t.Errorf("nil merge = %+v", got)
	}
	for _, bad := range []StyleYAML{
		{Accent: "teal"}, {Accent: "#12345"}, {Logo: "http://acme.com/l.png"}, {Logo: "javascript:x"},
		{Footer: "a\nb"}, {Footer: strings.Repeat("x", 201)},
	} {
		if bad.Validate() == nil {
			t.Errorf("Validate(%+v) accepted", bad)
		}
	}
}
