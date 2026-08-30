package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseClaudeStream(t *testing.T) {
	t.Parallel()
	raw := []byte(strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"I'll check."},{"type":"tool_use","name":"Bash","input":{"command":"trebi doctor --json"}}]}}`,
		`{"type":"result","usage":{"input_tokens":10,"output_tokens":4},"model":"claude"}`,
	}, "\n"))
	res := parseClaudeStream(raw)
	if len(res.Steps) != 2 {
		t.Fatalf("steps %+v", res.Steps)
	}
	if res.Steps[0].Role != "assistant" || res.Steps[1].Name != "Bash" {
		t.Fatalf("steps %+v", res.Steps)
	}
	if res.Usage.Input != 10 || res.CostUSD <= 0 {
		t.Fatalf("usage %+v cost %v", res.Usage, res.CostUSD)
	}
}

func TestParseCodexJSON(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"agent_message","text":"done"}
{"type":"usage","input_tokens":3,"output_tokens":1}`)
	res := parseCodexJSON(raw)
	if len(res.Steps) != 1 || res.Steps[0].Content != "done" {
		t.Fatalf("%+v", res.Steps)
	}
}

func TestParseCodexItemCompleted(t *testing.T) {
	t.Parallel()
	raw := []byte(strings.Join([]string{
		`{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"trebi doctor --json","aggregated_output":"","exit_code":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"trebi doctor --json","aggregated_output":"{\"ok\":true}","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"No. job not found."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"cache_write_input_tokens":2,"output_tokens":3}}`,
	}, "\n"))
	res := parseCodexJSON(raw)
	if len(res.Steps) != 2 {
		t.Fatalf("steps %d %+v", len(res.Steps), res.Steps)
	}
	if res.Steps[0].Role != "tool" || !strings.Contains(res.Steps[0].Content, "trebi doctor") || !strings.Contains(res.Steps[0].Content, "ok") {
		t.Fatalf("command step %+v", res.Steps[0])
	}
	if res.Steps[1].Role != "assistant" || res.Steps[1].Content != "No. job not found." {
		t.Fatalf("assistant %+v", res.Steps[1])
	}
	if res.Usage.Input != 10 || res.Usage.Output != 3 || res.Usage.CacheRead != 4 || res.Usage.CacheCreation != 2 {
		t.Fatalf("usage %+v", res.Usage)
	}
}

func TestMCPIsolationViolations(t *testing.T) {
	t.Parallel()
	steps := []Step{
		{Role: "tool", Name: "Bash", Content: `{"command":"trebi doctor --json"}`},
		{Role: "tool", Name: "Bash", Content: `{"command":"curl http://127.0.0.1:1234/v1/jobs"}`},
		{Role: "tool", Name: "mcp__other__list", Content: `{}`},
		{Role: "tool", Name: "job", Content: `{"action":"list"}`},
	}
	got := mcpIsolationViolations(steps, "http://127.0.0.1:1234")
	if len(got) != 3 {
		t.Fatalf("violations %v", got)
	}
	clean := mcpIsolationViolations([]Step{
		{Role: "tool", Name: "system", Content: `{"action":"guide","input":{"topic":"mcp-basics"}}`},
		{Role: "assistant", Content: "trebi is ready"},
	}, "http://127.0.0.1:1234")
	if len(clean) != 0 {
		t.Fatalf("clean %v", clean)
	}
}

func TestParseClientSpec(t *testing.T) {
	t.Parallel()
	cases := []struct {
		spec    string
		name    string
		model   string
		wantErr bool
		label   string
	}{
		{spec: "claude", name: "claude", model: "", label: "claude"},
		{spec: "codex", name: "codex", model: "", label: "codex"},
		{spec: "claude:claude-opus-5", name: "claude", model: "claude-opus-5", label: "claude:claude-opus-5"},
		{spec: "codex:gpt-5", name: "codex", model: "gpt-5", label: "codex:gpt-5"},
		{spec: "claude:sonnet", name: "claude", model: "sonnet", label: "claude:sonnet"},
		{spec: "unknown", wantErr: true},
		{spec: "gpt-5", wantErr: true},
	}
	for _, tt := range cases {
		t.Run(tt.spec, func(t *testing.T) {
			t.Parallel()
			name, model, err := parseClientSpec(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if _, err2 := newClient(tt.spec); err2 == nil {
					t.Fatal("newClient expected error")
				}
				return
			}
			if err != nil || name != tt.name || model != tt.model {
				t.Fatalf("got %s %s %v", name, model, err)
			}
			if clientLabel(name, model) != tt.label {
				t.Fatalf("label %s", clientLabel(name, model))
			}
			cl, err := newClient(tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			if cl.Name() != tt.label {
				t.Fatalf("Name %s want %s", cl.Name(), tt.label)
			}
		})
	}
}

func TestClientLaunchArgs(t *testing.T) {
	t.Parallel()
	pinned := claudeLaunchArgs("hi", 40, "claude-opus-5")
	found := false
	for i := 0; i < len(pinned)-1; i++ {
		if pinned[i] == "--model" && pinned[i+1] == "claude-opus-5" {
			found = true
		}
	}
	if !found {
		t.Fatalf("claude pinned: %v", pinned)
	}
	unpinned := claudeLaunchArgs("hi", 40, "")
	for _, a := range unpinned {
		if a == "--model" {
			t.Fatalf("claude unpinned has --model: %v", unpinned)
		}
	}
	cPinned := codexLaunchArgs("hi", "gpt-5")
	found = false
	for i := 0; i < len(cPinned)-1; i++ {
		if cPinned[i] == "-m" && cPinned[i+1] == "gpt-5" {
			found = true
		}
	}
	if !found {
		t.Fatalf("codex pinned: %v", cPinned)
	}
	if cPinned[len(cPinned)-1] != "hi" {
		t.Fatalf("codex prompt last: %v", cPinned)
	}
	cUnpinned := codexLaunchArgs("hi", "")
	for _, a := range cUnpinned {
		if a == "-m" {
			t.Fatalf("codex unpinned has -m: %v", cUnpinned)
		}
	}
}

func TestAgentsMarkdown(t *testing.T) {
	t.Parallel()
	got := string(AgentsMarkdown([]byte("# skill")))
	if !strings.Contains(got, "trebi") || !strings.Contains(got, "# skill") {
		t.Fatalf("%s", got)
	}
}

func TestOracleScriptExtract(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "o.md")
	body := "# Oracle\n\n```bash\necho hi\n```\n\n```bash\necho there\n```\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := oracleScript(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "echo hi") || !strings.Contains(got, "echo there") {
		t.Fatalf("%s", got)
	}
}
