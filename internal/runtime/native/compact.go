package native

import (
	"context"
	"fmt"
	"strings"

	"github.com/trebi-ai/trebi/internal/llm"
)

func maybeCompact(ctx context.Context, opts RunOpts, msgs *[]llm.Message) error {
	if opts.ContextTokens <= 0 || opts.Provider == nil || msgs == nil {
		return nil
	}
	est := estimateTokens(*msgs)
	if est < (opts.ContextTokens*60)/100 {
		return nil
	}
	if len(*msgs) < 4 {
		return nil
	}
	keep := 2
	prefix := (*msgs)[:len(*msgs)-keep]
	suffix := (*msgs)[len(*msgs)-keep:]
	var b strings.Builder
	for _, m := range prefix {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, trunc(m.Content, 2000))
	}
	resp, err := opts.Provider.Chat(ctx, llm.Request{
		Model: opts.Model, MaxTokens: min(1024, opts.MaxTokens),
		System:   "Summarize the conversation for a continuing assistant. Preserve facts, names, and open tasks.",
		Messages: []llm.Message{{Role: "user", Content: b.String()}},
	})
	if err != nil {
		return nil // compaction is best-effort
	}
	sum := strings.TrimSpace(resp.Text)
	if sum == "" {
		return nil
	}
	*msgs = append([]llm.Message{{Role: "user", Content: "Conversation recap:\n" + sum}}, suffix...)
	if opts.Sink.Recap != nil {
		_ = opts.Sink.Recap(ctx, sum)
	}
	if opts.Transcript != nil {
		opts.Transcript.write("recap", map[string]any{"text": sum})
	}
	return nil
}

func estimateTokens(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content) / 4
		for _, tc := range m.ToolCalls {
			n += (len(tc.Name) + len(tc.Arguments)) / 4
		}
	}
	return n
}
