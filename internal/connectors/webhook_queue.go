package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/protocol"
)

const (
	webhookTick       = 30 * time.Second
	webhookMaxAge     = 24 * time.Hour
	webhookBackoffCap = 10 * time.Minute
)

// EnqueueWebhook stores one delivery for a connection. A repeated id is not
// an error: the primary key drops it.
func (s *Service) EnqueueWebhook(ctx context.Context, connectionID string, req protocol.WebhookRequest) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO webhook_deliveries
		(id, connection_id, request_json, attempts, next_attempt_at, last_error, created_at)
		VALUES (?, ?, ?, 0, ?, '', ?) ON CONFLICT(id) DO NOTHING`,
		req.ID, connectionID, string(b), now, now); err != nil {
		return fmt.Errorf("enqueue webhook %s: %w", req.ID, err)
	}
	s.wakeWebhooks()
	return nil
}

// wakeWebhooks asks the queue worker for a pass.
func (s *Service) wakeWebhooks() {
	select {
	case s.webhookWake <- struct{}{}:
	default:
	}
}

// runWebhookQueue delivers due rows on each tick and wake. It ends with ctx.
func (s *Service) runWebhookQueue(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.webhookEvery)
	defer t.Stop()
	for {
		s.deliverDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.webhookWake:
		}
	}
}

// deliverDue drops the rows past webhookMaxAge, then delivers the due rows
// of each listening connection in created_at order.
func (s *Service) deliverDue(ctx context.Context) {
	now := s.now().UTC()
	s.dropExpired(ctx, now)
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT DISTINCT connection_id FROM webhook_deliveries WHERE next_attempt_at <= ?`, now.Format(time.RFC3339Nano))
	if err != nil {
		s.log.Warn("webhook queue", "error", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close() //nolint:errcheck // read-only query
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		p := s.procByName(id)
		if p == nil || p.snapshot().status != statusListening {
			continue
		}
		s.deliverConnection(ctx, p, now)
	}
}

type webhookRow struct {
	id       string
	request  string
	attempts int
	due      string
}

// deliverConnection sends the rows of one connection in order. A row in
// backoff holds the rows after it, so a handshake comes first.
func (s *Service) deliverConnection(ctx context.Context, p *proc, now time.Time) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, request_json, attempts, next_attempt_at
		FROM webhook_deliveries WHERE connection_id = ? ORDER BY created_at, id`, p.id)
	if err != nil {
		s.log.Warn("webhook queue", "connector", p.id, "error", err)
		return
	}
	var list []webhookRow
	for rows.Next() {
		var r webhookRow
		if rows.Scan(&r.id, &r.request, &r.attempts, &r.due) == nil {
			list = append(list, r)
		}
	}
	_ = rows.Close() //nolint:errcheck // read-only query
	stamp := now.Format(time.RFC3339Nano)
	for _, r := range list {
		if r.due > stamp || !s.deliverRow(ctx, p, r, now) {
			return
		}
	}
}

// deliverRow sends one row. It reports whether the next row may go.
func (s *Service) deliverRow(ctx context.Context, p *proc, r webhookRow, now time.Time) bool {
	var req protocol.WebhookRequest
	err := json.Unmarshal([]byte(r.request), &req)
	if err == nil {
		err = s.Call(ctx, p.id, protocol.MethodWebhookReceive, req, nil)
	}
	var pe *protocol.Error
	switch {
	case err == nil:
		s.deleteDelivery(ctx, r.id)
		return true
	case errors.As(err, &pe) && (pe.Code == protocol.CodeInvalid || pe.Code == protocol.CodePermanent):
		s.deleteDelivery(ctx, r.id)
		p.mu.Lock()
		p.rejected++
		p.mu.Unlock()
		s.log.Warn("connector.webhook.rejected", "connector", p.id, "delivery", r.id, "error", err)
		return true
	}
	attempts := r.attempts + 1
	wait := min(time.Duration(attempts*attempts)*time.Second, webhookBackoffCap)
	if _, uerr := s.st.DB().ExecContext(ctx, `UPDATE webhook_deliveries
		SET attempts = ?, next_attempt_at = ?, last_error = ? WHERE id = ?`,
		attempts, now.Add(wait).Format(time.RFC3339Nano), truncate(err.Error(), 500), r.id); uerr != nil {
		s.log.Warn("webhook queue", "connector", p.id, "error", uerr)
	}
	return false
}

func (s *Service) deleteDelivery(ctx context.Context, id string) {
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE id = ?`, id); err != nil {
		s.log.Warn("webhook queue", "delivery", id, "error", err)
	}
}

// dropExpired deletes the rows past webhookMaxAge and counts them.
func (s *Service) dropExpired(ctx context.Context, now time.Time) {
	cut := now.Add(-webhookMaxAge).Format(time.RFC3339Nano)
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id, connection_id FROM webhook_deliveries WHERE created_at < ?`, cut)
	if err != nil {
		return
	}
	type old struct{ id, conn string }
	var list []old
	for rows.Next() {
		var o old
		if rows.Scan(&o.id, &o.conn) == nil {
			list = append(list, o)
		}
	}
	_ = rows.Close() //nolint:errcheck // read-only query
	for _, o := range list {
		s.deleteDelivery(ctx, o.id)
		if p := s.procByName(o.conn); p != nil {
			p.mu.Lock()
			p.dropped++
			p.mu.Unlock()
		}
		s.log.Warn("connector.webhook.dropped", "connector", o.conn, "delivery", o.id)
	}
}

// dropWebhooks deletes the queued deliveries of a deleted connection.
func (s *Service) dropWebhooks(ctx context.Context, id string) {
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE connection_id = ?`, id); err != nil {
		s.log.Warn("webhook queue", "connector", id, "error", err)
	}
}
