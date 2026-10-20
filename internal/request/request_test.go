package request

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRunSpecBuild(t *testing.T) {
	t.Parallel()
	req := RunSpec{Job: "demo", Workspace: "ws1", Prompt: "extra text", DryRun: true}.Build()
	if req.Method != "POST" || req.Path != "/v1/jobs/demo/run?workspace=ws1" {
		t.Fatalf("method/path: %+v", req)
	}
	b, _ := json.Marshal(req.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
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

func TestRunsQueryPath(t *testing.T) {
	t.Parallel()
	path := RunsQuery{
		Status: "running", Job: "demo", Workspace: "ws1", Kind: "job", Cursor: "01ABC",
		Harness: "claude", Model: "sonnet", Conversation: "conv1", EndReason: "sentinel",
		Q: "deploy", Since: "24h", Until: "1h", Limit: 20,
	}.Path()
	if !strings.Contains(path, "/v1/runs?") {
		t.Fatalf("path: %s", path)
	}
	for _, want := range []string{
		"status=running", "job=demo", "workspace=ws1", "kind=job",
		"cursor=01ABC", "harness=claude", "model=sonnet", "limit=20",
		"conversation=conv1", "end_reason=sentinel", "q=deploy", "since=24h", "until=1h",
	} {
		if !strings.Contains(path, want) {
			t.Fatalf("missing %q in %s", want, path)
		}
	}
	if got := (RunsQuery{Limit: 5}).Path(); got != "/v1/runs?limit=5" {
		t.Fatalf("empty filters must be dropped: %s", got)
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
	list := BuildFileListPath("ws1", "reports/", "reports/a.py", 20, true)
	if !strings.HasPrefix(list, "/v1/workspaces/ws1/files?") {
		t.Fatalf("list %s", list)
	}
	for _, want := range []string{"prefix=reports", "cursor=reports", "limit=20", "recursive=1"} {
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
	list := BuildJobFileListPath("report", "ws1", "tools/", "tools/a.py", 20, false)
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
	list := BuildSkillFileListPath(SkillRef{Name: "demo"}, "scripts/", "scripts/a.py", 20, false)
	if !strings.HasPrefix(list, "/v1/skills/demo/files?") {
		t.Fatalf("list %s", list)
	}
	get := BuildSkillFileGetPath(SkillRef{Name: "demo"}, "helper.py", true)
	if !strings.Contains(get, "/v1/skills/demo/files/helper.py") || !strings.Contains(get, "stat=1") {
		t.Fatalf("get %s", get)
	}
	if p := BuildSkillFilePutPath(SkillRef{Name: "demo"}, "a/b.py"); p != "/v1/skills/demo/files/a/b.py" {
		t.Fatalf("put %s", p)
	}
	if p := BuildSkillFileDeletePath(SkillRef{Name: "demo"}, "a/b.py"); p != "/v1/skills/demo/files/a/b.py" {
		t.Fatalf("delete %s", p)
	}
}

func TestBuildWorkspaceAdd(t *testing.T) {
	t.Parallel()
	req := BuildWorkspaceAdd("/tmp/ws", "reports", "acme", true)
	if req.Method != "POST" || req.Path != "/v1/workspaces" {
		t.Fatalf("%+v", req)
	}
	body, _ := req.Body.(map[string]any)
	if body["path"] != "/tmp/ws" || body["name"] != "reports" || body["handle"] != "acme" || body["create"] != true {
		t.Fatalf("body %+v", body)
	}
	req = BuildWorkspaceAdd("", "reports", "", true)
	body, _ = req.Body.(map[string]any)
	if _, ok := body["path"]; ok {
		t.Fatalf("pathless should omit path: %+v", body)
	}
	if _, ok := body["handle"]; ok {
		t.Fatalf("empty handle should be omitted: %+v", body)
	}
}

func TestBuildJobDelete(t *testing.T) {
	t.Parallel()
	req := BuildJobDelete("demo", "ws1", "", false, false)
	if req.Method != "DELETE" {
		t.Fatalf("method %s", req.Method)
	}
	if !strings.HasPrefix(req.Path, "/v1/jobs/demo") || !strings.Contains(req.Path, "workspace=ws1") {
		t.Fatalf("path %s", req.Path)
	}
	if strings.Contains(req.Path, "confirm=") {
		t.Fatalf("unconfirmed delete must not send confirm: %s", req.Path)
	}
	req = BuildJobDelete("demo", "ws1", "abc123", true, true)
	if !strings.Contains(req.Path, "purge=1") || !strings.Contains(req.Path, "confirm=1") {
		t.Fatalf("purge path %s", req.Path)
	}
	if req.IfMatch != "abc123" {
		t.Fatalf("if-match %q", req.IfMatch)
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

func TestBuildConnectorRestart(t *testing.T) {
	t.Parallel()
	req := BuildConnectorRestart(ConnectorRef{Ref: "echo-src"})
	if req.Method != "POST" || req.Path != "/v1/connectors/echo-src/restart" {
		t.Fatalf("%+v", req)
	}
	req = BuildConnectorRestart(ConnectorRef{Ref: "con_01k6abcdefghjkmnpqrstvwxyz"})
	if req.Path != "/v1/connectors/con_01k6abcdefghjkmnpqrstvwxyz/restart" {
		t.Fatalf("an id ref names the path: %+v", req)
	}
}

func TestConnectorPatchBuild(t *testing.T) {
	t.Parallel()
	slug, title, version, track := "echo-2", "Echo", "1.2.0", "pinned"
	req := ConnectorPatch{
		Ref: "echo", Slug: &slug, Title: &title, Version: &version, Track: &track, Inputs: map[string]string{"A": "1"},
		Patch: json.RawMessage(`{"events":{"restart":"never"}}`),
	}.Build()
	if req.Method != "PATCH" || req.Path != "/v1/connectors/echo" {
		t.Fatalf("%+v", req)
	}
	b, _ := json.Marshal(req.Body)
	s := string(b)
	for _, want := range []string{`"slug":"echo-2"`, `"title":"Echo"`, `"version":"1.2.0"`, `"track":"pinned"`, `"inputs":{"A":"1"}`, `"patch":{"events":{"restart":"never"}}`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
	if strings.Contains(s, "manifest") {
		t.Fatalf("nil manifest must be omitted: %s", s)
	}
	b, _ = json.Marshal(ConnectorPatch{Ref: "echo", Patch: json.RawMessage("null")}.Build().Body)
	if string(b) != `{"patch":null}` {
		t.Fatalf("null must survive as a clear: %s", b)
	}
}

func TestBuildConnectorCatalogPaths(t *testing.T) {
	t.Parallel()
	path := BuildConnectorCatalogPath("ops", "official", "CUR1", 25)
	for _, want := range []string{"/v1/connectors/catalog?", "q=ops", "registry=official", "cursor=CUR1", "limit=25"} {
		if !strings.Contains(path, want) {
			t.Fatalf("path %q missing %q", path, want)
		}
	}
	if BuildConnectorCatalogPath("", "", "", 0) != "/v1/connectors/catalog" {
		t.Fatalf("empty path = %q", BuildConnectorCatalogPath("", "", "", 0))
	}
	entry := BuildConnectorCatalogEntryPath("official", "acme/tool", "1.0.0")
	for _, want := range []string{"/v1/connectors/catalog/entry?", "name=acme%2Ftool", "version=1.0.0", "registry=official"} {
		if !strings.Contains(entry, want) {
			t.Fatalf("entry path %q missing %q", entry, want)
		}
	}
}

func TestBuildConnectorInstall(t *testing.T) {
	t.Parallel()
	req := ConnectorInstall{
		Registry: "official", Name: "ai.adtest/adtest-mcp", Version: "1.1.0", Slug: "adtest", Title: "Ad test",
		Package: 0, Remote: -1, Inputs: map[string]string{"ADTEST_API_KEY": "k"},
	}.Build()
	if req.Method != "POST" || req.Path != "/v1/connectors/install" {
		t.Fatalf("%+v", req)
	}
	b, _ := json.Marshal(req.Body)
	s := string(b)
	for _, want := range []string{`"registry":"official"`, `"name":"ai.adtest/adtest-mcp"`, `"version":"1.1.0"`, `"slug":"adtest"`, `"title":"Ad test"`, `"package":0`, `"inputs":{"ADTEST_API_KEY":"k"}`} {
		if !strings.Contains(s, want) {
			t.Fatalf("body %s missing %s", s, want)
		}
	}
	if strings.Contains(s, `"remote"`) || strings.Contains(s, `"as"`) {
		t.Fatalf("remote -1 and as must be omitted: %s", s)
	}
	b, _ = json.Marshal(ConnectorInstall{Name: "m", Package: -1, Remote: 1}.Build().Body)
	s = string(b)
	if !strings.Contains(s, `"remote":1`) || strings.Contains(s, `"package"`) || strings.Contains(s, `"inputs"`) {
		t.Fatalf("remote body: %s", s)
	}
	b, _ = json.Marshal(ConnectorInstall{Source: &ConnectorSource{URL: "flarco/cli-tools/vitally-cli", CheckID: "chk_1"}, Force: true, Package: -1, Remote: -1}.Build().Body)
	if s = string(b); s != `{"force":true,"source":{"url":"flarco/cli-tools/vitally-cli","check_id":"chk_1"}}` {
		t.Fatalf("source body: %s", s)
	}
	chk := BuildConnectorSourceCheck("flarco/cli-tools")
	if chk.Method != "POST" || chk.Path != "/v1/connectors/sources/check" || chk.Body.(map[string]any)["url"] != "flarco/cli-tools" {
		t.Fatalf("check: %+v", chk)
	}
}

func TestBuildConnectorSessions(t *testing.T) {
	t.Parallel()
	wa := ConnectorRef{Ref: "whatsapp"}
	cases := []struct {
		name         string
		req          HTTPRequest
		method, path string
		body         string
	}{
		{"login", BuildConnectorLogin(wa, "qr"), "POST", "/v1/connectors/whatsapp/login", `{"method":"qr"}`},
		{"login methods", BuildConnectorLoginMethods(wa), "GET", "/v1/connectors/whatsapp/login/methods", `null`},
		{"login bare", BuildConnectorLogin(ConnectorRef{Ref: "whatsapp"}, ""), "POST", "/v1/connectors/whatsapp/login", `{}`},
		{"submit", BuildConnectorLoginSubmit("whatsapp", "f1", map[string]any{"code": "1", "teams": []string{"T1", "T2"}}), "POST", "/v1/connectors/whatsapp/login/f1", `{"fields":{"code":"1","teams":["T1","T2"]}}`},
		{"cancel", BuildConnectorLoginCancel("whatsapp", "f1"), "DELETE", "/v1/connectors/whatsapp/login/f1", `null`},
		{"logout", BuildConnectorLogout(wa), "POST", "/v1/connectors/whatsapp/logout", `{}`},
		{"bind", BuildConnectorBind(wa, "ws1", "r 1", "t1", "agents/triage"), "POST", "/v1/connectors/whatsapp/rooms/r%201/bind", `{"operator":"agents/triage","thread":"t1","workspace":"ws1"}`},
		{"subscribe", BuildConnectorSubscribe(wa, map[string]any{"repo": "a/b"}, nil), "POST", "/v1/connectors/whatsapp/subscriptions", `{"values":{"repo":"a/b"}}`},
		{"subscription set clears types", BuildConnectorSubscriptionSet(wa, "sub_1", nil, []string{}), "PATCH", "/v1/connectors/whatsapp/subscriptions/sub_1", `{"types":[]}`},
		{"unsubscribe", BuildConnectorUnsubscribe(wa, "sub_1"), "DELETE", "/v1/connectors/whatsapp/subscriptions/sub_1", `null`},
		{"subscription submit", BuildConnectorSubscriptionSubmit(wa, "sub_1", nil), "POST", "/v1/connectors/whatsapp/subscriptions/sub_1/submit", `{"fields":{}}`},
		{"subscriptions sync", BuildConnectorSubscriptionsSync(wa), "POST", "/v1/connectors/whatsapp/subscriptions/sync", `{}`},
		{"subscription options", BuildConnectorSubscriptionOptions(wa, "repo", "tre", nil, "c1", 5), "POST", "/v1/connectors/whatsapp/subscriptions/options", `{"cursor":"c1","field":"repo","limit":5,"q":"tre"}`},
		{"override get", BuildConnectorOverrideGet("ws1", "whatsapp"), "GET", "/v1/workspaces/ws1/connectors/whatsapp", `null`},
		{"override set", BuildConnectorOverrideSet("ws1", "whatsapp", map[string]any{"disabled": true}), "PUT", "/v1/workspaces/ws1/connectors/whatsapp", `{"disabled":true}`},
		{"override delete", BuildConnectorOverrideDelete("ws1", "whatsapp"), "DELETE", "/v1/workspaces/ws1/connectors/whatsapp", `null`},
	}
	for _, c := range cases {
		b, _ := json.Marshal(c.req.Body)
		if c.req.Method != c.method || c.req.Path != c.path || string(b) != c.body {
			t.Errorf("%s: %s %s %s", c.name, c.req.Method, c.req.Path, b)
		}
	}
	if p := BuildConnectorRoomsPath(wa, "fam", "group", "C1", 10); p != "/v1/connectors/whatsapp/rooms?cursor=C1&kind=group&limit=10&q=fam" {
		t.Errorf("rooms %s", p)
	}
	if p := BuildConnectorThreadsPath(ConnectorRef{Ref: "email"}, "mbx", "", 0); p != "/v1/connectors/email/rooms/mbx/threads" {
		t.Errorf("threads %s", p)
	}
	if p := BuildConnectorLogsPath(wa, true); p != "/v1/connectors/whatsapp/logs?follow=1" {
		t.Errorf("logs %s", p)
	}
}

// TestBuildConnectorConformance proves the suites ride the body only when
// set.
func TestBuildConnectorConformance(t *testing.T) {
	t.Parallel()
	req := BuildConnectorConformance(ConnectorRef{Ref: "wa-work"}, "123@g.us", []string{"protocol"})
	b, _ := json.Marshal(req.Body)
	if req.Method != "POST" || req.Path != "/v1/connectors/wa-work/conformance" || string(b) != `{"room":"123@g.us","suites":["protocol"]}` {
		t.Fatalf("%s %s %s", req.Method, req.Path, b)
	}
	b, _ = json.Marshal(BuildConnectorConformance(ConnectorRef{Ref: "wa-work"}, "", nil).Body)
	if strings.Contains(string(b), "suites") {
		t.Fatalf("no suites must omit the field: %s", b)
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
	p := BuildHarnessLoginStart("claude", "", "")
	if p.Method != "POST" || p.Path != "/v1/harnesses/claude/login" {
		t.Fatalf("%+v", p)
	}
	tok := BuildHarnessLoginToken("claude", "sk", "")
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
	md := BuildHarnessPatch("claude", map[string]any{"auth_mode": "separate"})
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

func TestBuildRunMark(t *testing.T) {
	t.Parallel()
	req := BuildRunMark("01ABC", "success", "digest was sent", false)
	if req.Method != "POST" || req.Path != "/v1/runs/01ABC/mark" {
		t.Fatalf("%+v", req)
	}
	body, _ := req.Body.(map[string]any)
	if body["status"] != "success" || body["note"] != "digest was sent" || body["clear"] != nil {
		t.Fatalf("body %+v", body)
	}
	req = BuildRunMark("01ABC", "", "", true)
	body, _ = req.Body.(map[string]any)
	if body["clear"] != true || body["status"] != nil || body["note"] != nil {
		t.Fatalf("clear body %+v", body)
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
	req = BuildServiceShutdown(true)
	if req.Method != "POST" || req.Path != "/v1/system/shutdown?confirm=1" {
		t.Fatalf("shutdown %+v", req)
	}
	req = BuildJobMemoryEntryAdd("demo", "ws1", "log", "2026-09-14 09:00", "note")
	if req.Method != "POST" || !strings.Contains(req.Path, "/v1/jobs/demo/memory/log") {
		t.Fatalf("mem add %+v", req)
	}
	if body, _ := req.Body.(map[string]any); body["text"] != "note" || body["at"] != "2026-09-14 09:00" {
		t.Fatalf("mem add body %+v", req.Body)
	}
	req = BuildJobMemoryEntryDelete("demo", "ws1", "lessons", "e1")
	if req.Method != "DELETE" || !strings.Contains(req.Path, "/memory/lessons/e1") {
		t.Fatalf("mem del %+v", req)
	}
	req = BuildAssistantMemoryEntryAdd("ws1", "log", "", "note")
	if req.Method != "POST" || !strings.Contains(req.Path, "/assistant/memory/log") {
		t.Fatalf("asst add %+v", req)
	}
	req = BuildAssistantMemoryEntryDelete("ws1", "log", "e1")
	if req.Method != "DELETE" || !strings.Contains(req.Path, "/memory/log/e1") {
		t.Fatalf("asst del %+v", req)
	}
	path := EventsQuery{Cursor: "01C", Limit: 10}.Path()
	if !strings.Contains(path, "cursor=01C") || !strings.Contains(path, "limit=10") {
		t.Fatalf("events list %s", path)
	}
	path = EventsQuery{After: "01A", Follow: true, Workspace: "w1"}.Path()
	if path != "/v1/events?after=01A&follow=1&workspace=w1" {
		t.Fatalf("events after %s", path)
	}
	path = BuildQueueListPath("pending", "01Q", 5)
	if !strings.Contains(path, "status=pending") || !strings.Contains(path, "cursor=01Q") {
		t.Fatalf("queue list %s", path)
	}
}

func TestBuildInboxAndAlerts(t *testing.T) {
	t.Parallel()
	path := BuildInboxListPath(InboxQuery{View: "done", Kind: "failure", Workspace: "ws1", Query: "disk full", Unread: true, Cursor: "01ABC", Limit: 20})
	if !strings.Contains(path, "/v1/inbox?") {
		t.Fatalf("path %s", path)
	}
	for _, want := range []string{"view=done", "kind=failure", "workspace=ws1", "q=disk+full", "unread=1", "cursor=01ABC", "limit=20"} {
		if !strings.Contains(path, want) {
			t.Fatalf("missing %q in %s", want, path)
		}
	}
	if got := BuildInboxListPath(InboxQuery{}); got != "/v1/inbox" {
		t.Fatalf("empty query %s", got)
	}
	got := BuildInboxGet("01XYZ")
	if got.Method != "GET" || got.Path != "/v1/inbox/01XYZ" {
		t.Fatalf("%+v", got)
	}
	got = BuildInboxApply(InboxApply{Op: "archive", IDs: []string{"01XYZ"}, Confirm: true})
	b, _ := got.Body.(InboxApply)
	if got.Method != "POST" || got.Path != "/v1/inbox/apply" || b.Op != "archive" || !b.Confirm || len(b.IDs) != 1 {
		t.Fatalf("%+v", got)
	}
	ap := BuildAlertsPath("2026-08-28T00:00:00Z", 5)
	if !strings.Contains(ap, "/v1/alerts?") || !strings.Contains(ap, "since=2026-08-28T00%3A00%3A00Z") {
		t.Fatalf("alerts path %s", ap)
	}
	if !strings.Contains(ap, "limit=5") {
		t.Fatalf("alerts limit %s", ap)
	}
	if strings.Contains(BuildAlertsPath("", 0), "limit=") {
		t.Fatalf("no limit must omit the parameter")
	}
}

func TestAssistantSettingsBuilders(t *testing.T) {
	t.Parallel()
	get := BuildAssistantDefaultsGet()
	if get.Method != "GET" || get.Path != "/v1/assistant" {
		t.Fatalf("defaults get %+v", get)
	}
	put := BuildAssistantDefaultsPut(map[string]any{"harness": "codex", "mode": "auto_edit"})
	if put.Method != "PUT" || put.Path != "/v1/assistant" {
		t.Fatalf("defaults put %+v", put)
	}
	body, _ := put.Body.(map[string]any)
	if body["harness"] != "codex" || body["mode"] != "auto_edit" {
		t.Fatalf("defaults body %+v", body)
	}
	wput := BuildAssistantSettingsPut("ws1", map[string]any{"harness": "codex"}, "h3")
	if wput.Method != "PUT" || wput.Path != "/v1/workspaces/ws1/assistant" || wput.IfMatch != "h3" {
		t.Fatalf("override put %+v", wput)
	}
	wbody, _ := wput.Body.(map[string]any)
	ov, _ := wbody["override"].(map[string]any)
	if ov["harness"] != "codex" {
		t.Fatalf("override body %+v", wbody)
	}
	if p := BuildAssistantPreviewPath("ws1"); p != "/v1/workspaces/ws1/assistant/preview" {
		t.Fatalf("preview %s", p)
	}
}

func TestConversationSessionBuilders(t *testing.T) {
	t.Parallel()
	del := BuildConversationDelete("01C", true)
	if del.Method != "DELETE" || del.Path != "/v1/conversations/01C?confirm=1" {
		t.Fatalf("delete %+v", del)
	}
	ev := BuildConversationEventsPath("01C", 7, 100, false)
	if ev != "/v1/conversations/01C/events?after=7&limit=100" {
		t.Fatalf("events %s", ev)
	}
	follow := BuildConversationEventsPath("01C", 0, 0, true)
	if follow != "/v1/conversations/01C/events?follow=1" {
		t.Fatalf("follow %s", follow)
	}
	interrupt := BuildConversationInterrupt("01C")
	if interrupt.Method != "POST" || interrupt.Path != "/v1/conversations/01C/interrupt" {
		t.Fatalf("interrupt %+v", interrupt)
	}
	closeReq := BuildConversationClose("01C")
	if closeReq.Method != "POST" || closeReq.Path != "/v1/conversations/01C/close" {
		t.Fatalf("close %+v", closeReq)
	}
	perm := BuildConversationPermission("01C", "p1", "allow_once", "")
	if perm.Method != "POST" || perm.Path != "/v1/conversations/01C/permissions/p1" {
		t.Fatalf("permission %+v", perm)
	}
	if b, _ := perm.Body.(map[string]any); b["decision"] != "allow_once" || len(b) != 1 {
		t.Fatalf("permission body %+v", perm.Body)
	}
	if msg := BuildConversationPermission("01C", "p1", "deny", "no"); msg.Body.(map[string]any)["message"] != "no" {
		t.Fatalf("permission message %+v", msg.Body)
	}
	mode := BuildConversationMode("01C", "ask")
	if mode.Method != "POST" || mode.Path != "/v1/conversations/01C/mode" {
		t.Fatalf("mode %+v", mode)
	}
	revert := BuildConversationRevert("01C", 3, "a/b.txt")
	if revert.Method != "POST" || revert.Path != "/v1/conversations/01C/revert" {
		t.Fatalf("revert %+v", revert)
	}
	if b, _ := revert.Body.(map[string]any); b["turn"] != 3 || b["path"] != "a/b.txt" {
		t.Fatalf("revert body %+v", revert.Body)
	}
}

func TestBuildConversationChatRequests(t *testing.T) {
	t.Parallel()
	path := BuildConversationsListPath("ws1", "cli", "", "01C", 10)
	if !strings.Contains(path, "workspace=ws1") || !strings.Contains(path, "channel=cli") {
		t.Fatalf("list %s", path)
	}
	req := BuildConversationCreate("ws1", "t", "cli", "cli", "ws1", "")
	if req.Method != "POST" || req.Path != "/v1/conversations" {
		t.Fatalf("create %+v", req)
	}
	title, summary, empty := "n", "s", ""
	req = BuildConversationPatch("01C", &title, &summary, 2)
	if req.Method != "PATCH" || req.Path != "/v1/conversations/01C" {
		t.Fatalf("patch %+v", req)
	}
	// Presence, not emptiness: a pointer to "" clears; nil leaves alone.
	patch, _ := BuildConversationPatch("01C", &empty, nil, 0).Body.(map[string]any)
	if v, ok := patch["title"]; !ok || v != "" {
		t.Fatalf("clear title %+v", patch)
	}
	if _, ok := patch["summary"]; ok {
		t.Fatalf("absent summary must stay absent: %+v", patch)
	}
	mp := BuildConversationMessagesPath("01C", "01M", 20)
	if !strings.Contains(mp, "/v1/conversations/01C/messages") || !strings.Contains(mp, "cursor=01M") {
		t.Fatalf("messages %s", mp)
	}
	req = BuildConversationSend("01C", "hi", "cli", "ws1", "k1")
	if req.Method != "POST" || !strings.Contains(req.Path, "/messages") {
		t.Fatalf("send %+v", req)
	}
	req = BuildPointerGet("cli", "ws1", "", "ws1")
	if req.Method != "GET" || !strings.Contains(req.Path, "/v1/channel-pointers/cli/ws1") ||
		!strings.Contains(req.Path, "workspace=ws1") {
		t.Fatalf("ptr get %+v", req)
	}
	req = BuildPointerPut("cli", "ws1", "", "01C")
	if req.Method != "PUT" || req.Path != "/v1/channel-pointers/cli/ws1" {
		t.Fatalf("ptr put %+v", req)
	}
	if got := BuildConversationOutboxPath("01C"); got != "/v1/conversations/01C/outbox" {
		t.Fatalf("outbox path %s", got)
	}
	req = BuildConversationOutboxRetry("01C", "01R")
	if req.Method != "POST" || req.Path != "/v1/conversations/01C/outbox/01R/retry" {
		t.Fatalf("outbox retry %+v", req)
	}
	// The ask route lets the daemon resolve the conversation, so a client
	// sends no pointer calls of its own.
	ask := AskSpec{Workspace: "ws1", Text: "hi"}.Build()
	if ask.Method != "POST" || ask.Path != "/v1/workspaces/ws1/assistant/messages" {
		t.Fatalf("ask %+v", ask)
	}
	if body, _ := ask.Body.(map[string]any); body["text"] != "hi" || len(body) != 1 {
		t.Fatalf("ask body %+v", ask.Body)
	}
	waited := AskSpec{Workspace: "ws1", Text: "hi", Connector: "ui", WaitMS: 5000}.Build()
	if waited.Path != "/v1/workspaces/ws1/assistant/messages?wait_ms=5000" {
		t.Fatalf("ask wait %+v", waited)
	}
	if body, _ := waited.Body.(map[string]any); body["connector"] != "ui" {
		t.Fatalf("ask channel %+v", waited.Body)
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
	req := BuildSkillPut(SkillRef{Name: "demo"}, "# skill", map[string]string{"claude": "sk/demo"}, "h1")
	if req.Method != "PUT" || req.Path != "/v1/skills/demo" || req.IfMatch != "h1" {
		t.Fatalf("%+v", req)
	}
	b, _ := json.Marshal(req.Body)
	for _, want := range []string{`"skill_md":"# skill"`, `"harnesses":{"claude":"sk/demo"}`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s in %s", want, b)
		}
	}
	if _, ok := BuildSkillPut(SkillRef{Name: "demo"}, "# skill", nil, "").Body.(map[string]any)["harnesses"]; ok {
		t.Fatalf("no harnesses must omit the field")
	}
}

func TestBuildConnectorCursor(t *testing.T) {
	t.Parallel()
	mail := ConnectorRef{Ref: "mail"}
	got := BuildConnectorCursorGet(mail)
	if got.Method != "GET" || got.Path != "/v1/connectors/mail/cursor" {
		t.Fatalf("get %+v", got)
	}
	got = BuildConnectorCursorSet(mail, "evt-1")
	if got.Path != "/v1/connectors/mail/cursor" {
		t.Fatalf("set path %q", got.Path)
	}
	if got.Method != "PUT" || got.Body == nil {
		t.Fatalf("set %+v", got)
	}
	b, _ := json.Marshal(got.Body)
	if !strings.Contains(string(b), `"last_event_id":"evt-1"`) {
		t.Fatalf("%s", b)
	}
	got = BuildConnectorCursorReset(mail)
	if got.Method != "PUT" || got.Body != nil {
		t.Fatalf("reset must send empty body, got %+v", got)
	}
}

func TestBuildWorkspaceAndMemoryAndRules(t *testing.T) {
	t.Parallel()
	req := BuildWorkspaceDelete("ws1", false)
	if req.Method != "DELETE" || req.Path != "/v1/workspaces/ws1" {
		t.Fatalf("delete %+v", req)
	}
	req = BuildWorkspaceDelete("ws1", true)
	if req.Path != "/v1/workspaces/ws1?confirm=1" {
		t.Fatalf("confirmed delete %+v", req)
	}
	req = BuildWorkspaceRename("ws1", "new")
	if req.Method != "PATCH" {
		t.Fatalf("rename %+v", req)
	}
	req = BuildWorkspaceMove("ws1", "/tmp/new", true, true, true)
	if req.Method != "POST" || req.Path != "/v1/workspaces/ws1/move?confirm=1" {
		t.Fatalf("move %+v", req)
	}
	body, _ := req.Body.(map[string]any)
	if body["path"] != "/tmp/new" || body["mkdir"] != true || body["dry_run"] != true {
		t.Fatalf("move body %+v", body)
	}
	req = BuildJobMemoryPut("demo", "ws1", "hello", "h2", false)
	if req.Method != "PUT" || !strings.Contains(req.Path, "/memory") || req.IfMatch != "h2" {
		t.Fatalf("mem put %+v", req)
	}
	if strings.Contains(req.Path, "force") {
		t.Fatalf("mem put must omit force when false: %s", req.Path)
	}
	req = BuildJobMemoryPut("demo", "ws1", "hello", "", true)
	if !strings.Contains(req.Path, "force=1") {
		t.Fatalf("mem put force %+v", req)
	}
	req = BuildJobMemoryClear("demo", "ws1", "", true)
	if req.Method != "DELETE" || !strings.Contains(req.Path, "confirm=1") {
		t.Fatalf("mem clear %+v", req)
	}
	if req.Body != nil {
		t.Fatalf("mem clear must carry no body: %+v", req.Body)
	}
	req = RuleTest{Match: "true", Extract: "x"}.Build("", "", map[string]any{"a": 1})
	if req.Method != "POST" || req.Path != "/v1/rules/test" {
		t.Fatalf("rules test %+v", req)
	}
	req = RuleTest{Match: "true", Rooms: []string{"r1"}}.Build("01E", "", nil)
	if b := req.Body.(map[string]any); b["event_id"] != "01E" || b["event"] != nil || len(b["rooms"].([]string)) != 1 {
		t.Fatalf("rules test by id %+v", req)
	}
	req = RuleTest{Match: "true"}.BuildBatch([]string{"01A", "01B"})
	if req.Path != "/v1/rules/test-batch" || len(req.Body.(map[string]any)["event_ids"].([]string)) != 2 {
		t.Fatalf("rules test batch %+v", req)
	}
	req = BuildRulesFire("mail", "ws", "job", "true", "", map[string]any{"a": 1}, 0)
	if req.Path != "/v1/rules/fire" {
		t.Fatalf("rules fire %+v", req)
	}
	req = BuildConnectorConnect(ConnectorRef{Ref: "mail"})
	if req.Path != "/v1/connectors/mail/connect" {
		t.Fatalf("connect %+v", req)
	}
	req = BuildConnectorDisconnect(ConnectorRef{Ref: "mail"})
	if req.Path != "/v1/connectors/mail/disconnect" {
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
	got, err = ParseJobListKind("agent")
	if err != nil || got != "agent" {
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
		{"kind": "agent", "name": "agents/cmo"},
		{"kind": "", "name": "c"},
	}
	jobs := FilterJobsByKindFlag(rows, "job")
	if len(jobs) != 2 || jobs[0]["name"] != "a" {
		t.Fatalf("%v", jobs)
	}
	ag := FilterJobsByKindFlag(rows, "agent")
	if len(ag) != 1 || ag[0]["name"] != "agents/cmo" {
		t.Fatalf("%v", ag)
	}
	all := FilterJobsByKindFlag(rows, "all")
	if len(all) != 3 {
		t.Fatalf("%v", all)
	}
	if _, err := ParseJobListKind("assistant"); err == nil {
		t.Fatal("assistant must no longer be a job list kind")
	}
}

func TestBuildJobPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"list", BuildJobsListPath("ws1", "", "", ""), "/v1/jobs?workspace=ws1"},
		{"list no workspace", BuildJobsListPath("", "", "", ""), "/v1/jobs"},
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
	del := BuildFileDelete("ws1", "a/b.py", "etag-2", false)
	if del.Method != "DELETE" || del.Path != "/v1/workspaces/ws1/files/a/b.py" || del.IfMatch != "etag-2" || del.Raw != nil {
		t.Fatalf("%+v", del)
	}
	if c := BuildFileDelete("ws1", "a/b.py", "", true); c.Path != "/v1/workspaces/ws1/files/a/b.py?confirm=1" {
		t.Fatalf("confirmed %+v", c)
	}
	jp := BuildJobFilePut("nightly", "ws1", "run.py", []byte("print(1)"), "", "")
	if jp.Path != "/v1/jobs/nightly/files/run.py?workspace=ws1" || jp.ContentType != "" || jp.IfMatch != "" {
		t.Fatalf("%+v", jp)
	}
	jd := BuildJobFileDelete("nightly", "ws1", "run.py", "e")
	if jd.Path != "/v1/jobs/nightly/files/run.py?workspace=ws1" || jd.IfMatch != "e" {
		t.Fatalf("%+v", jd)
	}
	sp := BuildSkillFilePut(SkillRef{Name: "deploy"}, "run.sh", []byte("set -e\n"), "", "e2")
	if sp.Path != "/v1/skills/deploy/files/run.sh" || sp.IfMatch != "e2" {
		t.Fatalf("%+v", sp)
	}
	sd := BuildSkillFileDelete(SkillRef{Name: "deploy"}, "run.sh", "")
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
		{"approve", BuildRunApprove("01ABC", "allow", ""), "POST", "/v1/runs/01ABC/approve", `{"decision":"allow"}`},
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
	if p := BuildRunGetPath("01ABC", 0); p != "/v1/runs/01ABC" {
		t.Fatalf("get %s", p)
	}
	if p := BuildRunGetPath("01ABC", 5000); p != "/v1/runs/01ABC?wait_ms=5000" {
		t.Fatalf("get wait %s", p)
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
	create := JobSpec{Name: "nightly", Workspace: "ws1", Content: "---\nharness: claude\n---\n"}.Build()
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
	if r := (RunSpec{Job: "nightly", Workspace: "ws1"}).Build(); r.Path != "/v1/jobs/nightly/run?workspace=ws1" {
		t.Fatalf("run %s", r.Path)
	}
	if r := (RunSpec{Job: "nightly"}).Build(); r.Path != "/v1/jobs/nightly/run" {
		t.Fatalf("run no ws %s", r.Path)
	}
}

func TestBuildWorkspaceAndAssistantRequests(t *testing.T) {
	t.Parallel()
	if p := BuildWorkspacesListPath(0); p != "/v1/workspaces" {
		t.Fatalf("list %s", p)
	}
	if p := BuildWorkspacesListPath(7); p != "/v1/workspaces?limit=7" {
		t.Fatalf("list limit %s", p)
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
}

func TestBuildHarnessConnectorQueueRequests(t *testing.T) {
	t.Parallel()
	if p := BuildHarnessesListPath(); p != "/v1/harnesses" {
		t.Fatalf("harnesses %s", p)
	}
	cancel := BuildHarnessLoginCancel("claude")
	if cancel.Method != "DELETE" || cancel.Path != "/v1/harnesses/claude/login" {
		t.Fatalf("%+v", cancel)
	}
	if p := BuildConnectorGetPath("mail", ""); p != "/v1/connectors/mail" {
		t.Fatalf("connector get %s", p)
	}
	if p := BuildConnectorGetPath("mail", "ws1"); p != "/v1/connectors/mail?workspace=ws1" {
		t.Fatalf("connector get %s", p)
	}
	def := map[string]any{"type": "mail", "slug": "mail-ops", "from": "catalog"}
	create := BuildConnectorCreate(def)
	if create.Method != "POST" || create.Path != "/v1/connectors" || create.Body.(map[string]any)["slug"] != "mail-ops" {
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
	if p := BuildSkillGetPath(SkillRef{Name: "deploy"}); p != "/v1/skills/deploy" {
		t.Fatalf("get %s", p)
	}
	sc := SkillSpec{Name: "deploy", SkillMD: "# SKILL.md\n"}.Build()
	if sc.Method != "POST" || sc.Path != "/v1/skills" {
		t.Fatalf("%+v", sc)
	}
	m, _ := sc.Body.(map[string]any)
	if m["name"] != "deploy" || m["skill_md"] != "# SKILL.md\n" {
		t.Fatalf("%v", m)
	}
	src, _ := SkillSpec{Name: "deploy", SourceGit: "git@host:x.git"}.Build().Body.(map[string]any)
	if _, ok := src["skill_md"]; ok {
		t.Fatalf("source spec must not send skill_md: %v", src)
	}
	if got := src["source"].(map[string]any); got["kind"] != "git" || got["url"] != "git@host:x.git" {
		t.Fatalf("%v", got)
	}
	up := BuildSkillRefresh(SkillRef{Name: "deploy"})
	if up.Method != "POST" || up.Path != "/v1/skills/deploy/refresh" {
		t.Fatalf("%+v", up)
	}
	del := BuildSkillDelete(SkillRef{Name: "deploy"}, true)
	if del.Method != "DELETE" || del.Path != "/v1/skills/deploy?confirm=1" {
		t.Fatalf("%+v", del)
	}
	if p := BuildSecretsListPath(0); p != "/v1/secrets" {
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
	sd := BuildSecretDelete("api_key", true)
	if sd.Method != "DELETE" || sd.Path != "/v1/secrets/api_key?confirm=1" {
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
	if BuildCloudAddressesPath(false) != "/v1/cloud/addresses" || BuildCloudAddressesPath(true) != "/v1/cloud/addresses?reveal=1" {
		t.Fatal("cloud addresses path")
	}
	if BuildHooksPath(true) != "/v1/hooks?reveal=1" {
		t.Fatal("hooks path")
	}
	cases := []struct {
		name string
		req  HTTPRequest
		want string
	}{
		{"drain", BuildSystemDrain(), "POST /v1/system/drain"},
		{"undrain", BuildSystemUndrain(), "POST /v1/system/undrain"},
		{"link", BuildCloudLink("ABCD-EFGH", "mbp"), "POST /v1/cloud/link"},
		{"unlink", BuildCloudUnlink(true), "DELETE /v1/cloud/link?confirm=1"},
		{"hook create", BuildHookCreate("ws1", "deploy"), "POST /v1/hooks"},
		{"hook delete", BuildHookDelete("01ABC", true), "DELETE /v1/hooks/01ABC?confirm=1"},
		{"config get", BuildConfigGet(), "GET /v1/config"},
	}
	for _, tc := range cases {
		if got := tc.req.Method + " " + tc.req.Path; got != tc.want {
			t.Fatalf("%s: %s", tc.name, got)
		}
	}
}

// TestIfMatchOnDocumentWrites covers A3: every file-backed document write
// carries If-Match, so a concurrent edit is refused instead of clobbered.
func TestIfMatchOnDocumentWrites(t *testing.T) {
	t.Parallel()
	cases := map[string]HTTPRequest{
		"job put":                BuildJobPutMarkdown("demo", "ws1", "# job", "h"),
		"job delete":             BuildJobDelete("demo", "ws1", "h", false, true),
		"job pause":              BuildJobPause("demo", "ws1", "h", true),
		"job unpause":            BuildJobPause("demo", "ws1", "h", false),
		"job memory put":         BuildJobMemoryPut("demo", "ws1", "m", "h", false),
		"job memory clear":       BuildJobMemoryClear("demo", "ws1", "h", true),
		"assistant override put": BuildAssistantSettingsPut("ws1", map[string]any{"mode": "ask"}, "h"),
		"assistant mem put":      BuildAssistantMemoryPut("ws1", "m", "h", false),
		"assistant mem clr":      BuildAssistantMemoryClear("ws1", "h", true),
		"skill put":              BuildSkillPut(SkillRef{Name: "demo"}, "# s", nil, "h"),
	}
	for name, req := range cases {
		if req.IfMatch != "h" {
			t.Errorf("%s: if-match %q, want h", name, req.IfMatch)
		}
	}
}

// TestBuildConfigSetValues covers the multi-key typed form of config.set.
func TestBuildConfigSetValues(t *testing.T) {
	t.Parallel()
	req := BuildConfigSetValues(map[string]any{"a": 1, "b": true})
	if req.Method != "PATCH" || req.Path != "/v1/config" {
		t.Fatalf("%+v", req)
	}
	body, _ := req.Body.(map[string]any)
	set, _ := body["set"].(map[string]any)
	if set["a"] != 1 || set["b"] != true {
		t.Fatalf("set %+v", body)
	}
}

// TestBuildSecretsListLimit covers the limit parameter added in A3.
func TestBuildSecretsListLimit(t *testing.T) {
	t.Parallel()
	if p := BuildSecretsListPath(3); p != "/v1/secrets?limit=3" {
		t.Fatalf("limit %s", p)
	}
	if p := BuildSecretsListPath(0); p != "/v1/secrets" {
		t.Fatalf("no limit %s", p)
	}
}

func TestBuildEmailRequests(t *testing.T) {
	t.Parallel()
	if p := BuildEmailStatusPath(); p != "/v1/email/status" {
		t.Fatalf("status %s", p)
	}
	test := BuildEmailTest()
	if test.Method != "POST" || test.Path != "/v1/email/test" {
		t.Fatalf("test %+v", test)
	}
	rot := BuildWorkspaceEmailRotate("ws_1", "", true)
	if rot.Method != "POST" || rot.Path != "/v1/workspaces/ws_1/email/rotate?confirm=1" || rot.Body != nil {
		t.Fatalf("rotate %+v", rot)
	}
	if bare := BuildWorkspaceEmailRotate("ws_1", "", false); bare.Path != "/v1/workspaces/ws_1/email/rotate" {
		t.Fatalf("rotate unconfirmed %+v", bare)
	}
	renamed := BuildWorkspaceEmailRotate("ws_1", "acme", true)
	if body, _ := renamed.Body.(map[string]any); body["handle"] != "acme" {
		t.Fatalf("rotate with handle %+v", renamed)
	}
	show := BuildEmailMessagePath("01A")
	if show != "/v1/email/messages/01A" {
		t.Fatalf("show %s", show)
	}
	att := BuildEmailAttachmentPath("01A", "a1")
	if att != "/v1/email/messages/01A/attachments/a1" {
		t.Fatalf("attachment %s", att)
	}
	poll := BuildEmailPoll()
	if poll.Method != "POST" || poll.Path != "/v1/email/poll" {
		t.Fatalf("poll %+v", poll)
	}
}

func TestBuildEmailMessagesQuery(t *testing.T) {
	t.Parallel()
	if p := BuildEmailMessagesPath("", "", "", "", 0); p != "/v1/email/messages" {
		t.Fatalf("bare %s", p)
	}
	p := BuildEmailMessagesPath("in", "ws_1", "<a@x>", "01B", 25)
	for _, want := range []string{
		"direction=in", "workspace=ws_1", "thread=%3Ca%40x%3E", "cursor=01B", "limit=25",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in %s", want, p)
		}
	}
	if p := BuildEmailMessagesPath("", "", "", "", 0); strings.Contains(p, "limit=") {
		t.Fatalf("limit must stay absent when zero: %s", p)
	}
}

func TestBuildWorkspaceEmailReveal(t *testing.T) {
	t.Parallel()
	if p := BuildWorkspaceEmailGetPath("ws_1", false); p != "/v1/workspaces/ws_1/email" {
		t.Fatalf("masked %s", p)
	}
	if p := BuildWorkspaceEmailGetPath("ws_1", true); p != "/v1/workspaces/ws_1/email?reveal=1" {
		t.Fatalf("revealed %s", p)
	}
}

func TestOutboxBuilders(t *testing.T) {
	t.Parallel()
	if got := BuildOutboxListPath(OutboxQuery{}); got != "/v1/outbox" {
		t.Fatalf("bare list %s", got)
	}
	got := BuildOutboxListPath(OutboxQuery{Kind: "email", Status: "failed", Workspace: "ws1", Limit: 5})
	for _, want := range []string{"/v1/outbox?", "kind=email", "status=failed", "workspace=ws1", "limit=5"} {
		if !strings.Contains(got, want) {
			t.Fatalf("list %s misses %s", got, want)
		}
	}
	if strings.Contains(got, "purpose=") {
		t.Fatalf("empty field in %s", got)
	}
	for _, tc := range []struct {
		req  HTTPRequest
		want string
	}{
		{BuildOutboxGet("01R"), "GET /v1/outbox/01R"},
		{BuildOutboxRetry("01R"), "POST /v1/outbox/01R/retry"},
		{BuildOutboxCancel("01R"), "POST /v1/outbox/01R/cancel"},
	} {
		if tc.req.Method+" "+tc.req.Path != tc.want {
			t.Fatalf("%+v, want %s", tc.req, tc.want)
		}
	}
}

func TestSpecEventScope(t *testing.T) {
	t.Parallel()
	job := JobSpec{Name: "n", Workspace: "ws1", Prompt: "p", Event: "whatsapp", Rooms: []string{"r1"}, Allowlist: []string{"u1"}}.Build()
	agent := AgentSpec{Name: "a", Workspace: "ws1", Prompt: "p", Event: "whatsapp", Rooms: []string{"r1"}, Allowlist: []string{"u1"}}.Build()
	for _, req := range []HTTPRequest{job, agent} {
		b, _ := json.Marshal(req.Body)
		if !strings.Contains(string(b), `"on":{"allowlist":["u1"],"event":"whatsapp","rooms":["r1"]}`) {
			t.Fatalf("%s %s", req.Path, b)
		}
	}
	bare, _ := json.Marshal(JobSpec{Name: "n", Event: "mail"}.Build().Body)
	if !strings.Contains(string(bare), `"on":{"event":"mail"}`) {
		t.Fatalf("bare %s", bare)
	}
}

func TestBuildState(t *testing.T) {
	t.Parallel()
	body := func(r HTTPRequest) string {
		b, _ := json.Marshal(r.Body)
		return string(b)
	}
	cases := []struct {
		name, method, path, body string
		req                      HTTPRequest
	}{
		{"summary self", "GET", "/v1/state/self", "null", BuildStateSummary("self", "")},
		{"summary name", "GET", "/v1/state/digest?workspace=ws1", "null", BuildStateSummary("digest", "ws1")},
		{"query", "POST", "/v1/state/jb_0123456789/reported/query", `{"q":"alz","since":"7d","fields":["key","title"],"limit":5}`,
			BuildStateQuery("jb_0123456789", "", "reported", StateQuery{Q: "alz", Since: "7d", Fields: []string{"key", "title"}, Limit: 5})},
		{"get", "POST", "/v1/state/self/reported/get", `{"keys":["doi:10/1"]}`, BuildStateGet("self", "", "reported", []string{"doi:10/1"})},
		{"has", "POST", "/v1/state/self/reported/has", `{"keys":["a","b"]}`, BuildStateHas("self", "", "reported", []string{"a", "b"})},
		{"put", "POST", "/v1/state/self/reported/rows", `{"mode":"merge","rows":[{"key":"a"}]}`,
			BuildStatePut("self", "", "reported", []any{map[string]any{"key": "a"}}, "merge")},
		{"put upsert", "POST", "/v1/state/self/reported/rows", `{"rows":[]}`, BuildStatePut("self", "", "reported", []any{}, "")},
		{"delete", "POST", "/v1/state/agt_0123456789/reported/delete?confirm=1", `{"keys":["a"]}`,
			BuildStateDelete("agt_0123456789", "", "reported", []string{"a"}, true)},
		{"prune", "POST", "/v1/state/digest/reported/prune?workspace=ws1", `{"until":"180d"}`,
			BuildStatePrune("digest", "ws1", "reported", "", "180d", "", false)},
		{"drop", "DELETE", "/v1/state/self/reported?confirm=1", "null", BuildStateDrop("self", "", "reported", true)},
	}
	for _, c := range cases {
		if c.req.Method != c.method || c.req.Path != c.path || body(c.req) != c.body {
			t.Errorf("%s: got %s %s %s", c.name, c.req.Method, c.req.Path, body(c.req))
		}
	}
}

func TestArtifactBuilders(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ ref, name, n string }{
		{"weekly", "weekly", "latest"}, {"weekly@3", "weekly", "3"}, {"art_01x", "art_01x", "latest"},
	} {
		if name, n := SplitArtifactRef(c.ref); name != c.name || n != c.n {
			t.Errorf("%s: %s %s", c.ref, name, n)
		}
	}
	a := ArtifactRef{Ref: "weekly", Workspace: "ws1"}
	base := 2
	live := true
	for _, c := range []struct {
		name string
		req  HTTPRequest
		want string
	}{
		{"list", HTTPRequest{Method: "GET", Path: BuildArtifactListPath(ArtifactFilter{Workspace: "ws1", Live: &live, Limit: 5})}, "GET /v1/artifacts?limit=5&live=true&workspace=ws1"},
		{"content", HTTPRequest{Method: "GET", Path: a.ContentPath("3", ContentOpts{Download: true})}, "GET /v1/artifacts/weekly/versions/3/content?download=1&workspace=ws1"},
		{"pin off", a.Pin("3", false), "DELETE /v1/artifacts/weekly/versions/3/pin?workspace=ws1"},
		{"delete", a.Delete(true), "DELETE /v1/artifacts/weekly?confirm=1&workspace=ws1"},
		{"send", a.Send("latest", "draft"), `POST /v1/artifacts/weekly/versions/latest/send?workspace=ws1 {"mode":"draft"}`},
		{"put", ArtifactPutSpec{Workspace: "ws1", Name: "weekly", Content: "x", Base: &base}.Build(), `POST /v1/artifacts {"base":2,"content":"x","name":"weekly","workspace":"ws1"}`},
		{"call", BuildOperationCall("pg", "query", "", map[string]any{"sql": "select 1"}), `POST /v1/connectors/pg/operations/query/call {"input":{"sql":"select 1"}}`},
		{"state", BuildOperationState("pg", "query", "ask"), `PUT /v1/connectors/pg/operations/query {"state":"ask"}`},
	} {
		got := c.req.Method + " " + c.req.Path
		if c.req.Body != nil {
			b, _ := json.Marshal(c.req.Body)
			got += " " + string(b)
		}
		if got != c.want {
			t.Errorf("%s: %s", c.name, got)
		}
	}
	if r := a.Patch(nil, nil, 4); r.IfMatch != "4" || r.Method != "PATCH" {
		t.Errorf("patch: %+v", r)
	}
}

func TestBuildJobMove(t *testing.T) {
	t.Parallel()
	none, folder := "", "SEO"
	cases := []struct {
		m    JobMove
		want string
	}{
		{JobMove{Folder: &folder}, `{"folder":"SEO"}`},
		{JobMove{Agent: &none}, `{"agent":""}`},
		{JobMove{Agent: &folder, Folder: &none, DryRun: true}, `{"agent":"SEO","folder":"","dry_run":true}`},
	}
	for _, c := range cases {
		req := BuildJobMove("ws1", "agents/cmo/x", c.m)
		if req.Method != "POST" || req.Path != "/v1/jobs/agents%2Fcmo%2Fx/move?workspace=ws1" {
			t.Fatalf("method/path: %+v", req)
		}
		if b, _ := json.Marshal(req.Body); string(b) != c.want {
			t.Fatalf("body=%s want %s", b, c.want)
		}
	}
	req := BuildAgentFolderRename("ws1", "cmo", "Customers", "")
	if req.Path != "/v1/workspaces/ws1/agents/cmo/folders/rename" {
		t.Fatalf("path=%s", req.Path)
	}
	if b, _ := json.Marshal(req.Body); string(b) != `{"from":"Customers","to":""}` {
		t.Fatalf("body=%s", b)
	}
}

func TestBuildSkillScopeRequests(t *testing.T) {
	t.Parallel()
	r := SkillRef{Name: "deploy", Scope: "agent", Owner: "support", Workspace: "ops"}
	if p := BuildSkillGetPath(r); p != "/v1/skills/deploy?owner=support&scope=agent&workspace=ops" {
		t.Fatalf("get %s", p)
	}
	if p := BuildSkillFileGetPath(r, "a b.py", true); p != "/v1/skills/deploy/files/a%20b.py?owner=support&scope=agent&stat=1&workspace=ops" {
		t.Fatalf("file get %s", p)
	}
	mv := BuildSkillMove(SkillRef{Name: "deploy"}, SkillMove{ToScope: "workspace", ToOwner: "ops", Replace: true})
	body, _ := mv.Body.(map[string]any)
	if mv.Path != "/v1/skills/deploy/move" || body["to_scope"] != "workspace" || body["to_owner"] != "ops" || body["replace"] != true {
		t.Fatalf("move %+v", mv)
	}
	if _, ok := body["rename"]; ok {
		t.Fatalf("empty rename sent: %v", body)
	}
	if ap := BuildSkillApprove(r); ap.Method != "POST" || ap.Path != "/v1/skills/deploy/approve?owner=support&scope=agent&workspace=ops" {
		t.Fatalf("approve %+v", ap)
	}
	if p := (SkillList{EffectiveFor: "agents/support", Workspace: "ops", Limit: 5}).Path(); p != "/v1/skills?limit=5&effective_for=agents%2Fsupport&workspace=ops" {
		t.Fatalf("list %s", p)
	}
	sc, _ := SkillSpec{Name: "deploy", SkillMD: "x", Scope: "job", Owner: "daily", Note: "share"}.Build().Body.(map[string]any)
	if sc["scope"] != "job" || sc["owner"] != "daily" || sc["note"] != "share" {
		t.Fatalf("create %v", sc)
	}
	if _, ok := sc["workspace"]; ok {
		t.Fatalf("empty workspace sent: %v", sc)
	}
}
