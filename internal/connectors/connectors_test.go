package connectors_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// eventsDef is a minimal valid events connector.
func eventsDef(name, command string) connectors.Definition {
	return connectors.Definition{Name: name, Events: &connectors.Events{Command: command, Format: "json"}}
}

func TestConnectorsCreateReplacePatchDelete(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()

	d := connectors.Definition{
		Name: "feed",
		Events: &connectors.Events{
			Command: "rss-fetch https://example.com/feed.xml", Format: "json", ID: "guid",
		},
	}
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home.ConnectorsPath()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(ctx, d); !errors.Is(err, connectors.ErrExists) {
		t.Fatalf("double create: %v", err)
	}
	if err := svc.Create(ctx, connectors.Definition{Name: "email", Events: &connectors.Events{Command: "x"}}); !errors.Is(err, connectors.ErrReserved) {
		t.Fatalf("reserved create: %v", err)
	}

	// Replace swaps the whole definition.
	replaced := d
	replaced.Events = &connectors.Events{Command: "rss-fetch v2", Format: "line"}
	if err := svc.Replace(ctx, replaced); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Get("feed")
	if err != nil {
		t.Fatal(err)
	}
	ev := row["events"].(map[string]any)
	if ev["command"] != "rss-fetch v2" || ev["id"] != "" {
		t.Fatalf("replace row %v", ev)
	}

	// Patch replaces one block and removes another with null.
	env := map[string]string{"FEED": "https://example.com/x"}
	cwd := "/tmp"
	p := connectors.Patch{Env: &env, Cwd: &cwd, Events: []byte(`{"command":"rss-fetch v3","format":"json","id":"guid"}`)}
	if err := svc.Patch(ctx, "feed", p); err != nil {
		t.Fatal(err)
	}
	row, _ = svc.Get("feed")
	if row["cwd"] != "/tmp" || row["env"].(map[string]string)["FEED"] != "***" {
		t.Fatalf("patch env/cwd %v", row)
	}
	if d, _ := svc.Definition("feed"); d.Env["FEED"] != "https://example.com/x" {
		t.Fatalf("stored env %v", d.Env)
	}
	if ev := row["events"].(map[string]any); ev["command"] != "rss-fetch v3" || ev["id"] != "guid" {
		t.Fatalf("patch events %v", ev)
	}

	// Add actions and a channel, then remove actions with null.
	add := connectors.Patch{
		Actions: []byte(`{"cli":{"commands":["rss-fetch"],"skill":"feeds"}}`),
		Channel: []byte(`{"ref":"chat","send":"rss-send"}`),
	}
	if err := svc.Patch(ctx, "feed", add); err != nil {
		t.Fatal(err)
	}
	row, _ = svc.Get("feed")
	caps := row["capabilities"].([]string)
	if strings.Join(caps, ",") != "actions,events,channel" {
		t.Fatalf("capabilities %v", caps)
	}
	// An unknown key in a patch block is a validation error, not a silent drop.
	for field, p := range map[string]connectors.Patch{
		"events":  {Events: []byte(`{"command":"x","formt":"json"}`)},
		"actions": {Actions: []byte(`{"cli":{"commands":["a"]},"mpc":{}}`)},
		"channel": {Channel: []byte(`{"ref":"chat","send":"s","typo":1}`)},
	} {
		var ve *connectors.ValidationError
		err := svc.Patch(ctx, "feed", p)
		if !errors.As(err, &ve) || ve.Fields[0].Field != field {
			t.Fatalf("%s: unknown key err = %v", field, err)
		}
	}
	if err := svc.Patch(ctx, "feed", connectors.Patch{Actions: []byte("null")}); err != nil {
		t.Fatal(err)
	}
	row, _ = svc.Get("feed")
	if row["actions"] != nil {
		t.Fatalf("actions after null: %v", row["actions"])
	}
	if _, ok := row["channel"].(map[string]any); !ok {
		t.Fatalf("channel survived actions removal: %v", row)
	}

	// Delete twice: the second is ErrNotFound.
	if err := svc.Delete(ctx, "feed"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, "feed"); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestConnectorsRewritePathPrefix(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	oldDir, newDir := "/tmp/old-ws", "/tmp/new-ws"
	d := connectors.Definition{
		Name: "whatsapp",
		Cwd:  oldDir,
		Env:  map[string]string{"ROOT": oldDir + "/data"},
		Actions: &connectors.Actions{
			MCP: &connectors.MCPAction{Command: "npx", Args: []string{"--root", oldDir + "/mcp"}},
			CLI: &connectors.CLIAction{Commands: []string{"whatsapp-cli"}},
		},
		Events: &connectors.Events{
			Command: "whatsapp-cli listen --dir " + oldDir,
			Format:  "json",
			ID:      "d.id",
			Catchup: &connectors.Catchup{Command: "whatsapp-cli back --dir " + oldDir},
		},
		Channel: &connectors.Channel{
			Ref: "chat", Send: "whatsapp-cli send " + oldDir,
			Typing: "whatsapp-cli typing " + oldDir, Ack: "whatsapp-cli ack " + oldDir,
		},
	}
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	preview, err := svc.RewritePathPrefix(oldDir, newDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 1 || preview[0] != "whatsapp" {
		t.Fatalf("preview %v", preview)
	}
	got, _ := svc.Get("whatsapp")
	if got["cwd"] != oldDir {
		t.Fatalf("dry-run wrote cwd %v", got["cwd"])
	}
	names, err := svc.RewritePathPrefix(oldDir, newDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatalf("names %v", names)
	}
	got, _ = svc.Get("whatsapp")
	if got["cwd"] != newDir {
		t.Fatalf("cwd %v", got["cwd"])
	}
	if d, _ := svc.Definition("whatsapp"); d.Env["ROOT"] != newDir+"/data" {
		t.Fatalf("env %v", d.Env)
	}
	ev := got["events"].(map[string]any)
	if ev["command"] != "whatsapp-cli listen --dir "+newDir {
		t.Fatalf("events command %v", ev["command"])
	}
	if ev["catchup"].(map[string]any)["command"] != "whatsapp-cli back --dir "+newDir {
		t.Fatalf("catchup %v", ev["catchup"])
	}
	ch := got["channel"].(map[string]any)
	for _, k := range []string{"send", "typing", "ack"} {
		if !strings.Contains(ch[k].(string), newDir) {
			t.Fatalf("channel %s %v", k, ch[k])
		}
	}
	acts := got["actions"].(map[string]any)
	if acts["mcp"].(map[string]any)["args"].([]string)[1] != newDir+"/mcp" {
		t.Fatalf("mcp args %v", acts["mcp"])
	}
}

// TestConnectorsListRowShape pins the wire row fields of Part B.3.
func TestConnectorsListRowShape(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	d := connectors.Definition{
		Name: "github",
		Env:  map[string]string{"GITHUB_TOKEN": "secret:github"},
		Actions: &connectors.Actions{
			MCP: &connectors.MCPAction{
				URL:     "https://mcp.example.com/github",
				Headers: map[string]string{"Authorization": "Bearer tok", "X-Ref": "secret:github"},
			},
			CLI: &connectors.CLIAction{Commands: []string{"definitely-not-on-path-xyz"}, Skill: "github"},
		},
		Source: &connectors.Source{Registry: "official", Name: "io.github.x/server-github", Version: "1.4.0"},
	}
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Get("github")
	if err != nil {
		t.Fatal(err)
	}
	if row["provenance"] != "managed" || row["managed"] != true {
		t.Fatalf("provenance %v", row)
	}
	if caps := row["capabilities"].([]string); strings.Join(caps, ",") != "actions" {
		t.Fatalf("capabilities %v", caps)
	}
	if row["events"] != nil || row["channel"] != nil {
		t.Fatalf("absent blocks must be nil: %v", row)
	}
	src := row["source"].(*connectors.Source)
	if src.Name != "io.github.x/server-github" || src.Version != "1.4.0" {
		t.Fatalf("source %+v", src)
	}
	acts := row["actions"].(map[string]any)
	mcp := acts["mcp"].(map[string]any)
	if mcp["url"] != "https://mcp.example.com/github" {
		t.Fatalf("mcp %v", mcp)
	}
	headers := mcp["headers"].(map[string]string)
	if headers["Authorization"] != "***" {
		t.Fatalf("plain header not masked: %v", headers)
	}
	if headers["X-Ref"] != "secret:github" {
		t.Fatalf("secret header masked: %v", headers)
	}
	cli := acts["cli"].(map[string]any)
	if len(cli["missing"].([]string)) != 1 || cli["missing"].([]string)[0] != "definitely-not-on-path-xyz" {
		t.Fatalf("missing %v", cli["missing"])
	}

	// An events row carries the live fields even when stopped.
	d2 := eventsDef("feed", "echo hi")
	if err := svc.Create(ctx, d2); err != nil {
		t.Fatal(err)
	}
	row, _ = svc.Get("feed")
	ev := row["events"].(map[string]any)
	if ev["status"] != "stopped" || ev["format"] != "json" || ev["last_err"] != "" {
		t.Fatalf("events row %v", ev)
	}
	counters := ev["counters"].(map[string]any)
	if counters["events"] != int64(0) || counters["duplicates"] != int64(0) || counters["parse_errors"] != int64(0) {
		t.Fatalf("counters %v", counters)
	}
}

func TestConnectorsStrictLoadRefusesOldKeys(t *testing.T) {
	svc, teardown := stackWithFile(t, "mail:\n  type: stream\n  events: {command: x}\n")
	defer teardown()
	if err := svc.Load(); err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("load: %v", err)
	}
	// A valid map loads and applies defaults.
	svc2, teardown2 := stackWithFile(t, "feed:\n  events: {command: echo, format: json, id: g}\n")
	defer teardown2()
	if err := svc2.Load(); err != nil {
		t.Fatal(err)
	}
	row, err := svc2.Get("feed")
	if err != nil {
		t.Fatal(err)
	}
	ev := row["events"].(map[string]any)
	if ev["restart"] != "on-failure" {
		t.Fatalf("default restart %v", ev["restart"])
	}
}

// TestConnectorsFailedLoadBlocksWrites proves a failed load keeps the error
// and refuses every write until a load succeeds.
func TestConnectorsFailedLoadBlocksWrites(t *testing.T) {
	body := "mail:\n  type: stream\n  events: {command: x}\n"
	svc, teardown := stackWithFile(t, body)
	defer teardown()
	ctx := context.Background()
	if err := svc.Load(); err == nil {
		t.Fatal("load passed")
	}
	if svc.LoadErr() == nil {
		t.Fatal("LoadErr is nil after a failed load")
	}
	def := eventsDef("feed", "echo")
	env := map[string]string{"A": "1"}
	for name, write := range map[string]func() error{
		"create":  func() error { return svc.Create(ctx, def) },
		"replace": func() error { return svc.Replace(ctx, def) },
		"patch":   func() error { return svc.Patch(ctx, "mail", connectors.Patch{Env: &env}) },
		"delete":  func() error { return svc.Delete(ctx, "mail") },
		"rewrite": func() error { _, err := svc.RewritePathPrefix("/a", "/b", false); return err },
	} {
		if err := write(); !errors.Is(err, connectors.ErrLoadFailed) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got, _ := os.ReadFile(home.ConnectorsPath()); string(got) != body {
		t.Fatalf("file changed: %q", got)
	}
	if err := os.WriteFile(home.ConnectorsPath(), []byte("mail:\n  events: {command: x}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(); err != nil || svc.LoadErr() != nil {
		t.Fatalf("load after fix: %v %v", err, svc.LoadErr())
	}
	if err := svc.Create(ctx, def); err != nil {
		t.Fatalf("create after fix: %v", err)
	}
}

// TestConnectorsMaskedValuesRoundTrip proves a masked value that comes back on
// a write keeps the stored value, and that GET hides every literal.
func TestConnectorsMaskedValuesRoundTrip(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	d := connectors.Definition{
		Name: "api",
		Env:  map[string]string{"TOKEN": "t0k", "REF": "secret:api_TOKEN"},
		Actions: &connectors.Actions{MCP: &connectors.MCPAction{
			URL:     "https://mcp.example.com",
			Headers: map[string]string{"X-Key": "abc", "Authorization": "Bearer secret:api_KEY"},
		}},
	}
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(home.ConnectorsPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", fi.Mode(), err)
	}
	row, _ := svc.Get("api")
	if env := row["env"].(map[string]string); env["TOKEN"] != "***" || env["REF"] != "secret:api_TOKEN" {
		t.Fatalf("env %v", env)
	}
	hdr := row["actions"].(map[string]any)["mcp"].(map[string]any)["headers"].(map[string]string)
	if hdr["X-Key"] != "***" || hdr["Authorization"] != "Bearer secret:api_KEY" {
		t.Fatalf("headers %v", hdr)
	}

	// A merge that does not name the headers keeps them.
	if err := svc.Patch(ctx, "api", connectors.Patch{ActionsMerge: []byte(`{"mcp":{"url":"https://mcp2.example.com"}}`)}); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Definition("api")
	if got.Actions.MCP.URL != "https://mcp2.example.com" || got.Actions.MCP.Headers["X-Key"] != "abc" {
		t.Fatalf("merge %+v", got.Actions.MCP)
	}

	// A whole-block PATCH and a PUT that send the mask back keep "abc".
	if err := svc.Patch(ctx, "api", connectors.Patch{ActionsMerge: []byte(`{"mcp":{"headers":{"X-Key":"***","Y":"1"}}}`)}); err != nil {
		t.Fatal(err)
	}
	put := d
	put.Env = map[string]string{"TOKEN": "***"}
	put.Actions = &connectors.Actions{MCP: &connectors.MCPAction{URL: "https://mcp.example.com", Headers: map[string]string{"X-Key": "***"}}}
	if err := svc.Replace(ctx, put); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Definition("api")
	if got.Actions.MCP.Headers["X-Key"] != "abc" || got.Env["TOKEN"] != "t0k" {
		t.Fatalf("put %+v %v", got.Actions.MCP, got.Env)
	}

	// A mask with no stored value is a validation error.
	put.Env = map[string]string{"NEW": "***"}
	if err := svc.Replace(ctx, put); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("new mask: %v", err)
	}
}

// TestConnectorsActionsMerge proves the field merge, the transport switch,
// and sub-block removal.
func TestConnectorsActionsMerge(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	d := connectors.Definition{
		Name: "gh",
		Actions: &connectors.Actions{
			MCP: &connectors.MCPAction{URL: "https://mcp.example.com", Headers: map[string]string{"X-Key": "abc"}},
			CLI: &connectors.CLIAction{Commands: []string{"gh"}, Skill: "gh"},
		},
	}
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	merge := func(body string) error {
		return svc.Patch(ctx, "gh", connectors.Patch{ActionsMerge: []byte(body)})
	}
	if err := merge(`{"mcp":{"command":"npx","args":["-v"]}}`); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Definition("gh")
	if m := got.Actions.MCP; m.Command != "npx" || m.URL != "" || m.Headers != nil || len(m.Args) != 1 {
		t.Fatalf("command switch %+v", m)
	}
	if c := got.Actions.CLI; c == nil || c.Skill != "gh" {
		t.Fatalf("cli touched %+v", c)
	}
	if err := merge(`{"mcp":{"env":{"A":"1"}}}`); err != nil {
		t.Fatal(err)
	}
	if err := merge(`{"mcp":{"url":"https://x.example.com"}}`); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Definition("gh")
	if m := got.Actions.MCP; m.Command != "" || m.Args != nil || m.Env != nil || m.URL != "https://x.example.com" {
		t.Fatalf("url switch %+v", m)
	}
	if err := merge(`{"mcp":{"bogus":1}}`); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("unknown field: %v", err)
	}
	if err := merge(`{"mcp":null}`); err != nil {
		t.Fatal(err)
	}
	if err := merge(`{"cli":null}`); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("empty connector: %v", err)
	}
	got, _ = svc.Definition("gh")
	if got.Actions == nil || got.Actions.MCP != nil || got.Actions.CLI == nil {
		t.Fatalf("remove mcp %+v", got.Actions)
	}
}

