package relay

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/flynn/noise"
	"github.com/oklog/ulid/v2"
)

// Direct transport limits (CONTRACT.md "Direct transport").
const (
	// MaxDirectMessage is the largest Noise message one frame carries.
	MaxDirectMessage = 65535
	// DefaultDirectPort is the default mobile.direct.port.
	DefaultDirectPort = 7434

	directHandshakeTimeout = 5 * time.Second
	directPendingPerIP     = 4
	directMaxAddrs         = 8
	directWriteTimeout     = 30 * time.Second
	defaultAddrInterval    = 30 * time.Second
	defaultGateInterval    = 5 * time.Second
)

var errFrame = errors.New("relay: bad direct frame")

var cgnat = netip.MustParsePrefix("100.64.0.0/10") //nolint:gochecknoglobals // immutable prefix (Tailscale and other VPNs)

// WriteFrame writes one Noise message with its 2-byte big-endian length.
func WriteFrame(w io.Writer, msg []byte) error {
	if len(msg) == 0 || len(msg) > MaxDirectMessage {
		return fmt.Errorf("%w: length %d", errFrame, len(msg))
	}
	buf := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(buf, uint16(len(msg))) //nolint:gosec // checked above
	copy(buf[2:], msg)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one framed Noise message. A clean close before a frame
// is io.EOF; a close inside a frame or a zero length is errFrame.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: short read", errFrame)
		}
		return nil, err
	}
	n := binary.BigEndian.Uint16(hdr[:])
	if n == 0 {
		return nil, fmt.Errorf("%w: length 0", errFrame)
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, fmt.Errorf("%w: short read: %w", errFrame, err)
	}
	return msg, nil
}

// tcpWire sends framed messages on one direct connection.
type tcpWire struct {
	conn net.Conn
	mu   sync.Mutex
}

func (w *tcpWire) Send(msg []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(directWriteTimeout)) // a failed deadline shows on the write
	return WriteFrame(w.conn, msg)
}

func (w *tcpWire) Close(string) { _ = w.conn.Close() } // the session ends either way

// LocalAddrs lists the addresses of the up interfaces that a phone on the
// same network can reach: RFC 1918, 100.64.0.0/10, and IPv6 ULA.
func LocalAddrs() ([]netip.Addr, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			pn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip, ok := netip.AddrFromSlice(pn.IP); ok && directAddr(ip) {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}

// directAddr reports whether a phone on the same network can reach ip.
func directAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid(), ip.IsLoopback(), ip.IsLinkLocalUnicast(), ip.IsUnspecified(), ip.IsMulticast():
		return false
	case ip.IsPrivate(): // RFC 1918 and fc00::/7
		return true
	default:
		return cgnat.Contains(ip)
	}
}

// DirectOptions configures the direct listener. Zero values use the defaults.
type DirectOptions struct {
	// Listen is the bind address; default ":<Port>" on all interfaces.
	Listen string
	Port   int
	// Addrs lists the addresses to announce; default LocalAddrs.
	Addrs        func() ([]netip.Addr, error)
	AddrInterval time.Duration
	GateInterval time.Duration
}

// Direct serves paired phones on the same network over TCP. The hub owns
// it; ServeDirect runs it.
type Direct struct {
	hub   *Hub
	ln    net.Listener
	port  int
	addrs func() ([]netip.Addr, error)
	log   *slog.Logger

	mu      sync.Mutex
	pending map[string]int // handshakes in flight by remote IP
	current []string
}

// ServeDirect binds the direct listener and serves until ctx ends. A bind
// error returns at once; DirectError keeps it for doctor.
func (h *Hub) ServeDirect(ctx context.Context, o DirectOptions) error {
	if o.Port == 0 {
		o.Port = DefaultDirectPort
	}
	if o.Listen == "" {
		o.Listen = ":" + strconv.Itoa(o.Port)
	}
	if o.Addrs == nil {
		o.Addrs = LocalAddrs
	}
	if o.AddrInterval == 0 {
		o.AddrInterval = defaultAddrInterval
	}
	if o.GateInterval == 0 {
		o.GateInterval = defaultGateInterval
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", o.Listen)
	h.mu.Lock()
	h.directErr = err
	h.mu.Unlock()
	if err != nil {
		h.log.Warn("mobile.direct.bind_failed", "listen", o.Listen, "err", err)
		return fmt.Errorf("direct listener: %w", err)
	}
	d := &Direct{hub: h, ln: ln, port: ln.Addr().(*net.TCPAddr).Port, addrs: o.Addrs, //nolint:forcetypeassert // a tcp listener
		log: h.log, pending: map[string]int{}}
	d.refresh()
	h.mu.Lock()
	h.direct = d
	h.mu.Unlock()
	h.log.Info("mobile.direct.listen", "addr", ln.Addr().String(), "addrs", d.list())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // stop: ctx ends
		defer wg.Done()
		d.watch(ctx, o.AddrInterval, o.GateInterval)
	}()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() }) // Accept returns net.ErrClosed
	defer stop()
	d.accept(ctx)
	wg.Wait()

	h.mu.Lock()
	h.direct = nil
	h.mu.Unlock()
	for _, s := range h.matching(func(s *session) bool { return s.direct }) {
		s.end("", CodeDaemonStopping)
	}
	return nil
}

