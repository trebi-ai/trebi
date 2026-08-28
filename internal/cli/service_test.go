package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLineHasLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line string
		want string
		ok   bool
	}{
		{`{"time":"t","level":"INFO","msg":"hi"}`, "INFO", true},
		{`{"time":"t","level":"info","msg":"hi"}`, "INFO", true},
		{`{"time":"t","level":"ERROR","msg":"x"}`, "INFO", false},
		{`plain INFO line`, "INFO", true},
		{`plain warn line`, "WARN", true},
		{`nothing`, "ERROR", false},
	}
	for _, tc := range cases {
		if got := lineHasLevel(tc.line, tc.want); got != tc.ok {
			t.Fatalf("lineHasLevel(%q, %q)=%v want %v", tc.line, tc.want, got, tc.ok)
		}
	}
}

func TestStreamServiceLog_Filters(t *testing.T) {
	t.Parallel()
	in := strings.Join([]string{
		`{"level":"DEBUG","msg":"skip"}`,
		`{"level":"INFO","msg":"keep-me"}`,
		`{"level":"ERROR","msg":"other"}`,
		``,
		`noise without level`,
	}, "\n")
	var buf bytes.Buffer
	streamServiceLog(&buf, strings.NewReader(in), "info", "keep", false)
	got := buf.String()
	if !strings.Contains(got, "keep-me") {
		t.Fatalf("expected keep-me, got %q", got)
	}
	if strings.Contains(got, "skip") || strings.Contains(got, "other") || strings.Contains(got, "noise") {
		t.Fatalf("unexpected lines: %q", got)
	}
}

func TestStreamServiceLog_GrepOnly(t *testing.T) {
	t.Parallel()
	in := "alpha\nbeta line\ngamma\n"
	var buf bytes.Buffer
	streamServiceLog(&buf, strings.NewReader(in), "", "BETA", false)
	if got := strings.TrimSpace(buf.String()); got != "beta line" {
		t.Fatalf("got %q", got)
	}
}

func TestServiceCommandRegistration(t *testing.T) {
	t.Parallel()
	app := NewApp()
	svc := findCmd(app.Commands, "service")
	if svc == nil {
		t.Fatal("service missing")
	}
	for _, want := range []string{
		"install", "uninstall", "start", "stop", "restart",
		"status", "logs", "migrate-db", "daemon", "drain", "undrain", "token",
	} {
		if findCmd(svc.Subcommands, want) == nil {
			t.Fatalf("service missing %s", want)
		}
	}
	inst := findCmd(svc.Subcommands, "install")
	hasBin, hasNoTray := false, false
	for _, f := range inst.Flags {
		switch f.Names()[0] {
		case "bin":
			hasBin = true
		case "no-tray":
			hasNoTray = true
		}
	}
	if !hasBin || !hasNoTray {
		t.Fatalf("install flags: bin=%v no-tray=%v", hasBin, hasNoTray)
	}
	logs := findCmd(svc.Subcommands, "logs")
	for _, name := range []string{"follow", "lines", "level", "grep", "json"} {
		found := false
		for _, f := range logs.Flags {
			if f.Names()[0] == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("logs missing flag %s", name)
		}
	}
}

func TestSetLogLevel(t *testing.T) {
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	for _, name := range []string{"", "info", "debug", "DEBUG", " warn ", "error"} {
		if err := setLogLevel(name); err != nil {
			t.Errorf("setLogLevel(%q): %v", name, err)
		}
	}
	if err := setLogLevel("bogus"); err == nil {
		t.Fatal("want error for bogus level")
	}
	// debug must actually enable debug records
	if err := setLogLevel("debug"); err != nil {
		t.Fatal(err)
	}
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug not enabled")
	}
	if err := setLogLevel("info"); err != nil {
		t.Fatal(err)
	}
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug still enabled at info")
	}
}
