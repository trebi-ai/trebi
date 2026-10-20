// Package fakeadapter is a test adapter for trebi-connector/1. Play runs a
// transcript strictly. Sandbox answers every request from in-memory state;
// with Reference it is the reference of the adapter folder contract, and
// Faults break that contract on purpose. A test binary runs either one as a
// child process through Main, so the supervisor and conformance tests start
// a real process.
package fakeadapter

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// Methods answer auth/methods. A sandbox with no methods has no login.
	Methods []protocol.LoginMethod `json:"methods,omitempty"`
	// Steps are the first steps of the methods, by method id. A method with
	// no step shows a QR code and ends at once. An input step, or a url step
	// with fields, ends at auth/submit.
	Steps map[string]protocol.Step `json:"steps,omitempty"`
	// Accounts go on auth/status.
	Accounts []protocol.AccountState `json:"accounts,omitempty"`
	Errors   map[string]string       `json:"errors,omitempty"` // method → error code
	// Record is a file that gets one JSON line per request the sandbox
	// receives, so a test can read what the daemon sent.
	Record string `json:"record,omitempty"`
	// Reference keeps the login and the sent keys in TREBI_STATE_DIR, emits
	// a self message event for each new send, and replays after a cursor.
	Reference bool `json:"reference,omitempty"`
	// Required are input env names. With one unset, the sandbox reports
	// auth_required with reason missing_input.
	Required []string `json:"required,omitempty"`
	// Faults break the adapter folder contract on purpose.
	Faults []string `json:"faults,omitempty"`
	// Options are the options of each dynamic subscription field.
	Options map[string][]protocol.FieldOption `json:"options,omitempty"`
	// States override the state that sync answers for a subscription id.
	States map[string]protocol.SubscriptionState `json:"states,omitempty"`
	// Synced is the last sync list, and Received holds each webhook
	// request. An in-process test reads them.
	Synced   []protocol.Subscription   `json:"-"`
	Received []protocol.WebhookRequest `json:"-"`

	w        io.Writer
	wmu      sync.Mutex
	sent     map[string]protocol.SendResult
	flows    int
	stateDir string
	log      []protocol.Event // the events of this process, for replay and history
	echo     *protocol.Event  // the self event of the last send, emitted after the reply
	webhook  *protocol.Webhook
	posts    []string        // targets to deliver to the hook after the reply
	answered map[string]bool // subscription ids that got their delivery
	answers  map[string]map[string]string
	pending  map[string]protocol.Step // flows that wait for auth/submit
}

// Subscription values of the sandbox. Target is the room field; the target
// ManualTarget asks a person to finish the setup.
const (
	FieldTarget     = "target"
	ManualTarget    = "sandbox/manual"
	SignatureHeader = "x-sandbox-signature"
)

// Faults of the adapter folder contract.
const (
	FaultWriteHome     = "write_home"      // writes a file to $HOME
	FaultAbsoluteState = "absolute_state"  // stores the absolute state path in a state file
	FaultExitEmpty     = "exit_empty"      // exits on an empty state folder
	FaultStdoutNoise   = "stdout_noise"    // prints a line that is not JSON-RPC on stdout
	FaultRejectReplyTo = "reject_reply_to" // fails a send with reply_to
	FaultAcceptForged  = "accept_forged"   // takes a webhook with a bad signature
	FaultBusyWebhook   = "busy_webhook"    // answers each webhook with transient
)

// Files of a Reference sandbox in TREBI_STATE_DIR.
const (
	loginFile = "login.json"
	sentFile  = "sent.json"
)

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
	if err := s.start(); err != nil {
		return err
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
		result, perr := s.answer(m)
		s.reply(m.ID, result, perr)
		if m.Method == protocol.MethodShutdown {
			return nil
		}
		s.afterReply(m.Method, result, perr)
	}
}

// answer is the reply to one request: a configured error, unsupported for
// a feature the adapter does not have, or the result of handle.
func (s *Sandbox) answer(m protocol.Message) (any, *protocol.Error) {
	if code, ok := s.Errors[m.Method]; ok {
		return nil, protocol.NewError(code, "sandbox error")
	}
	if f := protocol.FeatureOf(m.Method); f != "" && !slices.Contains(s.Result.Features, f) {
		return nil, protocol.NewError(protocol.CodeUnsupported, f+" is not a feature of this adapter")
	}
	return s.handle(m)
}

