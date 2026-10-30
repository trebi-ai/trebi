package cloudlink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

const (
	testDeviceID  = "01J8Y7ZQ5W8K3N6P9R2T4V6X8Y"
	testEmailInID = "01J8Y8A2C4E6G8H0J2K4M6N8P0"
	testWebhookID = "01J8Y9B3D5F7H9J1K3M5N7P9Q0"
	testGoodFrID  = "01J8YGOODFRAME000000000000"
	testMailMsgID = "01J8Y7ZR3A5B7C9D1E2F3G4H5J0"
	testGoodMsgID = "01J8YGOODMESSAGE0000000000"
	readWait      = 5 * time.Second
)

// mailSink records MailSink deliveries and can fail named message ids.
type mailSink struct {
	mu      sync.Mutex
	msgs    []email.IngressMessage
	failIDs map[string]bool
}

func (m *mailSink) Deliver(_ context.Context, msg email.IngressMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failIDs[msg.ID] {
		return errors.New("mail sink failed")
	}
	m.msgs = append(m.msgs, msg)
	return nil
}

func (m *mailSink) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs)
}

// hookSink records WebhookSink deliveries.
type hookSink struct {
	mu    sync.Mutex
	hooks []WebhookIngress
}

func (h *hookSink) Deliver(_ context.Context, f WebhookIngress) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hooks = append(h.hooks, f)
	return nil
}

func (h *hookSink) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.hooks)
}

// changeLog records the cached plan after every onChange fire.
type changeLog struct {
	mu    sync.Mutex
	plans []string
}

func (c *changeLog) fn(svc *Service) func(ctx context.Context) {
	return func(context.Context) {
		st := svc.Entitlement()
		c.mu.Lock()
		c.plans = append(c.plans, st.Plan)
		c.mu.Unlock()
	}
}

func (c *changeLog) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.plans...)
}

// fakeCloud is the hosted side of the wire: the register endpoint and a
// websocket that runs a per-session script and records every frame the
// daemon sends.
type fakeCloud struct {
	srv    *httptest.Server
	up     websocket.Upgrader
	script func(fc *fakeCloud, conn *websocket.Conn, session int)
	reject int // non-zero: answer the upgrade with this status

	mu       sync.Mutex
	sessions int
	seen     []map[string]any
}

func (fc *fakeCloud) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/devices":
		_, _ = w.Write([]byte(`{"device_id":"` + testDeviceID + `"}`))
	case r.URL.Path == "/v1/socket":
		fc.upgrade(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (fc *fakeCloud) upgrade(w http.ResponseWriter, r *http.Request) {
	fc.mu.Lock()
	fc.sessions++
	session := fc.sessions
	fc.mu.Unlock()
	if fc.reject != 0 {
		w.WriteHeader(fc.reject)
		return
	}
	conn, err := fc.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	if fc.script != nil {
		fc.script(fc, conn, session)
	}
}

// recv reads one frame from the daemon and records it.
func (fc *fakeCloud) recv(conn *websocket.Conn) (map[string]any, bool) {
	_ = conn.SetReadDeadline(time.Now().Add(readWait))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	fc.mu.Lock()
	fc.seen = append(fc.seen, m)
	fc.mu.Unlock()
	return m, true
}

// drain records frames until the peer closes or d passes; used to settle
// a session before dropping it and to hold a session open.
func (fc *fakeCloud) drain(conn *websocket.Conn, d time.Duration) {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(d))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		fc.mu.Lock()
		fc.seen = append(fc.seen, m)
		fc.mu.Unlock()
	}
}

func (fc *fakeCloud) send(conn *websocket.Conn, v any) {
	_ = conn.WriteJSON(v)
}

func (fc *fakeCloud) countType(typ string) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	n := 0
	for _, m := range fc.seen {
		if m["type"] == typ {
			n++
		}
	}
	return n
}

func (fc *fakeCloud) ackIDs() []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var out []string
	for _, m := range fc.seen {
		if m["type"] == "ack" {
			id, _ := m["id"].(string)
			out = append(out, id)
		}
	}
	return out
}

func (fc *fakeCloud) sessionCount() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.sessions
}

