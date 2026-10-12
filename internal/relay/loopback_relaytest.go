//go:build relaytest

package relay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/mobile"
)

// Loopback emulates the cloud relay on a local port, for the cross-language
// tests (plan T4). It is only in a binary built with -tags relaytest.
type Loopback struct {
	addr string
	srv  *http.Server
	up   websocket.Upgrader
	log  *slog.Logger

	mu      sync.Mutex
	hub     *Hub
	claims  map[string]*loopClaim
	tokens  map[string]string // token → client id
	revoked map[string]bool   // client id
	socks   map[string]*loopSock
}

type loopClaim struct {
	uses    int
	expires time.Time
}

type loopSock struct {
	clientID string
	conn     *websocket.Conn
	wmu      sync.Mutex
}

const loopDeviceID = "01JRELAYTEST0000000000DEV0"

// loopState is the part of the loopback that survives a daemon restart.
type loopState struct {
	Tokens  map[string]string `json:"tokens"`
	Revoked map[string]bool   `json:"revoked"`
}

// NewLoopback listens on addr and serves the phone API and the phone socket.
// The phone tokens persist in home.RelayLoopbackPath, as in the cloud.
func NewLoopback(addr string) (*Loopback, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	l := &Loopback{
		addr: ln.Addr().String(), log: slog.Default().With("source", "relay_loopback"),
		claims: map[string]*loopClaim{}, tokens: map[string]string{}, revoked: map[string]bool{}, socks: map[string]*loopSock{},
	}
	if raw, err := os.ReadFile(home.RelayLoopbackPath()); err == nil {
		var st loopState
		if json.Unmarshal(raw, &st) == nil {
			for k, v := range st.Tokens {
				l.tokens[k] = v
			}
			for k, v := range st.Revoked {
				l.revoked[k] = v
			}
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/clients", l.enroll)
	mux.HandleFunc("DELETE /v1/clients/me", l.forget)
	mux.HandleFunc("PUT /v1/clients/me/push-token", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := l.auth(w, r); ok {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("GET /v1/relay", l.relay)
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = l.srv.Serve(ln) }() // stop: Close
	return l, nil
}

// Addr is host:port of the loopback.
func (l *Loopback) Addr() string { return l.addr }

// Close stops the listener.
func (l *Loopback) Close() error { return l.srv.Close() }

func (l *Loopback) BindHub(h *Hub) {
	l.mu.Lock()
	l.hub = h
	l.mu.Unlock()
}

func (l *Loopback) MobileAllowed() bool { return true }

// DirectOptions serves and announces the direct path on 127.0.0.1 only.
func (l *Loopback) DirectOptions(port int) DirectOptions {
	return DirectOptions{
		Listen: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Addrs:  func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil },
	}
}
func (l *Loopback) DeviceID() string   { return loopDeviceID }
func (l *Loopback) DeviceName() string { return "relaytest" }

func (l *Loopback) PairTarget() mobile.Target {
	return mobile.Target{APIHost: l.addr, DeviceID: loopDeviceID, Machine: "relaytest", Linked: true, RemoteAccess: true, MobileOn: true}
}

func (l *Loopback) MintClaim(_ context.Context, uses int, ttl time.Duration) (string, time.Time, error) {
	claim := "trcl_" + randToken()
	exp := time.Now().Add(ttl).UTC()
	l.mu.Lock()
	l.claims[claim] = &loopClaim{uses: uses, expires: exp}
	l.mu.Unlock()
	return claim, exp, nil
}

func (l *Loopback) RevokeClient(_ context.Context, cloudClientID string) error {
	l.mu.Lock()
	l.revoked[cloudClientID] = true
	l.saveLocked()
	var socks []*loopSock
	for _, s := range l.socks {
		if s.clientID == cloudClientID {
			socks = append(socks, s)
		}
	}
	l.mu.Unlock()
	for _, s := range socks {
		s.close(4410, "revoked")
	}
	return nil
}

func (l *Loopback) SendRelay(sid string, raw []byte) error {
	l.mu.Lock()
	s := l.socks[sid]
	l.mu.Unlock()
	if s == nil {
		return errors.New("relay loopback: unknown sid")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.conn.WriteMessage(websocket.TextMessage, []byte(base64.StdEncoding.EncodeToString(raw)))
}

func (l *Loopback) CloseRelay(sid, reason string) error {
	l.mu.Lock()
	s := l.socks[sid]
	l.mu.Unlock()
	if s == nil {
		return nil
	}
	code := websocket.CloseNormalClosure
	if reason == ReasonRefused {
		code = 4409
	}
	s.close(code, reason)
	return nil
}

func (l *Loopback) enroll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Claim    string `json:"claim"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		loopErr(w, http.StatusBadRequest, "validation", "body is not JSON")
		return
	}
	l.mu.Lock()
	c := l.claims[body.Claim]
	if c == nil || c.uses < 1 || time.Now().After(c.expires) {
		l.mu.Unlock()
		loopErr(w, http.StatusUnauthorized, "bad_claim", "This pairing code is not valid.")
		return
	}
	c.uses--
	id, token := ulid.Make().String(), "trc_"+randToken()
	l.tokens[token] = id
	l.saveLocked()
	l.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"client_id": id, "token": token})
}

func (l *Loopback) forget(w http.ResponseWriter, r *http.Request) {
	id, ok := l.auth(w, r)
	if !ok {
		return
	}
	_ = l.RevokeClient(r.Context(), id) // never fails
	l.mu.Lock()
	hub := l.hub
	l.mu.Unlock()
	if hub != nil {
		hub.ClientRevoked(r.Context(), id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (l *Loopback) auth(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	l.mu.Lock()
	id, ok := l.tokens[token]
	gone := l.revoked[id]
	l.mu.Unlock()
	switch {
	case !ok:
		loopErr(w, http.StatusUnauthorized, "bad_token", "The phone token is not known.")
		return "", false
	case gone:
		loopErr(w, http.StatusGone, "client_revoked", "This phone was removed.")
		return "", false
	}
	return id, true
}

// relay maps one phone socket to one sid.
func (l *Loopback) relay(w http.ResponseWriter, r *http.Request) {
	id, ok := l.auth(w, r)
	if !ok {
		return
	}
	conn, err := l.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	sid := ulid.Make().String()
	s := &loopSock{clientID: id, conn: conn}
	l.mu.Lock()
	l.socks[sid] = s
	hub := l.hub
	l.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if hub != nil {
		hub.OpenRelay(ctx, sid, id)
	}
	for {
		typ, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if typ != websocket.TextMessage {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(string(msg))
		if err != nil || hub == nil {
			continue
		}
		hub.DeliverRelay(ctx, sid, raw)
	}
	l.mu.Lock()
	delete(l.socks, sid)
	l.mu.Unlock()
	if hub != nil {
		hub.CloseRelay(sid, ReasonClientGone)
	}
	_ = conn.Close()
}

func (l *Loopback) saveLocked() {
	raw, err := json.Marshal(loopState{Tokens: l.tokens, Revoked: l.revoked})
	if err != nil {
		return
	}
	if err := os.WriteFile(home.RelayLoopbackPath(), raw, 0o600); err != nil {
		l.log.Warn("relay_loopback.save", "err", err)
	}
}

func (s *loopSock) close(code int, reason string) {
	s.wmu.Lock()
	_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	s.wmu.Unlock()
	_ = s.conn.Close()
}

func loopErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func randToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand never fails on supported platforms
	return base64.RawURLEncoding.EncodeToString(b)
}
