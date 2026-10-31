package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/trebi-ai/trebi/internal/mobile"
	"github.com/trebi-ai/trebi/internal/tools"
)

// rigDialer opens hub sessions as the cloud relay does for a client token.
type rigDialer struct {
	r      *rig
	dials  atomic.Int64
	tokens map[string]string // token → cloud client id
}

func (d *rigDialer) DialRelay(_ context.Context, token string) (PeerConn, error) {
	cloudID, ok := d.tokens[token]
	if !ok {
		return nil, errors.New("bad_token")
	}
	d.dials.Add(1)
	sid := d.r.newSID()
	out, closed := d.r.cloud.chans(sid)
	d.r.hub.OpenRelay(context.Background(), sid, cloudID)
	return &rigConn{r: d.r, sid: sid, out: out, closed: closed, done: make(chan struct{})}, nil
}

type rigConn struct {
	r      *rig
	sid    string
	out    chan []byte
	closed chan string
	done   chan struct{}
	once   sync.Once
}

func (c *rigConn) Send(msg []byte) error {
	c.r.hub.DeliverRelay(context.Background(), c.sid, append([]byte(nil), msg...))
	return nil
}

func (c *rigConn) Recv() ([]byte, error) {
	select {
	case msg := <-c.out:
		return msg, nil
	case reason := <-c.closed:
		return nil, closeErr(reason)
	case <-c.done:
		return nil, io.EOF
	}
}

func (c *rigConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.r.hub.CloseRelay(c.sid, ReasonClientGone)
	})
	return nil
}

func TestPeerPairAndServe(t *testing.T) {
	t.Parallel()
	var actor atomic.Value
	var target atomic.Value
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/system/health", func(w http.ResponseWriter, r *http.Request) {
		a, _ := tools.ActorFrom(r.Context())
		actor.Store(a)
		target.Store(MachineTarget(r) + "|" + r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	r := newRig(t, m)
	d, creds := pairedPeer(t, r)
	if r.book.redeems() != 1 {
		t.Fatalf("redeems = %d", r.book.redeems())
	}
	c, ok := r.book.Lookup(newKeyFrom(t, creds.Static))
	if !ok || c.Platform != mobile.PlatformMachine || c.Name != "mbp" {
		t.Fatalf("client = %+v %v", c, ok)
	}

	p := NewPeer(creds, d, "mbp", "v0.2.170")
	t.Cleanup(p.Close)
	hc := &http.Client{Transport: p.Transport()}
	for range 3 {
		req, _ := http.NewRequest(http.MethodGet, "http://peer/v1/system/health?trebi_machine=x&a=1", nil)
		req.Header.Set(HeaderMachine, "m-3")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("health = %d", resp.StatusCode)
		}
	}
	a, _ := actor.Load().(tools.Actor)
	if a.Kind != tools.ActorMobile || a.Name != "machine:mbp" {
		t.Fatalf("actor = %+v", a)
	}
	if got := target.Load(); got != "|a=1" {
		t.Fatalf("machine target reached the target: %q", got)
	}
	if n := d.dials.Load(); n != 2 { // pairing plus one shared session
		t.Fatalf("dials = %d", n)
	}

	// The session ends; the next request opens a new one.
	r.hub.CloseAll(CodeDaemonStopping)
	waitUntil(t, "session closed", func() bool { return r.hub.Count() == 0 })
	waitUntil(t, "redial", func() bool {
		resp, err := hc.Get("http://peer/v1/system/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	})
	if p.Err() != nil {
		t.Fatalf("err = %v", p.Err())
	}
}

func TestPeerRevoked(t *testing.T) {
	t.Parallel()
	r := newRig(t, http.NotFoundHandler())
	d, creds := pairedPeer(t, r)
	r.hub.ClientRevoked(context.Background(), "cloud-m")
	p := NewPeer(creds, d, "mbp", "v0.2.170")
	t.Cleanup(p.Close)
	if _, err := (&http.Client{Transport: p.Transport()}).Get("http://peer/"); err == nil {
		t.Fatal("revoked link served a request")
	}
	if p.Err() == nil {
		t.Fatal("no error after a refused session")
	}
}

func TestStripMachineTarget(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("http://x/v1/runs?trebi_machine=m1&limit=5")
	r := &http.Request{URL: u, Header: http.Header{}}
	if MachineTarget(r) != "m1" {
		t.Fatalf("target = %q", MachineTarget(r))
	}
	r.Header.Set(HeaderMachine, "m2")
	if MachineTarget(r) != "m2" {
		t.Fatalf("header target = %q", MachineTarget(r))
	}
	StripMachineTarget(r)
	if MachineTarget(r) != "" || r.URL.RawQuery != "limit=5" {
		t.Fatalf("after strip: %q %q", MachineTarget(r), r.URL.RawQuery)
	}
}

func newKeyFrom(t *testing.T, private []byte) []byte {
	t.Helper()
	k, err := peerKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return k.Public
}

func pairedPeer(t *testing.T, r *rig) (*rigDialer, PeerCreds) {
	t.Helper()
	d := &rigDialer{r: r, tokens: map[string]string{"trc_a": "cloud-m"}}
	creds := PeerCreds{DeviceID: testDevice, ClientID: "cloud-m", Token: "trc_a", Static: newKey(t).Private,
		DaemonKey: r.key.Public, PSK: r.offer("offer-m")}
	creds, err := PairPeer(context.Background(), d, creds, "offer-m", "mbp", "v0.2.170")
	if err != nil {
		t.Fatal(err)
	}
	return d, creds
}
