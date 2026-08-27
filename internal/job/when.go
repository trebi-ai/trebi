package job

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
)

// WhenResult is the outcome of a schedule when: gate.
type WhenResult struct {
	Fire   bool
	Skip   bool
	Vars   map[string]string
	Error  string
	Stdout string
}

// EvalWhen runs the shell gate and JS match/extract (B25).
func EvalWhen(ctx context.Context, w *WhenYAML, cwd string, env map[string]string) WhenResult {
	if w == nil || strings.TrimSpace(w.Match) == "" {
		return WhenResult{Fire: true}
	}
	cctx, cancel := context.WithTimeout(ctx, w.gateTimeout())
	defer cancel()

	var stdout, stderr bytes.Buffer
	exitCode := 0
	if runCmd := w.command(); runCmd != "" {
		var skip *WhenResult
		exitCode, skip = w.runGate(cctx, runCmd, cwd, env, &stdout, &stderr)
		if skip != nil {
			return *skip
		}
	}
	event := map[string]any{
		"stdout": stdout.String(),
		"stderr": stderr.String(),
		"exit":   exitCode,
		"lines":  strings.Split(stdout.String(), "\n"),
	}
	res := dispatch.EvalMatch(cctx, w.Match, w.Extract, event)
	if res.Error != "" {
		return WhenResult{Error: res.Error, Skip: true, Stdout: stdout.String()}
	}
	if !res.Matched {
		return WhenResult{Skip: true, Stdout: stdout.String(), Vars: res.Vars}
	}
	return WhenResult{Fire: true, Vars: res.Vars, Stdout: stdout.String()}
}

// gateTimeout returns the configured timeout or the 30s default.
func (w *WhenYAML) gateTimeout() time.Duration {
	timeout := 30 * time.Second
	if w.Timeout != "" {
		if d, err := time.ParseDuration(w.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}
	return timeout
}

// command renders run: (string or list) as one shell command.
func (w *WhenYAML) command() string {
	switch v := w.Run.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, p := range v {
			parts = append(parts, fmt.Sprint(p))
		}
		return strings.Join(parts, " ")
	default:
		if w.Run != nil {
			return fmt.Sprint(w.Run)
		}
		return ""
	}
}

// runGate runs the shell command into stdout/stderr. A non-nil result means
// the gate decided to skip; otherwise the exit code is allowed.
func (w *WhenYAML) runGate(ctx context.Context, runCmd, cwd string, env map[string]string, stdout, stderr *bytes.Buffer) (int, *WhenResult) {
	cmd := exec.CommandContext(ctx, "sh", "-c", runCmd)
	if w.Cwd != "" {
		cmd.Dir = w.Cwd
	} else if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range w.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	exitCode := 0
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else if ctx.Err() != nil {
			return exitCode, &WhenResult{Error: "when: timeout", Skip: true}
		} else {
			return exitCode, &WhenResult{Error: err.Error(), Skip: true}
		}
	}
	allowed := map[int]bool{0: true}
	for _, c := range w.AllowExit {
		allowed[c] = true
	}
	if !allowed[exitCode] {
		return exitCode, &WhenResult{Skip: true, Stdout: stdout.String(), Error: fmt.Sprintf("exit %d", exitCode)}
	}
	return exitCode, nil
}
