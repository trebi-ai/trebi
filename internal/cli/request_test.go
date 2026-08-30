package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// urfaveCommand is a local alias so registration tests stay readable.
type urfaveCommand = cli.Command

func TestLaunchHTTPPath(t *testing.T) {
	t.Parallel()
	if !launchHTTPPath("/v1/runs") || !launchHTTPPath("/v1/runs?workspace=ws") {
		t.Fatal("create")
	}
	if !launchHTTPPath("/v1/runs/01ABC/retry") || !launchHTTPPath("/v1/runs/01ABC/replay") {
		t.Fatal("retry/replay")
	}
	if launchHTTPPath("/v1/jobs") || launchHTTPPath("/v1/runs/01ABC") {
		t.Fatal("non-launch")
	}
}

func TestBuildRunsCreate(t *testing.T) {
	t.Parallel()
	req := BuildRunsCreate("demo", "ws1", "extra text", true)
	if req.Method != "POST" || req.Path != "/v1/runs" {
		t.Fatalf("method/path: %+v", req)
	}
	b, _ := json.Marshal(req.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["job"] != "demo" || m["workspace"] != "ws1" {
		t.Fatalf("body: %s", b)
	}
	if m["prompt_override"] != "extra text" {
		t.Fatalf("prompt: %s", b)
	}
	if m["dry_run"] != true {
		t.Fatalf("dry_run: %s", b)
	}
	if m["reason"] != "manual" {
		t.Fatalf("reason: %s", b)
	}
}

func TestBuildRunTranscript(t *testing.T) {
	t.Parallel()
	req := BuildRunTranscript("01ABC", false)
	if req.Method != "GET" || req.Path != "/v1/runs/01ABC/transcript" {
		t.Fatalf("parsed: %+v", req)
	}
	raw := BuildRunTranscript("01ABC", true)
	if raw.Path != "/v1/runs/01ABC/transcript?raw=1" {
		t.Fatalf("raw: %s", raw.Path)
	}
}

func TestBuildRunsListPath(t *testing.T) {
	t.Parallel()
	path := BuildRunsListPath("running", "demo", "ws1", "job", "01ABC", "claude", "sonnet", 20)
	if !strings.Contains(path, "/v1/runs?") {
		t.Fatalf("path: %s", path)
	}
	for _, want := range []string{
		"status=running", "job=demo", "workspace=ws1", "kind=job",
		"cursor=01ABC", "harness=claude", "model=sonnet", "limit=20",
	} {
		if !strings.Contains(path, want) {
			t.Fatalf("missing %q in %s", want, path)
		}
	}
}

func TestMatchConversation(t *testing.T) {
	t.Parallel()
	if !MatchConversation("thread-abc", "ABC") {
		t.Fatal("expected case-insensitive match")
	}
	if MatchConversation("x", "y") {
		t.Fatal("expected no match")
	}
	if !MatchConversation("any", "") {
		t.Fatal("empty substr matches all")
	}
}

func TestBuildFilePaths(t *testing.T) {
	t.Parallel()
	list := BuildFileListPath("ws1", "reports/", "reports/a.py", 20)
	if !strings.HasPrefix(list, "/v1/workspaces/ws1/files?") {
		t.Fatalf("list %s", list)
	}
	for _, want := range []string{"prefix=reports", "cursor=reports", "limit=20"} {
		if !strings.Contains(list, want) {
			t.Fatalf("missing %q in %s", want, list)
		}
	}
	get := BuildFileGetPath("ws1", "reports/render.py", true)
	if !strings.Contains(get, "/files/reports/render.py") || !strings.Contains(get, "stat=1") {
		t.Fatalf("get %s", get)
	}
	if p := BuildFilePutPath("ws1", "a/b.py"); p != "/v1/workspaces/ws1/files/a/b.py" {
		t.Fatalf("put %s", p)
	}
	if p := BuildFileDeletePath("ws1", "a/b.py"); p != "/v1/workspaces/ws1/files/a/b.py" {
		t.Fatalf("delete %s", p)
	}
}

func TestBuildJobFilePaths(t *testing.T) {
	t.Parallel()
	list := BuildJobFileListPath("report", "ws1", "tools/", "tools/a.py", 20)
	if !strings.HasPrefix(list, "/v1/jobs/report/files?") {
		t.Fatalf("list %s", list)
	}
	for _, want := range []string{"prefix=tools", "cursor=tools", "limit=20", "workspace=ws1"} {
		if !strings.Contains(list, want) {
			t.Fatalf("missing %q in %s", want, list)
		}
	}
	get := BuildJobFileGetPath("report", "ws1", "render.py", true)
	if !strings.Contains(get, "/v1/jobs/report/files/render.py") || !strings.Contains(get, "stat=1") || !strings.Contains(get, "workspace=ws1") {
		t.Fatalf("get %s", get)
	}
	if p := BuildJobFilePutPath("report", "ws1", "a/b.py"); p != "/v1/jobs/report/files/a/b.py?workspace=ws1" {
		t.Fatalf("put %s", p)
	}
	if p := BuildJobFileDeletePath("report", "", "a/b.py"); p != "/v1/jobs/report/files/a/b.py" {
		t.Fatalf("delete %s", p)
	}
}

func TestBuildSkillFilePaths(t *testing.T) {
	t.Parallel()
	list := BuildSkillFileListPath("demo", "scripts/", "scripts/a.py", 20)
	if !strings.HasPrefix(list, "/v1/skills/demo/files?") {
		t.Fatalf("list %s", list)
	}
	get := BuildSkillFileGetPath("demo", "helper.py", true)
	if !strings.Contains(get, "/v1/skills/demo/files/helper.py") || !strings.Contains(get, "stat=1") {
		t.Fatalf("get %s", get)
	}
	if p := BuildSkillFilePutPath("demo", "a/b.py"); p != "/v1/skills/demo/files/a/b.py" {
		t.Fatalf("put %s", p)
	}
	if p := BuildSkillFileDeletePath("demo", "a/b.py"); p != "/v1/skills/demo/files/a/b.py" {
		t.Fatalf("delete %s", p)
	}
}

func TestBuildWorkspaceAdd(t *testing.T) {
	t.Parallel()
	req := BuildWorkspaceAdd("/tmp/ws", "reports", true)
	if req.Method != "POST" || req.Path != "/v1/workspaces" {
		t.Fatalf("%+v", req)
	}
	body, _ := req.Body.(map[string]any)
	if body["path"] != "/tmp/ws" || body["name"] != "reports" || body["create"] != true {
		t.Fatalf("body %+v", body)
	}
	req = BuildWorkspaceAdd("", "reports", true)
	body, _ = req.Body.(map[string]any)
	if _, ok := body["path"]; ok {
		t.Fatalf("pathless should omit path: %+v", body)
	}
}

func TestBuildJobDelete(t *testing.T) {
	t.Parallel()
	req := BuildJobDelete("demo", "ws1", false)
	if req.Method != "DELETE" {
		t.Fatalf("method %s", req.Method)
	}
	if !strings.HasPrefix(req.Path, "/v1/jobs/demo") || !strings.Contains(req.Path, "workspace=ws1") {
		t.Fatalf("path %s", req.Path)
	}
	req = BuildJobDelete("demo", "ws1", true)
	if !strings.Contains(req.Path, "purge=1") {
		t.Fatalf("purge path %s", req.Path)
	}
}

func TestPlanJobSchedule(t *testing.T) {
	t.Parallel()
	p, err := PlanJobSchedule("j", "ws", "enable")
	if err != nil || p.Mode != "unpause" || p.HTTP.Path == "" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = PlanJobSchedule("j", "ws", "disable")
	if err != nil || p.Mode != "pause" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = PlanJobSchedule("j", "ws", "@every 30m")
	if err != nil || p.Mode != "put_markdown" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestApplyScheduleMarkdown(t *testing.T) {
	t.Parallel()
	src := "---\nharness: claude\ntriggers:\n  - event: mail\n    match: true\n  - cron: \"0 9 * * *\"\n---\n\nBody.\n"
	out, err := ApplyScheduleMarkdown(src, "@every 30m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "every: 30m") && !strings.Contains(out, "every: \"30m\"") {
		// yaml may quote or not
		if !strings.Contains(out, "30m") {
			t.Fatalf("missing every: %s", out)
		}
	}
	if !strings.Contains(out, "event: mail") {
		t.Fatalf("event trigger lost: %s", out)
	}
	if strings.Contains(out, "0 9 * * *") {
		t.Fatalf("old cron should be gone: %s", out)
	}
	if !strings.Contains(out, "Body.") {
		t.Fatalf("body lost: %s", out)
	}
	// multi cron (comma+space separator)
	out2, err := ApplyScheduleMarkdown(src, "0 8 * * 1, 50 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "0 8 * * 1") || !strings.Contains(out2, "50 * * * *") {
		t.Fatalf("crons: %s", out2)
	}
	// DOW list commas must not split a single cron
	out3, err := ApplyScheduleMarkdown(src, "0 9 * * 1,3,5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out3, "1,3,5") {
		t.Fatalf("dow list broken: %s", out3)
	}
}

func TestAppendExtraPrompt(t *testing.T) {
	t.Parallel()
	got := AppendExtraPrompt("base body", "extra")
	if got != "base body\nextra" {
		t.Fatalf("%q", got)
	}
	if AppendExtraPrompt("", "only") != "only" {
		t.Fatal("empty base")
	}
}

func TestUnwrapConfigMap(t *testing.T) {
	t.Parallel()
	inner := map[string]any{"defaults": map[string]any{"command": "claude"}}
	wrapped := map[string]any{"config": inner}
	got := unwrapConfigMap(wrapped)
	if fmt.Sprint(got["defaults"]) == "" {
		t.Fatalf("%v", got)
	}
	if d, ok := got["defaults"].(map[string]any); !ok || d["command"] != "claude" {
		t.Fatalf("%v", got)
	}
	// already flat
	flat := unwrapConfigMap(inner)
	if flat["defaults"] == nil {
		t.Fatal("flat lost")
	}
}

func TestFetchRunLogText(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			_, _ = w.Write([]byte(`{"log":"line1\nline2\n","source":"file"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	client := &APIClient{Base: srv.URL, Token: "t", HTTP: srv.Client()}
	text, err := fetchRunLogText(client, "01ABC")
	if err != nil {
		t.Fatal(err)
	}
	if text != "line1\nline2\n" {
		t.Fatalf("%q", text)
	}
}

func TestSplitScheduleValues(t *testing.T) {
	t.Parallel()
	parts := splitScheduleValues("0 9 * * 1,3,5")
	if len(parts) != 1 || parts[0] != "0 9 * * 1,3,5" {
		t.Fatalf("%v", parts)
	}
	parts = splitScheduleValues("0 8 * * 1, 50 * * * *")
	if len(parts) != 2 {
		t.Fatalf("%v", parts)
	}
}

func TestBuildIntegrationRestart(t *testing.T) {
	t.Parallel()
	req := BuildIntegrationRestart("echo-src")
	if req.Method != "POST" || req.Path != "/v1/integrations/echo-src/restart" {
		t.Fatalf("%+v", req)
	}
}

func TestBuildIntegrationSet(t *testing.T) {
	t.Parallel()
	req := BuildIntegrationSet("echo", "echo hi", "json", "stream", "/tmp", map[string]string{"A": "1"})
	if req.Method != "PUT" || req.Path != "/v1/integrations/echo" {
		t.Fatalf("%+v", req)
	}
	b, _ := json.Marshal(req.Body)
	s := string(b)
	for _, want := range []string{`"name":"echo"`, `"command":"echo hi"`, `"format":"json"`, `"cwd":"/tmp"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
}

func TestBuildConfigSet(t *testing.T) {
	t.Parallel()
	req := BuildConfigSet("defaults.command", "claude")
	if req.Method != "PATCH" || req.Path != "/v1/config" {
		t.Fatalf("%+v", req)
	}
	b, _ := json.Marshal(req.Body)
	if !strings.Contains(string(b), `"defaults.command":"claude"`) {
		t.Fatalf("body %s", b)
	}
}

func TestBuildConfigBridgeSet(t *testing.T) {
	t.Parallel()
	req, err := BuildConfigBridgeSet("claude", "skills", "inherit")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(req.Body)
	if !strings.Contains(string(b), "tools.claude.bridge.resources.skills") {
		t.Fatalf("%s", b)
	}
	req, err = BuildConfigBridgeSet("", "mcp", "false")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(req.Body)
	if !strings.Contains(string(b), "tools.bridge.resources.mcp") {
		t.Fatalf("%s", b)
	}
}

func TestBuildHarnessEnsureAndAuth(t *testing.T) {
	t.Parallel()
	e := BuildHarnessEnsure("claude")
	if e.Path != "/v1/harnesses/claude/ensure" || e.Method != "POST" {
		t.Fatalf("%+v", e)
	}
	a := BuildHarnessAuthGet("claude", false)
	if a.Path != "/v1/harnesses/claude/auth" || a.Method != "GET" {
		t.Fatalf("%+v", a)
	}
	live := BuildHarnessAuthGet("claude", true)
	if live.Path != "/v1/harnesses/claude/auth?live=1" {
		t.Fatalf("%+v", live)
	}
	p := BuildHarnessLoginStart("claude", "")
	if p.Method != "POST" || p.Path != "/v1/harnesses/claude/login" {
		t.Fatalf("%+v", p)
	}
	tok := BuildHarnessLoginToken("claude", "sk")
	if tok.Body.(map[string]any)["token"] != "sk" {
		t.Fatalf("%+v", tok)
	}
	code := BuildHarnessLoginCode("claude", "ABCD")
	if code.Body.(map[string]any)["code"] != "ABCD" {
		t.Fatalf("%+v", code)
	}
	lo := BuildHarnessLogout("claude", "shared", true)
	if lo.Method != "POST" || lo.Path != "/v1/harnesses/claude/logout" {
		t.Fatalf("%+v", lo)
	}
	md := BuildHarnessPatch("claude", "separate")
	if md.Method != "PATCH" {
		t.Fatalf("%+v", md)
	}
}

// TestDoHTTPRoundTrip drives shipped client against a fake HTTP server for key paths.
func TestDoHTTPRoundTrip(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/runs":
			_, _ = w.Write([]byte(`{"id":"01TEST","status":"running"}`))
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			_, _ = w.Write([]byte(`{"deleted":"demo"}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/restart"):
			_, _ = w.Write([]byte(`{"status":"listening"}`))
		case r.Method == "PATCH" && r.URL.Path == "/v1/config":
			_, _ = w.Write([]byte(`{"config":{}}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/ensure"):
			_, _ = w.Write([]byte(`{"name":"claude","ensured":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := &APIClient{Base: srv.URL, Token: "t", HTTP: srv.Client()}

	// run dry_run + prompt
	req := BuildRunsCreate("demo", "ws", "hi", true)
	var out map[string]any
	if err := client.DoHTTP(req, &out); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotPath != "/v1/runs" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"dry_run":true`) || !strings.Contains(gotBody, `"prompt_override":"hi"`) {
		t.Fatalf("body %s", gotBody)
	}
	if out["id"] != "01TEST" {
		t.Fatalf("out %+v", out)
	}

	// job delete
	req = BuildJobDelete("demo", "ws1", false)
	if err := client.DoHTTP(req, nil); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "DELETE" || !strings.Contains(gotPath, "/v1/jobs/demo") || !strings.Contains(gotPath, "workspace=ws1") {
		t.Fatalf("delete %s %s", gotMethod, gotPath)
	}

	// integration restart
	req = BuildIntegrationRestart("mail")
	if err := client.DoHTTP(req, nil); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotPath != "/v1/integrations/mail/restart" {
		t.Fatalf("restart %s %s", gotMethod, gotPath)
	}

	// config set
	req = BuildConfigSet("defaults.command", "grok")
	if err := client.DoHTTP(req, nil); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "PATCH" || gotPath != "/v1/config" {
		t.Fatalf("config %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"defaults.command":"grok"`) {
		t.Fatalf("config body %s", gotBody)
	}

	// harness ensure
	req = BuildHarnessEnsure("claude")
	if err := client.DoHTTP(req, &out); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotPath != "/v1/harnesses/claude/ensure" {
		t.Fatalf("ensure %s %s", gotMethod, gotPath)
	}
}

func TestCommandRegistration(t *testing.T) {
	t.Parallel()
	app := NewApp()
	names := map[string]bool{}
	for _, c := range app.Commands {
		names[c.Name] = true
	}
	for _, want := range []string{"runs", "job", "harness", "integration", "config", "service", "conversation", "events", "queue", "rules", "bus", "doctor"} {
		if !names[want] {
			t.Fatalf("missing top-level %s", want)
		}
	}
	if names["run"] {
		t.Fatal("top-level run should be removed (use trebi job run)")
	}
	if names["schedule"] {
		t.Fatal("top-level schedule should be removed (use trebi job list --schedule)")
	}
	if names["list"] {
		t.Fatal("top-level list should be removed (use trebi job list)")
	}
	if names["approvals"] {
		t.Fatal("top-level approvals should be removed (use trebi runs approvals)")
	}
	if names["version"] {
		t.Fatal("top-level version should be removed (use --version)")
	}
	svc := findCmd(app.Commands, "service")
	for _, want := range []string{"install", "uninstall", "start", "stop", "restart", "status", "logs", "daemon", "shutdown"} {
		if findCmd(svc.Commands, want) == nil {
			t.Fatalf("service missing %s", want)
		}
	}
	job := findCmd(app.Commands, "job")
	if job == nil {
		t.Fatal("job missing")
	}
	for _, want := range []string{"delete", "schedule", "status", "tail", "list", "pause", "unpause", "run"} {
		if findCmd(job.Commands, want) == nil {
			t.Fatalf("job missing %s", want)
		}
	}
	jobList := findCmd(job.Commands, "list")
	if !hasFlag(jobList, "schedule") {
		t.Fatal("job list missing --schedule")
	}
	if !hasFlag(jobList, "kind") {
		t.Fatal("job list missing --kind")
	}
	h := findCmd(app.Commands, "harness")
	for _, want := range []string{"list", "ensure", "auth"} {
		if findCmd(h.Commands, want) == nil {
			t.Fatalf("harness missing %s", want)
		}
	}
	auth := findCmd(h.Commands, "auth")
	for _, want := range []string{"status", "login", "logout", "mode"} {
		if findCmd(auth.Commands, want) == nil {
			t.Fatalf("harness auth missing %s", want)
		}
	}
	if findCmd(auth.Commands, "mark") != nil {
		t.Fatal("harness auth mark should be gone")
	}
	integ := findCmd(app.Commands, "integration")
	for _, want := range []string{"list", "show", "set", "remove", "stop", "restart", "logs", "test", "connect", "disconnect", "cursor"} {
		if findCmd(integ.Commands, want) == nil {
			t.Fatalf("integration missing %s", want)
		}
	}
	cur := findCmd(integ.Commands, "cursor")
	for _, want := range []string{"show", "set", "reset"} {
		if findCmd(cur.Commands, want) == nil {
			t.Fatalf("integration cursor missing %s", want)
		}
	}
	if findCmd(integ.Commands, "queue") == nil || !findCmd(integ.Commands, "queue").Hidden {
		t.Fatal("integration queue should remain as a hidden alias")
	}
	cfg := findCmd(app.Commands, "config")
	for _, want := range []string{"show", "set", "validate", "path", "bridge"} {
		if findCmd(cfg.Commands, want) == nil {
			t.Fatalf("config missing %s", want)
		}
	}
	run := findCmd(job.Commands, "run")
	if !hasFlag(run, "extra-prompt") || !hasFlag(run, "test") || !hasFlag(run, "attach") || !hasFlag(run, "workspace") || !hasFlag(run, "json") {
		t.Fatal("job run flags")
	}
	runsList := findCmd(findCmd(app.Commands, "runs").Commands, "list")
	for _, f := range []string{"status", "job", "kind", "workspace", "harness", "model", "conversation", "limit"} {
		if !hasFlag(runsList, f) {
			t.Fatalf("runs list missing flag %s", f)
		}
	}
	if hasFlag(runsList, "assistant") {
		t.Fatal("runs list should not have --assistant (use --kind assistant)")
	}
	if findCmd(findCmd(app.Commands, "runs").Commands, "attach") == nil {
		t.Fatal("runs attach missing")
	}
	if findCmd(findCmd(app.Commands, "runs").Commands, "retry") == nil {
		t.Fatal("runs retry missing")
	}
	runs := findCmd(app.Commands, "runs")
	msg := findCmd(runs.Commands, "message")
	if msg == nil {
		t.Fatal("runs message missing")
	}
	if msg.ArgsUsage != "<id> <text...>" {
		t.Fatalf("runs message ArgsUsage %q", msg.ArgsUsage)
	}
	for _, want := range []string{"stats", "agents", "approvals"} {
		if findCmd(runs.Commands, want) == nil {
			t.Fatalf("runs missing %s", want)
		}
	}
	ev := findCmd(app.Commands, "events")
	if findCmd(ev.Commands, "retrigger") == nil {
		t.Fatal("events retrigger missing")
	}
	mem := findCmd(findCmd(app.Commands, "job").Commands, "memory")
	for _, want := range []string{"show", "add", "remove", "set", "clear"} {
		if findCmd(mem.Commands, want) == nil {
			t.Fatalf("job memory missing %s", want)
		}
	}
	if !hasFlag(findCmd(mem.Commands, "clear"), "force") {
		t.Fatal("job memory clear missing --force")
	}
	if findCmd(findCmd(app.Commands, "skill").Commands, "update") == nil {
		t.Fatal("skill update missing")
	}
	doc := findCmd(app.Commands, "doctor")
	if !hasFlag(doc, "fix") || !hasFlag(doc, "json") {
		t.Fatal("doctor missing --fix/--json")
	}
	if findCmd(doc.Commands, "fix") == nil {
		t.Fatal("doctor fix missing")
	}
	bus := findCmd(app.Commands, "bus")
	for _, f := range []string{"topics", "cursor", "replay", "since", "entity", "id", "action", "limit", "json"} {
		if !hasFlag(bus, f) {
			t.Fatalf("bus missing flag %s", f)
		}
	}
	cfgShow := findCmd(findCmd(app.Commands, "config").Commands, "show")
	if !hasFlag(cfgShow, "yaml") || !hasFlag(cfgShow, "tree") {
		t.Fatal("config show missing --yaml/--tree")
	}
	asst := findCmd(app.Commands, "assistant")
	for _, want := range []string{"show", "wake", "history", "say", "chat"} {
		if findCmd(asst.Commands, want) == nil {
			t.Fatalf("assistant missing %s", want)
		}
	}
	conv := findCmd(app.Commands, "conversation")
	for _, want := range []string{"list", "show", "messages", "new", "switch"} {
		if findCmd(conv.Commands, want) == nil {
			t.Fatalf("conversation missing %s", want)
		}
	}
	if !hasFlag(findCmd(asst.Commands, "wake"), "attach") {
		t.Fatal("assistant wake missing --attach")
	}
	hist := findCmd(asst.Commands, "history")
	for _, f := range []string{"status", "job", "harness", "model", "conversation", "limit", "json"} {
		if !hasFlag(hist, f) {
			t.Fatalf("assistant history missing flag %s", f)
		}
	}
	if findCmd(app.Commands, "remote") != nil {
		t.Fatal("trebi remote should be removed")
	}
	ws := findCmd(app.Commands, "workspace")
	for _, want := range []string{"list", "add", "use", "current", "unlink", "remove", "rename"} {
		if findCmd(ws.Commands, want) == nil {
			t.Fatalf("workspace missing %s", want)
		}
	}
	if !hasFlag(findCmd(ws.Commands, "remove"), "force") {
		t.Fatal("workspace remove missing --force")
	}
	asstMem := findCmd(findCmd(app.Commands, "assistant").Commands, "memory")
	for _, want := range []string{"show", "add", "remove", "set", "clear"} {
		if findCmd(asstMem.Commands, want) == nil {
			t.Fatalf("assistant memory missing %s", want)
		}
	}
	rules := findCmd(app.Commands, "rules")
	for _, want := range []string{"test", "fire"} {
		if findCmd(rules.Commands, want) == nil {
			t.Fatalf("rules missing %s", want)
		}
	}
	add := findCmd(ws.Commands, "add")
	if !hasFlag(add, "url") || !hasFlag(add, "token") || !hasFlag(add, "id") {
		t.Fatal("workspace add missing remote flags")
	}
}

func TestHTTPToWS(t *testing.T) {
	t.Parallel()
	got, err := httpToWS("https://host:7433/v1/runs/x/attach")
	if err != nil || got != "wss://host:7433/v1/runs/x/attach" {
		t.Fatalf("%q %v", got, err)
	}
	got, err = httpToWS("http://127.0.0.1:9/v1/runs/x/attach")
	if err != nil || got != "ws://127.0.0.1:9/v1/runs/x/attach" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestBuildRunsKill(t *testing.T) {
	t.Parallel()
	req := BuildRunsKill("01ABC")
	if req.Method != "POST" || req.Path != "/v1/runs/01ABC/kill" {
		t.Fatalf("%+v", req)
	}
}

func TestBuildContractGapRequests(t *testing.T) {
	t.Parallel()
	req := BuildRunsRetry("01ABC")
	if req.Method != "POST" || req.Path != "/v1/runs/01ABC/retry" {
		t.Fatalf("retry %+v", req)
	}
	req = BuildEventsRetrigger("01EV")
	if req.Method != "POST" || req.Path != "/v1/events/01EV/retrigger" {
		t.Fatalf("retrigger %+v", req)
	}
	req = BuildConversationGet("01C")
	if req.Method != "GET" || req.Path != "/v1/conversations/01C" {
		t.Fatalf("conv get %+v", req)
	}
	req = BuildServiceShutdown()
	if req.Method != "POST" || req.Path != "/v1/system/shutdown" {
		t.Fatalf("shutdown %+v", req)
	}
	req = BuildJobMemoryEntryAdd("demo", "ws1", "2026-08-28", "note")
	if req.Method != "POST" || !strings.Contains(req.Path, "/v1/jobs/demo/memory/entries") {
		t.Fatalf("mem add %+v", req)
	}
	req = BuildJobMemoryEntryDelete("demo", "ws1", "e1")
	if req.Method != "DELETE" || !strings.Contains(req.Path, "/memory/entries/e1") {
		t.Fatalf("mem del %+v", req)
	}
	req = BuildAssistantMemoryEntryAdd("ws1", "", "note")
	if req.Method != "POST" || !strings.Contains(req.Path, "/assistant/memory/entries") {
		t.Fatalf("asst add %+v", req)
	}
	req = BuildAssistantMemoryEntryDelete("ws1", "e1")
	if req.Method != "DELETE" || !strings.Contains(req.Path, "/entries/e1") {
		t.Fatalf("asst del %+v", req)
	}
	path := BuildEventsListPath("01C", 10)
	if !strings.Contains(path, "cursor=01C") || !strings.Contains(path, "limit=10") {
		t.Fatalf("events list %s", path)
	}
	path = BuildQueueListPath("pending", "01Q", 5)
	if !strings.Contains(path, "status=pending") || !strings.Contains(path, "cursor=01Q") {
		t.Fatalf("queue list %s", path)
	}
}

func TestBuildInboxAndAlerts(t *testing.T) {
	t.Parallel()
	path := BuildInboxListPath("unread", "failure", "01ABC", 20)
	if !strings.Contains(path, "/v1/inbox?") {
		t.Fatalf("path %s", path)
	}
	for _, want := range []string{"status=unread", "kind=failure", "cursor=01ABC", "limit=20"} {
		if !strings.Contains(path, want) {
			t.Fatalf("missing %q in %s", want, path)
		}
	}
	got := BuildInboxGet("01XYZ")
	if got.Method != "GET" || got.Path != "/v1/inbox/01XYZ" {
		t.Fatalf("%+v", got)
	}
	got = BuildInboxRead("01XYZ")
	if got.Method != "POST" || got.Path != "/v1/inbox/01XYZ/read" {
		t.Fatalf("%+v", got)
	}
	got = BuildInboxResolve("01XYZ")
	if got.Method != "POST" || got.Path != "/v1/inbox/01XYZ/resolve" {
		t.Fatalf("%+v", got)
	}
	got = BuildInboxReadAll()
	if got.Method != "POST" || got.Path != "/v1/inbox/read-all" {
		t.Fatalf("%+v", got)
	}
	ap := BuildAlertsPath("2026-08-28T00:00:00Z")
	if !strings.Contains(ap, "/v1/alerts?") || !strings.Contains(ap, "since=2026-08-28T00%3A00%3A00Z") {
		t.Fatalf("alerts path %s", ap)
	}
}

func TestBuildAssistantWake(t *testing.T) {
	t.Parallel()
	req := BuildAssistantWake("ws1", "hello", true)
	if req.Method != "POST" || !strings.Contains(req.Path, "/v1/workspaces/ws1/assistant/wake") {
		t.Fatalf("%+v", req)
	}
	b, _ := json.Marshal(req.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["extra_prompt"] != "hello" || m["dry_run"] != true {
		t.Fatalf("body %s", b)
	}
}

func TestBuildConversationChatRequests(t *testing.T) {
	t.Parallel()
	path := BuildConversationsListPath("ws1", "cli", "01C", 10)
	if !strings.Contains(path, "workspace=ws1") || !strings.Contains(path, "channel=cli") {
		t.Fatalf("list %s", path)
	}
	req := BuildConversationCreate("ws1", "t", "cli", "cli", "ws1")
	if req.Method != "POST" || req.Path != "/v1/conversations" {
		t.Fatalf("create %+v", req)
	}
	req = BuildConversationPatch("01C", "n", "s", 2)
	if req.Method != "PATCH" || req.Path != "/v1/conversations/01C" {
		t.Fatalf("patch %+v", req)
	}
	mp := BuildConversationMessagesPath("01C", "01M", 20)
	if !strings.Contains(mp, "/v1/conversations/01C/messages") || !strings.Contains(mp, "cursor=01M") {
		t.Fatalf("messages %s", mp)
	}
	req = BuildConversationSend("01C", "hi", "cli", "ws1", "k1")
	if req.Method != "POST" || !strings.Contains(req.Path, "/messages") {
		t.Fatalf("send %+v", req)
	}
	req = BuildPointerGet("cli", "ws1")
	if req.Method != "GET" || req.Path != "/v1/channel-pointers/cli/ws1" {
		t.Fatalf("ptr get %+v", req)
	}
	req = BuildPointerPut("cli", "ws1", "01C")
	if req.Method != "PUT" || req.Path != "/v1/channel-pointers/cli/ws1" {
		t.Fatalf("ptr put %+v", req)
	}
	req = BuildAssistantSay("01C", "hi", "ws1")
	if req.Method != "POST" || !strings.Contains(req.Path, "/v1/conversations/01C/messages") {
		t.Fatalf("say %+v", req)
	}
	// Remote sticky context uses the same request shape (no local branch).
	remote := BuildAssistantSay("01C", "hi", "ws1")
	if req.Method != remote.Method || req.Path != remote.Path {
		t.Fatalf("remote say shape %+v vs %+v", req, remote)
	}
}

func TestBuildRunMessage(t *testing.T) {
	t.Parallel()
	req := BuildRunMessage("a/b", "hello world")
	if req.Method != "POST" || req.Path != "/v1/runs/a%2Fb/message" {
		t.Fatalf("%+v", req)
	}
	b, _ := json.Marshal(req.Body)
	if string(b) != `{"text":"hello world"}` {
		t.Fatalf("body %s", b)
	}
}

func TestBuildStreamPath(t *testing.T) {
	t.Parallel()
	path := BuildStreamPath("run.failed,alert", "42")
	if !strings.Contains(path, "/v1/stream?") {
		t.Fatalf("%s", path)
	}
	if !strings.Contains(path, "topics=run.failed%2Calert") && !strings.Contains(path, "topics=run.failed,alert") {
		t.Fatalf("topics %s", path)
	}
	if !strings.Contains(path, "cursor=42") {
		t.Fatalf("cursor %s", path)
	}
	bare := BuildStreamPath("run", "")
	if strings.Contains(bare, "cursor=") {
		t.Fatalf("empty cursor should be omitted: %s", bare)
	}
	if !strings.Contains(bare, "topics=run") {
		t.Fatalf("%s", bare)
	}
}

func TestBuildDoctorFix(t *testing.T) {
	t.Parallel()
	req := BuildDoctorFix("config-missing")
	if req.Method != "POST" || req.Path != "/v1/system/doctor/config-missing/fix" {
		t.Fatalf("%+v", req)
	}
}

func TestBuildSkillPut(t *testing.T) {
	t.Parallel()
	req := BuildSkillPut("demo", "# skill")
	if req.Method != "PUT" || req.Path != "/v1/skills/demo" {
		t.Fatalf("%+v", req)
	}
	b, _ := json.Marshal(req.Body)
	if !strings.Contains(string(b), `"skill_md":"# skill"`) {
		t.Fatalf("%s", b)
	}
}

func TestBuildIntegrationCursor(t *testing.T) {
	t.Parallel()
	got := BuildIntegrationCursorGet("mail")
	if got.Method != "GET" || got.Path != "/v1/integrations/mail/cursor" {
		t.Fatalf("get %+v", got)
	}
	got = BuildIntegrationCursorSet("mail", "evt-1")
	if got.Method != "PUT" || got.Body == nil {
		t.Fatalf("set %+v", got)
	}
	b, _ := json.Marshal(got.Body)
	if !strings.Contains(string(b), `"last_event_id":"evt-1"`) {
		t.Fatalf("%s", b)
	}
	got = BuildIntegrationCursorReset("mail")
	if got.Method != "PUT" || got.Body != nil {
		t.Fatalf("reset must send empty body, got %+v", got)
	}
}

func TestBuildWorkspaceAndMemoryAndRules(t *testing.T) {
	t.Parallel()
	req := BuildWorkspaceDelete("ws1")
	if req.Method != "DELETE" || req.Path != "/v1/workspaces/ws1" {
		t.Fatalf("delete %+v", req)
	}
	req = BuildWorkspaceRename("ws1", "new")
	if req.Method != "PATCH" {
		t.Fatalf("rename %+v", req)
	}
	req = BuildJobMemoryPut("demo", "ws1", "hello")
	if req.Method != "PUT" || !strings.Contains(req.Path, "/memory") {
		t.Fatalf("mem put %+v", req)
	}
	req = BuildJobMemoryClear("demo", "ws1")
	if req.Method != "DELETE" {
		t.Fatalf("mem clear %+v", req)
	}
	req = BuildRulesTest("true", "x", map[string]any{"a": 1})
	if req.Method != "POST" || req.Path != "/v1/rules/test" {
		t.Fatalf("rules test %+v", req)
	}
	req = BuildRulesFire("mail", "ws", "job", "true", "", map[string]any{"a": 1}, 0)
	if req.Path != "/v1/rules/fire" {
		t.Fatalf("rules fire %+v", req)
	}
	req = BuildIntegrationConnect("mail")
	if req.Path != "/v1/integrations/mail/connect" {
		t.Fatalf("connect %+v", req)
	}
	req = BuildIntegrationDisconnect("mail")
	if req.Path != "/v1/integrations/mail/disconnect" {
		t.Fatalf("disconnect %+v", req)
	}
}

func TestParseJobListKind(t *testing.T) {
	t.Parallel()
	got, err := parseJobListKind("")
	if err != nil || got != "job" {
		t.Fatalf("%q %v", got, err)
	}
	got, err = parseJobListKind("all")
	if err != nil || got != "all" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := parseJobListKind("bogus"); err == nil {
		t.Fatal("expected error")
	}
}

func TestKeepJobKind(t *testing.T) {
	t.Parallel()
	rows := []map[string]any{
		{"kind": "job", "name": "a"},
		{"kind": "assistant", "name": "b"},
		{"kind": "", "name": "c"},
	}
	jobs := FilterJobsByKindFlag(rows, "job")
	if len(jobs) != 2 || mapStr(jobs[0], "name") != "a" {
		t.Fatalf("%v", jobs)
	}
	as := FilterJobsByKindFlag(rows, "assistant")
	if len(as) != 1 || mapStr(as[0], "name") != "b" {
		t.Fatalf("%v", as)
	}
	all := FilterJobsByKindFlag(rows, "all")
	if len(all) != 3 {
		t.Fatalf("%v", all)
	}
}

func TestResolveRunMessageText(t *testing.T) {
	t.Parallel()
	got, err := resolveRunMessageText("deploy the branch", "")
	if err != nil || got != "deploy the branch" {
		t.Fatalf("%q %v", got, err)
	}
	got, err = resolveRunMessageText("", "from stdin")
	if err != nil || got != "from stdin" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := resolveRunMessageText("args", "stdin"); err == nil {
		t.Fatal("both should error")
	}
	if _, err := resolveRunMessageText("", ""); err == nil {
		t.Fatal("empty should error")
	}
}

func TestIsTerminalRunStatus(t *testing.T) {
	t.Parallel()
	if !isTerminalRunStatus("complete") || !isTerminalRunStatus("failed") {
		t.Fatal("expected terminal")
	}
	if isTerminalRunStatus("running") {
		t.Fatal("running is not terminal")
	}
}

func findCmd(cmds []*urfaveCommand, name string) *urfaveCommand {
	for _, c := range cmds {
		if c != nil && c.Name == name {
			return c
		}
	}
	return nil
}

func hasFlag(cmd *urfaveCommand, name string) bool {
	if cmd == nil {
		return false
	}
	for _, f := range cmd.Flags {
		// urfave Flag has GetName via Names()[0] or String()
		if namer, ok := f.(interface{ Names() []string }); ok {
			for _, n := range namer.Names() {
				if n == name {
					return true
				}
			}
		}
	}
	return false
}
