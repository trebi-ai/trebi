package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"gopkg.in/yaml.v3"
)

const busWait = 600 * time.Millisecond

func TestWorkspaceCreateBootstrap(t *testing.T) {
	_, base := startTestServer(t)
	parent := t.TempDir()
	fresh := filepath.Join(parent, "fresh")

	code, ws, _ := doJSON(t, "POST", base+"/v1/workspaces", map[string]any{
		"path": fresh, "name": "fresh", "create": true,
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, ws)
	}
	if _, err := os.Stat(filepath.Join(fresh, "trebi.yaml")); err != nil {
		t.Fatal(err)
	}

	code, _, _ = doJSON(t, "POST", base+"/v1/workspaces", map[string]any{
		"path": fresh, "name": "fresh", "create": true,
	})
	if code != 201 {
		t.Fatalf("idempotent %d", code)
	}
	frames := collectBus(t, base, "workspace.created", busWait)
	if n := countTopic(frames, "workspace.created"); n != 1 {
		t.Fatalf("workspace.created count=%d want 1", n)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/workspaces", map[string]any{
		"path": filepath.Join(fresh, "nested"), "name": "nested", "create": true,
	})
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("nested %d %v", code, body)
	}

	filePath := filepath.Join(parent, "a-file")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/workspaces", map[string]any{
		"path": filepath.Join(filePath, "sub"), "name": "bad", "create": true,
	})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("ancestor file %d %v", code, body)
	}
}

func TestWorkspacePathlessCreate(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	wsRoot := t.TempDir()
	s := api.New(st, api.Options{WorkspacesRoot: wsRoot})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr

	code, ws, _ := doJSON(t, "POST", base+"/v1/workspaces", map[string]any{
		"name": "reports", "create": true,
	})
	if code != 201 {
		t.Fatalf("pathless %d %v", code, ws)
	}
	want := filepath.Join(wsRoot, "reports")
	if ws["path"] != want {
		t.Fatalf("path %v want %s", ws["path"], want)
	}
	if _, err := os.Stat(filepath.Join(want, "trebi.yaml")); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/workspaces", map[string]any{
		"name": "nope", "create": false,
	})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("no create %d %v", code, body)
	}
}

func TestWorkspacePathlessMissingRoot(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/workspaces", map[string]any{
		"name": "reports", "create": true,
	})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("missing root %d %v", code, body)
	}
	if !strings.Contains(errMsg(body), "workspaces.root") {
		t.Fatalf("message: %s", errMsg(body))
	}
}

func TestJobPurge(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "purge-ws")
	createTestJob(t, base, wsID, "agents/ops/report")
	code, _, _ := doRaw(t, "PUT", base+"/v1/jobs/agents%2Fops%2Freport/files/render.py?workspace="+wsID, nil, []byte("print(1)\n"))
	if code != 201 {
		t.Fatalf("put support %d", code)
	}

	code, body, _ := doJSON(t, "DELETE", base+"/v1/jobs/agents%2Fops%2Freport?workspace="+wsID+"&purge=1&confirm=1", nil)
	if code != 200 {
		t.Fatalf("purge %d %v", code, body)
	}
	if body["purged"] != true {
		t.Fatalf("body %v", body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "ops", "report")); !os.IsNotExist(err) {
		t.Fatalf("folder remains: %v", err)
	}
	frames := collectBus(t, base, "file.deleted", busWait)
	found := false
	for _, f := range frames {
		attrs, _ := f["attrs"].(map[string]any)
		if attrs["purge"] == "true" && attrs["path"] == "agents/ops/report" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing purge file.deleted: %v", topicsOf(frames))
	}
}

func TestJobPurgeGuards(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "purge-g")
	md := []byte("---\nharness: fake\n---\n\nDo work.\n")

	code, _, _ := doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/JOB.md", nil, md)
	if code != 201 {
		t.Fatalf("root JOB.md %d", code)
	}
	jobName := filepath.Base(wsDir)
	code, body, _ := doJSON(t, "DELETE", base+"/v1/jobs/"+jobName+"?workspace="+wsID+"&purge=1&confirm=1", nil)
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("root purge %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "JOB.md")); err != nil {
		t.Fatalf("root JOB.md removed: %v", err)
	}

	code, _, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/outer/JOB.md", nil, md)
	if code != 201 {
		t.Fatalf("outer %d", code)
	}
	code, _, _ = doRaw(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/outer/inner/JOB.md", nil, md)
	if code != 201 {
		t.Fatalf("inner %d", code)
	}
	code, body, _ = doJSON(t, "DELETE", base+"/v1/jobs/outer?workspace="+wsID+"&purge=1&confirm=1", nil)
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("nested purge %d %v", code, body)
	}
	if !strings.Contains(errMsg(body), "inner") {
		t.Fatalf("blockers: %s", errMsg(body))
	}
}

