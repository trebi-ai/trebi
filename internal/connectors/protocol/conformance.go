package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	checkLoginWait    = 15 * time.Second
	checkEchoWait     = 5 * time.Second
	checkPoll         = 20 * time.Millisecond
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

// Caller sends requests to an adapter. *Conn implements it, and so can the
// live session of the daemon.
type Caller interface {
	Call(ctx context.Context, method string, params, result any) error
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
	// Limits are the manifest channel.limits. The adapter must not promise
	// more.
	Limits Limits
	// Room is a test room the operator names; with it, the checker sends.
	Room string
	// Init is the initialize result of a live session. With Init, the
	// checker does not call initialize, initialized, or shutdown.
	Init *InitializeResult
	// SkipLogin skips auth/begin on a live instance that is logged in.
	SkipLogin bool
	// Sandbox marks an adapter that runs over a fake service. The checker
	// then logs in, writes to the first room, and waits for the self event
	// of each send.
	Sandbox bool
	// EmptyState marks a start on an empty TREBI_STATE_DIR.
	EmptyState bool
	// Dirs are the state and cache folders of an owned adapter. An
	// attachments.in path must be inside one of them. Empty skips the check.
	Dirs []string

	mu       sync.Mutex
	events   []Event
	bad      []string
	badLines []string
	statuses []Status
	done     map[string]AuthDoneParams
	sent     sentMessage
}

// sentMessage is the send of the "send once" check.
type sentMessage struct{ id, key string }

// Notify collects the events, statuses, and login results an owned adapter
// emits. Pass it to NewConn.
func (c *Checker) Notify(method string, params json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch method {
	case MethodEvent:
		var e Event
		if err := json.Unmarshal(params, &e); err != nil {
			c.bad = append(c.bad, "event does not decode: "+err.Error())
			return
		}
		c.events = append(c.events, e)
	case MethodStatus:
		var st Status
		if err := json.Unmarshal(params, &st); err != nil {
			c.bad = append(c.bad, "status does not decode: "+err.Error())
			return
		}
		c.statuses = append(c.statuses, st)
	case MethodAuthDone:
		var d AuthDoneParams
		if json.Unmarshal(params, &d) == nil && d.FlowID != "" {
			if c.done == nil {
				c.done = map[string]AuthDoneParams{}
			}
			c.done[d.FlowID] = d
		}
	case "":
		c.badLines = append(c.badLines, ErrNotJSONRPC.Error())
	}
}

// BadLine records a stdout line that is not JSON-RPC 2.0. Pass it to
// NewConn with WithBadLine.
func (c *Checker) BadLine(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.badLines = append(c.badLines, err.Error())
}

// checkRun collects the rows of one Run.
type checkRun struct{ out []CheckResult }

func (r *checkRun) add(check string, err error, skip string) {
	switch {
	case skip != "":
		r.out = append(r.out, CheckResult{Check: check, Status: CheckSkip, Message: skip})
	case err != nil:
		r.out = append(r.out, CheckResult{Check: check, Status: CheckFail, Message: err.Error()})
	default:
		r.out = append(r.out, CheckResult{Check: check, Status: CheckPass})
	}
}

// Run runs every check. The conn must already run. An owned adapter (no
// Init) needs a *Conn. A failed initialize stops the run, because no other
// check can pass without a session.
func (c *Checker) Run(ctx context.Context, conn Caller) []CheckResult {
	r := &checkRun{}
	owned := c.Init == nil
	init, err := c.Initialize(ctx, conn)
	r.add("initialize", err, "")
	if err != nil {
		return r.out
	}
	r.add("limits", c.limits(init), "")
	if owned {
		r.add("status", c.status(ctx, init), "")
	} else {
		r.add("status", nil, "live instance")
	}
	pctx, cancel := context.WithTimeout(ctx, checkPingWait)
	err = conn.Call(pctx, MethodPing, Empty{}, nil)
	cancel()
	r.add("ping", err, "")
	skip, err := c.login(ctx, conn, init)
	r.add("login", err, skip)
	if c.Sandbox {
		skip, err = c.sandboxLogin(ctx, conn, init)
		r.add("sandbox login", err, skip)
	}

	room := c.Room
	features := slices.Clone(init.Features)
	if i := slices.Index(features, "rooms.list"); i > 0 {
		features = append([]string{"rooms.list"}, slices.Delete(features, i, i+1)...)
	}
	w := &writer{c: c, conn: conn}
	for _, f := range features {
		if f == "rooms.list" {
			r.add("feature "+f, c.rooms(ctx, conn, &room), "")
			w.room = c.writeRoom(room)
			continue
		}
		skip, err := c.feature(ctx, conn, f, room, w)
		r.add("feature "+f, err, skip)
	}
	w.room = c.writeRoom(room)
	skip, err = c.unsupported(ctx, conn, init.Features)
	r.add("unsupported", err, skip)
	skip, err = c.send(ctx, conn, init.Features, w.room)
	r.add("send once", err, skip)
	skip, err = c.echo(ctx)
	r.add("events from send", err, skip)
	skip, err = c.replay(ctx, conn, init.Features)
	r.add("replay cursor", err, skip)
	r.add("error codes", c.errorCodes(ctx, conn, init.Features), "")
	skip, err = c.attachmentsIn()
	r.add("attachments in", err, skip)
	r.add("events", c.checkEvents(init), "")
	if owned {
		r.add("clean stdout", c.cleanStdout(), "")
		r.add("shutdown", c.shutdown(ctx, conn), "")
	} else {
		r.add("clean stdout", nil, "live instance")
		r.add("shutdown", nil, "live instance")
	}
	return r.out
}

