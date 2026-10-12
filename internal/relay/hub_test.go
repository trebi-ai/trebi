package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/flynn/noise"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/trebi-ai/trebi/internal/mobile"
	"github.com/trebi-ai/trebi/internal/tools"
)

const (
	testDevice = "01JA2B3C4D5E6F7G8H9J0K1DEV"
	testHost   = "127.0.0.1:43123"
	wait       = 5 * time.Second
)

// fakeBook is an in-memory KeyBook.
type fakeBook struct {
	mu       sync.Mutex
	offers   map[string][]byte
	clients  map[string]mobile.Client // by string(pub)
	redeemed int
	seen     int
	revoked  []string
}

func (b *fakeBook) Lookup(pub []byte) (mobile.Client, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.clients[string(pub)]
	return c, ok
}

func (b *fakeBook) OfferPSK(id string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.offers[id]
	return p, ok
}

func (b *fakeBook) Redeem(_ context.Context, offerID string, in mobile.Enrollment, psk []byte) (mobile.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.offers[offerID]; !ok {
		return mobile.Client{}, mobile.ErrNotFound
	}
	delete(b.offers, offerID)
	c := mobile.Client{ID: "local-" + in.CloudClientID, CloudClientID: in.CloudClientID, Name: in.Name, Platform: in.Platform,
		PublicKey: in.PublicKey, PSK: psk, Scope: mobile.ScopeFull}
	b.clients[string(in.PublicKey)] = c
	b.redeemed++
	return c, nil
}

func (b *fakeBook) Seen(string, time.Time) {
	b.mu.Lock()
	b.seen++
	b.mu.Unlock()
}

func (b *fakeBook) Revoked(_ context.Context, cloudClientID string) {
	b.mu.Lock()
	b.revoked = append(b.revoked, cloudClientID)
	for k, c := range b.clients {
		if c.CloudClientID == cloudClientID {
			delete(b.clients, k)
		}
	}
	b.mu.Unlock()
}

// fakeCloud routes daemon frames to the phone of each sid.
type fakeCloud struct {
	mu     sync.Mutex
	out    map[string]chan []byte
	closed map[string]chan string
}

func (c *fakeCloud) SendRelay(sid string, raw []byte) error {
	out, _ := c.chans(sid)
	select {
	case out <- append([]byte(nil), raw...):
		return nil
	default:
		return errors.New("fake cloud: queue full")
	}
}

func (c *fakeCloud) CloseRelay(sid, reason string) error {
	_, closed := c.chans(sid)
	select {
	case closed <- reason:
	default:
	}
	return nil
}

func (c *fakeCloud) DeviceID() string   { return testDevice }
func (c *fakeCloud) DeviceName() string { return "Test Mac" }

type fakeGate struct {
	off   atomic.Bool
	block atomic.Pointer[chan struct{}]
}

func (g *fakeGate) MobileAllowed() bool {
	if ch := g.block.Load(); ch != nil {
		<-*ch
	}
	return !g.off.Load()
}

type fakePub struct {
	mu     sync.Mutex
	events []string
}

func (p *fakePub) Connected(_ context.Context, id string, on bool) {
	p.mu.Lock()
	p.events = append(p.events, fmt.Sprintf("%s:%v", id, on))
	p.mu.Unlock()
}

type rig struct {
	hub   *Hub
	book  *fakeBook
	cloud *fakeCloud
	gate  *fakeGate
	pub   *fakePub
	key   noise.DHKey
	seq   atomic.Int64
}

// phone is the Go initiator of one session.
type phone struct {
	sid     string
	welcome Welcome
	send    *noise.CipherState
	recv    *noise.CipherState
	out     chan []byte
	closed  chan string
	r       *rig
	sendMu  sync.Mutex
	mux     *yamux.Session
}

type dialOpts struct {
	key      noise.DHKey
	cloudID  string
	helloID  string // client_id in the payload; "" means cloudID
	offer    string
	psk      []byte
	platform string
}

type closeErr string

func (e closeErr) Error() string { return "closed: " + string(e) }

