package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/flynn/noise"
	"github.com/hashicorp/yamux"
	"github.com/trebi-ai/trebi/internal/mobile"
	"golang.org/x/crypto/curve25519"
)

// HeaderMachine names the machine that a UI request is for. QueryMachine
// does the same for a request that cannot set a header (a WebSocket, a link).
const (
	HeaderMachine = "X-Trebi-Machine"
	QueryMachine  = "trebi_machine"
)

// MachineTarget is the machine id of a request; "" is this machine.
func MachineTarget(r *http.Request) string {
	if v := r.Header.Get(HeaderMachine); v != "" {
		return v
	}
	return r.URL.Query().Get(QueryMachine)
}

// StripMachineTarget removes the machine target from r.
func StripMachineTarget(r *http.Request) {
	r.Header.Del(HeaderMachine)
	if q := r.URL.Query(); q.Has(QueryMachine) {
		q.Del(QueryMachine)
		r.URL.RawQuery = q.Encode()
	}
}

// PeerCreds are the stored credentials of one peer link: this machine is a
// client of the target machine, as a phone is.
type PeerCreds struct {
	DeviceID  string `json:"device_id"` // the target machine
	ClientID  string `json:"client_id"` // cloud id of this client
	Token     string `json:"token"`     // opens the relay socket
	Static    []byte `json:"static"`    // private X25519 key of this side
	DaemonKey []byte `json:"daemon_key"`
	PSK       []byte `json:"psk"`
}

// PeerConn carries the Noise messages of one peer session.
type PeerConn interface {
	Send(msg []byte) error
	Recv() ([]byte, error)
	Close() error
}

// PeerDialer opens a relay socket with a client token.
// cloudlink.Service implements it.
type PeerDialer interface {
	DialRelay(ctx context.Context, token string) (PeerConn, error)
}

// ErrPeerRevoked is a peer link that the target machine removed.
var ErrPeerRevoked = errors.New("relay: the other machine removed this link")

// PairPeer runs the first handshake with the offer psk of a pairing link.
// It returns creds with the client psk that the target sends.
func PairPeer(ctx context.Context, d PeerDialer, creds PeerCreds, offer, name, version string) (PeerCreds, error) {
	conn, err := d.DialRelay(ctx, creds.Token)
	if err != nil {
		return creds, err
	}
	defer func() { _ = conn.Close() }()
	hs, err := peerHandshake(ctx, conn, creds, offer, name, version)
	if err != nil {
		return creds, err
	}
	raw, err := base64.StdEncoding.DecodeString(hs.welcome.ClientPSK)
	if err != nil || len(raw) != 32 {
		return creds, fmt.Errorf("%w: bad client psk", errProtocol)
	}
	creds.PSK = raw
	return creds, nil
}

// Peer is the client side of one peer link. It opens a relay session when
// a request needs one, and opens it again after the session ends. Each
// request is one yamux stream with HTTP/1.1 to the target daemon.
type Peer struct {
	creds   PeerCreds
	dialer  PeerDialer
	name    string
	version string

	mu        sync.Mutex
	mux       *yamux.Session
	lastErr   error
	closed    bool
	transport *http.Transport
}

// NewPeer builds the client of one link. name is this machine.
func NewPeer(creds PeerCreds, d PeerDialer, name, version string) *Peer {
	p := &Peer{creds: creds, dialer: d, name: name, version: version}
	p.transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			mux, err := p.session(ctx)
			if err != nil {
				return nil, err
			}
			return mux.Open()
		},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     streamIdle / 2,
	}
	return p
}

// Transport sends HTTP requests to the target daemon.
func (p *Peer) Transport() http.RoundTripper { return p.transport }

// Err is the error of the last session that failed; nil after a good one.
func (p *Peer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}

// Close ends the session. A closed Peer opens no new session.
func (p *Peer) Close() {
	p.mu.Lock()
	p.closed = true
	mux := p.mux
	p.mux = nil
	p.mu.Unlock()
	p.transport.CloseIdleConnections()
	if mux != nil {
		_ = mux.Close()
	}
}

// session returns the open session, or opens a new one. The lock is held
// across the dial, so concurrent requests share one session.
func (p *Peer) session(ctx context.Context) (*yamux.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, net.ErrClosed
	}
	if p.mux != nil && !p.mux.IsClosed() {
		return p.mux, nil
	}
	mux, err := p.open(ctx)
	p.lastErr = err
	if err != nil {
		return nil, err
	}
	p.mux = mux
	return mux, nil
}

