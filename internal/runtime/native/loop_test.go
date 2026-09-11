package native

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/llm"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/tools"
)

type stub struct {
	mu    sync.Mutex
	calls int
	seq   []llm.Response
	err   error
	reqs  []llm.Request
}

func (s *stub) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	i := s.calls
	s.calls++
	s.mu.Unlock()
	if i >= len(s.seq) {
		return &llm.Response{Text: "done", Stop: llm.StopEndTurn}, nil
	}
	r := s.seq[i]
	return &r, nil
}

type recDisp struct {
	mu     sync.Mutex
	status int
	body   []byte
	last   request.HTTPRequest
	n      int
}

func (f *recDisp) Dispatch(_ context.Context, req request.HTTPRequest) (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.last = req
	return f.status, f.body, nil
}

func TestLoopTextOnly(t *testing.T) {
	t.Parallel()
	var ingested []string
	var finished string
	err := Run(context.Background(), RunOpts{
		Provider: &stub{seq: []llm.Response{{Text: "pong", Stop: llm.StopEndTurn}}},
		Prompt:   "ping",
		Sink: Sink{
			Ingest: func(_ context.Context, texts []string) error {
				ingested = texts
				return nil
			},
			Finish: func(_ context.Context, status, end string, _ llm.Usage) error {
				finished = status + "/" + end
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ingested) != 1 || ingested[0] != "pong" {
		t.Fatalf("%v", ingested)
	}
	if finished != "complete/sentinel" {
		t.Fatalf("%s", finished)
	}
}

func TestLoopBashTool(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"command": "pwd"})
	var events []string
	err := Run(context.Background(), RunOpts{
		Provider: &stub{seq: []llm.Response{
			{Stop: llm.StopToolUse, ToolCalls: []llm.ToolCall{{ID: "1", Name: "bash", Arguments: args}}},
			{Text: "ran", Stop: llm.StopEndTurn},
		}},
		Prompt:        "go",
		WorkspaceRoot: ws,
		WorkingDir:    ws,
		Sink: Sink{
			AddEvent: func(_ context.Context, kind, _ string, detail any) {
				events = append(events, kind)
				_ = detail
			},
			Ingest: func(context.Context, []string) error { return nil },
			Finish: func(context.Context, string, string, llm.Usage) error { return nil },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0] != "tool" {
		t.Fatalf("events %v", events)
	}
}

func TestLoopWriteTool(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"path": "hello.txt", "content": "hi"})
	err := Run(context.Background(), RunOpts{
		Provider: &stub{seq: []llm.Response{
			{Stop: llm.StopToolUse, ToolCalls: []llm.ToolCall{{ID: "1", Name: "write", Arguments: args}}},
			{Text: "wrote it", Stop: llm.StopEndTurn},
		}},
		Prompt:        "write",
		WorkspaceRoot: ws,
		WorkingDir:    ws,
		Sink: Sink{
			AddEvent: func(context.Context, string, string, any) {},
			Ingest:   func(context.Context, []string) error { return nil },
			Finish:   func(context.Context, string, string, llm.Usage) error { return nil },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(ws, "hello.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("%s %v", got, err)
	}
}

func TestLoopCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	p := &blockProvider{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOpts{
			Provider: p,
			Prompt:   "x",
			Sink:     Sink{Finish: func(context.Context, string, string, llm.Usage) error { return nil }},
		})
	}()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider never called")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit")
	}
}

func TestLoop401(t *testing.T) {
	t.Parallel()
	var end string
	err := Run(context.Background(), RunOpts{
		Provider: &stub{err: llm.ErrUnauthorized},
		Prompt:   "x",
		Sink: Sink{Finish: func(_ context.Context, _, e string, _ llm.Usage) error {
			end = e
			return nil
		}},
	})
	if !errors.Is(err, llm.ErrUnauthorized) {
		t.Fatalf("%v", err)
	}
	if end != "auth" {
		t.Fatalf("end=%s", end)
	}
}

