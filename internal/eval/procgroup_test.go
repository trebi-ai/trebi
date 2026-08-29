package eval

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestRunWithGroupCancel(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := runWithGroup(ctx, exec.Command("sleep", "10"))
	if err == nil {
		t.Fatal("want cancel error")
	}
	if ctx.Err() == nil {
		t.Fatal("expected ctx done")
	}
}

func TestRunWithGroupOK(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("true not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runWithGroup(ctx, exec.Command("true")); err != nil {
		t.Fatal(err)
	}
}