func TestHandshakePairAndReconnect(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	key, cpsk, p := r.pair(t, "cloud-a")
	if p.welcome.Machine != "Test Mac" || p.welcome.Scope != mobile.ScopeFull || p.welcome.V != ProtocolV {
		t.Fatalf("welcome = %+v", p.welcome)
	}
	if r.book.redeems() != 1 {
		t.Fatalf("redeems = %d", r.book.redeems())
	}
	waitUntil(t, "connected", func() bool { return r.hub.Connected("local-cloud-a") })

	// The offer is spent: a second phone with the same offer is refused.
	if _, err := r.dial(t, dialOpts{key: newKey(t), cloudID: "cloud-b", offer: "offer-cloud-a", psk: cpsk}); err == nil {
		t.Fatal("spent offer accepted")
	}

	// Reconnect with the client psk and no offer.
	p2, err := r.dial(t, dialOpts{key: key, cloudID: "cloud-a", psk: cpsk})
	if err != nil {
		t.Fatal(err)
	}
	if p2.welcome.ClientPSK != "" {
		t.Fatal("reconnect got a new client_psk")
	}
	if r.book.redeems() != 1 {
		t.Fatalf("reconnect redeemed again")
	}
	if r.pub.count() < 2 {
		t.Fatalf("connected events = %d", r.pub.count())
	}
}

func TestHandshakeRefusals(t *testing.T) {
	t.Parallel()
	r := newRig(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached on a refused session")
	}))
	knownKey, cpsk, _ := r.pair(t, "cloud-known")
	before := r.book.redeems()

	cases := []struct {
		name string
		opts func() dialOpts
		gate bool // gate off
	}{
		{"unknown key no offer", func() dialOpts { return dialOpts{key: newKey(t), cloudID: "c1", psk: cpsk} }, false},
		{"unknown offer", func() dialOpts { return dialOpts{key: newKey(t), cloudID: "c2", offer: "nope", psk: cpsk} }, false},
		{"wrong psk", func() dialOpts {
			r.offer("o-wrong")
			return dialOpts{key: newKey(t), cloudID: "c3", offer: "o-wrong", psk: make([]byte, 32)}
		}, false},
		{"known key wrong psk", func() dialOpts { return dialOpts{key: knownKey, cloudID: "cloud-known", psk: make([]byte, 32)} }, false},
		{"client id mismatch", func() dialOpts {
			psk := r.offer("o-mis")
			return dialOpts{key: newKey(t), cloudID: "c4", helloID: "other", offer: "o-mis", psk: psk}
		}, false},
		{"known key other cloud id", func() dialOpts { return dialOpts{key: knownKey, cloudID: "c5", psk: cpsk} }, false},
		{"gate off", func() dialOpts {
			psk := r.offer("o-gate")
			return dialOpts{key: newKey(t), cloudID: "c6", offer: "o-gate", psk: psk}
		}, true},
	}
	for _, tc := range cases {
		r.gate.off.Store(tc.gate)
		_, err := r.dial(t, tc.opts())
		var ce closeErr
		if !errors.As(err, &ce) || string(ce) != ReasonRefused {
			t.Errorf("%s: err = %v, want refused", tc.name, err)
		}
	}
	r.gate.off.Store(false)
	if r.book.redeems() != before {
		t.Fatalf("refused sessions redeemed: %d", r.book.redeems()-before)
	}
	waitUntil(t, "sessions dropped", func() bool { return r.hub.Count() == 1 })
}

func TestRecordsSplitAndOrder(t *testing.T) {
	t.Parallel()
	r := newRig(t, echoRoot())
	_, _, p := r.pair(t, "cloud-a")
	body := make([]byte, 1<<20)
	_, _ = rand.Read(body)
	resp, err := p.http(t).Post("http://"+testHost+"/echo", "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("echo: %d bytes, equal=%v", len(got), bytes.Equal(got, body))
	}

	for _, name := range []string{"swap", "replay"} {
		_, _, q := r.pair(t, "cloud-"+name)
		var r1, r2 []byte
		q.sendMu.Lock()
		r1, _ = SealRecord(q.send, recordMux, []byte("one"))
		r2, _ = SealRecord(q.send, recordMux, []byte("two"))
		q.sendMu.Unlock()
		if name == "swap" {
			r.hub.DeliverRelay(context.Background(), q.sid, r2)
			r.hub.DeliverRelay(context.Background(), q.sid, r1)
		} else {
			r.hub.DeliverRelay(context.Background(), q.sid, r1)
			r.hub.DeliverRelay(context.Background(), q.sid, r1)
		}
		select {
		case reason := <-q.closed:
			if reason != ReasonRefused {
				t.Errorf("%s: close reason %q", name, reason)
			}
		case <-time.After(wait):
			t.Fatalf("%s: session not closed", name)
		}
	}
}