type blockProvider struct {
	started chan struct{}
}

func (p *blockProvider) Chat(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	close(p.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestPathEscapeIsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got := runRead(context.Background(), RunOpts{WorkspaceRoot: dir}, "../secret")
	if !strings.Contains(got, "error:") {
		t.Fatalf("%s", got)
	}
}

func TestLoopCatalogRunList(t *testing.T) {
	t.Parallel()
	d := &recDisp{status: 200, body: []byte(`{"runs":[]}`)}
	reg := tools.NewNativeRegistry(d)
	args, _ := json.Marshal(map[string]any{"action": "list", "input": map[string]any{}})
	p := &stub{seq: []llm.Response{
		{Stop: llm.StopToolUse, ToolCalls: []llm.ToolCall{{ID: "1", Name: "run", Arguments: args}}},
		{Text: "none", Stop: llm.StopEndTurn},
	}}
	var events []map[string]any
	err := Run(context.Background(), RunOpts{
		Provider: p,
		Prompt:   "list runs",
		RunID:    "01RUN",
		Registry: reg,
		Sink: Sink{
			AddEvent: func(_ context.Context, _ string, _ string, detail any) {
				if m, ok := detail.(map[string]any); ok {
					events = append(events, m)
				}
			},
			Ingest: func(context.Context, []string) error { return nil },
			Finish: func(context.Context, string, string, llm.Usage) error { return nil },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.n != 1 || d.last.Method != "GET" || !strings.HasPrefix(d.last.Path, "/v1/runs") {
		t.Fatalf("dispatch n=%d %s %s", d.n, d.last.Method, d.last.Path)
	}
	if len(p.reqs) < 2 {
		t.Fatalf("provider calls %d", len(p.reqs))
	}
	found := false
	for _, m := range p.reqs[1].Messages {
		if m.Role == "tool" && strings.Contains(m.Content, `"runs"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool result missing from follow-up: %+v", p.reqs[1].Messages)
	}
	if len(events) == 0 || events[0]["tool"] != "run" || events[0]["action"] != "list" {
		t.Fatalf("events %+v", events)
	}
	hasRun, hasTrebi := false, false
	for _, tl := range p.reqs[0].Tools {
		if tl.Name == "run" {
			hasRun = true
		}
		if tl.Name == "trebi" {
			hasTrebi = true
		}
	}
	if !hasRun || hasTrebi {
		t.Fatalf("tools run=%v trebi=%v", hasRun, hasTrebi)
	}
}

func TestLoopConfirmNotRetried(t *testing.T) {
	t.Parallel()
	envelope := []byte(`{"error":{"code":"validation","message":"this call deletes job x; repeat it with confirm=1 once the user agrees","details":{"confirm_required":true}}}`)
	d := &recDisp{status: 400, body: envelope}
	reg := tools.NewNativeRegistry(d)
	args, _ := json.Marshal(map[string]any{"action": "delete", "input": map[string]any{"name": "x"}})
	p := &stub{seq: []llm.Response{
		{Stop: llm.StopToolUse, ToolCalls: []llm.ToolCall{{ID: "1", Name: "job", Arguments: args}}},
		{Text: "need your OK", Stop: llm.StopEndTurn},
	}}
	err := Run(context.Background(), RunOpts{
		Provider: p,
		Prompt:   "delete it",
		Registry: reg,
		Sink: Sink{
			AddEvent: func(context.Context, string, string, any) {},
			Ingest:   func(context.Context, []string) error { return nil },
			Finish:   func(context.Context, string, string, llm.Usage) error { return nil },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.n != 1 {
		t.Fatalf("dispatched %d times; want 1 (no confirm retry)", d.n)
	}
	if p.calls != 2 {
		t.Fatalf("provider calls %d; want 2", p.calls)
	}
	got := p.reqs[1].Messages[len(p.reqs[1].Messages)-1].Content
	if !strings.Contains(got, "confirm_required") {
		t.Fatalf("tool result %s", got)
	}
}
