package cloudlink

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/trebi-ai/trebi/internal/email"
)

const (
	ingressPayloadCap = 4 << 20 // one ingress payload, on decode
	backoffStart      = time.Second
	backoffMax        = 60 * time.Second
	defaultHeartbeat  = 60 * time.Second
	dialTimeout       = 30 * time.Second // the full websocket handshake
)

// socket is one live connection to the cloud: signed dial, frame
// dispatch, heartbeats, and bounded reconnect. It holds no state beyond
// the current session; the Service owns the link row and the sinks.
type socket struct {
	svc *Service
	id  Identity

	mu           sync.Mutex
	conn         *websocket.Conn
	connected    bool
	downSince    time.Time // zero while connected
	unauthorized bool
	unknown      int64
	drops        int64

	mcpSlots chan struct{}       // bounds concurrent MCP requests
	sids     map[string]struct{} // phone sessions open on this connection
}

// run redials until the context is cancelled or the dial is rejected as
// revoked (401): a revoked device must never become a reconnect loop.
// Stop condition: ctx.Done (Stop cancels it) or the unauthorized state;
// only a new Link or a process restart dials again after that.
func (sk *socket) run(ctx context.Context) {
	backoff := backoffStart
	for {
		helloOK, unauthorized := sk.session(ctx)
		if ctx.Err() != nil {
			return
		}
		sk.markDown(ctx) // one cloud.disconnected per loss
		if unauthorized {
			return
		}
		if helloOK {
			backoff = backoffStart // reset after a successful hello_ok
		}
		if !sleepJitter(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// session runs one connection: dial, hello, then the read loop. It
// reports whether hello_ok was reached and whether the dial said the
// device is revoked.
func (sk *socket) session(ctx context.Context) (helloOK, unauthorized bool) {
	conn, resp, err := sk.dialAny(ctx)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			sk.setUnauthorized()
			sk.svc.log.Warn("cloudlink.dial", "err", "device unauthorized")
			return false, true
		}
		sk.svc.log.Warn("cloudlink.dial", "err", err)
		return false, false
	}
	sk.setConn(conn)
	defer sk.clearConn(conn)
	defer sk.closeSids() // phones reconnect on the next socket

	hello := Hello{V: ContractV, Type: "hello"}
	if sk.svc.remoteMCP() {
		hello.Caps = append(hello.Caps, CapMCP)
	}
	if sk.svc.mobile() {
		hello.Caps = append(hello.Caps, CapMobile)
	}
	if err := sk.writeFrame(hello); err != nil {
		return false, false
	}
	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel() // stop condition for the heartbeat loop below
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return helloOK, false
		}
		if sk.handleFrame(ctx, sessCtx, raw) {
			helloOK = true
		}
	}
}

// handleFrame decodes and dispatches one inbound frame, reporting whether
// it was hello_ok. The switch is on the decoded frame's type and nothing else.
func (sk *socket) handleFrame(ctx, sessCtx context.Context, raw []byte) bool {
	frame, typ, err := decodeFrame(raw)
	if err != nil {
		sk.bumpUnknown()
		sk.svc.log.Debug("cloudlink.frame", "err", err)
		return false
	}
	switch f := frame.(type) {
	case HelloOK:
		sk.svc.applyEntitlement(ctx, f.Plan, f.Features)
		sk.svc.setMcpURL(f.McpURL)
		go sk.heartbeatLoop(sessCtx, f.HeartbeatInterval) // stop: sessCtx done
		sk.markUp(ctx)
		sk.svc.fireOnChange(ctx)
		return true
	case Entitlement:
		sk.svc.applyEntitlement(ctx, f.Plan, f.Features)
		sk.svc.fireOnChange(ctx)
	case Ingress:
		sk.settleIngress(ctx, f)
	case RelayOpen:
		sk.openRelay(sessCtx, f)
	case RelayFrame:
		sk.handleRelay(ctx, f)
	case RelayClose:
		if sk.dropSid(f.SID) {
			sk.svc.relay.CloseRelay(f.SID, f.Reason)
		}
	case ClientRevoked:
		if sk.svc.relay != nil {
			sk.svc.relay.ClientRevoked(ctx, f.ClientID)
		}
	case McpRequest:
		go sk.handleMcp(sessCtx, f) // stop: the sink returns or the timeout ends
	case LoginReturn:
		go sk.handleLogin(sessCtx, f) // stop: the sink returns
	default:
		sk.bumpUnknown()
		sk.svc.log.Debug("cloudlink.frame", "type", typ)
	}
	return false
}

