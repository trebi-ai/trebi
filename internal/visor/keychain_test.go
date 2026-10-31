package visor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeService(t *testing.T) {
	t.Parallel()
	if got := claudeService("/Users/fritz/.trebi/harness/claude"); got != "Claude Code-credentials-60df8e7b" {
		t.Fatalf("service %q", got)
	}
}

func TestClaudeKeychainMove(t *testing.T) {
	t.Setenv("USER", "someone")
	oldDir, newDir := "/h/.trebi/harness/claude", "/h/.trebi/visor/harness/claude"
	oldSvc, newSvc := claudeService(oldDir), claudeService(newDir)
	bin, log := fakeSecurity(t, oldSvc, "someone")
	if err := (claudeKeychain{security: bin}).move(context.Background(), oldDir, newDir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"find-generic-password -s " + oldSvc + " -a someone -w",
		"add-generic-password -U -s " + newSvc + " -a someone -w the-value",
		"delete-generic-password -s " + oldSvc + " -a someone",
	}
	if got := strings.Split(strings.TrimSpace(string(data)), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestClaudeKeychainMoveMissing(t *testing.T) {
	t.Setenv("USER", "someone")
	bin, log := fakeSecurity(t, "no such item", "someone")
	if err := (claudeKeychain{security: bin}).move(context.Background(), "/a/claude", "/b/claude"); err != nil {
		t.Fatalf("a missing item must not be an error: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "add-generic-password") || strings.Contains(string(data), "delete-generic-password") {
		t.Fatalf("wrote with no old item:\n%s", data)
	}
}

// fakeSecurity writes a security program that logs its arguments and knows
// one item.
func fakeSecurity(t *testing.T, service, acct string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$@" >> "` + log + `"
if [ "$1" = find-generic-password ]; then
  if [ "$3" = "` + service + `" ] && [ "$5" = "` + acct + `" ]; then echo 'the-value'; exit 0; fi
  exit 44
fi
exit 0
`
	bin := filepath.Join(dir, "security")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // the fake must run
		t.Fatal(err)
	}
	return bin, log
}
