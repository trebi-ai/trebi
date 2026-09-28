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
	def, inputs, err := entry.Manifest(InstallOpts{Name: "adtest", Package: 0, Remote: -1, Env: map[string]string{
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
	if mcp.Env["ADTEST_API_KEY"] != "${ADTEST_API_KEY}" || mcp.Env["ADTEST_API_BASE"] != "${ADTEST_API_BASE}" {
		t.Fatalf("env = %v", mcp.Env)
	}
	if inputs["ADTEST_API_KEY"] != "key-1" || inputs["ADTEST_API_BASE"] != "https://api.example" {
		t.Fatalf("inputs = %v", inputs)
	}
	secret := map[string]bool{}
	for _, in := range def.Setup.Inputs {
		secret[in.Name] = in.Secret
	}
	if !secret["ADTEST_API_KEY"] || secret["ADTEST_API_BASE"] {
		t.Fatalf("setup inputs = %+v", def.Setup.Inputs)
	}
	if def.Version != "1.1.0" {
		t.Fatalf("version = %q", def.Version)
	}
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionPyPI(t *testing.T) {
	entry := entryFor(t, "medium-ops", "0.1.0")
	def, inputs, err := entry.Manifest(InstallOpts{Package: -1, Remote: -1})
	if err != nil {
		t.Fatal(err)
	}
	mcp := def.Actions.MCP
	if mcp.Command != "uvx" {
		t.Fatalf("command = %q", mcp.Command)
	}
	if want := []string{"--from", "medium-ops==0.1.0", "medium-ops", "mcp", "serve"}; !reflect.DeepEqual(mcp.Args, want) {
		t.Fatalf("args = %v", mcp.Args)
	}
	if def.Name != "medium-ops" {
		t.Fatalf("name = %q", def.Name)
	}
	if len(inputs) != 0 {
		t.Fatalf("inputs = %+v", inputs)
	}
}

func TestDefinitionOCI(t *testing.T) {
	entry := entryFor(t, "acme/docker", "1.0.0")
	def, inputs, err := entry.Manifest(InstallOpts{Name: "docker-mcp", Package: -1, Remote: -1, Env: map[string]string{"TOKEN": "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	mcp := def.Actions.MCP
	if mcp.Command != "docker" {
		t.Fatalf("command = %q", mcp.Command)
	}
	want := []string{"run", "-i", "--rm", "-e", "TOKEN", "acme/mcp:1.0.0"}
	if !reflect.DeepEqual(mcp.Args, want) {
		t.Fatalf("args = %v", mcp.Args)
	}
	if mcp.Env["TOKEN"] != "${TOKEN}" || inputs["TOKEN"] != "tok" {
		t.Fatalf("env = %v inputs = %v", mcp.Env, inputs)
	}
}

func TestDefinitionRemote(t *testing.T) {
	entry := entryFor(t, "acme/remote", "2.0.0")
	def, inputs, err := entry.Manifest(InstallOpts{Package: -1, Remote: -1})
	if err != nil {
		t.Fatal(err)
	}
	mcp := def.Actions.MCP
	if mcp.URL != "https://mcp.acme.example/mcp" {
		t.Fatalf("url = %q", mcp.URL)
	}
	if mcp.Headers["Authorization"] != "${AUTHORIZATION}" || inputs["AUTHORIZATION"] != "Bearer static-token" {
		t.Fatalf("headers = %v inputs = %v", mcp.Headers, inputs)
	}
	if mcp.Command != "" {
		t.Fatalf("mcp = %+v", mcp)
	}
}

func TestDefinitionMissingRequiredEnv(t *testing.T) {
	entry := entryFor(t, "ai.adtest/adtest-mcp", "1.1.0")
	_, _, err := entry.Manifest(InstallOpts{Name: "adtest", Package: 0, Remote: -1})
	if !errors.Is(err, ErrMissingInput) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "ADTEST_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefinitionUnsupportedPackage(t *testing.T) {
	entry := entryFor(t, "acme/cargo", "1.0.0")
	_, _, err := entry.Manifest(InstallOpts{Package: -1, Remote: -1})
	if !errors.Is(err, ErrUnsupportedPackage) {
		t.Fatalf("err = %v", err)
	}
}

func TestDefinitionPackageBeforeRemote(t *testing.T) {
	entry := entryFor(t, "acme/contributed", "1.0.0")
	def, _, err := entry.Manifest(InstallOpts{Package: -1, Remote: -1})
	if err != nil {
		t.Fatal(err)
	}
	if def.Actions.MCP == nil || def.Actions.MCP.Command != "npx" {
		t.Fatalf("mcp = %+v", def.Actions.MCP)
	}
	// The ai.trebi/connector _meta block is not read any more.
	if def.Events != nil || def.Channel != nil || def.Actions.CLI != nil {
		t.Fatalf("manifest = %+v", def)
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

// TestDefinitionKeepsSecretReference proves a given secret: value goes into
// the inputs as it is (P1-32).
func TestDefinitionKeepsSecretReference(t *testing.T) {
	entry := entryFor(t, "ai.adtest/adtest-mcp", "1.1.0")
	_, inputs, err := entry.Manifest(InstallOpts{Name: "adtest", Package: 0, Remote: -1, Env: map[string]string{
		"ADTEST_API_KEY": "secret:shared_key",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := inputs["ADTEST_API_KEY"]; got != "secret:shared_key" {
		t.Fatalf("inputs = %v", inputs)
	}
}

// templatedRemote is a remote with a URL variable and a secret header input.
func templatedRemote(url string) Entry {
	return Entry{Registry: "official", Server: ServerJSON{
		Name: "acme/tenant", Version: "1.0.0",
		Remotes: []Remote{{
			Type: "streamable-http", URL: url,
			Variables: map[string]Input{"tenant": {IsRequired: true}},
			Headers: []KeyValue{
				{Name: "Authorization", Input: Input{Value: "Bearer {api_key}", Variables: map[string]Input{
					"api_key": {IsRequired: true, IsSecret: true},
				}}},
				{Name: "X-Region", Input: Input{Default: "eu"}},
			},
		}},
	}}
}

func TestDefinitionRemoteInputs(t *testing.T) {
	entry := templatedRemote("https://mcp.acme.example/{tenant}")
	def, inputs, err := entry.Manifest(InstallOpts{Name: "acme", Package: -1, Remote: 0, Env: map[string]string{
		"tenant": "t1", "api_key": "k-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	m := def.Actions.MCP
	if m.URL != "https://mcp.acme.example/t1" {
		t.Fatalf("url = %q", m.URL)
	}
	if m.Headers["Authorization"] != "${AUTHORIZATION}" || m.Headers["X-Region"] != "${X_REGION}" {
		t.Fatalf("headers = %v", m.Headers)
	}
	if inputs["AUTHORIZATION"] != "Bearer k-1" || inputs["X_REGION"] != "eu" {
		t.Fatalf("inputs = %v", inputs)
	}
	for _, in := range def.Setup.Inputs {
		if in.Name == "AUTHORIZATION" && !in.Secret {
			t.Fatalf("setup inputs = %+v", def.Setup.Inputs)
		}
	}

	// The full header value can come as a secret reference.
	_, inputs, err = entry.Manifest(InstallOpts{Name: "acme", Package: -1, Remote: 0, Env: map[string]string{
		"tenant": "t1", "Authorization": "secret:gh_auth",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if inputs["AUTHORIZATION"] != "secret:gh_auth" {
		t.Fatalf("inputs = %v", inputs)
	}
}

func TestDefinitionRemoteInputErrors(t *testing.T) {
	cases := []struct {
		name string
		url  string
		env  map[string]string
		want error
	}{
		{"missing variable", "https://mcp.acme.example/{tenant}", map[string]string{"tenant": "t1"}, ErrMissingInput},
		{"unused key", "https://mcp.acme.example/{tenant}", map[string]string{"tenant": "t1", "api_key": "k", "API_KEY": "x"}, ErrUnusedInput},
		{"reference in template", "https://mcp.acme.example/{tenant}", map[string]string{"tenant": "t1", "api_key": "secret:x"}, ErrInvalidInput},
		{"plain http", "http://mcp.acme.example/{tenant}", map[string]string{"tenant": "t1", "api_key": "k"}, ErrUnsafeURL},
		{"loopback", "https://127.0.0.1/{tenant}", map[string]string{"tenant": "t1", "api_key": "k"}, ErrUnsafeURL},
		{"private", "https://10.1.2.3/{tenant}", map[string]string{"tenant": "t1", "api_key": "k"}, ErrUnsafeURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := templatedRemote(tc.url).Manifest(InstallOpts{Name: "acme", Package: -1, Remote: 0, Env: tc.env})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// dockerEntry follows the "Complex Docker Server" example of the registry
// server.json docs: the tag sits in the identifier, and runtimeArguments
// and a value hint take inputs.
func dockerEntry() Entry {
	return Entry{Registry: "official", Server: ServerJSON{
		Name: "io.github.example/database-manager", Version: "3.1.0",
		Packages: []Package{{
			RegistryType: "oci", Identifier: "docker.io/example/database-manager-mcp:3.1.0",
			RuntimeHint: "docker", Transport: &Transport{Type: "stdio"},
			RuntimeArguments: []Argument{
				{Type: "named", Name: "--network", Input: Input{Value: "host"}},
				{Type: "named", Name: "-e", Input: Input{Value: "DB_TYPE={db_type}", Variables: map[string]Input{
					"db_type": {IsRequired: true, Choices: []string{"postgres", "mysql"}},
				}}},
			},
			PackageArguments: []Argument{
				{Type: "named", Name: "--host", Input: Input{Default: "localhost", IsRequired: true}},
				{Type: "named", Name: "--port", Input: Input{Format: "number"}},
				{Type: "positional", ValueHint: "database_name", Input: Input{Description: "Database"}},
			},
			EnvironmentVariables: []KeyValue{
				{Name: "DB_PASSWORD", Input: Input{IsRequired: true, IsSecret: true}},
				{Name: "SSL_MODE", Input: Input{Default: "prefer"}},
			},
		}},
	}}
}

func TestDefinitionDockerArguments(t *testing.T) {
	def, inputs, err := dockerEntry().Manifest(InstallOpts{Name: "db", Package: 0, Remote: -1, Env: map[string]string{
		"db_type": "postgres", "database_name": "prod", "DB_PASSWORD": "pw", "EXTRA": "1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	m := def.Actions.MCP
	want := []string{
		"run", "-i", "--rm", "--network=host", "-e", "DB_TYPE=postgres",
		"-e", "DB_PASSWORD", "-e", "EXTRA", "-e", "SSL_MODE",
		"docker.io/example/database-manager-mcp:3.1.0", "--host=localhost", "prod",
	}
	if m.Command != "docker" || !reflect.DeepEqual(m.Args, want) {
		t.Fatalf("command = %q args = %v", m.Command, m.Args)
	}
	if m.Env["DB_PASSWORD"] != "${DB_PASSWORD}" || inputs["DB_PASSWORD"] != "pw" || inputs["SSL_MODE"] != "prefer" || inputs["EXTRA"] != "1" {
		t.Fatalf("env = %v inputs = %v", m.Env, inputs)
	}

	_, _, err = dockerEntry().Manifest(InstallOpts{Name: "db", Package: 0, Remote: -1, Env: map[string]string{
		"DB_PASSWORD": "pw",
	}})
	if !errors.Is(err, ErrMissingInput) || !strings.Contains(err.Error(), "database_name") || !strings.Contains(err.Error(), "db_type") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefinitionPackageMapping(t *testing.T) {
	base := func(p Package) Entry {
		p.Identifier, p.Version = "tool", "1.2.3"
		return Entry{Server: ServerJSON{Name: "acme/tool", Version: "1.2.3", Packages: []Package{p}}}
	}
	cases := []struct {
		name    string
		pkg     Package
		command string
		args    []string
		err     error
	}{
		{"npm runtime args", Package{RegistryType: "npm", RuntimeHint: "npx", RuntimeArguments: []Argument{{Type: "named", Name: "--node-options", Input: Input{Value: "x"}}}},
			"npx", []string{"-y", "--node-options=x", "tool@1.2.3"}, nil},
		{"npm other runtime", Package{RegistryType: "npm", RuntimeHint: "bunx"}, "bunx", []string{"tool@1.2.3"}, nil},
		{"pypi", Package{RegistryType: "pypi"}, "uvx", []string{"--from", "tool==1.2.3", "tool"}, nil},
		{"oci tag in version", Package{RegistryType: "oci"}, "docker", []string{"run", "-i", "--rm", "tool:1.2.3"}, nil},
		{"http transport", Package{RegistryType: "npm", Transport: &Transport{Type: "streamable-http"}}, "", nil, ErrUnsupportedPackage},
		{"runtime hint path", Package{RegistryType: "npm", RuntimeHint: "/bin/sh"}, "", nil, ErrInvalidEntry},
		{"secret argument", Package{RegistryType: "npm", PackageArguments: []Argument{{Type: "named", Name: "--token", Input: Input{Value: "t", IsSecret: true}}}}, "", nil, ErrInvalidEntry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def, _, err := base(tc.pkg).Manifest(InstallOpts{Name: "tool", Package: 0, Remote: -1})
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m := def.Actions.MCP; m.Command != tc.command || !reflect.DeepEqual(m.Args, tc.args) {
				t.Fatalf("command = %q args = %v", m.Command, m.Args)
			}
		})
	}
}

func TestEntryInputs(t *testing.T) {
	pkgs, _ := dockerEntry().Inputs()
	var names []string
	for _, in := range pkgs[0] {
		names = append(names, in.Kind+":"+in.Name)
	}
	want := []string{"variable:db_type", "argument:--host", "argument:--port", "argument:database_name", "env:DB_PASSWORD", "env:SSL_MODE"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("package inputs = %v", names)
	}
	_, remotes := templatedRemote("https://mcp.acme.example/{tenant}").Inputs()
	names = nil
	for _, in := range remotes[0] {
		names = append(names, in.Kind+":"+in.Name)
		if in.Name == "api_key" && (!in.Secret || !in.Required) {
			t.Fatalf("api_key = %+v", in)
		}
	}
	want = []string{"variable:tenant", "variable:api_key", "header:X-Region"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("remote inputs = %v", names)
	}
}
