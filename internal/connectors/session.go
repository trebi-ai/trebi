package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
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
func (s *Service) Call(ctx context.Context, instance, method string, params, result any) error {
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
func (s *Service) Features(instance string) []string {
	if p := s.procByName(instance); p != nil {
		if _, init := p.session(); init != nil {
			return slices.Clone(init.Features)
		}
	}
	r, err := s.resolveInstance(instance)
	if err != nil {
		return nil
	}
	return r.Manifest.Features()
}

// Limits returns the message limits of an instance: delivered, else
// promised.
func (s *Service) Limits(instance string) manifest.Limits {
	r, err := s.resolveInstance(instance)
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
func (s *Service) AuthState(instance string) protocol.Status {
	if p := s.procByName(instance); p != nil {
		return p.authStatus()
	}
	return protocol.Status{}
}

// Down reports whether a running instance cannot send: it waits for a login
// or does not answer ping. An instance that does not run is not down; the
// outbox retries it.
func (s *Service) Down(instance string) bool {
	p := s.procByName(instance)
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
			s.log.Warn("connector.parse_error", "connector", p.instance, "reason", "bad_event", "error", err)
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
			s.flow(p.instance, st.FlowID, true).setStep(st.Step, false)
		}
	case protocol.MethodAuthDone:
		var d protocol.AuthDoneParams
		if err := json.Unmarshal(params, &d); err != nil || d.FlowID == "" {
			return
		}
		s.flow(p.instance, d.FlowID, true).finish(d)
		if d.OK {
			s.emitConnector("connector.auth.completed", p.instance, nil)
		}
	default:
		s.log.Debug("connector notification", "connector", p.instance, "method", method)
	}
}

// applyStatus stores an adapter status and emits connector.status on a
// change. auth_required also emits connector.auth.required.
func (s *Service) applyStatus(p *proc, st protocol.Status) {
	if !p.setAuth(st) {
		return
	}
	s.emitStatus(p, st.State, st.Reason)
	if st.State == protocol.StateAuthRequired {
		s.emitConnector("connector.auth.required", p.instance, map[string]string{"reason": st.Reason})
	}
}

// Flow is one login flow. Steps go to subscribers only: never to the bus
// and never to a log.
type Flow struct {
	ID       string
	Instance string

	mu      sync.Mutex
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

// FindFlow returns a flow of a connector by id, for any of its instances.
func (s *Service) FindFlow(name, id string) (*Flow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, f := range s.flows {
		inst, fid, _ := strings.Cut(key, "\x00")
		if n, _ := splitInstance(inst); n == name && fid == id {
			return f, true
		}
	}
	return nil, false
}

// pruneFlows drops finished flows after flowKeep.
func (s *Service) pruneFlows() {
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

// Login starts a login flow on an instance and returns it with its first
// step.
func (s *Service) Login(ctx context.Context, instance, kind string) (*Flow, error) {
	s.pruneFlows()
	ctx, cancel := context.WithTimeout(ctx, loginWait)
	defer cancel()
	var res protocol.AuthBeginResult
	if err := s.Call(ctx, instance, protocol.MethodAuthBegin, protocol.AuthBeginParams{Kind: kind}, &res); err != nil {
		return nil, err
	}
	if res.FlowID == "" {
		return nil, protocol.NewError(protocol.CodeInvalid, "auth/begin returned no flow_id")
	}
	f := s.flow(instance, res.FlowID, true)
	f.setStep(res.Step, true)
	return f, nil
}

// SubmitLogin answers an input step.
func (s *Service) SubmitLogin(ctx context.Context, f *Flow, fields map[string]string) error {
	var res protocol.AuthSubmitResult
	if err := s.Call(ctx, f.Instance, protocol.MethodAuthSubmit, protocol.AuthSubmitParams{FlowID: f.ID, Fields: fields}, &res); err != nil {
		return err
	}
	if res.Step != nil {
		f.setStep(*res.Step, false)
	}
	return nil
}

// CancelLogin ends a flow.
func (s *Service) CancelLogin(ctx context.Context, f *Flow) error {
	err := s.Call(ctx, f.Instance, protocol.MethodAuthCancel, protocol.AuthFlowParams{FlowID: f.ID}, nil)
	f.finish(protocol.AuthDoneParams{FlowID: f.ID, Error: "cancelled"})
	return err
}

// Logout ends the session of an instance.
func (s *Service) Logout(ctx context.Context, instance string) error {
	return s.Call(ctx, instance, protocol.MethodAuthLogout, protocol.Empty{}, nil)
}

// Conformance runs the checks of Part H against the live session of an
// instance. It does not shut the adapter down. It starts a login only when
// the instance waits for one, and it sends only to the room the operator
// names.
func (s *Service) Conformance(ctx context.Context, instance, room string) ([]protocol.CheckResult, error) {
	r, err := s.resolveInstance(instance)
	if err != nil {
		return nil, err
	}
	m := r.Manifest
	if m.Protocol() != manifest.ProtocolConnector {
		return nil, fmt.Errorf("%w: connector %s does not speak %s", ErrInvalid, instance, protocol.Version)
	}
	p := s.procByName(instance)
	if p == nil {
		return nil, protocol.NewError(protocol.CodeTransient, fmt.Sprintf("connector %s is not running", instance))
	}
	conn, init := p.session()
	if conn == nil || init == nil {
		return nil, protocol.NewError(protocol.CodeTransient, fmt.Sprintf("connector %s is not connected", instance))
	}
	c := &protocol.Checker{
		Features:  m.Features(),
		Room:      room,
		Init:      init,
		SkipLogin: p.authStatus().State != protocol.StateAuthRequired,
	}
	if m.Setup != nil {
		c.Login = m.Setup.Login
	}
	for _, t := range m.Events.Types {
		c.EventTypes = append(c.EventTypes, t.Type)
	}
	if r.Dir != "" {
		if c.Schemas, err = m.EventSchemas(r.Dir); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	return c.Run(ctx, conn), nil
}
