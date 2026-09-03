package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Grader runs deterministic checks against a live trialEnv.
type Grader struct {
	env *trialEnv
	p   Placeholders
}

func newGrader(env *trialEnv, p Placeholders) *Grader {
	return &Grader{env: env, p: p}
}

func (g *Grader) runAll(ctx context.Context, checks []Check) []CheckResult {
	out := make([]CheckResult, 0, len(checks))
	for _, c := range checks {
		out = append(out, g.runOne(ctx, c))
	}
	return out
}

func (g *Grader) runOne(ctx context.Context, c Check) CheckResult {
	c = g.expandCheck(c)
	switch {
	case c.Cmd != "":
		return g.checkCmd(ctx, c)
	case c.FileExists != "":
		return fileStatCheck(c.Name, c.FileExists, true)
	case c.FileExistsAny != "":
		return fileStatCheck(c.Name, resolveWanted(c.FileExistsAny), true)
	case c.FileAbsent != "":
		return fileStatCheck(c.Name, c.FileAbsent, false)
	case c.FileContains != "":
		return fileTextCheck(c.Name, resolveWanted(c.FileContains), c.Text, true)
	case c.FileNotContains != "":
		return fileTextCheck(c.Name, c.FileNotContains, c.Text, false)
	case c.FileMatches != "":
		return fileMatchCheck(c.Name, resolveWanted(c.FileMatches), c.Text)
	case c.SnapshotJQ != "":
		return g.checkSnapshotJQ(c)
	case c.CommandsJQ != "":
		return g.checkCommandsJQ(c)
	case c.HomeUntouched:
		return g.checkHomeUntouched()
	default:
		return CheckResult{Name: c.Name, Detail: "empty check"}
	}
}

func (g *Grader) expandCheck(c Check) Check {
	c.Cmd = g.p.Expand(c.Cmd)
	c.FileExists = g.p.Expand(c.FileExists)
	c.FileExistsAny = g.p.Expand(c.FileExistsAny)
	c.FileAbsent = g.p.Expand(c.FileAbsent)
	c.FileContains = g.p.Expand(c.FileContains)
	c.FileNotContains = g.p.Expand(c.FileNotContains)
	c.FileMatches = g.p.Expand(c.FileMatches)
	c.Text = g.p.Expand(c.Text)
	c.StdoutContains = g.p.Expand(c.StdoutContains)
	c.StdoutAbsent = g.p.Expand(c.StdoutAbsent)
	return c
}

func (g *Grader) checkCmd(ctx context.Context, c Check) CheckResult {
	args := splitCommand(c.Cmd)
	if len(args) == 0 {
		return CheckResult{Name: c.Name, Detail: "empty cmd"}
	}
	if args[0] == "trebi" {
		args[0] = g.env.bin
	}
	out, errb, err := g.env.runOut(ctx, args[0], args[1:]...)
	wantExit := 0
	if c.Exit != nil {
		wantExit = *c.Exit
	}
	gotExit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			gotExit = ee.ExitCode()
		} else {
			return CheckResult{Name: c.Name, Infra: true, Detail: err.Error()}
		}
	}
	if gotExit != wantExit {
		return CheckResult{Name: c.Name, Detail: fmt.Sprintf("exit %d want %d\n%s%s", gotExit, wantExit, out, errb)}
	}
	if c.StdoutContains != "" && !bytes.Contains(out, []byte(c.StdoutContains)) {
		return CheckResult{Name: c.Name, Detail: fmt.Sprintf("stdout missing %q:\n%s", c.StdoutContains, out)}
	}
	if c.StdoutAbsent != "" && bytes.Contains(out, []byte(c.StdoutAbsent)) {
		return CheckResult{Name: c.Name, Detail: fmt.Sprintf("stdout contains %q:\n%s", c.StdoutAbsent, out)}
	}
	if c.JQ != "" {
		ok, detail, qerr := jqTruthy(c.JQ, out)
		if qerr != nil {
			return CheckResult{Name: c.Name, Infra: true, Detail: qerr.Error()}
		}
		if !ok {
			return CheckResult{Name: c.Name, Detail: detail}
		}
	}
	return CheckResult{Name: c.Name, Pass: true}
}

func (g *Grader) checkSnapshotJQ(c Check) CheckResult {
	data, err := os.ReadFile(g.p.Snapshot)
	if err != nil {
		return CheckResult{Name: c.Name, Detail: err.Error()}
	}
	ok, detail, qerr := jqTruthy(c.SnapshotJQ, data)
	if qerr != nil {
		return CheckResult{Name: c.Name, Infra: true, Detail: qerr.Error()}
	}
	if !ok {
		return CheckResult{Name: c.Name, Detail: detail}
	}
	return CheckResult{Name: c.Name, Pass: true}
}

