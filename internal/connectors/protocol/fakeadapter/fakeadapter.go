// Package fakeadapter is a test adapter for trebi-connector/1. Play runs a
// transcript strictly. Sandbox answers every request from in-memory state.
// A test binary runs either one as a child process through Main, so the
// supervisor tests start a real process.
package fakeadapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/trebi-ai/trebi/internal/connectors/protocol"
)

// Line is one transcript line. From is "daemon" or "adapter".
type Line struct {
	From string          `json:"from"`
	Msg  json.RawMessage `json:"msg"`
}

// Sandbox is a reactive adapter with in-memory rooms and a fake login.
type Sandbox struct {
	Result   protocol.InitializeResult    `json:"result"`
	Rooms    []protocol.Room              `json:"rooms,omitempty"`
	Threads  map[string][]protocol.Thread `json:"threads,omitempty"`
	Events   []protocol.Event             `json:"events,omitempty"`
	Replay   []protocol.Event             `json:"replay,omitempty"`
	LoggedIn bool                         `json:"logged_in,omitempty"`
	Errors   map[string]string            `json:"errors,omitempty"` // method → error code
	// Record is a file that gets one JSON line per request the sandbox
	// receives, so a test can read what the daemon sent.
	Record string `json:"record,omitempty"`

	w     io.Writer
	wmu   sync.Mutex
	sent  map[string]protocol.SendResult
	flows int
}

// Env is the variable that switches a test binary into an adapter:
// "transcript:<path>" or "sandbox:<path to a Sandbox JSON file>".
const Env = "TREBI_FAKEADAPTER"

// AnyValue in a transcript matches any value the daemon sends.
const AnyValue = "<any>"

// Enabled reports whether the process should run as a fake adapter.
func Enabled() bool { return os.Getenv(Env) != "" }

// Main runs the adapter named by Env on stdin and stdout and returns the
// exit code. Call it from TestMain when Enabled.
func Main() int {
	kind, path, _ := strings.Cut(os.Getenv(Env), ":")
	var err error
	switch kind {
	case "transcript":
		var lines []Line
		if lines, err = LoadTranscript(path); err == nil {
			err = Play(os.Stdin, os.Stdout, lines)
		}
	case "sandbox":
		var sb *Sandbox
		if sb, err = LoadSandbox(path); err == nil {
			err = sb.Serve(os.Stdin, os.Stdout)
		}
	default:
		err = fmt.Errorf("%s: unknown kind %q", Env, kind)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeadapter:", err)
		return 1
	}
	return 0
}

// LoadTranscript reads a .jsonl transcript.
func LoadTranscript(path string) ([]Line, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Line
	for i, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var l Line
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, err)
		}
		out = append(out, l)
	}
	return out, nil
}

// Play runs a transcript: each daemon line must match the next message on
// r, and each adapter line is written to w. A response id is rewritten to
// the id the daemon used for the matching request.
func Play(r io.Reader, w io.Writer, lines []Line) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), protocol.MaxLine)
	ids := map[string]json.RawMessage{}
	for i, l := range lines {
		var want protocol.Message
		if err := json.Unmarshal(l.Msg, &want); err != nil {
			return fmt.Errorf("line %d: %w", i+1, err)
		}
		switch l.From {
		case "daemon":
			got, err := next(sc)
			if err != nil {
				return fmt.Errorf("line %d: want %s: %w", i+1, want.Method, err)
			}
			if got.Method != want.Method || !Match(want.Params, got.Params) {
				return fmt.Errorf("line %d: want %s %s, got %s %s", i+1, want.Method, want.Params, got.Method, got.Params)
			}
			if len(want.ID) > 0 {
				ids[string(want.ID)] = got.ID
			}
		case "adapter":
			if want.Method == "" && len(want.ID) > 0 {
				if id, ok := ids[string(want.ID)]; ok {
					want.ID = id
				}
			}
			b, err := json.Marshal(want)
			if err != nil {
				return err
			}
			if _, err := w.Write(append(b, '\n')); err != nil {
				return err
			}
		default:
			return fmt.Errorf("line %d: from must be daemon or adapter", i+1)
		}
	}
	return nil
}

// Match compares two JSON values. A transcript string AnyValue matches any
// value, and an absent params matches {}.
func Match(want, got json.RawMessage) bool {
	var w, g any
	if len(bytes.TrimSpace(want)) == 0 {
		want = json.RawMessage(`{}`)
	}
	if len(bytes.TrimSpace(got)) == 0 {
		got = json.RawMessage(`{}`)
	}
	if json.Unmarshal(want, &w) != nil || json.Unmarshal(got, &g) != nil {
		return false
	}
	return matchValue(w, g)
}