// afterReply sends what follows a reply: the self event of a send, the
// stdout noise fault, and the end of a QR login.
func (s *Sandbox) afterReply(method string, result any, perr *protocol.Error) {
	if s.echo != nil {
		s.notify(protocol.MethodEvent, *s.echo)
		s.echo = nil
	}
	if method == protocol.MethodInitialize && s.has(FaultStdoutNoise) {
		s.writeRaw("fakeadapter ready")
	}
	if perr == nil {
		switch r := result.(type) {
		case protocol.AuthBeginResult:
			if r.Step.Kind == protocol.StepQR {
				s.notify(protocol.MethodStatus, protocol.Status{State: protocol.StateAuthPending})
				s.notify(protocol.MethodAuthStep, protocol.AuthStepParams{FlowID: r.FlowID, Step: protocol.Step{Kind: protocol.StepQR, Data: "sandbox-qr-2"}})
				s.finishLogin(r.FlowID)
			}
		case submitted:
			s.finishLogin(r.flowID)
		}
	}
	for _, target := range s.posts {
		s.deliver(target)
	}
	s.posts = nil
}

func (s *Sandbox) handle(m protocol.Message) (any, *protocol.Error) {
	switch m.Method {
	case protocol.MethodInitialize:
		var p protocol.InitializeParams
		_ = json.Unmarshal(m.Params, &p) //nolint:errcheck // a bad params starts with no hook
		s.webhook = p.Webhook
		return s.Result, nil
	case protocol.MethodSubscriptionsOptions:
		var p protocol.OptionsParams
		_ = json.Unmarshal(m.Params, &p) //nolint:errcheck // a bad params lists no field
		return protocol.OptionPage{Options: append([]protocol.FieldOption{}, s.Options[p.Field]...)}, nil
	case protocol.MethodSubscriptionsSync:
		return s.sync(m.Params)
	case protocol.MethodSubscriptionsSubmit:
		return s.submit(m.Params)
	case protocol.MethodWebhookReceive:
		return s.receive(m.Params)
	case protocol.MethodPing, protocol.MethodShutdown, protocol.MethodTyping, protocol.MethodMessagesSeen,
		protocol.MethodReactionsAdd, protocol.MethodMessagesEdit, protocol.MethodAuthCancel:
		return protocol.Empty{}, nil
	case protocol.MethodAuthStatus:
		return protocol.AuthStatusResult{State: s.state(), Account: s.account(), Accounts: s.Accounts}, nil
	case protocol.MethodAuthMethods:
		return protocol.AuthMethodsResult{Methods: append([]protocol.LoginMethod{}, s.Methods...)}, nil
	case protocol.MethodAuthBegin:
		return s.begin(m.Params)
	case protocol.MethodAuthSubmit:
		return s.submitLogin(m.Params)
	case protocol.MethodAuthLogout:
		s.LoggedIn = false
		s.notify(protocol.MethodStatus, protocol.Status{State: protocol.StateAuthRequired, Reason: protocol.ReasonLoggedOut})
		return protocol.Empty{}, nil
	case protocol.MethodMessagesSend:
		return s.send(m.Params)
	case protocol.MethodEventsReplay:
		return s.replay(m.Params)
	case protocol.MethodMessagesHistory:
		return s.history(m.Params)
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
	if s.state() == protocol.StateAuthRequired {
		return nil, protocol.NewError(protocol.CodeAuthRequired, "log in first")
	}
	if p.ReplyTo != "" && s.has(FaultRejectReplyTo) {
		return nil, protocol.NewError(protocol.CodeInvalid, "reply_to is not supported")
	}
	if max := s.Result.Limits.MaxText; max > 0 && len([]rune(p.Text)) > max {
		return nil, protocol.NewError(protocol.CodeInvalid, "text too long")
	}
	if r, ok := s.sent[p.Key]; ok && p.Key != "" {
		return r, nil
	}
	r := protocol.SendResult{MessageID: "m" + strconv.Itoa(len(s.sent)+1), Thread: p.Thread}
	s.sent[p.Key] = r
	if s.Reference {
		e := protocol.Event{
			ID: r.MessageID, Type: "message", TS: time.Now().UTC().Format(time.RFC3339Nano),
			Room: &protocol.Room{ID: p.Room}, Sender: &protocol.Sender{ID: s.account().ID, Self: true},
			Text: p.Text, ReplyTo: p.ReplyTo, Attachments: p.Attachments,
		}
		if p.Thread != "" {
			e.Thread = &protocol.Thread{ID: p.Thread}
		}
		s.log = append(s.log, e)
		s.echo = &e
		if err := s.save(sentFile, s.sent); err != nil {
			return nil, protocol.NewError(protocol.CodeTransient, err.Error())
		}
	}
	return r, nil
}

// start reads the state of a Reference sandbox and applies the faults.
func (s *Sandbox) start() error {
	s.stateDir = os.Getenv(protocol.EnvStateDir)
	s.log = slices.Clone(s.Events)
	if err := s.applyFaults(); err != nil {
		return err
	}
	if !s.Reference || s.stateDir == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(s.stateDir, loginFile)); err == nil {
		s.LoggedIn = true
	}
	data, err := os.ReadFile(filepath.Join(s.stateDir, sentFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &s.sent)
}

// applyFaults applies the folder faults at start.
func (s *Sandbox) applyFaults() error {
	if s.stateDir != "" && s.has(FaultExitEmpty) {
		if entries, err := os.ReadDir(s.stateDir); err == nil && len(entries) == 0 {
			return errors.New("the state folder is empty")
		}
	}
	if s.has(FaultWriteHome) {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(home, ".fakeadapter"), []byte("state\n"), 0o600); err != nil {
			return err
		}
	}
	if s.stateDir != "" && s.has(FaultAbsoluteState) {
		return s.save("config.json", map[string]string{"store": s.stateDir})
	}
	return nil
}