// TestSocketDeliversAcksAndReconnects is the read loop contract: hello
// first, the plan/features write on hello_ok and the entitlement change,
// one ack per delivered ingress carrying the frame id, the unknown type
// counted and ignored, and a reconnect after the drop.
func TestSocketDeliversAcksAndReconnects(t *testing.T) {
	testHome(t)
	st := openStore(t)
	mail := &mailSink{failIDs: map[string]bool{}}
	hooks := &hookSink{}
	changes := &changeLog{}

	emailRaw := mustRaw(t, email.IngressMessage{
		V: ContractV, ID: testMailMsgID, LocalPart: "ellis-x7k2qm4p",
		From: "fritz@example.com", To: []string{"ellis-x7k2qm4p@mail.example.com"},
		Subject: "Weekly report", Text: "Please run the weekly report.",
		ReceivedAt: "2026-09-22T10:00:00Z",
	})
	hookRaw := mustRaw(t, WebhookIngress{
		V: ContractV, ID: testWebhookID, Method: "POST", Path: "/deploy",
		ContentType: "application/json",
		Headers:     map[string]string{"x-github-event": "push"},
		Body:        `{"ref":"refs/heads/main"}`,
		ReceivedAt:  "2026-09-22T10:00:00Z",
	})

	fc := newFakeCloud(t, func(fc *fakeCloud, conn *websocket.Conn, session int) {
		if session >= 2 {
			fc.recv(conn)               // the second hello proves the reconnect
			fc.drain(conn, time.Minute) // hold open until the client stops
			return
		}
		fc.recv(conn) // hello
		fc.send(conn, HelloOK{
			V: ContractV, Type: "hello_ok", Plan: "pro_cloud",
			Features:          []string{"watchdog", "remote_access", "hosted_ingress", "backup"},
			HeartbeatInterval: 60,
		})
		fc.send(conn, Ingress{V: ContractV, Type: "ingress", ID: testEmailInID, Kind: "email", Payload: emailRaw})
		fc.send(conn, Ingress{V: ContractV, Type: "ingress", ID: testWebhookID, Kind: "webhook", Payload: hookRaw})
		fc.send(conn, Entitlement{V: ContractV, Type: "entitlement", Plan: "free", Features: []string{"watchdog"}})
		fc.send(conn, map[string]any{"v": ContractV, "type": "mystery"})
		// Both acks prove the ingress handlers committed. Settle before
		// dropping the connection.
		for i := 0; i < 50 && fc.countType("ack") < 2; i++ {
			if _, ok := fc.recv(conn); !ok {
				return
			}
		}
		fc.drain(conn, 50*time.Millisecond)
	})

	svc := New(st)
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: true})
	svc.WithMail(mail).WithWebhooks(hooks).WithOnChange(changes.fn(svc))
	defer svc.Stop()

	ctx := context.Background()
	if got := svc.Status().Status; got != statusNotLinked {
		t.Fatalf("status before link = %q", got)
	}
	if _, err := svc.Link(ctx, "ABCD-EFGH", "mac-mini"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "connected status", func() bool { return svc.Status().Status == statusConnected })
	waitFor(t, "reconnect", func() bool { return fc.sessionCount() >= 2 })

	// The first frame out is hello.
	if fc.countType("hello") < 1 {
		t.Fatal("no hello frame seen")
	}
	// Exactly one delivery per sink.
	if mail.count() != 1 {
		t.Fatalf("mail deliveries = %d, want 1", mail.count())
	}
	if hooks.count() != 1 {
		t.Fatalf("webhook deliveries = %d, want 1", hooks.count())
	}
	// Both acks carry the frame ids, in read order.
	acks := fc.ackIDs()
	if len(acks) != 2 || acks[0] != testEmailInID || acks[1] != testWebhookID {
		t.Fatalf("acks = %v, want [%s %s]", acks, testEmailInID, testWebhookID)
	}
	// The unknown type was counted and ignored.
	svc.sock.mu.Lock()
	unknown := svc.sock.unknown
	svc.sock.mu.Unlock()
	if unknown != 1 {
		t.Fatalf("unknown frames = %d, want 1", unknown)
	}
	// The entitlement change is in the row: hello_ok's write was
	// overwritten by the entitlement frame.
	ent := svc.Entitlement()
	if ent.Plan != "free" || len(ent.Features) != 1 || ent.Features[0] != "watchdog" {
		t.Fatalf("entitlement = %+v", ent)
	}
	if ent.RefreshedAt.IsZero() {
		t.Fatal("refreshed_at not refreshed")
	}
	// onChange fired after Link and both plan/features writes.
	plans := changes.snapshot()
	if len(plans) != 3 {
		t.Fatalf("onChange fires = %v, want 3 (link, hello_ok, entitlement)", plans)
	}
	joined := strings.Join(plans, ",")
	if !strings.Contains(joined, "pro_cloud") || !strings.Contains(joined, "free") {
		t.Fatalf("onChange plans = %v, want pro_cloud and free", plans)
	}
	// Bus: two entitlement frames, one connect, one loss.
	if n := busCount(t, st, "cloud.entitlement"); n != 2 {
		t.Errorf("cloud.entitlement events = %d, want 2", n)
	}
	if n := busCount(t, st, "cloud.connected"); n != 1 {
		t.Errorf("cloud.connected events = %d, want 1", n)
	}
	if n := busCount(t, st, "cloud.disconnected"); n != 1 {
		t.Errorf("cloud.disconnected events = %d, want 1", n)
	}
	// After the drop and the reconnect (no hello_ok in session 2) the
	// socket is down but the link stands.
	if got := svc.Status(); got.Status != statusDegraded || !got.Linked || got.Connected {
		t.Fatalf("status after drop = %+v", got)
	}
}