func TestConnectorsInheritedUnionAndGuard(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	user := t.TempDir()
	claude := filepath.Join(user, ".claude.json")
	if err := os.WriteFile(claude, []byte(`{"mcpServers":{"sling":{"command":"sling","args":["serve","mcp"]}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	svc.WithUserHome(user)

	list := svc.List()
	if len(list) != 1 {
		t.Fatalf("list=%v", list)
	}
	row := list[0]
	if row["name"] != "sling" || row["provenance"] != "inherited" || row["managed"] != false {
		t.Fatalf("row %+v", row)
	}
	if caps := row["capabilities"].([]string); strings.Join(caps, ",") != "actions" {
		t.Fatalf("capabilities %v", caps)
	}
	if row["events"] != nil {
		t.Fatalf("inherited events: %v", row)
	}
	mcp := row["actions"].(map[string]any)["mcp"].(map[string]any)
	if mcp["command"] != "sling serve mcp" || mcp["transport"] != "stdio" {
		t.Fatalf("mcp %v", mcp)
	}
	got, err := svc.Get("sling")
	if err != nil {
		t.Fatal(err)
	}
	if got["id"] != "inherited:sling" {
		t.Fatalf("id %v", got["id"])
	}
	if err := svc.Start(ctx, "sling"); !errors.Is(err, connectors.ErrInherited) {
		t.Fatalf("start inherited: %v", err)
	}
	if err := svc.Delete(ctx, "sling"); !errors.Is(err, connectors.ErrInherited) {
		t.Fatalf("delete inherited: %v", err)
	}

	// A managed connector stays a separate row.
	if err := svc.Create(ctx, eventsDef("mail", "echo")); err != nil {
		t.Fatal(err)
	}
	if list = svc.List(); len(list) != 2 {
		t.Fatalf("managed+inherited %d %v", len(list), list)
	}
}

// TestConnectorsMCPFingerprintMerge folds an inherited server into the
// managed connector that declares the same MCP action.
func TestConnectorsMCPFingerprintMerge(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	d := connectors.Definition{
		Name:    "sling",
		Actions: &connectors.Actions{MCP: &connectors.MCPAction{Command: "sling", Args: []string{"serve", "mcp"}}},
	}
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	user := t.TempDir()
	if err := os.WriteFile(filepath.Join(user, ".claude.json"), []byte(`{"mcpServers":{"sling":{"command":"sling","args":["serve","mcp"]}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	svc.WithUserHome(user)
	list := svc.List()
	if len(list) != 1 {
		t.Fatalf("want merged 1 row, got %v", list)
	}
	row := list[0]
	if row["managed"] != true || row["name"] != "sling" {
		t.Fatalf("%+v", row)
	}
	if _, ok := row["origins"]; !ok {
		t.Fatalf("origins missing %+v", row)
	}
}

func TestConnectorsInheritedAmbiguousName(t *testing.T) {
	_, _, svc := openStack(t)
	user := t.TempDir()
	if err := os.MkdirAll(filepath.Join(user, ".cursor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".claude.json"), []byte(`{"mcpServers":{"x":{"command":"echo","args":["a"]}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".cursor", "mcp.json"), []byte(`{"mcpServers":{"x":{"command":"echo","args":["b"]}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	svc.WithUserHome(user)
	list := svc.List()
	if len(list) != 2 {
		t.Fatalf("want 2 fingerprints named x, got %v", list)
	}
	_, err := svc.Get("x")
	var amb *connectors.AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("want ambiguous, got %v list=%v", err, list)
	}
}

// TestConnectorsStartStopLifecycle runs a short-lived stream process and
// observes listening → stopped status transitions.
func TestConnectorsStartStopLifecycle(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	d := eventsDef("tick", `while true; do echo '{"n":1}'; sleep 0.2; done`)
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "tick"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, "tick", "listening")
	waitUntil(t, 3*time.Second, func() bool {
		row, _ := svc.Get("tick")
		return eventCount(row) >= 1
	}, "no events observed")
	if err := svc.Stop("tick"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, "tick", "stopped")
}

// TestConnectorsStartOutlivesCallerContext: HTTP connect/restart pass
// r.Context(), which cancels when the response is written. The process must
// keep listening after that, and only Stop() should tear it down.
func TestConnectorsStartOutlivesCallerContext(t *testing.T) {
	_, _, svc := openStack(t)
	d := eventsDef("tick", `while true; do echo '{"n":1}'; sleep 0.2; done`)
	if err := svc.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(ctx, "tick"); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitStatus(t, svc, "tick", "listening")
	if err := svc.Stop("tick"); err != nil {
		t.Fatal(err)
	}
}

// TestConnectorsConcurrentStartSingleProcess proves the slot reservation:
// two concurrent Starts spawn exactly one subprocess.
func TestConnectorsConcurrentStartSingleProcess(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "pids")
	d := connectors.Definition{
		Name:   "once",
		Env:    map[string]string{"OUT": out},
		Events: &connectors.Events{Command: `echo started >> "$OUT"; sleep 3`},
	}
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.Start(ctx, "once"); err != nil {
				t.Errorf("start: %v", err)
			}
		}()
	}
	wg.Wait()
	t.Cleanup(func() { _ = svc.Stop("once") })
	waitUntil(t, 3*time.Second, func() bool {
		b, err := os.ReadFile(out)
		return err == nil && len(b) > 0
	}, "launch line missing")
	time.Sleep(300 * time.Millisecond)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Fatalf("subprocess launched %d times; want 1 (%q)", n, b)
	}
}

