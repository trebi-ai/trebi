package tray

import (
	"strings"
	"testing"
)

func TestBuildMenuLines_Healthy(t *testing.T) {
	t.Parallel()
	lines := BuildMenuLines(Snapshot{
		Healthy:      true,
		Version:      "0.1.0",
		RunningCount: 2,
		WaitingCount: 1,
	})
	if !strings.Contains(lines[0], "running") || !strings.Contains(lines[0], "0.1.0") {
		t.Fatalf("head: %q", lines[0])
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "waiting: 1") {
		t.Fatalf("want waiting count: %q", joined)
	}
	if lines[len(lines)-1] != "Quit Trebi service" {
		t.Fatalf("last: %q", lines[len(lines)-1])
	}
}

func TestBuildMenuLines_DrainingAndOffline(t *testing.T) {
	t.Parallel()
	d := BuildMenuLines(Snapshot{Healthy: true, Draining: true})
	if !strings.Contains(d[0], "draining") {
		t.Fatalf("draining: %q", d[0])
	}
	o := BuildMenuLines(Snapshot{Healthy: false, Message: "api.json missing"})
	if !strings.Contains(o[0], "offline") {
		t.Fatalf("offline: %q", o[0])
	}
	if !strings.Contains(strings.Join(o, "\n"), "api.json missing") {
		t.Fatalf("message: %v", o)
	}
}

func TestItoa(t *testing.T) {
	t.Parallel()
	if itoa(0) != "0" || itoa(42) != "42" || itoa(-3) != "-3" {
		t.Fatalf("itoa broken")
	}
}
