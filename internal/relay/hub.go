package relay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flynn/noise"
	"github.com/hashicorp/yamux"
	"github.com/trebi-ai/trebi/internal/mobile"
	"github.com/trebi-ai/trebi/internal/tools"
)

// Session limits (plan D7, B4).
const (
	MaxSessionsPerClient = 8
	MaxSessions          = 32
	// Each stream has at most a 256 KiB yamux window in flight. queueBytes
	// holds 128 full windows; more ends the session.
	queueBytes       = 32 << 20
	queueFrames      = 8192
	handshakeTimeout = 15 * time.Second
	streamIdle       = 120 * time.Second
)

// KeyBook finds and enrolls phones. mobile.Service implements it.
type KeyBook interface {
	Lookup(pub []byte) (mobile.Client, bool)
	OfferPSK(offerID string) ([]byte, bool)
	Redeem(ctx context.Context, offerID string, in mobile.Enrollment, psk []byte) (mobile.Client, error)
	Seen(id string, at time.Time)
	Revoked(ctx context.Context, cloudClientID string)
}

// Cloud writes to the cloud socket. cloudlink.Service implements it.
type Cloud interface {
	SendRelay(sid string, raw []byte) error
	CloseRelay(sid, reason string) error
	DeviceID() string
	DeviceName() string
}

// Gate says if a new session may start.
type Gate interface {
	MobileAllowed() bool
}

// Publisher writes the mobile.connected and mobile.disconnected events.
type Publisher interface {
	Connected(ctx context.Context, clientID string, on bool)
}

// Hub owns the phone sessions by cloud sid.
type Hub struct {
	key     noise.DHKey
	book    KeyBook
	cloud   Cloud
	gate    Gate
	pub     Publisher
	root    http.Handler
	version string
	log     *slog.Logger

	// ephemeral pins the responder ephemeral key (contract vectors only).
	ephemeral *noise.DHKey

	mu        sync.Mutex
	sessions  map[string]*session
	direct    *Direct // nil until ServeDirect binds
	directErr error   // the last bind error of ServeDirect
}

// NewHub builds the hub. root is the handler of the loopback listener.
func NewHub(key noise.DHKey, book KeyBook, cloud Cloud, gate Gate, pub Publisher, root http.Handler, version string) *Hub {
	return &Hub{
		key: key, book: book, cloud: cloud, gate: gate, pub: pub, root: root, version: version,
		log:      slog.Default().With("source", "relay"),
		sessions: map[string]*session{},
	}
}

// PublicKey is the daemon static key the pairing link carries.
func (h *Hub) PublicKey() []byte { return h.key.Public }

// OpenRelay starts a session for one phone socket. The session ends when
// ctx ends (the cloud socket dropped), on CloseRelay, or on a fault.
func (h *Hub) OpenRelay(ctx context.Context, sid, cloudClientID string) {
	s := h.newSession(sid, cloudWire{h: h, sid: sid})
	s.cloudClientID = cloudClientID
	switch h.add(s) {
	case addExists:
		return
	case addFull:
		h.refuse(sid, ReasonLimit)
		return
	}
	go s.run(ctx) // stop: s.done closes, or ctx ends
}

// DeliverRelay queues one inbound Noise message. It never blocks the
// socket read loop: a full queue ends only that session.
func (h *Hub) DeliverRelay(_ context.Context, sid string, raw []byte) {
	h.mu.Lock()
	s := h.sessions[sid]
	h.mu.Unlock()
	if s == nil {
		return
	}
	if s.queued.Add(int64(len(raw))) > queueBytes {
		h.log.Warn("relay.end", "sid", sid, "reason", ReasonLimit, "queued_bytes", s.queued.Load())
		s.end(ReasonLimit, "")
		return
	}
	select {
	case s.in <- raw:
	case <-s.done:
	default:
		h.log.Warn("relay.end", "sid", sid, "reason", ReasonLimit, "queued_frames", queueFrames)
		s.end(ReasonLimit, "")
	}
}

// CloseRelay ends one session because the cloud closed the phone socket.
func (h *Hub) CloseRelay(sid, _ string) {
	h.mu.Lock()
	s := h.sessions[sid]
	h.mu.Unlock()
	if s != nil {
		s.end("", "")
	}
}

