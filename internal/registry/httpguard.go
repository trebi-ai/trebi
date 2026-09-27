package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrUnsafeURL is a registry URL that is not https or that names a
// loopback, link-local, or private address (decision 6).
var ErrUnsafeURL = errors.New("unsafe registry url")

// cgnat is the shared address space of RFC 6598. netip does not mark it as
// private.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// SafeClient refuses non-https URLs, loopback, link-local, and private addresses, including after a redirect.
func SafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, Control: dialControl}
	tr := &http.Transport{
		Proxy:               nil, // a proxy dial would skip the address check
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: timeout,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: httpsOnly{next: tr},
	}
}

// httpsOnly refuses each request that is not https. The client sends every
// redirect through it too.
type httpsOnly struct{ next http.RoundTripper }

func (t httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(req)
}

// dialControl checks the resolved address just before the connect, so a DNS
// name that points at a private address is refused too.
func dialControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: address %q", ErrUnsafeURL, address)
	}
	if blockedAddr(ap.Addr()) {
		return fmt.Errorf("%w: address %s is not public", ErrUnsafeURL, ap.Addr())
	}
	return nil
}

// blockedAddr reports an address that a registry URL must not reach.
func blockedAddr(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() ||
		a.IsUnspecified() || cgnat.Contains(a)
}

// checkURL refuses a URL that is not https, or whose host is a local name or
// a blocked address literal. A DNS name is checked again at dial time.
func checkURL(u *url.URL) error {
	if u == nil || u.Scheme != "https" {
		return fmt.Errorf("%w: https required", ErrUnsafeURL)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return fmt.Errorf("%w: host required", ErrUnsafeURL)
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("%w: host %s is local", ErrUnsafeURL, host)
	}
	if a, err := netip.ParseAddr(host); err == nil && blockedAddr(a) {
		return fmt.Errorf("%w: address %s is not public", ErrUnsafeURL, a)
	}
	return nil
}

// CheckURL parses raw and applies the https and address checks.
func CheckURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafeURL, err)
	}
	return checkURL(u)
}

// maxSkillBody caps a contributed SKILL.md.
const maxSkillBody = 1 << 20

// readCapped reads at most limit bytes. A longer body is an error, not a cut.
func readCapped(ctx context.Context, hc *http.Client, raw string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(raw), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", req.URL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: body is larger than %d bytes", req.URL, limit)
	}
	return body, nil
}
