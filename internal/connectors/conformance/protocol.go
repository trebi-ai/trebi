package conformance

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
)

// protocolSuite runs the checks of Part H through protocol.Checker. In the
// sandbox it starts the program on empty folders; in live mode it uses the
// session of the connection and does not stop it.
type protocolSuite struct{}

func (protocolSuite) name() string { return SuiteProtocol }

func (protocolSuite) applies(m manifest.Manifest) bool {
	return m.Protocol() == manifest.ProtocolConnector
}

func (protocolSuite) run(ctx context.Context, r *run) []Result {
	chk := r.checker()
	var res []protocol.CheckResult
	if r.live {
		if r.t.Live == nil {
			return []Result{row(SuiteProtocol, "initialize", errors.New("the connection has no live session"))}
		}
		init := r.t.Live.Init()
		chk.Init = &init
		var st protocol.AuthStatusResult
		if err := r.t.Live.Call(ctx, protocol.MethodAuthStatus, protocol.Empty{}, &st); err != nil || st.State != protocol.StateAuthRequired {
			chk.SkipLogin = true
		}
		res = chk.Run(ctx, r.t.Live)
	} else {
		d, err := r.folders()
		if err != nil {
			return []Result{row(SuiteProtocol, "start", err)}
		}
		chk.Sandbox, chk.EmptyState, chk.Dirs = true, true, []string{d.state, d.cache}
		if spec := r.t.Manifest.SubscriptionSpec(); spec != nil && spec.Webhook != nil {
			sink, err := newHookSink(*spec.Webhook)
			if err != nil {
				return []Result{row(SuiteProtocol, "start", err)}
			}
			defer sink.close()
			chk.Hooks = sink
		}
		a, err := r.start(d, r.inputs(), chk.Notify, chk.BadLine)
		if err != nil {
			return []Result{row(SuiteProtocol, "start", err)}
		}
		res = chk.Run(ctx, a.conn)
		a.stop()
		if len(res) == 1 && res[0].Status == protocol.CheckFail && !a.alive() {
			res[0].Message += "; " + a.exitMessage()
		}
	}
	out := make([]Result, 0, len(res))
	for _, c := range res {
		out = append(out, Result{Suite: SuiteProtocol, Check: c.Check, Status: c.Status, Message: c.Message})
	}
	return out
}

// hookMaxBody caps one recorded request, as the cloud does.
const hookMaxBody = 1 << 20

// hookSink plays the cloud for a sandbox run: it answers the handshakes
// that the manifest declares and records each request that the daemon
// would get.
type hookSink struct {
	spec   manifest.Webhook
	secret string
	url    string
	srv    *http.Server

	mu   sync.Mutex
	reqs []protocol.WebhookRequest
}

func newHookSink(spec manifest.Webhook) (*hookSink, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("hook listener: %w", err)
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	h := &hookSink{spec: spec, secret: base64.RawURLEncoding.EncodeToString(b), url: "http://" + ln.Addr().String() + "/in/conformance"}
	h.srv = &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = h.srv.Serve(ln) }() // ends on close; the error is http.ErrServerClosed
	return h, nil
}

func (h *hookSink) Hook() protocol.Webhook { return protocol.Webhook{URL: h.url, Secret: h.secret} }

func (h *hookSink) Deliveries() []protocol.WebhookRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]protocol.WebhookRequest(nil), h.reqs...)
}

func (h *hookSink) close() { _ = h.srv.Close() } // the run is over; an error changes nothing

func (h *hookSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, hookMaxBody+1))
	if err != nil || len(body) > hookMaxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	ans := h.spec.Answer(manifest.HandshakeRequest{Method: r.Method, Query: r.URL.Query(), Headers: r.Header, Body: body})
	if ans != nil {
		for k, v := range ans.Headers {
			w.Header().Set(k, v)
		}
		if ans.ContentType != "" {
			w.Header().Set("Content-Type", ans.ContentType)
		}
		w.WriteHeader(ans.Status)
		_, _ = io.WriteString(w, ans.Body) // the platform reads it or not
		if !ans.Forward {
			return
		}
	}
	headers := map[string]string{}
	for _, name := range h.spec.Headers {
		if v := r.Header.Get(name); v != "" {
			headers[strings.ToLower(name)] = v
		}
	}
	h.mu.Lock()
	h.reqs = append(h.reqs, protocol.WebhookRequest{
		ID: fmt.Sprintf("conformance-%d", len(h.reqs)+1), Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		ContentType: r.Header.Get("Content-Type"), Headers: headers, Body: string(body),
		ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano), Handshake: ans != nil,
	})
	h.mu.Unlock()
	if ans == nil {
		w.WriteHeader(http.StatusOK)
	}
}