func (g *Grader) checkCommandsJQ(c Check) CheckResult {
	data, err := os.ReadFile(g.p.Transcript)
	if err != nil {
		return CheckResult{Name: c.Name, Infra: true, Detail: err.Error()}
	}
	cmds, err := transcriptCommands(data)
	if err != nil {
		return CheckResult{Name: c.Name, Infra: true, Detail: err.Error()}
	}
	// all(...) is vacuously true on an empty list, so a transcript that
	// projected no actions would pass without the agent doing anything.
	if len(cmds) == 0 {
		return CheckResult{Name: c.Name, Detail: "no commands or trebi MCP calls in transcript"}
	}
	payload, err := json.Marshal(cmds)
	if err != nil {
		return CheckResult{Name: c.Name, Infra: true, Detail: err.Error()}
	}
	ok, detail, qerr := jqTruthy(c.CommandsJQ, payload)
	if qerr != nil {
		return CheckResult{Name: c.Name, Infra: true, Detail: qerr.Error()}
	}
	if !ok {
		return CheckResult{Name: c.Name, Detail: detail}
	}
	return CheckResult{Name: c.Name, Pass: true}
}

func (g *Grader) checkHomeUntouched() CheckResult {
	now, err := hashTree(g.env.home)
	if err != nil {
		return CheckResult{Name: "home untouched", Infra: true, Detail: err.Error()}
	}
	var extra []string
	for rel := range now {
		if daemonOwned(rel) {
			continue
		}
		if g.env.hashes[rel] != now[rel] {
			extra = append(extra, rel)
		}
	}
	if len(extra) > 0 {
		return CheckResult{Name: "home untouched", Detail: "agent-touched: " + strings.Join(extra, ", ")}
	}
	return CheckResult{Name: "home untouched", Pass: true}
}

func (g *Grader) collectSnapshot(ctx context.Context) error {
	snap := map[string]any{}
	g.addDump(ctx, snap, "workspaces", "workspace", "list", "--json")
	g.addDump(ctx, snap, "jobs", "job", "list", "--json")
	g.addDump(ctx, snap, "runs", "runs", "list", "--json", "--limit", "50")
	g.addDump(ctx, snap, "queue", "queue", "--json")
	g.addDump(ctx, snap, "inbox", "inbox", "list", "--json")
	g.addDump(ctx, snap, "config", "config", "show", "--json")
	g.addDump(ctx, snap, "secrets", "secret", "list", "--json")
	snap["conversations"] = g.conversations(ctx)
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(g.p.Snapshot, data, 0o644)
}

// conversations lists every conversation with its messages inline, so a check
// can assert an assistant reply without knowing the conversation id.
func (g *Grader) conversations(ctx context.Context) []map[string]any {
	out, err := g.env.trebi(ctx, "conversation", "list", "--json", "--limit", "50")
	if err != nil {
		return nil
	}
	var list struct {
		Conversations []map[string]any `json:"conversations"`
	}
	if json.Unmarshal(out, &list) != nil {
		return nil
	}
	for _, conv := range list.Conversations {
		id, _ := conv["id"].(string)
		if id == "" {
			continue
		}
		msgs, err := g.env.trebi(ctx, "conversation", "messages", id, "--json", "--limit", "50")
		if err != nil {
			continue
		}
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if json.Unmarshal(msgs, &body) == nil {
			conv["messages"] = body.Messages
		}
	}
	return list.Conversations
}

func (g *Grader) addDump(ctx context.Context, snap map[string]any, key string, args ...string) {
	out, err := g.env.trebi(ctx, args...)
	if err != nil {
		snap[key] = map[string]any{"error": err.Error(), "stdout": string(out)}
		return
	}
	var v any
	if json.Unmarshal(out, &v) != nil {
		snap[key] = string(out)
		return
	}
	snap[key] = v
}

// resolveWanted finds a file the agent was asked to write. A prompt names a
// bare file name in the working directory, but a workspace is rooted one level
// down, so the trebi file store writes it there. Only file_exists_any opts
// into this search: an exact check must stay exact, or a file in the wrong
// folder would pass.
func resolveWanted(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	dir, base := filepath.Split(path)
	if dir == "" || base == "" {
		return path
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return path
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cand := filepath.Join(dir, e.Name(), base)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return path
}

func fileStatCheck(name, path string, wantExist bool) CheckResult {
	_, err := os.Stat(path)
	exists := err == nil
	if exists == wantExist {
		return CheckResult{Name: name, Pass: true}
	}
	if wantExist {
		return CheckResult{Name: name, Detail: "missing " + path}
	}
	return CheckResult{Name: name, Detail: "exists " + path}
}

func fileTextCheck(name, path, text string, want bool) CheckResult {
	data, err := os.ReadFile(path)
	if err != nil {
		return CheckResult{Name: name, Detail: err.Error()}
	}
	has := bytes.Contains(data, []byte(text))
	if has == want {
		return CheckResult{Name: name, Pass: true}
	}
	if want {
		return CheckResult{Name: name, Detail: fmt.Sprintf("%s missing %q", path, text)}
	}
	return CheckResult{Name: name, Detail: fmt.Sprintf("%s contains %q", path, text)}
}

// fileMatchCheck matches a file against a Go regexp. A bad pattern is infra.
func fileMatchCheck(name, path, pattern string) CheckResult {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return CheckResult{Name: name, Infra: true, Detail: err.Error()}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return CheckResult{Name: name, Detail: err.Error()}
	}
	if re.Match(data) {
		return CheckResult{Name: name, Pass: true}
	}
	return CheckResult{Name: name, Detail: fmt.Sprintf("%s does not match %q:\n%s", path, pattern, truncate(data, 400))}
}

