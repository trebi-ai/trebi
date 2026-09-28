package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Conformance timeouts of Part H.
const (
	checkInitWait     = 10 * time.Second
	checkPingWait     = 1 * time.Second
	checkShutdownWait = 5 * time.Second
)

// Check statuses.
const (
	CheckPass = "pass"
	CheckFail = "fail"
	CheckSkip = "skip"
)

// CheckResult is one row of a conformance run.
type CheckResult struct {
	Check   string `json:"check"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Checker runs the conformance checks of Part H against one adapter. The
// manifest fields are copied in, so this package stays a leaf.
type Checker struct {
	// Features are the manifest channel.features the adapter must deliver.
	Features []string
	// EventTypes are the manifest events.types; empty accepts any type.
	EventTypes []string
	// Schemas validate the data of an event, by event type.
	Schemas map[string]*jsonschema.Resolved
	// Login are the manifest setup.login step kinds.
	Login []string
	// Room is a test room the operator names; with it, the checker sends.
	Room string
	// Init is the initialize result of a live session. With Init, the
	// checker does not call initialize, initialized, or shutdown.
	Init *InitializeResult
	// SkipLogin skips auth/begin on a live instance that is logged in.
	SkipLogin bool

	mu     sync.Mutex
	events []Event
	bad    []string
}

// Notify collects the events an owned adapter emits. Pass it to NewConn.
func (c *Checker) Notify(method string, params json.RawMessage) {
	if method != MethodEvent {
		return
	}
	var e Event
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := json.Unmarshal(params, &e); err != nil {
		c.bad = append(c.bad, "event does not decode: "+err.Error())
		return
	}
	c.events = append(c.events, e)
}

// Run runs every check. The conn must already run. A failed initialize
// stops the run, because no other check can pass without a session.
func (c *Checker) Run(ctx context.Context, conn *Conn) []CheckResult {
	var out []CheckResult
	add := func(check string, err error, skip string) {
		switch {
		case skip != "":
			out = append(out, CheckResult{Check: check, Status: CheckSkip, Message: skip})
		case err != nil:
			out = append(out, CheckResult{Check: check, Status: CheckFail, Message: err.Error()})
		default:
			out = append(out, CheckResult{Check: check, Status: CheckPass})
		}
	}
	owned := c.Init == nil
	init, err := c.initialize(ctx, conn)
	add("initialize", err, "")
	if err != nil {
		return out
	}
	pctx, cancel := context.WithTimeout(ctx, checkPingWait)
	err = conn.Call(pctx, MethodPing, Empty{}, nil)
	cancel()
	add("ping", err, "")

	room := c.Room
	for _, f := range init.Features {
		skip, err := c.feature(ctx, conn, f, &room)
		add("feature "+f, err, skip)
	}
	skip, err := c.unsupported(ctx, conn, init.Features)
	add("unsupported", err, skip)
	skip, err = c.send(ctx, conn, init.Features)
	add("send once", err, skip)
	skip, err = c.login(ctx, conn, init)
	add("login", err, skip)
	add("events", c.checkEvents(init), "")
	if owned {
		add("shutdown", c.shutdown(ctx, conn), "")
	} else {
		add("shutdown", nil, "live instance")
	}
	return out
}

// initialize runs initialize, or checks the stored result of a live
// session, and checks the protocol and the promised features.
func (c *Checker) initialize(ctx context.Context, conn *Conn) (InitializeResult, error) {
	var init InitializeResult
	if c.Init != nil {
		init = *c.Init
	} else {
		ictx, cancel := context.WithTimeout(ctx, checkInitWait)
		err := conn.Call(ictx, MethodInitialize, InitializeParams{
			Protocol: Version,
			Daemon:   DaemonInfo{Version: "conformance"},
			Instance: InstanceInfo{Key: "conformance", Name: "conformance"},
		}, &init)
		cancel()
		if err != nil {
			return init, fmt.Errorf("initialize: %w", err)
		}
	}
	if init.Protocol != Version {
		return init, fmt.Errorf("protocol is %q, not %s", init.Protocol, Version)
	}
	var missing []string
	for _, f := range c.Features {
		if !slices.Contains(init.Features, f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return init, fmt.Errorf("the manifest promises features the adapter does not deliver: %s", strings.Join(missing, ", "))
	}
	if c.Init == nil {
		if err := conn.Notify(MethodInitialized, Empty{}); err != nil {
			return init, err
		}
	}
	return init, nil
}

// feature calls one read-only method of a feature and checks the shape.
// A feature that writes runs only with a test room. It returns a skip
// reason or the error.
func (c *Checker) feature(ctx context.Context, conn *Conn, f string, room *string) (string, error) {
	switch f {
	case "rooms.list":
		return "", c.rooms(ctx, conn, room)
	case "threads":
		if *room == "" {
			return "no room to list threads of", nil
		}
		var page ThreadPage
		if err := conn.Call(ctx, MethodThreadsList, ThreadsListParams{Room: *room, Limit: 5}, &page); err != nil {
			return "", err
		}
		if slices.ContainsFunc(page.Threads, func(t Thread) bool { return t.ID == "" }) {
			return "", errors.New("threads/list: a thread has no id")
		}
		return "", nil
	case "history":
		if *room == "" {
			return "no room to read", nil
		}
		var page EventPage
		err := conn.Call(ctx, MethodMessagesHistory, HistoryParams{Room: *room, Limit: 5}, &page)
		c.keep(page.Events)
		return "", err
	case "replay":
		var res ReplayResult
		err := conn.Call(ctx, MethodEventsReplay, ReplayParams{Limit: 5}, &res)
		c.keep(res.Events)
		return "", err
	case "typing":
		if c.Room == "" {
			return "writes to the account; give a test room", nil
		}
		return "", conn.Call(ctx, MethodTyping, TypingParams{Room: c.Room}, nil)
	case "rooms.open", "threads.create", "seen", "reactions", "edit":
		return "writes to the account or needs an id; not checked", nil
	case "attachments.in", "attachments.out":
		return "no method to call", nil
	}
	return "unknown feature", nil
}

// rooms lists rooms and reads the first one. With no test room, the first
// room becomes the room of the read checks.
func (c *Checker) rooms(ctx context.Context, conn *Conn, room *string) error {
	var page RoomPage
	if err := conn.Call(ctx, MethodRoomsList, RoomsListParams{Limit: 5}, &page); err != nil {
		return err
	}
	if slices.ContainsFunc(page.Rooms, func(r Room) bool { return r.ID == "" }) {
		return errors.New("rooms/list: a room has no id")
	}
	if len(page.Rooms) == 0 {
		return nil
	}
	if *room == "" {
		*room = page.Rooms[0].ID
	}
	var res RoomResult
	if err := conn.Call(ctx, MethodRoomsGet, RoomParams{Room: page.Rooms[0].ID}, &res); err != nil {
		return err
	}
	if res.Room.ID == "" {
		return errors.New("rooms/get: the room has no id")
	}
	return nil
}

// keep adds events that a read returned to the events check.
func (c *Checker) keep(events []Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, events...)
}

// probes are the requests the checker sends for a feature the adapter does
// not declare. Each must answer unsupported.
var probes = []struct { //nolint:gochecknoglobals // immutable probe table
	method string
	params any
}{
	{MethodMessagesEdit, EditParams{Room: "conformance", MessageID: "conformance", Text: "x"}},
	{MethodReactionsAdd, ReactionParams{Room: "conformance", MessageID: "conformance", Emoji: "+1"}},
	{MethodMessagesSeen, SeenParams{Room: "conformance", MessageID: "conformance"}},
	{MethodThreadsCreate, ThreadCreateParams{Room: "conformance"}},
	{MethodRoomsOpen, RoomOpenParams{User: "conformance"}},
	{MethodEventsReplay, ReplayParams{Limit: 1}},
	{MethodMessagesHistory, HistoryParams{Room: "conformance", Limit: 1}},
	{MethodThreadsList, ThreadsListParams{Room: "conformance", Limit: 1}},
	{MethodRoomsList, RoomsListParams{Limit: 1}},
	{MethodTyping, TypingParams{Room: "conformance"}},
}

// unsupported sends one request of a feature the adapter does not declare.
func (c *Checker) unsupported(ctx context.Context, conn *Conn, features []string) (string, error) {
	for _, p := range probes {
		f := FeatureOf(p.method)
		if slices.Contains(features, f) {
			continue
		}
		err := conn.Call(ctx, p.method, p.params, nil)
		if CodeOf(err) != CodeUnsupported {
			return "", fmt.Errorf("%s (feature %s not declared) answered %v, want %s", p.method, f, err, CodeUnsupported)
		}
		return "", nil
	}
	return "the adapter declares every feature", nil
}

// send sends one message twice with one key. The adapter must return one
// message id, and history, when declared, must hold the message once.
func (c *Checker) send(ctx context.Context, conn *Conn, features []string) (string, error) {
	if c.Room == "" {
		return "give a test room", nil
	}
	key := "conformance-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	p := SendParams{Room: c.Room, Text: "Trebi conformance check " + key, Format: "text", Key: key}
	var a, b SendResult
	if err := conn.Call(ctx, MethodMessagesSend, p, &a); err != nil {
		return "", err
	}
	if err := conn.Call(ctx, MethodMessagesSend, p, &b); err != nil {
		return "", err
	}
	if a.MessageID == "" || a.MessageID != b.MessageID {
		return "", fmt.Errorf("two sends with one key gave message ids %q and %q", a.MessageID, b.MessageID)
	}
	if !slices.Contains(features, "history") {
		return "", nil
	}
	var page EventPage
	if err := conn.Call(ctx, MethodMessagesHistory, HistoryParams{Room: c.Room, Limit: 20}, &page); err != nil {
		return "", err
	}
	n := 0
	for _, e := range page.Events {
		if e.ID == a.MessageID || strings.Contains(e.Text, key) {
			n++
		}
	}
	if n > 1 {
		return "", fmt.Errorf("history holds the message %d times", n)
	}
	return "", nil
}

// login starts a login and cancels it. The first step must be a declared
// kind.
func (c *Checker) login(ctx context.Context, conn *Conn, init InitializeResult) (string, error) {
	kinds := c.Login
	if len(kinds) == 0 {
		kinds = init.Login
	}
	switch {
	case len(kinds) == 0:
		return "no login", nil
	case c.SkipLogin:
		return "the instance is logged in", nil
	}
	lctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	var res AuthBeginResult
	if err := conn.Call(lctx, MethodAuthBegin, AuthBeginParams{}, &res); err != nil {
		return "", err
	}
	if res.FlowID == "" {
		return "", errors.New("auth/begin: no flow_id")
	}
	if !slices.Contains(kinds, res.Step.Kind) {
		return "", fmt.Errorf("auth/begin: step kind %q is not in %s", res.Step.Kind, strings.Join(kinds, ", "))
	}
	if err := conn.Call(lctx, MethodAuthCancel, AuthFlowParams{FlowID: res.FlowID}, nil); err != nil {
		return "", fmt.Errorf("auth/cancel: %w", err)
	}
	return "", nil
}

// checkEvents validates each event the run saw.
func (c *Checker) checkEvents(init InitializeResult) error {
	types := slices.Clone(c.EventTypes)
	if len(types) == 0 {
		for _, d := range init.Events {
			types = append(types, d.Type)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	problems := slices.Clone(c.bad)
	for _, e := range c.events {
		if p := c.eventProblem(e, types); p != "" {
			problems = append(problems, p)
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// eventProblem returns what is wrong with one event, or "".
func (c *Checker) eventProblem(e Event, types []string) string {
	switch {
	case e.ID == "":
		return "an event has no id"
	case e.Type == "":
		return "event " + e.ID + " has no type"
	case e.TS == "":
		return "event " + e.ID + " has no ts"
	case len(types) > 0 && !slices.Contains(types, e.Type):
		return fmt.Sprintf("event %s has type %q, not one of %s", e.ID, e.Type, strings.Join(types, ", "))
	case e.Room != nil && e.Room.ID == "":
		return "event " + e.ID + " has a room with no id"
	case e.Sender != nil && e.Sender.ID == "":
		return "event " + e.ID + " has a sender with no id"
	}
	if s := c.Schemas[e.Type]; s != nil && len(e.Data) > 0 {
		var data any
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return "event " + e.ID + ": data does not decode"
		}
		if err := s.Validate(data); err != nil {
			return "event " + e.ID + ": " + err.Error()
		}
	}
	return ""
}

// shutdown asks an owned adapter to stop and waits for it to exit.
func (c *Checker) shutdown(ctx context.Context, conn *Conn) error {
	sctx, cancel := context.WithTimeout(ctx, checkShutdownWait)
	defer cancel()
	if err := conn.Call(sctx, MethodShutdown, Empty{}, nil); err != nil {
		return err
	}
	select {
	case <-conn.Done():
		return nil
	case <-sctx.Done():
		return fmt.Errorf("the adapter did not exit in %s", checkShutdownWait)
	}
}
