package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/conformance"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/oauth"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
)

const (
	// loginWait bounds auth/begin; the flow itself has its own expires_at.
	loginWait = 30 * time.Second
	// flowKeep is how long a finished flow stays readable.
	flowKeep = 10 * time.Minute
)

// Call sends one request to the live session of an instance. An instance
// with no live session is a transient error, so the outbox retries. A method
// whose feature the adapter did not deliver is unsupported.
func (s *Service) Call(ctx context.Context, ref, method string, params, result any) error {
	instance := s.IDOf(ref)
	p := s.procByName(instance)
	if p == nil {
		return protocol.NewError(protocol.CodeTransient, fmt.Sprintf("connector %s is not running", instance))
	}
	conn, init := p.session()
	if conn == nil || init == nil {
		return protocol.NewError(protocol.CodeTransient, fmt.Sprintf("connector %s is not connected", instance))
	}
	if f := protocol.FeatureOf(method); f != "" && !slices.Contains(init.Features, f) {
		return protocol.NewError(protocol.CodeUnsupported, fmt.Sprintf("connector %s does not support %s", instance, f))
	}
	return conn.Call(ctx, method, params, result)
}

// Features returns the features the live session delivers, else the ones
// the manifest promises.
func (s *Service) Features(ref string) []string {
	instance := s.IDOf(ref)
	if p := s.procByName(instance); p != nil {
		if _, init := p.session(); init != nil {
			return slices.Clone(init.Features)
		}
	}
	r, err := s.Resolve(instance, "")
	if err != nil {
		return nil
	}
	return r.Manifest.Features()
}

// Limits returns the message limits of an instance: delivered, else
// promised.
func (s *Service) Limits(ref string) manifest.Limits {
	instance := s.IDOf(ref)
	r, err := s.Resolve(instance, "")
	if err != nil {
		return manifest.Limits{}
	}
	if p := s.procByName(instance); p != nil {
		return p.snapshot().limits(r.Manifest)
	}
	if r.Manifest.Channel != nil {
		return r.Manifest.Channel.Limits
	}
	return manifest.Limits{}
}

// AuthState returns the last status of an instance. The state is empty for
// an instance that does not run.
func (s *Service) AuthState(ref string) protocol.Status {
	if p := s.procByName(s.IDOf(ref)); p != nil {
		return p.authStatus()
	}
	return protocol.Status{}
}

// Down reports whether a running instance cannot send: it waits for a login
// or does not answer ping. An instance that does not run is not down; the
// outbox retries it.
func (s *Service) Down(ref string) bool {
	p := s.procByName(s.IDOf(ref))
	if p == nil {
		return false
	}
	snap := p.snapshot()
	return snap.auth.State == protocol.StateAuthRequired || snap.status == statusUnresponsive
}

// onNotify handles one adapter notification. It runs in the read loop of
// the session, so it must never call the adapter.
func (s *Service) onNotify(p *proc, method string, params json.RawMessage) {
	ctx := context.Background()
	switch method {
	case protocol.MethodEvent:
		var ev protocol.Event
		if err := json.Unmarshal(params, &ev); err != nil {
			p.addParseError()
			s.log.Warn("connector.parse_error", "connector", p.id, "reason", "bad_event", "error", err)
			return
		}
		s.ingestProtocol(ctx, p, ev, false)
	case protocol.MethodStatus:
		var st protocol.Status
		if err := json.Unmarshal(params, &st); err == nil && st.State != "" {
			s.applyStatus(p, st)
		}
	case protocol.MethodAuthStep:
		var st protocol.AuthStepParams
		if err := json.Unmarshal(params, &st); err == nil && st.FlowID != "" {
			s.flow(p.id, st.FlowID, true).setStep(st.Step, false)
		}
	case protocol.MethodAuthDone:
		var d protocol.AuthDoneParams
		if err := json.Unmarshal(params, &d); err != nil || d.FlowID == "" {
			return
		}
		s.flow(p.id, d.FlowID, true).finish(d)
		if d.OK {
			s.emitConnector("connector.auth.completed", p.id, nil)
			p.kick(p.syncNow) // a login can change the mode
		}
	case protocol.NoteSubscriptionsChanged:
		p.kick(p.syncChanged)
	default:
		s.log.Debug("connector notification", "connector", p.id, "method", method)
	}
}