// Initialize runs initialize and sends initialized, or checks the stored
// result of a live session. It checks the protocol and the promised
// features.
func (c *Checker) Initialize(ctx context.Context, conn Caller) (InitializeResult, error) {
	var init InitializeResult
	if c.Init != nil {
		init = *c.Init
	} else {
		if _, ok := conn.(*Conn); !ok {
			return init, errors.New("an owned adapter needs a *Conn")
		}
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
		if err := conn.(*Conn).Notify(MethodInitialized, Empty{}); err != nil {
			return init, err
		}
	}
	return init, nil
}

// FirstStatus waits for the first status notification of an owned adapter.
func (c *Checker) FirstStatus(ctx context.Context) (Status, bool) {
	var st Status
	ok := c.wait(ctx, checkInitWait, func() bool {
		if len(c.statuses) == 0 {
			return false
		}
		st = c.statuses[0]
		return true
	})
	return st, ok
}

// LogIn completes a sandbox login: it starts a flow, answers an input step
// with fake values, and waits for auth/done. A connected adapter needs no
// login.
func (c *Checker) LogIn(ctx context.Context, conn Caller) error {
	var st AuthStatusResult
	if err := conn.Call(ctx, MethodAuthStatus, Empty{}, &st); err != nil {
		return fmt.Errorf("auth/status: %w", err)
	}
	if st.State == StateConnected {
		return nil
	}
	var res AuthBeginResult
	bctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	err := conn.Call(bctx, MethodAuthBegin, AuthBeginParams{}, &res)
	cancel()
	if err != nil {
		return fmt.Errorf("auth/begin: %w", err)
	}
	if res.Step.Kind == StepInput {
		fields := map[string]string{}
		for _, f := range res.Step.Fields {
			fields[f.Name] = "sandbox"
		}
		if err := conn.Call(ctx, MethodAuthSubmit, AuthSubmitParams{FlowID: res.FlowID, Fields: fields}, nil); err != nil {
			return fmt.Errorf("auth/submit: %w", err)
		}
	}
	var d AuthDoneParams
	if !c.wait(ctx, checkLoginWait, func() bool {
		var ok bool
		d, ok = c.done[res.FlowID]
		return ok
	}) {
		return fmt.Errorf("no auth/done in %s", checkLoginWait)
	}
	if !d.OK {
		return fmt.Errorf("auth/done: %s", d.Error)
	}
	return nil
}

// wait polls cond under the lock until it is true, d passes, or ctx ends.
func (c *Checker) wait(ctx context.Context, d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		c.mu.Lock()
		ok := cond()
		c.mu.Unlock()
		if ok {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(checkPoll)
	}
}

