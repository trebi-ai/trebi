// Package native is the in-process assistant loop (no tmux, no vendor CLI).
package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/llm"
	"github.com/trebi-ai/trebi/internal/tools"
)

const (
	DefaultMaxTurns = 20
	defaultBashTO   = 60 * time.Second
	defaultSystem   = "You are the Trebi workspace assistant. Use the registered trebi tools for control-plane operations and workspace tools for files."
)

// Sink is the host callbacks. Native does not import runs or conversations.
type Sink struct {
	AddEvent func(ctx context.Context, kind, source string, detail any)
	Ingest   func(ctx context.Context, texts []string) error
	Recap    func(ctx context.Context, text string) error
	SetAgent func(ctx context.Context, state string)
	Finish   func(ctx context.Context, status, end string, usage llm.Usage) error
	// Gate validates memory at the completion point. A non-empty return is a
	// fix message the loop appends as the next user message; empty = pass.
	Gate func(ctx context.Context) string
}

// RunOpts is one native assistant execution.
type RunOpts struct {
	Provider      llm.Provider
	RunID         string
	ConvID        string
	WorkspaceRoot string
	WorkingDir    string
	System        string
	Prompt        string
	Model         string
	MaxTokens     int
	Headers       map[string]string
	MaxTurns      int
	ContextTokens int
	DailyLimit    int
	Inbox         <-chan string
	MaxFileBytes  int64
	Registry      *tools.Registry
	Env           []string
	BashTimeout   time.Duration
	CheckDaily    func(ctx context.Context, add int64) (ok bool, err error)
	Sink          Sink
	Log           *slog.Logger
	Transcript    *Transcript
}

// Run is the tool loop. Context cancel is kill.
func Run(ctx context.Context, opts RunOpts) error {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
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
	msgs := []llm.Message{{Role: "user", Content: opts.Prompt}}
	toolset := toolSpecs(opts.Registry)
	var ingested []string
	var usage llm.Usage
	if opts.Sink.SetAgent != nil {
		opts.Sink.SetAgent(ctx, "running")
	}
	err := loop(ctx, opts, sys, msgs, toolset, &ingested, &usage)
	status, end := "complete", "sentinel"
	if err != nil {
		status, end = classify(ctx, err)
	}
	if opts.Sink.SetAgent != nil {
		opts.Sink.SetAgent(ctx, "idle")
	}
	if opts.Sink.Finish != nil {
		_ = opts.Sink.Finish(ctx, status, end, usage)
	}
	return err
}

func loop(ctx context.Context, opts RunOpts, sys string, msgs []llm.Message, tools []llm.Tool, ingested *[]string, usage *llm.Usage) error {
	for turn := 0; turn < opts.MaxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := maybeCompact(ctx, opts, &msgs); err != nil {
			return err
		}
		resp, err := opts.Provider.Chat(ctx, llm.Request{
			Model: opts.Model, System: sys, Messages: msgs,
			Tools: tools, MaxTokens: opts.MaxTokens, Headers: opts.Headers,
		})
		if err != nil {
			return err
		}
		addUsage(usage, resp.Usage)
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
		if len(resp.ToolCalls) > 0 {
			msgs = appendAssistant(msgs, resp)
			msgs = append(msgs, execTools(ctx, opts, resp.ToolCalls)...)
			continue
		}
		if text := strings.TrimSpace(resp.Text); text != "" {
			*ingested = append(*ingested, text)
			if opts.Sink.Ingest != nil {
				_ = opts.Sink.Ingest(ctx, *ingested)
			}
		}
		msgs = appendAssistant(msgs, resp)
		if follow, ok := takeInbox(ctx, opts.Inbox); ok {
			msgs = append(msgs, llm.Message{Role: "user", Content: follow})
			continue
		}
		if resp.Stop == llm.StopMaxTokens {
			return fmt.Errorf("%s", llm.StopMaxTokens)
		}
		if opts.Sink.Gate != nil {
			if fix := opts.Sink.Gate(ctx); fix != "" {
				msgs = append(msgs, llm.Message{Role: "user", Content: fix})
				continue
			}
		}
		return nil
	}
	return fmt.Errorf("max turns")
}

func appendAssistant(msgs []llm.Message, resp *llm.Response) []llm.Message {
	return append(msgs, llm.Message{
		Role: "assistant", Content: resp.Text, ToolCalls: resp.ToolCalls,
	})
}

func takeInbox(ctx context.Context, inbox <-chan string) (string, bool) {
	if inbox == nil {
		return "", false
	}
	select {
	case <-ctx.Done():
		return "", false
	case msg, ok := <-inbox:
		return msg, ok && strings.TrimSpace(msg) != ""
	default:
		return "", false
	}
}

func addUsage(dst *llm.Usage, add llm.Usage) {
	dst.Input += add.Input
	dst.Output += add.Output
	dst.CacheRead += add.CacheRead
	dst.CacheCreation += add.CacheCreation
}

func enforceDaily(ctx context.Context, opts RunOpts, u llm.Usage) error {
	if opts.CheckDaily == nil {
		return nil
	}
	n := u.Input + u.Output + u.CacheCreation
	ok, err := opts.CheckDaily(ctx, n)
	if err != nil {
		return err
	}
	if !ok {
		return errDailyLimit
	}
	return nil
}

func classify(ctx context.Context, err error) (status, end string) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return "exited", "kill"
	case errors.Is(err, llm.ErrUnauthorized):
		return "failed", "auth"
	case errors.Is(err, errDailyLimit):
		return "failed", "limit"
	case strings.Contains(err.Error(), "max turns"):
		return "failed", "limit"
	default:
		return "failed", "failed"
	}
}

var errDailyLimit = errors.New("daily token limit")

func redactArgs(raw json.RawMessage) map[string]any {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return map[string]any{"raw": trunc(string(raw), 200)}
	}
	for k, v := range m {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "key") || strings.Contains(lk, "token") || strings.Contains(lk, "secret") {
			m[k] = "********"
			continue
		}
		if s, ok := v.(string); ok && len(s) > 400 {
			m[k] = trunc(s, 400)
		}
	}
	return m
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
