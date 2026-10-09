package protocol_test

import (
	"context"
	"io"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/connectors/protocol/fakeadapter"
)

func TestConformancePass(t *testing.T) {
	t.Parallel()
	c := &protocol.Checker{Features: []string{"rooms.list", "history"}, EventTypes: []string{"message"}, Login: []string{"qr"}, Room: "r1"}
	res := runChecker(t, sandbox(), c)
	for _, check := range []string{"initialize", "ping", "feature rooms.list", "feature threads", "feature history", "feature typing", "unsupported", "send once", "login", "events", "shutdown"} {
		if r := res[check]; r.Status != protocol.CheckPass {
			t.Errorf("%s = %+v", check, r)
		}
	}
}

func TestConformanceFailures(t *testing.T) {
	t.Parallel()
	sb := sandbox()
	sb.Events[0].Type = "other"
	c := &protocol.Checker{Features: []string{"rooms.list"}, EventTypes: []string{"message"}, Login: []string{"input"}}
	res := runChecker(t, sb, c)
	if r := res["events"]; r.Status != protocol.CheckFail {
		t.Errorf("events = %+v", r)
	}
	if r := res["login"]; r.Status != protocol.CheckFail {
		t.Errorf("login = %+v", r)
	}
	if r := res["send once"]; r.Status != protocol.CheckSkip {
		t.Errorf("send = %+v", r)
	}

	// A promised feature the adapter does not deliver stops the run.
	c = &protocol.Checker{Features: []string{"reactions"}}
	res = runChecker(t, sandbox(), c)
	if len(res) != 1 || res["initialize"].Status != protocol.CheckFail {
		t.Fatalf("res = %+v", res)
	}
}

func TestConformanceLiveSkipsShutdown(t *testing.T) {
	t.Parallel()
	sb := sandbox()
	init := sb.Result
	c := &protocol.Checker{Init: &init, SkipLogin: true}
	res := runChecker(t, sb, c)
	if res["shutdown"].Status != protocol.CheckSkip || res["login"].Status != protocol.CheckSkip || res["ping"].Status != protocol.CheckPass {
		t.Fatalf("res = %+v", res)
	}
}

// TestConformanceSandboxReference runs every new check against the
// reference sandbox: write features, the self event of a send, replay with
// a cursor, error codes, status, limits, and clean stdout.
func TestConformanceSandboxReference(t *testing.T) {
	t.Parallel()
	sb := sandbox()
	sb.Reference = true
	sb.LoggedIn = false
	sb.Result.Features = []string{"rooms.list", "rooms.open", "threads", "threads.create", "history", "replay", "typing", "seen", "reactions", "edit", "attachments.out", "replies"}
	sb.Result.Limits = protocol.Limits{MaxText: 1000, Formats: []string{"text"}}
	c := &protocol.Checker{
		Features: sb.Result.Features, EventTypes: []string{"message"}, Login: []string{"qr"},
		Limits: protocol.Limits{MaxText: 4000, Formats: []string{"text", "markdown"}}, Sandbox: true, EmptyState: true,
	}
	res := runChecker(t, sb, c)
	for check, r := range res {
		want := protocol.CheckPass
		switch check {
		case "unsupported", "attachments in":
			want = protocol.CheckSkip // every feature declared; no state folder in process
		case "subscriptions", "webhooks":
			want = protocol.CheckSkip // the conformance package proves them
		}
		if r.Status != want {
			t.Errorf("%s = %+v, want %s", check, r, want)
		}
	}
	for _, check := range []string{"status", "limits", "sandbox login", "feature edit", "feature attachments.out", "feature replies", "events from send", "replay cursor", "error codes", "clean stdout"} {
		if _, ok := res[check]; !ok {
			t.Errorf("no row %q", check)
		}
	}
}

func TestConformanceNewFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		edit  func(*fakeadapter.Sandbox, *protocol.Checker)
		check string
	}{
		{"stdout noise", func(sb *fakeadapter.Sandbox, _ *protocol.Checker) { sb.Faults = []string{fakeadapter.FaultStdoutNoise} }, "clean stdout"},
		{"limits", func(sb *fakeadapter.Sandbox, c *protocol.Checker) {
			sb.Result.Limits.MaxText = 5000
			c.Limits.MaxText = 4000
		}, "limits"},
		{"no echo", func(sb *fakeadapter.Sandbox, _ *protocol.Checker) { sb.Reference = false }, "events from send"},
		{"logged in on empty state", func(sb *fakeadapter.Sandbox, c *protocol.Checker) { sb.LoggedIn = true }, "status"},
		{"rejects reply_to", func(sb *fakeadapter.Sandbox, c *protocol.Checker) {
			sb.Faults = []string{fakeadapter.FaultRejectReplyTo}
			sb.Result.Features = append(sb.Result.Features, "replies")
			c.Features = []string{"replies"}
		}, "feature replies"},
		{"replay ignores the cursor", func(sb *fakeadapter.Sandbox, _ *protocol.Checker) {
			sb.Reference = false
			sb.Result.Features = append(sb.Result.Features, "replay")
		}, "replay cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sb := sandbox()
			sb.Reference = true
			sb.LoggedIn = false
			c := &protocol.Checker{Login: []string{"qr"}, Sandbox: true, EmptyState: true}
			tt.edit(sb, c)
			res := runChecker(t, sb, c)
			if r := res[tt.check]; r.Status != protocol.CheckFail {
				t.Fatalf("%s = %+v", tt.check, r)
			}
		})
	}
}

// runChecker runs a checker against an in-process sandbox and returns the
// status of each check.
func runChecker(t *testing.T, sb *fakeadapter.Sandbox, c *protocol.Checker) map[string]protocol.CheckResult {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { // ends at shutdown or when the test closes inW
		_ = sb.Serve(inR, outW) // the checker reports the failure
		_ = outW.Close()
	}()
	t.Cleanup(func() { _ = inW.Close() })
	conn := protocol.NewConn(outR, inW, c.Notify, protocol.WithBadLine(c.BadLine))
	ctx := context.Background()
	go func() { _ = conn.Run(ctx) }() // ends at EOF of outR
	out := map[string]protocol.CheckResult{}
	for _, r := range c.Run(ctx, conn) {
		out[r.Check] = r
	}
	return out
}

func sandbox() *fakeadapter.Sandbox {
	return &fakeadapter.Sandbox{
		Result: protocol.InitializeResult{
			Protocol: protocol.Version,
			Features: []string{"rooms.list", "threads", "history", "typing"},
			Events:   []protocol.EventDecl{{Type: "message"}},
			Login:    []string{protocol.StepQR},
		},
		Rooms:    []protocol.Room{{ID: "r1", Name: "general"}},
		Events:   []protocol.Event{{ID: "e1", Type: "message", TS: "2026-09-28T00:00:00Z", Room: &protocol.Room{ID: "r1"}, Sender: &protocol.Sender{ID: "u1"}}},
		LoggedIn: true,
	}
}