// TestConnectorsRestartPolicies covers never, on-failure (default), and
// always.
func TestConnectorsRestartPolicies(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()

	// never: one non-zero exit ends the supervisor.
	never := connectors.Definition{
		Name:   "never",
		Events: &connectors.Events{Command: "echo one; exit 1", Restart: connectors.RestartNever},
	}
	if err := svc.Create(ctx, never); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "never"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, "never", "crashed")
	time.Sleep(300 * time.Millisecond)
	row, _ := svc.Get("never")
	if ev := row["events"].(map[string]any); ev["restarts"].(int) != 0 {
		t.Fatalf("never restarted: %v", ev)
	}

	// on-failure: a non-zero exit restarts (backoff 1s), a zero exit does not.
	onFail := connectors.Definition{
		Name:   "onfail",
		Events: &connectors.Events{Command: "echo one; exit 1", Restart: connectors.RestartOnFailure},
	}
	if err := svc.Create(ctx, onFail); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "onfail"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		row, _ := svc.Get("onfail")
		ev, ok := row["events"].(map[string]any)
		return ok && ev["restarts"].(int) >= 1
	}, "on-failure did not restart")
	_ = svc.Stop("onfail")

	clean := connectors.Definition{
		Name:   "clean",
		Events: &connectors.Events{Command: "echo one; exit 0", Restart: connectors.RestartOnFailure},
	}
	if err := svc.Create(ctx, clean); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "clean"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, "clean", "stopped")
	time.Sleep(300 * time.Millisecond)
	row, _ = svc.Get("clean")
	if ev := row["events"].(map[string]any); ev["restarts"].(int) != 0 {
		t.Fatalf("clean exit restarted: %v", ev)
	}

	// always: a zero exit restarts.
	always := connectors.Definition{
		Name:   "always",
		Events: &connectors.Events{Command: "echo one; exit 0", Restart: connectors.RestartAlways},
	}
	if err := svc.Create(ctx, always); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "always"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		row, _ := svc.Get("always")
		ev, ok := row["events"].(map[string]any)
		return ok && ev["restarts"].(int) >= 1
	}, "always did not restart")
	_ = svc.Stop("always")
}

