package connectors_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/connectors/protocol/fakeadapter"
	"github.com/trebi-ai/trebi/internal/store"
)

// fakeHooks is the hook provider of a test.
type fakeHooks struct {
	mu    sync.Mutex
	hook  *protocol.Webhook
	wants []map[string]manifest.Webhook
}

func (f *fakeHooks) Hook(string) *protocol.Webhook {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hook
}

func (f *fakeHooks) HookState(string) connectors.WebhookState {
	return connectors.WebhookState{State: connectors.WebhookUnavailable, Reason: connectors.WebhookNotLinked}
}

func (f *fakeHooks) EnsureHooks(_ context.Context, want map[string]manifest.Webhook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wants = append(f.wants, want)
}

func (f *fakeHooks) set(h *protocol.Webhook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = h
}

func (f *fakeHooks) wanted(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.wants) == 0 {
		return false
	}
	_, ok := f.wants[len(f.wants)-1][id]
	return ok
}

// watchStack starts one sandbox connection "watch" with a subscription
// form of one dynamic room field.
func watchStack(t *testing.T, hook *protocol.Webhook, subs []connectors.Subscription, faults ...string) (*store.Store, *connectors.Service, *fakeHooks, connectors.Connection) {
	t.Helper()
	st, _, svc := openStack(t)
	hooks := &fakeHooks{hook: hook}
	svc.WithHooks(hooks)
	sb := &fakeadapter.Sandbox{
		Faults:  faults,
		Result:  protocol.InitializeResult{Features: []string{manifest.FeatureSubscriptions, manifest.FeatureWebhooks}},
		Options: map[string][]protocol.FieldOption{"target": {{Value: "a", Label: "A"}, {Value: "b"}}},
	}
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := sb.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	e := inline("watch", func(m *manifest.Manifest) {
		m.Events = &manifest.Events{
			Protocol: manifest.ProtocolConnector, Command: `"` + os.Args[0] + `"`,
			Types: []manifest.EventType{{Type: "message"}},
			Subscriptions: &manifest.Subscriptions{
				Action: "Watch a target",
				Modes:  []string{manifest.ModeAPI, manifest.ModeManual, manifest.ModePoll},
				Fields: []manifest.SubscriptionField{{Name: "target", Label: "Target", Dynamic: true, Room: true}},
				Webhook: &manifest.Webhook{
					Headers: []string{strings.ToLower(fakeadapter.SignatureHeader)},
				},
			},
		}
	})
	e.Inputs = map[string]string{fakeadapter.Env: "sandbox:" + path}
	e.Subscriptions = subs
	c, err := svc.Create(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	svc.StartAll(ctx)
	t.Cleanup(func() {
		cancel()
		svc.StopAll()
		_ = svc.Wait(context.Background())
	})
	return st, svc, hooks, c
}

// row waits for the first subscription row that passes ok.
func waitRow(t *testing.T, svc *connectors.Service, msg string, ok func(connectors.SubscriptionRow) bool) connectors.SubscriptionRow {
	t.Helper()
	var got connectors.SubscriptionRow
	waitUntil(t, func() bool {
		v, err := svc.Subscriptions(context.Background(), "watch")
		if err != nil || len(v.Subscriptions) == 0 {
			return false
		}
		got = v.Subscriptions[0]
		return ok(got)
	}, msg)
	return got
}

func pidOf(svc *connectors.Service) any {
	row, _ := svc.Get("watch")
	return row["pid"]
}

// TestSubscriptionsRecordAndSync proves the record list, the form rules,
// and a sync without a restart after a list change.
func TestSubscriptionsRecordAndSync(t *testing.T) {
	_, svc, hooks, c := watchStack(t, nil, nil)
	ctx := context.Background()
	waitStatus(t, svc, "watch", "listening")

	page, err := svc.SubscriptionOptions(ctx, "watch", protocol.OptionsParams{Field: "target"})
	if err != nil || len(page.Options) != 2 || page.Options[0].Value != "a" {
		t.Fatalf("options = %+v, %v", page, err)
	}
	if _, err := svc.SubscriptionOptions(ctx, "watch", protocol.OptionsParams{Field: "nope"}); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("options of an unknown field: %v", err)
	}

	v, err := svc.Subscribe(ctx, "watch", connectors.Subscription{Values: map[string]connectors.Value{"target": {"a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Action != "Watch a target" || len(v.Subscriptions) != 1 || !strings.HasPrefix(v.Subscriptions[0].ID, "sub_") {
		t.Fatalf("view = %+v", v)
	}
	if v.Webhook.State != connectors.WebhookUnavailable || v.Webhook.Reason != connectors.WebhookNotLinked {
		t.Fatalf("webhook = %+v", v.Webhook)
	}
	id := v.Subscriptions[0].ID
	got := waitRow(t, svc, "no polling state", func(r connectors.SubscriptionRow) bool { return r.State == protocol.SubscriptionPolling })
	if got.Title != "a" || got.Mode != manifest.ModePoll || got.Room == nil || got.Room.ID != "a" {
		t.Fatalf("row = %+v", got)
	}
	if !hooks.wanted(c.ID) {
		t.Fatal("EnsureHooks did not get the connection")
	}
	raw, err := os.ReadFile(filepath.Join(connectors.NewFolder(c.ID).Dir(), "connection.yaml"))
	if err != nil || !strings.Contains(string(raw), "target: a") {
		t.Fatalf("connection.yaml = %s, %v", raw, err)
	}

	pid := pidOf(svc)
	if _, err := svc.Subscribe(ctx, "watch", connectors.Subscription{Values: map[string]connectors.Value{"target": {"a"}}}); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("a duplicate subscription: %v", err)
	}
	if _, err := svc.Subscribe(ctx, "watch", connectors.Subscription{Values: map[string]connectors.Value{"nope": {"x"}}}); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("an unknown field: %v", err)
	}
	if _, err := svc.Subscribe(ctx, "watch", connectors.Subscription{Types: []string{"nope"}}); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("an unknown type: %v", err)
	}
	target := map[string]connectors.Value{"target": {"b"}}
	if _, err := svc.UpdateSubscription(ctx, "watch", id, connectors.SubscriptionPatch{Values: &target}); err != nil {
		t.Fatal(err)
	}
	waitRow(t, svc, "no sync after the change", func(r connectors.SubscriptionRow) bool { return r.Title == "b" })
	if pidOf(svc) != pid {
		t.Fatal("a list change restarted the adapter")
	}
	if _, err := svc.UpdateSubscription(ctx, "watch", "sub_nope", connectors.SubscriptionPatch{}); !errors.Is(err, connectors.ErrSubscriptionNotFound) {
		t.Fatalf("an unknown id: %v", err)
	}

	v, err = svc.Unsubscribe(ctx, "watch", id)
	if err != nil || len(v.Subscriptions) != 0 {
		t.Fatalf("unsubscribe = %+v, %v", v, err)
	}
}

