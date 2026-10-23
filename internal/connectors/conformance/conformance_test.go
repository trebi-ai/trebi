package conformance_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors/conformance"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/oauth/oauthtest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/connectors/protocol/fakeadapter"
)

func TestMain(m *testing.M) {
	if fakeadapter.Enabled() {
		os.Exit(fakeadapter.Main())
	}
	os.Exit(m.Run())
}

func TestFolderReference(t *testing.T) {
	t.Parallel()
	res := conformance.NewRunner(nil).Run(context.Background(), sandboxTarget(t, reference()), []string{conformance.SuiteFolder})
	rows := statuses(res, conformance.SuiteFolder)
	if len(rows) != 8 {
		t.Fatalf("rows = %+v", res)
	}
	for check, r := range rows {
		if r.Status != conformance.StatusPass {
			t.Errorf("%s = %+v", check, r)
		}
	}
}

func TestFolderFaults(t *testing.T) {
	t.Parallel()
	tests := []struct{ fault, check string }{
		{fakeadapter.FaultWriteHome, "writes stay inside"},
		{fakeadapter.FaultAbsoluteState, "relative paths"},
		{fakeadapter.FaultExitEmpty, "empty state"},
		{"", "restart keeps state"}, // not a reference: no state files
	}
	for _, tt := range tests {
		t.Run(tt.check, func(t *testing.T) {
			t.Parallel()
			sb := reference()
			sb.Reference = tt.fault != ""
			if tt.fault != "" {
				sb.Faults = []string{tt.fault}
			}
			res := conformance.NewRunner(nil).Run(context.Background(), sandboxTarget(t, sb), []string{conformance.SuiteFolder})
			if r := statuses(res, conformance.SuiteFolder)[tt.check]; r.Status != conformance.StatusFail {
				t.Fatalf("%s = %+v; all = %+v", tt.check, r, res)
			}
		})
	}
}

func TestProtocolSandbox(t *testing.T) {
	t.Parallel()
	res := conformance.NewRunner(nil).Run(context.Background(), sandboxTarget(t, reference()), []string{conformance.SuiteProtocol})
	rows := statuses(res, conformance.SuiteProtocol)
	for _, check := range []string{"initialize", "status", "sandbox login", "send once", "events from send", "clean stdout", "shutdown"} {
		if r := rows[check]; r.Status != conformance.StatusPass {
			t.Errorf("%s = %+v", check, r)
		}
	}

	sb := reference()
	sb.Faults = []string{fakeadapter.FaultStdoutNoise}
	res = conformance.NewRunner(nil).Run(context.Background(), sandboxTarget(t, sb), []string{conformance.SuiteProtocol})
	if r := statuses(res, conformance.SuiteProtocol)["clean stdout"]; r.Status != conformance.StatusFail {
		t.Fatalf("clean stdout = %+v", r)
	}
}

// TestProtocolSubscriptions proves the subscriptions and webhooks steps on
// an api and a manual sandbox, and a fail on a sandbox that accepts a
// changed body.
func TestProtocolSubscriptions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		target string
		faults []string
		hooks  string
	}{
		{"api", "r1", nil, conformance.StatusPass},
		{"manual", fakeadapter.ManualTarget, nil, conformance.StatusPass},
		{"forged", "r1", []string{fakeadapter.FaultAcceptForged}, conformance.StatusFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb := reference()
			sb.Faults = tc.faults
			sb.Result.Features = []string{"rooms.list", manifest.FeatureSubscriptions, manifest.FeatureWebhooks}
			sb.Options = map[string][]protocol.FieldOption{fakeadapter.FieldTarget: {{Value: tc.target}}}
			target := sandboxTarget(t, sb)
			target.Manifest.Channel.Features = []string{"rooms.list"}
			target.Manifest.Events.Subscriptions = &manifest.Subscriptions{
				Action:  "Watch a target",
				Modes:   []string{manifest.ModeAPI, manifest.ModeManual},
				Fields:  []manifest.SubscriptionField{{Name: fakeadapter.FieldTarget, Label: "Target", Dynamic: true, Room: true}},
				Webhook: &manifest.Webhook{Headers: []string{fakeadapter.SignatureHeader}},
			}
			rows := statuses(conformance.NewRunner(nil).Run(context.Background(), target, []string{conformance.SuiteProtocol}), conformance.SuiteProtocol)
			if r := rows["subscriptions"]; r.Status != conformance.StatusPass {
				t.Fatalf("subscriptions = %+v", r)
			}
			if r := rows["webhooks"]; r.Status != tc.hooks {
				t.Fatalf("webhooks = %+v", r)
			}
		})
	}
}

