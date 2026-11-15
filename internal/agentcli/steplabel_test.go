package agentcli

import (
	"bytes"
	"strings"
	"testing"
)

func TestStepLabel(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, tool, input, want string }{
		{"description wins", "Bash", `{"command":"ls","description":"  List  files "}`, "List files"},
		{"bash without description", "Bash", `{"command":"ls"}`, "Ran a command"},
		{"read", "Read", `{"file_path":"/a/b/run.go"}`, "Read run.go"},
		{"read without path", "Read", `{}`, "Read a file"},
		{"edit", "Edit", `{"file_path":"/a/b/run.go"}`, "Edited run.go"},
		{"write", "Write", `{"file_path":"/a/new.md"}`, "Wrote new.md"},
		{"codex patch", "apply_patch", `{}`, "Edited files"},
		{"grep", "Grep", `{"pattern":"x"}`, "Searched files"},
		{"web search", "WebSearch", `{"query":"x"}`, WebSearchLabel},
		{"web fetch", "WebFetch", `{"url":"https://www.example.org/docs/page/"}`, "Read example.org/docs/page"},
		{"web fetch bad url", "WebFetch", `{"url":"nope"}`, "Read a web page"},
		{"mcp server", "mcp__github__get_issue", `{}`, "Used github"},
		{"codex mcp", "mcp:get_issue", `{}`, "Used Get issue"},
		{"unknown tool", "ReadMcpResource", `{}`, "Read mcp resource"},
		{"quiet", "ToolSearch", `{"query":"x"}`, ""},
		{"quiet todo", "TodoWrite", `{}`, ""},
		{"bad json", "Read", `not json`, "Read a file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := StepLabel(tc.tool, tc.input); got != tc.want {
				t.Fatalf("StepLabel(%s) = %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}

func TestStepLabelCapsDescription(t *testing.T) {
	t.Parallel()
	got := StepLabel("Bash", `{"description":"`+strings.Repeat("a", 300)+`"}`)
	if n := len([]rune(got)); n != StepLabelMaxRunes+1 {
		t.Fatalf("label has %d runes", n)
	}
}

func TestClaudeTranscriptLabels(t *testing.T) {
	t.Parallel()
	turns, err := claudeTool{}.ParseTranscript(bytes.NewReader(readTranscript(t, "claude", "labels.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	var calls []ToolCall
	for _, turn := range turns {
		for _, tc := range turn.Tools {
			if tc.Name != "tool_result" {
				calls = append(calls, tc)
			}
		}
	}
	if len(calls) != 4 {
		t.Fatalf("calls: %+v", calls)
	}
	wantLabel := []string{"Read main.go", "Run the tests", "Used github", ""}
	wantKind := []string{"read", "exec", "mcp", ""}
	wantMS := []int64{2000, 8500, 0, 0}
	for i, tc := range calls {
		if tc.Label != wantLabel[i] || tc.ID == "" || !tc.AtIsStart || tc.DurationMS != wantMS[i] {
			t.Fatalf("call %d: %+v", i, tc)
		}
		if wantKind[i] != "" && tc.Kind != wantKind[i] {
			t.Fatalf("call %d kind %q want %q", i, tc.Kind, wantKind[i])
		}
	}
}

func TestCodexLabels(t *testing.T) {
	t.Parallel()
	codex := `{"timestamp":"2026-08-28T12:00:01.000Z","type":"response_item","payload":{"type":"function_call","name":"read_file","arguments":"{\"path\":\"/a/x.go\"}","call_id":"c1"}}`
	turns, err := parseCodexTranscript(strings.NewReader(codex + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range turns {
		for _, tc := range turn.Tools {
			if tc.Name == "read_file" && (tc.Label != "Read x.go" || !tc.AtIsStart) {
				t.Fatalf("codex call: %+v", tc)
			}
		}
	}
}

func TestOpenCodeLabels(t *testing.T) {
	t.Parallel()
	raw := `[{"info":{"role":"assistant","time":{"created":1000}},"parts":[{"type":"tool","tool":"edit","state":{"status":"completed","input":{"filePath":"/a/x.go"},"output":"ok"}}]}]`
	turns, err := ParseOpenCodeMessages([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || len(turns[0].Tools) != 1 {
		t.Fatalf("turns: %+v", turns)
	}
	if tc := turns[0].Tools[0]; tc.Label != "Edited x.go" || tc.Kind != "edit" {
		t.Fatalf("call: %+v", tc)
	}
}
