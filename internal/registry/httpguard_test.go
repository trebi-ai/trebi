package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestSafeClientRefuses(t *testing.T) {
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(tls.Close)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(plain.Close)
	hc := SafeClient(2 * time.Second)
	for _, raw := range []string{
		plain.URL,                        // http
		tls.URL,                          // https on a loopback literal
		"https://localhost/x",            // a local name
		"https://[::1]/x",                // IPv6 loopback
		"https://169.254.169.254/latest", // link-local metadata address
		"https://192.168.1.10/x",         // private
		"https://100.64.0.1/x",           // shared address space
		"ftp://example.com/x",
	} {
		_, err := readCapped(context.Background(), hc, raw, 10)
		if !errors.Is(err, ErrUnsafeURL) {
			t.Fatalf("%s: err = %v, want ErrUnsafeURL", raw, err)
		}
	}
}

// TestSafeClientRefusesRedirect proves a redirect to http is refused: the
// client sends every hop through the https check.
func TestSafeClientRefusesRedirect(t *testing.T) {
	hops := 0
	fake := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hops++
		h := http.Header{}
		h.Set("Location", "http://example.com/skill.md")
		return &http.Response{StatusCode: http.StatusFound, Header: h, Body: http.NoBody, Request: r}, nil
	})
	hc := &http.Client{Transport: httpsOnly{next: fake}}
	_, err := readCapped(context.Background(), hc, "https://example.com/skill.md", 10)
	if !errors.Is(err, ErrUnsafeURL) || hops != 1 {
		t.Fatalf("err = %v hops = %d", err, hops)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestDialControl proves the resolved-address check, which catches a DNS
// name that points at a private address.
func TestDialControl(t *testing.T) {
	cases := map[string]bool{
		"93.184.216.34:443":          true,
		"[2606:4700::6810:84e5]:443": true,
		"127.0.0.1:443":              false,
		"10.0.0.1:443":               false,
		"172.16.5.4:443":             false,
		"[fe80::1]:443":              false,
		"[::ffff:127.0.0.1]:443":     false,
		"0.0.0.0:443":                false,
		"[fd00::1]:443":              false,
	}
	for addr, ok := range cases {
		err := dialControl("tcp", addr, nil)
		if (err == nil) != ok {
			t.Fatalf("%s: err = %v, want ok=%v", addr, err, ok)
		}
	}
	if !blockedAddr(netip.Addr{}) {
		t.Fatal("an invalid address must be blocked")
	}
}

func TestCheckURL(t *testing.T) {
	if err := CheckURL("https://registry.example.com/skill.md"); err != nil {
		t.Fatal(err)
	}
	if err := CheckURL("https://LOCALHOST./x"); err == nil || !strings.Contains(err.Error(), "local") {
		t.Fatalf("err = %v", err)
	}
}
