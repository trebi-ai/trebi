package connectors_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"gopkg.in/yaml.v3"
)

func TestDefinitionValidate(t *testing.T) {
	validEvents := &connectors.Events{Command: "listen", Format: "json", ID: "id"}
	cases := []struct {
		name   string
		def    connectors.Definition
		fields []string // expected failing fields; empty = valid
	}{
		{"minimal line", connectors.Definition{Name: "a", Events: &connectors.Events{Command: "echo"}}, nil},
		{"no blocks", connectors.Definition{Name: "a"}, []string{""}},
		{"bad name", connectors.Definition{Name: "A_b", Events: &connectors.Events{Command: "x"}}, []string{"name"}},
		{"reserved email", connectors.Definition{Name: "email", Events: &connectors.Events{Command: "x"}}, []string{"name"}},
		{"reserved webhook", connectors.Definition{Name: "webhook", Events: &connectors.Events{Command: "x"}}, []string{"name"}},
		{"reserved trebi", connectors.Definition{Name: "trebi", Actions: &connectors.Actions{MCP: &connectors.MCPAction{Command: "x"}}}, []string{"name"}},
		{"actions empty", connectors.Definition{Name: "a", Actions: &connectors.Actions{}}, []string{"actions"}},
		{"mcp both", connectors.Definition{Name: "a", Actions: &connectors.Actions{MCP: &connectors.MCPAction{Command: "npx", URL: "http://x"}}}, []string{"actions.mcp"}},
		{"mcp neither", connectors.Definition{Name: "a", Actions: &connectors.Actions{MCP: &connectors.MCPAction{}}}, []string{"actions.mcp"}},
		{"mcp headers need url", connectors.Definition{Name: "a", Actions: &connectors.Actions{MCP: &connectors.MCPAction{Command: "npx", Headers: map[string]string{"A": "b"}}}}, []string{"actions.mcp.headers"}},
		{"mcp args need command", connectors.Definition{Name: "a", Actions: &connectors.Actions{MCP: &connectors.MCPAction{URL: "http://x", Args: []string{"y"}}}}, []string{"actions.mcp.args"}},
		{"mcp valid", connectors.Definition{Name: "a", Actions: &connectors.Actions{MCP: &connectors.MCPAction{Command: "npx", Args: []string{"-y", "srv"}}}}, nil},
		{"cli no commands", connectors.Definition{Name: "a", Actions: &connectors.Actions{CLI: &connectors.CLIAction{}}}, []string{"actions.cli.commands"}},
		{"cli valid", connectors.Definition{Name: "a", Actions: &connectors.Actions{CLI: &connectors.CLIAction{Commands: []string{"psql"}, Skill: "postgres"}}}, nil},
		{"events no command", connectors.Definition{Name: "a", Events: &connectors.Events{}}, []string{"events.command"}},
		{"events bad format", connectors.Definition{Name: "a", Events: &connectors.Events{Command: "x", Format: "xml"}}, []string{"events.format"}},
		{"events bad restart", connectors.Definition{Name: "a", Events: &connectors.Events{Command: "x", Restart: "sometimes"}}, []string{"events.restart"}},
		{"poll too short", connectors.Definition{Name: "a", Events: &connectors.Events{Command: "x", Poll: connectors.Duration(5 * time.Second)}}, []string{"events.poll"}},
		{"path needs json", connectors.Definition{Name: "a", Events: &connectors.Events{Command: "x", ID: "id"}}, []string{"events.id"}},
		{"catchup needs json", connectors.Definition{Name: "a", Events: &connectors.Events{Command: "x", Catchup: &connectors.Catchup{Command: "back"}}}, []string{"events.catchup"}},
		{"catchup needs id", connectors.Definition{Name: "a", Events: &connectors.Events{Command: "x", Format: "json", Catchup: &connectors.Catchup{Command: "back"}}}, []string{"events.catchup"}},
		{"catchup beats poll", connectors.Definition{Name: "a", Events: &connectors.Events{Command: "x", Format: "json", ID: "id", Poll: connectors.Duration(15 * time.Second), Catchup: &connectors.Catchup{Command: "back"}}}, []string{"events.catchup"}},
		{"catchup no command", connectors.Definition{Name: "a", Events: &connectors.Events{Command: "x", Format: "json", ID: "id", Catchup: &connectors.Catchup{}}}, []string{"events.catchup.command"}},
		{"bad ts path", connectors.Definition{Name: "a", Events: &connectors.Events{Command: "x", Format: "json", TS: "a..b"}}, []string{"events.ts"}},
		{"channel needs json", connectors.Definition{Name: "a", Events: &connectors.Events{Command: "x"}, Channel: &connectors.Channel{Ref: "r", Send: "s"}}, []string{"channel"}},
		{"channel needs ref", connectors.Definition{Name: "a", Events: validEvents, Channel: &connectors.Channel{Send: "s"}}, []string{"channel.ref"}},
		{"channel needs send", connectors.Definition{Name: "a", Events: validEvents, Channel: &connectors.Channel{Ref: "r"}}, []string{"channel.send"}},
		{"typing interval short", connectors.Definition{Name: "a", Events: validEvents, Channel: &connectors.Channel{Ref: "r", Send: "s", TypingInterval: connectors.Duration(500 * time.Millisecond)}}, []string{"channel.typing_interval"}},
		{"env bad key", connectors.Definition{Name: "a", Env: map[string]string{"../x": "v"}, Events: &connectors.Events{Command: "x"}}, []string{"env"}},
		{"mcp env bad key", connectors.Definition{Name: "a", Actions: &connectors.Actions{MCP: &connectors.MCPAction{Command: "npx", Env: map[string]string{"A-B": "v"}}}}, []string{"actions.mcp.env"}},
		{"channel valid", connectors.Definition{Name: "a", Events: validEvents, Channel: &connectors.Channel{Ref: "r", Send: "s", Typing: "t", Ack: "a"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.def.Validate()
			if len(tc.fields) == 0 {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			var ve *connectors.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want ValidationError, got %v", err)
			}
			if !errors.Is(err, connectors.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			got := map[string]bool{}
			for _, f := range ve.Fields {
				got[f.Field] = true
			}
			for _, want := range tc.fields {
				if !got[want] {
					t.Fatalf("missing field %q in %v", want, ve.Fields)
				}
			}
		})
	}
}

func TestDefinitionCapabilitiesOrder(t *testing.T) {
	d := connectors.Definition{
		Name: "all",
		Actions: &connectors.Actions{
			CLI: &connectors.CLIAction{Commands: []string{"x"}},
		},
		Events:  &connectors.Events{Command: "x"},
		Channel: &connectors.Channel{Ref: "r", Send: "s"},
	}
	got := d.Capabilities()
	want := []string{"actions", "events", "channel"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("capabilities %v want %v", got, want)
	}
	empty := connectors.Definition{Name: "none"}
	if len(empty.Capabilities()) != 0 {
		t.Fatalf("capabilities %v", empty.Capabilities())
	}
}

// TestParseDefinitionStrict proves the old keys fail with the key name.
func TestParseDefinitionStrict(t *testing.T) {
	cases := []struct {
		name string
		body string
		key  string
	}{
		{"type", "mail:\n  type: stream\n  events: {command: x}\n", "type"},
		{"facets", "mail:\n  facets: [email]\n  events: {command: x}\n", "facets"},
		{"command", "mail:\n  command: x\n  events: {command: x}\n", "command"},
		{"format", "mail:\n  format: json\n  events: {command: x}\n", "format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, teardown := stackWithFile(t, tc.body)
			defer teardown()
			err := svc.Load()
			if err == nil {
				t.Fatal("want strict load failure")
			}
			if !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), "mail") {
				t.Fatalf("error %q must name the connector and the key %q", err, tc.key)
			}
		})
	}
}

func TestDefinitionDurationJSON(t *testing.T) {
	type holder struct {
		Poll connectors.Duration `json:"poll_ms,omitempty"`
	}
	var h holder
	if err := json.Unmarshal([]byte(`{"poll_ms":1500}`), &h); err != nil {
		t.Fatal(err)
	}
	if h.Poll.Std() != 1500*time.Millisecond {
		t.Fatalf("poll %v", h.Poll.Std())
	}
	b, err := json.Marshal(holder{Poll: connectors.Duration(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"poll_ms":120000}` {
		t.Fatalf("json %s", b)
	}
	// A zero duration with omitempty must vanish.
	b, _ = json.Marshal(holder{})
	if string(b) != `{}` {
		t.Fatalf("empty json %s", b)
	}
	// YAML text form.
	var def struct {
		Poll connectors.Duration `yaml:"poll"`
	}
	if err := yaml.Unmarshal([]byte("poll: 15m"), &def); err != nil {
		t.Fatal(err)
	}
	if def.Poll.Std() != 15*time.Minute {
		t.Fatalf("yaml poll %v", def.Poll.Std())
	}
}