// save writes one state file of a Reference sandbox: a temp file, then a
// rename. It does nothing outside Trebi.
func (s *Sandbox) save(name string, v any) error {
	if s.stateDir == "" {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.stateDir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.stateDir, name))
}

func (s *Sandbox) has(fault string) bool { return slices.Contains(s.Faults, fault) }

// missing returns the first required input with no value, or "".
func (s *Sandbox) missing() string {
	for _, name := range s.Required {
		if os.Getenv(name) == "" {
			return name
		}
	}
	return ""
}

// replay returns the events after a cursor. A Reference sandbox knows the
// events of this process only; an unknown cursor is not complete.
func (s *Sandbox) replay(raw json.RawMessage) (any, *protocol.Error) {
	if !s.Reference {
		return protocol.ReplayResult{Events: orEmpty(s.Replay), Complete: true}, nil
	}
	var p protocol.ReplayParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, protocol.NewError(protocol.CodeInvalid, err.Error())
	}
	events := slices.Concat(s.Replay, s.log)
	if p.After == "" {
		return protocol.ReplayResult{Events: orEmpty(events), Complete: true}, nil
	}
	i := slices.IndexFunc(events, func(e protocol.Event) bool { return e.ID == p.After })
	if i < 0 {
		return protocol.ReplayResult{Events: []protocol.Event{}, Complete: false}, nil
	}
	return protocol.ReplayResult{Events: orEmpty(events[i+1:]), Complete: true}, nil
}

// history returns the configured events, or for a Reference sandbox the
// events of the room.
func (s *Sandbox) history(raw json.RawMessage) (any, *protocol.Error) {
	if !s.Reference {
		return protocol.EventPage{Events: orEmpty(s.Events)}, nil
	}
	var p protocol.HistoryParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, protocol.NewError(protocol.CodeInvalid, err.Error())
	}
	out := []protocol.Event{}
	for _, e := range s.log {
		if e.Room != nil && e.Room.ID == p.Room {
			out = append(out, e)
		}
	}
	return protocol.EventPage{Events: out}, nil
}

func (s *Sandbox) afterInitialized() {
	st := protocol.Status{State: s.state(), Account: s.account(), Reason: s.reason()}
	if name := s.missing(); name != "" {
		st.Message = name + " is missing"
	}
	s.notify(protocol.MethodStatus, st)
	for _, e := range s.Events {
		s.notify(protocol.MethodEvent, e)
	}
}

// submitted is the result of a good auth/submit. afterReply ends its flow.
type submitted struct {
	protocol.AuthSubmitResult
	flowID string
}

