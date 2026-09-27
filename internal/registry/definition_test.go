package registry

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
)

// entryFor loads one fixture entry through the real client so the tests read
// the same shapes the registry serves.
func entryFor(t *testing.T, name, version string) Entry {
	t.Helper()
	srv := fixtureServer(t, nil)
	svc := New(Config{Registries: []config.Registry{{Name: "official", URL: srv.URL}}})
	entry, err := svc.Get(context.Background(), "official", name, version)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestDefinitionNPM(t *testing.T) {
	entry := entryFor(t, "ai.adtest/adtest-mcp", "1.1.0")
	def, secrets, err := entry.Definition(InstallOpts{Name: "adtest", Package: 0, Remote: -1, Env: map[string]string{
		"ADTEST_API_KEY":         "key-1",
		"ADTEST_API_BASE":        "https://api.example",
		"ADTEST_POLL_TIMEOUT_MS": "1000",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != "adtest" {
		t.Fatalf("name = %q", def.Name)
	}
	mcp := def.Actions.MCP
	if mcp == nil || mcp.Command != "npx" {
		t.Fatalf("mcp = %+v", mcp)
	}
	if want := []string{"-y", "adtest-mcp@1.1.0"}; !reflect.DeepEqual(mcp.Args, want) {
		t.Fatalf("args = %v", mcp.Args)
	}
	if mcp.Env["ADTEST_API_KEY"] != "secret:adtest_ADTEST_API_KEY" {
		t.Fatalf("env = %v", mcp.Env)
	}
	if mcp.Env["ADTEST_API_BASE"] != "https://api.example" {
		t.Fatalf("env = %v", mcp.Env)
	}
	if len(secrets) != 1 || secrets[0].Name != "adtest_ADTEST_API_KEY" || secrets[0].Value != "key-1" {
		t.Fatalf("secrets = %+v", secrets)
	}
	if def.Source == nil || def.Source.Registry != "official" || def.Source.Name != "ai.adtest/adtest-mcp" || def.Source.Version != "1.1.0" {
		t.Fatalf("source = %+v", def.Source)
	}
}

func TestDefinitionPyPI(t *testing.T) {
	entry := entryFor(t, "medium-ops", "0.1.0")
	def, secrets, err := entry.Definition(InstallOpts{Package: -1, Remote: -1})
	if err != nil {
		t.Fatal(err)
	}
	mcp := def.Actions.MCP
	if mcp.Command != "uvx" {
		t.Fatalf("command = %q", mcp.Command)
	}
	if want := []string{"medium-ops==0.1.0", "mcp", "serve"}; !reflect.DeepEqual(mcp.Args, want) {
		t.Fatalf("args = %v", mcp.Args)
	}
	if def.Name != "medium-ops" {
		t.Fatalf("name = %q", def.Name)
	}
	if len(secrets) != 0 {
		t.Fatalf("secrets = %+v", secrets)
	}
}

func TestDefinitionOCI(t *testing.T) {
	entry := entryFor(t, "acme/docker", "1.0.0")
	def, secrets, err := entry.Definition(InstallOpts{Name: "docker-mcp", Package: -1, Remote: -1, Env: map[string]string{"TOKEN": "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	mcp := def.Actions.MCP
	if mcp.Command != "docker" {
		t.Fatalf("command = %q", mcp.Command)
	}
	want := []string{"run", "-i", "--rm", "-e", "TOKEN", "-e", "DEBUG", "acme/mcp:1.0.0"}
	if !reflect.DeepEqual(mcp.Args, want) {
		t.Fatalf("args = %v", mcp.Args)
	}
	if mcp.Env["TOKEN"] != "secret:docker-mcp_TOKEN" {
		t.Fatalf("env = %v", mcp.Env)
	}
	if len(secrets) != 1 || secrets[0].Value != "tok" {
		t.Fatalf("secrets = %+v", secrets)
	}
}

func TestDefinitionRemote(t *testing.T) {
	entry := entryFor(t, "acme/remote", "2.0.0")
	def, secrets, err := entry.Definition(InstallOpts{Package: -1, Remote: -1})
	if err != nil {
		t.Fatal(err)
	}
	mcp := def.Actions.MCP
	if mcp.URL != "https://mcp.acme.example/mcp" {
		t.Fatalf("url = %q", mcp.URL)
	}
	if mcp.Headers["Authorization"] != "Bearer static-token" {
		t.Fatalf("headers = %v", mcp.Headers)
	}
	if mcp.Command != "" || len(secrets) != 0 {
		t.Fatalf("mcp = %+v secrets = %+v", mcp, secrets)
	}
}

func TestDefinitionMissingRequiredEnv(t *testing.T) {
	entry := entryFor(t, "ai.adtest/adtest-mcp", "1.1.0")
	_, _, err := entry.Definition(InstallOpts{Name: "adtest", Package: 0, Remote: -1})
	if !errors.Is(err, ErrMissingEnv) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "ADTEST_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefinitionUnsupportedPackage(t *testing.T) {
	entry := entryFor(t, "acme/cargo", "1.0.0")
	_, _, err := entry.Definition(InstallOpts{Package: -1, Remote: -1})
	if !errors.Is(err, ErrUnsupportedPackage) {
		t.Fatalf("err = %v", err)
	}
}

func TestDefinitionPackageBeforeRemote(t *testing.T) {
	entry := entryFor(t, "acme/contributed", "1.0.0")
	def, _, err := entry.Definition(InstallOpts{Package: -1, Remote: -1})
	if err != nil {
		t.Fatal(err)
	}
	if def.Actions.MCP == nil || def.Actions.MCP.Command != "npx" {
		t.Fatalf("mcp = %+v", def.Actions.MCP)
	}
	if def.Events == nil || def.Events.Command != "contributed listen" || def.Events.Format != "json" {
		t.Fatalf("events = %+v", def.Events)
	}
	if def.Channel == nil || def.Channel.Send != "contributed send" {
		t.Fatalf("channel = %+v", def.Channel)
	}
	// The _meta CLI action installs, and the contributed skill names itself.
	if def.Actions.CLI == nil || len(def.Actions.CLI.Commands) != 1 || def.Actions.CLI.Commands[0] != "contributed-cli" {
		t.Fatalf("cli = %+v", def.Actions.CLI)
	}
	if def.Actions.CLI.Skill != "contributed" {
		t.Fatalf("cli skill = %q", def.Actions.CLI.Skill)
	}
}

func TestSkillReturned(t *testing.T) {
	entry := entryFor(t, "acme/contributed", "1.0.0")
	sk, ok := entry.Skill()
	if !ok {
		t.Fatal("expected skill block")
	}
	if sk.Name != "contributed" || sk.URL != "https://example.com/contributed-skill.tar.gz" {
		t.Fatalf("skill = %+v", sk)
	}
}

func TestSkillAbsent(t *testing.T) {
	entry := entryFor(t, "ai.adtest/adtest-mcp", "1.1.0")
	if _, ok := entry.Skill(); ok {
		t.Fatal("adtest has no skill block")
	}
}

func TestConnectorNameDerived(t *testing.T) {
	cases := map[string]string{
		"ai.adtest/adtest-mcp":      "adtest-mcp",
		"medium-ops":                "medium-ops",
		"io.github.me/weird_name.x": "weird-name-x",
	}
	for in, want := range cases {
		if got := (InstallOpts{}).connectorName(in); got != want {
			t.Fatalf("connectorName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := (InstallOpts{Name: "custom"}).connectorName("ai.adtest/adtest-mcp"); got != "custom" {
		t.Fatalf("explicit name = %q", got)
	}
}
