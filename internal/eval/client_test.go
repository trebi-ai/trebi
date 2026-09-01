package eval

import (
	"encoding/json"
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

func TestMCPIsolationCommandPosition(t *testing.T) {
	t.Parallel()
	api := "http://127.0.0.1:1234"
	incident, err := json.Marshal(map[string]any{
		"command":   `bash -lc "printf '%s\n' 'READY: Trebi health is ok (daemon reachable, API token present). Codex harness auth_status=ok.' > health.txt"`,
		"exit_code": 0,
		"output":    "READY: Trebi health is ok\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cmd  string
		out  string
		want int
	}{
		{name: "incident prose", cmd: "", want: 0},
		{name: "trebi service", cmd: "trebi service status", want: 1},
		{name: "abs path", cmd: "/usr/local/bin/trebi doctor", want: 1},
		{name: "env assign", cmd: "FOO=1 trebi x", want: 1},
		{name: "bash -lc", cmd: `bash -lc "trebi x"`, want: 1},
		{name: "echo prose", cmd: "echo 'run trebi later'", want: 0},
		{name: "printf Trebi", cmd: "printf 'Trebi health ok' > f", want: 0},
		{name: "curl in output", cmd: "echo hello", out: "curl http://127.0.0.1:1234/v1/jobs", want: 0},
		{name: "curl in command", cmd: "curl http://127.0.0.1:1234/v1/jobs", want: 1},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var step Step
			if tt.name == "incident prose" {
				step = Step{Role: "tool", Name: "command", Content: string(incident)}
			} else {
				body := map[string]any{"command": tt.cmd}
				if tt.out != "" {
					body["output"] = tt.out
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				step = Step{Role: "tool", Name: "command", Content: string(raw)}
			}
			got := mcpIsolationViolations([]Step{step}, api)
			if len(got) != tt.want {
				t.Fatalf("want %d got %v", tt.want, got)
			}
		})
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

func TestParseCodexErrors(t *testing.T) {
	t.Parallel()
	raw := []byte(strings.Join([]string{
		`{"type":"error","message":"Reconnecting... 2/5 (unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: wss://api.openai.com/v1/responses, cf-ray: abc)"}`,
		`{"type":"error","message":"Reconnecting... 3/5 (unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: wss://api.openai.com/v1/responses, cf-ray: def)"}`,
		`{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: https://api.openai.com/v1/responses, request id: req_1"}}`,
	}, "\n"))
	res := parseCodexJSON(raw)
	if hasAssistantStep(res.Steps) {
		t.Fatalf("steps %+v", res.Steps)
	}
	sum := clientErrorSummary(res)
	if !strings.Contains(sum, "401 Unauthorized") {
		t.Fatalf("errors %v summary %q", res.Errors, sum)
	}
}

func TestParseCodexMCPToolCall(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"trebi","tool":"system","arguments":{"action":"health"},"status":"completed"}}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"DONE"}}`)
	res := parseCodexJSON(raw)
	if !mcpHandshakeOK(res.Steps) {
		t.Fatalf("steps %+v", res.Steps)
	}
	if !hasAssistantStep(res.Steps) {
		t.Fatal("missing assistant")
	}
}

// TestParseCodexMCPResult pins that a tool response lands on the step. Without
// it the judge sees the request only, and a rubric rule about what trebi
// answered can never be graded.
func TestParseCodexMCPResult(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"item.started","item":{"id":"item_1","type":"mcp_tool_call","server":"trebi","tool":"harness","arguments":{"action":"list"},"status":"in_progress","result":null}}
{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"trebi","tool":"harness","arguments":{"action":"list"},"status":"completed","result":{"content":[{"type":"text","text":"{\"harnesses\":[{\"name\":\"codex\",\"auth_ok\":true}]}"}]}}}`)
	res := parseCodexJSON(raw)
	if len(res.Steps) != 1 {
		t.Fatalf("steps %+v", res.Steps)
	}
	if got := res.Steps[0].Content; got != `{"action":"list"}` {
		t.Errorf("content = %s", got)
	}
	if !strings.Contains(res.Steps[0].Result, `"auth_ok":true`) {
		t.Errorf("result = %q", res.Steps[0].Result)
	}
}

