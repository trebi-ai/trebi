package job_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/secrets"
	"gopkg.in/yaml.v3"
)

func TestEnvsetListYAMLRoundTrip(t *testing.T) {
	t.Parallel()

	var one job.EnvsetList
	if err := yaml.Unmarshal([]byte("prod\n"), &one); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(one, job.EnvsetList{"prod"}) {
		t.Fatalf("scalar unmarshal = %v", one)
	}
	out, err := yaml.Marshal(one)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "prod\n" {
		t.Fatalf("scalar marshal = %q", out)
	}

	var many job.EnvsetList
	if err := yaml.Unmarshal([]byte("[alpha, beta]\n"), &many); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(many, job.EnvsetList{"alpha", "beta"}) {
		t.Fatalf("sequence unmarshal = %v", many)
	}
	out, err = yaml.Marshal(many)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "- alpha\n- beta\n" {
		t.Fatalf("sequence marshal = %q", out)
	}

	for name, src := range map[string]string{
		"null":   "null\n",
		"blank":  "\"\"\n",
		"empty":  "[]\n",
		"spaces": "  \n",
	} {
		var got job.EnvsetList
		if err := yaml.Unmarshal([]byte(src), &got); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: got %v, want empty", name, got)
		}
	}

	var bad job.EnvsetList
	if err := yaml.Unmarshal([]byte("name: prod\n"), &bad); err == nil {
		t.Fatal("mapping accepted")
	}

	var zero job.EnvsetList
	if got := zero.List(); got == nil || len(got) != 0 {
		t.Fatalf("nil List = %#v", got)
	}
	if zero.First() != "" {
		t.Fatalf("nil First = %q", zero.First())
	}
}