// TestSubscriptionsWebhook proves a new hook restarts the adapter with the
// hook, and that a signed delivery through the queue becomes an event. A
// manual subscription waits for a person, and submit does not change the
// record.
func TestSubscriptionsWebhook(t *testing.T) {
	st, svc, hooks, c := watchStack(t, nil, nil)
	ctx := context.Background()
	var n int
	var nmu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		headers := map[string]string{}
		for k := range r.Header {
			headers[strings.ToLower(k)] = r.Header.Get(k)
		}
		nmu.Lock()
		n++
		id := "dlv-" + strconv.Itoa(n)
		nmu.Unlock()
		req := protocol.WebhookRequest{ID: id, Method: r.Method, Path: r.URL.Path, Headers: headers, Body: string(body), ReceivedAt: time.Now().UTC().Format(time.RFC3339)}
		if err := svc.EnqueueWebhook(r.Context(), c.ID, req); err != nil && r.Context().Err() == nil { // a poster that stops cancels its delivery
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	waitStatus(t, svc, "watch", "listening")
	if _, err := svc.Subscribe(ctx, "watch", connectors.Subscription{Values: map[string]connectors.Value{"target": {"a"}}}); err != nil {
		t.Fatal(err)
	}
	waitRow(t, svc, "no polling state", func(r connectors.SubscriptionRow) bool { return r.State == protocol.SubscriptionPolling })

	pid := pidOf(svc)
	hooks.set(&protocol.Webhook{URL: srv.URL, Secret: "s3cret"})
	svc.HooksChanged(ctx)
	waitUntil(t, func() bool { p := pidOf(svc); return p != nil && p != pid }, "a new hook did not restart the adapter")
	got := waitRow(t, svc, "no active state", func(r connectors.SubscriptionRow) bool { return r.State == protocol.SubscriptionActive })
	if got.Mode != manifest.ModeAPI {
		t.Fatalf("row = %+v", got)
	}
	v, _ := svc.Subscriptions(ctx, "watch")
	if v.Webhook.State != connectors.WebhookReady {
		t.Fatalf("webhook = %+v", v.Webhook)
	}
	waitUntil(t, func() bool {
		var k int
		_ = st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE instance = ? AND room = 'a'`, c.ID).Scan(&k) // retried until the deadline
		return k == 1
	}, "the delivery made no event")

	folder := filepath.Join(connectors.NewFolder(c.ID).Dir(), "connection.yaml")
	if _, err := svc.Subscribe(ctx, "watch", connectors.Subscription{Values: map[string]connectors.Value{"target": {fakeadapter.ManualTarget}}}); err != nil {
		t.Fatal(err)
	}
	var manual connectors.SubscriptionRow
	waitUntil(t, func() bool {
		v, _ := svc.Subscriptions(ctx, "watch")
		for _, r := range v.Subscriptions {
			if r.State == protocol.SubscriptionActionRequired {
				manual = r
				return true
			}
		}
		return false
	}, "no action_required state")
	if manual.Action == nil || len(manual.Action.Show) != 1 || manual.Action.Show[0].Value != srv.URL {
		t.Fatalf("action = %+v", manual.Action)
	}
	before, _ := os.ReadFile(folder)
	if _, err := svc.SubmitSubscription(ctx, "watch", manual.ID, map[string]string{}); protocol.CodeOf(err) != protocol.CodeInvalid {
		t.Fatalf("submit without the code: %v", err)
	}
	v, err := svc.SubmitSubscription(ctx, "watch", manual.ID, map[string]string{"code": "123"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Subscriptions[1].State != protocol.SubscriptionActive || v.Subscriptions[1].Mode != manifest.ModeManual {
		t.Fatalf("after submit = %+v", v.Subscriptions[1])
	}
	after, _ := os.ReadFile(folder)
	if string(before) != string(after) {
		t.Fatal("submit changed the record")
	}
	if _, err := svc.SubmitSubscription(ctx, "watch", "sub_nope", nil); !errors.Is(err, connectors.ErrSubscriptionNotFound) {
		t.Fatalf("submit of an unknown id: %v", err)
	}
}

// TestSubscriptionsNeedManifest proves a connector without a form refuses
// the subscription calls.
func TestSubscriptionsNeedManifest(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	if err := create(ctx, svc, eventsDef("plain", "sleep 30")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Subscriptions(ctx, "plain"); !errors.Is(err, connectors.ErrNoSubscriptions) {
		t.Fatalf("view: %v", err)
	}
	if _, err := svc.Subscribe(ctx, "plain", connectors.Subscription{}); !errors.Is(err, connectors.ErrNoSubscriptions) {
		t.Fatalf("subscribe: %v", err)
	}
	raw := []byte(`[{"values":{}}]`)
	if _, err := svc.Patch(ctx, "plain", connectors.ConnectionPatch{Subscriptions: raw}); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("patch: %v", err)
	}
}

// TestDoctorWebhookUnavailable proves the warning for a watch that only a
// hook can serve.
func TestDoctorWebhookUnavailable(t *testing.T) {
	for _, tc := range []struct {
		modes []string
		want  bool
	}{
		{[]string{manifest.ModeAPI, manifest.ModeManual}, true},
		{[]string{manifest.ModeAPI, manifest.ModePoll}, false},
	} {
		_, _, svc := openStack(t)
		e := inline("watch", func(m *manifest.Manifest) {
			m.Events = &manifest.Events{
				Protocol: manifest.ProtocolConnector, Command: "watch-serve",
				Types:         []manifest.EventType{{Type: "message"}},
				Subscriptions: &manifest.Subscriptions{Action: "Watch", Modes: tc.modes, Webhook: &manifest.Webhook{}},
			}
		})
		e.Subscriptions = []connectors.Subscription{{ID: connectors.NewSubscriptionID()}}
		if _, err := svc.Create(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		got := false
		for _, f := range svc.Doctor(context.Background()) {
			if f.Code == "connector_webhook_unavailable" {
				got = strings.HasSuffix(f.Message, "needs Trebi Cloud for live events")
			}
		}
		if got != tc.want {
			t.Fatalf("modes %v: finding = %v", tc.modes, got)
		}
	}
}
