package cloudlink

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/email"
)

func TestEndpointsFailedMovesOnceFromCurrent(t *testing.T) {
	t.Parallel()
	e := newEndpoints([]string{"https://a", "https://b"})
	var switches []string
	e.onSwitch = func(from, to string) { switches = append(switches, from+">"+to) }

	if next, ok := e.Failed("https://a"); !ok || next != "https://b" || !e.OnFallback() {
		t.Fatalf("Failed(a) = %q %v, fallback %v", next, ok, e.OnFallback())
	}
	// A late failure on the old address does not move again.
	if next, ok := e.Failed("https://a"); !ok || next != "https://b" {
		t.Fatalf("stale Failed(a) = %q %v", next, ok)
	}
	// All addresses failed: the list wraps to the primary address.
	if next, _ := e.Failed("https://b"); next != "https://a" || e.OnFallback() {
		t.Fatalf("Failed(b) = %q, fallback %v", next, e.OnFallback())
	}
	if strings.Join(switches, " ") != "https://a>https://b https://b>https://a" {
		t.Fatalf("switches = %v", switches)
	}
}

func TestEndpointsConcurrentFailedMovesOnce(t *testing.T) {
	t.Parallel()
	e := newEndpoints([]string{"https://a", "https://b", "https://c"})
	var n atomic.Int32
	e.onSwitch = func(string, string) { n.Add(1) }
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Failed("https://a")
		}()
	}
	wg.Wait()
	if n.Load() != 1 || e.Current() != "https://b" {
		t.Fatalf("switches = %d, current %q", n.Load(), e.Current())
	}
}

func TestEndpointsSingleAddressNeverMoves(t *testing.T) {
	t.Parallel()
	e := newEndpoints([]string{"https://a"})
	if next, ok := e.Failed("https://a"); ok || next != "https://a" || e.OnFallback() {
		t.Fatalf("Failed = %q %v", next, ok)
	}
}

func TestEndpointsProbePrimary(t *testing.T) {
	t.Parallel()
	e := newEndpoints([]string{"https://a", "https://b"})
	probes := 0
	var probeErr error
	e.probe = func(_ context.Context, base string) error {
		probes++
		if base != "https://a" {
			t.Errorf("probe %q, want the primary address", base)
		}
		return probeErr
	}
	e.ProbePrimary(context.Background())
	if probes != 0 {
		t.Fatal("probe ran while on the primary address")
	}
	e.Failed("https://a")
	probeErr = errors.New("still blocked")
	e.ProbePrimary(context.Background())
	if !e.OnFallback() {
		t.Fatal("moved back after a failed probe")
	}
	probeErr = nil
	e.ProbePrimary(context.Background())
	if e.OnFallback() || e.Current() != "https://a" {
		t.Fatalf("no move back after a good probe: %q", e.Current())
	}
}

func TestEndpointsHealthProbe(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	e := newEndpoints([]string{srv.URL})
	if err := e.health(context.Background(), srv.URL); err != nil {
		t.Fatalf("health on a live server: %v", err)
	}
	if err := e.health(context.Background(), deadURL()); err == nil {
		t.Fatal("health on a closed port passed")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestConnectFailed(t *testing.T) {
	t.Parallel()
	_, refused := net.Dial("tcp", strings.TrimPrefix(deadURL(), "http://"))
	untrusted := httptest.NewTLSServer(http.NotFoundHandler()) // its certificate fails the handshake
	t.Cleanup(untrusted.Close)
	_, tlsErr := newHTTPClient().Get(untrusted.URL)

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"dial refused", refused, true},
		{"dial timeout", &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}, true},
		{"dns", fmt.Errorf("wrap: %w", &net.DNSError{Err: "no such host", Name: "x.invalid"}), true},
		{"tls handshake", tlsErr, true},
		{"tls handshake timeout", errors.New("net/http: TLS handshake timeout"), true},
		{"read timeout", &net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}, false},
		{"eof after write", io.ErrUnexpectedEOF, false},
		{"canceled", context.Canceled, false},
		{"nil", nil, false},
	} {
		if got := connectFailed(tc.err); got != tc.want {
			t.Errorf("%s: connectFailed(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestClientFailsOverOnDialError: a refused dial sends the same call to
// the next address, signed, and the move sticks for the next call.
func TestClientFailsOverOnDialError(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get(headerDevice) != "dev-1" || r.Header.Get(headerSignature) == "" || r.Header.Get(headerNonce) == "" {
			http.Error(w, "unsigned", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"message_id":"msg-1"}`))
	}))
	t.Cleanup(srv.Close)
	ep := newEndpoints([]string{deadURL(), srv.URL})
	c := NewClient(ep, testIdentity())

	res, err := c.Send(context.Background(), email.SendRequest{})
	if err != nil || res.MessageID != "msg-1" {
		t.Fatalf("Send = (%+v, %v)", res, err)
	}
	if !ep.OnFallback() || ep.Current() != srv.URL {
		t.Fatalf("current %q after failover", ep.Current())
	}
	if _, err := c.Send(context.Background(), email.SendRequest{}); err != nil || calls.Load() != 2 {
		t.Fatalf("second Send: %v, calls %d", err, calls.Load())
	}
}

// TestClientNoFailoverAfterReply: any HTTP status, and a timeout after the
// request was sent, keep the address. Send is not idempotent.
func TestClientNoFailoverAfterReply(t *testing.T) {
	t.Parallel()
	var backup atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backup.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(second.Close)

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }},
		{"429", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }},
		{"hang", func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}},
	} {
		first := httptest.NewServer(tc.handler)
		ep := newEndpoints([]string{first.URL, second.URL})
		c := NewClient(ep, testIdentity())
		c.http.Timeout = 200 * time.Millisecond
		if _, err := c.Send(context.Background(), email.SendRequest{}); err == nil {
			t.Errorf("%s: Send passed", tc.name)
		}
		if ep.OnFallback() {
			t.Errorf("%s: moved to the backup address", tc.name)
		}
		first.Close()
	}
	if n := backup.Load(); n != 0 {
		t.Fatalf("backup address got %d calls", n)
	}
}

// deadURL is an http URL on port 1: every dial is refused, and no parallel
// test server can take the port.
func deadURL() string { return "http://127.0.0.1:1" }

func testIdentity() Identity {
	return Identity{deviceID: "dev-1", key: ed25519.NewKeyFromSeed(knownSeed)}
}
