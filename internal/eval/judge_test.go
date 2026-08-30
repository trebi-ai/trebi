package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJudgePreflight(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		wantErr string
	}{
		{"ok", `echo '{"ok":true}'`, ""},
		{"empty reply", `echo ""`, "empty reply"},
		{"quota error on stdout", `echo "usage limit reached"; exit 1`, "usage limit reached"},
		{"error on stderr", `echo "not logged in" >&2; exit 1`, "not logged in"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubClaude(t, tt.script)
			j := &judgeRunner{model: "claude-sonnet-5"}
			err := j.preflight(context.Background())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("preflight: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("preflight err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// stubClaude puts a fake claude binary on PATH that runs the given script.
func stubClaude(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}
