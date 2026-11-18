package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/hashicorp/yamux"

	"github.com/trebi-ai/trebi/internal/waittest"
)

func TestFrameRoundTrip(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	msgs := [][]byte{{1}, bytes.Repeat([]byte{2}, 300), bytes.Repeat([]byte{3}, MaxDirectMessage)}
	for _, m := range msgs {
		if err := WriteFrame(&b, m); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range msgs {
		got, err := ReadFrame(&b)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("frame %d: len %d, %v", i, len(got), err)
		}
	}
	if _, err := ReadFrame(&b); !errors.Is(err, io.EOF) {
		t.Fatalf("end = %v, want EOF", err)
	}
	for _, n := range []int{0, MaxDirectMessage + 1} {
		if err := WriteFrame(io.Discard, make([]byte, n)); !errors.Is(err, errFrame) {
			t.Fatalf("write %d bytes: %v", n, err)
		}
	}
	for _, s := range [][]byte{{0, 0}, {0}, {0, 5, 1, 2}} {
		if _, err := ReadFrame(bytes.NewReader(s)); !errors.Is(err, errFrame) {
			t.Fatalf("read %x: %v", s, err)
		}
	}
}

func TestDirectAddr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"192.168.1.10", true},
		{"10.0.0.5", true},
		{"172.16.3.4", true},
		{"172.32.0.1", false},
		{"100.64.0.1", true},
		{"100.127.255.254", true},
		{"100.128.0.1", false},
		{"fd7a:115c:a1e0::1", true},
		{"::ffff:192.168.1.10", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"169.254.10.10", false},
		{"fe80::1", false},
		{"8.8.8.8", false},
		{"2001:db8::1", false},
		{"0.0.0.0", false},
		{"224.0.0.251", false},
	} {
		if got := directAddr(netip.MustParseAddr(tc.ip)); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// fakeAddrs is a mutable address source.
type fakeAddrs struct {
	mu  sync.Mutex
	ips []netip.Addr
}

func (f *fakeAddrs) set(ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ips = nil
	for _, ip := range ips {
		f.ips = append(f.ips, netip.MustParseAddr(ip))
	}
}

func (f *fakeAddrs) list() ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ips), nil
}

// serveDirect runs the direct listener on 127.0.0.1 and returns its address.
func (r *rig) serveDirect(t *testing.T, addrs *fakeAddrs) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.hub.ServeDirect(ctx, DirectOptions{Listen: "127.0.0.1:0", Addrs: addrs.list,
			AddrInterval: 20 * time.Millisecond, GateInterval: 20 * time.Millisecond})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	waitUntil(t, "direct listener", func() bool { return r.hub.directServer() != nil })
	return r.hub.directServer().Addr().String()
}

// directPhone is the Go initiator of one direct session.
type directPhone struct {
	conn    net.Conn
	welcome Welcome
	send    *noise.CipherState
	recv    *noise.CipherState
	in      chan []byte
	done    chan struct{}
	mu      sync.Mutex
}