// TestSocketAckSkippedOnSinkError: a sink error leaves the frame unacked
// so the cloud redelivers it. The good frame's ack proves the failing
// frame was processed and deliberately not acked.
func TestSocketAckSkippedOnSinkError(t *testing.T) {
	testHome(t)
	st := openStore(t)
	mail := &mailSink{failIDs: map[string]bool{testMailMsgID: true}}

	failing := mustRaw(t, email.IngressMessage{
		V: ContractV, ID: testMailMsgID, Subject: "failing", ReceivedAt: "2026-09-22T10:00:00Z",
	})
	good := mustRaw(t, email.IngressMessage{
		V: ContractV, ID: testGoodMsgID, Subject: "good", ReceivedAt: "2026-09-22T10:00:01Z",
	})

	fc := newFakeCloud(t, func(fc *fakeCloud, conn *websocket.Conn, session int) {
		if session >= 2 {
			fc.recv(conn)
			fc.drain(conn, time.Minute)
			return
		}
		fc.recv(conn) // hello
		fc.send(conn, HelloOK{V: ContractV, Type: "hello_ok", Plan: "free", Features: []string{"watchdog"}, HeartbeatInterval: 60})
		fc.send(conn, Ingress{V: ContractV, Type: "ingress", ID: testEmailInID, Kind: "email", Payload: failing})
		fc.send(conn, Ingress{V: ContractV, Type: "ingress", ID: testGoodFrID, Kind: "email", Payload: good})
		for i := 0; i < 50 && fc.countType("ack") < 1; i++ {
			if _, ok := fc.recv(conn); !ok {
				return
			}
		}
		fc.drain(conn, 50*time.Millisecond)
	})

	svc := New(st)
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: true})
	svc.WithMail(mail)
	defer svc.Stop()
	if _, err := svc.Link(context.Background(), "ABCD-EFGH", "mac-mini"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the good ack", func() bool { return len(fc.ackIDs()) >= 1 })
	if acks := fc.ackIDs(); len(acks) != 1 || acks[0] != testGoodFrID {
		t.Fatalf("acks = %v, want only the settled frame %s", acks, testGoodFrID)
	}
	if mail.count() != 1 {
		t.Fatalf("mail deliveries = %d, want only the good frame", mail.count())
	}
}

