package tray

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestRouteFromLink(t *testing.T) {
	t.Parallel()
	tests := []struct {
		link, want string
		err        bool
	}{
		{link: "trebi://", want: "/"},
		{link: "trebi://run/abc?tab=session&request=r1", want: "/run/abc?tab=session&request=r1"},
		{link: "trebi://inbox", want: "/inbox"},
		{link: "trebi://inbox/", want: "/inbox"},
		{link: "trebi://../x", want: "/x"},
		{link: "trebi:run/abc", want: "/run/abc"},
		{link: "http://x", err: true},
		{link: "ai.trebi.shell://open", err: true},
	}
	for _, tt := range tests {
		got, err := RouteFromLink(tt.link)
		if (err != nil) != tt.err {
			t.Errorf("%s: err %v, want err %v", tt.link, err, tt.err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s: %q, want %q", tt.link, got, tt.want)
		}
	}
}

func TestShellOpenURL(t *testing.T) {
	t.Parallel()
	raw := Shell{}.OpenURL("http://127.0.0.1:7433", "/run/abc?tab=session&request=r1")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != ShellScheme || u.Host != "open" {
		t.Fatalf("url %q", raw)
	}
	q := u.Query()
	if q.Get("base") != "http://127.0.0.1:7433" || q.Get("path") != "/run/abc?tab=session&request=r1" {
		t.Fatalf("query %v", q)
	}
}

func TestNewShell(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exe := filepath.Join(dir, "Trebi.app", "Contents", "MacOS", "trebi")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if NewShell(exe).Available() {
		t.Fatal("shell found in a bundle with no helper")
	}
	if err := os.MkdirAll(filepath.Join(dir, "Trebi.app", "Contents", "Helpers", "TrebiShell.app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !NewShell(exe).Available() {
		t.Fatal("shell not found")
	}
	if NewShell(filepath.Join(dir, "trebi")).Available() {
		t.Fatal("shell found outside a bundle")
	}
}

func TestShellProgressURL(t *testing.T) {
	t.Parallel()
	for _, step := range []StartStep{StepInstall, StepEngine, StepSetup} {
		if got, want := (Shell{}).ProgressURL(step), "ai.trebi.shell://progress?step="+string(step); got != want {
			t.Errorf("%s: got %q, want %q", step, got, want)
		}
	}
	if err := (Shell{}).Progress(StepInstall); err == nil {
		t.Error("Progress without a shell must fail")
	}
}