func (p *Peer) open(ctx context.Context) (*yamux.Session, error) {
	conn, err := p.dialer.DialRelay(ctx, p.creds.Token)
	if err != nil {
		return nil, err
	}
	keys, err := peerHandshake(ctx, conn, p.creds, "", p.name, p.version)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	in := make(chan []byte, queueFrames)
	done := make(chan struct{})
	var once sync.Once
	end := func() { once.Do(func() { close(done); _ = conn.Close() }) }
	go func() { // stop: Recv fails when the socket closes
		defer end()
		for {
			msg, err := conn.Recv()
			if err != nil {
				return
			}
			select {
			case in <- msg:
			case <-done:
				return
			}
		}
	}()
	var sendMu sync.Mutex
	rc := &recordConn{
		recv: keys.recv, in: in, done: done,
		onError: func(error) { end() },
		onCtrl: func(ctl Control) error {
			if ctl.Type != "error" {
				return nil // ready, direct, and unknown types
			}
			p.mu.Lock()
			p.lastErr = fmt.Errorf("relay: the other machine ended the session: %s", ctl.Code)
			if ctl.Code == CodeRevoked {
				p.lastErr = ErrPeerRevoked
			}
			p.mu.Unlock()
			end()
			return io.EOF
		},
		send: func(seal func(*noise.CipherState) ([]byte, error)) error {
			sendMu.Lock()
			defer sendMu.Unlock()
			msg, err := seal(keys.send)
			if err != nil {
				return err
			}
			return conn.Send(msg)
		},
		closeFn: func() error { end(); return nil },
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.StreamCloseTimeout = streamIdle
	mux, err := yamux.Client(rc, cfg)
	if err != nil {
		end()
		return nil, err
	}
	return mux, nil
}

func peerKey(private []byte) (noise.DHKey, error) {
	if len(private) != 32 {
		return noise.DHKey{}, errors.New("relay: peer key must be 32 bytes")
	}
	pub, err := curve25519.X25519(private, curve25519.Basepoint)
	if err != nil {
		return noise.DHKey{}, err
	}
	return noise.DHKey{Private: private, Public: pub}, nil
}

type peerKeys struct {
	send, recv *noise.CipherState
	welcome    Welcome
}

// peerHandshake runs the initiator side and the ready exchange.
func peerHandshake(ctx context.Context, conn PeerConn, creds PeerCreds, offer, name, version string) (peerKeys, error) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	timer := time.AfterFunc(handshakeTimeout, func() { _ = conn.Close() })
	defer timer.Stop()

	key, err := peerKey(creds.Static)
	if err != nil {
		return peerKeys{}, err
	}
	hs, err := noise.NewHandshakeState(HandshakeConfig(true, creds.DeviceID, key, creds.DaemonKey, creds.PSK, nil))
	if err != nil {
		return peerKeys{}, err
	}
	hello, err := json.Marshal(Hello{V: ProtocolV, ClientID: creds.ClientID, Offer: offer, Name: name,
		Platform: mobile.PlatformMachine, AppVersion: version})
	if err != nil {
		return peerKeys{}, err
	}
	msg1, _, _, err := hs.WriteMessage(nil, hello)
	if err != nil {
		return peerKeys{}, err
	}
	if err := conn.Send(msg1); err != nil {
		return peerKeys{}, err
	}
	msg2, err := conn.Recv()
	if err != nil {
		return peerKeys{}, fmt.Errorf("%w: no answer: %v", errRefused, err)
	}
	payload, send, recv, err := hs.ReadMessage(nil, msg2)
	if err != nil {
		return peerKeys{}, fmt.Errorf("%w: message 2: %v", errRefused, err)
	}
	var k peerKeys
	if err := json.Unmarshal(payload, &k.welcome); err != nil || k.welcome.V != ProtocolV {
		return peerKeys{}, fmt.Errorf("%w: bad welcome", errRefused)
	}
	k.send, k.recv = send, recv
	if err := exchangeReady(conn, send, recv); err != nil {
		return peerKeys{}, err
	}
	return k, nil
}

// exchangeReady sends the ready record and waits for the one of the target.
func exchangeReady(conn PeerConn, send, recv *noise.CipherState) error {
	ready, err := SealControl(send, Control{Type: "ready"})
	if err != nil {
		return err
	}
	if err := conn.Send(ready); err != nil {
		return err
	}
	msg, err := conn.Recv()
	if err != nil {
		return fmt.Errorf("%w: no ready: %v", errRefused, err)
	}
	typ, data, err := OpenRecord(recv, msg)
	if err != nil {
		return err
	}
	var ctl Control
	if typ != recordControl || json.Unmarshal(data, &ctl) != nil {
		return fmt.Errorf("%w: no ready record", errRefused)
	}
	if ctl.Type == "error" && ctl.Code == CodeRevoked {
		return ErrPeerRevoked
	}
	if ctl.Type != "ready" {
		return fmt.Errorf("%w: no ready record", errRefused)
	}
	return nil
}
