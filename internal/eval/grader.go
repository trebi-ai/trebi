package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	case c.FileAbsent != "":
		return fileStatCheck(c.Name, c.FileAbsent, false)
	case c.FileContains != "":
		return fileTextCheck(c.Name, c.FileContains, c.Text, true)
	case c.FileNotContains != "":
		return fileTextCheck(c.Name, c.FileNotContains, c.Text, false)
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
	c.FileAbsent = g.p.Expand(c.FileAbsent)
	c.FileContains = g.p.Expand(c.FileContains)
	c.FileNotContains = g.p.Expand(c.FileNotContains)
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
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(g.p.Snapshot, data, 0o644)
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

// transcriptCommands projects command strings from a normalized transcript.
// Codex names the tool "command"; Claude names it "Bash". Both store the
// line in content.command. Other tools without that field are ignored.
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
		var body struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(s.Content), &body) != nil || body.Command == "" {
			continue
		}
		cmds = append(cmds, body.Command)
	}
	return cmds, nil
}

func jqTruthy(expr string, data []byte) (ok bool, detail string, err error) {
	if _, look := exec.LookPath("jq"); look != nil {
		return false, "", fmt.Errorf("jq not on PATH")
	}
	cmd := exec.Command("jq", "-e", expr)
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
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
