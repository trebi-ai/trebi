package connectors

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
)

var (
	// ErrNoSubscriptions marks a connector whose manifest has no
	// events.subscriptions.
	ErrNoSubscriptions = errors.New("connector has no subscriptions")
	// ErrSubscriptionNotFound marks an unknown subscription id.
	ErrSubscriptionNotFound = errors.New("subscription not found")
)

const (
	// syncEvery is the interval of the sync that renews platform hooks.
	syncEvery = 6 * time.Hour
	// syncThrottle bounds the syncs that subscriptions/changed asks for.
	syncThrottle = 10 * time.Second
	// deleteSyncWait bounds the sync with an empty list before a delete.
	deleteSyncWait = 15 * time.Second
)

// Webhook states and reasons of a connection.
const (
	WebhookReady       = "ready"
	WebhookUnavailable = "unavailable"
	WebhookNotNeeded   = "not_needed"

	WebhookNotLinked  = "not_linked"
	WebhookNoPlan     = "no_plan"
	WebhookMintFailed = "mint_failed"
)

// HookProvider gives a connection its hosted hook.
type HookProvider interface {
	// Hook returns the stored hook of the connection, or nil.
	Hook(id string) *protocol.Webhook
	// HookState tells why a connection has no hook.
	HookState(id string) WebhookState
	// EnsureHooks mints the missing hooks and revokes the others in the
	// background. It calls Service.HooksChanged when a row changes.
	EnsureHooks(ctx context.Context, want map[string]manifest.Webhook)
}

// WebhookState tells whether a connection has a hosted hook.
type WebhookState struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// SubscriptionPatch changes the present fields of one subscription.
type SubscriptionPatch struct {
	Values *map[string]Value `json:"values,omitempty"`
	Types  *[]string         `json:"types,omitempty"`
}

// SubscriptionView is the list of one connection with the last states of
// its adapter.
type SubscriptionView struct {
	ID            string            `json:"id"`
	Action        string            `json:"action"`
	Webhook       WebhookState      `json:"webhook"`
	Subscriptions []SubscriptionRow `json:"subscriptions"`
	SyncedAt      string            `json:"synced_at,omitempty"`
}

// SubscriptionRow is one record subscription with its state.
type SubscriptionRow struct {
	ID     string           `json:"id"`
	Values map[string]Value `json:"values"`
	Types  []string         `json:"types,omitempty"`
	protocol.SubscriptionState
}

// WithHooks sets the provider of hosted hooks.
func (s *Service) WithHooks(h HookProvider) *Service {
	s.hooks = h
	return s
}

// HooksChanged runs reconcile, so that a new or revoked hook restarts its
// adapter.
func (s *Service) HooksChanged(ctx context.Context) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.reconcile(ctx)
}

// hookWants returns the hook spec of each connection that needs a hook: its
// manifest declares a webhook and its record has a subscription.
func hookWants(rs []Resolved, conns map[string]Connection) map[string]manifest.Webhook {
	want := map[string]manifest.Webhook{}
	for _, r := range rs {
		spec := r.Manifest.SubscriptionSpec()
		if spec == nil || spec.Webhook == nil || len(conns[r.ID].Subscriptions) == 0 {
			continue
		}
		want[r.ID] = *spec.Webhook
	}
	return want
}

// spec returns the subscription spec of a connection, or ErrNoSubscriptions.
func (s *Service) spec(ref string) (Resolved, *manifest.Subscriptions, error) {
	r, err := s.Resolve(ref, "")
	if err != nil {
		return Resolved{}, nil, err
	}
	spec := r.Manifest.SubscriptionSpec()
	if spec == nil {
		return r, nil, fmt.Errorf("%w: connector %s", ErrNoSubscriptions, r.Slug)
	}
	return r, spec, nil
}

