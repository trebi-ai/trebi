package connectors_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/connectors/protocol/fakeadapter"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func TestMain(m *testing.M) {
	if fakeadapter.Enabled() {
		os.Exit(fakeadapter.Main())
	}
	os.Exit(m.Run())
}

// spec is a short form of an inline entry with an events block.
type spec struct {
	Name   string
	Env    map[string]string
	Events *manifest.Events
}

func (s spec) entry() connectors.Entry {
	m := baseManifest(s.Name)
	m.Events = s.Events
	return connectors.Entry{Name: s.Name, From: connectors.FromInline, Inputs: s.Env, Manifest: &m}
}

func create(ctx context.Context, svc *connectors.Service, s spec) error {
	return svc.Create(ctx, s.entry())
}

// eventsDef is a minimal valid lines connector.
func eventsDef(name, command string) spec {
	return spec{Name: name, Events: &manifest.Events{Command: command, Format: manifest.FormatJSON}}
}

func baseManifest(name string) manifest.Manifest {
	return manifest.Manifest{Schema: manifest.SchemaID, Name: name, Title: name, Description: "Test connector.", Version: "1.0.0"}
}

// inline builds an inline entry from a manifest body.
func inline(name string, edit func(*manifest.Manifest)) connectors.Entry {
	m := baseManifest(name)
	edit(&m)
	return connectors.Entry{Name: name, From: connectors.FromInline, Manifest: &m}
}

func TestConnectorsCreateReplacePatchDelete(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()

	e := inline("feed", func(m *manifest.Manifest) {
		m.Events = &manifest.Events{Command: "rss-fetch https://example.com/feed.xml", Format: "json", ID: "guid"}
	})
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home.ConnectorsPath()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(ctx, e); !errors.Is(err, connectors.ErrExists) {
		t.Fatalf("double create: %v", err)
	}
	if err := svc.Create(ctx, eventsDef("email", "x").entry()); !errors.Is(err, connectors.ErrReserved) {
		t.Fatalf("reserved create: %v", err)
	}
	if err := svc.Create(ctx, connectors.Entry{Name: "bad", From: connectors.FromInline}); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("inline with no manifest: %v", err)
	}

	// Replace swaps the whole entry.
	replaced := inline("feed", func(m *manifest.Manifest) {
		m.Events = &manifest.Events{Command: "rss-fetch v2", Format: "line"}
	})
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

	// Patch sets inputs and a merge patch on the manifest.
	inputs := map[string]string{"FEED": "https://example.com/x"}
	p := connectors.EntryPatch{
		Inputs: &inputs,
		Patch:  json.RawMessage(`{"events":{"command":"rss-fetch v3","format":"json","id":"guid"},"actions":{"cli":{"commands":["rss-fetch"]}}}`),
	}
	if err := svc.Patch(ctx, "feed", p); err != nil {
		t.Fatal(err)
	}
	row, _ = svc.Get("feed")
	if row["inputs"].(map[string]string)["FEED"] != "***" {
		t.Fatalf("patch inputs %v", row["inputs"])
	}
	if got, _ := svc.Entry("feed"); got.Inputs["FEED"] != "https://example.com/x" {
		t.Fatalf("stored inputs %v", got.Inputs)
	}
	if ev := row["events"].(map[string]any); ev["command"] != "rss-fetch v3" || ev["id"] != "guid" {
		t.Fatalf("patch events %v", ev)
	}
	if caps := row["capabilities"].([]string); strings.Join(caps, ",") != "actions,events" {
		t.Fatalf("capabilities %v", caps)
	}

	// A patch that breaks the manifest is a validation error on the global layer.
	var ve *connectors.ValidationError
	if err := svc.Patch(ctx, "feed", connectors.EntryPatch{Patch: json.RawMessage(`{"events":{"restart":"sometimes"}}`)}); !errors.As(err, &ve) || ve.Layer != connectors.LayerGlobal {
		t.Fatalf("bad patch err = %v", err)
	}
	// A null patch removes the layer.
	if err := svc.Patch(ctx, "feed", connectors.EntryPatch{Patch: json.RawMessage("null")}); err != nil {
		t.Fatal(err)
	}
	row, _ = svc.Get("feed")
	if row["actions"] != nil {
		t.Fatalf("actions after null patch: %v", row["actions"])
	}

	// Delete twice: the second is ErrNotFound.
	if err := svc.Delete(ctx, "feed"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, "feed"); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// TestConnectorsSecretInputs proves a literal secret input becomes a secret
