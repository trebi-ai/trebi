package api

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/trebi-ai/trebi/internal/guides"
	"github.com/trebi-ai/trebi/internal/tools"
)

func TestGuideTranslationDrift(t *testing.T) {
	t.Parallel()
	catalog := tools.NewCatalog()
	dir := filepath.Join("..", "guides")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		for _, inv := range extractTrebi(path, src) {
			tool, action, local, ok := mcpMatch(catalog, inv.words)
			if local && strings.Join(inv.words, " ") == "config validate" && slices.ContainsFunc(inv.flags, isDraftFlag) {
				// A draft check goes through the daemon.
				tool, action, local = "config", "validate_draft", false
				_, ok = catalog[tool][action]
			}
			if local {
				if !hasLocalOnly(src, inv.line) {
					t.Errorf("%s:%d: local-only %q missing **local only** marker", e.Name(), inv.line, inv.raw)
				}
				continue
			}
			if !ok {
				t.Errorf("%s:%d: no MCP action %s.%s for %q", e.Name(), inv.line, tool, action, inv.raw)
				continue
			}
			for _, flag := range inv.flags {
				if skipFlag[strings.ReplaceAll(flag, "-", "_")] {
					continue
				}
				if len(flag) == 1 {
					t.Errorf("%s:%d: short flag -%s in %q; guides use long flags", e.Name(), inv.line, flag, inv.raw)
				}
			}
		}
	}
}

