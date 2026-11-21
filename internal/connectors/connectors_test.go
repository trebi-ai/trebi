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

	"github.com/trebi-ai/trebi/internal/hometest"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/conformance"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/connectors/protocol/fakeadapter"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/waittest"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// templateDB is a migrated store that openStack copies: the migrations are slow under -race.
var templateDB string //nolint:gochecknoglobals // set once in TestMain, read only

func TestMain(m *testing.M) {
	if fakeadapter.Enabled() {
		os.Exit(fakeadapter.Main())
	}
	dir, err := os.MkdirTemp("", "connectors-test")
	if err != nil {
		panic(err)
	}
	templateDB = filepath.Join(dir, "t.db")
	st, err := store.OpenPath(templateDB)
	if err != nil {
		panic(err)
	}
	if err := st.Close(); err != nil {
		panic(err)
	}
	code := hometest.Run(m)
	_ = os.RemoveAll(dir) // a leftover temp dir is harmless
	os.Exit(code)
}

// spec is a short form of an inline connection with an events block. The
// type and the slug are Name.
type spec struct {
	Name   string
	Env    map[string]string
	Events *manifest.Events
}

func TestConnectorsCreatePatchDelete(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()

	e := inline("feed", func(m *manifest.Manifest) {
		m.Events = &manifest.Events{Command: "rss-fetch https://example.com/feed.xml", Format: "json", ID: "guid"}
	})
	c, err := svc.Create(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if !connectors.IsConnectionID(c.ID) || c.Slug != "feed" || c.Type != "feed" {
		t.Fatalf("created %+v", c)
	}
	if fi, err := os.Stat(filepath.Join(home.ConnectionDir(c.ID), "connection.yaml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("record %v %v", fi, err)
	}
	if _, err := svc.Create(ctx, e); !errors.Is(err, connectors.ErrExists) {
		t.Fatalf("double create: %v", err)
	}
	if _, err := svc.Create(ctx, eventsDef("email", "x").entry()); !errors.Is(err, connectors.ErrReserved) {
		t.Fatalf("reserved create: %v", err)
	}
	if _, err := svc.Create(ctx, connectors.Connection{Type: "bad", Slug: "bad", From: connectors.FromInline}); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("inline with no manifest: %v", err)
	}
	if _, err := os.Stat(home.ConnectionsDir()); err != nil {
		t.Fatal(err)
	}

	// Patch sets inputs and a merge patch on the manifest.
	inputs := map[string]string{"FEED": "https://example.com/x"}
	p := connectors.ConnectionPatch{
		Inputs: &inputs,
		Patch:  json.RawMessage(`{"events":{"command":"rss-fetch v3","format":"json","id":"guid"},"actions":{"cli":{"commands":["rss-fetch"]}}}`),
	}
	if _, err := svc.Patch(ctx, "feed", p); err != nil {
		t.Fatal(err)
	}
	row, _ := svc.Get(c.ID)
	if row["inputs"].(map[string]string)["FEED"] != "***" {
		t.Fatalf("patch inputs %v", row["inputs"])
	}
	if got, _ := svc.Lookup("feed"); got.Inputs["FEED"] != "https://example.com/x" {
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
	if _, err := svc.Patch(ctx, "feed", connectors.ConnectionPatch{Patch: json.RawMessage(`{"events":{"restart":"sometimes"}}`)}); !errors.As(err, &ve) || ve.Layer != connectors.LayerGlobal {
		t.Fatalf("bad patch err = %v", err)
	}
	// A null patch removes the layer.
	if _, err := svc.Patch(ctx, "feed", connectors.ConnectionPatch{Patch: json.RawMessage("null")}); err != nil {
		t.Fatal(err)
	}
	row, _ = svc.Get("feed")
	if row["actions"] != nil {
		t.Fatalf("actions after null patch: %v", row["actions"])
	}

	// Delete removes the folder. The second delete is ErrNotFound.
	if err := svc.Delete(ctx, "feed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home.ConnectionDir(c.ID)); !os.IsNotExist(err) {
		t.Fatalf("folder after delete: %v", err)
	}
	if err := svc.Delete(ctx, c.ID); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// TestConnectorsTitle proves a display name replaces the manifest title on
// the row, keeps the name, and an empty title restores the manifest title.
func TestConnectorsTitle(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, inline("feed", func(m *manifest.Manifest) { m.Events = &manifest.Events{Command: "rss-fetch", Format: "line"} })); err != nil {
		t.Fatal(err)
	}
	row, _ := svc.Get("feed")
	base, _ := row["title"].(string)

	title := "  Work feed "
	if _, err := svc.Patch(ctx, "feed", connectors.ConnectionPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	row, _ = svc.Get("feed")
	if row["title"] != "Work feed" || row["slug"] != "feed" {
		t.Fatalf("row title %v name %v", row["title"], row["slug"])
	}
	if e, _ := svc.Lookup("feed"); e.Title != "Work feed" {
		t.Fatalf("stored title %q", e.Title)
	}

	for _, bad := range []string{strings.Repeat("x", 61), "two\nlines"} {
		if _, err := svc.Patch(ctx, "feed", connectors.ConnectionPatch{Title: &bad}); !errors.Is(err, connectors.ErrInvalid) {
			t.Fatalf("title %q: %v", bad, err)
		}
	}

	empty := ""
	if _, err := svc.Patch(ctx, "feed", connectors.ConnectionPatch{Title: &empty}); err != nil {
		t.Fatal(err)
	}
	if row, _ = svc.Get("feed"); row["title"] != base {
		t.Fatalf("cleared title %v, want %q", row["title"], base)
	}
}

// TestConnectorsSecretInputs proves a literal secret input moves to the
// secrets folder of the connection, and Delete removes it. A secret: value
// stays a reference to a user secret.
func TestConnectorsSecretInputs(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	e := inline("gh", func(m *manifest.Manifest) {
		m.Setup = &manifest.Setup{Inputs: []manifest.Input{{Name: "TOKEN", Secret: true, Required: true}}}
		m.Actions = &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"gh"}}}
	})
	e.Inputs = map[string]string{"TOKEN": "t0k-secret"}
	c, err := svc.Create(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Inputs["TOKEN"]; ok {
		t.Fatalf("the record keeps the literal: %+v", c.Inputs)
	}
	f := connectors.NewFolder(c.ID)
	if v, err := f.Secret("TOKEN"); err != nil || v != "t0k-secret" {
		t.Fatalf("owned secret %q %v", v, err)
	}
	if fi, err := os.Stat(filepath.Join(f.Dir(), "secrets", "TOKEN")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.Dir(), "connection.yaml")); strings.Contains(string(b), "t0k-secret") {
		t.Fatalf("connection.yaml holds the secret: %s", b)
	}
	row, _ := svc.Get("gh")
	if in := row["inputs"].(map[string]string); in["TOKEN"] != "***" {
		t.Fatalf("row inputs %v", in)
	}
	r, err := svc.Resolve("gh", "")
	if err != nil {
		t.Fatal(err)
	}
	if env, missing := r.Env(); env["TOKEN"] != "t0k-secret" || len(missing) != 0 {
		t.Fatalf("env %v missing %v", env, missing)
	}
	if err := svc.Delete(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Secret("TOKEN"); err == nil {
		t.Fatal("owned secret survived delete")
	}

	// A secret: reference reads the user secret and owns nothing.
	if err := secrets.Set("gh_TOKEN", "mine"); err != nil {
		t.Fatal(err)
	}
	e.Inputs = map[string]string{"TOKEN": "secret:gh_TOKEN"}
	c, err = svc.Create(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := connectors.NewFolder(c.ID).Secrets(); len(got) != 0 {
		t.Fatalf("owned = %v", got)
	}
	r, _ = svc.Resolve("gh", "")
	if env, _ := r.Env(); env["TOKEN"] != "mine" {
		t.Fatalf("env %v", env)
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
	if _, err := svc.Create(ctx, e); err != nil {
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
	if _, err := svc.Create(ctx, fileConn("local", filepath.Join(oldDir, "conn", manifest.FileName))); err != nil {
		t.Fatal(err)
	}

	preview, err := svc.RewritePathPrefix(oldDir, newDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(preview, ",") != "local,whatsapp" {
		t.Fatalf("preview %v", preview)
	}
	if got, _ := svc.Lookup("whatsapp"); got.Inputs["ROOT"] != oldDir+"/data" || got.Manifest.Events.Command != "whatsapp-cli listen --dir "+oldDir {
		t.Fatalf("dry run changed the live entry: %+v", got)
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RewritePathPrefix(oldDir, newDir, false); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Lookup("whatsapp")
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
	if local, _ := svc.Lookup("local"); local.FilePath() != filepath.Join(newDir, "conn", manifest.FileName) {
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
	if _, err := svc.Create(ctx, e); err != nil {
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
	if row["status"] != "stopped" || ev["format"] != "json" || row["last_err"] != "" || ev["protocol"] != "lines" || ev["restart"] != "on-failure" {
		t.Fatalf("events row %v", row)
	}
	if _, ok := row["rss_bytes"]; ok {
		t.Fatalf("a stopped row has rss_bytes: %v", row)
	}
	counters := row["counters"].(map[string]any)
	if counters["events"] != int64(0) || counters["duplicates"] != int64(0) || counters["parse_errors"] != int64(0) {
		t.Fatalf("counters %v", counters)
	}
	if id, _ := row["id"].(string); !connectors.IsConnectionID(id) || row["slug"] != "feed" || row["type"] != "feed" || row["folder"] != home.ConnectionDir(id) {
		t.Fatalf("identity fields %v", row)
	}
}

// TestConnectorsLoadFolders proves Load reads each connection folder, skips
// a native cursor folder, and applies the manifest defaults.
func TestConnectorsLoadFolders(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	c, err := svc.Create(ctx, eventsDef("feed", "echo").entry())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.PutCursor(ctx, "email", "e1"); err != nil {
		t.Fatal(err)
	}
	fresh := connectors.New(nil, nil, nil)
	if err := fresh.Load(); err != nil {
		t.Fatal(err)
	}
	row, err := fresh.Get("feed")
	if err != nil || row["id"] != c.ID {
		t.Fatalf("row %v %v", row, err)
	}
	if ev := row["events"].(map[string]any); ev["restart"] != "on-failure" {
		t.Fatalf("default restart %v", ev["restart"])
	}
	if len(fresh.List("")) != 1 {
		t.Fatalf("list %v", fresh.List(""))
	}
}

// TestConnectorsFailedLoadBlocksWrites proves a folder that does not load
// keeps its error and refuses writes to that connection, while the other
// connections stay writable.
func TestConnectorsFailedLoadBlocksWrites(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	mail, err := svc.Create(ctx, eventsDef("mail", "echo").entry())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, eventsDef("feed", "echo").entry()); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(home.ConnectionDir(mail.ID), "connection.yaml")
	good, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	bad := append(append([]byte{}, good...), "frm: x\n"...)
	if err := os.WriteFile(record, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(); err == nil || !strings.Contains(err.Error(), mail.ID) {
		t.Fatalf("load: %v", err)
	}
	if svc.LoadErr() == nil {
		t.Fatal("LoadErr is nil after a failed load")
	}
	inputs := map[string]string{"A": "1"}
	if _, err := svc.Patch(ctx, "mail", connectors.ConnectionPatch{Inputs: &inputs}); !errors.Is(err, connectors.ErrLoadFailed) {
		t.Fatalf("patch: %v", err)
	}
	if got, _ := os.ReadFile(record); string(got) != string(bad) {
		t.Fatalf("file changed: %q", got)
	}
	row, _ := svc.Get("mail")
	if row["load_error"] == nil {
		t.Fatalf("row has no load error: %v", row)
	}
	if _, err := svc.Patch(ctx, "feed", connectors.ConnectionPatch{Inputs: &inputs}); err != nil {
		t.Fatalf("patch of a good connection: %v", err)
	}
	if err := os.WriteFile(record, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(); err != nil || svc.LoadErr() != nil {
		t.Fatalf("load after fix: %v %v", err, svc.LoadErr())
	}
	if _, err := svc.Patch(ctx, "mail", connectors.ConnectionPatch{Inputs: &inputs}); err != nil {
		t.Fatalf("patch after fix: %v", err)
	}

	// A folder that never loaded is a list row, and Delete removes it.
	broken := connectors.NewConnectionID()
	if err := os.MkdirAll(home.ConnectionDir(broken), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.ConnectionDir(broken), "connection.yaml"), []byte("id: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(); err == nil {
		t.Fatal("broken folder loaded")
	}
	if row, err := svc.Get(broken); err != nil || row["load_error"] == nil {
		t.Fatalf("broken row %v %v", row, err)
	}
	if err := svc.Delete(ctx, broken); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home.ConnectionDir(broken)); !os.IsNotExist(err) {
		t.Fatalf("broken folder kept: %v", err)
	}
}

// TestConnectorsMaskedInputsRoundTrip proves a masked input that comes back
// on a write keeps the stored value.
func TestConnectorsMaskedInputsRoundTrip(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	e := eventsDef("api", "true").entry()
	e.Inputs = map[string]string{"TOKEN": "t0k", "REF": "secret:api_TOKEN"}
	if _, err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	row, _ := svc.Get("api")
	if in := row["inputs"].(map[string]string); in["TOKEN"] != "***" || in["REF"] != "secret:api_TOKEN" {
		t.Fatalf("inputs %v", in)
	}
	in := map[string]string{"TOKEN": "***"}
	if _, err := svc.Patch(ctx, "api", connectors.ConnectionPatch{Inputs: &in}); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Lookup("api"); got.Inputs["TOKEN"] != "t0k" {
		t.Fatalf("patch %v", got.Inputs)
	}
	in = map[string]string{"NEW": "***"}
	if _, err := svc.Patch(ctx, "api", connectors.ConnectionPatch{Inputs: &in}); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("new mask: %v", err)
	}
}

// TestConnectorsWorkspaceOverride covers the workspace layer: an actions
// patch applies in that workspace only, and disabled turns the connection
// off there. The layer cannot hold inputs.
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
		m.Setup = &manifest.Setup{Inputs: []manifest.Input{{Name: "ROOM"}}}
		m.Actions = &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"chat"}}}
		m.Events = &manifest.Events{Command: "sleep 30", Format: "json"}
	})
	e.Inputs = map[string]string{"ROOM": "global"}
	c, err := svc.Create(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetOverride(ctx, w.ID, "missing", &workspaces.ConnectorOverride{}); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("override of a missing connector: %v", err)
	}
	bad := &workspaces.ConnectorOverride{Patch: map[string]any{"events": map[string]any{"command": "x"}}}
	if err := svc.SetOverride(ctx, w.ID, "chat", bad); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("events patch in a workspace: %v", err)
	}

	patchOnly := &workspaces.ConnectorOverride{Patch: map[string]any{"actions": map[string]any{"cli": map[string]any{"commands": []any{"chat", "chat-admin"}}}}}
	if err := svc.SetOverride(ctx, w.ID, "chat", patchOnly); err != nil {
		t.Fatal(err)
	}
	if got := svc.ConnectionFor("chat", w.ID); got != c.ID {
		t.Fatalf("connection in the workspace %q", got)
	}
	r, err := svc.Resolve("chat", w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Manifest.Actions.CLI.Commands) != 2 || r.ID != c.ID {
		t.Fatalf("resolved %+v", r)
	}
	if g, _ := svc.Resolve("chat", ""); len(g.Manifest.Actions.CLI.Commands) != 1 || g.Inputs["ROOM"] != "global" {
		t.Fatalf("global layer changed: %+v", g)
	}
	if ov, ok, err := svc.Override(ctx, w.ID, c.ID); err != nil || !ok || ov.Patch == nil {
		t.Fatalf("override %+v %v %v", ov, ok, err)
	}

	if err := svc.SetOverride(ctx, w.ID, "chat", &workspaces.ConnectorOverride{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := svc.ConnectionFor("chat", w.ID); got != "" {
		t.Fatalf("disabled connection %q", got)
	}
	if got := svc.ConnectionFor("chat", ""); got != c.ID {
		t.Fatalf("global connection %q", got)
	}
	if _, err := svc.Resolve("chat", w.ID); !errors.Is(err, connectors.ErrDisabled) {
		t.Fatalf("disabled resolve: %v", err)
	}
	if row, _ := svc.GetFor("chat", w.ID); row["disabled"] != true {
		t.Fatalf("row %v", row)
	}
	if _, err := svc.ResolveActions(ctx, w.ID, []string{"chat"}); err == nil {
		t.Fatal("disabled connector gave actions")
	}

	if err := svc.SetOverride(ctx, w.ID, "chat", nil); err != nil {
		t.Fatal(err)
	}
	if got := svc.ConnectionFor("chat", w.ID); got != c.ID {
		t.Fatalf("removed override connection %q", got)
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
	if row["slug"] != "sling" || row["provenance"] != "inherited" || row["managed"] != false {
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
	if _, err := svc.Create(ctx, e); err != nil {
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
	if row := list[0]; row["managed"] != true || row["slug"] != "sling" || row["origins"] == nil {
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
	waitUntil(t, func() bool {
		row, _ := svc.Get("tick")
		return eventCount(row) >= 1
	}, "no events observed")
	row, _ := svc.Get("tick")
	if n, _ := row["rss_bytes"].(uint64); n == 0 {
		t.Fatalf("a running process has no rss_bytes: %v", row["rss_bytes"])
	}
	if err := svc.Stop("tick"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, "tick", "stopped")
	row, _ = svc.Get("tick")
	if _, ok := row["rss_bytes"]; ok {
		t.Fatalf("a stopped process has rss_bytes: %v", row["rss_bytes"])
	}
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
	waitUntil(t, func() bool {
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
	if n, _ := row["restarts"].(int); n != 0 {
		t.Fatalf("never restarted: %v", row)
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
	waitUntil(t, func() bool {
		row, _ := svc.Get("onfail")
		n, _ := row["restarts"].(int)
		return n >= 1
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
	if n, _ := row["restarts"].(int); n != 0 {
		t.Fatalf("clean exit restarted: %v", row)
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
	waitUntil(t, func() bool {
		row, _ := svc.Get("always")
		n, _ := row["restarts"].(int)
		return n >= 1
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
	if le := row["last_err"].(string); le == "" {
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
	want := map[string]any{"events": int64(2), "events_total": int64(2), "duplicates": int64(1), "parse_errors": int64(1), "rejected": int64(0), "dropped": int64(0)}
	var got map[string]any
	waitUntil(t, func() bool {
		row, _ := svc.Get("count")
		got, _ = row["counters"].(map[string]any)
		return reflect.DeepEqual(got, want)
	}, "counters")
}

// TestConnectorsInjectEventMatchesJob enqueues via the ingest path when a
// job subscribes with match true.
func TestConnectorsInjectEventMatchesJob(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()

	wsPath := filepath.Join(home.Base(), "ws")
	routeAgent(t, wsPath, "sub", "src", job.TriggerYAML{Match: "true"})
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
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='sub')`).Scan(&n); err != nil {
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
	// A route that gets nothing never fires.
	routingtest.Write(t, wsPath, "src", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Assistant: true, Gets: routing.GetsEveryMessage},
		{Assistant: true, Gets: routing.GetsOff},
	}})
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
	routeAgent(t, wsPath, "guard", "src", job.TriggerYAML{Match: "true", Allowlist: []string{"alice", "12345"}})
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
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='guard')`).Scan(&n); err != nil {
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
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes WHERE job_id=(SELECT id FROM job_index WHERE name='guard') AND outcome='blocked'`).Scan(&n); err != nil {
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
	routeAgent(t, wsPath, "open", "open", job.TriggerYAML{Match: "true"})
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectEvent(ctx, "open", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='open')`).Scan(&n); err != nil {
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
	for name, event := range map[string]string{"mail": "email", "chat": "src"} {
		routeAgent(t, wsPath, name, event, job.TriggerYAML{Match: "true", Allowlist: []string{"Ops@Example.com"}})
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
	for name, want := range map[string]int{"mail": 1, "chat": 0} {
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name=?)`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("%s queue rows = %d; want %d", name, n, want)
		}
	}
}

func TestConnectorsDedup(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()

	wsPath := filepath.Join(home.Base(), "ws")
	routeAgent(t, wsPath, "dedup", "webhook", job.TriggerYAML{Match: "true"})
	w, err := ws.Add(ctx, wsPath, "dedup-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	meta := connectors.NativeMeta{SourceID: "evt-1"}
	id1, err := svc.InjectNative(ctx, "webhook", map[string]any{"n": 1}, meta)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := svc.InjectNative(ctx, "webhook", map[string]any{"n": 1}, meta)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatalf("duplicate returned the same id %s", id1)
	}
	var events, queue int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE connector='webhook'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("events rows %d want 1", events)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='dedup')`).Scan(&queue); err != nil {
		t.Fatal(err)
	}
	if queue != 1 {
		t.Fatalf("queue rows %d want 1", queue)
	}
	// A native event never moves the position (decision 2).
	cur, _ := svc.GetCursor(ctx, "webhook")
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
	for name, conn := range map[string]string{"hook": "webhook", "feed": "src"} {
		routeAgent(t, wsPath, name, conn, job.TriggerYAML{Match: "event.n == 7"})
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
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='hook') AND event_id=?`, nativeID); n != 2 {
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
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='feed')`); n != 2 {
		t.Fatalf("feed queue rows = %d; want 2", n)
	}
	if err := svc.Redispatch(ctx, "missing"); !errors.Is(err, connectors.ErrEventNotFound) {
		t.Fatalf("missing err = %v", err)
	}
}

// TestConnectorsInjectNativeKeepsEmailPosition proves an email event leaves
// the cursor of the email service alone. A native cursor lives in a folder
// named after the connector, and Load does not read it as a connection.
func TestConnectorsInjectNativeKeepsEmailPosition(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	if err := svc.PutCursor(ctx, "email", "42"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home.ConnectionDir("email"), "cursor.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InjectNative(ctx, "email", map[string]any{"subject": "hi"}, connectors.NativeMeta{SourceID: "<id@x>"}); err != nil {
		t.Fatal(err)
	}
	cur, _ := svc.GetCursor(ctx, "email")
	if cur.LastEventID != "42" {
		t.Fatalf("email position = %q; want 42", cur.LastEventID)
	}
	if err := svc.Load(); err != nil {
		t.Fatalf("load with a native cursor folder: %v", err)
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
	waitUntil(t, func() bool {
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
	waitUntil(t, func() bool {
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
	waitUntil(t, func() bool {
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
	waitUntil(t, func() bool {
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

// TestConnectorsConcurrentWrites proves writes are serialized: two creates of
// one slug give one ErrExists, and parallel patches keep every edit.
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
			p := connectors.ConnectionPatch{Inputs: &env}
			if i%2 == 1 {
				p = connectors.ConnectionPatch{Track: &track}
			}
			if _, err := svc.Patch(ctx, "dup", p); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	e, _ := svc.Lookup("dup")
	if e.Inputs["K"] != "v" || e.Track != "minor" {
		t.Fatalf("lost an edit: inputs %v track %q", e.Inputs, e.Track)
	}
}

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
	if _, err := svc.Patch(ctx, "wd", connectors.ConnectionPatch{Inputs: &inputs}); err != nil {
		t.Fatal(err)
	}
	row, _ := svc.Get("wd")
	if n := row["restarts"]; n != 1 {
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
	if _, err := svc.Create(ctx, fileConn("psql", filepath.Join(dir, manifest.FileName))); err != nil {
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
		Result:  protocol.InitializeResult{Features: []string{manifest.FeatureRoomsList}},
		Methods: []protocol.LoginMethod{{ID: "qr", Type: protocol.LoginPhone, Available: true}},
		Rooms:   []protocol.Room{{ID: "r1", Name: "General", Kind: protocol.RoomGroup}},
		Events:  []protocol.Event{{ID: "e1", Type: "message", TS: "2026-09-28T10:00:00Z", Room: &protocol.Room{ID: "r1"}, Text: "hi"}},
	}
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := sb.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	e := inline("chat", func(m *manifest.Manifest) {
		m.Events = &manifest.Events{Protocol: manifest.ProtocolConnector, Command: `"` + os.Args[0] + `"`, Types: []manifest.EventType{{Type: "message"}}}
		m.Channel = &manifest.Channel{Features: []string{manifest.FeatureRoomsList}}
		m.Setup = &manifest.Setup{Login: []manifest.LoginMethod{{ID: "qr", Type: manifest.LoginPhone}, {ID: "app", Type: manifest.LoginExisting}}}
	})
	e.Inputs = map[string]string{fakeadapter.Env: "sandbox:" + path}
	c, err := svc.Create(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	reasons := func() map[string]string {
		methods, err := svc.LoginMethods(ctx, "chat")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, m := range methods {
			out[m.ID] = m.Type + ":" + m.Reason
		}
		return out
	}
	if got := reasons(); got["qr"] != "phone:Connect the connector first." || len(got) != 2 {
		t.Fatalf("methods of a stopped connector = %v", got)
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
	waitUntil(t, func() bool { return svc.AuthState("chat").State == protocol.StateAuthRequired }, "no auth_required status")
	if !svc.Down("chat") {
		t.Fatal("an instance that needs a login is not down")
	}
	if !svc.IsChannel("chat") {
		t.Fatal("chat is not a channel")
	}

	if got := reasons(); got["qr"] != "phone:" || !strings.HasPrefix(got["app"], "existing:This version") {
		t.Fatalf("methods of a running connector = %v", got)
	}
	if _, err := svc.Login(ctx, "chat", "app"); protocol.CodeOf(err) != protocol.CodeInvalid {
		t.Fatalf("an unavailable method began: %v", err)
	}
	f, err := svc.Login(ctx, "chat", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Method().ID != "qr" {
		t.Fatalf("picked %+v", f.Method())
	}
	waitUntil(t, func() bool { d := f.Current().Done; return d != nil && d.OK }, "login did not finish")
	waitUntil(t, func() bool { return svc.AuthState("chat").State == protocol.StateConnected }, "not connected")
	if svc.Down("chat") {
		t.Fatal("a connected instance is down")
	}
	if got, ok := svc.FindFlow("chat", f.ID); !ok || got != f {
		t.Fatal("FindFlow lost the flow")
	}
	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(states, c.ID+":"+protocol.StateConnected)
	}, "no connected hook")

	var page protocol.RoomPage
	if err := svc.Call(ctx, "chat", protocol.MethodRoomsList, protocol.RoomsListParams{}, &page); err != nil || len(page.Rooms) != 1 {
		t.Fatalf("rooms %+v %v", page, err)
	}
	if err := svc.Call(ctx, "chat", protocol.MethodThreadsList, protocol.ThreadsListParams{Room: "r1"}, nil); protocol.CodeOf(err) != protocol.CodeUnsupported {
		t.Fatalf("threads without the feature: %v", err)
	}
	waitUntil(t, func() bool {
		var n int
		_ = st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE connector='chat' AND source_id='e1'`).Scan(&n) // retried until the deadline
		return n == 1
	}, "event e1 not stored")

	if err := svc.Logout(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return svc.AuthState("chat").State == protocol.StateAuthRequired }, "logout kept the session")

	if _, err := svc.Login(ctx, "chat", "qr"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return svc.AuthState("chat").State == protocol.StateConnected }, "not connected again")
	if err := svc.Stop("chat"); err != nil {
		t.Fatal(err)
	}
	if st := svc.AuthState("chat"); st.State != "" {
		t.Fatalf("a stopped instance keeps the status %+v", st)
	}
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
	if _, err := svc.Create(ctx, fileConn("tool", filepath.Join(dir, manifest.FileName))); err != nil {
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
		Result:   protocol.InitializeResult{Features: []string{manifest.FeatureRoomsList}},
		Methods:  []protocol.LoginMethod{{ID: "qr", Type: protocol.LoginPhone, Available: true}},
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
		m.Setup = &manifest.Setup{Login: []manifest.LoginMethod{{ID: "qr", Type: manifest.LoginPhone}}}
	})
	e.Inputs = map[string]string{fakeadapter.Env: "sandbox:" + path}
	if _, err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Conformance(ctx, "chat", "", []string{conformance.SuiteProtocol}); err == nil {
		t.Fatal("a stopped instance must fail")
	}
	if err := svc.Start(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop("chat") })
	waitUntil(t, func() bool { return svc.AuthState("chat").State == protocol.StateConnected }, "no connected status")
	res, err := svc.Conformance(ctx, "chat", "r1", []string{conformance.SuiteProtocol})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range res {
		got[r.Check] = r.Status
		if r.Status == conformance.StatusFail {
			t.Errorf("%s failed: %s", r.Check, r.Message)
		}
	}
	if got["send once"] != conformance.StatusPass || got["login"] != conformance.StatusSkip || got["shutdown"] != conformance.StatusSkip {
		t.Fatalf("results = %v", got)
	}
	if svc.AuthState("chat").State != protocol.StateConnected {
		t.Fatal("the adapter stopped")
	}
}

// TestConnectorsTwoOfOneType proves two connections of one type get their
// own folder, state, log, and events rows.
func TestConnectorsTwoOfOneType(t *testing.T) {
	st, _, svc := openStack(t)
	ctx := context.Background()
	d := spec{Name: "feed", Events: &manifest.Events{
		Command: `printf '{"id":"a"}\n'; sleep 30`, Format: manifest.FormatJSON, ID: "id", Restart: manifest.RestartNever,
	}}
	first, err := svc.Create(ctx, d.entry())
	if err != nil {
		t.Fatal(err)
	}
	e := d.entry()
	e.Slug = ""
	second, err := svc.Create(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if second.Slug != "feed-2" || second.Type != "feed" || second.ID == first.ID {
		t.Fatalf("second = %+v", second)
	}
	var dirs, states, logs []string
	for _, c := range []connectors.Connection{first, second} {
		r, err := svc.Resolve(c.Slug, "")
		if err != nil {
			t.Fatal(err)
		}
		lp := r.Folder().LogPath()
		dir := r.Folder().Dir()
		if dir != home.ConnectionDir(c.ID) || !strings.HasPrefix(r.StateDir(), dir) || !strings.HasPrefix(lp, dir) {
			t.Fatalf("%s: dir %s state %s log %s", c.Slug, dir, r.StateDir(), lp)
		}
		dirs, states, logs = append(dirs, dir), append(states, r.StateDir()), append(logs, lp)
		if err := svc.Start(ctx, c.Slug); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = svc.Stop(c.Slug) })
	}
	if dirs[0] == dirs[1] || states[0] == states[1] || logs[0] == logs[1] {
		t.Fatalf("shared paths: %v %v %v", dirs, states, logs)
	}
	// The same source id in two accounts is two events, one per instance.
	for _, c := range []connectors.Connection{first, second} {
		waitUntil(t, func() bool {
			var n int
			_ = st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE connector='feed' AND instance=?`, c.ID).Scan(&n)
			return n == 1
		}, "events row of "+c.Slug)
	}
}

// TestConnectorsIDAndSlug proves that a ref resolves by id or by slug, and
// that a native name is its own id and slug.
func TestConnectorsIDAndSlug(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	c, err := svc.Create(ctx, eventsDef("feed", "sleep 30").entry())
	if err != nil {
		t.Fatal(err)
	}
	if !connectors.IsConnectionID(c.ID) || connectors.IsConnectionID("feed") {
		t.Fatalf("IsConnectionID(%q)", c.ID)
	}
	byID, err := svc.Lookup(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	bySlug, err := svc.Lookup("feed")
	if err != nil || !reflect.DeepEqual(byID, bySlug) {
		t.Fatalf("lookup by slug %+v %v", bySlug, err)
	}
	for ref, want := range map[string]string{"feed": c.ID, c.ID: c.ID, "email": "email", "webhook": "webhook", "missing": ""} {
		if got := svc.IDOf(ref); got != want {
			t.Fatalf("IDOf(%q) = %q; want %q", ref, got, want)
		}
	}
	if svc.SlugOf(c.ID) != "feed" || svc.SlugOf("email") != "email" || svc.SlugOf("con_missing") != "" {
		t.Fatal("SlugOf")
	}
	if row, err := svc.Get(c.ID); err != nil || row["slug"] != "feed" || row["id"] != c.ID {
		t.Fatalf("get by id %v %v", row, err)
	}
	bad := eventsDef("x", "true").entry()
	bad.Slug = "con_x"
	if _, err := svc.Create(ctx, bad); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("con_ slug: %v", err)
	}
	slug := "news"
	if _, err := svc.Patch(ctx, c.ID, connectors.ConnectionPatch{Slug: &slug}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Lookup("feed"); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("old slug still resolves: %v", err)
	}
	if svc.IDOf("news") != c.ID {
		t.Fatal("new slug does not resolve to the same id")
	}
}

// TestConnectorsSlugRewrite proves a slug change rewrites the references in
// config.yaml, trebi.yaml, and job front matter, and puts them all back when
// one write fails.
func TestConnectorsSlugRewrite(t *testing.T) {
	_, ws, svc := openStack(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	jobDir := filepath.Join(wsPath, "digest")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jobPath := filepath.Join(jobDir, "JOB.md")
	md := "---\nid: jb_00000000a1\nharness: fake\nconnectors: [old]\n---\n\nRead old mail.\n"
	if err := os.WriteFile(jobPath, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	routesOld := routingtest.WriteRaw(t, wsPath, "old", "mode: off\nroutes: []\n")
	routesMail := filepath.Join(wsPath, routing.Dir, "mail.yaml")
	cfg := "notify:\n  route:\n    - via: old\n      to: admins\n"
	if err := os.WriteFile(home.ConfigPath(), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Add(ctx, wsPath, "rw-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	c, err := svc.Create(ctx, inline("old", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"old"}}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetOverride(ctx, w.ID, "old", &workspaces.ConnectorOverride{Disabled: true}); err != nil {
		t.Fatal(err)
	}

	slug := "mail"
	changed, err := svc.Patch(ctx, "old", connectors.ConnectionPatch{Slug: &slug})
	if err != nil {
		t.Fatal(err)
	}
	wsFile := filepath.Join(wsPath, "trebi.yaml")
	want := []string{home.ConfigPath(), jobPath, routesMail, wsFile}
	slices.Sort(want)
	slices.Sort(changed)
	if !slices.Equal(changed, want) {
		t.Fatalf("changed = %v; want %v", changed, want)
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read(jobPath); got != "---\nid: jb_00000000a1\nharness: fake\nconnectors: [mail]\n---\n\nRead old mail.\n" {
		t.Fatalf("job = %q", got)
	}
	if _, err := os.Stat(routesOld); !os.IsNotExist(err) {
		t.Fatalf("old routes file after the rename: %v", err)
	}
	if got := read(home.ConfigPath()); got != strings.Replace(cfg, "via: old", "via: mail", 1) {
		t.Fatalf("config = %q", got)
	}
	if got := read(wsFile); !strings.Contains(got, "mail:") || strings.Contains(got, "old:") {
		t.Fatalf("trebi.yaml = %q", got)
	}
	if svc.ConnectionFor("mail", w.ID) != "" {
		t.Fatal("workspace override lost after the rename")
	}

	// A failed write puts back every file and keeps the slug.
	if err := os.Chmod(jobDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(jobDir, 0o755) })
	cfgBefore, jobBefore := read(home.ConfigPath()), read(jobPath)
	slug = "post"
	if _, err := svc.Patch(ctx, "mail", connectors.ConnectionPatch{Slug: &slug}); !errors.Is(err, connectors.ErrSlugRewrite) {
		t.Fatalf("patch with a read-only job = %v", err)
	}
	if read(home.ConfigPath()) != cfgBefore || read(jobPath) != jobBefore {
		t.Fatal("rollback did not restore the files")
	}
	if _, err := os.Stat(routesMail); err != nil {
		t.Fatalf("routes file after the rollback: %v", err)
	}
	if got, err := svc.Lookup(c.ID); err != nil || got.Slug != "mail" {
		t.Fatalf("slug after rollback = %q %v", got.Slug, err)
	}
}

// TestConnectorsCursorFile proves the connector cursor lives in cursor.json
// in the connection folder.
func TestConnectorsCursorFile(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	c, err := svc.Create(ctx, eventsDef("feed", "sleep 30").entry())
	if err != nil {
		t.Fatal(err)
	}
	if cur, err := svc.GetCursor(ctx, "feed"); err != nil || cur.LastEventID != "" {
		t.Fatalf("new cursor %+v %v", cur, err)
	}
	if err := svc.PutCursor(ctx, "feed", "evt-9"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home.ConnectionDir(c.ID), "cursor.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk connectors.Cursor
	if err := json.Unmarshal(b, &onDisk); err != nil || onDisk.LastEventID != "evt-9" || onDisk.UpdatedAt == "" {
		t.Fatalf("cursor.json = %s %v", b, err)
	}
	if cur, err := svc.GetCursor(ctx, c.ID); err != nil || cur.LastEventID != "evt-9" {
		t.Fatalf("cursor by id %+v %v", cur, err)
	}
	if _, err := svc.GetCursor(ctx, "missing"); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("missing cursor: %v", err)
	}
	if err := svc.Delete(ctx, "feed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home.ConnectionDir(c.ID)); !os.IsNotExist(err) {
		t.Fatalf("folder after delete: %v", err)
	}
}

// TestResolveActionsCLIConflict proves one run cannot use the CLI actions of
// two connections of one type, and that a CLI action gets its own folders.
func TestResolveActionsCLIConflict(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	gh := inline("gh", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"gh"}}}
	})
	if _, err := svc.Create(ctx, gh); err != nil {
		t.Fatal(err)
	}
	gh.Slug = ""
	work, err := svc.Create(ctx, gh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveActions(ctx, "", []string{"gh", work.Slug}); err == nil || !strings.Contains(err.Error(), "are both gh connections") {
		t.Fatalf("conflict err = %v", err)
	}
	acts, err := svc.ResolveActions(ctx, "", []string{work.Slug})
	if err != nil {
		t.Fatal(err)
	}
	f := connectors.NewFolder(work.ID)
	if len(acts) != 1 || acts[0].ID != work.ID || acts[0].Env["TREBI_STATE_DIR"] != f.StateDir() || acts[0].Env["TREBI_CACHE_DIR"] != f.CacheDir() {
		t.Fatalf("actions = %+v", acts)
	}
}

func (s spec) entry() connectors.Connection {
	m := baseManifest(s.Name)
	m.Events = s.Events
	return connectors.Connection{Type: s.Name, Slug: s.Name, From: connectors.FromInline, Inputs: s.Env, Manifest: &m}
}

func create(ctx context.Context, svc *connectors.Service, s spec) error {
	_, err := svc.Create(ctx, s.entry())
	return err
}

// eventsDef is a minimal valid lines connector.
func eventsDef(name, command string) spec {
	return spec{Name: name, Events: &manifest.Events{Command: command, Format: manifest.FormatJSON}}
}

func baseManifest(name string) manifest.Manifest {
	return manifest.Manifest{Schema: manifest.SchemaID, Name: name, Title: name, Description: "Test connector.", Version: "1.0.0"}
}

// inline builds an inline connection from a manifest body. The type and the
// slug are name.
func inline(name string, edit func(*manifest.Manifest)) connectors.Connection {
	m := baseManifest(name)
	edit(&m)
	return connectors.Connection{Type: name, Slug: name, From: connectors.FromInline, Manifest: &m}
}

// fileConn is a file: connection of the manifest at path.
func fileConn(name, path string) connectors.Connection {
	return connectors.Connection{Type: name, Slug: name, From: "file:" + path}
}

func openStack(t *testing.T) (*store.Store, *workspaces.Service, *connectors.Service) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	db, err := os.ReadFile(templateDB)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "t.db")
	if err := os.WriteFile(path, db, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := workspaces.New(st)
	svc := connectors.New(st, ws, nil)
	return st, ws, svc
}

// waitStatus polls Get until the status of the connection reaches want.
func waitStatus(t *testing.T, svc *connectors.Service, name, want string) {
	t.Helper()
	var status string
	waitUntil(t, func() bool {
		row, _ := svc.Get(name)
		status, _ = row["status"].(string)
		return status == want
	}, "status want "+want)
}

// eventCount reads the events counter of one connection row.
func eventCount(row map[string]any) int64 {
	counters, ok := row["counters"].(map[string]any)
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

// openProbe reports every conversation as open and records the keys.
type openProbe struct {
	mu   sync.Mutex
	keys []string
}

func (p *openProbe) ConversationOpen(_ context.Context, _, _, key string, _ time.Duration) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, key)
	return true, nil
}

// TestConnectorsConversations proves the root rule, the follow-up gate,
// the bot reply rule, the built-in vars, and the recap (plan A.7 to A.11).
func TestConnectorsConversations(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()
	probe := &openProbe{}
	svc.WithConversations(probe)

	r1 := &protocol.Room{ID: "r1", Name: "ops"}
	msg := func(id, sender, text, replyTo string, bot bool) protocol.Event {
		return protocol.Event{
			ID: id, Type: "message", TS: "2026-10-01T10:00:00Z", Room: r1,
			Sender: &protocol.Sender{ID: sender, Name: sender, Bot: bot}, Text: text, ReplyTo: replyTo,
		}
	}
	startConvChat(t, ws, svc, []protocol.Event{
		msg("e1", "ann", "please deploy", "", false),
		msg("e2", "bob", "thanks", "e1", false),
		msg("e3", "ci", "done", "e1", true),
		msg("e4", "ann", "hello", "", false),
		msg("e5", "ann", "and staging?", "e2", false),
		// The ack reaction of the account itself never dispatches.
		{ID: "e6", Type: "reaction", TS: "2026-10-01T10:00:01Z", Room: r1, Text: "deploy",
			Sender: &protocol.Sender{ID: "me", Self: true}},
	}, true)
	waitUntil(t, func() bool {
		var n int
		_ = st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes`).Scan(&n) // retried until the deadline
		return n == 5
	}, "not every event has an outcome")
	waitUntil(t, func() bool {
		var n int
		_ = st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE connector='chat'`).Scan(&n) // retried until the deadline
		return n == 6
	}, "the self reaction is not stored")

	roots := map[string]string{}
	rows, err := st.DB().Query(`SELECT source_id, root FROM events WHERE connector='chat'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, root string
		if err := rows.Scan(&id, &root); err != nil {
			t.Fatal(err)
		}
		roots[id] = root
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"e1": "e1", "e2": "e1", "e3": "e1", "e4": "e4", "e5": "e1", "e6": "e6"}; !reflect.DeepEqual(roots, want) {
		t.Fatalf("roots = %v, want %v", roots, want)
	}

	outcomes := map[string]string{}
	rows, err = st.DB().Query(`SELECT e.source_id, o.outcome FROM event_outcomes o JOIN events e ON e.id = o.event_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, outcome string
		if err := rows.Scan(&id, &outcome); err != nil {
			t.Fatal(err)
		}
		outcomes[id] = outcome
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// e2 and e5 skip contains as follow-ups; the bot reply e3 and the
	// unrelated e4 do not run.
	if want := map[string]string{"e1": "queued", "e2": "queued", "e3": "no_match", "e4": "no_match", "e5": "queued"}; !reflect.DeepEqual(outcomes, want) {
		t.Fatalf("outcomes = %v, want %v", outcomes, want)
	}

	type item struct {
		key      string
		followUp bool
		vars     map[string]string
	}
	var items []item
	rows, err = st.DB().Query(`SELECT conversation_key, follow_up, vars_json FROM event_queue ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var it item
		var raw string
		if err := rows.Scan(&it.key, &it.followUp, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &it.vars); err != nil {
			t.Fatal(err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].followUp || !items[1].followUp || !items[2].followUp {
		t.Fatalf("items = %+v", items)
	}
	for _, it := range items {
		if it.key != "r1/e1" {
			t.Fatalf("key = %q", it.key)
		}
	}
	if v := items[1].vars; v["text"] != "thanks" || v["sender"] != "bob" || v["room_name"] != "ops" || v["reply_to"] != "e1" || v["connection"] != "chat" {
		t.Fatalf("vars = %v", v)
	}

	var instance string
	if err := st.DB().QueryRow(`SELECT instance FROM events WHERE source_id='e1'`).Scan(&instance); err != nil {
		t.Fatal(err)
	}
	recap, err := svc.Recap(ctx, instance, "e1", 3)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, l := range recap {
		texts = append(texts, l.Sender+": "+l.Text)
	}
	if want := []string{"bob: thanks", "ci: done", "ann: and staging?"}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("recap = %v, want %v", texts, want)
	}
	storedKey(t, st, svc, "e1", "r1/e1")
}

// TestConnectorsConversationsThreadless proves the lane rule of a channel
// without threads: a group is one lane with no follow-ups, and a DM keeps
// its follow-ups (plan A.8).
func TestConnectorsConversationsThreadless(t *testing.T) {
	st, ws, svc := openStack(t)
	probe := &openProbe{}
	svc.WithConversations(probe)

	g := &protocol.Room{ID: "g", Name: "family", Kind: protocol.RoomGroup}
	d := &protocol.Room{ID: "d", Name: "ann", Kind: protocol.RoomDM}
	msg := func(id string, room *protocol.Room, text, replyTo string) protocol.Event {
		return protocol.Event{
			ID: id, Type: "message", TS: "2026-10-01T10:00:00Z", Room: room,
			Sender: &protocol.Sender{ID: "ann", Name: "ann"}, Text: text, ReplyTo: replyTo,
		}
	}
	startConvChat(t, ws, svc, []protocol.Event{
		msg("g1", g, "please deploy", ""),
		msg("g2", g, "thanks", "g1"),
		msg("g3", g, "deploy staging", ""),
		msg("d1", d, "deploy", ""),
		msg("d2", d, "and logs?", ""),
	}, false)
	waitUntil(t, func() bool {
		var n int
		_ = st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes`).Scan(&n) // retried until the deadline
		return n == 5
	}, "not every event has an outcome")

	type result struct {
		outcome  string
		key      string
		followUp bool
	}
	got := map[string]result{}
	rows, err := st.DB().Query(`SELECT e.source_id, o.outcome, COALESCE(q.conversation_key, ''), COALESCE(q.follow_up, 0)
		FROM event_outcomes o JOIN events e ON e.id = o.event_id LEFT JOIN event_queue q ON q.event_id = o.event_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var r result
		if err := rows.Scan(&id, &r.outcome, &r.key, &r.followUp); err != nil {
			t.Fatal(err)
		}
		got[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// g2 quotes g1 but is no follow-up, so contains stops it.
	want := map[string]result{
		"g1": {"queued", "g", false},
		"g2": {"no_match", "", false},
		"g3": {"queued", "g", false},
		"d1": {"queued", "d", true},
		"d2": {"queued", "d", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
	var root string
	if err := st.DB().QueryRow(`SELECT root FROM events WHERE source_id='g2'`).Scan(&root); err != nil || root != "g1" {
		t.Fatalf("g2 root = %q, %v; want g1", root, err)
	}
	probe.mu.Lock()
	for _, k := range probe.keys {
		if k == "g" || strings.HasPrefix(k, "g/") {
			t.Fatalf("group lookup %q; a threadless group has no follow-ups", k)
		}
	}
	probe.mu.Unlock()
	storedKey(t, st, svc, "g1", "g")
}

// TestConnectorsKeyLogin drives a daemon key method: the methods list, a
// key that the test command refuses, then a good key.
func TestConnectorsKeyLogin(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	e := inline("apollo", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"sh"}}}
		m.Setup = &manifest.Setup{
			Inputs: []manifest.Input{{Name: "APOLLO_KEY", Label: "API key", Secret: true, URL: "https://example.com/keys"}},
			Login:  []manifest.LoginMethod{{ID: "key", Type: manifest.LoginKey, Input: "APOLLO_KEY", Test: `test "$APOLLO_KEY" = good || { echo "bad key" >&2; exit 1; }`}},
		}
	})
	if _, err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	loginState := func() string {
		row, err := svc.Get("apollo")
		if err != nil {
			t.Fatal(err)
		}
		return row["login"].(map[string]any)["state"].(string)
	}
	if got := loginState(); got != protocol.StateAuthRequired {
		t.Fatalf("login state with no key = %q", got)
	}
	methods, err := svc.LoginMethods(ctx, "apollo")
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 || methods[0].Owner != connectors.OwnerDaemon || !methods[0].Available || methods[0].Label != "Paste a key" {
		t.Fatalf("methods = %+v", methods)
	}
	if _, err := svc.Login(ctx, "apollo", "nope"); protocol.CodeOf(err) != protocol.CodeInvalid {
		t.Fatalf("unknown method: %v", err)
	}

	f, err := svc.Login(ctx, "apollo", "")
	if err != nil {
		t.Fatal(err)
	}
	step := f.Current().Step
	if step == nil || step.Kind != protocol.StepInput || len(step.Fields) != 1 || step.Fields[0].Name != "APOLLO_KEY" || !step.Fields[0].Secret || step.Fields[0].URL == "" {
		t.Fatalf("first step = %+v", step)
	}
	if err := svc.SubmitLogin(ctx, f, map[string]protocol.Value{}); protocol.CodeOf(err) != protocol.CodeInvalid {
		t.Fatalf("empty key: %v", err)
	}
	if err := svc.SubmitLogin(ctx, f, map[string]protocol.Value{"APOLLO_KEY": {"wrong"}}); err != nil {
		t.Fatal(err)
	}
	if d := f.Current().Done; d == nil || d.OK || !strings.HasSuffix(d.Error, " did not accept the key: bad key") {
		t.Fatalf("bad key done = %+v", d)
	}
	if got := loginState(); got != protocol.StateAuthRequired {
		t.Fatalf("login state after a bad key = %q", got)
	}

	f, err = svc.Login(ctx, "apollo", "key")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SubmitLogin(ctx, f, map[string]protocol.Value{"APOLLO_KEY": {"good"}}); err != nil {
		t.Fatal(err)
	}
	if d := f.Current().Done; d == nil || !d.OK {
		t.Fatalf("good key done = %+v", d)
	}
	if got := loginState(); got != protocol.StateConnected {
		t.Fatalf("login state after a good key = %q", got)
	}
	r, err := svc.Resolve("apollo", "")
	if err != nil {
		t.Fatal(err)
	}
	if env, _ := r.Env(); env["APOLLO_KEY"] != "good" {
		t.Fatal("the key is not saved")
	}
}

// startConvChat starts the channel connection "chat" with events and the
// job deploy (contains: deploy, replies: continue). threads sets the
// threads feature in the manifest and the live session.
func startConvChat(t *testing.T, ws *workspaces.Service, svc *connectors.Service, events []protocol.Event, threads bool) {
	t.Helper()
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	jobDir := filepath.Join(writeAgent(t, wsPath, "ops"), "deploy")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte("---\nharness: fake\n---\n\nRun.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	routingtest.Write(t, wsPath, "chat", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Job: "ops/deploy", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Contains: []string{"deploy"}, Replies: "continue"}},
	}})
	w, err := ws.Add(ctx, wsPath, "conv-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	var features []string
	if threads {
		features = []string{manifest.FeatureThreads}
	}
	sb := &fakeadapter.Sandbox{
		Methods:  []protocol.LoginMethod{{ID: "qr", Type: protocol.LoginPhone, Available: true}},
		Events:   events,
		LoggedIn: true,
		Result:   protocol.InitializeResult{Features: features},
	}
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := sb.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	e := inline("chat", func(m *manifest.Manifest) {
		m.Events = &manifest.Events{Protocol: manifest.ProtocolConnector, Command: `"` + os.Args[0] + `"`, Types: []manifest.EventType{{Type: "message"}, {Type: "reaction"}}}
		m.Channel = &manifest.Channel{Features: features}
	})
	e.Inputs = map[string]string{fakeadapter.Env: "sandbox:" + path}
	if _, err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop("chat") })
}

// storedKey checks the replies: continue key of a stored event, with the
// live session and then with the manifest alone.
func storedKey(t *testing.T, st *store.Store, svc *connectors.Service, sourceID, want string) {
	t.Helper()
	var id string
	if err := st.DB().QueryRow(`SELECT id FROM events WHERE source_id=?`, sourceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"live", "stopped"} {
		if phase == "stopped" {
			if err := svc.Stop("chat"); err != nil {
				t.Fatal(err)
			}
		}
		ev, err := svc.StoredGateEvent(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got := ev.ConversationKey(true, nil); got != want {
			t.Fatalf("%s: stored key of %s = %q, want %q", phase, sourceID, got, want)
		}
	}
}

func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	waittest.Until(t, msg, cond)
}

// writeAgent writes agents/<name>/AGENT.md with no triggers and returns its folder.
func writeAgent(t *testing.T, wsPath, name string) string {
	t.Helper()
	dir := filepath.Join(wsPath, "agents", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("---\nharness: fake\n---\n\nRun.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// routeAgent writes agent name and a routes file of the connection with one
// route to it.
func routeAgent(t *testing.T, wsPath, name, connection string, filter job.TriggerYAML) {
	t.Helper()
	writeAgent(t, wsPath, name)
	routingtest.Write(t, wsPath, connection, routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Agent: name, Gets: routing.GetsEveryMessage, Filter: filter},
	}})
}

func TestGetForManagedMatchesList(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	userHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(userHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	decl := `{"mcpServers":{"tool":{"command":"mytool","args":["--x"]}}}`
	if err := os.WriteFile(filepath.Join(userHome, ".claude", "mcp.json"), []byte(decl), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.WithUserHome(userHome)
	for _, name := range []string{"plain", "withmcp"} {
		def := inline(name, func(m *manifest.Manifest) {
			m.Events = &manifest.Events{Command: "rss-fetch x", Format: "json", ID: "guid"}
			if name == "withmcp" {
				m.Actions = &manifest.Actions{MCP: &manifest.MCPAction{Command: "mytool", Args: []string{"--x"}}}
			}
		})
		if _, err := svc.Create(ctx, def); err != nil {
			t.Fatal(err)
		}
	}
	rows := map[string]map[string]any{}
	for _, row := range svc.List("") {
		rows[row["slug"].(string)] = row
	}
	for _, name := range []string{"plain", "withmcp"} {
		got, err := svc.GetFor(name, "")
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(rows[name])
		if string(a) != string(b) {
			t.Errorf("%s: GetFor differs from List\n got %s\nwant %s", name, a, b)
		}
	}
	if _, merged := rows["withmcp"]["origins"]; !merged {
		t.Fatal("the inherited server did not merge into the row of the connection")
	}
}