func TestJobPurgeStaleIfMatch(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "purge-if")
	createTestJob(t, base, wsID, "agents/ops/report")
	code, _, _ := doRaw(t, "PUT", base+"/v1/jobs/agents%2Fops%2Freport/files/a.py?workspace="+wsID, nil, []byte("x"))
	if code != 201 {
		t.Fatalf("put %d", code)
	}

	reqHeaders := map[string]string{"If-Match": "deadbeef"}
	code, body, _ := doRaw(t, "DELETE", base+"/v1/jobs/agents%2Fops%2Freport?workspace="+wsID+"&purge=1&confirm=1", reqHeaders, nil)
	if code != 412 || errCode(body) != "precondition_failed" {
		t.Fatalf("stale %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "ops", "report", "a.py")); err != nil {
		t.Fatalf("support file removed on stale purge: %v", err)
	}
}

func TestJobDeleteFilesRemaining(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "remain-ws")
	createTestJob(t, base, wsID, "agents/ops/report")
	code, _, _ := doRaw(t, "PUT", base+"/v1/jobs/agents%2Fops%2Freport/files/a.py?workspace="+wsID, nil, []byte("x"))
	if code != 201 {
		t.Fatalf("put %d", code)
	}
	code, body, _ := doJSON(t, "DELETE", base+"/v1/jobs/agents%2Fops%2Freport?workspace="+wsID+"&confirm=1", nil)
	if code != 200 {
		t.Fatalf("delete %d %v", code, body)
	}
	if body["files_remaining"] != float64(1) {
		t.Fatalf("files_remaining %v", body["files_remaining"])
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "ops", "report", "a.py")); err != nil {
		t.Fatalf("support file gone: %v", err)
	}
}

func TestSkillFiles(t *testing.T) {
	_, base := startTestServer(t)
	code, sk, _ := doJSON(t, "POST", base+"/v1/skills", map[string]any{
		"name": "bundle", "skill_md": "# Bundle\n\nSkill body.\n",
	})
	if code != 201 {
		t.Fatalf("create skill %d %v", code, sk)
	}
	oldHash, _ := sk["hash"].(string)

	code, resp, _ := doRaw(t, "PUT", base+"/v1/skills/bundle/files/helper.py", nil, []byte("print(1)\n"))
	if code != 201 || resp["path"] != "helper.py" {
		t.Fatalf("put %d %v", code, resp)
	}

	code, got, _ := doJSON(t, "GET", base+"/v1/skills/bundle", nil)
	if code != 200 {
		t.Fatalf("get skill %d %v", code, got)
	}
	if got["hash"] == oldHash {
		t.Fatal("hash unchanged")
	}
	scripts, _ := got["scripts"].([]any)
	if len(scripts) != 1 || scripts[0] != "helper.py" {
		t.Fatalf("scripts %v", scripts)
	}

	code, body, _ := doRaw(t, "PUT", base+"/v1/skills/bundle/files/SKILL.md", nil, []byte("# no\n"))
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("SKILL.md put %d %v", code, body)
	}
	if !strings.Contains(errMsg(body), "trebi skill update") {
		t.Fatalf("message: %s", errMsg(body))
	}
	code, _, _ = doRaw(t, "GET", base+"/v1/skills/bundle/files/SKILL.md", nil, nil)
	if code != 200 {
		t.Fatalf("SKILL.md get %d", code)
	}

	code, body, _ = doRaw(t, "PUT", base+"/v1/skills/bundle/files/%2e%2e%2fx", nil, []byte("x"))
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("jail %d %v", code, body)
	}

	_, got, _ = doJSON(t, "GET", base+"/v1/skills/bundle", nil)
	data, err := os.ReadFile(filepath.Join(got["path"].(string), "helper.py"))
	if err != nil || string(data) != "print(1)\n" {
		t.Fatalf("skill folder: %q %v", data, err)
	}

	frames := collectBus(t, base, "skill.updated", busWait)
	if len(frames) == 0 {
		t.Fatal("no skill.updated")
	}
}

func TestSkillFilesProvenanceDeny(t *testing.T) {
	_, base := startTestServer(t)
	code, _, _ := doJSON(t, "POST", base+"/v1/skills", map[string]any{
		"name": "upstream", "skill_md": "# Up\n", "provenance": "git",
	})
	if code != 201 {
		t.Fatalf("create %d", code)
	}
	code, body, _ := doRaw(t, "PUT", base+"/v1/skills/upstream/files/a.py", nil, []byte("x"))
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("git write %d %v", code, body)
	}
}

func TestWorkspacePatchGitEnabled(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "settings")
	code, body, _ := doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID, map[string]any{
		"git_enabled": true,
	})
	if code != 200 {
		t.Fatalf("patch %d %v", code, body)
	}
	if body["git_enabled"] != true {
		t.Fatalf("git_enabled %v", body["git_enabled"])
	}
	rev, _ := body["revision"].(float64)
	if rev < 2 {
		t.Fatalf("revision %v", body["revision"])
	}
	raw, err := os.ReadFile(filepath.Join(wsDir, "trebi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var y struct {
		GitEnabled bool   `yaml:"git_enabled"`
		Name       string `yaml:"name"`
	}
	if err := yaml.Unmarshal(raw, &y); err != nil {
		t.Fatal(err)
	}
	if !y.GitEnabled || y.Name != "settings" {
		t.Fatalf("yaml %+v", y)
	}
}

func countTopic(frames []map[string]any, topic string) int {
	n := 0
	for _, f := range frames {
		if f["topic"] == topic {
			n++
		}
	}
	return n
}