func TestSessionServesRoot(t *testing.T) {
	t.Parallel()
	fs := fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Trebi</title>")}}
	var actor atomic.Value
	var leaked atomic.Bool
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/system/health", func(w http.ResponseWriter, r *http.Request) {
		a, _ := tools.ActorFrom(r.Context())
		actor.Store(a)
		if r.Header.Get(tools.HeaderRunToken) != "" || r.Header.Get(tools.HeaderRunID) != "" {
			leaked.Store(true)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	m.Handle("GET /", http.FileServerFS(fs))
	r := newRig(t, m)
	_, _, p := r.pair(t, "cloud-a")
	c := p.http(t)

	req, _ := http.NewRequest(http.MethodGet, "http://"+testHost+"/v1/system/health", nil)
	req.Header.Set(tools.HeaderRunToken, "stolen")
	req.Header.Set(tools.HeaderRunID, "run-1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health = %d", resp.StatusCode)
	}
	a, _ := actor.Load().(tools.Actor)
	if a.Kind != tools.ActorMobile || a.Name != "mobile:Test iPad" {
		t.Fatalf("actor = %+v", a)
	}
	if leaked.Load() {
		t.Fatal("run headers reached the handler")
	}

	resp, err = c.Get("http://" + testHost + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "<title>Trebi</title>") {
		t.Fatalf("index = %d %q", resp.StatusCode, b)
	}
}

func TestSessionSSE(t *testing.T) {
	t.Parallel()
	fire := make(chan struct{})
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-fire:
			_, _ = io.WriteString(w, "data: {\"topic\":\"run.started\"}\n\n")
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			return
		}
		<-r.Context().Done()
	})
	r := newRig(t, m)
	_, _, p := r.pair(t, "cloud-a")
	resp, err := (&http.Client{Transport: p.http(t).Transport}).Get("http://" + testHost + "/v1/stream?follow=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	start := time.Now()
	close(fire)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("event took %v", d)
	}
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("line = %q", line)
	}
}

func TestSessionWebSocket(t *testing.T) {
	t.Parallel()
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "http://"+r.Host }}
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/runs/{id}/attach", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			typ, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(typ, append([]byte("echo:"), msg...))
		}
	})
	r := newRig(t, m)
	_, _, p := r.pair(t, "cloud-a")
	mux := p.open(t)
	d := websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) { return mux.Open() }}
	hdr := http.Header{"Origin": {"http://" + testHost}}
	ws, _, err := d.Dial("ws://"+testHost+"/v1/runs/r1/attach", hdr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close() }()
	if err := ws.WriteMessage(websocket.TextMessage, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(wait))
	_, msg, err := ws.ReadMessage()
	if err != nil || string(msg) != "echo:hi" {
		t.Fatalf("ws = %q %v", msg, err)
	}
}

