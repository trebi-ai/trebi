package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/connectors/protocol/fakeadapter"
)

type notice struct {
	method string
	params json.RawMessage
}

// TestFlows drives each flow transcript from the daemon side through Conn.
func TestFlows(t *testing.T) {
	t.Parallel()
	files, _ := filepath.Glob("testdata/contract/flow.*.jsonl")
	if len(files) == 0 {
		t.Fatal("no flows")
	}
	for _, f := range files {
		t.Run(strings.TrimSuffix(filepath.Base(f), ".jsonl"), func(t *testing.T) {
			t.Parallel()
			lines, err := fakeadapter.LoadTranscript(f)
			if err != nil {
				t.Fatal(err)
			}
			runFlow(t, lines)
		})
	}
}

func TestSandbox(t *testing.T) {
	t.Parallel()
	sb := &fakeadapter.Sandbox{
		Result: protocol.InitializeResult{
			Login:    []string{protocol.StepQR},
			Features: []string{"rooms.list"},
			Limits:   protocol.Limits{MaxText: 5},
		},
		Rooms: []protocol.Room{{ID: "r1", Kind: protocol.RoomGroup}},
	}
	c, notices, _ := pair(t, sb.Serve)
	ctx := context.Background()
	var init protocol.InitializeResult
	if err := c.Call(ctx, protocol.MethodInitialize, protocol.InitializeParams{Protocol: protocol.Version}, &init); err != nil {
		t.Fatal(err)
	}
	_ = c.Notify(protocol.MethodInitialized, nil) //nolint:errcheck // a pipe write
	send := protocol.SendParams{Room: "r1", Text: "hi", Key: "k1"}
	if err := c.Call(ctx, protocol.MethodMessagesSend, send, nil); protocol.CodeOf(err) != protocol.CodeAuthRequired {
		t.Fatalf("send before login: %v", err)
	}
	var begin protocol.AuthBeginResult
	if err := c.Call(ctx, protocol.MethodAuthBegin, protocol.AuthBeginParams{Kind: protocol.StepQR}, &begin); err != nil || begin.Step.Kind != protocol.StepQR {
		t.Fatalf("begin %+v %v", begin, err)
	}
	var r1, r2 protocol.SendResult
	if err := c.Call(ctx, protocol.MethodMessagesSend, send, &r1); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, protocol.MethodMessagesSend, send, &r2); err != nil || r1 != r2 {
		t.Fatalf("dedupe %v %v %v", r1, r2, err)
	}
	send.Text, send.Key = "too long", "k2"
	if err := c.Call(ctx, protocol.MethodMessagesSend, send, nil); protocol.CodeOf(err) != protocol.CodeInvalid {
		t.Fatalf("max_text: %v", err)
	}
	if err := c.Call(ctx, protocol.MethodTyping, protocol.TypingParams{Room: "r1"}, nil); protocol.CodeOf(err) != protocol.CodeUnsupported {
		t.Fatalf("feature gate: %v", err)
	}
	var page protocol.RoomPage
	if err := c.Call(ctx, protocol.MethodRoomsList, protocol.RoomsListParams{}, &page); err != nil || len(page.Rooms) != 1 {
		t.Fatalf("rooms %v %v", page, err)
	}
	if err := c.Call(ctx, protocol.MethodShutdown, nil, nil); err != nil {
		t.Fatal(err)
	}
	<-c.Done()
	var done bool
	for _, n := range notices() {
		done = done || n.method == protocol.MethodAuthDone
	}
	if !done {
		t.Fatal("no auth/done")
	}
}

func TestConnEdges(t *testing.T) {
	t.Parallel()
	c, notices, _ := pair(t, func(r io.Reader, w io.Writer) error {
		_, _ = io.WriteString(w, "not json\n")                                 //nolint:errcheck // a pipe
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":7,"method":"x"}`+"\n") //nolint:errcheck // a pipe
		b := make([]byte, 256)
		n, err := r.Read(b)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b[:n]), `"code":-32601`) {
			return errors.New("want method-not-found for an adapter request: " + string(b[:n]))
		}
		_, _ = io.WriteString(w, strings.Repeat("x", protocol.MaxLine+1)+"\n") //nolint:errcheck // a pipe
		return nil
	})
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("no end")
	}
	if !errors.Is(c.Err(), protocol.ErrLineTooLong) {
		t.Fatalf("err %v", c.Err())
	}
	if n := notices(); len(n) != 1 || n[0].method != "" {
		t.Fatalf("parse errors %v", n)
	}
	if err := c.Call(context.Background(), protocol.MethodPing, nil, nil); !errors.Is(err, protocol.ErrClosed) {
		t.Fatalf("call after close: %v", err)
	}
	e := &protocol.Error{Code: protocol.CodeRateLimited, RetryAfter: time.Second}
	if !errors.Is(e, protocol.NewError(protocol.CodeRateLimited, "")) || errors.Is(e, protocol.NewError(protocol.CodeTransient, "")) {
		t.Fatal("Is by code")
	}
}