// settleIngress delivers one ingress frame and acks it when it is settled
// (delivered or deliberately dropped). An unsettled frame stays unacked on
// purpose: the cloud redelivers it after a reconnect.
func (sk *socket) settleIngress(ctx context.Context, f Ingress) {
	if err := sk.handleIngress(ctx, f); err != nil {
		sk.svc.log.Warn("cloudlink.ingress", "id", f.ID, "err", err)
		return
	}
	if err := sk.writeFrame(Ack{V: ContractV, Type: "ack", ID: f.ID}); err != nil {
		sk.svc.log.Warn("cloudlink.ack", "id", f.ID, "err", err)
	}
}

// handleLogin ends one OAuth login that came back through the cloud and
// sends the result. It runs off the read loop, because the token exchange
// calls the login server.
func (sk *socket) handleLogin(ctx context.Context, f LoginReturn) {
	sk.svc.mu.Lock()
	sink := sk.svc.login
	sk.svc.mu.Unlock()
	res := LoginResult{Status: LoginFailed, Message: "Trebi cannot finish logins on this computer."}
	if sink != nil {
		res = sink.ReturnLogin(ctx, f)
	}
	res.V, res.Type, res.ID = ContractV, "login_result", f.ID
	if err := sk.writeFrame(res); err != nil {
		sk.svc.log.Warn("cloudlink.login", "id", f.ID, "err", err)
	}
}

// handleIngress delivers one ingress frame. A nil return means the frame
// is settled and may be acked: either its sink committed, or the frame
// was deliberately dropped and must not redeliver forever. A non-nil
// return leaves it unacked so the cloud redelivers it.
func (sk *socket) handleIngress(ctx context.Context, f Ingress) error {
	if len(f.Payload) > ingressPayloadCap {
		sk.bumpDrops()
		sk.svc.log.Warn("cloudlink.ingress.drop", "id", f.ID, "reason", "payload over cap")
		return nil
	}
	switch f.Kind {
	case "email":
		var msg email.IngressMessage
		if err := json.Unmarshal(f.Payload, &msg); err != nil {
			sk.bumpDrops()
			sk.svc.log.Warn("cloudlink.ingress.drop", "id", f.ID, "err", err)
			return nil
		}
		if sk.svc.mail == nil {
			sk.bumpDrops()
			return nil
		}
		return sk.svc.mail.Deliver(ctx, msg)
	case "webhook":
		var hook WebhookIngress
		if err := json.Unmarshal(f.Payload, &hook); err != nil {
			sk.bumpDrops()
			sk.svc.log.Warn("cloudlink.ingress.drop", "id", f.ID, "err", err)
			return nil
		}
		if sk.svc.hooks == nil {
			sk.bumpDrops()
			return nil
		}
		return sk.svc.hooks.Deliver(ctx, hook)
	default:
		sk.bumpDrops()
		sk.svc.log.Warn("cloudlink.ingress.drop", "id", f.ID, "kind", f.Kind)
		return nil
	}
}

// openRelay starts one phone session. The session ends with sessCtx, so a
// lost socket ends its phones too. With phone access off it is refused.
func (sk *socket) openRelay(sessCtx context.Context, f RelayOpen) {
	if !sk.svc.mobile() || f.SID == "" {
		sk.bumpDrops()
		if err := sk.writeFrame(RelayClose{V: ContractV, Type: "relay_close", SID: f.SID, Reason: "refused"}); err != nil {
			sk.svc.log.Debug("cloudlink.relay", "err", err)
		}
		return
	}
	sk.mu.Lock()
	sk.sids[f.SID] = struct{}{}
	sk.mu.Unlock()
	sk.svc.relay.OpenRelay(sessCtx, f.SID, f.ClientID)
}

// handleRelay hands the frame's opaque bytes to the relay sink. They are
// decoded from base64 and never interpreted beyond that. A frame for an
// unknown sid is counted and dropped.
func (sk *socket) handleRelay(ctx context.Context, f RelayFrame) {
	raw, err := base64.StdEncoding.DecodeString(f.Bytes)
	if err != nil {
		sk.bumpDrops()
		sk.svc.log.Debug("cloudlink.relay", "err", err)
		return
	}
	sk.mu.Lock()
	_, open := sk.sids[f.SID]
	sk.mu.Unlock()
	if !open {
		sk.bumpDrops()
		return
	}
	sk.svc.relay.DeliverRelay(ctx, f.SID, raw)
}

