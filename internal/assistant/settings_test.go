package assistant_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func TestSettingsMergeOrder(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{})
	ctx := context.Background()
	wsID := env.workspaceID(t)

	eff, err := env.resolver.Effective(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Source["harness"] != "workspace" {
		t.Fatalf("pinned harness source %q", eff.Source["harness"])
	}
	if eff.Mode != "auto_edit" {
		t.Fatalf("default mode %q", eff.Mode)
	}
	if eff.Name != "sling" {
		t.Fatalf("workspace name fill %q", eff.Name)
	}
	model := "claude-sonnet-5"
	ov := wireOverride("fake")
	ov.Model = &model
	if _, err := env.ws.SetAssistantOverride(ctx, wsID, ov, ""); err != nil {
		t.Fatal(err)
	}
	eff, err = env.resolver.Effective(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Model != model || eff.Source["model"] != "workspace" {
		t.Fatalf("model merge %+v", eff.Source)
	}

	// An empty skills list is a real override: no skills.
	empty := []string{}
	ov.Skills = &empty
	if _, err := env.ws.SetAssistantOverride(ctx, wsID, ov, ""); err != nil {
		t.Fatal(err)
	}
	eff, err = env.resolver.Effective(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eff.Skills) != 0 {
		t.Fatalf("empty skills must stay empty, got %v", eff.Skills)
	}
	if eff.Source["skills"] != "workspace" {
		t.Fatalf("skills source %q", eff.Source["skills"])
	}
}

func TestSettingsValidate(t *testing.T) {
	s := assistant.Settings{Mode: "nope"}
	if err := s.Validate(context.Background(), nil); err == nil {
		t.Fatal("bad mode accepted")
	}
	s = assistant.Settings{Mode: "auto_edit", Effort: "extreme"}
	if err := s.Validate(context.Background(), nil); err == nil {
		t.Fatal("bad effort accepted")
	}
	s = assistant.Settings{Mode: "auto_edit", Harness: "martian"}
	if err := s.Validate(context.Background(), nil); err == nil {
		t.Fatal("bad harness accepted")
	}
	s = assistant.Settings{Mode: "auto_edit", Harness: "native", Skills: []string{"missing"}}
	if err := s.Validate(context.Background(), fixedSkills{"trebi"}); err == nil {
		t.Fatal("unknown skill accepted")
	}
	s.Skills = []string{"trebi"}
	if err := s.Validate(context.Background(), fixedSkills{"trebi"}); err != nil {
		t.Fatal(err)
	}
}

type fixedSkills []string

func (f fixedSkills) SkillNames(context.Context) ([]string, error) {
	return f, nil
}

func TestWorkspaceOverrideIfMatchConflict(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	root := t.TempDir()
	wsSvc := workspaces.New(st).WithRoot(root)
	dir := filepath.Join(root, "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := wsSvc.Add(context.Background(), dir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Hash of the fresh file.
	_, hash, err := wsSvc.AssistantOverride(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	name := "ellis"
	ov := assistant.Override{Name: &name}
	newHash, err := wsSvc.SetAssistantOverride(ctx, ws.ID, ov, hash)
	if err != nil {
		t.Fatal(err)
	}
	if newHash == hash {
		t.Fatal("hash must move on write")
	}
	if _, err := wsSvc.SetAssistantOverride(ctx, ws.ID, ov, hash); !errors.Is(err, workspaces.ErrPrecondition) {
		t.Fatalf("stale write: %v", err)
	}
	got, gotHash, err := wsSvc.AssistantOverride(ctx, ws.ID)
	if err != nil || got.Name == nil || *got.Name != "ellis" || gotHash != newHash {
		t.Fatalf("readback %+v %q %v", got, gotHash, err)
	}
}

func TestNativeDetectReportsKeyState(t *testing.T) {
	t.Setenv("TREBI_TEST_NATIVE_KEY", "")
	cfg := config.NativeConfig{Model: "claude-sonnet-5", APIKeyEnv: "TREBI_TEST_NATIVE_KEY"}
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rt := agentwire.New(agentwire.Options{ClientName: "trebi-test"})
	assistant.RegisterNativeDriver(rt, func() config.NativeConfig { return cfg }, nil, assistant.NewStore(st), 0, nil)

	d, err := rt.Detect(context.Background(), agentwire.Native)
	if err != nil {
		t.Fatal(err)
	}
	if d.Auth != agentwire.AuthMissing || d.Ready {
		t.Fatalf("missing key detection %+v", d)
	}
	t.Setenv("TREBI_TEST_NATIVE_KEY", "sk-test")
	d, err = rt.Detect(context.Background(), agentwire.Native)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Supported || d.Auth != agentwire.AuthOK || !d.Ready {
		t.Fatalf("key present detection %+v", d)
	}
}

// --- plan 2026-09-26 F: the harness picker reports a status ---

// stubDetector answers Detect from a fixed table.
type stubDetector struct {
	dets map[agentwire.Harness]agentwire.Detection
}

func (s stubDetector) Detect(_ context.Context, h agentwire.Harness) (agentwire.Detection, error) {
	if d, ok := s.dets[h]; ok {
		return d, nil
	}
	return agentwire.Detection{Harness: h}, nil
}

// statusOf finds the picker entry of one harness.
func statusOf(t *testing.T, opts []assistant.HarnessOption, name string) assistant.HarnessStatus {
	t.Helper()
	for _, o := range opts {
		if o.Name == name {
			return o.Status
		}
	}
	t.Fatalf("no option for %q in %+v", name, opts)
	return ""
}

func TestHarnessOptionsStatuses(t *testing.T) {
	t.Run("native unconfigured is not_configured", func(t *testing.T) {
		res := assistant.NewResolver(nil, nil, stubDetector{}, nil, assistant.ResolverTiming{})
		opts := res.HarnessOptions(context.Background())
		if got := statusOf(t, opts, "native"); got != assistant.HarnessNotConfigured {
			t.Fatalf("native status = %q, want not_configured", got)
		}
	})
	t.Run("native with model and key is ready", func(t *testing.T) {
		t.Setenv("TREBI_TEST_NATIVE_KEY", "sk-test")
		native := config.NativeConfig{Model: "claude-sonnet-5", APIKeyEnv: "TREBI_TEST_NATIVE_KEY"}
		res := assistant.NewResolver(nil, nil, stubDetector{}, func() config.NativeConfig { return native }, assistant.ResolverTiming{})
		opts := res.HarnessOptions(context.Background())
		if got := statusOf(t, opts, "native"); got != assistant.HarnessReady {
			t.Fatalf("native status = %q, want ready", got)
		}
	})
	t.Run("cli statuses", func(t *testing.T) {
		rt := stubDetector{dets: map[agentwire.Harness]agentwire.Detection{
			agentwire.Claude: {Harness: agentwire.Claude, Supported: true, Path: "/usr/bin/claude", Auth: agentwire.AuthOK, Version: "2.1.0"},
			agentwire.Codex:  {Harness: agentwire.Codex, Supported: true, Path: "/usr/bin/codex", Auth: agentwire.AuthMissing},
			agentwire.Pi:     {Harness: agentwire.Pi, Supported: false, Path: "/usr/bin/pi", Detail: "pi 0.9 is below the supported floor 1.0"},
			agentwire.OpenCode: {
				Harness: agentwire.OpenCode, Detail: "opencode is not installed",
			},
		}}
		res := assistant.NewResolver(nil, nil, rt, nil, assistant.ResolverTiming{})
		opts := res.HarnessOptions(context.Background())
		cases := map[string]assistant.HarnessStatus{
			"claude":   assistant.HarnessReady,
			"codex":    assistant.HarnessLoginRequired,
			"pi":       assistant.HarnessUnsupported,
			"opencode": assistant.HarnessNotInstalled,
		}
		for name, want := range cases {
			if got := statusOf(t, opts, name); got != want {
				t.Fatalf("%s status = %q, want %q", name, got, want)
			}
		}
	})
}
