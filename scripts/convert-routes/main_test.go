package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/hometest"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
)

func TestMain(m *testing.M) { hometest.Main(m) }

// fixture is a Trebi home with one workspace.
type fixture struct {
	t    *testing.T
	home string
	ws   string
}

func (f *fixture) write(rel, content string) string {
	f.t.Helper()
	p := filepath.Join(f.ws, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) read(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.ws, rel))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *fixture) run(args ...string) (int, string) {
	f.t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"--home", f.home, "--lock-wait", "300ms"}, args...), &out, &errb)
	return code, out.String() + errb.String()
}

func (f *fixture) routes(conn string) routing.File {
	f.t.Helper()
	p := filepath.Join(f.ws, "routes", conn+".yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	file := routing.Parse(conn, p, data)
	if len(file.Problems) > 0 {
		f.t.Fatalf("%s problems: %+v", p, file.Problems)
	}
	return file
}

// tree hashes every file of the workspace.
func (f *fixture) tree() map[string]string {
	f.t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(f.ws, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			sum := sha256.Sum256(b)
			out[p] = string(sum[:])
		}
		return nil
	})
	return out
}

const jobHead = "---\nharness: claude\n"

func TestConvertAgentTrigger(t *testing.T) {
	f := setupFixture(t)
	f.write("agents/cmo/AGENT.md", "---\n# keep this comment\nharness: claude\ntriggers:\n  - event: discord\n    rooms: [r1]\n    allowlist: [u1]\n---\n\nBody.\n")
	if code, out := f.run("--apply"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	r := f.routes("discord").Routes
	if len(r) != 1 || r[0].Agent != "cmo" || r[0].Gets != routing.GetsEveryMessage || r[0].Filter.Rooms[0] != "r1" || r[0].Filter.Allowlist[0] != "u1" {
		t.Fatalf("routes: %+v", r)
	}
	if f.routes("discord").Mode != routing.ModeOff {
		t.Fatal("mode is not off")
	}
	want := "---\n# keep this comment\nharness: claude\n---\n\nBody.\n"
	if got := f.read("agents/cmo/AGENT.md"); got != want {
		t.Fatalf("AGENT.md:\n%s", got)
	}
}

func TestConvertJobTriggerInputs(t *testing.T) {
	f := setupFixture(t)
	f.write("agents/cmo/AGENT.md", "---\nharness: claude\n---\nx\n")
	f.write("agents/cmo/digest/JOB.md", jobHead+"triggers:\n  - event: gmail\n    batch: 2m\n    inputs:\n      topic: \"{{ event.subject }}\"\n---\nDo it.\n")
	if code, out := f.run("--apply"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	r := f.routes("gmail").Routes
	if len(r) != 1 || r[0].Job != "cmo/digest" || r[0].Filter.Batch != "2m" || len(r[0].Inputs) != 1 || r[0].Inputs[0].Name != "topic" {
		t.Fatalf("routes: %+v", r)
	}
}

func TestConvertManualDisabled(t *testing.T) {
	for _, tc := range []struct{ name, keys, gets string }{
		{"manual", "manual: true", routing.GetsByHand},
		{"disabled", "disabled: true", routing.GetsOff},
		{"both", "manual: true\n    disabled: true", routing.GetsOff},
		{"none", "rooms: [r]", routing.GetsEveryMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupFixture(t)
			f.write("agents/a/AGENT.md", "---\ntriggers:\n  - event: x\n    "+tc.keys+"\n---\n")
			if code, out := f.run("--apply"); code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			if got := f.routes("x").Routes[0].Gets; got != tc.gets {
				t.Fatalf("gets %s want %s", got, tc.gets)
			}
		})
	}
}

func TestConvertKeepsSchedules(t *testing.T) {
	f := setupFixture(t)
	f.write("agents/a/AGENT.md", "---\nharness: claude\n---\n")
	f.write("agents/a/j/JOB.md", jobHead+"triggers:\n  - cron: \"0 9 * * *\" # morning\n  - event: discord\n    rooms: [r]\n  - every: 1h\n---\nbody\n")
	if code, out := f.run("--apply"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	want := jobHead + "triggers:\n  - cron: \"0 9 * * *\" # morning\n  - every: 1h\n---\nbody\n"
	if got := f.read("agents/a/j/JOB.md"); got != want {
		t.Fatalf("JOB.md:\n%s", got)
	}
}

func TestStripFlowAndWhole(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"flow all", "a: 1\ntriggers: [{event: d, rooms: [r]}]\nb: 2\n", "a: 1\nb: 2\n"},
		{"flow keep", "triggers: [{event: d}, {cron: \"* * * * *\"}]\nb: 2\n", "triggers: [{cron: \"* * * * *\"}]\nb: 2\n"},
		{"block flow items", "triggers:\n- {event: d}\n- {every: 1h}\nb: 2\n", "triggers:\n- {every: 1h}\nb: 2\n"},
		{"block all", "triggers:\n  - event: d\n    match: x\n  - event: e\nb: 2\n", "b: 2\n"},
		{"none", "triggers:\n  - cron: \"* * * * *\"\n", "triggers:\n  - cron: \"* * * * *\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := stripEventTriggers([]byte("---\n"+tc.in+"---\nbody\n"), true)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "---\n"+tc.want+"---\nbody\n" {
				t.Fatalf("got:\n%s", got)
			}
		})
	}
}