// that the entry owns, and Delete removes it.
func TestConnectorsSecretInputs(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	e := inline("gh", func(m *manifest.Manifest) {
		m.Setup = &manifest.Setup{Inputs: []manifest.Input{{Name: "TOKEN", Secret: true, Required: true}}}
		m.Actions = &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"gh"}}}
	})
	e.Inputs = map[string]string{"TOKEN": "t0k"}
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Entry("gh")
	if got.Inputs["TOKEN"] != "secret:gh_TOKEN" || !reflect.DeepEqual(got.InstalledSecrets, []string{"gh_TOKEN"}) {
		t.Fatalf("entry %+v", got)
	}
	if v, err := secrets.Get("gh_TOKEN"); err != nil || v != "t0k" {
		t.Fatalf("secret %q %v", v, err)
	}
	r, err := svc.Resolve("gh", "")
	if err != nil {
		t.Fatal(err)
	}
	if env, missing := r.Env(); env["TOKEN"] != "t0k" || len(missing) != 0 {
		t.Fatalf("env %v missing %v", env, missing)
	}
	if err := svc.Delete(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Get("gh_TOKEN"); err == nil {
		t.Fatal("owned secret survived delete")
	}

	// A secret that the user made first is not overwritten or owned.
	if err := secrets.Set("gh_TOKEN", "mine"); err != nil {
		t.Fatal(err)
	}
	e.Inputs = map[string]string{"TOKEN": "t1k"}
	var ve *connectors.ValidationError
	if err := svc.Create(ctx, e); !errors.As(err, &ve) {
		t.Fatalf("create over a user secret = %v", err)
	}
	if v, _ := secrets.Get("gh_TOKEN"); v != "mine" {
		t.Fatalf("user secret = %q", v)
	}
	e.Inputs = map[string]string{"TOKEN": "secret:gh_TOKEN"}
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Entry("gh"); len(got.InstalledSecrets) != 0 {
		t.Fatalf("installed = %v", got.InstalledSecrets)
	}
}

