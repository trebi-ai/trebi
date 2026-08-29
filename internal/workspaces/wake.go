package workspaces

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
)

const (
	wakeIndexCap = 8 * 1024
	wakeRecapCap = 8 * 1024
	truncMark    = "\n…[truncated]"
)

// WakeOpts is the extra material folded into a composed assistant wake prompt.
type WakeOpts struct {
	ExtraPrompt string
	Recap       string
	Message     string
}

// ComposeWakePrompt builds the B27 assistant prompt:
// TREBI.md body → job index → MEMORY.md → recap → triggering message / extra.
func (s *Service) ComposeWakePrompt(ctx context.Context, wsID string, opts WakeOpts) (string, error) {
	ws, err := s.Get(ctx, wsID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	writeSection(&b, "Instructions", trebiBody(ws.Path))
	writeSection(&b, "Workspace jobs", capSection(s.jobIndex(ctx, wsID), wakeIndexCap))
	writeSection(&b, "Memory", memoryBody(ws.Path))
	if recap := strings.TrimSpace(opts.Recap); recap != "" {
		writeSection(&b, "Conversation recap", capSection(recap, wakeRecapCap))
	}
	msg := strings.TrimSpace(opts.Message)
	if msg == "" {
		msg = strings.TrimSpace(opts.ExtraPrompt)
	}
	if msg != "" {
		writeSection(&b, "Current message", msg)
	}
	return strings.TrimSpace(b.String()), nil
}

func trebiBody(wsPath string) string {
	j, err := job.ParseFile(filepath.Join(wsPath, "TREBI.md"))
	if err != nil || j == nil || strings.TrimSpace(j.Prompt) == "" {
		return "(none)"
	}
	return strings.TrimSpace(j.Prompt)
}

func memoryBody(wsPath string) string {
	data, err := os.ReadFile(filepath.Join(wsPath, "MEMORY.md"))
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return "(empty)"
	}
	return strings.TrimSpace(string(data))
}

func (s *Service) jobIndex(ctx context.Context, wsID string) string {
	jobs, err := s.ListJobs(ctx, wsID)
	if err != nil || len(jobs) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, row := range jobs {
		active := "inactive"
		if row.Active {
			active = "active"
		}
		fmt.Fprintf(&b, "- %s kind=%s harness=%s %s", row.Name, row.Kind, row.Harness, active)
		if row.ParseError != "" {
			fmt.Fprintf(&b, " parse_error=%s", row.ParseError)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func writeSection(b *strings.Builder, title, body string) {
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	fmt.Fprintf(b, "# %s\n%s", title, strings.TrimSpace(body))
}

func capSection(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + truncMark
}