// SubscriptionOptions asks the adapter for the options of a dynamic field.
func (s *Service) SubscriptionOptions(ctx context.Context, ref string, p protocol.OptionsParams) (protocol.OptionPage, error) {
	r, spec, err := s.spec(ref)
	if err != nil {
		return protocol.OptionPage{}, err
	}
	f, ok := spec.Field(p.Field)
	if !ok || !f.Dynamic {
		return protocol.OptionPage{}, &ValidationError{Name: r.Slug, Fields: []FieldError{{Field: "field", Message: "must name a dynamic field"}}}
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	var page protocol.OptionPage
	if err := s.Call(ctx, r.ID, protocol.MethodSubscriptionsOptions, p, &page); err != nil {
		return protocol.OptionPage{}, err
	}
	if page.Options == nil {
		page.Options = []protocol.FieldOption{}
	}
	return page, nil
}

// Subscriptions returns the view of one connection.
func (s *Service) Subscriptions(_ context.Context, ref string) (SubscriptionView, error) {
	r, spec, err := s.spec(ref)
	if err != nil {
		return SubscriptionView{}, err
	}
	c, err := s.Lookup(r.ID)
	if err != nil {
		return SubscriptionView{}, err
	}
	v := SubscriptionView{ID: c.ID, Action: spec.Action, Webhook: s.webhookState(c.ID, spec), Subscriptions: []SubscriptionRow{}}
	p := s.procByName(c.ID)
	var states map[string]protocol.SubscriptionState
	running := false
	if p != nil {
		var at time.Time
		states, at = p.subscriptions()
		conn, _ := p.session()
		running = conn != nil
		if !at.IsZero() {
			v.SyncedAt = at.UTC().Format(time.RFC3339)
		}
	}
	for _, sub := range c.Subscriptions {
		st, ok := states[sub.ID]
		if !ok {
			st = protocol.SubscriptionState{State: protocol.SubscriptionPending}
			if !running {
				st.Message = "The connection is not running"
			}
		}
		st.ID = ""
		values := sub.Values
		if values == nil {
			values = map[string]Value{}
		}
		v.Subscriptions = append(v.Subscriptions, SubscriptionRow{ID: sub.ID, Values: values, Types: sub.Types, SubscriptionState: st})
	}
	return v, nil
}

// webhookState tells whether the connection has its hook.
func (s *Service) webhookState(id string, spec *manifest.Subscriptions) WebhookState {
	switch {
	case spec.Webhook == nil:
		return WebhookState{State: WebhookNotNeeded}
	case s.hooks == nil:
		return WebhookState{State: WebhookUnavailable, Reason: WebhookNotLinked}
	case s.hooks.Hook(id) != nil:
		return WebhookState{State: WebhookReady}
	}
	return s.hooks.HookState(id)
}

// Subscribe adds one subscription to the record.
func (s *Service) Subscribe(ctx context.Context, ref string, sub Subscription) (SubscriptionView, error) {
	sub.ID = NewSubscriptionID()
	return s.editSubscriptions(ctx, ref, func(list []Subscription) ([]Subscription, error) {
		return append(list, sub), nil
	})
}

// UpdateSubscription changes the values or the types of one subscription.
func (s *Service) UpdateSubscription(ctx context.Context, ref, id string, p SubscriptionPatch) (SubscriptionView, error) {
	return s.editSubscriptions(ctx, ref, func(list []Subscription) ([]Subscription, error) {
		i := slices.IndexFunc(list, func(sub Subscription) bool { return sub.ID == id })
		if i < 0 {
			return nil, fmt.Errorf("%w: %s", ErrSubscriptionNotFound, id)
		}
		if p.Values != nil {
			list[i].Values = maps.Clone(*p.Values)
		}
		if p.Types != nil {
			list[i].Types = slices.Clone(*p.Types)
		}
		return list, nil
	})
}

// Unsubscribe removes one subscription from the record.
func (s *Service) Unsubscribe(ctx context.Context, ref, id string) (SubscriptionView, error) {
	return s.editSubscriptions(ctx, ref, func(list []Subscription) ([]Subscription, error) {
		i := slices.IndexFunc(list, func(sub Subscription) bool { return sub.ID == id })
		if i < 0 {
			return nil, fmt.Errorf("%w: %s", ErrSubscriptionNotFound, id)
		}
		return slices.Delete(list, i, i+1), nil
	})
}

// editSubscriptions changes the list through the normal commit.
func (s *Service) editSubscriptions(ctx context.Context, ref string, edit func([]Subscription) ([]Subscription, error)) (SubscriptionView, error) {
	if _, _, err := s.spec(ref); err != nil {
		return SubscriptionView{}, err
	}
	if err := s.GuardMutate(ref); err != nil {
		return SubscriptionView{}, err
	}
	s.writeMu.Lock()
	old, err := s.Lookup(ref)
	if err == nil {
		err = s.checkLoaded(old.ID)
	}
	if err != nil {
		s.writeMu.Unlock()
		return SubscriptionView{}, err
	}
	c := old.clone()
	if c.Subscriptions, err = edit(c.Subscriptions); err == nil {
		err = s.commit(ctx, c, &old, nil)
	}
	s.writeMu.Unlock()
	if err != nil {
		return SubscriptionView{}, err
	}
	return s.Subscriptions(ctx, c.ID)
}

// SubmitSubscription sends the answers of a person to the adapter and keeps
// the returned state on the process. The record does not change.
func (s *Service) SubmitSubscription(ctx context.Context, ref, id string, fields map[string]string) (SubscriptionView, error) {
	r, _, err := s.spec(ref)
	if err != nil {
		return SubscriptionView{}, err
	}
	c, err := s.Lookup(r.ID)
	if err != nil {
		return SubscriptionView{}, err
	}
	if !slices.ContainsFunc(c.Subscriptions, func(sub Subscription) bool { return sub.ID == id }) {
		return SubscriptionView{}, fmt.Errorf("%w: %s", ErrSubscriptionNotFound, id)
	}
	var st protocol.SubscriptionState
	if err := s.Call(ctx, r.ID, protocol.MethodSubscriptionsSubmit, protocol.SubmitParams{ID: id, Fields: fields}, &st); err != nil {
		return SubscriptionView{}, err
	}
	if p := s.procByName(r.ID); p != nil {
		st.ID = id
		p.setSubscription(st)
		s.emitSynced(p)
	}
	return s.Subscriptions(ctx, r.ID)
}

// SyncSubscriptions makes the platform match the record now.
func (s *Service) SyncSubscriptions(ctx context.Context, ref string) (SubscriptionView, error) {
	r, _, err := s.spec(ref)
	if err != nil {
		return SubscriptionView{}, err
	}
	p := s.procByName(r.ID)
	if p == nil {
		return SubscriptionView{}, protocol.NewError(protocol.CodeTransient, fmt.Sprintf("connector %s is not running", r.Slug))
	}
	if err := s.syncSubscriptions(ctx, p); err != nil {
		return SubscriptionView{}, err
	}
	return s.Subscriptions(ctx, r.ID)
}

// syncSubscriptionsAsync asks the sync loop of a connection for a sync.
func (s *Service) syncSubscriptionsAsync(id string) {
	if p := s.procByName(id); p != nil {
		p.kick(p.syncNow)
	}
}

// syncSubscriptions sends the record list to the adapter and keeps the
// states. A connector without the feature is not an error.
func (s *Service) syncSubscriptions(ctx context.Context, p *proc) error {
	spec := p.res.Manifest.SubscriptionSpec()
	if spec == nil {
		return nil
	}
	c, err := s.Lookup(p.id)
	if err != nil {
		return err
	}
	params := protocol.SyncParams{Subscriptions: []protocol.Subscription{}}
	for _, sub := range c.Subscriptions {
		params.Subscriptions = append(params.Subscriptions, sub.Wire(spec))
	}
	p.syncMu.Lock()
	defer p.syncMu.Unlock()
	var res protocol.SyncResult
	if err := s.Call(ctx, p.id, protocol.MethodSubscriptionsSync, params, &res); err != nil {
		s.log.Warn("connector.subscriptions.synced", "connector", p.id, "error", err)
		return err
	}
	p.setSubscriptions(res.Subscriptions, s.now())
	s.emitSynced(p)
	return nil
}

// emitSynced appends connector.subscriptions.synced with the counts.
func (s *Service) emitSynced(p *proc) {
	states, _ := p.subscriptions()
	counts := map[string]int{}
	var action, failure string
	for _, st := range states {
		counts[st.State]++
		switch {
		case st.State == protocol.SubscriptionActionRequired && st.Action != nil && action == "":
			action = st.Action.Text
		case st.State == protocol.SubscriptionError && failure == "":
			failure = cmp.Or(st.Message, st.Title)
		}
	}
	s.emitConnector("connector.subscriptions.synced", p.id, map[string]string{
		"active":          strconv.Itoa(counts[protocol.SubscriptionActive]),
		"polling":         strconv.Itoa(counts[protocol.SubscriptionPolling]),
		"action_required": strconv.Itoa(counts[protocol.SubscriptionActionRequired]),
		"errors":          strconv.Itoa(counts[protocol.SubscriptionError]),
		"action":          action,
		"error":           failure,
	})
}

// runSyncLoop syncs after initialized, on a kick, every syncEvery, and at
// most each syncThrottle on subscriptions/changed. It ends with the session.
func (s *Service) runSyncLoop(ctx context.Context, p *proc, conn *protocol.Conn) {
	tick := time.NewTicker(syncEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			return
		case <-p.syncNow:
		case <-tick.C:
		case <-p.syncChanged:
			_, at := p.subscriptions()
			if s.sleep(ctx, syncThrottle-s.now().Sub(at)) {
				return
			}
		}
		_ = s.syncSubscriptions(ctx, p) //nolint:errcheck // syncSubscriptions logs the failure
	}
}

// unsyncBeforeDelete asks the adapter to delete every platform hook. A
// failure does not block the delete.
func (s *Service) unsyncBeforeDelete(ctx context.Context, id string) {
	p := s.procByName(id)
	if p == nil || p.res.Manifest.SubscriptionSpec() == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, deleteSyncWait)
	defer cancel()
	empty := protocol.SyncParams{Subscriptions: []protocol.Subscription{}}
	if err := s.Call(ctx, id, protocol.MethodSubscriptionsSync, empty, &protocol.SyncResult{}); err != nil {
		s.log.Warn("connector.subscriptions.synced", "connector", id, "error", err)
	}
}

// kick sends to a channel of capacity one without a wait.
func (p *proc) kick(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (p *proc) subscriptions() (map[string]protocol.SubscriptionState, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.subs), p.syncedAt
}

func (p *proc) setSubscriptions(states []protocol.SubscriptionState, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subs = make(map[string]protocol.SubscriptionState, len(states))
	for _, st := range states {
		p.subs[st.ID] = st
	}
	p.syncedAt = at
}

func (p *proc) setSubscription(st protocol.SubscriptionState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.subs == nil {
		p.subs = map[string]protocol.SubscriptionState{}
	}
	p.subs[st.ID] = st
}