func (sk *socket) dropSid(sid string) bool {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	_, ok := sk.sids[sid]
	delete(sk.sids, sid)
	return ok
}

// closeSids ends every phone session of the lost connection.
func (sk *socket) closeSids() {
	sk.mu.Lock()
	sids := sk.sids
	sk.sids = map[string]struct{}{}
	sk.mu.Unlock()
	for sid := range sids {
		sk.svc.relay.CloseRelay(sid, "socket_lost")
	}
}

// heartbeatLoop sends heartbeat frames on the interval hello_ok named.
// Stop condition: the session context is cancelled.
func (sk *socket) heartbeatLoop(ctx context.Context, seconds int) {
	d := time.Duration(seconds) * time.Second
	if seconds <= 0 {
		d = defaultHeartbeat
	}
	t := time.NewTicker(d)
	defer t.Stop()
	var sent []byte // the last summary on this socket
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beat := Heartbeat{V: ContractV, Type: "heartbeat"}
			caps, raw := sk.svc.capabilities(ctx)
			if raw != nil && !bytes.Equal(raw, sent) {
				beat.Capabilities = caps
			}
			if err := sk.writeFrame(beat); err != nil {
				return // the read loop sees the loss and reconnects
			}
			if beat.Capabilities != nil {
				sent = raw
			}
		}
	}
}

// dialAny dials the active address. When the connection does not open, it
// dials the next address at once, without the backoff. An HTTP answer, a
// 401 too, never moves the address.
func (sk *socket) dialAny(ctx context.Context) (*websocket.Conn, *http.Response, error) {
	ep := sk.svc.endpoints()
	if ep == nil {
		return nil, nil, errors.New("cloudlink dial: cloud api url is not configured")
	}
	base := ep.Current()
	for tries := 1; ; tries++ {
		conn, resp, err := sk.dial(ctx, base)
		if err == nil || resp != nil || tries >= ep.Len() || ctx.Err() != nil || !connectFailed(err) {
			return conn, resp, err
		}
		sk.svc.log.Warn("cloudlink.dial", "err", err)
		next, ok := ep.Failed(base)
		if !ok || next == base {
			return conn, resp, err
		}
		base = next
	}
}

// dial opens the signed socket at GET /v1/socket on base. The upgrade
// request carries the four signed headers.
func (sk *socket) dial(ctx context.Context, base string) (*websocket.Conn, *http.Response, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, nil, fmt.Errorf("cloudlink dial: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = "/v1/socket"
	u.RawQuery = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cloudlink.invalid/v1/socket", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("cloudlink dial: %w", err)
	}
	SignRequest(req, sk.id, time.Now().UTC())
	header := make(http.Header, len(req.Header))
	for name, values := range req.Header {
		header[name] = values
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: dialTimeout,
		NetDialContext:   (&net.Dialer{Timeout: connectTimeout}).DialContext,
	}
	conn, resp, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		return nil, resp, fmt.Errorf("cloudlink dial: %w", err)
	}
	return conn, nil, nil
}

// writeFrame sends one outbound frame. A failed write logs the
// cloud.push_failed event: that name is log-only (Bus: false) and
// store.Append rejects non-bus topics, so the ops log is its home.
func (sk *socket) writeFrame(v any) error {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	if sk.conn == nil {
		return errors.New("cloudlink: socket is not connected")
	}
	if err := sk.conn.WriteJSON(v); err != nil {
		sk.svc.log.Error("cloud.push_failed", "err", err)
		return fmt.Errorf("cloudlink write: %w", err)
	}
	return nil
}

func (sk *socket) setConn(conn *websocket.Conn) {
	sk.mu.Lock()
	sk.conn = conn
	sk.mu.Unlock()
}

func (sk *socket) clearConn(conn *websocket.Conn) {
	sk.mu.Lock()
	if sk.conn == conn {
		sk.conn = nil
	}
	sk.mu.Unlock()
	_ = conn.Close()
}

