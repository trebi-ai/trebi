package native

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/llm"
)

// Transcript is a JSONL sidecar for native runs.
type Transcript struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

// OpenTranscript creates ~/.trebi/runs/<id>/transcript.jsonl.
func OpenTranscript(runID string) (*Transcript, error) {
	dir := filepath.Join(home.RunsDir(), runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "transcript.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Transcript{path: path, f: f}, nil
}

// Path is the JSONL file.
func (t *Transcript) Path() string {
	if t == nil {
		return ""
	}
	return t.path
}

// Close the file.
func (t *Transcript) Close() error {
	if t == nil || t.f == nil {
		return nil
	}
	return t.f.Close()
}

func (t *Transcript) write(kind string, body map[string]any) {
	if t == nil || t.f == nil {
		return
	}
	body["type"] = kind
	body["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(body)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = t.f.Write(append(b, '\n'))
}

// Assistant records a model text/tool turn.
func (t *Transcript) Assistant(resp *llm.Response) {
	if resp == nil {
		return
	}
	t.write("assistant", map[string]any{
		"text": resp.Text, "stop": resp.Stop, "tool_calls": resp.ToolCalls,
	})
}

// ToolCall records a pre-execution call.
func (t *Transcript) ToolCall(c llm.ToolCall) {
	t.write("tool", map[string]any{"id": c.ID, "name": c.Name, "args": redactArgs(c.Arguments)})
}

// ToolResult records a tool return.
func (t *Transcript) ToolResult(id, name, text string) {
	t.write("tool_result", map[string]any{"id": id, "name": name, "text": trunc(text, 4000)})
}

// Turn is one parsed transcript row for GET /transcript.
type Turn struct {
	Type string `json:"type"`
	Role string `json:"role,omitempty"`
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"`
	At   string `json:"at,omitempty"`
}

// ReadTurns parses a native JSONL transcript into drawer-shaped turns.
func ReadTurns(path string) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Turn
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var raw map[string]any
		if json.Unmarshal(sc.Bytes(), &raw) != nil {
			continue
		}
		kind, _ := raw["type"].(string)
		at, _ := raw["at"].(string)
		switch kind {
		case "assistant":
			text, _ := raw["text"].(string)
			if text != "" {
				out = append(out, Turn{Type: "message", Role: "assistant", Text: text, At: at})
			}
		case "tool":
			name, _ := raw["name"].(string)
			out = append(out, Turn{Type: "tool", Name: name, At: at})
		case "tool_result":
			name, _ := raw["name"].(string)
			text, _ := raw["text"].(string)
			out = append(out, Turn{Type: "tool_result", Name: name, Text: text, At: at})
		}
	}
	return out, sc.Err()
}

// TranscriptPath is ~/.trebi/runs/<id>/transcript.jsonl.
func TranscriptPath(runID string) string {
	return filepath.Join(home.RunsDir(), runID, "transcript.jsonl")
}