// TestSocketUnauthorizedStopsRedial: a 401 on the dial (the device was
// revoked in the cloud) marks the link unauthorized and must not become a
// reconnect loop.
func TestSocketUnauthorizedStopsRedial(t *testing.T) {
	testHome(t)
	st := openStore(t)
	fc := newFakeCloud(t, nil)
	fc.reject = http.StatusUnauthorized

	svc := New(st)
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: true})
	defer svc.Stop()
	if _, err := svc.Link(context.Background(), "ABCD-EFGH", "mac-mini"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "unauthorized status", func() bool { return svc.Status().Status == statusUnauthorized })
	// Longer than the first backoff window: a reconnect loop would dial again.
	time.Sleep(1200 * time.Millisecond)
	if n := fc.sessionCount(); n != 1 {
		t.Fatalf("dial attempts = %d, want 1 (no redial after 401)", n)
	}
	// It was never connected, so there is no loss to publish.
	if n := busCount(t, st, "cloud.disconnected"); n != 0 {
		t.Errorf("cloud.disconnected events = %d, want 0", n)
	}
	if got := svc.Status(); !got.Linked || got.Connected {
		t.Fatalf("status = %+v", got)
	}
}

// testHome points home at a temp dir so MintIdentity writes there.
func testHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// mustRaw encodes a wire value on the test goroutine.
func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newFakeCloud(t *testing.T, script func(fc *fakeCloud, conn *websocket.Conn, session int)) *fakeCloud {
	t.Helper()
	fc := &fakeCloud{script: script}
	fc.srv = httptest.NewServer(http.HandlerFunc(fc.handle))
	t.Cleanup(fc.srv.Close)
	return fc
}

