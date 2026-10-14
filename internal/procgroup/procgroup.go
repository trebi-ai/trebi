// Package procgroup runs `sh -c` commands in their own process group, so a
// timeout or a stop reaches every child of the shell, not only the shell.
package procgroup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

const defaultWaitDelay = 2 * time.Second

// Command runs `sh -c Script` in its own process group.
type Command struct {
	Script    string
	Dir       string
	Env       []string
	Stdin     io.Reader
	WaitDelay time.Duration // zero means 2s
}

// Start starts the command with stdout and stderr pipes. When ctx ends, the
// group gets SIGTERM, then SIGKILL after half of WaitDelay.
func (c Command) Start(ctx context.Context) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	cmd := c.command(ctx)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdout, stderr, nil
}

// Run runs the command to its end and returns the combined output. The
// output keeps the first max bytes; the rest is read and dropped.
func (c Command) Run(ctx context.Context, max int) ([]byte, error) {
	cmd := c.command(ctx)
	out := &capped{max: max}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	return out.buf.Bytes(), err
}

func (c Command) command(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", c.Script)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin = c.Stdin
	cmd.WaitDelay = c.WaitDelay
	if cmd.WaitDelay <= 0 {
		cmd.WaitDelay = defaultWaitDelay
	}
	setGroup(cmd)
	return cmd
}

// RSS returns the resident memory in bytes of pid and its descendants. The
// started pid is the shell, so the real program is often a child.
func RSS(ctx context.Context, pid int) (uint64, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return 0, err
	}
	var root *process.Process
	kids := map[int32][]*process.Process{}
	for _, p := range procs {
		if int(p.Pid) == pid {
			root = p
			continue
		}
		if ppid, err := p.PpidWithContext(ctx); err == nil {
			kids[ppid] = append(kids[ppid], p)
		}
	}
	if root == nil {
		return 0, fmt.Errorf("process %d not found", pid)
	}
	var total uint64
	for queue := []*process.Process{root}; len(queue) > 0; queue = queue[1:] {
		p := queue[0]
		if m, err := p.MemoryInfoWithContext(ctx); err == nil {
			total += m.RSS
		}
		queue = append(queue, kids[p.Pid]...)
	}
	return total, nil
}

// capped is a writer that keeps the first max bytes. exec serializes the
// writes when Stdout and Stderr are the same writer.
type capped struct {
	buf bytes.Buffer
	max int
}

func (w *capped) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
