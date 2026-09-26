package assistant

import (
	"fmt"
	"strings"
)

// memoryCap is the largest memory render the instructions carry.
const memoryCap = 8 * 1024

// baseRole is the fixed role text of every assistant session (plan D.10).
const baseRole = `You are the workspace assistant. You run inside Trebi, ` +
	`the local job runner for this workspace.` + "\n\n" +
	`The Trebi MCP tools are the way to read and change jobs, runs, agents, ` +
	`and memory. Use them for control-plane operations; use your own file ` +
	`tools for workspace files.` + "\n\n" +
	`Ask before you delete a job or change a trigger.`

// compose renders the instructions a new session starts with: role,
// workspace, the effective context prompt, and the rendered memory. It does
// not add a job index, a recap, or the current message.
func compose(eff Settings, ws WorkspaceInfo, memory string) string {
	var b strings.Builder
	writeSection(&b, "Role", baseRole)
	writeSection(&b, "Workspace", fmt.Sprintf("Name: %s\nID: %s\nPath: %s", ws.Name, ws.ID, ws.Path))
	if ctx := strings.TrimSpace(eff.Context); ctx != "" {
		writeSection(&b, "Context", ctx)
	}
	if mem := strings.TrimSpace(memory); mem != "" {
		writeSection(&b, "Memory", capRunes(mem, memoryCap))
	}
	return strings.TrimSpace(b.String())
}

func writeSection(b *strings.Builder, title, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	b.WriteString("## " + title + "\n\n")
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n\n")
}

// capRunes cuts text to at most max runes, marking a cut.
func capRunes(text string, max int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max-1]) + "\n…[truncated]"
}

// firstLine returns the first line of text, at most max runes.
func firstLine(text string, max int) string {
	if line, _, ok := strings.Cut(text, "\n"); ok {
		text = line
	}
	return truncateRunes(text, max)
}

// truncateRunes cuts text to at most max runes, marking a cut with an ellipsis.
func truncateRunes(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max-1]) + "…"
}
