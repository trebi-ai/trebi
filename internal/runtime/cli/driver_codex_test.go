package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// codexPeer is a scripted app-server peer for driver tests.
type codexPeer struct {
	t        *testing.T
	read     *bufio.Scanner
	write    func(v any)
	seen     chan map[string]any
	approve  bool
	closeOut func()
	resumeID string // handshake resumes this thread id instead of starting

	mu       sync.Mutex
	hold     bool // turn/start stays active until the test completes it
	steerErr bool // answer turn/steer with method-not-found
}

func startCodexPeer(t *testing.T, approve bool, opts ...func(*codexPeer)) (*codexPeer, *wireSession, *codexProtocol) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	peer := &codexPeer{t: t, read: bufio.NewScanner(inR), seen: make(chan map[string]any, 16), approve: approve}
	for _, opt := range opts {
		opt(peer)
	}
	enc := json.NewEncoder(outW)
	peer.write = func(v any) {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		_ = enc.Encode(v)
	}
	peer.closeOut = func() { _ = outW.Close() }

	go peer.serve()
	proto := &codexProtocol{
		oneShot: false, model: "gpt-5", cwd: t.TempDir(),
		approvalPolicy: "never", sandbox: "workspace-write",
		resumeID: peer.resumeID, waiters: map[int]chan map[string]any{},
	}
	s := newTestWire("codex", proto, inW, outR, context.Background())
	proto.setWriter(&wireWriter{s: s})
	return peer, s, proto
}

func (p *codexPeer) reply(id any, result map[string]any) {
	p.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (p *codexPeer) notify(method string, params map[string]any) {
	p.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (p *codexPeer) serve() {
	defer p.closeOut()
	for p.read.Scan() {
		var m map[string]any
		if json.Unmarshal(p.read.Bytes(), &m) != nil {
			continue
		}
		method, _ := m["method"].(string)
		p.seen <- m
		switch method {
		case "initialize":
			p.reply(m["id"], map[string]any{"userAgent": "codex-test"})
		case "initialized":
			// nothing
		case "thread/start":
			p.reply(m["id"], map[string]any{"threadId": "thr_1"})
			p.notify("thread/started", map[string]any{"thread": map[string]any{"id": "thr_1"}})
		case "thread/resume":
			params, _ := m["params"].(map[string]any)
			p.reply(m["id"], map[string]any{"threadId": params["threadId"]})
		case "turn/start":
			p.reply(m["id"], map[string]any{"turnId": "turn_1"})
			p.notify("turn/started", map[string]any{"turn": map[string]any{"id": "turn_1"}})
			if p.hold {
				continue // keep the turn active; the test completes it
			}
			if p.approve {
				p.write(map[string]any{
					"jsonrpc": "2.0", "id": 7, "method": "item/commandExecution/requestApproval",
					"params": map[string]any{"threadId": "thr_1", "command": "touch /tmp/x"},
				})
				// Wait for the decision before completing the turn.
				for p.read.Scan() {
					var d map[string]any
					if json.Unmarshal(p.read.Bytes(), &d) != nil {
						continue
					}
					if _, has := d["result"]; has {
						break
					}
				}
			}
			p.notify("item/started", map[string]any{"item": map[string]any{
				"id": "item_1", "type": "agentMessage", "text": "codex says pong",
			}})
			p.notify("item/completed", map[string]any{"item": map[string]any{
				"id": "item_1", "type": "agentMessage", "text": "codex says pong",
			}})
			p.notify("turn/completed", map[string]any{"turn": map[string]any{"id": "turn_1", "status": "completed"}})
			return
		case "turn/steer":
			if p.steerErr {
				p.write(map[string]any{"jsonrpc": "2.0", "id": m["id"],
					"error": map[string]any{"code": -32601, "message": "method not found"}})
				continue // keep reading: the client falls back to interrupt
			}
			p.reply(m["id"], map[string]any{"turnId": "turn_1"})
		case "turn/interrupt":
			p.reply(m["id"], map[string]any{})
			p.notify("turn/completed", map[string]any{"turn": map[string]any{"id": "turn_1", "status": "interrupted"}})
		}
	}
}

// TestCodexHandshakeAndTurn drives initialize → thread/start → turn/start.
func TestCodexHandshakeAndTurn(t *testing.T) {
	t.Parallel()
	_, s, proto := startCodexPeer(t, false)
	defer s.procCloseForTest()
	deadline := time.Now().Add(3 * time.Second)
	for proto.threadID == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if proto.threadID != "thr_1" {
		t.Fatalf("thread id: %q", proto.threadID)
	}
	if s.ID() != "thr_1" {
		t.Fatalf("session id: %q", s.ID())
	}
	if err := s.Prompt(context.Background(), Prompt{Text: "say pong"}); err != nil {
		t.Fatal(err)
	}
	var got []Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				if countType(got, EventAssistant) == 0 {
					t.Fatalf("no assistant text: %+v", got)
				}
				if countType(got, EventResult) != 1 {
					t.Fatalf("result count: %+v", got)
				}
				return
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timeout: %+v", got)
		}
	}
}

// TestCodexResumeHandshake pins the start/resume split: a stored thread id
// sends thread/resume; an empty one sends thread/start. A fresh run carries a
// trebi-minted id that must never reach the wire as a resume handle.
func TestCodexResumeHandshake(t *testing.T) {
	t.Parallel()
	peer, s, proto := startCodexPeer(t, false, func(p *codexPeer) { p.resumeID = "thr_9" })
	defer s.procCloseForTest()
	if proto.threadID != "thr_9" {
		t.Fatalf("resumed thread id: %q", proto.threadID)
	}
	if s.ID() != "thr_9" {
		t.Fatalf("session id: %q", s.ID())
	}
	methods := peerSentMethods(peer)
	if !hasMethod(methods, "thread/resume") || hasMethod(methods, "thread/start") {
		t.Fatalf("handshake methods: %v", methods)
	}
}