// writeCommands saves the command projection next to the transcript, so
// scripts/eval.py why can show the same list commands_jq graded.
func writeCommands(artDir string, transcript []byte) error {
	cmds, err := transcriptCommands(transcript)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cmds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(artDir, "commands.json"), data, 0o644)
}

// transcriptCommands projects one string per agent action from a normalized
// transcript, so a commands_jq pattern matches on either surface.
//
// Shell calls contribute content.command verbatim (codex names the tool
// "command", claude names it "Bash"). MCP calls contribute a CLI-shaped
// rendering, "trebi <tool> <action> --<field> <value>", so that a pattern
// written for the CLI ("runs message", "--if-match") also matches the MCP
// call that does the same work.
func transcriptCommands(data []byte) ([]string, error) {
	var steps []Step
	if err := json.Unmarshal(data, &steps); err != nil {
		return nil, err
	}
	cmds := make([]string, 0, len(steps))
	for _, s := range steps {
		if s.Role != "tool" || s.Content == "" {
			continue
		}
		if cmd := s.shellCommand(); cmd != "" {
			cmds = append(cmds, cmd)
			continue
		}
		if cmd := s.mcpCommand(); cmd != "" {
			cmds = append(cmds, cmd)
		}
	}
	return cmds, nil
}

// cliAliases gives the CLI spellings of one MCP tool+action, for tools the
// CLI names differently. Empty when the MCP spelling is already the CLI one.
func cliAliases(tool, action string) []string {
	switch tool {
	case "run":
		return []string{"runs " + action}
	case "event":
		switch action {
		case "rules_test", "rules_fire":
			return []string{"rules " + strings.TrimPrefix(action, "rules_")}
		}
		return []string{"event " + action, "events " + action}
	case "system":
		switch action {
		case "doctor", "doctor_fix":
			return []string{"doctor"}
		case "bus_poll":
			return []string{"bus"}
		case "logs":
			return []string{"service logs"}
		case "version":
			return []string{"service status"}
		case "overview":
			return []string{"status"}
		case "guide", "guides":
			return []string{"guide"}
		case "cloud_status", "cloud_link", "cloud_unlink":
			return []string{"service cloud " + strings.TrimPrefix(action, "cloud_")}
		}
	}
	// A renamed action keeps its old CLI spelling as an alias.
	if old, ok := renamedActions[action]; ok {
		return []string{tool + " " + old}
	}
	return nil
}

// renamedActions maps a canonical action to the CLI alias it replaced (A4).
var renamedActions = map[string]string{ //nolint:gochecknoglobals // static rename table, never mutated
	"create":        "add",
	"delete":        "remove",
	"memory_delete": "memory remove",
	"refresh":       "update",
	"logs":          "log",
}

func jqTruthy(expr string, data []byte) (ok bool, detail string, err error) {
	if _, look := exec.LookPath("jq"); look != nil {
		return false, "", fmt.Errorf("jq not on PATH")
	}
	cmd := exec.Command("jq", "-e", expr)
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// 1 is a false result; 5 is a runtime error against the data (for
		// example iterating a null the agent never created). Both say the
		// check did not hold. Anything else is a broken expression or a
		// missing jq, which is infra.
		if ee, ok := err.(*exec.ExitError); ok && (ee.ExitCode() == 1 || ee.ExitCode() == 5) {
			return false, fmt.Sprintf("jq %s => %s", expr, bytes.TrimSpace(out)), nil
		}
		return false, "", fmt.Errorf("jq %s: %w\n%s", expr, err, out)
	}
	return true, "", nil
}

func splitCommand(cmd string) []string {
	// Shell-style split that keeps quoted segments intact.
	var args []string
	var b strings.Builder
	quote := rune(0)
	flush := func() {
		if b.Len() == 0 {
			return
		}
		args = append(args, b.String())
		b.Reset()
	}
	for _, r := range cmd {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '\'' || r == '"'):
			quote = r
		case quote == 0 && r == ' ':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return args
}
