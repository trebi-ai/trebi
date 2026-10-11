package cloudlink

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	connectTimeout = 8 * time.Second // dial and TLS handshake; a blocked address fails fast
	probeEvery     = 10 * time.Minute
)

// endpoints owns the cloud API addresses and the active one. The primary
// address is first. The client and the socket share one instance, so a
// failover on one moves both. Only a connection error moves the address
// (see connectFailed): the signature has no host, so each address accepts
// the same signed request.
type endpoints struct {
	mu     sync.Mutex
	urls   []string
	active int

	onSwitch func(from, to string)                        // nil in tests
	probe    func(ctx context.Context, base string) error // GET /health; replaced in tests
}

func newEndpoints(urls []string) *endpoints {
	e := &endpoints{urls: urls}
	e.probe = e.health
	return e
}

// Current is the active address.
func (e *endpoints) Current() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.urls[e.active]
}

// Len is the number of addresses.
func (e *endpoints) Len() int { return len(e.urls) }

// Primary is the first address, which the pairing link names.
func (e *endpoints) Primary() string {
	return e.urls[0]
}

// OnFallback reports whether the active address is not the primary one.
func (e *endpoints) OnFallback() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active != 0
}

// Failed moves to the next address after a connection error on base. It
// moves only when base is still the active address, so two calls that fail
// at the same time move once. It reports false when no other address exists.
func (e *endpoints) Failed(base string) (next string, ok bool) {
	e.mu.Lock()
	if len(e.urls) < 2 {
		e.mu.Unlock()
		return base, false
	}
	if e.urls[e.active] != base {
		next = e.urls[e.active]
		e.mu.Unlock()
		return next, true
	}
	e.active = (e.active + 1) % len(e.urls)
	next = e.urls[e.active]
	e.mu.Unlock()
	if e.onSwitch != nil {
		e.onSwitch(base, next)
	}
	return next, true
}

// ProbePrimary moves back to the primary address when it answers. It does
// nothing while the primary address is active.
func (e *endpoints) ProbePrimary(ctx context.Context) {
	e.mu.Lock()
	if e.active == 0 {
		e.mu.Unlock()
		return
	}
	from := e.urls[e.active]
	e.mu.Unlock()
	if e.probe(ctx, e.urls[0]) != nil {
		return
	}
	e.mu.Lock()
	moved := e.urls[e.active] == from
	if moved {
		e.active = 0
	}
	e.mu.Unlock()
	if moved && e.onSwitch != nil {
		e.onSwitch(from, e.urls[0])
	}
}

// health sends GET /health to base. Any 2xx answer is a pass.
func (e *endpoints) health(ctx context.Context, base string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*connectTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", apiUserAgent)
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, apiErrCap)) // drain for reuse; the body is not used
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("cloud health %s: %s", base, resp.Status)
	}
	return nil
}

// hostOf is host[:port] of the given address, or "".
func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Host
}

// newHTTPClient is the cloud API client: short connect timeouts so a
// blocked address fails fast, and a 30 s limit for the full call.
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = connectTimeout
	return &http.Client{Timeout: 30 * time.Second, Transport: tr}
}

// connectFailed reports whether the request never reached the server: a
// dial error, a DNS error, or a TLS handshake error. Only then is it safe
// to send the same request to another address, because POST /v1/send is
// not idempotent.
func connectFailed(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var recErr tls.RecordHeaderError
	if errors.As(err, &recErr) {
		return true
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var alertErr tls.AlertError
	if errors.As(err, &alertErr) {
		return true
	}
	// net/http gives a handshake timeout an unexported type; only the text names it.
	return strings.Contains(err.Error(), "TLS handshake timeout")
}