// TestCodexApprovalRoundtrip maps a server approval request to a permission
// and answers accept.
func TestCodexApprovalRoundtrip(t *testing.T) {
	t.Parallel()
	_, s, proto := startCodexPeer(t, true)
	defer s.procCloseForTest()
	deadline := time.Now().Add(3 * time.Second)
	for proto.threadID == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Prompt(context.Background(), Prompt{Text: "touch a file"}); err != nil {
		t.Fatal(err)
	}
	var perm *Permission
	timeout := time.After(5 * time.Second)
	for perm == nil {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("stream closed before permission")
			}
			if ev.Type == EventPermission {
				perm = ev.Permission
			}
		case <-timeout:
			t.Fatal("timeout waiting for permission")
		}
	}
	if perm.ID != "7" {
		t.Fatalf("permission id: %q", perm.ID)
	}
	if err := s.AnswerPermission(context.Background(), perm.ID, Decision{Allow: true}); err != nil {
		t.Fatal(err)
	}
	// The peer only completes the turn after it sees the decision.
	timeout = time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return
			}
			if ev.Type == EventResult {
				if ev.Result == nil || ev.Result.IsError {
					t.Fatalf("result: %+v", ev.Result)
				}
				return
			}
		case <-timeout:
			t.Fatal("timeout waiting for turn completion")
		}
	}
}

// procCloseForTest tears down the in-memory session's pipes.
func (s *wireSession) procCloseForTest() {
	if c, ok := s.stdin.(io.Closer); ok {
		_ = c.Close()
	}
}

// waitSeen drains peer.seen until the wanted method arrives.
func waitSeen(t *testing.T, peer *codexPeer, method string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-peer.seen:
			if m["method"] == method {
				return m
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s", method)
		}
	}
}

// waitThreadID blocks until the handshake captured the thread id.
func waitThreadID(t *testing.T, proto *codexProtocol) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for proto.threadID == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if proto.threadID == "" {
		t.Fatal("thread id not captured")
	}
}

// waitTurnID blocks until turn/started has been observed.
func waitTurnID(t *testing.T, proto *codexProtocol) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		proto.mu.Lock()
		id := proto.turnID
		proto.mu.Unlock()
		if id != "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("turn id not captured")
}

// TestCodexSteersActiveTurn pins E.2: a prompt during a live turn becomes
// turn/steer with the expected turn id.
func TestCodexSteersActiveTurn(t *testing.T) {
	t.Parallel()
	peer, s, proto := startCodexPeer(t, false, func(p *codexPeer) { p.hold = true })
	defer s.procCloseForTest()
	waitThreadID(t, proto)

	if err := s.Prompt(context.Background(), Prompt{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	waitTurnID(t, proto)
	if err := s.Prompt(context.Background(), Prompt{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	steer := waitSeen(t, peer, "turn/steer")
	params, _ := steer["params"].(map[string]any)
	if params["expectedTurnId"] != "turn_1" {
		t.Fatalf("steer params: %+v", params)
	}
	input, _ := params["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("steer input: %+v", params["input"])
	}
	if part, _ := input[0].(map[string]any); part["text"] != "second" {
		t.Fatalf("steer text: %+v", part)
	}
	peer.notify("turn/completed", map[string]any{"turn": map[string]any{"id": "turn_1", "status": "completed"}})
}

// TestCodexInterruptsWhenSteerUnsupported pins the fallback: an app-server
// without turn/steer gets turn/interrupt then a fresh turn/start.
func TestCodexInterruptsWhenSteerUnsupported(t *testing.T) {
	t.Parallel()
	peer, s, proto := startCodexPeer(t, false, func(p *codexPeer) { p.hold = true; p.steerErr = true })
	defer s.procCloseForTest()
	waitThreadID(t, proto)

	if err := s.Prompt(context.Background(), Prompt{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	waitTurnID(t, proto)
	if err := s.Prompt(context.Background(), Prompt{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	waitSeen(t, peer, "turn/steer")
	interrupt := waitSeen(t, peer, "turn/interrupt")
	params, _ := interrupt["params"].(map[string]any)
	if params["turnId"] != "turn_1" {
		t.Fatalf("interrupt params: %+v", params)
	}
	restart := waitSeen(t, peer, "turn/start")
	restartParams, _ := restart["params"].(map[string]any)
	input, _ := restartParams["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("restart input: %+v", restartParams["input"])
	}
	if part, _ := input[0].(map[string]any); part["text"] != "second" {
		t.Fatalf("restart text: %+v", part)
	}
	peer.notify("turn/completed", map[string]any{"turn": map[string]any{"id": "turn_1", "status": "completed"}})
}

// peerSentMethods drains the frames the driver sent; a quiet gap ends the
// collection (the handshake is synchronous, so the frames are already sent).
func peerSentMethods(p *codexPeer) []string {
	var methods []string
	for {
		select {
		case m := <-p.seen:
			if method, _ := m["method"].(string); method != "" {
				methods = append(methods, method)
			}
		case <-time.After(200 * time.Millisecond):
			return methods
		}
	}
}

func hasMethod(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