// TestConnectorsNonZeroExitCrashes: a non-zero exit marks the connector
// crashed with the error on the row.
func TestConnectorsNonZeroExitCrashes(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	d := connectors.Definition{
		Name:   "boom",
		Events: &connectors.Events{Command: "echo one; exit 1", Restart: connectors.RestartNever},
	}
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "boom"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, "boom", "crashed")
	row, _ := svc.Get("boom")
	if le := row["events"].(map[string]any)["last_err"].(string); le == "" {
		t.Fatalf("want non-empty last_err: %v", row)
	}
}

// TestConnectorsInjectEventMatchesJob enqueues via the ingest path when a
// job subscribes with match true.
func TestConnectorsInjectEventMatchesJob(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()

	wsPath := filepath.Join(home.Base(), "ws")
	jobDir := filepath.Join(wsPath, "sub")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\ntriggers:\n  - event: src\n    match: \"true\"\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Add(ctx, wsPath, "inj-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}

	if err := svc.Create(ctx, eventsDef("src", "true")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"n": 42}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE connector='src'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatal("expected events row")
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job='sub'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatal("expected queue row from match")
	}
}

// TestConnectorsInjectEventMatchesAssistant proves assistant rows fan out.
func TestConnectorsInjectEventMatchesAssistant(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()

	wsPath := filepath.Join(home.Base(), "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Add(ctx, wsPath, "aide-ws")
	if err != nil {
		t.Fatal(err)
	}
	ov := wire.Override{Triggers: []job.Trigger{{
		Family: "event", Event: "src", Match: "true",
	}}}
	if _, err := ws.SetAssistantOverride(ctx, w.ID, ov, ""); err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(ctx, eventsDef("src", "true")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := st.DB().QueryRow(`SELECT kind FROM event_queue WHERE kind='assistant'`).Scan(&kind); err != nil {
		t.Fatal("expected queue row for the assistant on: trigger")
	}
	if kind != "assistant" {
		t.Fatalf("queue kind=%s want assistant", kind)
	}
}

// TestConnectorsAllowlistIdentity proves the declared identity is the only
// field the allowlist checks, and no allowlist is open.
func TestConnectorsAllowlistIdentity(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()

	wsPath := filepath.Join(home.Base(), "ws")
	jobDir := filepath.Join(wsPath, "guard")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\ntriggers:\n  - event: src\n    match: \"true\"\n    allowlist: [\"alice\", \"12345\"]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Add(ctx, wsPath, "allow-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	def := eventsDef("src", "true")
	def.Events.Identity = "sender"
	if err := svc.Create(ctx, def); err != nil {
		t.Fatal(err)
	}
	queueRows := func() int {
		t.Helper()
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job='guard'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Unlisted sender → blocked outcome, no queue row. A different field
	// carrying the same value is not the declared identity.
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"sender": "mallory", "chat": "alice"}); err != nil {
		t.Fatal(err)
	}
	if n := queueRows(); n != 0 {
		t.Fatalf("blocked sender enqueued %d row(s); want 0", n)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes WHERE job='guard' AND outcome='blocked'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("blocked outcome rows=%d want 1", n)
	}

	// A missing identity path value is closed.
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"n": 2}); err != nil {
		t.Fatal(err)
	}
	if n := queueRows(); n != 0 {
		t.Fatalf("identity-less event enqueued %d row(s); want 0", n)
	}

	// Listed sender, and a numeric identity rendered as a decimal string.
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"sender": "alice"}); err != nil {
		t.Fatal(err)
	}
	if n := queueRows(); n != 1 {
		t.Fatalf("allowed sender queue rows=%d want 1", n)
	}
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"sender": 12345}); err != nil {
		t.Fatal(err)
	}
	if n := queueRows(); n != 2 {
		t.Fatalf("numeric identity queue rows=%d want 2", n)
	}

	// A connector with no identity and no allowlist stays open.
	if err := svc.Create(ctx, eventsDef("open", "true")); err != nil {
		t.Fatal(err)
	}
	openJob := filepath.Join(wsPath, "open")
	if err := os.MkdirAll(openJob, 0o755); err != nil {
		t.Fatal(err)
	}
	md2 := "---\nharness: fake\ntriggers:\n  - event: open\n    match: \"true\"\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(openJob, "JOB.md"), []byte(md2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectEvent(ctx, "open", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job='open'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("open connector queue rows=%d want 1", n)
	}
}