// applyStatus stores an adapter status and emits connector.status on a
// change. auth_required, or an account that needs a login, also emits
// connector.auth.required. A change of the links emits connector.updated.
// connected after a status outage emits connector.recovered.
func (s *Service) applyStatus(p *proc, st protocol.Status) {
	if st.Links != nil {
		st.Links = s.localLinks(p.id, st.Links)
	}
	changed, linksChanged := p.setAuth(st)
	if linksChanged {
		s.emitConnector("connector.updated", p.id, nil)
	}
	if p.trackOutage(st.State, s.now()) {
		s.emitRecovered(p.id)
	}
	if !changed {
		return
	}
	s.emitStatus(p, st.State, st.Reason)
	if out := st.LoggedOut(); st.State == protocol.StateAuthRequired || len(out) > 0 {
		s.emitConnector("connector.auth.required", p.id, map[string]string{"reason": st.Reason, "message": st.Message, "accounts": strings.Join(out, ", ")})
	}
}

// maxLinks bounds the links of one status.
const maxLinks = 8

// localLinks keeps the links that point to this computer or to the tailnet.
// It drops the others with a log line. The result is never nil, so an empty
// list clears the stored links.
func (s *Service) localLinks(id string, links []protocol.Link) []protocol.Link {
	kept := []protocol.Link{}
	for _, l := range links {
		if l.Label == "" || !localURL(l.URL) || len(kept) == maxLinks {
			s.log.Warn("connector.link_dropped", "connector", id, "label", l.Label, "url", l.URL)
			continue
		}
		kept = append(kept, l)
	}
	return kept
}

// localURL reports whether raw is an http or https URL on 127.0.0.1,
// localhost, a Tailscale address (100.64.0.0/10), or a *.ts.net name.
func localURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".ts.net") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	tailnet := netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, 0, 0}), 10)
	return err == nil && (ip == netip.AddrFrom4([4]byte{127, 0, 0, 1}) || tailnet.Contains(ip))
}

// Flow is one login flow. Steps go to subscribers only: never to the bus
// and never to a log.
type Flow struct {
	ID       string
	Instance string

	mu      sync.Mutex
	method  protocol.LoginMethod
	key     *manifest.LoginMethod // set for a daemon key flow
	oauth   *oauth.Flow           // set for a daemon OAuth flow
	step    *protocol.Step
	done    *protocol.AuthDoneParams
	subs    []chan FlowUpdate
	updated time.Time
}

// FlowUpdate is one frame of a flow: a step, or the end.
type FlowUpdate struct {
	FlowID string                   `json:"flow_id"`
	Step   *protocol.Step           `json:"step,omitempty"`
	Done   *protocol.AuthDoneParams `json:"done,omitempty"`
}

// Current returns the latest frame.
func (f *Flow) Current() FlowUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return FlowUpdate{FlowID: f.ID, Step: f.step, Done: f.done}
}

// Subscribe returns a channel that gets the current frame first, then each
// new one. It closes after the done frame or when ctx ends.
func (f *Flow) Subscribe(ctx context.Context) <-chan FlowUpdate {
	ch := make(chan FlowUpdate, 8)
	f.mu.Lock()
	cur := FlowUpdate{FlowID: f.ID, Step: f.step, Done: f.done}
	if f.done != nil {
		f.mu.Unlock()
		ch <- cur
		close(ch)
		return ch
	}
	if f.step != nil {
		ch <- cur
	}
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	go func() { // ends when ctx ends or the flow finishes
		<-ctx.Done()
		f.unsubscribe(ch)
	}()
	return ch
}

func (f *Flow) unsubscribe(ch chan FlowUpdate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.subs {
		if c == ch {
			f.subs = slices.Delete(f.subs, i, i+1)
			close(ch)
			return
		}
	}
}

// Method returns the login method of the flow.
func (f *Flow) Method() protocol.LoginMethod {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.method
}

// setMethod stores the method of the flow. key is set for a daemon flow.
func (f *Flow) setMethod(m protocol.LoginMethod, key *manifest.LoginMethod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.method, f.key = m, key
}

func (f *Flow) keyMethod() *manifest.LoginMethod {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.key
}

