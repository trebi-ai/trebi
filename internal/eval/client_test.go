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
