// Package relay serves paired phones over the cloud relay. Each phone socket
// carries one Noise IKpsk2 session (trebi-relay/1). Inside it, yamux carries
// one stream per webview connection, and each stream carries plain HTTP/1.1
// to the same root handler as the loopback listener. The cloud sees only
// ciphertext. testdata/contract/CONTRACT.md is the wire contract.
package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/flynn/noise"
)

// Protocol constants. The Rust client in trebi-ui/mobile/relay uses the same values.
const (
	ProtocolName = "trebi-relay/1"
	ProtocolV    = 1
	// MaxRecord is the plaintext size of one record, type byte included.
	MaxRecord = 16 << 10

	recordMux     byte = 0x00
	recordControl byte = 0x01
)

// Control codes in an error record.
const (
	CodeRevoked        = "revoked"
	CodeScopeChanged   = "scope_changed"
	CodeDaemonStopping = "daemon_stopping"
)

// Close reasons in a relay_close frame.
const (
	ReasonRefused    = "refused"
	ReasonLimit      = "limit"
	ReasonRevoked    = "revoked"
	ReasonDaemonOff  = "daemon_off"
	ReasonClientGone = "client_gone"
)

var (
	errProtocol = errors.New("relay: protocol error")
	errRefused  = errors.New("relay: handshake refused")
)

// CipherSuite is Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s.
var CipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s) //nolint:gochecknoglobals // immutable protocol constant

// Prologue binds a session to one machine.
func Prologue(deviceID string) []byte {
	return append([]byte(ProtocolName+"\x00"), deviceID...)
}

// Hello is the message 1 payload from the phone.
type Hello struct {
	V          int    `json:"v"`
	ClientID   string `json:"client_id"`
	Offer      string `json:"offer"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	AppVersion string `json:"app_version"`
}

// Welcome is the message 2 payload from the daemon. ClientPSK is set only
// when the session makes a pairing.
type Welcome struct {
	V             int    `json:"v"`
	Machine       string `json:"machine"`
	DaemonVersion string `json:"daemon_version"`
	Scope         string `json:"scope"`
	ClientPSK     string `json:"client_psk,omitempty"`
}

// Control is the JSON body of a control record. Each side sends "ready"
// first after the handshake. "error" ends the session.
type Control struct {
	Type string `json:"type"`
	Code string `json:"code,omitempty"`
}

// HandshakeConfig builds the Noise state for one side. A nil Ephemeral
// makes a random key; the contract vectors pin it.
func HandshakeConfig(initiator bool, deviceID string, static noise.DHKey, peer []byte, psk []byte, ephemeral *noise.DHKey) noise.Config {
	cfg := noise.Config{
		CipherSuite:           CipherSuite,
		Pattern:               noise.HandshakeIK,
		Initiator:             initiator,
		Prologue:              Prologue(deviceID),
		PresharedKey:          psk,
		PresharedKeyPlacement: 2,
		StaticKeypair:         static,
		PeerStatic:            peer,
	}
	if ephemeral != nil {
		cfg.Random = bytes.NewReader(ephemeral.Private) // GenerateKeypair reads the 32 private bytes
	}
	return cfg
}

// SealRecord encrypts one record. data must fit MaxRecord-1 bytes.
func SealRecord(cs *noise.CipherState, typ byte, data []byte) ([]byte, error) {
	if len(data) > MaxRecord-1 {
		return nil, fmt.Errorf("relay: record of %d bytes is too large", len(data))
	}
	pt := make([]byte, 0, len(data)+1)
	pt = append(pt, typ)
	pt = append(pt, data...)
	return cs.Encrypt(nil, nil, pt)
}

// OpenRecord decrypts one record and splits off its type byte.
func OpenRecord(cs *noise.CipherState, msg []byte) (byte, []byte, error) {
	pt, err := cs.Decrypt(nil, nil, msg)
	if err != nil {
		return 0, nil, fmt.Errorf("relay: decrypt: %w", err)
	}
	if len(pt) == 0 || (pt[0] != recordMux && pt[0] != recordControl) {
		return 0, nil, errProtocol
	}
	return pt[0], pt[1:], nil
}

// SealControl encrypts one control record.
func SealControl(cs *noise.CipherState, c Control) ([]byte, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return SealRecord(cs, recordControl, raw)
}

// SplitMux cuts mux bytes into record-sized chunks.
func SplitMux(p []byte) [][]byte {
	var out [][]byte
	for len(p) > 0 {
		n := min(len(p), MaxRecord-1)
		out = append(out, p[:n])
		p = p[n:]
	}
	return out
}

// recordConn is the byte stream that yamux runs on. Read decrypts frames
// from the inbound queue; Write splits, encrypts, and sends. Noise nonces
// make a replayed or reordered frame fail to decrypt, which ends the session.
type recordConn struct {
	recv    *noise.CipherState
	in      <-chan []byte
	queued  *atomic.Int64 // bytes in in; nil in tests
	done    <-chan struct{}
	onError func(error)
	onCtrl  func(Control) error
	buf     []byte

	send    func(seal func(*noise.CipherState) ([]byte, error)) error
	closeFn func() error
}

func (c *recordConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		var msg []byte
		select {
		case msg = <-c.in:
		case <-c.done:
			return 0, io.EOF
		}
		if c.queued != nil {
			c.queued.Add(-int64(len(msg)))
		}
		typ, data, err := OpenRecord(c.recv, msg)
		if err != nil {
			c.onError(err)
			return 0, err
		}
		if typ == recordControl {
			var ctl Control
			if err := json.Unmarshal(data, &ctl); err != nil {
				c.onError(errProtocol)
				return 0, errProtocol
			}
			if err := c.onCtrl(ctl); err != nil {
				return 0, err
			}
			continue
		}
		c.buf = data
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *recordConn) Write(p []byte) (int, error) {
	for _, chunk := range SplitMux(p) {
		err := c.send(func(cs *noise.CipherState) ([]byte, error) { return SealRecord(cs, recordMux, chunk) })
		if err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (c *recordConn) Close() error { return c.closeFn() }