func TestConnectorsRewritePathPrefix(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	oldDir := filepath.Join(t.TempDir(), "old")
	newDir := filepath.Join(filepath.Dir(oldDir), "new")
	e := inline("whatsapp", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{MCP: &manifest.MCPAction{Command: "npx", Args: []string{"--root", oldDir + "/mcp"}}}
		m.Events = &manifest.Events{
			Command: "whatsapp-cli listen --dir " + oldDir, Format: "json", ID: "d.id",
			Catchup: &manifest.Catchup{Command: "whatsapp-cli back --dir " + oldDir},
		}
	})
	e.Inputs = map[string]string{"ROOT": oldDir + "/data"}
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}

	// A file entry moves with its folder.
	if err := os.MkdirAll(filepath.Join(oldDir, "conn"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(inline("local", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"local"}}}
	}).Manifest)
	if err := os.WriteFile(filepath.Join(oldDir, "conn", manifest.FileName), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(ctx, connectors.Entry{Name: "local", From: "file:" + filepath.Join(oldDir, "conn", manifest.FileName)}); err != nil {
		t.Fatal(err)
	}

	preview, err := svc.RewritePathPrefix(oldDir, newDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(preview, ",") != "local,whatsapp" {
		t.Fatalf("preview %v", preview)
	}
	if got, _ := svc.Entry("whatsapp"); got.Inputs["ROOT"] != oldDir+"/data" || got.Manifest.Events.Command != "whatsapp-cli listen --dir "+oldDir {
		t.Fatalf("dry run changed the live entry: %+v", got)
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RewritePathPrefix(oldDir, newDir, false); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Entry("whatsapp")
	if got.Inputs["ROOT"] != newDir+"/data" {
		t.Fatalf("inputs %v", got.Inputs)
	}
	m := got.Manifest
	if m.Events.Command != "whatsapp-cli listen --dir "+newDir || m.Events.Catchup.Command != "whatsapp-cli back --dir "+newDir {
		t.Fatalf("events %+v", m.Events)
	}
	if m.Actions.MCP.Args[1] != newDir+"/mcp" {
		t.Fatalf("mcp args %v", m.Actions.MCP.Args)
	}
	if local, _ := svc.Entry("local"); local.FilePath() != filepath.Join(newDir, "conn", manifest.FileName) {
		t.Fatalf("file entry %q", local.From)
	}
	if _, err := svc.Resolve("local", ""); err != nil {
		t.Fatalf("resolve after move: %v", err)
	}
}

// TestConnectorsListRowShape pins the wire row fields.
func TestConnectorsListRowShape(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	e := inline("github", func(m *manifest.Manifest) {
		m.Setup = &manifest.Setup{Inputs: []manifest.Input{{Name: "GITHUB_TOKEN"}}}
		m.Actions = &manifest.Actions{
			MCP: &manifest.MCPAction{
				URL:     "https://mcp.example.com/github",
				Headers: map[string]string{"Authorization": "Bearer tok", "X-Ref": "secret:github"},
			},
			CLI: &manifest.CLIAction{Commands: []string{"definitely-not-on-path-xyz"}},
		}
	})
	e.Inputs = map[string]string{"GITHUB_TOKEN": "secret:github"}
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Get("github")
	if err != nil {
		t.Fatal(err)
	}
	if row["provenance"] != "managed" || row["managed"] != true || row["from"] != "inline" || row["version"] != "1.0.0" {
		t.Fatalf("row %v", row)
	}
	if caps := row["capabilities"].([]string); strings.Join(caps, ",") != "actions" {
		t.Fatalf("capabilities %v", caps)
	}
	if row["events"] != nil || row["channel"] != nil {
		t.Fatalf("absent blocks must be nil: %v", row)
	}
	if in := row["inputs"].(map[string]string); in["GITHUB_TOKEN"] != "secret:github" {
		t.Fatalf("secret reference masked: %v", in)
	}
	acts := row["actions"].(map[string]any)
	mcp := acts["mcp"].(map[string]any)
	if mcp["url"] != "https://mcp.example.com/github" {
		t.Fatalf("mcp %v", mcp)
	}
	headers := mcp["headers"].(map[string]string)
	if headers["Authorization"] != "***" || headers["X-Ref"] != "secret:github" {
		t.Fatalf("headers %v", headers)
	}
	cli := acts["cli"].(map[string]any)
	if missing := cli["missing"].([]string); len(missing) != 1 || missing[0] != "definitely-not-on-path-xyz" {
		t.Fatalf("missing %v", missing)
	}

	// An events row carries the live fields even when stopped.
	if err := create(ctx, svc, eventsDef("feed", "echo hi")); err != nil {
		t.Fatal(err)
	}
	row, _ = svc.Get("feed")
	ev := row["events"].(map[string]any)
	if ev["status"] != "stopped" || ev["format"] != "json" || ev["last_err"] != "" || ev["protocol"] != "lines" || ev["restart"] != "on-failure" {
		t.Fatalf("events row %v", ev)
	}
	counters := ev["counters"].(map[string]any)
	if counters["events"] != int64(0) || counters["duplicates"] != int64(0) || counters["parse_errors"] != int64(0) {
		t.Fatalf("counters %v", counters)
	}
	if inst := row["instances"].([]map[string]any); len(inst) != 1 || inst[0]["key"] != "feed" {
		t.Fatalf("instances %v", inst)
	}
}

func TestConnectorsStrictLoadRefusesOldKeys(t *testing.T) {
	svc, teardown := stackWithFile(t, "mail:\n  events: {command: x}\n")
	defer teardown()
	if err := svc.Load(); err == nil || !strings.Contains(err.Error(), "events") {
		t.Fatalf("load: %v", err)
	}
	svc2, teardown2 := stackWithFile(t, "feed:\n  from: inline\n  manifest:\n    schema: trebi-connector/1\n    name: feed\n    title: Feed\n    description: A feed.\n    version: 1.0.0\n    events: {command: echo, format: json, id: g}\n")
	defer teardown2()
	if err := svc2.Load(); err != nil {
		t.Fatal(err)
	}
	row, err := svc2.Get("feed")
	if err != nil {
		t.Fatal(err)
	}
	if ev := row["events"].(map[string]any); ev["restart"] != "on-failure" {
		t.Fatalf("default restart %v", ev["restart"])
	}
}

// TestConnectorsFailedLoadBlocksWrites proves a failed load keeps the error
// and refuses every write until a load succeeds.
func TestConnectorsFailedLoadBlocksWrites(t *testing.T) {
	body := "mail:\n  type: stream\n"
	svc, teardown := stackWithFile(t, body)
	defer teardown()
	ctx := context.Background()
	if err := svc.Load(); err == nil {
		t.Fatal("load passed")
	}
	if svc.LoadErr() == nil {
		t.Fatal("LoadErr is nil after a failed load")
	}
	e := eventsDef("feed", "echo").entry()
	inputs := map[string]string{"A": "1"}
	for name, write := range map[string]func() error{
		"create":  func() error { return svc.Create(ctx, e) },
		"replace": func() error { return svc.Replace(ctx, e) },
		"patch":   func() error { return svc.Patch(ctx, "mail", connectors.EntryPatch{Inputs: &inputs}) },
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
	if err := os.WriteFile(home.ConnectorsPath(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(); err != nil || svc.LoadErr() != nil {
		t.Fatalf("load after fix: %v %v", err, svc.LoadErr())
	}
	if err := svc.Create(ctx, e); err != nil {
		t.Fatalf("create after fix: %v", err)
	}
}

// TestConnectorsMaskedInputsRoundTrip proves a masked input that comes back
// on a write keeps the stored value.
func TestConnectorsMaskedInputsRoundTrip(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	e := eventsDef("api", "true").entry()
	e.Inputs = map[string]string{"TOKEN": "t0k", "REF": "secret:api_TOKEN"}
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(home.ConnectorsPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", fi, err)
	}
	row, _ := svc.Get("api")
	if in := row["inputs"].(map[string]string); in["TOKEN"] != "***" || in["REF"] != "secret:api_TOKEN" {
		t.Fatalf("inputs %v", in)
	}
	put := e
	put.Inputs = map[string]string{"TOKEN": "***"}
	if err := svc.Replace(ctx, put); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Entry("api"); got.Inputs["TOKEN"] != "t0k" {
		t.Fatalf("put %v", got.Inputs)
	}
	put.Inputs = map[string]string{"NEW": "***"}
	if err := svc.Replace(ctx, put); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("new mask: %v", err)
	}
}

// TestConnectorsWorkspaceOverride covers the workspace layer: an actions
// patch keeps the global instance, new inputs make a workspace instance, and
// disabled turns the connector off.
func TestConnectorsWorkspaceOverride(t *testing.T) {
	_, ws, svc := openStack(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Add(ctx, wsPath, "ov-ws")
	if err != nil {
		t.Fatal(err)
	}
	e := inline("chat", func(m *manifest.Manifest) {
		m.Setup = &manifest.Setup{Inputs: []manifest.Input{{Name: "KEY", Secret: true}, {Name: "ROOM"}}}
		m.Actions = &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"chat"}}}
		m.Events = &manifest.Events{Command: "sleep 30", Format: "json"}
	})
	e.Inputs = map[string]string{"ROOM": "global"}
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetOverride(ctx, w.ID, "missing", &workspaces.ConnectorOverride{}); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("override of a missing connector: %v", err)
	}

	patchOnly := &workspaces.ConnectorOverride{Patch: map[string]any{"actions": map[string]any{"cli": map[string]any{"commands": []any{"chat", "chat-admin"}}}}}
	if err := svc.SetOverride(ctx, w.ID, "chat", patchOnly); err != nil {
		t.Fatal(err)
	}
	if got := svc.InstanceFor("chat", w.ID); got != "chat" {
		t.Fatalf("actions override instance %q", got)
	}
	r, err := svc.Resolve("chat", w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Manifest.Actions.CLI.Commands) != 2 || r.Workspace != w.ID {
		t.Fatalf("resolved %+v", r)
	}

	own := &workspaces.ConnectorOverride{Inputs: map[string]string{"ROOM": "team", "KEY": "k3y"}}
	if err := svc.SetOverride(ctx, w.ID, "chat", own); err != nil {
		t.Fatal(err)
	}
	inst := "chat@" + w.ID
	if got := svc.InstanceFor("chat", w.ID); got != inst {
		t.Fatalf("inputs override instance %q", got)
	}
	ov, ok, err := svc.Override(ctx, w.ID, "chat")
	if err != nil || !ok || ov.Inputs["KEY"] != "secret:"+inst+"_KEY" {
		t.Fatalf("override %+v %v %v", ov, ok, err)
	}
	r, _ = svc.Resolve("chat", w.ID)
	if env, _ := r.Env(); env["ROOM"] != "team" || env["KEY"] != "k3y" {
		t.Fatalf("workspace env %v", env)
	}
	if g, _ := svc.Resolve("chat", ""); g.Inputs["ROOM"] != "global" {
		t.Fatalf("global inputs changed: %v", g.Inputs)
	}
	row, err := svc.GetFor("chat", w.ID)
	if err != nil || row["instance"] != inst || row["workspace"] != w.ID {
		t.Fatalf("row %v %v", row, err)
	}

	if err := svc.SetOverride(ctx, w.ID, "chat", &workspaces.ConnectorOverride{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := svc.InstanceFor("chat", w.ID); got != "" {
		t.Fatalf("disabled instance %q", got)
	}
	if _, err := svc.Resolve("chat", w.ID); !errors.Is(err, connectors.ErrDisabled) {
		t.Fatalf("disabled resolve: %v", err)
	}
	if _, err := svc.ResolveActions(ctx, w.ID, []string{"chat"}); err == nil {
		t.Fatal("disabled connector gave actions")
	}

	if err := svc.SetOverride(ctx, w.ID, "chat", nil); err != nil {
		t.Fatal(err)
	}
	if got := svc.InstanceFor("chat", w.ID); got != "chat" {
		t.Fatalf("removed override instance %q", got)
	}
}

func TestConnectorsInheritedUnionAndGuard(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	user := t.TempDir()
	if err := os.WriteFile(filepath.Join(user, ".claude.json"), []byte(`{"mcpServers":{"sling":{"command":"sling","args":["serve","mcp"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.WithUserHome(user)

	list := svc.List("")
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
	if err := create(ctx, svc, eventsDef("mail", "echo")); err != nil {
		t.Fatal(err)
	}
	if list = svc.List(""); len(list) != 2 {
		t.Fatalf("managed+inherited %d %v", len(list), list)
	}
}

// TestConnectorsMCPFingerprintMerge folds an inherited server into the
// managed connector that declares the same MCP action.
func TestConnectorsMCPFingerprintMerge(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	e := inline("sling", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{MCP: &manifest.MCPAction{Command: "sling", Args: []string{"serve", "mcp"}}}
	})
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	user := t.TempDir()
	if err := os.WriteFile(filepath.Join(user, ".claude.json"), []byte(`{"mcpServers":{"sling":{"command":"sling","args":["serve","mcp"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.WithUserHome(user)
	list := svc.List("")
	if len(list) != 1 {
		t.Fatalf("want merged 1 row, got %v", list)
	}
	if row := list[0]; row["managed"] != true || row["name"] != "sling" || row["origins"] == nil {
		t.Fatalf("%+v", row)
	}
}

func TestConnectorsInheritedAmbiguousName(t *testing.T) {
	_, _, svc := openStack(t)
	user := t.TempDir()
	if err := os.MkdirAll(filepath.Join(user, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".claude.json"), []byte(`{"mcpServers":{"x":{"command":"echo","args":["a"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".cursor", "mcp.json"), []byte(`{"mcpServers":{"x":{"command":"echo","args":["b"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.WithUserHome(user)
	if list := svc.List(""); len(list) != 2 {
		t.Fatalf("want 2 fingerprints named x, got %v", list)
	}
	var amb *connectors.AmbiguousError
	if _, err := svc.Get("x"); !errors.As(err, &amb) {
		t.Fatalf("want ambiguous, got %v", err)
	}
}

func TestConnectorsStartStopLifecycle(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	d := eventsDef("tick", `while true; do echo '{"n":1}'; sleep 0.2; done`)
	if err := create(ctx, svc, d); err != nil {
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
	if err := create(context.Background(), svc, d); err != nil {
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
	d := spec{
		Name:   "once",
		Env:    map[string]string{"OUT": out},
		Events: &manifest.Events{Command: `echo started >> "$OUT"; sleep 3`},
	}
	if err := create(ctx, svc, d); err != nil {
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
	never := spec{
		Name:   "never",
		Events: &manifest.Events{Command: "echo one; exit 1", Restart: manifest.RestartNever},
	}
	if err := create(ctx, svc, never); err != nil {
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
	onFail := spec{
		Name:   "onfail",
		Events: &manifest.Events{Command: "echo one; exit 1", Restart: manifest.RestartOnFailure},
	}
	if err := create(ctx, svc, onFail); err != nil {
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

	clean := spec{
		Name:   "clean",
		Events: &manifest.Events{Command: "echo one; exit 0", Restart: manifest.RestartOnFailure},
	}
	if err := create(ctx, svc, clean); err != nil {
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
	always := spec{
		Name:   "always",
		Events: &manifest.Events{Command: "echo one; exit 0", Restart: manifest.RestartAlways},
	}
	if err := create(ctx, svc, always); err != nil {
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
	d := spec{
		Name:   "boom",
		Events: &manifest.Events{Command: "echo one; exit 1", Restart: manifest.RestartNever},
	}
	if err := create(ctx, svc, d); err != nil {
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

// TestConnectorsCountersPerOutcome proves the events counter counts only
// inserted rows. A duplicate and a parse error have their own counters.
func TestConnectorsCountersPerOutcome(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	d := spec{
		Name: "count",
		Events: &manifest.Events{
			Command: `printf '{"id":"a"}\n{"id":"a"}\nnot json\n{"id":"b"}\n'; sleep 30`,
			Format:  "json", ID: "id", Restart: manifest.RestartNever,
		},
	}
	if err := create(ctx, svc, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "count"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop("count") })
	want := map[string]any{"events": int64(2), "duplicates": int64(1), "parse_errors": int64(1)}
	var got map[string]any
	waitUntil(t, 5*time.Second, func() bool {
		row, _ := svc.Get("count")
		got, _ = row["events"].(map[string]any)["counters"].(map[string]any)
		return reflect.DeepEqual(got, want)
	}, "counters")
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

	if err := create(ctx, svc, eventsDef("src", "true")); err != nil {
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
	// A disabled trigger and a cron trigger never fire on an event.
	ov := wire.Override{Triggers: []job.Trigger{
		{Family: "event", Event: "src", Match: "true"},
		{Family: "event", Event: "src", Match: "true", Disabled: true, Index: 1},
		{Family: "cron", Cron: "0 * * * *", Index: 2},
	}}
	if _, err := ws.SetAssistantOverride(ctx, w.ID, ov, ""); err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := create(ctx, svc, eventsDef("src", "true")); err != nil {
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
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE kind='assistant'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("assistant queue rows = %d (%v); want 1", n, err)
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
	if err := create(ctx, svc, def); err != nil {
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
	if err := create(ctx, svc, eventsDef("open", "true")); err != nil {
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

// TestConnectorsAllowlistEmailCase proves the email identity compares
// without case, while a managed connector compares exactly.
func TestConnectorsAllowlistEmailCase(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	for job, event := range map[string]string{"mail": "email", "chat": "src"} {
		dir := filepath.Join(wsPath, job)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		md := "---\nharness: fake\ntriggers:\n  - event: " + event + "\n    match: \"true\"\n    allowlist: [\"Ops@Example.com\"]\n---\n\nRun.\n"
		if err := os.WriteFile(filepath.Join(dir, "JOB.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w, err := ws.Add(ctx, wsPath, "case-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	def := eventsDef("src", "true")
	def.Events.Identity = "sender"
	if err := create(ctx, svc, def); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectNative(ctx, "email", map[string]any{"subject": "hi"}, connectors.NativeMeta{SourceID: "<1@x>", Identity: "ops@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"sender": "ops@example.com"}); err != nil {
		t.Fatal(err)
	}
	for job, want := range map[string]int{"mail": 1, "chat": 0} {
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job=?`, job).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("%s queue rows = %d; want %d", job, n, want)
		}
	}
}

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
	if err := create(ctx, svc, eventsDef("src", "true")); err != nil {
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

// TestConnectorsRedispatch proves a retrigger reuses the stored row: it
// works for a native connector and for a source id that is already stored,
// and it writes no new events row. A synthetic duplicate is ErrDuplicate.
func TestConnectorsRedispatch(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()

	wsPath := filepath.Join(home.Base(), "ws")
	for _, name := range []string{"hook", "feed"} {
		dir := filepath.Join(wsPath, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		md := "---\nharness: fake\ntriggers:\n  - event: " + map[string]string{"hook": "webhook", "feed": "src"}[name] +
			"\n    match: \"event.n == 7\"\n---\n\nRun.\n"
		if err := os.WriteFile(filepath.Join(dir, "JOB.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w, err := ws.Add(ctx, wsPath, "redispatch-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := st.DB().QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	nativeID, err := svc.InjectNative(ctx, "webhook", map[string]any{"n": 7}, connectors.NativeMeta{SourceID: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Redispatch(ctx, nativeID); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE job='hook' AND event_id=?`, nativeID); n != 2 {
		t.Fatalf("native queue rows = %d; want 2", n)
	}
	if n := count(`SELECT COUNT(*) FROM events WHERE connector='webhook'`); n != 1 {
		t.Fatalf("native events rows = %d; want 1", n)
	}
	// The outcome points at the newest queue row.
	var qid string
	if err := st.DB().QueryRow(`SELECT queue_id FROM event_outcomes WHERE event_id=?`, nativeID).Scan(&qid); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE id=? AND id=(SELECT MAX(id) FROM event_queue WHERE event_id=?)`, qid, nativeID); n != 1 {
		t.Fatalf("outcome queue id %s is not the newest", qid)
	}

	def := eventsDef("src", "true")
	def.Events.ID = "id"
	if err := create(ctx, svc, def); err != nil {
		t.Fatal(err)
	}
	id, err := svc.InjectEvent(ctx, "src", map[string]any{"id": "a", "n": 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectEvent(ctx, "src", map[string]any{"id": "a", "n": 7}); !errors.Is(err, connectors.ErrDuplicate) {
		t.Fatalf("second inject err = %v; want ErrDuplicate", err)
	}
	if err := svc.Redispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE job='feed'`); n != 2 {
		t.Fatalf("feed queue rows = %d; want 2", n)
	}
	if err := svc.Redispatch(ctx, "missing"); !errors.Is(err, connectors.ErrEventNotFound) {
		t.Fatalf("missing err = %v", err)
	}
}

// TestConnectorsInjectNativeKeepsEmailPosition proves an email event leaves
// the ingress position row of the email service alone.
func TestConnectorsInjectNativeKeepsEmailPosition(t *testing.T) {
	st, _, svc := openStack(t)
	ctx := context.Background()
	if _, err := st.DB().Exec(`INSERT INTO connector_cursors (instance, last_event_id, updated_at) VALUES ('email','42','2026-09-27T00:00:00Z')`); err != nil {
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
	skip := spec{
		Name: "feed",
		Events: &manifest.Events{
			Command: "sleep 30",
			Format:  "json",
			ID:      "id",
			Catchup: &manifest.Catchup{
				Command: `printf '%s\n' '{"id":"n1"}' '{"id":"n2"}'`,
			},
		},
	}
	if err := create(ctx, svc, skip); err != nil {
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
	replay := spec{
		Name: "feed2",
		Events: &manifest.Events{
			Command:  "sleep 30",
			Format:   "json",
			ID:       "id",
			TS:       "ts",
			Identity: "sender",
			Catchup: &manifest.Catchup{
				Command: `printf '%s\n' '{"id":"5","ts":"2026-09-27T10:05:00Z","sender":"e"}' '{"id":"3","ts":"2026-09-27T10:03:00Z","sender":"c"}' '{"id":"4","ts":"2026-09-27T10:04:00Z","sender":"d"}' '{"id":"2","ts":"2026-09-27T10:02:00Z","sender":"b"}'`,
			},
		},
	}
	if err := create(ctx, svc, replay); err != nil {
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
	capped := spec{
		Name: "feed4",
		Events: &manifest.Events{
			Command: "sleep 30",
			Format:  "json",
			ID:      "id",
			Catchup: &manifest.Catchup{
				Command:   `i=0; while [ $i -lt 50 ]; do i=$((i+1)); echo "{\"id\":\"c$i\"}"; sleep 0.05; done`,
				MaxEvents: 3,
			},
		},
	}
	if err := create(ctx, svc, capped); err != nil {
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
	empty := spec{
		Name: "feed3",
		Events: &manifest.Events{
			Command: "sleep 30",
			Format:  "json",
			ID:      "id",
			Catchup: &manifest.Catchup{Command: "echo should-not-run"},
		},
	}
	if err := create(ctx, svc, empty); err != nil {
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
		go func() { errs <- create(ctx, svc, eventsDef("dup", "true")) }()
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
			track := "minor"
			p := connectors.EntryPatch{Inputs: &env}
			if i%2 == 1 {
				p = connectors.EntryPatch{Track: &track}
			}
			if err := svc.Patch(ctx, "dup", p); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	e, _ := svc.Entry("dup")
	if e.Inputs["K"] != "v" || e.Track != "minor" {
		t.Fatalf("lost an edit: inputs %v track %q", e.Inputs, e.Track)
	}
}

// TestConnectorsRewriteDryRunKeepsLiveBlocks proves a dry run does not change
// the pointer blocks of the live definitions.

// TestConnectorsEditRestarts proves an edit that changes the process
// restarts a live instance.
func TestConnectorsEditRestarts(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	if err := create(ctx, svc, eventsDef("wd", "sleep 30")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "wd"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop("wd") })
	waitStatus(t, svc, "wd", "listening")
	inputs := map[string]string{"A": "1"}
	if err := svc.Patch(ctx, "wd", connectors.EntryPatch{Inputs: &inputs}); err != nil {
		t.Fatal(err)
	}
	row, _ := svc.Get("wd")
	if n := row["events"].(map[string]any)["restarts"]; n != 1 {
		t.Fatalf("restarts %v, want 1", n)
	}
}

// TestResolveActionsSkill proves the skill folder of a file entry projects
// with the connector name.
func TestResolveActionsSkill(t *testing.T) {
	st, ws, _ := openStack(t)
	svc := connectors.New(st, ws, skills.New(st))
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skill", "SKILL.md"), []byte("---\nname: psql\ndescription: d\n---\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(inline("psql", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"psql"}}}
	}).Manifest)
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(ctx, connectors.Entry{Name: "psql", From: "file:" + filepath.Join(dir, manifest.FileName)}); err != nil {
		t.Fatal(err)
	}
	acts, err := svc.ResolveActions(ctx, "", []string{"psql"})
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].Skill != "psql" || acts[0].SkillDir != filepath.Join(dir, "skill") {
		t.Fatalf("actions = %+v", acts)
	}
	if _, err := svc.ResolveActions(ctx, "", []string{"nope"}); err == nil {
		t.Fatal("unknown name resolved")
	}
}

// TestConnectorsProtocolSession drives a trebi-connector/1 adapter: the
// status gate, a login flow, a room call, one event, and a logout.
func TestConnectorsProtocolSession(t *testing.T) {
	st, _, svc := openStack(t)
	ctx := context.Background()
	sb := &fakeadapter.Sandbox{
		Result: protocol.InitializeResult{Login: []string{protocol.StepQR}, Features: []string{manifest.FeatureRoomsList}},
		Rooms:  []protocol.Room{{ID: "r1", Name: "General", Kind: protocol.RoomGroup}},
		Events: []protocol.Event{{ID: "e1", Type: "message", TS: "2026-09-28T10:00:00Z", Room: &protocol.Room{ID: "r1"}, Text: "hi"}},
	}
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := sb.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	e := inline("chat", func(m *manifest.Manifest) {
		m.Events = &manifest.Events{Protocol: manifest.ProtocolConnector, Command: `"` + os.Args[0] + `"`, Types: []manifest.EventType{{Type: "message"}}}
		m.Channel = &manifest.Channel{Features: []string{manifest.FeatureRoomsList}}
	})
	e.Inputs = map[string]string{fakeadapter.Env: "sandbox:" + path}
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var states []string
	svc.OnStatus(func(_ context.Context, instance, state string) {
		mu.Lock()
		defer mu.Unlock()
		states = append(states, instance+":"+state)
	})
	if err := svc.Start(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop("chat") })
	waitUntil(t, 5*time.Second, func() bool { return svc.AuthState("chat").State == protocol.StateAuthRequired }, "no auth_required status")
	if !svc.Down("chat") {
		t.Fatal("an instance that needs a login is not down")
	}
	if !svc.IsChannel("chat") {
		t.Fatal("chat is not a channel")
	}

	f, err := svc.Login(ctx, "chat", protocol.StepQR)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool { d := f.Current().Done; return d != nil && d.OK }, "login did not finish")
	waitUntil(t, 5*time.Second, func() bool { return svc.AuthState("chat").State == protocol.StateConnected }, "not connected")
	if svc.Down("chat") {
		t.Fatal("a connected instance is down")
	}
	if got, ok := svc.FindFlow("chat", f.ID); !ok || got != f {
		t.Fatal("FindFlow lost the flow")
	}
	waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(states, "chat:"+protocol.StateConnected)
	}, "no connected hook")

	var page protocol.RoomPage
	if err := svc.Call(ctx, "chat", protocol.MethodRoomsList, protocol.RoomsListParams{}, &page); err != nil || len(page.Rooms) != 1 {
		t.Fatalf("rooms %+v %v", page, err)
	}
	if err := svc.Call(ctx, "chat", protocol.MethodThreadsList, protocol.ThreadsListParams{Room: "r1"}, nil); protocol.CodeOf(err) != protocol.CodeUnsupported {
		t.Fatalf("threads without the feature: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		var n int
		_ = st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE connector='chat' AND source_id='e1'`).Scan(&n) // retried until the deadline
		return n == 1
	}, "event e1 not stored")

	if err := svc.Logout(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool { return svc.AuthState("chat").State == protocol.StateAuthRequired }, "logout kept the session")
}

// TestConnectorsDoctor proves the findings of a broken file entry.
func TestConnectorsDoctor(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	dir := t.TempDir()
	body, _ := json.Marshal(inline("tool", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"definitely-not-on-path-xyz"}}}
		m.Install = &manifest.Install{Bin: "definitely-not-on-path-xyz", VersionCommand: "echo tool 0.9.0"}
	}).Manifest)
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(ctx, connectors.Entry{Name: "tool", From: "file:" + filepath.Join(dir, manifest.FileName)}); err != nil {
		t.Fatal(err)
	}
	codes := func() []string {
		var out []string
		for _, f := range svc.Doctor(ctx) {
			out = append(out, f.Code)
		}
		return out
	}
	want := []string{"connector_binary_missing", "connector_version_mismatch", "connector_skill_missing"}
	if got := codes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("codes %v, want %v", got, want)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skill", "SKILL.md"), []byte("---\nname: tool\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := baseManifest("tool")
	m.Skill = &manifest.Skill{Compat: ">=2.0.0"}
	m.Actions = &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"sh"}}}
	body, _ = json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := codes(); !reflect.DeepEqual(got, []string{"connector_skill_compat"}) {
		t.Fatalf("codes %v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte("schema: nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := codes(); !reflect.DeepEqual(got, []string{"connector_manifest_invalid"}) {
		t.Fatalf("codes %v", got)
	}
}

// TestConnectorsConformance runs the live checks against a logged-in
// sandbox and proves that the adapter keeps running.
func TestConnectorsConformance(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	sb := &fakeadapter.Sandbox{
		Result:   protocol.InitializeResult{Login: []string{protocol.StepQR}, Features: []string{manifest.FeatureRoomsList}},
		Rooms:    []protocol.Room{{ID: "r1", Name: "General"}},
		LoggedIn: true,
	}
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := sb.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	e := inline("chat", func(m *manifest.Manifest) {
		m.Events = &manifest.Events{Protocol: manifest.ProtocolConnector, Command: `"` + os.Args[0] + `"`, Types: []manifest.EventType{{Type: "message"}}}
		m.Channel = &manifest.Channel{Features: []string{manifest.FeatureRoomsList}}
		m.Setup = &manifest.Setup{Login: []string{"qr"}}
	})
	e.Inputs = map[string]string{fakeadapter.Env: "sandbox:" + path}
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Conformance(ctx, "chat", ""); err == nil {
		t.Fatal("a stopped instance must fail")
	}
	if err := svc.Start(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop("chat") })
	waitUntil(t, 5*time.Second, func() bool { return svc.AuthState("chat").State == protocol.StateConnected }, "no connected status")
	res, err := svc.Conformance(ctx, "chat", "r1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range res {
		got[r.Check] = r.Status
		if r.Status == protocol.CheckFail {
			t.Errorf("%s failed: %s", r.Check, r.Message)
		}
	}
	if got["send once"] != protocol.CheckPass || got["login"] != protocol.CheckSkip || got["shutdown"] != protocol.CheckSkip {
		t.Fatalf("results = %v", got)
	}
	if svc.AuthState("chat").State != protocol.StateConnected {
		t.Fatal("the adapter stopped")
	}
}
