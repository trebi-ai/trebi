package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/trebi-ai/trebi/internal/guides"
	"github.com/urfave/cli/v3"
)

func TestGuideListAndGet(t *testing.T) {
	out, err := runCLI(t, nil, "guide", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"topic"`) || !strings.Contains(out, "getting-started") {
		t.Fatalf("list: %s", out)
	}
	out, err = runCLI(t, nil, "guide", "getting-started")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Fresh install") {
		t.Fatalf("get body: %s", out)
	}
	_, err = runCLI(t, nil, "guide", "no-such-topic")
	if err == nil {
		t.Fatal("want error for unknown topic")
	}
}

func TestGuideCommandDrift(t *testing.T) {
	t.Parallel()
	app := NewApp(Options{})
	files := guideSources(t)
	var failed int
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, inv := range extractTrebi(f, string(data)) {
			if err := resolveCLI(app, inv.words); err != nil {
				t.Errorf("%s:%d: %s\n  %v", relGuide(f), inv.line, inv.raw, err)
				failed++
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%d unknown trebi invocations", failed)
	}
}

func TestGuideCorpusLoaded(t *testing.T) {
	t.Parallel()
	if n := len(guides.List()); n != 17 {
		t.Fatalf("guides.List = %d", n)
	}
}

type trebiInv struct {
	file  string
	line  int
	raw   string
	words []string // positional words after trebi (flags stripped)
	flags []string // long flag names, no leading dashes
}

var fenceRE = regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\\r?\\n(.*?)```")

func extractTrebi(path, src string) []trebiInv {
	var out []trebiInv
	for _, m := range fenceRE.FindAllStringSubmatchIndex(src, -1) {
		body := src[m[2]:m[3]]
		start := 1 + strings.Count(src[:m[2]], "\n")
		offset := 0
		for _, logical := range splitLogicalLines(body) {
			lineNo := start + strings.Count(body[:offset], "\n")
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
	if len(toks) == 0 {
		return nil, nil
	}
	i := 0
	if toks[0] == "trebi" {
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

func resolveCLI(app *cli.Command, words []string) error {
	cmd := app
	for i, w := range words {
		sub := findNamed(cmd.Commands, w)
		if sub == nil {
			if i == 0 {
				return fmt.Errorf("unknown command %q", w)
			}
			return nil
		}
		cmd = sub
	}
	return nil
}

func findNamed(cmds []*cli.Command, name string) *cli.Command {
	for _, c := range cmds {
		if c == nil {
			continue
		}
		if c.Name == name {
			return c
		}
		for _, a := range c.Aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}

func guideSources(t *testing.T) []string {
	t.Helper()
	var files []string
	skill := filepath.Join("..", "..", "SKILL.md")
	if _, err := os.Stat(skill); err != nil {
		t.Fatal(err)
	}
	files = append(files, skill)
	dir := filepath.Join("..", "guides")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files
}

func relGuide(path string) string {
	return filepath.ToSlash(path)
}
