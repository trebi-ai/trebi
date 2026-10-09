package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestContractFixtures decodes each fixture strictly into its Go type and
// checks that the Go type writes the same JSON back.
func TestContractFixtures(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("testdata/contract/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	types := fixtureTypes()
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var m Message
			if err := json.Unmarshal(data, &m); err != nil || m.JSONRPC != "2.0" {
				t.Fatalf("envelope: %v", err)
			}
			if code, ok := strings.CutPrefix(name, "error."); ok {
				checkErrorFixture(t, m, code)
				return
			}
			newT, ok := types[name]
			if !ok {
				t.Fatalf("fixture %s has no Go type in fixtureTypes", name)
			}
			body := m.Params
			if len(m.Result) > 0 {
				body = m.Result
			}
			v := newT()
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.DisallowUnknownFields()
			if err := dec.Decode(v); err != nil {
				t.Fatalf("strict decode: %v", err)
			}
			back, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if !sameJSON(t, body, back) {
				t.Fatalf("round trip\n got %s\nwant %s", back, body)
			}
		})
	}
}

func TestFallbackErrorCodes(t *testing.T) {
	t.Parallel()
	cases := map[int]string{-32601: CodeUnsupported, -32602: CodeInvalid, -32000: CodePermanent}
	for rpc, want := range cases {
		if got := (&wireError{Code: rpc, Message: "x"}).typed().Code; got != want {
			t.Fatalf("%d → %s, want %s", rpc, got, want)
		}
	}
}

// fixtureTypes maps each contract fixture to the Go type of its params or
// result. Every file in testdata/contract must be listed.
func fixtureTypes() map[string]func() any {
	return map[string]func() any{
		"initialize.request":       func() any { return &InitializeParams{} },
		"initialize.result":        func() any { return &InitializeResult{} },
		"initialized":              func() any { return &Empty{} },
		"ping.request":             func() any { return &Empty{} },
		"ping.result":              func() any { return &Empty{} },
		"shutdown.request":         func() any { return &Empty{} },
		"shutdown.result":          func() any { return &Empty{} },
		"event.message":            func() any { return &Event{} },
		"event.message.thread":     func() any { return &Event{} },
		"event.message.bot":        func() any { return &Event{} },
		"event.reaction":           func() any { return &Event{} },
		"status.connected":         func() any { return &Status{} },
		"status.auth_required":     func() any { return &Status{} },
		"auth.status.request":      func() any { return &Empty{} },
		"auth.status.result":       func() any { return &AuthStatusResult{} },
		"auth.begin.request":       func() any { return &AuthBeginParams{} },
		"auth.begin.result":        func() any { return &AuthBeginResult{} },
		"auth.step.qr":             func() any { return &AuthStepParams{} },
		"auth.step.device_code":    func() any { return &AuthStepParams{} },
		"auth.step.url":            func() any { return &AuthStepParams{} },
		"auth.step.input":          func() any { return &AuthStepParams{} },
		"auth.step.wait":           func() any { return &AuthStepParams{} },
		"auth.submit.request":      func() any { return &AuthSubmitParams{} },
		"auth.submit.result":       func() any { return &AuthSubmitResult{} },
		"auth.cancel.request":      func() any { return &AuthFlowParams{} },
		"auth.logout.request":      func() any { return &Empty{} },
		"auth.done":                func() any { return &AuthDoneParams{} },
		"auth.done.failed":         func() any { return &AuthDoneParams{} },
		"messages.send.request":    func() any { return &SendParams{} },
		"messages.send.result":     func() any { return &SendResult{} },
		"messages.edit.request":    func() any { return &EditParams{} },
		"messages.history.request": func() any { return &HistoryParams{} },
		"messages.history.result":  func() any { return &EventPage{} },
		"messages.seen.request":    func() any { return &SeenParams{} },
		"events.replay.request":    func() any { return &ReplayParams{} },
		"events.replay.result":     func() any { return &ReplayResult{} },
		"rooms.list.request":       func() any { return &RoomsListParams{} },
		"rooms.list.result":        func() any { return &RoomPage{} },
		"rooms.get.request":        func() any { return &RoomParams{} },
		"rooms.get.result":         func() any { return &RoomResult{} },
		"rooms.open.request":       func() any { return &RoomOpenParams{} },
		"rooms.open.result":        func() any { return &RoomResult{} },
		"threads.list.request":     func() any { return &ThreadsListParams{} },
		"threads.list.result":      func() any { return &ThreadPage{} },
		"threads.create.request":   func() any { return &ThreadCreateParams{} },
		"threads.create.result":    func() any { return &ThreadResult{} },
		"typing.request":           func() any { return &TypingParams{} },
		"reactions.add.request":    func() any { return &ReactionParams{} },
		"empty.result":             func() any { return &Empty{} },

		"initialize.request.webhook":    func() any { return &InitializeParams{} },
		"subscriptions.options.request": func() any { return &OptionsParams{} },
		"subscriptions.options.result":  func() any { return &OptionPage{} },
		"subscriptions.sync.request":    func() any { return &SyncParams{} },
		"subscriptions.sync.result":     func() any { return &SyncResult{} },
		"subscriptions.submit.request":  func() any { return &SubmitParams{} },
		"subscriptions.submit.result":   func() any { return &SubscriptionState{} },
		"webhook.receive.request":       func() any { return &WebhookRequest{} },
		"webhook.receive.handshake":     func() any { return &WebhookRequest{} },
		"subscriptions.changed":         func() any { return &Empty{} },
	}
}

func checkErrorFixture(t *testing.T, m Message, code string) {
	t.Helper()
	if m.Error == nil {
		t.Fatal("no error object")
	}
	e := m.Error.typed()
	if e.Code != code {
		t.Fatalf("code %q, want %q", e.Code, code)
	}
	if code == CodeRateLimited && e.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("retry_after %v", e.RetryAfter)
	}
	back := (&Error{Code: e.Code, Message: e.Message, RetryAfter: e.RetryAfter}).Wire()
	want, _ := json.Marshal(m.Error)
	if !sameJSON(t, want, back) {
		t.Fatalf("wire\n got %s\nwant %s", back, want)
	}
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