// TestConnBadLine proves a line that is not JSON-RPC reaches WithBadLine,
// and reaches notify with an empty method without it.
func TestConnBadLine(t *testing.T) {
	t.Parallel()
	in := strings.NewReader("hello from the adapter\n{\"jsonrpc\":\"2.0\",\"method\":\"status\",\"params\":{\"state\":\"connected\"}}\n")
	var bad []error
	var methods []string
	c := protocol.NewConn(in, io.Discard, func(method string, _ json.RawMessage) { methods = append(methods, method) },
		protocol.WithBadLine(func(err error) { bad = append(bad, err) }))
	if err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || !errors.Is(bad[0], protocol.ErrNotJSONRPC) || !strings.Contains(bad[0].Error(), "hello from the adapter") {
		t.Fatalf("bad = %v", bad)
	}
	if len(methods) != 1 || methods[0] != protocol.MethodStatus {
		t.Fatalf("methods = %v", methods)
	}

	methods = nil
	c = protocol.NewConn(strings.NewReader("noise\n"), io.Discard, func(method string, _ json.RawMessage) { methods = append(methods, method) })
	if err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 || methods[0] != "" {
		t.Fatalf("methods without WithBadLine = %v", methods)
	}
}

// pair wires a Conn to an adapter function over two pipes.
func pair(t *testing.T, adapter func(r io.Reader, w io.Writer) error) (*protocol.Conn, func() []notice, <-chan error) {
	t.Helper()
	dr, aw := io.Pipe()
	ar, dw := io.Pipe()
	var mu sync.Mutex
	var got []notice
	c := protocol.NewConn(dr, dw, func(method string, params json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, notice{method, params})
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.Run(ctx) }() //nolint:errcheck // Done reports the end
	aerr := make(chan error, 1)
	go func() {
		err := adapter(ar, aw)
		_ = aw.Close()
		aerr <- err
	}()
	t.Cleanup(func() {
		cancel()
		_ = dw.Close()
		_ = ar.Close()
	})
	return c, func() []notice {
		mu.Lock()
		defer mu.Unlock()
		return append([]notice(nil), got...)
	}, aerr
}

func runFlow(t *testing.T, lines []fakeadapter.Line) {
	t.Helper()
	c, notices, aerr := pair(t, func(r io.Reader, w io.Writer) error { return fakeadapter.Play(r, w, lines) })
	answers := map[string]protocol.Message{}
	var wantNotices []protocol.Message
	for _, l := range lines {
		var m protocol.Message
		if err := json.Unmarshal(l.Msg, &m); err != nil {
			t.Fatal(err)
		}
		switch {
		case l.From == "adapter" && m.Method == "":
			answers[string(m.ID)] = m
		case l.From == "adapter":
			wantNotices = append(wantNotices, m)
		}
	}
	ctx := context.Background()
	for _, l := range lines {
		var m protocol.Message
		_ = json.Unmarshal(l.Msg, &m) //nolint:errcheck // decoded above
		if l.From != "daemon" {
			continue
		}
		if len(m.ID) == 0 {
			if err := c.Notify(m.Method, m.Params); err != nil {
				t.Fatal(err)
			}
			continue
		}
		var got json.RawMessage
		err := c.Call(ctx, m.Method, m.Params, &got)
		want := answers[string(m.ID)]
		if want.Error != nil {
			wantCode := protocol.CodeOf(errorOf(t, want))
			if protocol.CodeOf(err) != wantCode {
				t.Fatalf("%s: err %v, want code %s", m.Method, err, wantCode)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", m.Method, err)
		}
		if !fakeadapter.Match(want.Result, got) {
			t.Fatalf("%s: result %s, want %s", m.Method, got, want.Result)
		}
	}
	if err := <-aerr; err != nil {
		t.Fatalf("adapter: %v", err)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("conn did not end at EOF")
	}
	got := notices()
	if len(got) != len(wantNotices) {
		t.Fatalf("%d notifications, want %d", len(got), len(wantNotices))
	}
	for i, n := range got {
		if n.method != wantNotices[i].Method || !fakeadapter.Match(wantNotices[i].Params, n.params) {
			t.Fatalf("notification %d: %s %s, want %s %s", i, n.method, n.params, wantNotices[i].Method, wantNotices[i].Params)
		}
	}
}

// errorOf builds the typed error of a transcript response line.
func errorOf(t *testing.T, m protocol.Message) error {
	t.Helper()
	b, _ := json.Marshal(m)
	c, _, _ := pair(t, func(r io.Reader, w io.Writer) error {
		var in protocol.Message
		if err := json.NewDecoder(r).Decode(&in); err != nil {
			return err
		}
		var out map[string]json.RawMessage
		_ = json.Unmarshal(b, &out) //nolint:errcheck // marshalled above
		out["id"] = in.ID
		ob, _ := json.Marshal(out)
		_, err := w.Write(append(ob, '\n'))
		return err
	})
	return c.Call(context.Background(), "x", nil, nil)
}
