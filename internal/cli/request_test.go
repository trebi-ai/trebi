package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/request"
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
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	text, err := client.RunLogText("01ABC")
	if err != nil {
		t.Fatal(err)
	}
	if text != "line1\nline2\n" {
		t.Fatalf("%q", text)
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
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/run"):
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

	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	// run dry_run + prompt
	req := request.RunSpec{Job: "demo", Workspace: "ws", Prompt: "hi", DryRun: true}.Build()
	var out map[string]any
	if err := client.DoHTTP(req, &out); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotPath != "/v1/jobs/demo/run?workspace=ws" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"dry_run":true`) || !strings.Contains(gotBody, `"prompt_override":"hi"`) {
		t.Fatalf("body %s", gotBody)
	}
	if out["id"] != "01TEST" {
		t.Fatalf("out %+v", out)
	}

	// job delete
	req = request.BuildJobDelete("demo", "ws1", "", false, true)
	if err := client.DoHTTP(req, nil); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "DELETE" || !strings.Contains(gotPath, "/v1/jobs/demo") || !strings.Contains(gotPath, "workspace=ws1") {
		t.Fatalf("delete %s %s", gotMethod, gotPath)
	}

	// connector restart
	req = request.BuildConnectorRestart("mail")
	if err := client.DoHTTP(req, nil); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotPath != "/v1/connectors/mail/restart" {
		t.Fatalf("restart %s %s", gotMethod, gotPath)
	}

	// config set
	req = request.BuildConfigSet("defaults.command", "grok")
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
	req = request.BuildHarnessEnsure("claude")
	if err := client.DoHTTP(req, &out); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotPath != "/v1/harnesses/claude/ensure" {
		t.Fatalf("ensure %s %s", gotMethod, gotPath)
	}
}

// TestDoHTTPRawRoundTrip covers the file-plane fields: Raw body, ContentType,
// and If-Match on PUT, plus If-Match on DELETE.
func TestDoHTTPRawRoundTrip(t *testing.T) {
	var gotBody, gotContentType, gotIfMatch string
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotContentType = r.Header.Get("Content-Type")
		gotIfMatch = r.Header.Get("If-Match")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"path":"a.py"}`))
	}))
	defer srv.Close()

	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	req := request.HTTPRequest{
		Method:      "PUT",
		Path:        "/v1/workspaces/ws1/files/a.py",
		Raw:         []byte("print(1)\n"),
		ContentType: "text/x-python",
		IfMatch:     "etag-1",
	}
	var out map[string]any
	if err := client.DoHTTP(req, &out); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "PUT" || gotPath != "/v1/workspaces/ws1/files/a.py" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	if gotBody != "print(1)\n" || gotContentType != "text/x-python" || gotIfMatch != "etag-1" {
		t.Fatalf("body=%q ct=%q ifmatch=%q", gotBody, gotContentType, gotIfMatch)
	}
	if out["path"] != "a.py" {
		t.Fatalf("out %+v", out)
	}

	// Default content type for raw bodies.
	req = request.HTTPRequest{Method: "PUT", Path: "/v1/workspaces/ws1/files/b.py", Raw: []byte("x")}
	if err := client.DoHTTP(req, nil); err != nil {
		t.Fatal(err)
	}
	if gotContentType != "application/octet-stream" {
		t.Fatalf("default ct=%q", gotContentType)
	}

	// DELETE with If-Match.
	req = request.HTTPRequest{Method: "DELETE", Path: "/v1/workspaces/ws1/files/a.py", IfMatch: "etag-2"}
	if err := client.DoHTTP(req, nil); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "DELETE" || gotIfMatch != "etag-2" || gotBody != "" {
		t.Fatalf("delete %s ifmatch=%q body=%q", gotMethod, gotIfMatch, gotBody)
	}

	// A JSON Body plus If-Match must still send the body. `trebi job schedule`
	// takes this path, and an empty body reaches the API as a 400.
	req = request.BuildJobPutMarkdown("report", "ws1", "---\ntitle: r\n---\n", "hash-1")
	if err := client.DoHTTP(req, nil); err != nil {
		t.Fatal(err)
	}
	if gotIfMatch != "hash-1" || gotContentType != "application/json" {
		t.Fatalf("ifmatch=%q ct=%q", gotIfMatch, gotContentType)
	}
	if !strings.Contains(gotBody, `"markdown"`) {
		t.Fatalf("body %q", gotBody)
	}
}