// dialDirect runs the handshake and the ready exchange over TCP. A refusal
// returns an error.
func (r *rig) dialDirect(t *testing.T, addr string, o dialOpts) (_ *directPhone, err error) {
	t.Helper()
	if o.helloID == "" {
		o.helloID = o.cloudID
	}
	conn, err := net.DialTimeout("tcp", addr, waittest.D(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	defer func() {
		if err != nil {
			_ = conn.Close() // a refused phone hangs up
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(waittest.D(5 * time.Second)))
	hs, err := noise.NewHandshakeState(HandshakeConfig(true, testDevice, o.key, r.key.Public, o.psk, nil))
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := json.Marshal(Hello{V: ProtocolV, ClientID: o.helloID, Offer: o.offer, Name: "Test iPad", Platform: "ios", AppVersion: "1.0"})
	msg1, _, _, err := hs.WriteMessage(nil, hello)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(conn, msg1); err != nil {
		return nil, err
	}
	msg2, err := ReadFrame(conn)
	if err != nil {
		return nil, err
	}
	payload, send, recv, err := hs.ReadMessage(nil, msg2)
	if err != nil {
		return nil, err
	}
	p := &directPhone{conn: conn, send: send, recv: recv, in: make(chan []byte, 64), done: make(chan struct{})}
	if err := json.Unmarshal(payload, &p.welcome); err != nil {
		t.Fatal(err)
	}
	if err := p.write(func(cs *noise.CipherState) ([]byte, error) { return SealControl(cs, Control{Type: "ready"}) }); err != nil {
		return nil, err
	}
	msg3, err := ReadFrame(conn)
	if err != nil {
		return nil, err
	}
	if typ, data, err := OpenRecord(recv, msg3); err != nil || typ != recordControl || !bytes.Contains(data, []byte(`"ready"`)) {
		t.Fatalf("daemon ready = %d %q %v", typ, data, err)
	}
	_ = conn.SetDeadline(time.Time{})
	go func() { // stop: the connection closes
		defer close(p.done)
		for {
			msg, err := ReadFrame(conn)
			if err != nil {
				return
			}
			p.in <- msg
		}
	}()
	return p, nil
}

func (p *directPhone) write(seal func(*noise.CipherState) ([]byte, error)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	msg, err := seal(p.send)
	if err != nil {
		return err
	}
	return WriteFrame(p.conn, msg)
}

func (p *directPhone) http(t *testing.T) *http.Client {
	t.Helper()
	conn := &recordConn{
		recv: p.recv, in: p.in, done: p.done,
		onError: func(error) {},
		onCtrl: func(c Control) error {
			if c.Type == "error" {
				return io.EOF
			}
			return nil
		},
		send:    p.write,
		closeFn: func() error { return p.conn.Close() },
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	mux, err := yamux.Client(conn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mux.Close() })
	return &http.Client{Timeout: waittest.D(5 * time.Second), Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return mux.Open() },
	}}
}

func TestDirectKnownPhoneServes(t *testing.T) {
	t.Parallel()
	r := newRig(t, healthRoot())
	addr := r.serveDirect(t, &fakeAddrs{})
	key, cpsk, _ := r.pair(t, "cloud-a")

	p, err := r.dialDirect(t, addr, dialOpts{key: key, cloudID: "cloud-a", psk: cpsk})
	if err != nil {
		t.Fatal(err)
	}
	if p.welcome.ClientPSK != "" || p.welcome.Scope != "full" {
		t.Fatalf("welcome = %+v", p.welcome)
	}
	resp, err := p.http(t).Get("http://" + testHost + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"ok":true`)) {
		t.Fatalf("health = %d %s", resp.StatusCode, body)
	}
	if r.book.redeems() != 1 {
		t.Fatal("direct session redeemed an offer")
	}
	waitUntil(t, "two sessions", func() bool {
		return len(r.hub.matching(func(s *session) bool { return s.clientID() == "local-cloud-a" })) == 2
	})

	// Removing the phone ends its direct session too.
	r.hub.ClientRevoked(context.Background(), "cloud-a")
	select {
	case <-p.done:
	case <-time.After(waittest.D(5 * time.Second)):
		t.Fatal("direct session survived the removal")
	}
}

func TestDirectRefusals(t *testing.T) {
	t.Parallel()
	r := newRig(t, healthRoot())
	addr := r.serveDirect(t, &fakeAddrs{})
	key, cpsk, _ := r.pair(t, "cloud-a")
	revKey, revPSK, _ := r.pair(t, "cloud-r")
	r.hub.ClientRevoked(context.Background(), "cloud-r")

	newKeyT := newKey(t)
	offerPSK := r.offer("offer-new")
	for _, tc := range []struct {
		name string
		o    dialOpts
	}{
		{"unknown key", dialOpts{key: newKey(t), cloudID: "cloud-x", psk: cpsk}},
		{"offer psk", dialOpts{key: newKeyT, cloudID: "cloud-n", offer: "offer-new", psk: offerPSK}},
		{"wrong client id", dialOpts{key: key, cloudID: "cloud-other", psk: cpsk}},
		{"wrong psk", dialOpts{key: key, cloudID: "cloud-a", psk: offerPSK}},
		{"removed phone", dialOpts{key: revKey, cloudID: "cloud-r", psk: revPSK}},
	} {
		if _, err := r.dialDirect(t, addr, tc.o); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	if r.book.redeems() != 2 {
		t.Fatalf("redeems = %d, want 2", r.book.redeems())
	}

	// Gate off: no new session, and the live one ends.
	p, err := r.dialDirect(t, addr, dialOpts{key: key, cloudID: "cloud-a", psk: cpsk})
	if err != nil {
		t.Fatal(err)
	}
	r.gate.off.Store(true)
	select {
	case <-p.done:
	case <-time.After(waittest.D(5 * time.Second)):
		t.Fatal("direct session survived the closed gate")
	}
	if _, err := r.dialDirect(t, addr, dialOpts{key: key, cloudID: "cloud-a", psk: cpsk}); err == nil {
		t.Fatal("gate off: accepted")
	}
}

func TestDirectPendingLimit(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	addr := r.serveDirect(t, &fakeAddrs{})
	var conns []net.Conn
	for range directPendingPerIP {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	waitUntil(t, "pending handshakes", func() bool { return r.hub.Count() == directPendingPerIP })
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(waittest.D(5 * time.Second)))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("fifth pending handshake kept open")
	}
	if n := r.hub.Count(); n != directPendingPerIP {
		t.Fatalf("sessions = %d", n)
	}
}

func TestDirectRecordAnnounce(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	addrs := &fakeAddrs{}
	addrs.set("192.168.1.10", "fd7a:115c:a1e0::1")
	r.serveDirect(t, addrs)
	_, _, p := r.pair(t, "cloud-a")
	port := r.hub.directServer().port

	next := func() []string {
		t.Helper()
		msg, err := p.next()
		if err != nil {
			t.Fatal(err)
		}
		typ, data, err := OpenRecord(p.recv, msg)
		if err != nil || typ != recordControl {
			t.Fatalf("record = %d %v", typ, err)
		}
		var rec DirectRecord
		if err := json.Unmarshal(data, &rec); err != nil || rec.Type != "direct" || rec.V != 1 || rec.Addrs == nil {
			t.Fatalf("record = %s", data)
		}
		return rec.Addrs
	}
	pp := func(h string) string { return net.JoinHostPort(h, strconv.Itoa(port)) }
	if a := next(); !slices.Equal(a, []string{pp("192.168.1.10"), pp("fd7a:115c:a1e0::1")}) {
		t.Fatalf("first = %v", a)
	}
	addrs.set("100.101.102.103")
	if a := next(); !slices.Equal(a, []string{pp("100.101.102.103")}) {
		t.Fatalf("change = %v", a)
	}
	addrs.set()
	if a := next(); len(a) != 0 {
		t.Fatalf("empty = %v", a)
	}
}

func healthRoot() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"actor":"` + r.Header.Get("X-Trebi-Actor") + `"}`))
	})
	return m
}