func TestSessionParallelStreams(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	m := http.NewServeMux()
	m.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("slow"))
	})
	m.HandleFunc("GET /fast/{n}", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.PathValue("n"))) })
	r := newRig(t, m)
	_, _, p := r.pair(t, "cloud-a")
	c := p.http(t)

	slow := make(chan error, 1)
	go func() {
		resp, err := c.Get("http://" + testHost + "/slow")
		if err == nil {
			_ = resp.Body.Close()
		}
		slow <- err
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 19)
	for i := range 19 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.Get(fmt.Sprintf("http://%s/fast/%d", testHost, i))
			if err != nil {
				errs <- err
				return
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(b) != fmt.Sprint(i) {
				errs <- fmt.Errorf("fast %d = %q", i, b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	select {
	case <-slow:
		t.Fatal("slow stream finished before release")
	default:
	}
	close(release)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
}

func TestHubLimits(t *testing.T) {
	t.Parallel()
	m := http.NewServeMux()
	m.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	r := newRig(t, m)
	key, cpsk, _ := r.pair(t, "cloud-a")
	for i := 1; i < MaxSessionsPerClient; i++ {
		if _, err := r.dial(t, dialOpts{key: key, cloudID: "cloud-a", psk: cpsk}); err != nil {
			t.Fatalf("session %d: %v", i+1, err)
		}
	}
	_, err := r.dial(t, dialOpts{key: key, cloudID: "cloud-a", psk: cpsk})
	var ce closeErr
	if !errors.As(err, &ce) || string(ce) != ReasonRefused {
		t.Fatalf("9th session of one phone: %v", err)
	}

	// A phone with a full inbound queue ends only its own session.
	_, _, good := r.pair(t, "cloud-good")
	block := make(chan struct{})
	r.gate.block.Store(&block)
	sid := r.newSID()
	_, closed := r.cloud.chans(sid)
	r.hub.OpenRelay(context.Background(), sid, "cloud-flood")
	for range queueFrames + 1 {
		r.hub.DeliverRelay(context.Background(), sid, []byte("x"))
	}
	select {
	case reason := <-closed:
		if reason != ReasonLimit {
			t.Fatalf("flood close = %q", reason)
		}
	case <-time.After(wait):
		t.Fatal("flooded session not closed")
	}
	r.gate.block.Store(nil)
	close(block)
	resp, err := good.http(t).Get("http://" + testHost + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// Total limit: fill to MaxSessions with sessions in the handshake.
	waitUntil(t, "flood dropped", func() bool { return r.hub.Count() == MaxSessionsPerClient+1 })
	for r.hub.Count() < MaxSessions {
		r.hub.OpenRelay(context.Background(), r.newSID(), "idle")
	}
	sid = r.newSID()
	_, closed = r.cloud.chans(sid)
	r.hub.OpenRelay(context.Background(), sid, "one-more")
	select {
	case reason := <-closed:
		if reason != ReasonLimit {
			t.Fatalf("33rd close = %q", reason)
		}
	case <-time.After(wait):
		t.Fatal("33rd session not refused")
	}
	if r.hub.Count() != MaxSessions {
		t.Fatalf("count = %d", r.hub.Count())
	}
}

func TestHubCloseClient(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	key, cpsk, p1 := r.pair(t, "cloud-a")
	p2, err := r.dial(t, dialOpts{key: key, cloudID: "cloud-a", psk: cpsk})
	if err != nil {
		t.Fatal(err)
	}
	_, _, other := r.pair(t, "cloud-b")

	r.hub.CloseClient("local-cloud-a", CodeRevoked)
	for _, p := range []*phone{p1, p2} {
		reason := ""
		for reason == "" {
			msg, err := p.next()
			var ce closeErr
			if errors.As(err, &ce) {
				reason = string(ce)
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if typ, data, err := OpenRecord(p.recv, msg); err == nil && typ == recordControl && !strings.Contains(string(data), CodeRevoked) {
				t.Errorf("control = %s", data)
			}
		}
		if reason != ReasonRevoked {
			t.Fatalf("close = %q", reason)
		}
	}
	if r.hub.Connected("local-cloud-a") {
		t.Fatal("phone still connected")
	}
	if !r.hub.Connected("local-cloud-b") {
		t.Fatal("other phone closed")
	}

	r.hub.ClientRevoked(context.Background(), "cloud-b")
	select {
	case reason := <-other.closed:
		if reason != ReasonRevoked {
			t.Fatalf("revoked close = %q", reason)
		}
	case <-time.After(wait):
		t.Fatal("revoked phone not closed")
	}
	r.book.mu.Lock()
	defer r.book.mu.Unlock()
	if len(r.book.revoked) != 1 || r.book.revoked[0] != "cloud-b" {
		t.Fatalf("book revoked = %v", r.book.revoked)
	}
}

// FuzzRecords: no input panics the record reader, and only a valid record opens.
func FuzzRecords(f *testing.F) {
	i, _ := mustPair(f)
	good, _ := SealRecord(i, recordMux, []byte("hello"))
	f.Add(good)
	f.Add([]byte{})
	f.Add(make([]byte, 16))
	f.Fuzz(func(t *testing.T, msg []byte) {
		_, rRecv := mustPair(t)
		_, _, _ = OpenRecord(rRecv, msg)
		in := make(chan []byte, 1)
		in <- msg
		done := make(chan struct{})
		close(done)
		c := &recordConn{recv: rRecv, in: in, done: done, onError: func(error) {}, onCtrl: func(Control) error { return nil }}
		_, _ = c.Read(make([]byte, 64))
	})
}

// FuzzHandshakeMessage1: a random message 1 never panics and never starts a session.
func FuzzHandshakeMessage1(f *testing.F) {
	r := newRig(f, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { f.Error("handler reached") }))
	r.offer("offer-x")
	hs, _ := noise.NewHandshakeState(HandshakeConfig(true, testDevice, fixedKey(0x40), r.key.Public, make([]byte, 32), nil))
	hello, _ := json.Marshal(Hello{V: 1, ClientID: "c", Offer: "offer-x", Platform: "ios"})
	m1, _, _, _ := hs.WriteMessage(nil, hello)
	f.Add(m1)
	f.Add([]byte{})
	f.Add(make([]byte, 96))
	f.Fuzz(func(t *testing.T, msg []byte) {
		sid := r.newSID()
		out, closed := r.cloud.chans(sid)
		r.hub.OpenRelay(context.Background(), sid, "c")
		r.hub.DeliverRelay(context.Background(), sid, msg)
		select {
		case m2 := <-out:
			// A valid message 1 gets message 2; the session still needs the ready record.
			_ = m2
			r.hub.CloseRelay(sid, "")
		case reason := <-closed:
			if reason != ReasonRefused {
				t.Fatalf("close = %q", reason)
			}
		case <-time.After(wait):
			t.Fatal("no answer to message 1")
		}
		if r.book.redeems() != 0 || r.pub.count() != 0 {
			t.Fatal("random message 1 made a session")
		}
		r.cloud.mu.Lock()
		delete(r.cloud.out, sid)
		delete(r.cloud.closed, sid)
		r.cloud.mu.Unlock()
	})
}

func newFakeBook() *fakeBook {
	return &fakeBook{offers: map[string][]byte{}, clients: map[string]mobile.Client{}}
}

func (b *fakeBook) redeems() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.redeemed
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{out: map[string]chan []byte{}, closed: map[string]chan string{}}
}

func (c *fakeCloud) chans(sid string) (chan []byte, chan string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.out[sid] == nil {
		c.out[sid] = make(chan []byte, 4096)
		c.closed[sid] = make(chan string, 4)
	}
	return c.out[sid], c.closed[sid]
}

func (p *fakePub) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

func newRig(t testing.TB, root http.Handler) *rig {
	t.Helper()
	key, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{book: newFakeBook(), cloud: newFakeCloud(), gate: &fakeGate{}, pub: &fakePub{}, key: key}
	if root == nil {
		root = http.NotFoundHandler()
	}
	r.hub = NewHub(key, r.book, r.cloud, r.gate, r.pub, root, "test")
	t.Cleanup(func() { r.hub.CloseAll(CodeDaemonStopping) })
	return r
}

// offer opens a pairing offer and returns its psk.
func (r *rig) offer(id string) []byte {
	psk := make([]byte, 32)
	_, _ = rand.Read(psk)
	r.book.mu.Lock()
	r.book.offers[id] = psk
	r.book.mu.Unlock()
	return psk
}

func (r *rig) newSID() string { return fmt.Sprintf("sid-%d", r.seq.Add(1)) }

// dial runs the handshake and the ready exchange. A refusal returns the
// relay_close reason as an error.
func (r *rig) dial(t testing.TB, o dialOpts) (*phone, error) {
	t.Helper()
	if o.helloID == "" {
		o.helloID = o.cloudID
	}
	if o.platform == "" {
		o.platform = mobile.PlatformIOS
	}
	sid := r.newSID()
	out, closed := r.cloud.chans(sid)
	p := &phone{sid: sid, out: out, closed: closed, r: r}
	r.hub.OpenRelay(context.Background(), sid, o.cloudID)

	hs, err := noise.NewHandshakeState(HandshakeConfig(true, testDevice, o.key, r.key.Public, o.psk, nil))
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := json.Marshal(Hello{V: ProtocolV, ClientID: o.helloID, Offer: o.offer, Name: "Test iPad", Platform: o.platform, AppVersion: "1.0"})
	msg1, _, _, err := hs.WriteMessage(nil, hello)
	if err != nil {
		t.Fatal(err)
	}
	r.hub.DeliverRelay(context.Background(), sid, msg1)

	msg2, err := p.next()
	if err != nil {
		return nil, err
	}
	payload, send, recv, err := hs.ReadMessage(nil, msg2)
	if err != nil {
		// Wrong psk: send a record the daemon cannot open, then wait for the close.
		r.hub.DeliverRelay(context.Background(), sid, []byte("not a record"))
		if _, err2 := p.next(); err2 != nil {
			return nil, err2
		}
		return nil, fmt.Errorf("message 2: %w", err)
	}
	if err := json.Unmarshal(payload, &p.welcome); err != nil {
		t.Fatal(err)
	}
	p.send, p.recv = send, recv
	if err := p.sealSend(func(cs *noise.CipherState) ([]byte, error) { return SealControl(cs, Control{Type: "ready"}) }); err != nil {
		t.Fatal(err)
	}
	msg3, err := p.next()
	if err != nil {
		return nil, err
	}
	typ, data, err := OpenRecord(p.recv, msg3)
	if err != nil || typ != recordControl || !strings.Contains(string(data), `"ready"`) {
		t.Fatalf("daemon ready = %d %q %v", typ, data, err)
	}
	return p, nil
}

// next waits for one daemon frame, or returns the close reason as an error.
func (p *phone) next() ([]byte, error) {
	select {
	case m := <-p.out:
		return m, nil
	case reason := <-p.closed:
		return nil, closeErr(reason)
	case <-time.After(wait):
		return nil, errors.New("timeout")
	}
}

func (p *phone) sealSend(fn func(*noise.CipherState) ([]byte, error)) error {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	msg, err := fn(p.send)
	if err != nil {
		return err
	}
	p.r.hub.DeliverRelay(context.Background(), p.sid, msg)
	return nil
}

// open starts yamux as the client over the records.
func (p *phone) open(t testing.TB) *yamux.Session {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	conn := &recordConn{
		recv: p.recv, in: p.out, done: done,
		onError: func(error) {},
		onCtrl: func(c Control) error {
			if c.Type == "error" {
				return io.EOF
			}
			return nil
		},
		send:    p.sealSend,
		closeFn: func() error { once.Do(func() { close(done) }); return nil },
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	mux, err := yamux.Client(conn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.mux = mux
	t.Cleanup(func() { _ = mux.Close() })
	return mux
}

// http returns a client whose every connection is a new stream.
func (p *phone) http(t testing.TB) *http.Client {
	mux := p.mux
	if mux == nil {
		mux = p.open(t)
	}
	return &http.Client{Timeout: wait, Transport: &http.Transport{
		DialContext:     func(context.Context, string, string) (net.Conn, error) { return mux.Open() },
		MaxIdleConns:    100,
		MaxConnsPerHost: 0,
	}}
}

func newKey(t testing.TB) noise.DHKey {
	t.Helper()
	k, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// pair makes a new phone through an offer and returns its key, cloud id, and client psk.
func (r *rig) pair(t testing.TB, cloudID string) (noise.DHKey, []byte, *phone) {
	t.Helper()
	key := newKey(t)
	psk := r.offer("offer-" + cloudID)
	p, err := r.dial(t, dialOpts{key: key, cloudID: cloudID, offer: "offer-" + cloudID, psk: psk})
	if err != nil {
		t.Fatal(err)
	}
	cpsk, err := base64.StdEncoding.DecodeString(p.welcome.ClientPSK)
	if err != nil || len(cpsk) != 32 {
		t.Fatalf("client_psk = %q", p.welcome.ClientPSK)
	}
	return key, cpsk, p
}

func waitUntil(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func echoRoot() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, r.Body) })
	return m
}

// mustPair runs one in-memory handshake and returns the initiator send and
// the responder receive cipher.
func mustPair(tb testing.TB) (*noise.CipherState, *noise.CipherState) {
	tb.Helper()
	ik, rk := fixedKey(0x00), fixedKey(0x20)
	psk := make([]byte, 32)
	ih, _ := noise.NewHandshakeState(HandshakeConfig(true, testDevice, ik, rk.Public, psk, nil))
	rh, _ := noise.NewHandshakeState(HandshakeConfig(false, testDevice, rk, nil, nil, nil))
	m1, _, _, _ := ih.WriteMessage(nil, nil)
	if _, _, _, err := rh.ReadMessage(nil, m1); err != nil {
		tb.Fatal(err)
	}
	_ = rh.SetPresharedKey(psk)
	m2, rRecv, _, _ := rh.WriteMessage(nil, nil)
	_, iSend, _, err := ih.ReadMessage(nil, m2)
	if err != nil {
		tb.Fatal(err)
	}
	return iSend, rRecv
}