func TestCommandRegistration(t *testing.T) {
	t.Parallel()
	app := NewApp(Options{})
	names := map[string]bool{}
	for _, c := range app.Commands {
		names[c.Name] = true
	}
	for _, want := range []string{"runs", "job", "harness", "connector", "config", "service", "conversation", "event", "queue", "rules", "bus", "doctor"} {
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
	connector := findCmd(app.Commands, "connector")
	for _, want := range []string{"list", "show", "set", "apply", "remove", "restart", "logs", "test", "connect", "disconnect", "cursor"} {
		if findCmd(connector.Commands, want) == nil {
			t.Fatalf("connector missing %s", want)
		}
	}
	cur := findCmd(connector.Commands, "cursor")
	for _, want := range []string{"show", "set", "reset"} {
		if findCmd(cur.Commands, want) == nil {
			t.Fatalf("connector cursor missing %s", want)
		}
	}
	if findCmd(connector.Commands, "queue") == nil || !findCmd(connector.Commands, "queue").Hidden {
		t.Fatal("connector queue should remain as a hidden alias")
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
	for _, want := range []string{"stats", "sessions", "approvals"} {
		if findCmd(runs.Commands, want) == nil {
			t.Fatalf("runs missing %s", want)
		}
	}
	ev := findCmd(app.Commands, "events")
	if findCmd(ev.Commands, "retrigger") == nil {
		t.Fatal("events retrigger missing")
	}
	mem := findCmd(findCmd(app.Commands, "job").Commands, "memory")
	for _, want := range []string{"show", "validate", "migrate", "set-context", "set-state", "lesson", "log", "set", "clear", "add"} {
		if findCmd(mem.Commands, want) == nil {
			t.Fatalf("job memory missing %s", want)
		}
	}
	if !hasFlag(findCmd(mem.Commands, "clear"), "force") {
		t.Fatal("job memory clear missing --force")
	}
	logAdd := findCmd(findCmd(mem.Commands, "log").Commands, "add")
	if logAdd == nil || !hasFlag(logAdd, "text") || !hasFlag(logAdd, "at") {
		t.Fatal("job memory log add flags")
	}
	if findCmd(findCmd(mem.Commands, "log").Commands, "delete") == nil {
		t.Fatal("job memory log delete missing")
	}
	offline := findCmd(app.Commands, "memory")
	if offline == nil || findCmd(offline.Commands, "validate") == nil || findCmd(offline.Commands, "skeleton") == nil {
		t.Fatal("offline memory validate/skeleton missing")
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
	for _, want := range []string{"show", "set", "say", "chat", "sessions", "close", "interrupt", "preview"} {
		if findCmd(asst.Commands, want) == nil {
			t.Fatalf("assistant missing %s", want)
		}
	}
	for _, gone := range []string{"wake", "history", "patch", "delete"} {
		if findCmd(asst.Commands, gone) != nil {
			t.Fatalf("assistant must no longer have %s", gone)
		}
	}
	conv := findCmd(app.Commands, "conversation")
	for _, want := range []string{"list", "show", "messages", "new", "switch", "delete", "outbox"} {
		if findCmd(conv.Commands, want) == nil {
			t.Fatalf("conversation missing %s", want)
		}
	}
	if findCmd(findCmd(conv.Commands, "outbox").Commands, "retry") == nil {
		t.Fatal("conversation outbox missing retry")
	}
	setCmd := findCmd(asst.Commands, "set")
	for _, f := range []string{"harness", "model", "effort", "envset", "mode", "skill", "context-file", "disabled", "unset", "defaults", "workspace"} {
		if !hasFlag(setCmd, f) {
			t.Fatalf("assistant set missing flag %s", f)
		}
	}
	if findCmd(app.Commands, "remote") != nil {
		t.Fatal("trebi remote should be removed")
	}
	ws := findCmd(app.Commands, "workspace")
	for _, want := range []string{"list", "add", "use", "current", "unlink", "remove", "rename", "move"} {
		if findCmd(ws.Commands, want) == nil {
			t.Fatalf("workspace missing %s", want)
		}
	}
	if !hasFlag(findCmd(ws.Commands, "remove"), "force") {
		t.Fatal("workspace remove missing --force")
	}
	move := findCmd(ws.Commands, "move")
	if !hasFlag(move, "force") || !hasFlag(move, "mkdir") || !hasFlag(move, "dry-run") {
		t.Fatal("workspace move missing --force/--mkdir/--dry-run")
	}
	asstMem := findCmd(findCmd(app.Commands, "assistant").Commands, "memory")
	for _, want := range []string{"show", "validate", "migrate", "set-context", "set-state", "lesson", "log", "set", "clear", "add"} {
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
	if !hasFlag(add, "url") || !hasFlag(add, "id") {
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

// findCmd resolves a command by its name or by any alias, so a renamed
// command still answers to the spelling it replaced.
func findCmd(cmds []*urfaveCommand, name string) *urfaveCommand {
	for _, c := range cmds {
		if c == nil {
			continue
		}
		if c.Name == name || slices.Contains(c.Aliases, name) {
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