func TestInputsMissing(t *testing.T) {
	t.Parallel()
	sb := reference()
	sb.Required = []string{"FAKE_TOKEN"}
	tgt := sandboxTarget(t, sb)
	tgt.Manifest.Setup.Inputs = []manifest.Input{{Name: "FAKE_TOKEN", Label: "Token", Required: true, Secret: true}}
	res := conformance.NewRunner(nil).Run(context.Background(), tgt, []string{conformance.SuiteInputs})
	rows := statuses(res, conformance.SuiteInputs)
	if rows["labels"].Status != conformance.StatusPass || rows["missing input"].Status != conformance.StatusPass {
		t.Fatalf("rows = %+v", res)
	}
}

// TestCLIOnlyManifest runs a CLI-only catalog entry like apollo: manifest,
// install (warn: no block), inputs, and actions.cli with <dir>/bin on PATH.
func TestCLIOnlyManifest(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "apollo")
	data, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	res := conformance.NewRunner(nil).Run(context.Background(), conformance.Target{Manifest: m, Dir: dir}, nil)
	want := map[string]string{
		"manifest/validate":           conformance.StatusPass,
		"manifest/skill":              conformance.StatusPass,
		"install/method":              conformance.StatusWarn,
		"inputs/labels":               conformance.StatusPass,
		"inputs/missing input":        conformance.StatusSkip,
		"actions.cli/path apollo-cli": conformance.StatusPass,
		"actions.cli/help apollo-cli": conformance.StatusPass,
	}
	got := map[string]conformance.Result{}
	for _, r := range res {
		got[r.Suite+"/"+r.Check] = r
		if strings.HasPrefix(r.Suite, "protocol") || r.Suite == conformance.SuiteFolder || r.Suite == conformance.SuiteActionsMCP {
			t.Errorf("unexpected row %+v", r)
		}
	}
	for k, st := range want {
		if got[k].Status != st {
			t.Errorf("%s = %+v, want %s", k, got[k], st)
		}
	}
}

// TestOnlineOAuthManifest: an online server with an OAuth login passes on
// its login discovery, and a Trebi entry gets no install warning.
func TestOnlineOAuthManifest(t *testing.T) {
	t.Parallel()
	srv := oauthtest.New(t, oauthtest.Options{})
	m, err := manifest.Parse([]byte(`schema: trebi-connector/1
name: online
title: Online
description: An online MCP server.
version: 1.0.0
publisher: trebi
setup:
  login: [{id: oauth, type: browser, label: Log in, oauth: {registration: auto}}]
actions:
  mcp: {url: "` + srv.MCPURL() + `"}
`))
	if err != nil {
		t.Fatal(err)
	}
	res := conformance.NewRunner(nil).Run(context.Background(), conformance.Target{Manifest: m, Dir: t.TempDir()}, nil)
	got := map[string]conformance.Result{}
	for _, r := range res {
		got[r.Suite+"/"+r.Check] = r
		if r.Suite == conformance.SuiteInstall {
			t.Errorf("unexpected row %+v", r)
		}
	}
	if r := got["actions.mcp/login"]; r.Status != conformance.StatusPass || !strings.Contains(r.Message, srv.URL) {
		t.Errorf("login = %+v", r)
	}
	if r := got["actions.mcp/tools/list"]; r.Status != conformance.StatusSkip {
		t.Errorf("tools/list = %+v", r)
	}
}