func TestConvertAssistant(t *testing.T) {
	f := setupFixture(t)
	f.write("trebi.yaml", "# top\nassistant:\n  name: Sling # note\n  triggers:\n    - event: discord\n      rooms: [r1]\n    - event: gmail\n      manual: true\nother: 1\n")
	if code, out := f.run("--apply"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if r := f.routes("discord").Routes; len(r) != 1 || !r[0].Assistant {
		t.Fatalf("discord: %+v", r)
	}
	if r := f.routes("gmail").Routes; len(r) != 1 || r[0].Gets != routing.GetsByHand {
		t.Fatalf("gmail: %+v", r)
	}
	if got := f.read("trebi.yaml"); got != "# top\nassistant:\n  name: Sling # note\nother: 1\n" {
		t.Fatalf("trebi.yaml:\n%s", got)
	}
}

func TestConvertAssistantOnlyTriggers(t *testing.T) {
	f := setupFixture(t)
	f.write("trebi.yaml", "id: ws\nassistant:\n  triggers:\n    - event: x\n")
	if code, out := f.run("--apply"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := f.read("trebi.yaml"); got != "id: ws\n" {
		t.Fatalf("trebi.yaml:\n%s", got)
	}
}

func TestConvertOrderByName(t *testing.T) {
	f := setupFixture(t)
	for _, n := range []string{"zeta", "alpha"} {
		f.write("agents/a/AGENT.md", "---\n---\n")
		f.write("agents/a/"+n+"/JOB.md", jobHead+"triggers:\n  - event: discord\n    rooms: [r1]\n    chat: reply\n---\nx\n")
	}
	if code, out := f.run("--apply"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var names []string
	for _, r := range f.routes("discord").Routes {
		names = append(names, r.Job)
	}
	if !sort.StringsAreSorted(names) || len(names) != 2 {
		t.Fatalf("order: %v", names)
	}
}

func TestConvertMergeExisting(t *testing.T) {
	f := setupFixture(t)
	f.write("agents/b/AGENT.md", "---\ntriggers:\n  - event: discord\n---\n")
	f.write("routes/discord.yaml", "# mine\nmode: on\nroutes:\n  - id: rt_0123456789\n    agent: a\n    gets: chosen\n")
	if code, out := f.run("--apply"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	file := f.routes("discord")
	if file.Mode != routing.ModeOn || len(file.Routes) != 2 || file.Routes[0].ID != "rt_0123456789" || file.Routes[1].Agent != "b" {
		t.Fatalf("file: %+v", file)
	}
	if !strings.Contains(f.read("routes/discord.yaml"), "# mine") {
		t.Fatal("comment lost")
	}
}

func TestConvertBrokenExisting(t *testing.T) {
	f := setupFixture(t)
	f.write("agents/b/AGENT.md", "---\ntriggers:\n  - event: discord\n---\n")
	f.write("routes/discord.yaml", "mode: [\n")
	before := f.tree()
	code, out := f.run("--apply")
	if code != 1 || !strings.Contains(out, "routes/discord.yaml") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !equal(before, f.tree()) {
		t.Fatal("files changed")
	}
}

func TestConvertUnroutable(t *testing.T) {
	f := setupFixture(t)
	f.write("reports/JOB.md", jobHead+"triggers:\n  - event: discord\n---\nx\n")
	before := f.tree()
	code, out := f.run("--apply")
	if code != 1 || !strings.Contains(out, "unroutable") || !equal(before, f.tree()) {
		t.Fatalf("exit %d: %s", code, out)
	}
	code, out = f.run("--apply", "--drop-unroutable")
	if code != 0 || !strings.Contains(out, "unroutable") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := f.read("reports/JOB.md"); got != jobHead+"---\nx\n" {
		t.Fatalf("JOB.md:\n%s", got)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	f := setupFixture(t)
	f.write("agents/a/AGENT.md", "---\ntriggers:\n  - event: discord\n---\n")
	before := f.tree()
	code, out := f.run()
	if code != 0 || !strings.Contains(out, "routes/discord.yaml") || !equal(before, f.tree()) {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(f.home, "backups")); err == nil {
		t.Fatal("dry run made a backup")
	}
}

func TestApplyIdempotent(t *testing.T) {
	f := setupFixture(t)
	f.write("agents/a/AGENT.md", "---\ntriggers:\n  - event: discord\n---\n")
	if code, out := f.run("--apply"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(f.home, "backups")); err != nil {
		t.Fatal("no backup")
	}
	before := f.tree()
	code, out := f.run("--apply")
	if code != 0 || !strings.Contains(out, "nothing to do") || !equal(before, f.tree()) {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestLockHeld(t *testing.T) {
	f := setupFixture(t)
	home.SetBase(f.home)
	lock, err := home.AcquireDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	f.write("agents/a/AGENT.md", "---\ntriggers:\n  - event: discord\n---\n")
	if code, out := f.run("--apply"); code != 2 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(f.read("agents/a/AGENT.md"), "event: discord") {
		t.Fatal("file changed")
	}
}

func TestNoDatabase(t *testing.T) {
	f := setupFixture(t)
	if err := os.Remove(filepath.Join(f.home, "trebi.db")); err != nil {
		t.Fatal(err)
	}
	if code, out := f.run(); code != 2 {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestOutputParses(t *testing.T) {
	f := setupFixture(t)
	f.write("agents/a/AGENT.md", "---\nharness: claude\ntriggers:\n  - event: discord\n    rooms: [r1]\n  - cron: \"0 9 * * *\"\n---\nBody.\n")
	f.write("agents/a/j/JOB.md", jobHead+"triggers:\n  - event: discord\n    rooms: [r2]\n    chat: reply\n    manual: true\n---\nDo.\n")
	if code, out := f.run("--apply"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	f.routes("discord")
	for _, rel := range []string{"agents/a/AGENT.md", "agents/a/j/JOB.md"} {
		if _, err := job.ParseFile(filepath.Join(f.ws, rel)); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
}

func TestSkipsOrphanedAndArchived(t *testing.T) {
	f := setupFixture(t)
	code, out := f.run()
	if code != 0 || strings.Contains(out, "workspace gone") || strings.Contains(out, "workspace old") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func setupFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("TREBI_SERVICE_CMD", "false")
	t.Cleanup(func() { home.SetBase("") })
	f := &fixture{t: t, home: t.TempDir(), ws: filepath.Join(t.TempDir(), "sling")}
	if err := os.MkdirAll(f.ws, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(f.home, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, name TEXT, path TEXT, orphaned INTEGER NOT NULL DEFAULT 0, archived_at TEXT)`,
		`INSERT INTO workspaces (id, name, path) VALUES ('ws1', 'sling', '` + f.ws + `')`,
		`INSERT INTO workspaces (id, name, path, orphaned) VALUES ('ws2', 'gone', '/nowhere', 1)`,
		`INSERT INTO workspaces (id, name, path, archived_at) VALUES ('ws3', 'old', '/nowhere2', '2026-01-01')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
