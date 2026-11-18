package connectors_test

import (
	"context"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/connectors/protocol/fakeadapter"
)

// queueStack starts "watch" with one subscription and a hook whose URL
// takes nothing, so the queue gets only the requests of the test.
func queueStack(t *testing.T, faults ...string) (*connectors.Service, func(query string, args ...any) int, connectors.Connection) {
	t.Helper()
	hook := &protocol.Webhook{URL: "http://127.0.0.1:1/closed", Secret: "s3cret"}
	subs := []connectors.Subscription{{ID: connectors.NewSubscriptionID(), Values: map[string]connectors.Value{"target": {"a"}}}}
	st, svc, _, c := watchStack(t, hook, subs, faults...)
	count := func(query string, args ...any) int {
		var n int
		_ = st.DB().QueryRow(query, args...).Scan(&n) // a failed read counts zero
		return n
	}
	waitStatus(t, svc, "watch", "listening")
	return svc, count, c
}

func signed(id, target string) protocol.WebhookRequest {
	body := `{"target":"` + target + `","text":"hi"}`
	return protocol.WebhookRequest{
		ID: id, Method: "POST", Path: "/", Body: body, ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Headers: map[string]string{fakeadapter.SignatureHeader: fakeadapter.Sign("s3cret", body)},
	}
}

// TestWebhookQueueDeliversInOrderOnce proves the de-duplication by id and
// the delivery order.
func TestWebhookQueueDeliversInOrderOnce(t *testing.T) {
	svc, count, c := queueStack(t)
	ctx := context.Background()
	for _, req := range []protocol.WebhookRequest{signed("d1", "r1"), signed("d1", "r1"), signed("d2", "r2")} {
		if err := svc.EnqueueWebhook(ctx, c.ID, req); err != nil {
			t.Fatal(err)
		}
	}
	// The event of a delivery commits before its row is deleted.
	waitUntil(t, func() bool {
		return count(`SELECT COUNT(*) FROM events WHERE instance = ? AND source_id IN ('d1', 'd2')`, c.ID) == 2 &&
			count(`SELECT COUNT(*) FROM webhook_deliveries`) == 0
	}, "the deliveries made no events, or rows stayed")
	if n := count(`SELECT COUNT(*) FROM events WHERE instance = ? AND source_id = 'd1' AND id < (SELECT id FROM events WHERE source_id = 'd2')`, c.ID); n != 1 {
		t.Fatal("d2 came before d1")
	}
}

// TestWebhookQueueRejectsForged proves an invalid answer deletes the row
// and counts it.
func TestWebhookQueueRejectsForged(t *testing.T) {
	svc, count, c := queueStack(t)
	req := signed("d1", "r1")
	req.Body += " "
	if err := svc.EnqueueWebhook(context.Background(), c.ID, req); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return count(`SELECT COUNT(*) FROM webhook_deliveries`) == 0 }, "the forged row stayed")
	row, _ := svc.Get("watch")
	if got := row["counters"].(map[string]any)["rejected"]; got != int64(1) {
		t.Fatalf("rejected = %v", got)
	}
	if n := count(`SELECT COUNT(*) FROM events WHERE instance = ? AND source_id = 'd1'`, c.ID); n != 0 {
		t.Fatal("a forged delivery made an event")
	}
}

// TestWebhookQueueBacksOffAndDrops proves a transient answer keeps the row
// with a later attempt, and a row past the maximum age is dropped.
func TestWebhookQueueBacksOffAndDrops(t *testing.T) {
	svc, count, c := queueStack(t, fakeadapter.FaultBusyWebhook)
	ctx := context.Background()
	if err := svc.EnqueueWebhook(ctx, c.ID, signed("d1", "r1")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		return count(`SELECT COUNT(*) FROM webhook_deliveries WHERE attempts = 1 AND last_error != '' AND next_attempt_at > created_at`) == 1
	}, "no backoff after transient")
	old := time.Now().UTC().Add(-25 * time.Hour).Format(time.RFC3339Nano)
	if n := count(`SELECT COUNT(*) FROM webhook_deliveries`); n != 1 {
		t.Fatalf("rows = %d", n)
	}
	if err := svc.EnqueueWebhook(ctx, c.ID, protocol.WebhookRequest{ID: "d2"}); err != nil {
		t.Fatal(err)
	}
	if count(`UPDATE webhook_deliveries SET created_at = ? RETURNING 1`, old) == 0 {
		t.Fatal("no row aged")
	}
	if err := svc.EnqueueWebhook(ctx, c.ID, protocol.WebhookRequest{ID: "d3"}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		row, _ := svc.Get("watch")
		return row["counters"].(map[string]any)["dropped"] == int64(2)
	}, "the old rows were not dropped")
}
