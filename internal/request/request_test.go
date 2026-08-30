package request

import (
	"encoding/json"
	"strings"
	"testing"
)

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
	got, err := ParseJobListKind("")
	if err != nil || got != "job" {
		t.Fatalf("%q %v", got, err)
	}
	got, err = ParseJobListKind("all")
	if err != nil || got != "all" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := ParseJobListKind("bogus"); err == nil {
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
	if len(jobs) != 2 || jobs[0]["name"] != "a" {
		t.Fatalf("%v", jobs)
	}
	as := FilterJobsByKindFlag(rows, "assistant")
	if len(as) != 1 || as[0]["name"] != "b" {
		t.Fatalf("%v", as)
	}
	all := FilterJobsByKindFlag(rows, "all")
	if len(all) != 3 {
		t.Fatalf("%v", all)
	}
}

func TestBuildJobPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"list", BuildJobsListPath("ws1"), "/v1/jobs?workspace=ws1"},
		{"list no workspace", BuildJobsListPath(""), "/v1/jobs"},
		{"get", BuildJobGetPath("nightly", "ws1"), "/v1/jobs/nightly?workspace=ws1"},
		{"validate", BuildJobValidatePath("nightly", ""), "/v1/jobs/nightly/validate"},
		{"memory", BuildJobMemoryGetPath("nightly", "ws1"), "/v1/jobs/nightly/memory?workspace=ws1"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.name, tc.got, tc.want)
		}
	}
}

func TestBuildStreamDumpPath(t *testing.T) {
	t.Parallel()
	if p := BuildStreamDumpPath("", ""); p != "/v1/stream?follow=0" {
		t.Fatalf("bare %s", p)
	}
	p := BuildStreamDumpPath("run,alert", "42")
	for _, want := range []string{"follow=0", "cursor=42"} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in %s", want, p)
		}
	}
}

func TestBuildRawFileRequests(t *testing.T) {
	t.Parallel()
	put := BuildFilePut("ws1", "a/b.py", []byte("x=1\n"), "text/x-python", "etag-1")
	if put.Method != "PUT" || put.Path != "/v1/workspaces/ws1/files/a/b.py" {
		t.Fatalf("%+v", put)
	}
	if string(put.Raw) != "x=1\n" || put.ContentType != "text/x-python" || put.IfMatch != "etag-1" {
		t.Fatalf("%+v", put)
	}
	del := BuildFileDelete("ws1", "a/b.py", "etag-2")
	if del.Method != "DELETE" || del.Path != "/v1/workspaces/ws1/files/a/b.py" || del.IfMatch != "etag-2" || del.Raw != nil {
		t.Fatalf("%+v", del)
	}
	jp := BuildJobFilePut("nightly", "ws1", "run.py", []byte("print(1)"), "", "")
	if jp.Path != "/v1/jobs/nightly/files/run.py?workspace=ws1" || jp.ContentType != "" || jp.IfMatch != "" {
		t.Fatalf("%+v", jp)
	}
	jd := BuildJobFileDelete("nightly", "ws1", "run.py", "e")
	if jd.Path != "/v1/jobs/nightly/files/run.py?workspace=ws1" || jd.IfMatch != "e" {
		t.Fatalf("%+v", jd)
	}
	sp := BuildSkillFilePut("deploy", "run.sh", []byte("set -e\n"), "", "e2")
	if sp.Path != "/v1/skills/deploy/files/run.sh" || sp.IfMatch != "e2" {
		t.Fatalf("%+v", sp)
	}
	sd := BuildSkillFileDelete("deploy", "run.sh", "")
	if sd.Path != "/v1/skills/deploy/files/run.sh" || sd.IfMatch != "" {
		t.Fatalf("%+v", sd)
	}
}