// LoadSandbox reads a Sandbox from a JSON file.
func LoadSandbox(path string) (*Sandbox, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sb Sandbox
	if err := json.Unmarshal(data, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// WriteFile writes the sandbox config for a child process.
func (s *Sandbox) WriteFile(path string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Serve answers requests until shutdown or EOF.
func (s *Sandbox) Serve(r io.Reader, w io.Writer) error {
	s.w = w
	s.sent = map[string]protocol.SendResult{}
	if s.Result.Protocol == "" {
		s.Result.Protocol = protocol.Version
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), protocol.MaxLine)
	for {
		m, err := next(sc)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		s.record(m)
		if len(m.ID) == 0 {
			if m.Method == protocol.MethodInitialized {
				s.afterInitialized()
			}
			continue
		}
		if code, ok := s.Errors[m.Method]; ok {
			s.reply(m.ID, nil, protocol.NewError(code, "sandbox error"))
			continue
		}
		if f := protocol.FeatureOf(m.Method); f != "" && !slices.Contains(s.Result.Features, f) {
			s.reply(m.ID, nil, protocol.NewError(protocol.CodeUnsupported, f+" is not a feature of this adapter"))
			continue
		}
		result, perr := s.handle(m)
		s.reply(m.ID, result, perr)
		if m.Method == protocol.MethodShutdown {
			return nil
		}
		if m.Method == protocol.MethodAuthBegin && perr == nil {
			s.finishQR(result.(protocol.AuthBeginResult))
		}
	}
}

func (s *Sandbox) handle(m protocol.Message) (any, *protocol.Error) {
	switch m.Method {
	case protocol.MethodInitialize:
		return s.Result, nil
	case protocol.MethodPing, protocol.MethodShutdown, protocol.MethodTyping, protocol.MethodMessagesSeen,
		protocol.MethodReactionsAdd, protocol.MethodMessagesEdit, protocol.MethodAuthCancel:
		return protocol.Empty{}, nil
	case protocol.MethodAuthStatus:
		return protocol.AuthStatusResult{State: s.state(), Account: s.account()}, nil
	case protocol.MethodAuthBegin:
		s.flows++
		return protocol.AuthBeginResult{FlowID: "f" + strconv.Itoa(s.flows), Step: protocol.Step{Kind: protocol.StepQR, Data: "sandbox-qr-1"}}, nil
	case protocol.MethodAuthSubmit:
		return protocol.AuthSubmitResult{}, nil
	case protocol.MethodAuthLogout:
		s.LoggedIn = false
		s.notify(protocol.MethodStatus, protocol.Status{State: protocol.StateAuthRequired, Reason: protocol.ReasonLoggedOut})
		return protocol.Empty{}, nil
	case protocol.MethodMessagesSend:
		return s.send(m.Params)
	case protocol.MethodEventsReplay:
		return protocol.ReplayResult{Events: orEmpty(s.Replay), Complete: true}, nil
	case protocol.MethodMessagesHistory:
		return protocol.EventPage{Events: orEmpty(s.Events)}, nil
	case protocol.MethodRoomsList:
		return protocol.RoomPage{Rooms: s.rooms()}, nil
	case protocol.MethodRoomsOpen, protocol.MethodRoomsGet, protocol.MethodThreadsList, protocol.MethodThreadsCreate:
		return s.roomCall(m)
	}
	return nil, protocol.NewError(protocol.CodeUnsupported, m.Method)
}

func (s *Sandbox) send(raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SendParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, protocol.NewError(protocol.CodeInvalid, err.Error())
	}
	if !s.LoggedIn && len(s.Result.Login) > 0 {
		return nil, protocol.NewError(protocol.CodeAuthRequired, "log in first")
	}
	if max := s.Result.Limits.MaxText; max > 0 && len([]rune(p.Text)) > max {
		return nil, protocol.NewError(protocol.CodeInvalid, "text too long")
	}
	if r, ok := s.sent[p.Key]; ok && p.Key != "" {
		return r, nil
	}
	r := protocol.SendResult{MessageID: "m" + strconv.Itoa(len(s.sent)+1), Thread: p.Thread}
	s.sent[p.Key] = r
	return r, nil
}

func (s *Sandbox) afterInitialized() {
	s.notify(protocol.MethodStatus, protocol.Status{State: s.state(), Account: s.account(), Reason: s.reason()})
	for _, e := range s.Events {
		s.notify(protocol.MethodEvent, e)
	}
}

// finishQR completes a login at once: one rotated code, then done.
func (s *Sandbox) finishQR(begin protocol.AuthBeginResult) {
	s.notify(protocol.MethodStatus, protocol.Status{State: protocol.StateAuthPending})
	s.notify(protocol.MethodAuthStep, protocol.AuthStepParams{FlowID: begin.FlowID, Step: protocol.Step{Kind: protocol.StepQR, Data: "sandbox-qr-2"}})
	s.LoggedIn = true
	s.notify(protocol.MethodAuthDone, protocol.AuthDoneParams{FlowID: begin.FlowID, OK: true, Account: s.account()})
	s.notify(protocol.MethodStatus, protocol.Status{State: protocol.StateConnected, Account: s.account()})
}

func (s *Sandbox) state() string {
	if !s.LoggedIn && len(s.Result.Login) > 0 {
		return protocol.StateAuthRequired
	}
	return protocol.StateConnected
}

func (s *Sandbox) reason() string {
	if s.state() == protocol.StateAuthRequired {
		return protocol.ReasonNone
	}
	return ""
}

func (s *Sandbox) account() *protocol.Account {
	if s.state() != protocol.StateConnected {
		return nil
	}
	if s.Result.Account != nil {
		return s.Result.Account
	}
	return &protocol.Account{ID: "sandbox"}
}

func (s *Sandbox) rooms() []protocol.Room {
	return append([]protocol.Room{}, s.Rooms...)
}

func (s *Sandbox) reply(id json.RawMessage, result any, perr *protocol.Error) {
	m := protocol.Message{JSONRPC: "2.0", ID: id}
	if perr != nil {
		s.writeRaw(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":%s}`, id, perr.Wire()))
		return
	}
	m.Result, _ = json.Marshal(result)
	s.write(m)
}

func (s *Sandbox) notify(method string, params any) {
	raw, _ := json.Marshal(params)
	s.write(protocol.Message{JSONRPC: "2.0", Method: method, Params: raw})
}

func (s *Sandbox) write(m protocol.Message) {
	b, _ := json.Marshal(m)
	s.writeRaw(string(b))
}

func (s *Sandbox) writeRaw(line string) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = io.WriteString(s.w, line+"\n") //nolint:errcheck // the daemon closed the pipe; Serve ends at EOF
}

func (s *Sandbox) record(m protocol.Message) {
	if s.Record == "" {
		return
	}
	f, err := os.OpenFile(s.Record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(m)
	_, _ = f.Write(append(b, '\n')) //nolint:errcheck // a test helper; a lost record fails the test that reads it
}

func orEmpty(e []protocol.Event) []protocol.Event {
	if e == nil {
		return []protocol.Event{}
	}
	return e
}

func matchValue(w, g any) bool {
	if s, ok := w.(string); ok && s == AnyValue {
		return true
	}
	wm, wok := w.(map[string]any)
	gm, gok := g.(map[string]any)
	if wok && gok {
		if len(wm) != len(gm) {
			return false
		}
		for k, v := range wm {
			gv, ok := gm[k]
			if !ok || !matchValue(v, gv) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(w, g)
}

func next(sc *bufio.Scanner) (protocol.Message, error) {
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var m protocol.Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return m, err
		}
		return m, nil
	}
	if err := sc.Err(); err != nil {
		return protocol.Message{}, err
	}
	return protocol.Message{}, io.EOF
}

func (s *Sandbox) roomCall(m protocol.Message) (any, *protocol.Error) {
	switch m.Method {
	case protocol.MethodRoomsOpen:
		var p protocol.RoomOpenParams
		_ = json.Unmarshal(m.Params, &p) //nolint:errcheck // a bad params opens an empty id
		return protocol.RoomResult{Room: protocol.Room{ID: p.User, Kind: protocol.RoomDM}}, nil
	case protocol.MethodRoomsGet:
		var p protocol.RoomParams
		_ = json.Unmarshal(m.Params, &p) //nolint:errcheck // a bad params reads as an unknown room
		for _, r := range s.Rooms {
			if r.ID == p.Room {
				return protocol.RoomResult{Room: r}, nil
			}
		}
		return nil, protocol.NewError(protocol.CodeNotFound, "unknown room")
	case protocol.MethodThreadsList:
		var p protocol.ThreadsListParams
		_ = json.Unmarshal(m.Params, &p) //nolint:errcheck // a bad params reads as an empty room
		return protocol.ThreadPage{Threads: append([]protocol.Thread{}, s.Threads[p.Room]...)}, nil
	case protocol.MethodThreadsCreate:
		var p protocol.ThreadCreateParams
		_ = json.Unmarshal(m.Params, &p) //nolint:errcheck // a bad params creates an untitled thread
		return protocol.ThreadResult{Thread: protocol.Thread{ID: "t-" + p.Room, Title: p.Title}}, nil
	}
	return nil, protocol.NewError(protocol.CodeUnsupported, m.Method)
}
