package tmux

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

// TestOMPLaunchEnv proves shared mode keeps the existing omp home and
// separate mode points the agent directory at the Trebi folder.
func TestOMPLaunchEnv(t *testing.T) {
	isolateHome(t)
	tool := ompTool{harnessHome{tool: OMP}}
	plan := tool.PrepareLaunch("omp")
	if _, ok := plan.Env["PI_CODING_AGENT_DIR"]; ok {
		t.Fatalf("shared plan sets PI_CODING_AGENT_DIR: %v", plan.Env)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tools.SetPin(OMP, &config.ToolPin{Auth: harness.AuthModeSeparate})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	plan = tool.PrepareLaunch("omp")
	if got := plan.Env["PI_CODING_AGENT_DIR"]; got != config.HarnessDir(OMP) {
		t.Fatalf("separate plan PI_CODING_AGENT_DIR = %q, want %q", got, config.HarnessDir(OMP))
	}
	if plan.SessionID != "" {
		t.Fatalf("fresh pane plan has a session id: %q", plan.SessionID)
	}
	resumed, err := tool.PrepareResume("omp", "ses_1")
	if err != nil || resumed.SessionID != "ses_1" || resumed.Command != "omp --resume ses_1" {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
}

// TestOMPProbeAuth proves the vendor model list is the login probe.
func TestOMPProbeAuth(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	script := filepath.Join(dir, OMP)
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$OMP_MODELS\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	t.Setenv("OMP_MODELS", `{"models":[{"id":"a"},{"id":"b"}]}`)
	if st := (ompTool{}).ProbeAuth(context.Background()); st.Status != harness.AuthOK || st.Detail != "2 models" {
		t.Fatalf("logged in: %+v", st)
	}
	t.Setenv("OMP_MODELS", `{"models":[]}`)
	if st := (ompTool{}).ProbeAuth(context.Background()); st.Status != harness.AuthMissing {
		t.Fatalf("logged out: %+v", st)
	}
}