func TestBuildRunPathsAndActions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		req        HTTPRequest
		wantMethod string
		wantPath   string
		wantBody   string
	}{
		{"resume", BuildRunResume("01ABC"), "POST", "/v1/runs/01ABC/resume", ""},
		{"replay", BuildRunReplay("01ABC"), "POST", "/v1/runs/01ABC/replay", ""},
		{"approve", BuildRunApprove("01ABC", "allow"), "POST", "/v1/runs/01ABC/approve", `{"decision":"allow"}`},
	}
	for _, tc := range cases {
		if tc.req.Method != tc.wantMethod || tc.req.Path != tc.wantPath {
			t.Fatalf("%s: %+v", tc.name, tc.req)
		}
		if tc.wantBody != "" {
			b, _ := json.Marshal(tc.req.Body)
			if string(b) != tc.wantBody {
				t.Fatalf("%s body: %s", tc.name, b)
			}
		}
	}
	if p := BuildRunGetPath("01ABC"); p != "/v1/runs/01ABC" {
		t.Fatalf("get %s", p)
	}
	if p := BuildRunTimelinePath("01ABC"); p != "/v1/runs/01ABC/timeline" {
		t.Fatalf("timeline %s", p)
	}
	if p := BuildRunLogPath("01ABC", false); p != "/v1/runs/01ABC/log" {
		t.Fatalf("log %s", p)
	}
	if p := BuildRunLogPath("01ABC", true); p != "/v1/runs/01ABC/log?follow=1" {
		t.Fatalf("log follow %s", p)
	}
}

func TestBuildJobCreatePatchRun(t *testing.T) {
	t.Parallel()
	create := BuildJobCreate("nightly", "ws1", "---\nharness: claude\n---\n")
	if create.Method != "POST" || create.Path != "/v1/jobs" {
		t.Fatalf("%+v", create)
	}
	m, _ := create.Body.(map[string]any)
	if m["name"] != "nightly" || m["workspace"] != "ws1" || m["content"] == "" {
		t.Fatalf("%v", m)
	}
	patch := BuildJobPatch("nightly", "ws1", map[string]any{"active": false}, "etag-1")
	if patch.Method != "PATCH" || patch.Path != "/v1/jobs/nightly?workspace=ws1" || patch.IfMatch != "etag-1" {
		t.Fatalf("%+v", patch)
	}
	if p := BuildJobRunPath("nightly", "ws1"); p != "/v1/jobs/nightly/run?workspace=ws1" {
		t.Fatalf("run %s", p)
	}
	if p := BuildJobRunPath("nightly", ""); p != "/v1/jobs/nightly/run" {
		t.Fatalf("run no ws %s", p)
	}
}

func TestBuildWorkspaceAndAssistantRequests(t *testing.T) {
	t.Parallel()
	if p := BuildWorkspacesListPath(); p != "/v1/workspaces" {
		t.Fatalf("list %s", p)
	}
	if p := BuildWorkspaceGetPath("ws1"); p != "/v1/workspaces/ws1" {
		t.Fatalf("get %s", p)
	}
	if p := BuildAssistantGetPath("ws1"); p != "/v1/workspaces/ws1/assistant" {
		t.Fatalf("asst %s", p)
	}
	if p := BuildAssistantMemoryGetPath("ws1"); p != "/v1/workspaces/ws1/assistant/memory" {
		t.Fatalf("asst mem %s", p)
	}
	put := BuildAssistantPut("ws1", "# hi\n")
	if put.Method != "PUT" || put.Path != "/v1/workspaces/ws1/assistant" {
		t.Fatalf("%+v", put)
	}
	m, _ := put.Body.(map[string]string)
	if m["content"] != "# hi\n" {
		t.Fatalf("%v", m)
	}
	no := false
	patch := BuildAssistantPatch("ws1", &no, "e1")
	if patch.Method != "PATCH" || patch.IfMatch != "e1" {
		t.Fatalf("%+v", patch)
	}
	pm, _ := patch.Body.(map[string]any)
	if d, _ := pm["disabled"].(*bool); d == nil || *d != false {
		t.Fatalf("%v", pm)
	}
	if patch := BuildAssistantPatch("ws1", nil, ""); patch.IfMatch != "" {
		t.Fatalf("%+v", patch)
	}
	del := BuildAssistantDelete("ws1", "e2")
	if del.Method != "DELETE" || del.Path != "/v1/workspaces/ws1/assistant" || del.IfMatch != "e2" {
		t.Fatalf("%+v", del)
	}
}