// limits checks that the adapter promises no more than the manifest.
func (c *Checker) limits(init InitializeResult) error {
	var errs []string
	if max := c.Limits.MaxText; max > 0 && init.Limits.MaxText > max {
		errs = append(errs, fmt.Sprintf("max_text is %d, the manifest allows %d", init.Limits.MaxText, max))
	}
	if len(c.Limits.Formats) > 0 {
		for _, f := range init.Limits.Formats {
			if !slices.Contains(c.Limits.Formats, f) {
				errs = append(errs, fmt.Sprintf("format %q is not in the manifest formats", f))
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// status checks the first status of an owned adapter. On an empty state
// folder, an adapter with a login must ask for it.
func (c *Checker) status(ctx context.Context, init InitializeResult) error {
	st, ok := c.FirstStatus(ctx)
	if !ok {
		return fmt.Errorf("no status in %s", checkInitWait)
	}
	switch st.State {
	case StateConnecting, StateConnected, StateAuthRequired, StateAuthPending, StateRateLimited, StateError:
	default:
		return fmt.Errorf("unknown state %q", st.State)
	}
	if c.Sandbox && c.EmptyState && len(c.loginKinds(init)) > 0 && st.State != StateAuthRequired {
		return fmt.Errorf("the state folder is empty and the adapter has a login, but the first status is %q, not %s", st.State, StateAuthRequired)
	}
	return nil
}

func (c *Checker) loginKinds(init InitializeResult) []string {
	if len(c.Login) > 0 {
		return c.Login
	}
	return init.Login
}

// sandboxLogin logs the sandbox in, so the write checks can send.
func (c *Checker) sandboxLogin(ctx context.Context, conn Caller, init InitializeResult) (string, error) {
	if len(c.loginKinds(init)) == 0 {
		return "no login", nil
	}
	return "", c.LogIn(ctx, conn)
}

// writeRoom is the room the write checks use: the test room, or in the
// sandbox the first room.
func (c *Checker) writeRoom(room string) string {
	if c.Room != "" || !c.Sandbox {
		return c.Room
	}
	return room
}

// writer runs the write features on one room. It sends one message the
// first time a feature needs a message id.
type writer struct {
	c    *Checker
	conn Caller
	room string
	msg  string
}

func (w *writer) message(ctx context.Context) (string, error) {
	if w.msg != "" {
		return w.msg, nil
	}
	key := "conformance-write-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	var res SendResult
	if err := w.conn.Call(ctx, MethodMessagesSend, SendParams{Room: w.room, Text: "Trebi conformance check " + key, Format: "text", Key: key}, &res); err != nil {
		return "", err
	}
	if res.MessageID == "" {
		return "", errors.New("messages/send: no message_id")
	}
	w.msg = res.MessageID
	return w.msg, nil
}

// feature checks one feature. A read feature needs a room; a write feature
// needs a test room, or the sandbox. It returns a skip reason or the error.
func (c *Checker) feature(ctx context.Context, conn Caller, f, room string, w *writer) (string, error) {
	switch f {
	case "threads":
		if room == "" {
			return "no room to list threads of", nil
		}
		var page ThreadPage
		if err := conn.Call(ctx, MethodThreadsList, ThreadsListParams{Room: room, Limit: 5}, &page); err != nil {
			return "", err
		}
		if slices.ContainsFunc(page.Threads, func(t Thread) bool { return t.ID == "" }) {
			return "", errors.New("threads/list: a thread has no id")
		}
		return "", nil
	case "history":
		if room == "" {
			return "no room to read", nil
		}
		var page EventPage
		err := conn.Call(ctx, MethodMessagesHistory, HistoryParams{Room: room, Limit: 5}, &page)
		c.keep(page.Events)
		return "", err
	case "replay":
		var res ReplayResult
		err := conn.Call(ctx, MethodEventsReplay, ReplayParams{Limit: 5}, &res)
		c.keep(res.Events)
		return "", err
	case "attachments.in":
		return "see the attachments in check", nil
	case "rooms.open":
		return c.roomsOpen(ctx, conn)
	}
	if w.room == "" {
		if c.Sandbox {
			return "no room to write to", nil
		}
		return "writes to the account; give a test room", nil
	}
	return w.feature(ctx, f)
}

// feature runs one write feature on the room of w.
func (w *writer) feature(ctx context.Context, f string) (string, error) {
	switch f {
	case "typing":
		return "", w.conn.Call(ctx, MethodTyping, TypingParams{Room: w.room}, nil)
	case "threads.create":
		var res ThreadResult
		if err := w.conn.Call(ctx, MethodThreadsCreate, ThreadCreateParams{Room: w.room, Title: "Trebi conformance"}, &res); err != nil {
			return "", err
		}
		if res.Thread.ID == "" {
			return "", errors.New("threads/create: the thread has no id")
		}
		return "", nil
	case "attachments.out":
		return "", w.attachment(ctx)
	case "seen", "reactions", "edit":
		id, err := w.message(ctx)
		if err != nil {
			return "", err
		}
		switch f {
		case "seen":
			return "", w.conn.Call(ctx, MethodMessagesSeen, SeenParams{Room: w.room, MessageID: id}, nil)
		case "reactions":
			return "", w.conn.Call(ctx, MethodReactionsAdd, ReactionParams{Room: w.room, MessageID: id, Emoji: "👍"}, nil)
		default:
			return "", w.conn.Call(ctx, MethodMessagesEdit, EditParams{Room: w.room, MessageID: id, Text: "Trebi conformance check (edited)"}, nil)
		}
	}
	return "unknown feature", nil
}

// attachment sends one small file.
func (w *writer) attachment(ctx context.Context) error {
	f, err := os.CreateTemp("", "trebi-conformance-*.txt")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) //nolint:errcheck // a temp file; the OS cleans it up too
	_, err = f.WriteString("Trebi conformance check\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	key := "conformance-file-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	var res SendResult
	p := SendParams{Room: w.room, Text: "Trebi conformance file", Format: "text", Key: key,
		Attachments: []Attachment{{Name: filepath.Base(f.Name()), Mime: "text/plain", Path: f.Name(), Size: 24}}}
	if err := w.conn.Call(ctx, MethodMessagesSend, p, &res); err != nil {
		return err
	}
	if res.MessageID == "" {
		return errors.New("messages/send: no message_id")
	}
	return nil
}

// rooms lists rooms and reads the first one. With no test room, the first
// room becomes the room of the read checks.
func (c *Checker) rooms(ctx context.Context, conn Caller, room *string) error {
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
func (c *Checker) unsupported(ctx context.Context, conn Caller, features []string) (string, error) {
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
func (c *Checker) send(ctx context.Context, conn Caller, features []string, room string) (string, error) {
	if room == "" {
		if c.Sandbox {
			return "no room to send to", nil
		}
		return "give a test room", nil
	}
	key := "conformance-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	p := SendParams{Room: room, Text: "Trebi conformance check " + key, Format: "text", Key: key}
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
	c.mu.Lock()
	c.sent = sentMessage{id: a.MessageID, key: key}
	c.mu.Unlock()
	if !slices.Contains(features, "history") {
		return "", nil
	}
	var page EventPage
	if err := conn.Call(ctx, MethodMessagesHistory, HistoryParams{Room: room, Limit: 20}, &page); err != nil {
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

// echo waits for the self message event of the "send once" message. Only a
// sandbox must emit it.
func (c *Checker) echo(ctx context.Context) (string, error) {
	if !c.Sandbox {
		return "checked in the sandbox only", nil
	}
	c.mu.Lock()
	sent := c.sent
	c.mu.Unlock()
	if sent.id == "" {
		return "no message was sent", nil
	}
	if c.wait(ctx, checkEchoWait, func() bool {
		return slices.ContainsFunc(c.events, func(e Event) bool {
			return e.Sender != nil && e.Sender.Self && (e.ID == sent.id || strings.Contains(e.Text, sent.key))
		})
	}) {
		return "", nil
	}
	return "", fmt.Errorf("no message event with sender.self for message %s in %s", sent.id, checkEchoWait)
}

// replay asks for the events after a seen event, then after an unknown id.
func (c *Checker) replay(ctx context.Context, conn Caller, features []string) (string, error) {
	if !slices.Contains(features, "replay") {
		return "no replay feature", nil
	}
	c.mu.Lock()
	var last Event
	for _, e := range c.events {
		if e.ID != "" {
			last = e
		}
	}
	c.mu.Unlock()
	var errs []string
	if last.ID != "" {
		var res ReplayResult
		if err := conn.Call(ctx, MethodEventsReplay, ReplayParams{After: last.ID, Limit: 50}, &res); err != nil {
			return "", fmt.Errorf("events/replay after %s: %w", last.ID, err)
		}
		c.keep(res.Events)
		at, atErr := time.Parse(time.RFC3339Nano, last.TS)
		for _, e := range res.Events {
			if e.ID == last.ID {
				errs = append(errs, "events/replay after "+last.ID+" returns that event")
				continue
			}
			if ts, err := time.Parse(time.RFC3339Nano, e.TS); err == nil && atErr == nil && ts.Before(at) {
				errs = append(errs, fmt.Sprintf("events/replay after %s returns the older event %s", last.ID, e.ID))
			}
		}
	}
	unknown := "conformance-unknown-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	var res ReplayResult
	if err := conn.Call(ctx, MethodEventsReplay, ReplayParams{After: unknown, Limit: 1}, &res); err != nil {
		return "", fmt.Errorf("events/replay after an unknown id: %w", err)
	}
	if res.Complete {
		errs = append(errs, "events/replay after an unknown id returns complete: true, want false")
	}
	if len(errs) > 0 {
		return "", errors.New(strings.Join(errs, "; "))
	}
	return "", nil
}

// errorCodes checks the typed code of an unknown room and of params that
// do not decode.
func (c *Checker) errorCodes(ctx context.Context, conn Caller, features []string) error {
	var errs []string
	if slices.Contains(features, "rooms.list") {
		err := conn.Call(ctx, MethodRoomsGet, RoomParams{Room: "conformance-unknown-room"}, nil)
		if CodeOf(err) != CodeNotFound {
			errs = append(errs, fmt.Sprintf("rooms/get of an unknown room answered %v, want %s", err, CodeNotFound))
		}
	}
	err := conn.Call(ctx, MethodMessagesSend, json.RawMessage(`{"room":1,"text":2,"key":3}`), nil)
	if CodeOf(err) != CodeInvalid {
		errs = append(errs, fmt.Sprintf("messages/send with params that do not decode answered %v, want %s", err, CodeInvalid))
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// attachmentsIn checks that each attachment path of an event is inside the
// state or cache folder.
func (c *Checker) attachmentsIn() (string, error) {
	if len(c.Dirs) == 0 {
		return "the state folder is not known", nil
	}
	var roots []string
	for _, d := range c.Dirs {
		roots = append(roots, filepath.Clean(d))
		if real, err := filepath.EvalSymlinks(d); err == nil {
			roots = append(roots, real)
		}
	}
	inside := func(p string) bool {
		return slices.ContainsFunc(roots, func(root string) bool {
			rel, err := filepath.Rel(root, p)
			return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
		})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := 0
	var errs []string
	for _, e := range c.events {
		for _, a := range e.Attachments {
			if a.Path == "" {
				continue
			}
			seen++
			p := filepath.Clean(a.Path)
			if real, err := filepath.EvalSymlinks(p); err == nil {
				p = real
			}
			if !filepath.IsAbs(a.Path) || !inside(p) {
				errs = append(errs, fmt.Sprintf("event %s: attachment path %s is not inside TREBI_STATE_DIR or TREBI_CACHE_DIR", e.ID, a.Path))
			}
		}
	}
	switch {
	case len(errs) > 0:
		return "", errors.New(strings.Join(errs, "; "))
	case seen == 0:
		return "no event had an attachment path", nil
	}
	return "", nil
}

// cleanStdout fails when a stdout line was not JSON-RPC 2.0.
func (c *Checker) cleanStdout() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.badLines) == 0 {
		return nil
	}
	return fmt.Errorf("%d stdout lines are not JSON-RPC 2.0; stdout is for the protocol only, logs go to stderr: %s", len(c.badLines), c.badLines[0])
}

// login starts a login and cancels it. The first step must be a declared
// kind. It returns a skip reason or the error.
func (c *Checker) login(ctx context.Context, conn Caller, init InitializeResult) (string, error) {
	kinds := c.loginKinds(init)
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
func (c *Checker) shutdown(ctx context.Context, conn Caller) error {
	cn, ok := conn.(*Conn)
	if !ok {
		return errors.New("an owned adapter needs a *Conn")
	}
	sctx, cancel := context.WithTimeout(ctx, checkShutdownWait)
	defer cancel()
	if err := cn.Call(sctx, MethodShutdown, Empty{}, nil); err != nil {
		return err
	}
	select {
	case <-cn.Done():
		return nil
	case <-sctx.Done():
		return fmt.Errorf("the adapter did not exit in %s", checkShutdownWait)
	}
}

// roomsOpen opens a room with a user from a seen event. With no such user,
// a made-up user must give invalid.
func (c *Checker) roomsOpen(ctx context.Context, conn Caller) (string, error) {
	if !c.Sandbox {
		return "needs a user; checked in the sandbox only", nil
	}
	// A sender of a seen event is a user in the form of the platform.
	user := ""
	for _, ev := range c.events {
		if ev.Sender != nil && ev.Sender.ID != "" {
			user = ev.Sender.ID
			break
		}
	}
	if user == "" {
		err := conn.Call(ctx, MethodRoomsOpen, RoomOpenParams{User: "conformance"}, nil)
		if CodeOf(err) == CodeInvalid {
			return "no event gave a user; the adapter rejects a made-up user as invalid", nil
		}
		return "", err
	}
	var res RoomResult
	if err := conn.Call(ctx, MethodRoomsOpen, RoomOpenParams{User: user}, &res); err != nil {
		return "", err
	}
	if res.Room.ID == "" {
		return "", errors.New("rooms/open: the room has no id")
	}
	return "", nil
}
