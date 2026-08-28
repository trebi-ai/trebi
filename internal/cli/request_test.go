package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urfave/cli/v2"
)

// urfaveCommand is a local alias so registration tests stay readable.
type urfaveCommand = cli.Command

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

func TestBuildJobDelete(t *testing.T) {
	t.Parallel()
	req := BuildJobDelete("demo", "ws1")
	if req.Method != "DELETE" {
		t.Fatalf("method %s", req.Method)
	}
	if !strings.HasPrefix(req.Path, "/v1/jobs/demo") || !strings.Contains(req.Path, "workspace=ws1") {
		t.Fatalf("path %s", req.Path)
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
	a := BuildHarnessAuthGet("claude")
	if a.Path != "/v1/harnesses/claude/auth" || a.Method != "GET" {
		t.Fatalf("%+v", a)
	}
	p := BuildHarnessAuthPost("claude")
	if p.Method != "POST" {
		t.Fatalf("%+v", p)
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
	req = BuildJobDelete("demo", "ws1")
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
	for _, want := range []string{"list", "runs", "job", "harness", "integration", "config", "service", "conversation", "approvals", "events"} {
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
	svc := findCmd(app.Commands, "service")
	for _, want := range []string{"install", "uninstall", "start", "stop", "restart", "status", "logs", "daemon", "shutdown"} {
		if findCmd(svc.Subcommands, want) == nil {
			t.Fatalf("service missing %s", want)
		}
	}
	job := findCmd(app.Commands, "job")
	if job == nil {
		t.Fatal("job missing")
	}
	for _, want := range []string{"delete", "schedule", "status", "tail", "list", "pause", "unpause", "run"} {
		if findCmd(job.Subcommands, want) == nil {
			t.Fatalf("job missing %s", want)
		}
	}
	jobList := findCmd(job.Subcommands, "list")
	if !hasFlag(jobList, "schedule") {
		t.Fatal("job list missing --schedule")
	}
	h := findCmd(app.Commands, "harness")
	for _, want := range []string{"list", "ensure", "auth"} {
		if findCmd(h.Subcommands, want) == nil {
			t.Fatalf("harness missing %s", want)
		}
	}
	auth := findCmd(h.Subcommands, "auth")
	for _, want := range []string{"status", "mark"} {
		if findCmd(auth.Subcommands, want) == nil {
			t.Fatalf("harness auth missing %s", want)
		}
	}
	integ := findCmd(app.Commands, "integration")
	for _, want := range []string{"list", "show", "set", "remove", "stop", "restart", "logs", "test"} {
		if findCmd(integ.Subcommands, want) == nil {
			t.Fatalf("integration missing %s", want)
		}
	}
	cfg := findCmd(app.Commands, "config")
	for _, want := range []string{"show", "set", "validate", "path", "bridge"} {
		if findCmd(cfg.Subcommands, want) == nil {
			t.Fatalf("config missing %s", want)
		}
	}
	list := findCmd(app.Commands, "list")
	if !hasFlag(list, "detailed") || !hasFlag(list, "assistants-only") {
		t.Fatal("list flags")
	}
	run := findCmd(job.Subcommands, "run")
	if !hasFlag(run, "extra-prompt") || !hasFlag(run, "test") || !hasFlag(run, "attach") {
		t.Fatal("job run flags")
	}
	runsList := findCmd(findCmd(app.Commands, "runs").Subcommands, "list")
	for _, f := range []string{"status", "job", "kind", "workspace", "harness", "model", "conversation", "limit"} {
		if !hasFlag(runsList, f) {
			t.Fatalf("runs list missing flag %s", f)
		}
	}
	if hasFlag(runsList, "assistant") {
		t.Fatal("runs list should not have --assistant (use --kind assistant)")
	}
	if findCmd(findCmd(app.Commands, "runs").Subcommands, "attach") == nil {
		t.Fatal("runs attach missing")
	}
	if findCmd(findCmd(app.Commands, "runs").Subcommands, "retry") == nil {
		t.Fatal("runs retry missing")
	}
	ev := findCmd(app.Commands, "events")
	if findCmd(ev.Subcommands, "retrigger") == nil {
		t.Fatal("events retrigger missing")
	}
	mem := findCmd(findCmd(app.Commands, "job").Subcommands, "memory")
	for _, want := range []string{"show", "add", "remove"} {
		if findCmd(mem.Subcommands, want) == nil {
			t.Fatalf("job memory missing %s", want)
		}
	}
	cfgShow := findCmd(findCmd(app.Commands, "config").Subcommands, "show")
	if !hasFlag(cfgShow, "yaml") || !hasFlag(cfgShow, "tree") {
		t.Fatal("config show missing --yaml/--tree")
	}
	asst := findCmd(app.Commands, "assistant")
	for _, want := range []string{"show", "wake", "history"} {
		if findCmd(asst.Subcommands, want) == nil {
			t.Fatalf("assistant missing %s", want)
		}
	}
	if !hasFlag(findCmd(asst.Subcommands, "wake"), "attach") {
		t.Fatal("assistant wake missing --attach")
	}
	hist := findCmd(asst.Subcommands, "history")
	for _, f := range []string{"status", "job", "harness", "model", "conversation", "limit", "json"} {
		if !hasFlag(hist, f) {
			t.Fatalf("assistant history missing flag %s", f)
		}
	}
	if findCmd(app.Commands, "remote") != nil {
		t.Fatal("trebi remote should be removed")
	}
	ws := findCmd(app.Commands, "workspace")
	for _, want := range []string{"list", "add", "use", "current", "unlink"} {
		if findCmd(ws.Subcommands, want) == nil {
			t.Fatalf("workspace missing %s", want)
		}
	}
	add := findCmd(ws.Subcommands, "add")
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
	if m["prompt_override"] != "hello" || m["dry_run"] != true {
		t.Fatalf("body %s", b)
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
