package native

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/llm"
	"github.com/trebi-ai/trebi/internal/tools"
)

// Session is one incremental native conversation (plan 2026-09-26 D.11).
// Prompt runs one tool-loop turn and reports each step through RunOpts.Sink
// and the streaming hooks. The transcript, compaction, and tool jail are the
// loop's own.
type Session struct {
	opts RunOpts
	sys  string

	mu       sync.Mutex
	msgs     []llm.Message
	ingested []string
	usage    llm.Usage
	turn     int
}

// NewSession builds a session over one provider. RunID names the transcript
// and message log directory (for example "conv-<id>"); empty disables both.
func NewSession(opts RunOpts) *Session {
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = DefaultMaxTurns
	}
	if opts.BashTimeout <= 0 {
		opts.BashTimeout = defaultBashTO
	}
	if opts.WorkingDir == "" {
		opts.WorkingDir = opts.WorkspaceRoot
	}
	sys := strings.TrimSpace(opts.System)
	if sys == "" {
		sys = defaultSystem + "\n\n" + tools.Instructions
	}
	return &Session{opts: opts, sys: sys}
}

// Prompt appends one user turn and runs the tool loop until the model stops
// calling tools. Context cancel is kill.
func (s *Session) Prompt(ctx context.Context, text string) error {
	s.mu.Lock()
	s.turn++
	s.msgs = append(s.msgs, llm.Message{Role: "user", Content: text})
	s.persistLocked()
	turn := s.turn
	opts := s.opts
	s.mu.Unlock()

	if opts.Sink.SetAgent != nil {
		opts.Sink.SetAgent(ctx, "running")
	}
	turnOpts := opts
	turnOpts.Transcript = opts.Transcript
	err := s.loopTurn(ctx, turnOpts, turn)
	status := "complete"
	if err != nil {
		status = "failed"
	}
	if opts.Sink.SetAgent != nil {
		opts.Sink.SetAgent(ctx, "idle")
	}
	if opts.Sink.Finish != nil {
		_ = opts.Sink.Finish(ctx, status, "session", s.Usage())
	}
	return err
}

// loopTurn is one Prompt's tool loop over the shared message list.
func (s *Session) loopTurn(ctx context.Context, opts RunOpts, turn int) error {
	for i := 0; i < opts.MaxTurns; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		if err := maybeCompact(ctx, opts, &s.msgs); err != nil {
			s.mu.Unlock()
			return err
		}
		msgs := append([]llm.Message(nil), s.msgs...)
		s.mu.Unlock()

		resp, err := opts.Provider.Chat(ctx, llm.Request{
			Model: opts.Model, System: s.sys, Messages: msgs,
			Tools: toolSpecs(opts.Registry), MaxTokens: opts.MaxTokens, Headers: opts.Headers,
		})
		if err != nil {
			return err
		}
		s.addUsage(resp.Usage)
		if opts.Sink.Usage != nil {
			opts.Sink.Usage(resp.Usage)
		}
		if opts.Transcript != nil {
			opts.Transcript.Assistant(resp)
		}
		if err := enforceDaily(ctx, opts, resp.Usage); err != nil {
			return err
		}
		if resp.Stop == llm.StopUnknown {
			return fmt.Errorf("unknown stop reason %q", resp.RawStop)
		}
		if resp.Stop == llm.StopRefusal || resp.Stop == llm.StopContentFilter {
			return fmt.Errorf("%s: %s", resp.Stop, resp.Text)
		}
		if resp.Text != "" && opts.Sink.AssistantText != nil {
			opts.Sink.AssistantText(resp.Text)
		}
		if len(resp.ToolCalls) > 0 {
			s.mu.Lock()
			s.msgs = appendAssistant(s.msgs, resp)
			s.persistLocked()
			s.mu.Unlock()
			for _, c := range resp.ToolCalls {
				if opts.Sink.ToolEvent != nil {
					opts.Sink.ToolEvent("started", c, "")
				}
				if opts.Transcript != nil {
					opts.Transcript.ToolCall(c)
				}
				out := s.runOne(ctx, opts, c)
				if opts.Transcript != nil {
					opts.Transcript.ToolResult(c.ID, c.Name, out)
				}
				if opts.Sink.ToolEvent != nil {
					opts.Sink.ToolEvent("completed", c, out)
				}
				s.mu.Lock()
				s.msgs = append(s.msgs, llm.Message{Role: "tool", ToolCallID: c.ID, Name: c.Name, Content: out})
				s.persistLocked()
				s.mu.Unlock()
			}
			continue
		}
		s.mu.Lock()
		s.msgs = appendAssistant(s.msgs, resp)
		if text := strings.TrimSpace(resp.Text); text != "" {
			s.ingested = append(s.ingested, text)
			if opts.Sink.Ingest != nil {
				_ = opts.Sink.Ingest(ctx, s.ingested)
			}
		}
		s.persistLocked()
		s.mu.Unlock()
		return nil
	}
	return fmt.Errorf("max turns")
}

// runOne executes one tool call through the catalog or the file jail.
func (s *Session) runOne(ctx context.Context, opts RunOpts, c llm.ToolCall) string {
	var args map[string]any
	_ = json.Unmarshal(c.Arguments, &args)
	str := func(k string) string {
		v, _ := args[k].(string)
		return v
	}
	if opts.Registry != nil {
		if _, ok := opts.Registry.Catalog()[c.Name]; ok {
			return runCatalog(ctx, opts, c)
		}
	}
	switch c.Name {
	case "bash":
		return runBash(ctx, opts, str("command"))
	case "read":
		return runRead(ctx, opts, str("path"))
	case "write":
		return runWrite(ctx, opts, str("path"), str("content"))
	case "edit":
		return runEdit(ctx, opts, str("path"), str("old_string"), str("new_string"))
	case "glob":
		return runGlob(opts, str("pattern"))
	case "grep":
		return runGrep(opts, str("pattern"), str("path"))
	default:
		return fmt.Sprintf("unknown tool %q", c.Name)
	}
}

// Usage returns the cumulative token usage of the session.
func (s *Session) Usage() llm.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *Session) addUsage(add llm.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	addUsage(&s.usage, add)
}

// Turn returns the number of prompts the session has taken.
func (s *Session) Turn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

// --- full-fidelity message log, so a new process resumes exactly ---

func (s *Session) persistLocked() {
	if s.opts.RunID == "" {
		return
	}
	path := MessagesPath(s.opts.RunID)
	if err := writeMessages(path, s.msgs); err != nil {
		// The transcript still holds the display log; a resume then starts
		// fresh instead of failing the prompt.
		_ = err
	}
}

// MessagesPath is ~/.trebi/runs/<id>/messages.jsonl.
func MessagesPath(runID string) string {
	return filepath.Join(home.RunsDir(), runID, "messages.jsonl")
}

func writeMessages(path string, msgs []llm.Message) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// LoadMessages reads a persisted message log. A missing file is an empty log.
func LoadMessages(runID string) ([]llm.Message, error) {
	f, err := os.Open(MessagesPath(runID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []llm.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		var m llm.Message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

// SetSink installs or replaces the streaming hooks. The session driver calls
// it right after NewSession, when the hooks can reference the session.
func (s *Session) SetSink(sink Sink) {
	s.mu.Lock()
	s.opts.Sink = sink
	s.mu.Unlock()
}

// SeedMessages loads history from a previous process. Call it before the
// first Prompt.
func (s *Session) SeedMessages(msgs []llm.Message) {
	s.mu.Lock()
	s.msgs = append(s.msgs, msgs...)
	s.mu.Unlock()
}