func writeEnvSets(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home.Base(), "envsets.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

const envSetsFixture = "alpha:\n  SHARED: from-alpha\n  ALPHA_ONLY: '1'\nbeta:\n  SHARED: from-beta\n  BETA_ONLY: '2'\n"

// TestResolvedEnvMergeOrder covers plan H.3: sets apply in list order, a
// later set wins on a key conflict, inline env wins over every set, and an
// undefined name is skipped.
func TestResolvedEnvMergeOrder(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	writeEnvSets(t, envSetsFixture)

	j, err := job.ParseBytes("/ws/demo/JOB.md", []byte("---\nenvset: [alpha, beta]\n---\n\nRun.\n"))
	if err != nil {
		t.Fatal(err)
	}
	env := j.ResolvedEnv()
	want := map[string]string{"SHARED": "from-beta", "ALPHA_ONLY": "1", "BETA_ONLY": "2"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("merged = %v, want %v", env, want)
	}

	j, err = job.ParseBytes("/ws/demo/JOB.md", []byte("---\nenvset: [alpha, beta]\nenv:\n  SHARED: inline-wins\n---\n\nRun.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if env := j.ResolvedEnv(); env["SHARED"] != "inline-wins" {
		t.Fatalf("inline lost: %v", env)
	}

	j, err = job.ParseBytes("/ws/demo/JOB.md", []byte("---\nenvset: [alpha, ghost]\n---\n\nRun.\n"))
	if err != nil {
		t.Fatal(err)
	}
	env = j.ResolvedEnv()
	if !reflect.DeepEqual(env, map[string]string{"SHARED": "from-alpha", "ALPHA_ONLY": "1"}) {
		t.Fatalf("missing name not skipped: %v", env)
	}
}

// TestLaunchDefaultsWithKitEnvsetList pins the replacement rule: a job
// envset list replaces the agent list as a whole; without a job list the
// agent list applies.
func TestLaunchDefaultsWithKitEnvsetList(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	writeEnvSets(t, envSetsFixture)

	kit := &job.KitYAML{
		Envset: job.EnvsetList{"beta"},
		Env:    map[string]string{"KIT_ONLY": "k"},
	}
	j, err := job.ParseBytes("/ws/agents/research/task/JOB.md", []byte("---\nenvset: [alpha]\nenv:\n  JOB_ONLY: j\n---\n\nRun.\n"))
	if err != nil {
		t.Fatal(err)
	}
	env := j.LaunchDefaultsWithKit(kit).Env
	want := map[string]string{"SHARED": "from-alpha", "ALPHA_ONLY": "1", "JOB_ONLY": "j", "KIT_ONLY": "k"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("job list did not replace agent list: %v", env)
	}

	j, err = job.ParseBytes("/ws/agents/research/task/JOB.md", []byte("---\n---\n\nRun.\n"))
	if err != nil {
		t.Fatal(err)
	}
	env = j.LaunchDefaultsWithKit(kit).Env
	want = map[string]string{"SHARED": "from-beta", "BETA_ONLY": "2", "KIT_ONLY": "k"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("agent list not applied: %v", env)
	}
}

// TestLaunchDefaultsFolderEnv pins the folder .env step: it sits below the
// envset list and inline env, an env_file key turns it off, and its values
// are never read as secret references.
func TestLaunchDefaultsFolderEnv(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	writeEnvSets(t, envSetsFixture)
	if err := secrets.Set("folder_secret", "from-store"); err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()
	dotenv := "SHARED=from-folder\nINLINE=from-folder\nFOLDER_ONLY=f\nREF=folder_secret\nPEM=\"a\nb\"\nbad line\n"
	if err := os.WriteFile(filepath.Join(wd, ".env"), []byte(dotenv), 0o600); err != nil {
		t.Fatal(err)
	}
	parse := func(fm string) *job.Job {
		t.Helper()
		j, err := job.ParseBytes("/ws/agents/research/task/JOB.md", []byte("---\n"+fm+"---\n\nRun.\n"))
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	off, on := false, true

	j := parse("working_dir: " + wd + "\nenvset: [alpha]\nenv:\n  INLINE: from-job\n")
	d := j.LaunchDefaults()
	want := map[string]string{"SHARED": "from-alpha", "ALPHA_ONLY": "1", "INLINE": "from-job", "FOLDER_ONLY": "f", "REF": "folder_secret", "PEM": "a\nb"}
	if !reflect.DeepEqual(d.Env, want) {
		t.Fatalf("env = %q, want %q", d.Env, want)
	}
	if d.FolderEnv == nil || !reflect.DeepEqual(d.FolderEnv.Keys, []string{"SHARED", "INLINE", "FOLDER_ONLY", "REF", "PEM"}) ||
		!reflect.DeepEqual(d.FolderEnv.Overridden, []string{"SHARED", "INLINE"}) || len(d.FolderEnv.Skipped) != 1 {
		t.Fatalf("folder env = %+v", d.FolderEnv)
	}

	d = parse("working_dir: " + wd + "\nenv_file: false\n").LaunchDefaults()
	if d.FolderEnv != nil || d.Env["FOLDER_ONLY"] != "" {
		t.Fatalf("env_file false still loads: %+v %v", d.FolderEnv, d.Env)
	}

	kit := &job.KitYAML{WorkingDir: wd, EnvFile: &off}
	if d := parse("").LaunchDefaultsWithKit(kit); d.FolderEnv != nil {
		t.Fatalf("kit off still loads: %+v", d.FolderEnv)
	}
	if d := parse("env_file: true\n").LaunchDefaultsWithKit(kit); d.FolderEnv == nil || d.Env["FOLDER_ONLY"] != "f" {
		t.Fatalf("job on does not win over kit off: %+v", d.FolderEnv)
	}
	kit.EnvFile = &on
	if d := parse("").LaunchDefaultsWithKit(kit); d.WorkingDir != wd || d.Env["FOLDER_ONLY"] != "f" {
		t.Fatalf("agent working dir did not load: %q %v", d.WorkingDir, d.Env)
	}

	agent, err := job.ParseBytes("/ws/agents/research/AGENT.md", []byte("---\nworking_dir: "+wd+"\nenv_file: false\n---\n\nRun.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d := parse("").LaunchDefaultsWithKit(agent.OwnerKit()); d.WorkingDir != wd || d.FolderEnv != nil {
		t.Fatalf("agent env_file false not inherited: %q %+v", d.WorkingDir, d.FolderEnv)
	}

	if d := parse("working_dir: " + t.TempDir() + "\n").LaunchDefaults(); d.FolderEnv != nil {
		t.Fatalf("folder without .env = %+v", d.FolderEnv)
	}
}