// ClientRevoked ends the sessions of a phone that the cloud removed, then
// removes the phone here.
func (h *Hub) ClientRevoked(ctx context.Context, cloudClientID string) {
	for _, s := range h.matching(func(s *session) bool { return s.cloudID() == cloudClientID }) {
		s.end(ReasonRevoked, CodeRevoked)
	}
	h.book.Revoked(ctx, cloudClientID)
}

// CloseClient ends all sessions of one phone at once, with the control code.
func (h *Hub) CloseClient(clientID, code string) {
	reason := ReasonRevoked
	if code == CodeDaemonStopping {
		reason = ReasonDaemonOff
	}
	for _, s := range h.matching(func(s *session) bool { return s.clientID() == clientID }) {
		s.end(reason, code)
	}
}

// CloseAll ends every session, for a shutdown or for phone access off.
func (h *Hub) CloseAll(code string) {
	for _, s := range h.matching(func(*session) bool { return true }) {
		s.end(ReasonDaemonOff, code)
	}
}

// Count is the number of open sessions.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

// Connected reports whether a phone has a ready session.
func (h *Hub) Connected(clientID string) bool {
	return len(h.matching(func(s *session) bool { return s.clientID() == clientID })) > 0
}

func (h *Hub) newSession(sid string, w wire) *session {
	return &session{hub: h, sid: sid, wire: w, hsTimeout: handshakeTimeout, in: make(chan []byte, queueFrames), done: make(chan struct{})}
}

type addResult int

const (
	addOK addResult = iota
	addExists
	addFull
)

// add registers a session under the total limit.
func (h *Hub) add(s *session) addResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.sessions[s.sid]; ok {
		return addExists
	}
	if len(h.sessions) >= MaxSessions {
		return addFull
	}
	h.sessions[s.sid] = s
	return addOK
}

func (h *Hub) matching(keep func(*session) bool) []*session {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*session
	for _, s := range h.sessions {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

func (h *Hub) refuse(sid, reason string) {
	if err := h.cloud.CloseRelay(sid, reason); err != nil {
		h.log.Debug("relay.close", "sid", sid, "err", err)
	}
}

func (h *Hub) drop(s *session) {
	h.mu.Lock()
	if h.sessions[s.sid] == s {
		delete(h.sessions, s.sid)
	}
	h.mu.Unlock()
}

// wire carries the Noise messages of one session: the cloud relay, or a
// direct TCP connection.
type wire interface {
	Send(msg []byte) error
	// Close ends the transport. reason, when set, is the relay_close reason.
	Close(reason string)
}

// cloudWire sends through the cloud socket by sid.
type cloudWire struct {
	h   *Hub
	sid string
}

func (w cloudWire) Send(msg []byte) error { return w.h.cloud.SendRelay(w.sid, msg) }

func (w cloudWire) Close(reason string) {
	if reason != "" {
		w.h.refuse(w.sid, reason)
	}
}

// session is one phone socket: handshake, then yamux and HTTP.
type session struct {
	hub       *Hub
	sid       string
	wire      wire
	direct    bool   // a direct TCP session (no cloud client id until the handshake)
	remote    string // remote address of a direct session
	hsTimeout time.Duration
	// afterHandshake, when set, runs once when the handshake ends.
	afterHandshake func()
	in             chan []byte
	queued         atomic.Int64 // bytes in in
	done           chan struct{}

	mu            sync.Mutex
	cloudClientID string
	client        mobile.Client
	ready         bool
	sendCS        *noise.CipherState
	ended         bool
	mux           *yamux.Session
	endedBy       string // reason sent to the cloud, "" when the cloud closed it
	endCode       string
	stopOnce      sync.Once
}

func (s *session) cloudID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cloudClientID
}

func (s *session) clientID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.ended {
		return ""
	}
	return s.client.ID
}

// end stops the session once. reason, when set, goes to the cloud as
// relay_close; code, when set, goes to the phone as an error record first.
func (s *session) end(reason, code string) {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.ended, s.endedBy, s.endCode = true, reason, code
		cs, mux := s.sendCS, s.mux
		s.mu.Unlock()
		if code != "" && cs != nil {
			_ = s.seal(func(cs *noise.CipherState) ([]byte, error) { // best effort: the session ends anyway
				return SealControl(cs, Control{Type: "error", Code: code})
			})
		}
		close(s.done)
		if mux != nil {
			// Async: end runs inside the yamux read loop and inside conn.Close,
			// and mux.Close waits for both.
			go func() { _ = mux.Close() }() // stop: Close returns
		}
		s.wire.Close(reason)
	})
}

