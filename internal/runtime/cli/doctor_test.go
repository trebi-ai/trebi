package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorFindingsFlagProbe pins the Part J posture line: runtime kind,
// version, flag probe, and the entitlement sentence.
func TestDoctorFindingsFlagProbe(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := `#!/bin/sh
case "$1" in
  --version) echo "2.1.267 (Claude Code)";;
  --help) echo "  --output-format <format>"; echo "  --permission-prompt-tool <tool>";;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	t.Setenv("TREBI_HOME", t.TempDir())

	findings := DoctorFindings(context.Background())
	var claude *Finding
	for i := range findings {
		if findings[i].ID == "harness-runtime-claude" {
			claude = &findings[i]
		}
	}
	if claude == nil {
		t.Fatalf("no claude finding: %+v", findings)
	}
	for _, want := range []string{"runtime cli", "2.1.267", "--output-format ok", "vendor may meter"} {
		if !strings.Contains(claude.Message, want) {
			t.Fatalf("finding missing %q: %s", want, claude.Message)
		}
	}
	if claude.Severity != "info" {
		t.Fatalf("severity: %s", claude.Severity)
	}
}

// TestDoctorFindingsTUIOnly pins the Part J warning for harnesses without a
// proven wire (Grok): the pane is the only runtime and the doctor says so.
func TestDoctorFindingsTUIOnly(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "grok")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'grok 9.9.9'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	t.Setenv("TREBI_HOME", t.TempDir())

	findings := DoctorFindings(context.Background())
	var grok *Finding
	for i := range findings {
		if findings[i].ID == "harness-runtime-grok" {
			grok = &findings[i]
		}
	}
	if grok == nil {
		t.Fatalf("no grok finding: %+v", findings)
	}
	if grok.Code != "harness_runtime_tui_only" || !strings.Contains(grok.Message, "tui only") {
		t.Fatalf("grok finding: %+v", grok)
	}
}

// TestDoctorFindingsKillSwitch reports the incident posture.
func TestDoctorFindingsKillSwitch(t *testing.T) {
	homeDir := t.TempDir()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	t.Setenv("TREBI_HOME", homeDir)
	if err := os.WriteFile(filepath.Join(homeDir, "config.yaml"), []byte("harness:\n  runtime: tui\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := DoctorFindings(context.Background())
	var claude *Finding
	for i := range findings {
		if findings[i].ID == "harness-runtime-claude" {
			claude = &findings[i]
		}
	}
	if claude == nil || claude.Severity != "warn" || claude.Code != "harness_runtime_tui" {
		t.Fatalf("kill switch finding: %+v", claude)
	}
}
