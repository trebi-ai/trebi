package webhooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// fakeHosted records the signed mint/revoke side.
type fakeHosted struct {
	deleted []string
}

func (f *fakeHosted) MintHook(_ context.Context, label string) (cloudlink.Hook, error) {
	return cloudlink.Hook{
		ID:    "01J8YHOOK000000000000000001",
		Token: "tok-1",
		URL:   "https://in.example.com/hook/tok-1",
	}, nil
}

func (f *fakeHosted) DeleteHook(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// TestDeliverWritesDocumentedEventPayload pins the event payload shape
// every match expression depends on: body stays a string and label comes
// from the posted path.
func TestDeliverWritesDocumentedEventPayload(t *testing.T) {
	st, conn := jobStack(t)
	svc := New(st, conn, &fakeHosted{})
	ctx := context.Background()

	if err := svc.Deliver(ctx, testFrame("01J8YINGRESS000000000000001")); err != nil {
		t.Fatal(err)
	}

	var raw string
	if err := st.DB().QueryRow(`SELECT raw_json FROM events WHERE connector = 'webhook'`).Scan(&raw); err != nil {
		t.Fatalf("events row: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"hook_id":      "01J8YINGRESS000000000000001",
		"label":        "deploy",
		"method":       "POST",
		"path":         "/deploy",
		"content_type": "application/json",
		"headers":      map[string]any{"x-github-event": "push"},
		"body":         `{"ref":"refs/heads/main"}`,
		"received_at":  "2026-09-22T10:00:00Z",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %+v\nwant %+v", got, want)
	}

	// hook.received carries the id and the path, never the body: the bus
	// is a notification stream, not a log of untrusted payloads.
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic = 'hook.received'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("hook.received events = %d, want 1", n)
	}
	var attrs string
	if err := st.DB().QueryRow(`SELECT attrs_json FROM event_log WHERE topic = 'hook.received'`).Scan(&attrs); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(attrs, "refs/heads/main") || strings.Contains(attrs, "body") {
		t.Fatalf("bus frame leaked the body: %s", attrs)
	}

	// Label comes from the path the sender posted to.
	for path, label := range map[string]string{"/stripe/x": "stripe", "/": "", "": ""} {
		if got := hookLabel(path); got != label {
			t.Errorf("hookLabel(%q) = %q, want %q", path, got, label)
		}
	}
}

// TestDeliverFiresWebhookJobOnce runs the full door: frame → event →
// dispatchTrigger (allowlist, Goja match/extract, policy) → queue.
func TestDeliverFiresWebhookJobOnce(t *testing.T) {
	st, conn := jobStack(t)
	svc := New(st, conn, &fakeHosted{})
	ctx := context.Background()

	if err := svc.Deliver(ctx, testFrame("01J8YINGRESS000000000000001")); err != nil {
		t.Fatal(err)
	}

	var events, queued, outcomes int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE connector = 'webhook'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("events rows = %d, want 1", events)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job = 'deploy'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("queue items = %d, want 1", queued)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes WHERE job = 'deploy' AND outcome = 'queued'`).Scan(&outcomes); err != nil {
		t.Fatal(err)
	}
	if outcomes != 1 {
		t.Fatalf("queued outcomes = %d, want 1", outcomes)
	}
	// The extract parsed the string body with json(): vars carry the ref.
	var varsJSON string
	if err := st.DB().QueryRow(`SELECT vars_json FROM event_queue WHERE job = 'deploy'`).Scan(&varsJSON); err != nil {
		t.Fatal(err)
	}
	var vars map[string]string
	if err := json.Unmarshal([]byte(varsJSON), &vars); err != nil {
		t.Fatal(err)
	}
	if vars["ref"] != "refs/heads/main" {
		t.Fatalf("vars = %+v, want ref from json(event.body)", vars)
	}
}

// TestDeliverDuplicateFrameIgnored: the frame id is the ledger key, so a
// redelivery after a reconnect fires nothing a second time.
func TestDeliverDuplicateFrameIgnored(t *testing.T) {
	st, conn := jobStack(t)
	svc := New(st, conn, &fakeHosted{})
	ctx := context.Background()

	for range 2 {
		if err := svc.Deliver(ctx, testFrame("01J8YINGRESS000000000000001")); err != nil {
			t.Fatal(err)
		}
	}

	for _, q := range []struct {
		query string
		want  int
	}{
		{`SELECT COUNT(*) FROM events WHERE connector = 'webhook'`, 1},
		{`SELECT COUNT(*) FROM event_queue WHERE job = 'deploy'`, 1},
		{`SELECT COUNT(*) FROM cloud_cursors WHERE stream = 'ingress:01J8YINGRESS000000000000001'`, 1},
		{`SELECT COUNT(*) FROM event_log WHERE topic = 'hook.received'`, 1},
	} {
		var n int
		if err := st.DB().QueryRow(q.query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != q.want {
			t.Errorf("%s = %d, want %d", q.query, n, q.want)
		}
	}
}

// TestDeliverOversizedBodyDropped: the daemon caps again on ingress. The
// frame is counted, logged, acked, and never retried.
func TestDeliverOversizedBodyDropped(t *testing.T) {
	st, conn := jobStack(t)
	svc := New(st, conn, &fakeHosted{})
	ctx := context.Background()

	f := testFrame("01J8YINGRESS000000000000002")
	f.Body = strings.Repeat("x", bodyCap+1)
	if err := svc.Deliver(ctx, f); err != nil {
		t.Fatalf("oversized body must settle (ack), got %v", err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("events rows = %d, want 0", n)
	}
	svc.mu.Lock()
	drops := svc.drops
	svc.mu.Unlock()
	if drops != 1 {
		t.Fatalf("drops = %d, want 1", drops)
	}
}

// TestCreateDeleteLifecycle proves the hosted object: mint, row, bus
// frame, and revoke with the row gone afterwards.
func TestCreateDeleteLifecycle(t *testing.T) {
	st, conn := openStack(t)
	hosted := &fakeHosted{}
	svc := New(st, conn, hosted)
	ctx := context.Background()

	h, err := svc.Create(ctx, "ws-1", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if h.ID == "" || h.URL != "https://in.example.com/hook/tok-1" || h.Label != "deploy" || h.CreatedAt == "" {
		t.Fatalf("created = %+v", h)
	}
	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 || list[0].WorkspaceID != "ws-1" {
		t.Fatalf("List = (%+v, %v)", list, err)
	}
	for _, topic := range []string{"hook.created", "hook.deleted"} {
		var n int
		want := 0
		if topic == "hook.created" {
			want = 1
		}
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic = ?`, topic).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s events = %d, want %d", topic, n, want)
		}
	}

	if err := svc.Delete(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if len(hosted.deleted) != 1 || hosted.deleted[0] != h.ID {
		t.Fatalf("revoked = %v, want [%s]", hosted.deleted, h.ID)
	}
	list, err = svc.List(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("List after delete = (%+v, %v)", list, err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic = 'hook.deleted'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("hook.deleted events = %d, want 1", n)
	}
}

// testFrame is one webhook-ingress fixture, the shape the contract pins.
func testFrame(id string) cloudlink.WebhookIngress {
	return cloudlink.WebhookIngress{
		V:           cloudlink.ContractV,
		ID:          id,
		Method:      "POST",
		Path:        "/deploy",
		ContentType: "application/json",
		Headers:     map[string]string{"x-github-event": "push"},
		Body:        `{"ref":"refs/heads/main"}`,
		ReceivedAt:  "2026-09-22T10:00:00Z",
	}
}

// openStack wires store + connectors the way the daemon does.
func openStack(t *testing.T) (*store.Store, *connectors.Service) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := workspaces.New(st)
	return st, connectors.New(st, ws, nil)
}

// jobStack indexes one workspace with a webhook job whose match reads
// event.headers and whose extract keeps the fields the match needs and
// parses the body with json() (dispatch runs extract before match and
// rebinds event to the extracted value).
func jobStack(t *testing.T) (*store.Store, *connectors.Service) {
	t.Helper()
	st, conn := openStack(t)
	wsPath := filepath.Join(home.Base(), "ws")
	jobDir := filepath.Join(wsPath, "deploy")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\ntriggers:\n  - event: webhook\n" +
		"    match: 'event.headers[\"x-github-event\"] === \"push\"'\n" +
		"    extract: '({ hook_id: event.hook_id, label: event.label, path: event.path, headers: event.headers, ref: json(event.body).ref })'\n" +
		"---\n\nFire.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := workspaces.New(st)
	w, err := ws.Add(context.Background(), wsPath, "hook-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	return st, conn
}
