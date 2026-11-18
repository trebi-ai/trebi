package eval

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/waittest"
)

func TestDaemonHeadlessForeignHome(t *testing.T) {
	bin := lookTrebi(t)
	art := t.TempDir()
	home := filepath.Join(art, "home")
	work := filepath.Join(art, "work")
	env := newTrialEnv(home, work, art, bin)
	ctx, cancel := context.WithTimeout(context.Background(), waittest.D(30*time.Second))
	defer cancel()
	if err := env.startDaemon(ctx, "codex", work); err != nil {
		t.Fatal(err)
	}
	defer env.stop(context.Background())
	if env.apiURL == "" {
		t.Fatal("empty api url")
	}
	out, err := env.trebi(ctx, "doctor", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["api_json_reachable"] != true {
		t.Fatalf("doctor %s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "api.json")); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonExitsWhenParentPipeCloses(t *testing.T) {
	bin := lookTrebi(t)
	art := t.TempDir()
	env := newTrialEnv(filepath.Join(art, "home"), filepath.Join(art, "work"), art, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := env.startDaemon(ctx, "codex", env.workdir); err != nil {
		t.Fatal(err)
	}
	if env.parentW == nil || env.cmd == nil || env.cmd.Process == nil {
		t.Fatal("parent pipe or daemon missing")
	}
	if err := env.parentW.Close(); err != nil {
		t.Fatal(err)
	}
	env.parentW = nil
	done := make(chan error, 1)
	go func() { done <- env.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(waittest.D(5 * time.Second)):
		env.kill()
		t.Fatal("daemon did not exit after parent pipe close")
	}
}

func lookTrebi(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("TREBI_BIN"); p != "" {
		return p
	}
	_, file, _, _ := runtime.Caller(0)
	cand := filepath.Join(filepath.Dir(file), "..", "..", "build", "trebi")
	if _, err := os.Stat(cand); err == nil {
		abs, _ := filepath.Abs(cand)
		return abs
	}
	p, err := exec.LookPath("trebi")
	if err != nil {
		t.Skip("trebi binary not found")
	}
	return p
}
