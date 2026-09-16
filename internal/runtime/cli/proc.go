package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// gracefulCloseTimeout is the flush window between close-stdin, SIGTERM, and
// SIGKILL (A.15). The SDK comment is load-bearing: SIGTERM too early can
// truncate the vendor session file and lose the last assistant message.
const gracefulCloseTimeout = 5 * time.Second

// ProcOpts spawns one child.
type ProcOpts struct {
	Path string
	Args []string
	Dir  string
	// Env is the full child environment (nil inherits os.Environ via exec).
	Env []string
}

// Proc is a spawned child with explicit pipes, process-group control, and a
// single Wait. It is the only lifecycle mechanism for vendor children;
// exec.CommandContext is deliberately not used (A.15).
type Proc struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr *os.File
	pid    int
	pgid   int

	platform procPlatform

	stdinOnce sync.Once
	waitOnce  sync.Once
	waitErr   error
	waitDone  chan struct{}

	exitCode atomic.Int64

	// stderrTail keeps the last chunk of stderr for classification when the
	// process dies without a protocol result.
	stderrMu   sync.Mutex
	stderrTail []byte
}

// shimError reports a refused .cmd/.bat wrapper (Windows, Part D).
type shimError struct{ path string }

func (e *shimError) Error() string {
	return "cli: refusing " + e.path + ": .cmd/.bat shims re-parse the command line; pin a native executable"
}

const stderrTailMax = 32 << 10

// StartProc spawns a child process. The returned Proc has a bounded stderr
// tail reader attached; stdout is the caller's to frame.
func StartProc(ctx context.Context, opts ProcOpts) (*Proc, error) {
	if opts.Path == "" {
		return nil, errors.New("cli: empty command path")
	}
	if err := rejectShim(opts.Path); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}

	cmd := exec.Command(opts.Path, opts.Args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.SysProcAttr = procSysProcAttr()

	if err := cmd.Start(); err != nil {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return nil, fmt.Errorf("start %s: %w", opts.Path, err)
	}
	// Parent copies of the child ends go away now.
	_ = inR.Close()
	_ = outW.Close()
	_ = errW.Close()

	p := &Proc{
		cmd:      cmd,
		stdin:    inW,
		stdout:   outR,
		stderr:   errR,
		pid:      cmd.Process.Pid,
		waitDone: make(chan struct{}),
	}
	p.pgid = processGroupID(cmd.Process.Pid)
	_ = p.platform.afterStart(cmd)
	go p.wait()
	go p.drainStderr()
	return p, nil
}

// wait blocks on the child exactly once and records the exit code.
func (p *Proc) wait() {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		p.waitErr = err
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		p.exitCode.Store(int64(code))
		p.platform.afterWait()
		close(p.waitDone)
	})
}

func (p *Proc) drainStderr() {
	defer p.stderr.Close()
	sc := bufio.NewScanner(p.stderr)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		p.stderrMu.Lock()
		p.stderrTail = append(p.stderrTail, line...)
		p.stderrTail = append(p.stderrTail, '\n')
		if len(p.stderrTail) > stderrTailMax {
			p.stderrTail = p.stderrTail[len(p.stderrTail)-stderrTailMax:]
		}
		p.stderrMu.Unlock()
	}
}

// PID is the child process id.
func (p *Proc) PID() int { return p.pid }

// Pgid is the process group id (Unix; equal to PID on other platforms).
func (p *Proc) Pgid() int { return p.pgid }

// Stdin is the child's stdin writer.
func (p *Proc) Stdin() io.Writer { return p.stdin }

// Stdout is the child's stdout reader.
func (p *Proc) Stdout() io.Reader { return p.stdout }

// CloseStdin closes the write side of the child's stdin once.
func (p *Proc) CloseStdin() error {
	var err error
	p.stdinOnce.Do(func() { err = p.stdin.Close() })
	return err
}

// Wait blocks until the child exits or ctx ends. It returns the wait error and
// the exit code (-1 when ctx ended first).
func (p *Proc) Wait(ctx context.Context) (error, int) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-p.waitDone:
	case <-ctx.Done():
		return ctx.Err(), -1
	}
	return p.waitErr, int(p.exitCode.Load())
}

// WaitTimeout waits up to d for the child to exit. ok=false means still alive.
func (p *Proc) WaitTimeout(d time.Duration) bool {
	if d <= 0 {
		d = gracefulCloseTimeout
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.waitDone:
		return true
	case <-t.C:
		return false
	}
}

// ExitCode returns the code once Wait has completed (-1 before).
func (p *Proc) ExitCode() int {
	select {
	case <-p.waitDone:
		return int(p.exitCode.Load())
	default:
		return -1
	}
}

// Done is closed when the child exits.
func (p *Proc) Done() <-chan struct{} { return p.waitDone }

// Stderr returns the accumulated stderr tail.
func (p *Proc) Stderr() string {
	p.stderrMu.Lock()
	defer p.stderrMu.Unlock()
	return string(p.stderrTail)
}

// KillTree hard-kills the process tree and waits for the child.
func (p *Proc) KillTree() error {
	p.killTreePlatform()
	<-p.waitDone
	return p.waitErr
}

// SignalGroup delivers sig to the whole process group when possible, else to
// the process.
func (p *Proc) SignalGroup(sig os.Signal) { p.signalGroup(sig) }

// gracefulClose is the A.15 sequence: close stdin → wait for flush → SIGTERM →
// wait → SIGKILL, on the process group. timeout is per stage (default 5s).
func (p *Proc) gracefulClose(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = gracefulCloseTimeout
	}
	_ = p.CloseStdin()
	if p.WaitTimeout(timeout) {
		return nil
	}
	p.signalGroup(sigTerm())
	if p.WaitTimeout(timeout) {
		return nil
	}
	p.signalGroup(sigKill())
	<-p.waitDone
	return nil
}