// DirectError is the last bind error of the direct listener, or nil.
func (h *Hub) DirectError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.directErr
}

// DirectAddr is the bound address of the direct listener, or nil when it
// is closed.
func (h *Hub) DirectAddr() net.Addr {
	if d := h.directServer(); d != nil {
		return d.Addr()
	}
	return nil
}

func (h *Hub) directServer() *Direct {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.direct
}

// Addr is the bound listener address.
func (d *Direct) Addr() net.Addr { return d.ln.Addr() }

func (d *Direct) accept(ctx context.Context) {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			d.log.Debug("mobile.direct.accept", "err", err)
			continue
		}
		d.serve(ctx, conn)
	}
}

// serve starts one direct session. The session goroutines stop when the
// connection closes or ctx ends.
func (d *Direct) serve(ctx context.Context, conn net.Conn) {
	ip := conn.RemoteAddr().String()
	if ap, err := netip.ParseAddrPort(ip); err == nil {
		ip = ap.Addr().Unmap().String()
	}
	if !d.reserve(ip) {
		_ = conn.Close() // too many handshakes from this address
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true) // already the Go default
	}
	h := d.hub
	s := h.newSession("direct-"+ulid.Make().String(), &tcpWire{conn: conn})
	s.direct, s.remote, s.hsTimeout = true, conn.RemoteAddr().String(), directHandshakeTimeout
	var once sync.Once
	s.afterHandshake = func() { once.Do(func() { d.release(ip) }) }
	if h.add(s) != addOK {
		s.afterHandshake()
		_ = conn.Close()
		return
	}
	go s.run(ctx)      // stop: s.done closes, or ctx ends
	go s.readTCP(conn) // stop: the connection closes
}

// readTCP feeds the framed messages of a direct connection into the
// session. It blocks on a full queue, so TCP pushes back on the phone.
func (s *session) readTCP(conn net.Conn) {
	for {
		msg, err := ReadFrame(conn)
		if err != nil {
			s.end("", "")
			return
		}
		s.queued.Add(int64(len(msg)))
		select {
		case s.in <- msg:
		case <-s.done:
			return
		}
	}
}

func (d *Direct) reserve(ip string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pending[ip] >= directPendingPerIP {
		return false
	}
	d.pending[ip]++
	return true
}

func (d *Direct) release(ip string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pending[ip]--; d.pending[ip] <= 0 {
		delete(d.pending, ip)
	}
}

func (d *Direct) list() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.current)
}

// watch checks the addresses and the gate. A new address list goes to
// every relay session; a closed gate ends the direct sessions.
func (d *Direct) watch(ctx context.Context, addrEvery, gateEvery time.Duration) {
	addrT := time.NewTicker(addrEvery)
	defer addrT.Stop()
	gateT := time.NewTicker(gateEvery)
	defer gateT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-addrT.C:
			if d.refresh() {
				for _, s := range d.hub.matching(func(s *session) bool { return !s.direct && s.clientID() != "" }) {
					d.announce(s)
				}
			}
		case <-gateT.C:
			if !d.hub.gate.MobileAllowed() {
				for _, s := range d.hub.matching(func(s *session) bool { return s.direct }) {
					s.end("", "")
				}
			}
		}
	}
}

// refresh reads the address list and reports whether it changed.
func (d *Direct) refresh() bool {
	ips, err := d.addrs()
	if err != nil {
		d.log.Debug("mobile.direct.addrs", "err", err)
		return false
	}
	next := make([]string, 0, len(ips))
	for _, ip := range ips {
		next = append(next, net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(d.port)))
	}
	slices.Sort(next)
	next = slices.Compact(next)
	if len(next) > directMaxAddrs {
		next = next[:directMaxAddrs]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if slices.Equal(next, d.current) {
		return false
	}
	d.current = next
	return true
}

// announce sends the direct record on one ready relay session.
func (d *Direct) announce(s *session) {
	rec := NewDirectRecord(d.list())
	err := s.seal(func(cs *noise.CipherState) ([]byte, error) { return SealControl(cs, rec) })
	if err != nil {
		d.log.Debug("mobile.direct.announce", "sid", s.sid, "err", err)
	}
}
