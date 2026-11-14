package cli

import (
	"strings"
	"testing"
)

func TestEnsureVisorPath(t *testing.T) {
	t.Setenv("PATH", "/x/bin")
	svc, err := visorService("/app/trebi")
	if err != nil {
		t.Fatal(err)
	}
	path := svc.Env["PATH"]
	if !strings.HasPrefix(path, "/x/bin:") {
		t.Errorf("PATH %q does not start with the current PATH", path)
	}
	for _, want := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin"} {
		if !strings.Contains(path, want) {
			t.Errorf("PATH %q lacks %s", path, want)
		}
	}
}
