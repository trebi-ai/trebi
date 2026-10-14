package procgroup

import (
	"context"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunTimeoutKillsPipeline proves a timeout reaches the pipeline children.
func TestRunTimeoutKillsPipeline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Command{Script: "sleep 30 | cat"}.Run(ctx, 1024)
	if err == nil {
		t.Fatal("want an error after the timeout")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Run returned after %v", d)
	}
}

func TestRunCapsOutput(t *testing.T) {
	t.Parallel()
	out, err := Command{Script: "printf abcdef; printf ghij >&2", Stdin: strings.NewReader("")}.Run(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "abcd" {
		t.Fatalf("out = %q", out)
	}
}

// TestKillReachesGroup proves Kill ends a child that holds stdout open.
func TestKillReachesGroup(t *testing.T) {
	t.Parallel()
	cmd, stdout, _, err := Command{Script: "sleep 30 | cat"}.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stdout) // EOF when every writer is gone
		_ = cmd.Wait()                     // the kill makes the exit non-zero
		close(done)
	}()
	if err := Kill(cmd, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the pipeline outlived Kill")
	}
}

// TestRSS sums the memory of the shell and its child, and fails for a pid
// that does not exist.
func TestRSS(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, _, _, err := Command{Script: "sleep 5 | cat"}.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	shell, err := RSS(ctx, cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if shell == 0 {
		t.Fatal("rss = 0")
	}
	if _, err := RSS(ctx, 1<<30); err == nil {
		t.Fatal("want an error for a missing pid")
	}
}
