package agentcli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

// Fixtures are verbatim CLI output captured from the installed binaries.

const grokModelsOut = `You are not authenticated.

Default model: grok-4.6

Available models:
  * grok-4.6 (default)
  - grok-4.5
`

const cursorModelsOut = `Available models

auto - Auto (default)
gpt-5.3-codex-low - Codex 5.3 Low
composer-2.5 - Composer 2.5
claude-sonnet-5-thinking-high - Claude Sonnet 5 1M Thinking
`

const opencodeModelsOut = `opencode/big-pickle
opencode/nemotron-3-ultra-free
xai/grok-4.6
zai-coding-plan/glm-5.3
`

const piModelsOut = `provider  model           context  max-out  thinking  images
xai       grok-4.3        1M       30K      yes       yes   
xai       grok-4.5        500K     500K     yes       yes   
`

func TestParseModels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		out  string
		want []ModelInfo
	}{
		{
			name: Grok,
			out:  grokModelsOut,
			want: []ModelInfo{
				{ID: "grok-4.6", Default: true},
				{ID: "grok-4.5"},
			},
		},
		{
			name: Cursor,
			out:  cursorModelsOut,
			want: []ModelInfo{
				{ID: "auto", Label: "Auto", Default: true},
				{ID: "gpt-5.3-codex-low", Label: "Codex 5.3 Low"},
				{ID: "composer-2.5", Label: "Composer 2.5"},
				{ID: "claude-sonnet-5-thinking-high", Label: "Claude Sonnet 5 1M Thinking"},
			},
		},
		{
			name: OpenCode,
			out:  opencodeModelsOut,
			want: []ModelInfo{
				{ID: "opencode/big-pickle"},
				{ID: "opencode/nemotron-3-ultra-free"},
				{ID: "xai/grok-4.6"},
				{ID: "zai-coding-plan/glm-5.3"},
			},
		},
		{
			name: Pi,
			out:  piModelsOut,
			want: []ModelInfo{
				{ID: "xai/grok-4.3"},
				{ID: "xai/grok-4.5"},
			},
		},
	}
	for _, tc := range cases {
		got := parseModels(tc.name, tc.out)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
		for i := range got {
			if !reflect.DeepEqual(got[i], tc.want[i]) {
				t.Errorf("%s[%d]: got %+v want %+v", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

func TestParseModelsSkipsNoise(t *testing.T) {
	t.Parallel()
	if got := parseModels(OpenCode, "Available providers: none\n\nno models\n"); len(got) != 0 {
		t.Fatalf("opencode noise: %+v", got)
	}
	if got := parseModels(Grok, "You are not authenticated.\n"); len(got) != 0 {
		t.Fatalf("grok noise: %+v", got)
	}
	if got := parseModels(Cursor, "Tip - read the docs\n"); len(got) != 0 {
		t.Fatalf("cursor noise: %+v", got)
	}
}

func TestParseOpencodeConfigModel(t *testing.T) {
	t.Parallel()
	out := `{
  "mcp": { "sling": { "type": "local" } },
  "$schema": "https://opencode.ai/config.json",
  "model": "xai/grok-4.6"
}`
	if got := parseOpencodeConfigModel(out); got != "xai/grok-4.6" {
		t.Fatalf("got %q", got)
	}
	// A warning before the JSON must not defeat the parse.
	if got := parseOpencodeConfigModel("WARN something\n" + out); got != "xai/grok-4.6" {
		t.Fatalf("noise: got %q", got)
	}
	if got := parseOpencodeConfigModel("no config here"); got != "" {
		t.Fatalf("garbage: got %q", got)
	}
	if got := parseOpencodeConfigModel(`{"model": null}`); got != "" {
		t.Fatalf("null model: got %q", got)
	}
	if got := parseOpencodeConfigModel(opencode2DebugConfigOut); got != "opencode-go/deepseek-v4.1-flash" {
		t.Fatalf("2.x documents: got %q", got)
	}
}

// opencode2DebugConfigOut is `opencode debug config` of OpenCode 2.1.4: one
// entry per config document, with the model as {providerID, model}.
const opencode2DebugConfigOut = `[
  {
    "type": "document",
    "path": "/home/u/.config/opencode/opencode.json",
    "info": {
      "$schema": "https://opencode.ai/config.json",
      "model": { "providerID": "xai", "model": "grok-4.7" }
    }
  },
  {
    "type": "document",
    "path": "/home/u/project/opencode.json",
    "info": { "model": { "providerID": "opencode-go", "model": "deepseek-v4.1-flash" } }
  },
  { "type": "document", "path": "/home/u/.trebi/opencode.json", "info": {} }
]`

func TestStderrReason(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"\nERROR\n  Unrecognized flag: --verbose in command opencode models\n": "Unrecognized flag: --verbose in command opencode models",
		"error: not logged in\nmore":   "error: not logged in",
		"\x1b[31mERROR\x1b[0m\n\nboom": "boom",
		"ERROR\n":                      "",
		"":                             "",
	}
	for in, want := range cases {
		if got := stderrReason(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

// TestListModelsOpencode2 runs a stub of OpenCode 2.x, which rejects
// `models --verbose`.
func TestListModelsOpencode2(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	bin := t.TempDir()
	stub := `#!/bin/sh
case "$*" in
"models --verbose") printf '\nERROR\n  Unrecognized flag: --verbose in command opencode models\n' >&2; exit 1 ;;
"models") [ -n "$STUB_BROKEN" ] && { printf 'ERROR\n  No provider is logged in\n' >&2; exit 1; }; printf 'xai/grok-4.7\nzai-coding-plan/glm-5.3\n' ;;
"debug config") cat <<'JSON'
` + opencode2DebugConfigOut + `
JSON
;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	got := ListModels(context.Background(), OpenCode)
	if got.Source != ModelSourceCLI || got.Error != "" {
		t.Fatalf("list: %+v", got)
	}
	ids := []string{}
	for _, m := range got.Models {
		ids = append(ids, m.ID)
	}
	if !reflect.DeepEqual(ids, []string{"xai/grok-4.7", "zai-coding-plan/glm-5.3"}) {
		t.Fatalf("ids: %v", ids)
	}
	if got.Default != "opencode-go/deepseek-v4.1-flash" {
		t.Fatalf("default: %q", got.Default)
	}

	t.Setenv("STUB_BROKEN", "1")
	got = ListModels(context.Background(), OpenCode)
	if got.Error != "exit status 1: No provider is logged in" {
		t.Fatalf("error: %q", got.Error)
	}
}

func TestHarnessConfigDefault(t *testing.T) {
	base := t.TempDir()
	home.SetBase(base)
	t.Cleanup(func() { home.SetBase("") })

	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("visor/harness/claude/settings.json", `{"model": "opus[1m]"}`)
	write("visor/harness/pi/settings.json", `{"defaultProvider": "xai", "defaultModel": "grok-4.5"}`)
	write("visor/harness/grok/config.toml", "[models]\ndefault = \"grok-composer-2.5-fast\"\n")
	write("visor/harness/opencode/opencode.json", `{"model": "xai/grok-4.6"}`)
	write("visor/harness/cursor/cli-config.json", `{"model": {"modelId": "grok-4.6"}}`)

	codexDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(codexDir, "config.toml"),
		[]byte("model_provider = \"ZAI\"\nmodel = \"glm-5.3-Flash\"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexDir)

	cases := map[string]string{
		"claude":   "opus[1m]",
		"codex":    "glm-5.3-Flash",
		"grok":     "grok-composer-2.5-fast",
		"opencode": "xai/grok-4.6",
		"cursor":   "grok-4.6",
		"pi":       "xai/grok-4.5",
		"copilot":  "",
		"gemini":   "",
	}
	for name, want := range cases {
		if got := harnessConfigDefault(name); got != want {
			t.Errorf("%s: got %q want %q", name, got, want)
		}
	}
}

func TestListModelsWithoutListCommand(t *testing.T) {
	t.Parallel()
	for _, name := range []string{Gemini, Fake} {
		got := ListModels(context.Background(), name)
		if got.Source != ModelSourceNone {
			t.Errorf("%s: source=%q, want none", name, got.Source)
		}
		if got.Error != "" {
			t.Errorf("%s: unexpected error %q", name, got.Error)
		}
		if got.Models == nil {
			t.Errorf("%s: models must encode as [], not null", name)
		}
	}
}

func TestListModelsStatic(t *testing.T) {
	base := t.TempDir()
	home.SetBase(base)
	t.Cleanup(func() { home.SetBase("") })
	if err := os.MkdirAll(filepath.Join(base, "visor/harness/claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "visor/harness/claude/settings.json"), []byte(`{"model": "opus"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	codexDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte("model = \"glm-5.3-Flash\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexDir)

	cases := []struct{ name, def, first string }{
		{Claude, "opus", "sonnet"},
		{Codex, "glm-5.3-Flash", "glm-5.3-Flash"}, // not in the list: it comes first
		{Copilot, "", "claude-sonnet-4.6"},
	}
	for _, tc := range cases {
		got := ListModels(context.Background(), tc.name)
		if got.Source != ModelSourceStatic || len(got.Models) < 2 || got.Error != "" {
			t.Fatalf("%s: %+v", tc.name, got)
		}
		if got.Default != tc.def || got.Models[0].ID != tc.first {
			t.Errorf("%s: default=%q first=%q", tc.name, got.Default, got.Models[0].ID)
		}
		marked := 0
		for _, m := range got.Models {
			if m.Default {
				marked++
				if m.ID != tc.def {
					t.Errorf("%s: marked %q", tc.name, m.ID)
				}
			}
		}
		if (tc.def != "") != (marked == 1) {
			t.Errorf("%s: %d marked", tc.name, marked)
		}
	}
	list := staticModels(Claude)
	list[0].ID = "changed"
	if staticModels(Claude)[0].ID != "sonnet" {
		t.Fatal("staticModels shares the package list")
	}
}

func TestParseOpencodeVerbose(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "models", "opencode_verbose.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := parseOpencodeModels(string(raw))
	want := []ModelInfo{
		{ID: "opencode/big-pickle", Label: "Big Pickle", Group: opencodeAccount([]string{"opencode"}), Free: true},
		{ID: "openai/gpt-5.5", Label: "GPT-5.5", Group: opencodeAccount([]string{"openai"}), Variants: []string{"low", "medium", "high"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if want[1].Group != "OpenAI" {
		t.Fatalf("group label %q", want[1].Group)
	}
}