// TestBuildRows checks the build method row and the go.mod row of a folder
// with install.build.
func TestBuildRows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := manifest.Manifest{
		Schema: manifest.SchemaID, Name: "b", Title: "B", Description: "d", Version: "0.1.0",
		Install: &manifest.Install{Build: &manifest.Build{Go: "."}, Bin: "b"},
		Actions: &manifest.Actions{CLI: &manifest.CLIAction{Commands: []string{"b"}}},
	}
	rows := func() map[string]conformance.Result {
		got := map[string]conformance.Result{}
		for _, r := range conformance.NewRunner(nil).Run(context.Background(), conformance.Target{Manifest: m, Dir: dir}, []string{conformance.SuiteManifest, conformance.SuiteInstall}) {
			got[r.Suite+"/"+r.Check] = r
		}
		return got
	}
	if r := rows()["manifest/build module"]; r.Status != conformance.StatusFail {
		t.Fatalf("no go.mod: %+v", r)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := rows()
	if r := got["manifest/build module"]; r.Status != conformance.StatusPass {
		t.Fatalf("go.mod: %+v", r)
	}
	if r := got["install/method"]; r.Status != conformance.StatusPass {
		t.Fatalf("method: %+v", r)
	}
}

func TestUnknownSuite(t *testing.T) {
	t.Parallel()
	res := conformance.NewRunner(nil).Run(context.Background(), conformance.Target{Manifest: manifest.Manifest{}}, []string{"nope", "actions"})
	if len(res) < 1 || res[0].Suite != "nope" || res[0].Status != conformance.StatusFail {
		t.Fatalf("res = %+v", res)
	}
	for _, r := range res[1:] {
		if r.Status != conformance.StatusSkip || !strings.HasPrefix(r.Suite, "actions.") {
			t.Errorf("row = %+v", r)
		}
	}
}

// reference is a sandbox that keeps its login and its sent keys in the
// state folder.
func reference() *fakeadapter.Sandbox {
	return &fakeadapter.Sandbox{
		Result: protocol.InitializeResult{
			Protocol: protocol.Version,
			Features: []string{"rooms.list"},
			Events:   []protocol.EventDecl{{Type: "message"}},
		},
		Methods:   []protocol.LoginMethod{{ID: "qr", Type: protocol.LoginPhone, Available: true}},
		Rooms:     []protocol.Room{{ID: "r1", Name: "general"}},
		Reference: true,
	}
}

// sandboxTarget writes sb and returns a target that runs this test binary
// as the adapter. The clean env drops TREBI_FAKEADAPTER, so the command
// line sets it.
func sandboxTarget(t *testing.T, sb *fakeadapter.Sandbox) conformance.Target {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := sb.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	return conformance.Target{
		Manifest: manifest.Manifest{
			Schema: manifest.SchemaID, Name: "fake", Title: "Fake", Description: "Fake adapter", Version: "0.1.0",
			Setup:   &manifest.Setup{Login: []manifest.LoginMethod{{ID: "qr", Type: manifest.LoginPhone}}},
			Events:  &manifest.Events{Protocol: manifest.ProtocolConnector, Command: "fake", Types: []manifest.EventType{{Type: "message"}}},
			Channel: &manifest.Channel{Features: sb.Result.Features},
		},
		Command: fakeadapter.Env + "=sandbox:" + path + " '" + os.Args[0] + "'",
	}
}

func statuses(res []conformance.Result, suite string) map[string]conformance.Result {
	out := map[string]conformance.Result{}
	for _, r := range res {
		if r.Suite == suite {
			out[r.Check] = r
		}
	}
	return out
}