// seal encrypts and sends one record under the send lock, so the nonce
// order and the wire order agree.
func (s *session) seal(fn func(*noise.CipherState) ([]byte, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendCS == nil {
		return errProtocol
	}
	msg, err := fn(s.sendCS)
	if err != nil {
		return err
	}
	return s.wire.Send(msg)
}

// next waits for one inbound frame during the handshake.
func (s *session) next(ctx context.Context, timer <-chan time.Time) ([]byte, error) {
	select {
	case msg := <-s.in:
		s.queued.Add(-int64(len(msg)))
		return msg, nil
	case <-s.done:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer:
		return nil, errors.New("relay: handshake timeout")
	}
}

func (s *session) run(ctx context.Context) {
	h := s.hub
	defer h.drop(s)
	stop := context.AfterFunc(ctx, func() { s.end("", "") })
	defer stop()

	timer := time.NewTimer(s.hsTimeout)
	defer timer.Stop()
	recv, err := s.handshake(ctx, timer.C)
	if s.afterHandshake != nil {
		s.afterHandshake()
	}
	if err != nil {
		h.log.Info("relay.refused", "sid", s.sid, "direct", s.direct, "remote", s.remote, "err", err)
		s.end(ReasonRefused, "")
		return
	}
	timer.Stop()

	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	h.book.Seen(c.ID, time.Now().UTC())
	h.pub.Connected(ctx, c.ID, true)
	defer h.pub.Connected(context.WithoutCancel(ctx), c.ID, false)
	if s.direct {
		h.log.Info("mobile.direct.session", "state", "open", "sid", s.sid, "client_id", c.ID, "client", c.Name, "remote", s.remote)
		defer h.log.Info("mobile.direct.session", "state", "close", "sid", s.sid, "client_id", c.ID, "client", c.Name, "remote", s.remote)
	} else {
		h.log.Info("relay.session", "sid", s.sid, "client_id", c.ID)
		if d := h.directServer(); d != nil {
			d.announce(s)
		}
	}

	conn := &recordConn{
		recv: recv, in: s.in, queued: &s.queued, done: s.done,
		onError: func(error) { s.end(ReasonRefused, "") },
		onCtrl: func(ctl Control) error {
			if ctl.Type == "error" {
				s.end("", "")
				return io.EOF
			}
			return nil // unknown control types are ignored
		},
		send:    s.seal,
		closeFn: func() error { s.end(ReasonClientGone, ""); return nil },
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.StreamCloseTimeout = streamIdle
	mux, err := yamux.Server(conn, cfg)
	if err != nil {
		s.end(ReasonRefused, "")
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		_ = mux.Close()
		return
	}
	s.mux = mux
	s.mu.Unlock()

	actor := tools.Actor{Kind: tools.ActorMobile, Name: "mobile:" + c.Name, Scopes: []string{c.Scope}}
	srv := &http.Server{
		Handler:           mobileHandler(h.root, actor),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       streamIdle,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	_ = srv.Serve(mux) // returns when the mux closes
	_ = srv.Close()
	s.end(ReasonClientGone, "")
}

// handshake runs the responder side and the ready exchange. It returns the
// receive cipher; the send cipher is on the session.
func (s *session) handshake(ctx context.Context, timer <-chan time.Time) (*noise.CipherState, error) {
	h := s.hub
	if !h.gate.MobileAllowed() {
		return nil, fmt.Errorf("%w: phone access is off", errRefused)
	}
	msg1, err := s.next(ctx, timer)
	if err != nil {
		return nil, err
	}
	hs, err := noise.NewHandshakeState(HandshakeConfig(false, h.cloud.DeviceID(), h.key, nil, nil, h.ephemeral))
	if err != nil {
		return nil, err
	}
	hello, err := s.readHello(hs, msg1)
	if err != nil {
		return nil, err
	}
	pub := hs.PeerStatic()
	client, known, psk, clientPSK, err := s.choosePSK(pub, hello)
	if err != nil {
		return nil, err
	}
	if err := hs.SetPresharedKey(psk); err != nil {
		return nil, err
	}
	welcome := Welcome{V: ProtocolV, Machine: h.cloud.DeviceName(), DaemonVersion: h.version, Scope: client.Scope}
	if !known {
		welcome.Scope = mobile.ScopeFull
		welcome.ClientPSK = base64.StdEncoding.EncodeToString(clientPSK)
	}
	payload, err := json.Marshal(welcome)
	if err != nil {
		return nil, err
	}
	msg2, recv, send, err := hs.WriteMessage(nil, payload)
	if err != nil {
		return nil, err
	}
	if err := s.wire.Send(msg2); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sendCS = send
	s.mu.Unlock()

	// The phone proves the psk with its first record. Only then does a
	// pairing become a phone.
	if err := s.awaitReady(ctx, timer, recv); err != nil {
		return nil, err
	}
	if !known {
		client, err = h.book.Redeem(ctx, hello.Offer, mobile.Enrollment{
			CloudClientID: hello.ClientID, Name: hello.Name, Platform: hello.Platform, PublicKey: pub,
		}, clientPSK)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errRefused, err)
		}
	}
	s.mu.Lock()
	s.client, s.ready = client, true
	s.mu.Unlock()
	if err := s.seal(func(cs *noise.CipherState) ([]byte, error) { return SealControl(cs, Control{Type: "ready"}) }); err != nil {
		return nil, err
	}
	return recv, nil
}

// readHello reads message 1 and checks its payload against the socket.
func (s *session) readHello(hs *noise.HandshakeState, msg1 []byte) (Hello, error) {
	raw, _, _, err := hs.ReadMessage(nil, msg1)
	if err != nil {
		return Hello{}, fmt.Errorf("%w: message 1: %v", errRefused, err)
	}
	var hello Hello
	if err := json.Unmarshal(raw, &hello); err != nil || hello.V != ProtocolV {
		return Hello{}, fmt.Errorf("%w: bad hello", errRefused)
	}
	if hello.ClientID == "" || (!s.direct && hello.ClientID != s.cloudID()) {
		return Hello{}, fmt.Errorf("%w: client id does not match", errRefused)
	}
	return hello, nil
}

// choosePSK picks the psk of a known phone, or the offer psk plus a new
// client psk for a pairing. A direct session takes only a known phone: it
// has no cloud client id to check, so the stored key and psk are the proof.
func (s *session) choosePSK(pub []byte, hello Hello) (client mobile.Client, known bool, psk, clientPSK []byte, err error) {
	h := s.hub
	if client, known = h.book.Lookup(pub); known {
		if client.CloudClientID != hello.ClientID {
			return client, true, nil, nil, fmt.Errorf("%w: key belongs to another phone", errRefused)
		}
		if len(h.matching(func(o *session) bool { return o.clientID() == client.ID })) >= MaxSessionsPerClient {
			return client, true, nil, nil, fmt.Errorf("%w: session limit", errRefused)
		}
		if s.direct {
			s.mu.Lock()
			s.cloudClientID = client.CloudClientID
			s.mu.Unlock()
		}
		return client, true, client.PSK, nil, nil
	}
	if s.direct {
		return client, false, nil, nil, fmt.Errorf("%w: unknown key on the direct path", errRefused)
	}
	offer, ok := h.book.OfferPSK(hello.Offer)
	if hello.Offer == "" || !ok {
		return client, false, nil, nil, fmt.Errorf("%w: unknown key and no valid offer", errRefused)
	}
	clientPSK = make([]byte, 32)
	if _, err := rand.Read(clientPSK); err != nil {
		return client, false, nil, nil, err
	}
	return client, false, offer, clientPSK, nil
}

// awaitReady reads the first record of the phone, which must be "ready".
func (s *session) awaitReady(ctx context.Context, timer <-chan time.Time, recv *noise.CipherState) error {
	msg, err := s.next(ctx, timer)
	if err != nil {
		return err
	}
	typ, data, err := OpenRecord(recv, msg)
	if err != nil {
		return fmt.Errorf("%w: %v", errRefused, err)
	}
	var ctl Control
	if typ != recordControl || json.Unmarshal(data, &ctl) != nil || ctl.Type != "ready" {
		return fmt.Errorf("%w: no ready record", errRefused)
	}
	return nil
}

// mobileHandler pins the phone actor on every request. It drops the
// internal actor headers, so a phone cannot claim to be a run.
func mobileHandler(root http.Handler, actor tools.Actor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(tools.HeaderRunToken)
		r.Header.Del(tools.HeaderRunID)
		r.Header.Set(tools.HeaderActor, actor.Name)
		root.ServeHTTP(w, r.WithContext(tools.WithActor(r.Context(), actor)))
	})
}