func TestBuildHarnessIntegrationQueueRequests(t *testing.T) {
	t.Parallel()
	if p := BuildHarnessesListPath(); p != "/v1/harnesses" {
		t.Fatalf("harnesses %s", p)
	}
	cancel := BuildHarnessLoginCancel("claude")
	if cancel.Method != "DELETE" || cancel.Path != "/v1/harnesses/claude/login" {
		t.Fatalf("%+v", cancel)
	}
	if p := BuildIntegrationGetPath("mail"); p != "/v1/integrations/mail" {
		t.Fatalf("integ get %s", p)
	}
	def := map[string]any{"name": "mail", "kind": "imap"}
	create := BuildIntegrationCreate(def)
	if create.Method != "POST" || create.Path != "/v1/integrations" || create.Body.(map[string]any)["name"] != "mail" {
		t.Fatalf("%+v", create)
	}
	rq := BuildQueueRequeue("01ABC")
	if rq.Method != "POST" || rq.Path != "/v1/queue/01ABC/requeue" {
		t.Fatalf("requeue %+v", rq)
	}
	cl := BuildQueueCancel("01ABC")
	if cl.Method != "POST" || cl.Path != "/v1/queue/01ABC/cancel" {
		t.Fatalf("cancel %+v", cl)
	}
}

func TestBuildSkillSecretRequests(t *testing.T) {
	t.Parallel()
	if p := BuildSkillGetPath("deploy"); p != "/v1/skills/deploy" {
		t.Fatalf("get %s", p)
	}
	sc := BuildSkillCreate("deploy", "# SKILL.md\n", "")
	if sc.Method != "POST" || sc.Path != "/v1/skills" {
		t.Fatalf("%+v", sc)
	}
	m, _ := sc.Body.(map[string]any)
	if m["name"] != "deploy" || m["skill_md"] != "# SKILL.md\n" {
		t.Fatalf("%v", m)
	}
	up := BuildSkillUpdate("deploy")
	if up.Method != "POST" || up.Path != "/v1/skills/deploy/update" {
		t.Fatalf("%+v", up)
	}
	del := BuildSkillDelete("deploy")
	if del.Method != "DELETE" || del.Path != "/v1/skills/deploy" {
		t.Fatalf("%+v", del)
	}
	if p := BuildSecretsListPath(); p != "/v1/secrets" {
		t.Fatalf("secrets %s", p)
	}
	sp := BuildSecretPut("api_key", "s3cr3t")
	if sp.Method != "PUT" || sp.Path != "/v1/secrets/api_key" {
		t.Fatalf("%+v", sp)
	}
	sm, _ := sp.Body.(map[string]string)
	if sm["value"] != "s3cr3t" {
		t.Fatalf("%v", sm)
	}
	sd := BuildSecretDelete("api_key")
	if sd.Method != "DELETE" || sd.Path != "/v1/secrets/api_key" {
		t.Fatalf("%+v", sd)
	}
}

func TestBuildSystemAndCloudRequests(t *testing.T) {
	t.Parallel()
	paths := map[string]string{
		"health": BuildSystemHealthPath(),
		"doctor": BuildSystemDoctorPath(),
		"logs":   BuildSystemLogsPath(),
		"update": BuildSystemUpdatePath(),
		"cloud":  BuildCloudStatusPath(),
	}
	for name, p := range paths {
		if p == "" {
			t.Fatalf("%s empty", name)
		}
	}
	if BuildSystemHealthPath() != "/v1/system/health" || BuildSystemDoctorPath() != "/v1/system/doctor" {
		t.Fatal("system paths")
	}
	if BuildSystemLogsPath() != "/v1/system/logs" || BuildSystemUpdatePath() != "/v1/system/update" {
		t.Fatal("system paths 2")
	}
	if BuildSystemGuidesPath() != "/v1/system/guides" {
		t.Fatal("guides path")
	}
	if BuildSystemGuidePath("getting-started") != "/v1/system/guides/getting-started" {
		t.Fatal("guide topic path")
	}
	if BuildCloudStatusPath() != "/v1/cloud/status" {
		t.Fatal("cloud status")
	}
	cases := []struct {
		name string
		req  HTTPRequest
		want string
	}{
		{"drain", BuildSystemDrain(), "POST /v1/system/drain"},
		{"undrain", BuildSystemUndrain(), "POST /v1/system/undrain"},
		{"link", BuildCloudLink(), "POST /v1/cloud/link"},
		{"unlink", BuildCloudUnlink(), "DELETE /v1/cloud/link"},
		{"config get", BuildConfigGet(), "GET /v1/config"},
	}
	for _, tc := range cases {
		if got := tc.req.Method + " " + tc.req.Path; got != tc.want {
			t.Fatalf("%s: %s", tc.name, got)
		}
	}
}
