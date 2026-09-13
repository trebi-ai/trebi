package runs_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/llm"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

type stubProv struct {
	seq []llm.Response
	n   int
	err error
	ch  chan struct{}
}

func (s *stubProv) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if s.ch != nil {
		close(s.ch)
		s.ch = nil
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.n >= len(s.seq) {
		return &llm.Response{Text: "ok", Stop: llm.StopEndTurn}, nil
	}
	r := s.seq[s.n]
	s.n++
	return &r, nil
}

func nativeSvc(t *testing.T, p llm.Provider) (*runs.Service, context.Context) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	_ = os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("assistant:\n  model: test-model\n  api_key_env: NATIVE_TEST_KEY\n"), 0644)
	t.Setenv("NATIVE_TEST_KEY", "sk-test")
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := runs.New(st, nil)
	svc.SetNativeHooks(runs.NativeHooks{
		Lifetime: context.Background(),
		Provider: p,
	})
	return svc, context.Background()
}

func TestNativeSayCompletes(t *testing.T) {
	svc, ctx := nativeSvc(t, &stubProv{seq: []llm.Response{{Text: "pong", Stop: llm.StopEndTurn}}})
	run, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "assistant", Kind: runs.KindAssistant,
			WorkingDir: t.TempDir(),
		},
		Prompt: "ping",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.WaitFor(ctx, run.ID, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runs.StatusComplete || got.Harness != "native" || got.TmuxSession != "" {
		t.Fatalf("%+v", got)
	}
	if got.Runtime != "native" {
		t.Fatalf("runtime=%s", got.Runtime)
	}
}

func TestNative401(t *testing.T) {
	svc, ctx := nativeSvc(t, &stubProv{err: llm.ErrUnauthorized})
	run, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "assistant", Kind: runs.KindAssistant,
			WorkingDir: t.TempDir(),
		},
		Prompt: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.WaitFor(ctx, run.ID, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.EndReason != runs.EndAuth {
		t.Fatalf("%+v", got)
	}
}

func TestNativeKillCancels(t *testing.T) {
	started := make(chan struct{})
	svc, ctx := nativeSvc(t, &stubProv{ch: started})
	run, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "assistant", Kind: runs.KindAssistant,
			WorkingDir: t.TempDir(),
		},
		Prompt: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("not started")
	}
	if err := svc.Kill(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := svc.WaitFor(ctx, run.ID, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.EndReason != runs.EndKill {
		t.Fatalf("%+v", got)
	}
}

func TestNativeRecoverOrphan(t *testing.T) {
	svc, ctx := nativeSvc(t, &stubProv{})
	run, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "assistant", Kind: runs.KindAssistant,
		Harness: "native", Runtime: "native",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.RecoverNative(ctx)
	got, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EndReason != runs.EndSessionGone || got.Status != runs.StatusExited {
		t.Fatalf("%+v", got)
	}
}

func TestNativeRuntimeTuiSkips(t *testing.T) {
	svc, ctx := nativeSvc(t, &stubProv{seq: []llm.Response{{Text: "nope", Stop: llm.StopEndTurn}}})
	run, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "assistant", Kind: runs.KindAssistant,
			Harness: "fake", Runtime: "tui", WorkingDir: t.TempDir(),
		},
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Harness == "native" {
		t.Fatal("tui escape hatch used native")
	}
}

func TestNativeJobStaysFake(t *testing.T) {
	svc, ctx := nativeSvc(t, &stubProv{seq: []llm.Response{{Text: "nope", Stop: llm.StopEndTurn}}})
	run, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "nightly", Kind: runs.KindJob,
			Harness: "fake", WorkingDir: t.TempDir(),
		},
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Harness == "native" {
		t.Fatal("job used native")
	}
}

func TestNativeBashEvent(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"command": "pwd"})
	svc, ctx := nativeSvc(t, &stubProv{seq: []llm.Response{
		{Stop: llm.StopToolUse, ToolCalls: []llm.ToolCall{{ID: "1", Name: "bash", Arguments: args}}},
		{Text: "done", Stop: llm.StopEndTurn},
	}})
	ws := t.TempDir()
	run, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "assistant", Kind: runs.KindAssistant,
			WorkingDir: ws,
		},
		Prompt: "run",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.WaitFor(ctx, run.ID, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runs.StatusComplete {
		t.Fatalf("%+v", got)
	}
}
