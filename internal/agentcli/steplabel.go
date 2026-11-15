package agentcli

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// StepLabelMaxRunes caps a label that comes from a harness description.
const StepLabelMaxRunes = 200

// WebSearchLabel is the label of a web search. A chapter folds all of them.
const WebSearchLabel = "Searched the web"

// pathKeys are the input keys that name a file, as agentwire reads them.
var pathKeys = []string{"file_path", "filePath", "path", "notebook_path", "target_file", "file"} //nolint:gochecknoglobals // immutable key list

// QuietTool reports whether the tool does bookkeeping only. A quiet step
// counts, but a view does not show it.
func QuietTool(name string) bool {
	switch strings.ToLower(name) {
	case "toolsearch", "todowrite", "todoread":
		return true
	}
	return false
}

// StepLabel returns the short words of one tool call. The harness
// description wins. Otherwise the words come from the tool name and its main
// input. A quiet tool gives an empty label.
func StepLabel(name, input string) string {
	if QuietTool(name) {
		return ""
	}
	var in map[string]any
	if json.Unmarshal([]byte(input), &in) == nil {
		if d := strings.Join(strings.Fields(str(in["description"])), " "); d != "" {
			return capRunes(d, StepLabelMaxRunes)
		}
	}
	switch n := strings.ToLower(name); n {
	case "websearch", "web_search":
		return WebSearchLabel
	case "webfetch":
		return "Read " + webTarget(str(in["url"]))
	case "read", "readfile", "read_file", "notebookread":
		return withFile("Read", "a file", in)
	case "edit", "multiedit", "notebookedit":
		return withFile("Edited", "a file", in)
	case "write":
		return withFile("Wrote", "a file", in)
	case "filechange", "apply_patch", "applypatch":
		return withFile("Edited", "files", in)
	case "grep", "glob":
		return "Searched files"
	case "bash", "shell", "command", "exec_command":
		return "Ran a command"
	default:
		if server, ok := mcpServer(name); ok {
			return "Used " + server
		}
		return words(name)
	}
}

// mcpServer reads the server of mcp__<server>__<tool>. The Codex wire name
// mcp:<tool> has no server, so the tool stands for it.
func mcpServer(name string) (string, bool) {
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		server, _, _ := strings.Cut(rest, "__")
		return server, server != ""
	}
	if tool, ok := strings.CutPrefix(name, "mcp:"); ok {
		return words(tool), tool != ""
	}
	return "", false
}

// withFile joins the verb and the base name of the file of the input.
func withFile(verb, fallback string, in map[string]any) string {
	for _, k := range pathKeys {
		if p := str(in[k]); p != "" {
			return verb + " " + filepath.Base(p)
		}
	}
	return verb + " " + fallback
}

// webTarget is the host without "www." and the path without its last slash.
func webTarget(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "a web page"
	}
	return strings.TrimPrefix(u.Host, "www.") + strings.TrimRight(u.Path, "/")
}

// words turns a tool name such as get_issue or ReadMcpResource into words.
func words(name string) string {
	var b strings.Builder
	var prev rune
	for _, r := range name {
		switch {
		case r == '_' || r == '-' || r == ':' || r == '.':
			b.WriteByte(' ')
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			b.WriteByte(' ')
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(unicode.ToLower(r))
		}
		prev = r
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if out == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(out)
	return string(unicode.ToUpper(r)) + out[size:]
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