// MarshalJSON writes the submit result only.
func (r submitted) MarshalJSON() ([]byte, error) { return json.Marshal(r.AuthSubmitResult) }

// begin starts a flow with the named method, or with the first available
// one.
func (s *Sandbox) begin(raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.AuthBeginParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, protocol.NewError(protocol.CodeInvalid, err.Error())
	}
	i := slices.IndexFunc(s.Methods, func(m protocol.LoginMethod) bool {
		return m.ID == p.Method || (p.Method == "" && m.Available)
	})
	switch {
	case i < 0 && p.Method == "":
		return nil, protocol.NewError(protocol.CodeInvalid, "no login method is available")
	case i < 0:
		return nil, protocol.NewError(protocol.CodeInvalid, "unknown login method "+strconv.Quote(p.Method))
	case !s.Methods[i].Available:
		return nil, protocol.NewError(protocol.CodeInvalid, s.Methods[i].Reason)
	}
	s.flows++
	id := "f" + strconv.Itoa(s.flows)
	step, ok := s.Steps[s.Methods[i].ID]
	if !ok {
		step = protocol.Step{Kind: protocol.StepQR, Data: "sandbox-qr-1"}
	}
	if len(step.Fields) > 0 {
		if s.pending == nil {
			s.pending = map[string]protocol.Step{}
		}
		s.pending[id] = step
	}
	return protocol.AuthBeginResult{FlowID: id, Step: step}, nil
}

// submitLogin checks the answer of a waiting flow: each field has a value,
// and a pick list gets only its own choices.
func (s *Sandbox) submitLogin(raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.AuthSubmitParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, protocol.NewError(protocol.CodeInvalid, err.Error())
	}
	step, ok := s.pending[p.FlowID]
	if !ok {
		return nil, protocol.NewError(protocol.CodeInvalid, "unknown flow "+strconv.Quote(p.FlowID))
	}
	for _, f := range step.Fields {
		v := p.Fields[f.Name]
		if step.Kind == protocol.StepInput && len(v) == 0 {
			return nil, protocol.NewError(protocol.CodeInvalid, f.Label+" is required")
		}
		if len(v) > 1 && !f.Multiple {
			return nil, protocol.NewError(protocol.CodeInvalid, f.Label+" takes one value")
		}
		for _, x := range v {
			if len(f.Choices) > 0 && !slices.ContainsFunc(f.Choices, func(c protocol.Choice) bool { return c.Value == x }) {
				return nil, protocol.NewError(protocol.CodeInvalid, strconv.Quote(x)+" is not a choice of "+f.Label)
			}
		}
	}
	delete(s.pending, p.FlowID)
	return submitted{flowID: p.FlowID}, nil
}

// finishLogin ends a flow with a good login.
func (s *Sandbox) finishLogin(flowID string) {
	s.LoggedIn = true
	if s.Reference {
		_ = s.save(loginFile, s.account()) //nolint:errcheck // a lost login file shows as auth_required at the next start
	}
	s.notify(protocol.MethodAuthDone, protocol.AuthDoneParams{FlowID: flowID, OK: true, Account: s.account()})
	s.notify(protocol.MethodStatus, protocol.Status{State: protocol.StateConnected, Account: s.account(), Accounts: s.Accounts})
}

func (s *Sandbox) state() string {
	if s.missing() != "" || (!s.LoggedIn && len(s.Methods) > 0) {
		return protocol.StateAuthRequired
	}
	return protocol.StateConnected
}

func (s *Sandbox) reason() string {
	if s.missing() != "" {
		return protocol.ReasonMissingInput
	}
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

// sync answers one state for each subscription. With a hook, a target
// becomes active and the fake platform posts one delivery for each new one;
// ManualTarget waits for a person. Without a hook, the sandbox polls.
func (s *Sandbox) sync(raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SyncParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, protocol.NewError(protocol.CodeInvalid, err.Error())
	}
	if s.answered == nil {
		s.answered = map[string]bool{}
	}
	s.Synced = p.Subscriptions
	out := protocol.SyncResult{Subscriptions: []protocol.SubscriptionState{}}
	for _, sub := range p.Subscriptions {
		out.Subscriptions = append(out.Subscriptions, s.stateOf(sub))
	}
	return out, nil
}