// TestParseCodexMCPError pins that a failed call is visible as an error.
func TestParseCodexMCPError(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"item.completed","item":{"id":"i1","type":"mcp_tool_call","server":"trebi","tool":"file","arguments":{"action":"put"},"status":"completed","error":"conflict: skill is not local"}}`)
	res := parseCodexJSON(raw)
	if len(res.Steps) != 1 || !strings.Contains(res.Steps[0].Result, "conflict") {
		t.Fatalf("steps %+v", res.Steps)
	}
}

// TestParseCodexFileChange pins that a write made with the client's own file
// tool is recorded. The codex stream reports it as its own item type, not a
// tool call, so it was invisible: a task finished that way looked unfinished.
func TestParseCodexFileChange(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"item.started","item":{"id":"f1","type":"file_change","changes":[{"path":"/w/health.txt","kind":"add"}],"status":"in_progress"}}
{"type":"item.completed","item":{"id":"f1","type":"file_change","changes":[{"path":"/w/health.txt","kind":"add"}],"status":"completed"}}`)
	res := parseCodexJSON(raw)
	if len(res.Steps) != 1 {
		t.Fatalf("steps %+v", res.Steps)
	}
	s := res.Steps[0]
	if s.Name != "file_change" || !strings.Contains(s.Content, "/w/health.txt") {
		t.Fatalf("step %+v", s)
	}
	if s.Result != "completed" {
		t.Errorf("result = %q", s.Result)
	}
}

// TestFileChangeNotACommand pins that a file_change step does not project into
// the commands_jq view, which matches trebi CLI and MCP calls only.
func TestFileChangeNotACommand(t *testing.T) {
	t.Parallel()
	steps := []Step{{Role: "tool", Name: "file_change", Content: `{"paths":["/w/a.txt"],"kinds":["add"]}`}}
	data, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	cmds, err := transcriptCommands(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 0 {
		t.Fatalf("commands = %v, want none", cmds)
	}
}

// TestParseClaudeToolResult pins that a tool_result attaches to the tool_use
// that asked for it, instead of becoming anonymous user prose.
func TestParseClaudeToolResult(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"trebi harness list"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"codex auth_ok=true"}]}]}}`)
	res := parseClaudeStream(raw)
	if len(res.Steps) != 1 {
		t.Fatalf("steps %+v", res.Steps)
	}
	if res.Steps[0].Role != "tool" || res.Steps[0].Result != "codex auth_ok=true" {
		t.Fatalf("step %+v", res.Steps[0])
	}
}

// TestParseClaudeUserProseKept pins that real user text still records as a
// user step; only tool-result messages are folded into their tool step.
func TestParseClaudeUserProseKept(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"user","message":{"content":[{"type":"text","text":"go on"}]}}`)
	res := parseClaudeStream(raw)
	if len(res.Steps) != 1 || res.Steps[0].Role != "user" {
		t.Fatalf("steps %+v", res.Steps)
	}
}

func TestParseClaudeResultError(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"result","is_error":true,"subtype":"error","result":"Authentication error"}`)
	res := parseClaudeStream(raw)
	if len(res.Errors) != 1 || res.Errors[0] != "Authentication error" {
		t.Fatalf("%+v", res.Errors)
	}
}

func TestWriteTranscriptEmptySteps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.json")
	if err := writeTranscript(path, []byte("raw"), nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "[]" {
		t.Fatalf("got %q", got)
	}
}

func TestAgentsMarkdown(t *testing.T) {
	t.Parallel()
	got := string(AgentsMarkdown([]byte("# skill"), surfaceCLI))
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