// TestConnectorsChannelRefConversationKey proves the channel ref becomes the
// queue conversation key when extract does not set one, and that an
// extracted key wins.
func TestConnectorsChannelRefConversationKey(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()

	wsPath := filepath.Join(home.Base(), "ws")
	jobDir := filepath.Join(wsPath, "chat")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\ntriggers:\n  - event: whatsapp\n    match: \"true\"\n    extract: '({conversation_key: event.chat, who: event.sender})'\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Add(ctx, wsPath, "chat-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	def := connectors.Definition{
		Name: "whatsapp",
		Events: &connectors.Events{
			Command: "true", Format: "json", ID: "id", Identity: "sender",
		},
		Channel: &connectors.Channel{Ref: "chat", Send: "send"},
	}
	if err := svc.Create(ctx, def); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectEvent(ctx, "whatsapp", map[string]any{"id": "m1", "sender": "a", "chat": "c-1"}); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := st.DB().QueryRow(`SELECT conversation_key FROM event_queue WHERE job='chat'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "c-1" {
		t.Fatalf("conversation key %q want c-1", key)
	}

	// Without an extract key, an event with no chat ref leaves it empty.
	def2 := connectors.Definition{
		Name:   "wa2",
		Events: &connectors.Events{Command: "true", Format: "json", ID: "id"},
	}
	if err := svc.Create(ctx, def2); err != nil {
		t.Fatal(err)
	}
	md2 := "---\nharness: fake\ntriggers:\n  - event: wa2\n    match: \"true\"\n---\n\nRun.\n"
	jobDir2 := filepath.Join(wsPath, "chat2")
	if err := os.MkdirAll(jobDir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir2, "JOB.md"), []byte(md2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectEvent(ctx, "wa2", map[string]any{"id": "m2"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT conversation_key FROM event_queue WHERE job='chat2'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "" {
		t.Fatalf("conversation key %q want empty", key)
	}
}

// TestConnectorsDedup: the same source id twice gives one row, one dispatch,
// and one duplicate counter; the connector cursor moves only on a new row.
func TestConnectorsDedup(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()

	wsPath := filepath.Join(home.Base(), "ws")
	jobDir := filepath.Join(wsPath, "dedup")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\ntriggers:\n  - event: src\n    match: \"true\"\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Add(ctx, wsPath, "dedup-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(ctx, eventsDef("src", "true")); err != nil {
		t.Fatal(err)
	}
	meta := connectors.NativeMeta{SourceID: "evt-1"}
	id1, err := svc.InjectNative(ctx, "src", map[string]any{"n": 1}, meta)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := svc.InjectNative(ctx, "src", map[string]any{"n": 1}, meta)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatalf("duplicate returned the same id %s", id1)
	}
	var events, queue int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE connector='src'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("events rows %d want 1", events)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job='dedup'`).Scan(&queue); err != nil {
		t.Fatal(err)
	}
	if queue != 1 {
		t.Fatalf("queue rows %d want 1", queue)
	}
	// A native event never moves the position (decision 2).
	cur, _ := svc.GetCursor(ctx, "src")
	if cur.LastEventID != "" {
		t.Fatalf("native event moved the cursor to %q", cur.LastEventID)
	}
}

