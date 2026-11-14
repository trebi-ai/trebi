package onboarding_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/onboarding"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

type fakeHarnesses struct {
	loggedIn []string
	turnedOn []string
}

func (f *fakeHarnesses) LoggedIn(context.Context) ([]string, error) { return f.loggedIn, nil }

func (f *fakeHarnesses) TurnOn(_ context.Context, name string) error {
	f.turnedOn = append(f.turnedOn, name)
	return nil
}

func TestApplyDefaultsHarness(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		loggedIn []string
		choose   bool
		harness  string
	}{
		{name: "none", choose: true},
		{name: "one", loggedIn: []string{"claude"}, harness: "claude"},
		{name: "two", loggedIn: []string{"claude", "codex"}, choose: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, h := applyDefaults(t, tc.loggedIn...)
			if out.Choose != tc.choose || out.Harness != tc.harness {
				t.Fatalf("defaults = %+v", out)
			}
			wantOn := 0
			if tc.harness != "" {
				wantOn = 1
			}
			if len(h.turnedOn) != wantOn || (wantOn == 1 && h.turnedOn[0] != tc.harness) {
				t.Fatalf("turned on %v", h.turnedOn)
			}
		})
	}
}

func TestApplyDefaultsWorkspaceUnderHome(t *testing.T) {
	t.Parallel()
	out, _ := applyDefaults(t)
	home, _ := os.UserHomeDir()
	if out.Workspace == nil || out.Workspace.Name != "Personal" {
		t.Fatalf("workspace = %+v", out.Workspace)
	}
	if want := filepath.Join(home, "trebi", "Personal"); out.Workspace.Path != want {
		t.Fatalf("path = %s, want %s", out.Workspace.Path, want)
	}
	if !strings.HasPrefix(out.Workspace.Path, home) {
		t.Fatalf("path %s is outside the test home %s", out.Workspace.Path, home)
	}
	if info, err := os.Stat(out.Workspace.Path); err != nil || !info.IsDir() {
		t.Fatalf("folder: %v", err)
	}
}

func TestApplyDefaultsIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	home, _ := os.UserHomeDir()
	ws := workspaces.New(st).WithRoot(filepath.Join(home, "trebi"))
	svc := onboarding.New(st)
	h := &fakeHarnesses{}

	first, err := svc.ApplyDefaults(ctx, ws, h)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.ApplyDefaults(ctx, ws, h)
	if err != nil {
		t.Fatal(err)
	}
	list, err := ws.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("workspaces = %d %v", len(list), err)
	}
	if first.Workspace.ID != second.Workspace.ID {
		t.Fatalf("ids differ: %s %s", first.Workspace.ID, second.Workspace.ID)
	}
	if got, _ := svc.Get(ctx); got.WorkspaceID != first.Workspace.ID {
		t.Fatalf("state workspace = %q", got.WorkspaceID)
	}

	// A workspace that the user deletes comes back at the next run of setup.
	if err := ws.Remove(ctx, first.Workspace.ID); err != nil {
		t.Fatal(err)
	}
	third, err := svc.ApplyDefaults(ctx, ws, h)
	if err != nil || third.Workspace == nil {
		t.Fatalf("after remove: %+v %v", third, err)
	}
}

func applyDefaults(t *testing.T, loggedIn ...string) (onboarding.Defaults, *fakeHarnesses) {
	t.Helper()
	st := openTest(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	ws := workspaces.New(st).WithRoot(filepath.Join(home, "trebi"))
	h := &fakeHarnesses{loggedIn: loggedIn}
	out, err := onboarding.New(st).ApplyDefaults(context.Background(), ws, h)
	if err != nil {
		t.Fatal(err)
	}
	return out, h
}