func (f *Flow) setOAuth(of *oauth.Flow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oauth = of
}

func (f *Flow) oauthFlow() *oauth.Flow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.oauth
}

// setStep stores a step and sends it. With first set, it keeps a step that
// a notification already delivered.
func (f *Flow) setStep(step protocol.Step, first bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done != nil || (first && f.step != nil) {
		return
	}
	f.step = &step
	f.updated = time.Now()
	f.send(FlowUpdate{FlowID: f.ID, Step: f.step})
}

// finish stores the end of the flow, sends it, and closes the subscribers.
func (f *Flow) finish(d protocol.AuthDoneParams) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done != nil {
		return
	}
	f.done = &d
	f.updated = time.Now()
	f.send(FlowUpdate{FlowID: f.ID, Done: f.done})
	for _, c := range f.subs {
		close(c)
	}
	f.subs = nil
}

// send drops a frame for a subscriber that does not read; the next frame or
// Current catches it up. The caller holds f.mu.
func (f *Flow) send(u FlowUpdate) {
	for _, c := range f.subs {
		select {
		case c <- u:
		default:
		}
	}
}

// flow returns the flow of an instance, and with create makes a missing one.
// A notification can reach the daemon before the auth/begin answer, so a
// flow is made on first sight.
func (s *Service) flow(instance, id string, create bool) *Flow {
	key := instance + "\x00" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[key]
	if !ok && create {
		f = &Flow{ID: id, Instance: instance, updated: time.Now()}
		s.flows[key] = f
	}
	return f
}

// FindFlow returns a flow of a connection by flow id.
func (s *Service) FindFlow(ref, id string) (*Flow, bool) {
	instance := s.IDOf(ref)
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[instance+"\x00"+id]
	return f, ok
}

// pruneFlows ends expired OAuth flows and drops finished flows after
// flowKeep.
func (s *Service) pruneFlows() {
	s.pruneOAuth()
	cut := time.Now().Add(-flowKeep)
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, f := range s.flows {
		f.mu.Lock()
		old := f.done != nil && f.updated.Before(cut)
		f.mu.Unlock()
		if old {
			delete(s.flows, key)
		}
	}
}

// Logout ends the session of an instance. An OAuth login revokes its
// tokens.
func (s *Service) Logout(ctx context.Context, instance string) error {
	if r, err := s.Resolve(instance, ""); err == nil {
		if _, ok := s.oauthMethod(r); ok {
			return s.logoutOAuth(ctx, r.ID)
		}
	}
	return s.Call(ctx, instance, protocol.MethodAuthLogout, protocol.Empty{}, nil)
}

// Conformance runs the conformance suites against a live connection. With
// suites, it runs the named suites only. It does not shut the adapter down,
// it starts a login only when the connection waits for one, and it sends
// only to the room that the operator names.
func (s *Service) Conformance(ctx context.Context, ref, room string, suites []string) ([]conformance.Result, error) {
	r, err := s.Resolve(ref, "")
	if err != nil {
		return nil, err
	}
	inputs, _ := r.Env()
	t := conformance.Target{Manifest: r.Manifest, Dir: r.Dir, Inputs: inputs, Room: room}
	if r.Manifest.Protocol() == manifest.ProtocolConnector {
		p := s.procByName(r.ID)
		if p == nil {
			return nil, protocol.NewError(protocol.CodeTransient, fmt.Sprintf("connector %s is not running", r.Slug))
		}
		if _, init := p.session(); init == nil {
			return nil, protocol.NewError(protocol.CodeTransient, fmt.Sprintf("connector %s is not connected", r.Slug))
		}
		t.Live = liveSession{s: s, p: p, id: r.ID}
	}
	return conformance.NewRunner(s.log).Run(ctx, t, suites), nil
}

// liveSession is the running session of one connection for the
// conformance runner.
type liveSession struct {
	s  *Service
	p  *proc
	id string
}

func (l liveSession) Call(ctx context.Context, method string, params, result any) error {
	return l.s.Call(ctx, l.id, method, params, result)
}

func (l liveSession) Init() protocol.InitializeResult {
	if _, init := l.p.session(); init != nil {
		return *init
	}
	return protocol.InitializeResult{}
}