func (s *Sandbox) stateOf(sub protocol.Subscription) protocol.SubscriptionState {
	if st, ok := s.States[sub.ID]; ok {
		st.ID = sub.ID
		return st
	}
	target := ""
	if v := sub.Values[FieldTarget]; len(v) > 0 {
		target = v[0]
	}
	st := protocol.SubscriptionState{ID: sub.ID, Title: cmpOr(target, "Everything")}
	if target != "" {
		st.Room = &protocol.Room{ID: target}
	}
	switch {
	case s.webhook == nil:
		st.Mode, st.State = "poll", protocol.SubscriptionPolling
	case target == ManualTarget && s.answers[sub.ID] == nil:
		st.Mode, st.State = "manual", protocol.SubscriptionActionRequired
		st.Action = &protocol.Action{
			Text: "Paste the URL into the sandbox, then type the code that it shows.",
			Show: []protocol.ShowValue{{Label: "URL", Value: s.webhook.URL}},
			Ask:  []protocol.AskField{{Name: "code", Label: "Code", Secret: true, Required: true}},
		}
	default:
		st.Mode, st.State = "api", protocol.SubscriptionActive
		if target == ManualTarget {
			st.Mode = "manual"
		}
		if !s.answered[sub.ID] {
			s.answered[sub.ID] = true
			s.posts = append(s.posts, target)
		}
	}
	return st
}

// submit stores the answers of a person and finishes the subscription.
func (s *Sandbox) submit(raw json.RawMessage) (any, *protocol.Error) {
	var p protocol.SubmitParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, protocol.NewError(protocol.CodeInvalid, err.Error())
	}
	i := slices.IndexFunc(s.Synced, func(sub protocol.Subscription) bool { return sub.ID == p.ID })
	if i < 0 {
		return nil, protocol.NewError(protocol.CodeInvalid, "unknown subscription "+p.ID)
	}
	if p.Fields["code"] == "" {
		return nil, protocol.NewError(protocol.CodeInvalid, "code is required")
	}
	if s.answers == nil {
		s.answers = map[string]map[string]string{}
	}
	s.answers[p.ID] = p.Fields
	return s.stateOf(s.Synced[i]), nil
}

// receive checks the signature and turns one delivery into one event. A
// handshake makes no event.
func (s *Sandbox) receive(raw json.RawMessage) (any, *protocol.Error) {
	var req protocol.WebhookRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, protocol.NewError(protocol.CodeInvalid, err.Error())
	}
	s.Received = append(s.Received, req)
	if s.has(FaultBusyWebhook) {
		return nil, protocol.NewError(protocol.CodeTransient, "the platform is busy")
	}
	if req.Handshake {
		return protocol.Empty{}, nil
	}
	if s.webhook == nil || (req.Headers[SignatureHeader] != Sign(s.webhook.Secret, req.Body) && !s.has(FaultAcceptForged)) {
		return nil, protocol.NewError(protocol.CodeInvalid, "bad signature")
	}
	var body struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
		return nil, protocol.NewError(protocol.CodeInvalid, err.Error())
	}
	typ := "message"
	if len(s.Result.Events) > 0 {
		typ = s.Result.Events[0].Type
	}
	e := protocol.Event{ID: req.ID, Type: typ, TS: cmpOr(req.ReceivedAt, time.Now().UTC().Format(time.RFC3339Nano)), Text: body.Text, Sender: &protocol.Sender{ID: "sandbox-platform"}}
	if body.Target != "" {
		e.Room = &protocol.Room{ID: body.Target}
	}
	s.notify(protocol.MethodEvent, e)
	return protocol.Empty{}, nil
}

// deliver posts one signed delivery to the hook, as the platform does after
// a subscription is created. A failed post is not an error of the adapter.
func (s *Sandbox) deliver(target string) {
	if s.webhook == nil {
		return
	}
	b, _ := json.Marshal(map[string]string{"target": target, "text": "hello"})
	req, err := http.NewRequest(http.MethodPost, s.webhook.URL, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, Sign(s.webhook.Secret, string(b)))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close() //nolint:errcheck // the status is not checked
}

// Sign is the sandbox signature of a body: sha256=<hex HMAC>.
func Sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