func busCount(t *testing.T, st *store.Store, topic string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic = ?`, topic).Scan(&n); err != nil {
		t.Fatalf("bus count %s: %v", topic, err)
	}
	return n
}

// relaySink records hub calls by sid.
type relaySink struct {
	mu      sync.Mutex
	opened  map[string]string
	frames  map[string][]string
	closed  map[string]string
	revoked []string
}

func (r *relaySink) OpenRelay(_ context.Context, sid, clientID string) {
	r.mu.Lock()
	r.opened[sid] = clientID
	r.mu.Unlock()
}

func (r *relaySink) DeliverRelay(_ context.Context, sid string, raw []byte) {
	r.mu.Lock()
	r.frames[sid] = append(r.frames[sid], string(raw))
	r.mu.Unlock()
}

func (r *relaySink) CloseRelay(sid, reason string) {
	r.mu.Lock()
	r.closed[sid] = reason
	r.mu.Unlock()
}

func (r *relaySink) ClientRevoked(_ context.Context, clientID string) {
	r.mu.Lock()
	r.revoked = append(r.revoked, clientID)
	r.mu.Unlock()
}

func (r *relaySink) Count() int { return 0 }

func (r *relaySink) snapshot(fn func(r *relaySink) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fn(r)
}

// TestSocketRelayRouting: frames reach the session of their own sid,
// SendRelay writes relay_frame{sid}, and a lost socket closes every sid.
func TestSocketRelayRouting(t *testing.T) {
	testHome(t)
	st := openStore(t)
	sink := newRelaySink()
	sent := make(chan map[string]any, 4)

	fc := newFakeCloud(t, func(fc *fakeCloud, conn *websocket.Conn, session int) {
		if session >= 2 {
			fc.drain(conn, time.Minute)
			return
		}
		fc.recv(conn) // hello
		fc.send(conn, HelloOK{V: ContractV, Type: "hello_ok", Plan: "pro_cloud", Features: []string{"remote_access"}, HeartbeatInterval: 60})
		fc.send(conn, RelayOpen{V: ContractV, Type: "relay_open", SID: "sa", ClientID: "ca"})
		fc.send(conn, RelayOpen{V: ContractV, Type: "relay_open", SID: "sb", ClientID: "cb"})
		fc.send(conn, RelayFrame{V: ContractV, Type: "relay_frame", SID: "sa", Bytes: "YQ=="})
		fc.send(conn, RelayFrame{V: ContractV, Type: "relay_frame", SID: "sb", Bytes: "Yg=="})
		fc.send(conn, RelayFrame{V: ContractV, Type: "relay_frame", SID: "zz", Bytes: "eg=="})
		fc.send(conn, RelayFrame{V: ContractV, Type: "relay_frame", SID: "sa", Bytes: "!bad"})
		fc.send(conn, RelayClose{V: ContractV, Type: "relay_close", SID: "sb", Reason: "client_gone"})
		fc.send(conn, ClientRevoked{V: ContractV, Type: "client_revoked", ClientID: "cb"})
		for {
			f, ok := fc.recv(conn)
			if !ok {
				return
			}
			if f["type"] == "relay_frame" {
				sent <- f
				return // drop the socket
			}
		}
	})

	svc := New(st).WithRelay(sink)
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: true, Mobile: true})
	defer svc.Stop()
	if _, err := svc.Link(context.Background(), "ABCD-EFGH", "mac-mini"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "relay close of sb", func() bool {
		return sink.snapshot(func(r *relaySink) bool { return r.closed["sb"] == "client_gone" && len(r.revoked) == 1 })
	})
	if err := svc.SendRelay("sa", []byte("reply")); err != nil {
		t.Fatal(err)
	}
	f := <-sent
	if f["sid"] != "sa" || f["bytes"] != "cmVwbHk=" {
		t.Fatalf("relay_frame out = %v", f)
	}
	waitFor(t, "socket_lost close of sa", func() bool {
		return sink.snapshot(func(r *relaySink) bool { return r.closed["sa"] == "socket_lost" })
	})
	sink.snapshot(func(r *relaySink) bool {
		if r.opened["sa"] != "ca" || r.opened["sb"] != "cb" {
			t.Errorf("opened = %v", r.opened)
		}
		if strings.Join(r.frames["sa"], ",") != "a" || strings.Join(r.frames["sb"], ",") != "b" || len(r.frames["zz"]) != 0 {
			t.Errorf("frames = %v", r.frames)
		}
		if r.revoked[0] != "cb" {
			t.Errorf("revoked = %v", r.revoked)
		}
		return true
	})
}

// TestSocketHelloCapsMobile: caps holds mobile only with cloud.mobile on and
// a hub; a toggle redials with the new caps. With mobile off, relay_open is refused.
func TestSocketHelloCapsMobile(t *testing.T) {
	testHome(t)
	st := openStore(t)
	hellos := make(chan []any, 4)
	refused := make(chan string, 1)
	fc := newFakeCloud(t, func(fc *fakeCloud, conn *websocket.Conn, session int) {
		f, ok := fc.recv(conn)
		if !ok {
			return
		}
		caps, _ := f["caps"].([]any)
		hellos <- caps
		if session == 2 {
			fc.send(conn, RelayOpen{V: ContractV, Type: "relay_open", SID: "s1", ClientID: "c1"})
			if f, ok := fc.recv(conn); ok && f["type"] == "relay_close" {
				refused <- f["reason"].(string)
			}
		}
		fc.drain(conn, time.Minute)
	})
	svc := New(st).WithRelay(newRelaySink())
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: true, Mobile: true})
	defer svc.Stop()
	if _, err := svc.Link(context.Background(), "ABCD-EFGH", "mac-mini"); err != nil {
		t.Fatal(err)
	}
	if caps := <-hellos; len(caps) != 1 || caps[0] != CapMobile {
		t.Fatalf("caps = %v, want [mobile]", caps)
	}
	svc.SetMobile(false)
	if caps := <-hellos; len(caps) != 0 {
		t.Fatalf("caps after off = %v, want none", caps)
	}
	if got := <-refused; got != "refused" {
		t.Fatalf("relay_open with mobile off: reason %q", got)
	}
	if svc.Status().Mobile {
		t.Fatal("status mobile = true after off")
	}
}

func newRelaySink() *relaySink {
	return &relaySink{opened: map[string]string{}, frames: map[string][]string{}, closed: map[string]string{}}
}

// TestSocketFailsOverOnDialError: a refused dial on the primary address
// dials the backup address at once, and the bus records the move.
func TestSocketFailsOverOnDialError(t *testing.T) {
	testHome(t)
	st := openStore(t)
	fc := newFakeCloud(t, func(fc *fakeCloud, conn *websocket.Conn, _ int) {
		fc.recv(conn)
		fc.send(conn, HelloOK{V: ContractV, Type: "hello_ok", Plan: "pro_cloud", HeartbeatInterval: 60})
		fc.drain(conn, time.Minute)
	})
	svc := New(st)
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: false})
	if _, err := svc.Link(context.Background(), "ABCD-EFGH", "mac-mini"); err != nil {
		t.Fatal(err)
	}
	svc.Configure(Settings{APIURLs: []string{deadURL(t), fc.srv.URL}, Enabled: true})
	svc.Start(context.Background())
	defer svc.Stop()

	waitFor(t, "connected through the backup", func() bool { return svc.Status().Connected })
	got := svc.Status()
	if !got.Fallback || got.Endpoint != strings.TrimPrefix(fc.srv.URL, "http://") {
		t.Fatalf("status = %+v", got)
	}
	if n := busCount(t, st, "cloud.endpoint_switched"); n != 1 {
		t.Fatalf("cloud.endpoint_switched events = %d, want 1", n)
	}
}

// TestSocketUnauthorizedKeepsAddress: a 401 on the primary address marks
// the link unauthorized and never tries the backup address.
func TestSocketUnauthorizedKeepsAddress(t *testing.T) {
	testHome(t)
	st := openStore(t)
	fc := newFakeCloud(t, nil)
	fc.reject = http.StatusUnauthorized
	backup := newFakeCloud(t, nil)

	svc := New(st)
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL, backup.srv.URL}, Enabled: true})
	defer svc.Stop()
	if _, err := svc.Link(context.Background(), "ABCD-EFGH", "mac-mini"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "unauthorized status", func() bool { return svc.Status().Status == statusUnauthorized })
	if backup.sessionCount() != 0 || svc.Status().Fallback {
		t.Fatalf("backup dialed after a 401: sessions %d", backup.sessionCount())
	}
}

// capSource returns the summary with the current version.
type capSource struct {
	mu      sync.Mutex
	version string
}

func (c *capSource) CloudCapabilities(context.Context) (Capabilities, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Capabilities{OS: "darwin", Arch: "arm64", Version: c.version, Harnesses: []CapabilityItem{{Name: "claude", State: "ready"}}, Connectors: []CapabilityItem{}}, nil
}

func (c *capSource) set(v string) {
	c.mu.Lock()
	c.version = v
	c.mu.Unlock()
}

// TestSocketHeartbeatCapabilities: the first heartbeat carries the summary,
// the next one does not, and a change sends it again.
func TestSocketHeartbeatCapabilities(t *testing.T) {
	testHome(t)
	st := openStore(t)
	src := &capSource{version: "v1"}
	beats := make(chan map[string]any, 8)
	fc := newFakeCloud(t, func(fc *fakeCloud, conn *websocket.Conn, session int) {
		fc.recv(conn) // hello
		fc.send(conn, HelloOK{V: ContractV, Type: "hello_ok", Plan: "free", HeartbeatInterval: 1})
		for {
			m, ok := fc.recv(conn)
			if !ok {
				return
			}
			if m["type"] == "heartbeat" {
				beats <- m
			}
		}
	})
	svc := New(st).WithCapabilities(src)
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: true})
	defer svc.Stop()
	if _, err := svc.Link(context.Background(), "ABCD-EFGH", "mac-mini"); err != nil {
		t.Fatal(err)
	}
	next := func() map[string]any {
		select {
		case m := <-beats:
			return m
		case <-time.After(10 * time.Second):
			t.Fatal("no heartbeat")
		}
		return nil
	}
	first := next()
	caps, _ := first["capabilities"].(map[string]any)
	if caps["version"] != "v1" || caps["os"] != "darwin" {
		t.Fatalf("first heartbeat = %v", first)
	}
	if m := next(); m["capabilities"] != nil {
		t.Fatalf("second heartbeat repeats the summary: %v", m)
	}
	src.set("v2")
	for range 3 { // a beat built before the change can come first
		if caps, _ := next()["capabilities"].(map[string]any); caps["version"] == "v2" {
			return
		}
	}
	t.Fatal("no summary after the change")
}