func TestGuideCrossReferences(t *testing.T) {
	t.Parallel()
	topics := map[string]bool{}
	for _, g := range guides.List() {
		topics[g.Topic] = true
	}
	footerTopics := map[string]bool{}
	docs, err := filepath.Glob("../tools/mcpdocs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	footerRE := regexp.MustCompile("`([a-z0-9]+(?:-[a-z0-9]+)*)`")
	for _, path := range docs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		idx := strings.Index(text, "## Related guides")
		if idx < 0 {
			t.Errorf("%s: missing Related guides footer", path)
			continue
		}
		for _, m := range footerRE.FindAllStringSubmatch(text[idx:], -1) {
			topic := m[1]
			if !topics[topic] {
				t.Errorf("%s: Related guides names unknown topic %q", path, topic)
			}
			footerTopics[topic] = true
		}
	}
	for topic := range topics {
		if !footerTopics[topic] {
			t.Errorf("topic %q is not named in any mcpdocs Related guides footer", topic)
		}
	}

	skill, err := os.ReadFile(filepath.Join("..", "..", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for topic := range topics {
		counts[topic] = strings.Count(string(skill), "`"+topic+"`")
	}
	// Slim skill must name every topic once in the Guides index. Before the
	// restructure the fat SKILL.md may name none — allow zero or one.
	named := 0
	for _, n := range counts {
		if n > 0 {
			named++
		}
		if n > 1 {
			t.Errorf("SKILL.md names a topic %d times; want 0 (pre-slim) or 1", n)
		}
	}
	if named != 0 && named != len(topics) {
		t.Errorf("SKILL.md names %d/%d guide topics; want all or none", named, len(topics))
	}
}

var skipFlag = map[string]bool{
	"json": true, // no-op remotely
}

var localOnlyPrefixes = []string{
	"guide",
	"workspace use",
	"workspace current",
	"workspace unlink",
	"runs attach",
	"assistant chat",
	"assistant sessions",
	"assistant close",
	"assistant history",
	"job tail",
	"job status",
	"job schedule",
	"service install",
	"service start",
	"service stop",
	"service restart",
	"service uninstall",
	"service daemon",
	"service migrate-db",
	"config path",
	"config validate",
	"config bridge",
	"conversation switch",
	"mcp",
}

var exactFolds = map[string]string{
	"bus":                 "system.bus_poll",
	"doctor":              "system.doctor",
	"doctor fix":          "system.doctor_fix",
	"alerts":              "inbox.alerts",
	"rules test":          "event.rules_test",
	"rules test-batch":    "event.rules_test_batch",
	"rules fire":          "event.rules_fire",
	"service logs":        "system.logs",
	"service drain":       "system.drain",
	"service undrain":     "system.undrain",
	"service shutdown":    "system.shutdown",
	"service status":      "system.health",
	"cloud status":        "system.cloud_status",
	"cloud link":          "system.cloud_link",
	"cloud unlink":        "system.cloud_unlink",
	"assistant interrupt": "conversation.interrupt",
	"cloud addresses":     "system.cloud_addresses",
	"hooks create":        "system.hook_create",
	"hooks list":          "system.hook_list",
	"hooks delete":        "system.hook_delete",
	"harness auth status": "harness.auth",
	"harness auth logout": "harness.logout",
	"harness auth mode":   "harness.patch",
	"harness auth login":  "harness.login_start",
	"inbox read":          "inbox.apply",
	"inbox unread":        "inbox.apply",
	"inbox archive":       "inbox.apply",
	"inbox unarchive":     "inbox.apply",
	"inbox ls":            "inbox.list",
	"status":              "system.overview",
	"state":               "state.summary",
	"decisions ls":        "decision.list",
	"decisions show":      "decision.get",
	"decisions kinds":     "decision.kinds",
	"decisions stats":     "decision.stats",
	"decisions test":      "decision.test",
	"decisions feedback":  "decision.feedback",
	// The CLI says `memory log add`; the catalog kept the shorter name.
	"job memory log add":          "job.memory_add",
	"job memory log delete":       "job.memory_delete",
	"agent memory log add":        "agent.memory_add",
	"agent memory log delete":     "agent.memory_delete",
	"assistant memory log add":    "assistant.memory_add",
	"assistant memory log delete": "assistant.memory_delete",
}

func mcpMatch(catalog tools.Catalog, words []string) (tool, action string, local, ok bool) {
	if len(words) == 0 {
		return "", "", false, false
	}
	path := strings.Join(words, " ")
	for _, p := range localOnlyPrefixes {
		if path == p || strings.HasPrefix(path, p+" ") {
			return "", "", true, true
		}
	}
	if fold := foldPrefix(path); fold != "" {
		tool, action, _ = strings.Cut(fold, ".")
		_, ok = catalog[tool][action]
		return tool, action, false, ok
	}
	rest := words
	switch {
	case len(words) >= 2 && words[0] == "job" && words[1] == "file":
		tool, rest = "file", words[2:]
	case len(words) >= 2 && words[0] == "skill" && words[1] == "file":
		tool, rest = "file", words[2:]
	default:
		tool = words[0]
		if tool == "runs" {
			tool = "run"
		}
		rest = words[1:]
	}
	actions, have := catalog[tool]
	if !have {
		return tool, joinAction(rest), false, false
	}
	for n := len(rest); n >= 0; n-- {
		act := "list"
		if n > 0 {
			act = joinAction(rest[:n])
		}
		if _, exists := actions[act]; exists {
			return tool, act, false, true
		}
	}
	return tool, joinAction(rest), false, false
}

func joinAction(words []string) string {
	return strings.ReplaceAll(strings.Join(words, "_"), "-", "_")
}

func foldPrefix(path string) string {
	best, fold := "", ""
	for p, f := range exactFolds {
		if path == p || strings.HasPrefix(path, p+" ") {
			if len(p) > len(best) {
				best, fold = p, f
			}
		}
	}
	return fold
}

var localOnlyRE = regexp.MustCompile(`(?i)local only`)

func hasLocalOnly(src string, line int) bool {
	lines := strings.Split(src, "\n")
	for d := -4; d <= 8; d++ {
		i := line - 1 + d
		if i >= 0 && i < len(lines) && localOnlyRE.MatchString(lines[i]) {
			return true
		}
	}
	return false
}

type trebiInv struct {
	file  string
	line  int
	raw   string
	words []string
	flags []string
}

var fenceRE = regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\\r?\\n(.*?)```")

func extractTrebi(path, src string) []trebiInv {
	var out []trebiInv
	for _, m := range fenceRE.FindAllStringSubmatchIndex(src, -1) {
		body := src[m[2]:m[3]]
		start := 1 + strings.Count(src[:m[2]], "\n")
		offset := 0
		for _, logical := range splitLogicalLines(body) {
			lineNo := start + strings.Count(body[:min(offset, len(body))], "\n")
			offset += len(logical) + 1
			cmd := trebiCommand(logical)
			if cmd == "" {
				continue
			}
			words, flags := splitTrebi(cmd)
			out = append(out, trebiInv{file: path, line: lineNo, raw: cmd, words: words, flags: flags})
		}
	}
	return out
}

func splitLogicalLines(body string) []string {
	raw := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var out []string
	var buf string
	for _, ln := range raw {
		trim := strings.TrimRight(ln, " \t")
		if strings.HasSuffix(trim, "\\") {
			buf += strings.TrimSuffix(trim, "\\") + " "
			continue
		}
		out = append(out, buf+ln)
		buf = ""
	}
	if buf != "" {
		out = append(out, buf)
	}
	return out
}

func trebiCommand(line string) string {
	line = stripHashComment(line)
	idx := indexCommand(line, "trebi")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(line[idx:])
}

func indexCommand(line, name string) int {
	for i := 0; i <= len(line)-len(name); i++ {
		if i > 0 {
			prev := rune(line[i-1])
			if unicode.IsLetter(prev) || unicode.IsDigit(prev) || prev == '_' || prev == '-' {
				continue
			}
		}
		if line[i:i+len(name)] != name {
			continue
		}
		end := i + len(name)
		if end < len(line) {
			next := rune(line[end])
			if unicode.IsLetter(next) || unicode.IsDigit(next) || next == '_' || next == '-' {
				continue
			}
		}
		return i
	}
	return -1
}

func stripHashComment(line string) string {
	inQ := rune(0)
	for i, r := range line {
		switch {
		case inQ != 0:
			if r == inQ {
				inQ = 0
			}
		case r == '"' || r == '\'':
			inQ = r
		case r == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return strings.TrimSpace(line[:i])
		}
	}
	return line
}

var boolFlags = map[string]bool{
	"json": true, "test": true, "attach": true, "create": true, "use": true,
	"force": true, "purge": true, "raw": true, "replay": true, "stat": true,
	"check": true, "live": true, "follow": true, "yes": true, "token-stdin": true,
	"git-enabled": true, "fix": true, "detailed": true, "schedule": true,
	"skip-check": true, "tree": true, "yaml": true,
}

func splitTrebi(cmd string) (words, flags []string) {
	toks := tokenize(cmd)
	i := 0
	if len(toks) > 0 && toks[0] == "trebi" {
		i = 1
	}
	for i < len(toks) {
		t := toks[i]
		if t == "--" {
			break
		}
		if strings.HasPrefix(t, "-") {
			name, _, hadEq := strings.Cut(strings.TrimLeft(t, "-"), "=")
			flags = append(flags, name)
			i++
			if !hadEq && !boolFlags[name] && i < len(toks) && !strings.HasPrefix(toks[i], "-") {
				i++
			}
			continue
		}
		words = append(words, t)
		i++
	}
	return words, flags
}

func tokenize(s string) []string {
	var toks []string
	var b strings.Builder
	quote := rune(0)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case unicode.IsSpace(r):
			if b.Len() > 0 {
				toks = append(toks, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		toks = append(toks, b.String())
	}
	return toks
}

func isDraftFlag(f string) bool { return f == "set" || f == "unset" || f == "file" }