// close drops the current connection so the read loop wakes and exits.
func (sk *socket) close() {
	sk.mu.Lock()
	conn := sk.conn
	sk.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// markUp records the transition to connected and publishes
// cloud.connected once per transition.
func (sk *socket) markUp(ctx context.Context) {
	sk.mu.Lock()
	was := sk.connected
	sk.connected = true
	sk.downSince = time.Time{}
	sk.mu.Unlock()
	if !was {
		sk.svc.publish(ctx, "connected")
	}
}

// markDown records the loss and publishes cloud.disconnected once per loss.
func (sk *socket) markDown(ctx context.Context) {
	sk.mu.Lock()
	was := sk.connected
	sk.connected = false
	if was {
		sk.downSince = time.Now()
	}
	sk.mu.Unlock()
	if was {
		sk.svc.publish(ctx, "disconnected")
	}
}

func (sk *socket) setUnauthorized() {
	sk.mu.Lock()
	sk.unauthorized = true
	sk.mu.Unlock()
}

func (sk *socket) isConnected() bool {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	return sk.connected
}

func (sk *socket) downSinceAt() time.Time {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	return sk.downSince
}

func (sk *socket) isUnauthorized() bool {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	return sk.unauthorized
}

func (sk *socket) bumpUnknown() {
	sk.mu.Lock()
	sk.unknown++
	sk.mu.Unlock()
}

func (sk *socket) bumpDrops() {
	sk.mu.Lock()
	sk.drops++
	sk.mu.Unlock()
}

// decodeFrame decodes one socket message into its wire frame. The type
// switch below the read loop keys on the decoded value, which mirrors the
// frame's `type` field; an unknown type decodes to (nil, typ) and is
// counted and dropped. Every contract frame decodes so the wire shapes
// round-trip in frames_test.go.
func decodeFrame(raw []byte) (frame any, typ string, err error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, "", err
	}
	decode, ok := frameDecoders[head.Type]
	if !ok {
		return nil, head.Type, nil
	}
	if frame, err = decode(raw); err != nil {
		return nil, head.Type, err
	}
	return frame, head.Type, nil
}

//nolint:gochecknoglobals // static decoder table, never mutated
var frameDecoders = map[string]func([]byte) (any, error){
	"hello":          decodeAs[Hello],
	"heartbeat":      decodeAs[Heartbeat],
	"alert":          decodeAs[Alert],
	"ack":            decodeAs[Ack],
	"hello_ok":       decodeAs[HelloOK],
	"entitlement":    decodeAs[Entitlement],
	"ingress":        decodeAs[Ingress],
	"relay_frame":    decodeAs[RelayFrame],
	"relay_open":     decodeAs[RelayOpen],
	"relay_close":    decodeAs[RelayClose],
	"client_revoked": decodeAs[ClientRevoked],
	"mcp_request":    decodeAs[McpRequest],
	"mcp_response":   decodeAs[McpResponse],
	"login_return":   decodeAs[LoginReturn],
	"login_result":   decodeAs[LoginResult],
}

func decodeAs[T any](raw []byte) (any, error) {
	var f T
	err := json.Unmarshal(raw, &f)
	return f, err
}

// sleepJitter waits a full-jitter slice of max (uniform in [0, max)) and
// reports whether the wait completed rather than the context ending.
func sleepJitter(ctx context.Context, max time.Duration) bool {
	t := time.NewTimer(time.Duration(rand.Int64N(int64(max))))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RelaySocket is the client side of GET /v1/relay: each text message is the
// base64 of one Noise message. The peer link uses it.
type RelaySocket struct {
	conn *websocket.Conn
	wmu  sync.Mutex
}

func dialRelay(ctx context.Context, base, token string) (*RelaySocket, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("cloudlink relay: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = "/v1/relay"
	u.RawQuery = ""
	dialer := websocket.Dialer{
		HandshakeTimeout: dialTimeout,
		NetDialContext:   (&net.Dialer{Timeout: connectTimeout}).DialContext,
	}
	header := http.Header{"Authorization": {"Bearer " + token}, "User-Agent": {apiUserAgent}}
	conn, resp, err := dialer.DialContext(ctx, u.String(), header)
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = resp.Body.Close() }()
		return nil, apiError(resp)
	}
	if err != nil {
		return nil, fmt.Errorf("cloudlink relay: %w", err)
	}
	conn.SetReadLimit(maxRelayFrame)
	return &RelaySocket{conn: conn}, nil
}

// maxRelayFrame is the base64 of the largest Noise message.
const maxRelayFrame = 1 << 17

// Send writes one Noise message.
func (r *RelaySocket) Send(msg []byte) error {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	return r.conn.WriteMessage(websocket.TextMessage, []byte(base64.StdEncoding.EncodeToString(msg)))
}

// Recv reads one Noise message. It drops binary messages.
func (r *RelaySocket) Recv() ([]byte, error) {
	for {
		typ, raw, err := r.conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		if typ != websocket.TextMessage {
			continue
		}
		return base64.StdEncoding.DecodeString(string(raw))
	}
}

// Close ends the socket.
func (r *RelaySocket) Close() error { return r.conn.Close() }
