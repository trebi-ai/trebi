package cloudlink

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
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
	dialTimeout       = 30 * time.Second
)

// socket is one live connection to the cloud: signed dial, frame
// dispatch, heartbeats, and bounded reconnect. It holds no state beyond
// the current session; the Service owns the link row and the sinks.
type socket struct {
	svc  *Service
	base string
	id   Identity

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
	conn, resp, err := sk.dial(ctx)
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
		if err := sk.handleIngress(ctx, f); err != nil {
			// Unacked on purpose: the cloud redelivers after a reconnect.
			sk.svc.log.Warn("cloudlink.ingress", "id", f.ID, "err", err)
			return false
		}
		// Settled (delivered or deliberately dropped): ack it.
		if err := sk.writeFrame(Ack{V: ContractV, Type: "ack", ID: f.ID}); err != nil {
			sk.svc.log.Warn("cloudlink.ack", "id", f.ID, "err", err)
		}
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
	default:
		sk.bumpUnknown()
		sk.svc.log.Debug("cloudlink.frame", "type", typ)
	}
	return false
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := sk.writeFrame(Heartbeat{V: ContractV, Type: "heartbeat"}); err != nil {
				return // the read loop sees the loss and reconnects
			}
		}
	}
}

// dial opens the signed socket at GET /v1/socket. The upgrade request
// carries the four signed headers.
func (sk *socket) dial(ctx context.Context) (*websocket.Conn, *http.Response, error) {
	u, err := url.Parse(sk.base)
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
	dialer := websocket.Dialer{HandshakeTimeout: dialTimeout}
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
	switch head.Type {
	case "hello":
		var f Hello
		err = json.Unmarshal(raw, &f)
		frame = f
	case "heartbeat":
		var f Heartbeat
		err = json.Unmarshal(raw, &f)
		frame = f
	case "alert":
		var f Alert
		err = json.Unmarshal(raw, &f)
		frame = f
	case "ack":
		var f Ack
		err = json.Unmarshal(raw, &f)
		frame = f
	case "hello_ok":
		var f HelloOK
		err = json.Unmarshal(raw, &f)
		frame = f
	case "entitlement":
		var f Entitlement
		err = json.Unmarshal(raw, &f)
		frame = f
	case "ingress":
		var f Ingress
		err = json.Unmarshal(raw, &f)
		frame = f
	case "relay_frame":
		var f RelayFrame
		err = json.Unmarshal(raw, &f)
		frame = f
	case "relay_open":
		var f RelayOpen
		err = json.Unmarshal(raw, &f)
		frame = f
	case "relay_close":
		var f RelayClose
		err = json.Unmarshal(raw, &f)
		frame = f
	case "client_revoked":
		var f ClientRevoked
		err = json.Unmarshal(raw, &f)
		frame = f
	case "mcp_request":
		var f McpRequest
		err = json.Unmarshal(raw, &f)
		frame = f
	case "mcp_response":
		var f McpResponse
		err = json.Unmarshal(raw, &f)
		frame = f
	default:
		return nil, head.Type, nil
	}
	if err != nil {
		return nil, head.Type, err
	}
	return frame, head.Type, nil
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