// TestConnectorsInjectNativeKeepsEmailPosition proves an email event leaves
// the ingress position row of the email service alone.
func TestConnectorsInjectNativeKeepsEmailPosition(t *testing.T) {
	st, _, svc := openStack(t)
	ctx := context.Background()
	if _, err := st.DB().Exec(`INSERT INTO connector_cursors (name, last_event_id, updated_at) VALUES ('email','42','2026-09-27T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectNative(ctx, "email", map[string]any{"subject": "hi"}, connectors.NativeMeta{SourceID: "<id@x>"}); err != nil {
		t.Fatal(err)
	}
	cur, _ := svc.GetCursor(ctx, "email")
	if cur.LastEventID != "42" {
		t.Fatalf("email position = %q; want 42", cur.LastEventID)
	}
}

// TestConnectorsCatchup drives the three catch-up rules through a real
// supervisor.
func TestConnectorsCatchup(t *testing.T) {
	st, _, svc := openStack(t)
	ctx := context.Background()

	// A cursor that does not appear in the output skips the replay.
	skip := connectors.Definition{
		Name: "feed",
		Events: &connectors.Events{
			Command: "sleep 30",
			Format:  "json",
			ID:      "id",
			Catchup: &connectors.Catchup{
				Command: `printf '%s\n' '{"id":"n1"}' '{"id":"n2"}'`,
			},
		},
	}
	if err := svc.Create(ctx, skip); err != nil {
		t.Fatal(err)
	}
	if err := svc.PutCursor(ctx, "feed", "old"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "feed"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, func() bool {
		evs, err := st.ListBus(ctx, 0, []string{"connector"}, 50)
		if err != nil {
			return false
		}
		for _, ev := range evs {
			if ev.Topic == "connector.catchup.skipped" && strings.Contains(string(ev.Attrs), "position_not_found") {
				return true
			}
		}
		return false
	}, "position_not_found skip missing")
	if n := countEvents(t, st); n != 0 {
		t.Fatalf("skipped replay wrote %d event(s)", n)
	}
	_ = svc.Stop("feed")

	// The cut after the last occurrence replays only later rows, sorted by
	// ts, and marks them catch_up.
	replay := connectors.Definition{
		Name: "feed2",
		Events: &connectors.Events{
			Command:  "sleep 30",
			Format:   "json",
			ID:       "id",
			TS:       "ts",
			Identity: "sender",
			Catchup: &connectors.Catchup{
				Command: `printf '%s\n' '{"id":"5","ts":"2026-09-27T10:05:00Z","sender":"e"}' '{"id":"3","ts":"2026-09-27T10:03:00Z","sender":"c"}' '{"id":"4","ts":"2026-09-27T10:04:00Z","sender":"d"}' '{"id":"2","ts":"2026-09-27T10:02:00Z","sender":"b"}'`,
			},
		},
	}
	if err := svc.Create(ctx, replay); err != nil {
		t.Fatal(err)
	}
	if err := svc.PutCursor(ctx, "feed2", "3"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "feed2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop("feed2") })
	waitUntil(t, 3*time.Second, func() bool {
		cur, _ := svc.GetCursor(ctx, "feed2")
		return cur.LastEventID == "5"
	}, "catch-up did not advance the cursor to the last replayed id")
	var catchUps int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE connector='feed2' AND catch_up=1`).Scan(&catchUps); err != nil {
		t.Fatal(err)
	}
	if catchUps != 2 {
		t.Fatalf("replayed %d rows want 2", catchUps)
	}
	var first string
	if err := st.DB().QueryRow(`SELECT source_id FROM events WHERE connector='feed2' ORDER BY source_id`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first != "4" {
		t.Fatalf("first replayed %q want 4", first)
	}

	// The buffer cap kills the command and keeps the rows it already read.
	capped := connectors.Definition{
		Name: "feed4",
		Events: &connectors.Events{
			Command: "sleep 30",
			Format:  "json",
			ID:      "id",
			Catchup: &connectors.Catchup{
				Command:   `i=0; while [ $i -lt 50 ]; do i=$((i+1)); echo "{\"id\":\"c$i\"}"; sleep 0.05; done`,
				MaxEvents: 3,
			},
		},
	}
	if err := svc.Create(ctx, capped); err != nil {
		t.Fatal(err)
	}
	if err := svc.PutCursor(ctx, "feed4", "c1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "feed4"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop("feed4") })
	waitUntil(t, 5*time.Second, func() bool {
		cur, _ := svc.GetCursor(ctx, "feed4")
		return cur.LastEventID == "c3"
	}, "capped catch-up did not finish")
	var cappedRows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE connector='feed4'`).Scan(&cappedRows); err != nil {
		t.Fatal(err)
	}
	if cappedRows != 2 {
		t.Fatalf("cap replayed %d rows want 2 (c2, c3)", cappedRows)
	}

	// An empty connector cursor skips without running the command.
	empty := connectors.Definition{
		Name: "feed3",
		Events: &connectors.Events{
			Command: "sleep 30",
			Format:  "json",
			ID:      "id",
			Catchup: &connectors.Catchup{Command: "echo should-not-run"},
		},
	}
	if err := svc.Create(ctx, empty); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "feed3"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop("feed3") })
	waitUntil(t, 3*time.Second, func() bool {
		evs, err := st.ListBus(ctx, 0, []string{"connector"}, 80)
		if err != nil {
			return false
		}
		for _, ev := range evs {
			if ev.Topic == "connector.catchup.skipped" && strings.Contains(string(ev.Attrs), "no_position") {
				return true
			}
		}
		return false
	}, "no_position skip missing")
}

func openStack(t *testing.T) (*store.Store, *workspaces.Service, *connectors.Service) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := workspaces.New(st)
	svc := connectors.New(st, ws, nil)
	return st, ws, svc
}

// stackWithFile opens a stack whose connectors.yaml holds body. The
// returned teardown closes the store.
func stackWithFile(t *testing.T, body string) (*connectors.Service, func()) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	if err := os.WriteFile(home.ConnectorsPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	svc := connectors.New(st, nil, nil)
	return svc, func() {
		_ = st.Close()
		home.SetBase("")
	}
}

// waitStatus polls Get until the connector's events.status reaches want.
func waitStatus(t *testing.T, svc *connectors.Service, name, want string) {
	t.Helper()
	var status string
	waitUntil(t, 3*time.Second, func() bool {
		row, _ := svc.Get(name)
		if ev, ok := row["events"].(map[string]any); ok {
			status, _ = ev["status"].(string)
		}
		return status == want
	}, "status want "+want)
}

func waitUntil(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal(msg)
}

// eventCount reads the events counter of one connector row.
func eventCount(row map[string]any) int64 {
	ev, ok := row["events"].(map[string]any)
	if !ok {
		return 0
	}
	counters, ok := ev["counters"].(map[string]any)
	if !ok {
		return 0
	}
	switch n := counters["events"].(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func countEvents(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestConnectorsConcurrentWrites proves writes are serialized: two creates of
// one name give one ErrExists, and parallel patches keep every edit.
func TestConnectorsConcurrentWrites(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- svc.Create(ctx, eventsDef("dup", "true")) }()
	}
	var exists int
	for range 2 {
		if err := <-errs; errors.Is(err, connectors.ErrExists) {
			exists++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if exists != 1 {
		t.Fatalf("ErrExists count %d, want 1", exists)
	}

	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env := map[string]string{"K": "v"}
			cwd := "/tmp/c"
			p := connectors.Patch{Env: &env}
			if i%2 == 1 {
				p = connectors.Patch{Cwd: &cwd}
			}
			if err := svc.Patch(ctx, "dup", p); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	d, _ := svc.Definition("dup")
	if d.Env["K"] != "v" || d.Cwd != "/tmp/c" {
		t.Fatalf("lost an edit: env %v cwd %q", d.Env, d.Cwd)
	}
}

// TestConnectorsRewriteDryRunKeepsLiveBlocks proves a dry run does not change
// the pointer blocks of the live definitions.
func TestConnectorsRewriteDryRunKeepsLiveBlocks(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	d := connectors.Definition{
		Name:    "dry",
		Actions: &connectors.Actions{MCP: &connectors.MCPAction{Command: "/old/bin/mcp", Args: []string{"/old/a"}}},
		Events:  &connectors.Events{Command: "/old/bin/listen", Format: "json"},
		Channel: &connectors.Channel{Ref: "id", Send: "/old/bin/send"},
	}
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	names, err := svc.RewritePathPrefix("/old", "/new", true)
	if err != nil || len(names) != 1 {
		t.Fatalf("names %v err %v", names, err)
	}
	got, _ := svc.Definition("dry")
	if got.Events.Command != "/old/bin/listen" || got.Channel.Send != "/old/bin/send" ||
		got.Actions.MCP.Command != "/old/bin/mcp" || got.Actions.MCP.Args[0] != "/old/a" {
		t.Fatalf("dry run changed the live definition: %+v %+v %+v", got.Events, got.Channel, got.Actions.MCP)
	}
}

// TestConnectorsCwdChangeRestarts proves a cwd edit restarts a live
// connector.
func TestConnectorsCwdChangeRestarts(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	if err := svc.Create(ctx, eventsDef("wd", "sleep 30")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "wd"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop("wd") })
	waitStatus(t, svc, "wd", "listening")
	cwd := t.TempDir()
	if err := svc.Patch(ctx, "wd", connectors.Patch{Cwd: &cwd}); err != nil {
		t.Fatal(err)
	}
	row, _ := svc.Get("wd")
	if n := row["events"].(map[string]any)["restarts"]; n != 1 {
		t.Fatalf("restarts %v, want 1", n)
	}
}

// TestResolveActionsUserHomeSkill proves a connector skill found in the user
// home projects from that folder, not from a missing managed folder (P1-23).
func TestResolveActionsUserHomeSkill(t *testing.T) {
	st, ws, _ := openStack(t)
	user := t.TempDir()
	skillDir := filepath.Join(user, ".agents", "skills", "pg")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: pg\ndescription: d\n---\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := connectors.New(st, ws, skills.New(st).WithUserHome(user))
	ctx := context.Background()
	if err := svc.Create(ctx, connectors.Definition{
		Name:    "psql",
		Actions: &connectors.Actions{CLI: &connectors.CLIAction{Commands: []string{"psql"}, Skill: "pg"}},
	}); err != nil {
		t.Fatal(err)
	}
	acts, err := svc.ResolveActions(ctx, []string{"psql"})
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].SkillDir != skillDir {
		t.Fatalf("actions = %+v; want skill dir %s", acts, skillDir)
	}
}
