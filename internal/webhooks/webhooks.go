// Package webhooks owns the hosted webhook object and the ingress sink
// that turns a webhook frame into one event the job pipeline can match.
//
// The event payload has a fixed shape because every match expression
// depends on it:
//
//	{
//	  "hook_id":      "<ingress id of this delivery>",
//	  "label":        "<first path segment of the posted path>",
//	  "method":       "POST",
//	  "path":         "/deploy",
//	  "content_type": "application/json",
//	  "headers":      {"x-github-event": "push"},
//	  "body":         "{\"ref\":\"refs/heads/main\"}",
//	  "received_at":  "2026-09-22T10:00:00Z"
//	}
//
// body stays a string by design: a job calls the existing json() helper
// when it wants a value, and the daemon never parses untrusted payload
// bytes. label is derived from the path the sender posted to, so one hook
// fans out to many jobs by path. Delivery is at-least-once and sink-side
// idempotent on the frame id.
package webhooks

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/store"
)

// Hook is one hosted webhook with its local row. URL holds the public
// receiver including the token: treat it like a webhook secret.
type Hook struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	URL         string `json:"url"`
	CreatedAt   string `json:"created_at"`
}

// HostedHooks is the signed cloud side of a hook object. cloudlink.Service
// implements it structurally (checked below).
type HostedHooks interface {
	MintHook(ctx context.Context, label string) (cloudlink.Hook, error)
	DeleteHook(ctx context.Context, id string) error
	Status() cloudlink.Status
}

// Service is the webhook object family plus the ingress sink.
type Service struct {
	st     *store.Store
	conn   *connectors.Service
	hosted HostedHooks
	log    *slog.Logger

	mu    sync.Mutex
	drops int64
}

const bodyCap = 1 << 20 // the cloud caps the body at the same size

var _ HostedHooks = (*cloudlink.Service)(nil)

// New creates the webhook service. conn is the existing dispatch door
// (connectors.InjectEvent); hosted is the signed mint/revoke side.
func New(st *store.Store, conn *connectors.Service, hosted HostedHooks) *Service {
	return &Service{
		st:     st,
		conn:   conn,
		hosted: hosted,
		log:    slog.Default().With("source", "webhooks"),
	}
}

// Create mints the hosted hook, records the row, and publishes
// hook.created.
func (s *Service) Create(ctx context.Context, workspaceID, label string) (Hook, error) {
	minted, err := s.hosted.MintHook(ctx, label)
	if err != nil {
		return Hook{}, err
	}
	h := Hook{
		ID:          minted.ID,
		WorkspaceID: workspaceID,
		Label:       label,
		URL:         minted.URL,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO cloud_hooks (id, workspace_id, label, url, created_at)
			VALUES (?, ?, ?, ?, ?)`, h.ID, h.WorkspaceID, h.Label, h.URL, h.CreatedAt)
		return err
	}, store.BusEvent{
		Topic: "hook.created", Entity: "hook", EntityID: h.ID, Action: "created",
		Attrs: map[string]string{"workspace_id": h.WorkspaceID, "label": h.Label},
	})
	if err != nil {
		return Hook{}, fmt.Errorf("hook %s: %w", h.ID, err)
	}
	return h, nil
}

// List returns the local hook rows.
func (s *Service) List(ctx context.Context) ([]Hook, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id, workspace_id, label, url, created_at FROM cloud_hooks ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("hooks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Hook{}
	for rows.Next() {
		var h Hook
		if err := rows.Scan(&h.ID, &h.WorkspaceID, &h.Label, &h.URL, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("hooks: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Delete revokes the hosted hook, drops the row, and publishes
// hook.deleted.
func (s *Service) Delete(ctx context.Context, id string) error {
	if err := s.hosted.DeleteHook(ctx, id); err != nil {
		return err
	}
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM cloud_hooks WHERE id = ?`, id)
		return err
	}, store.BusEvent{
		Topic: "hook.deleted", Entity: "hook", EntityID: id, Action: "deleted",
	})
	if err != nil {
		return fmt.Errorf("hook %s: %w", id, err)
	}
	return nil
}

// Deliver turns one webhook-ingress frame into one dispatch event. The
// frame id is claimed in cloud_cursors first so a redelivery after a
// reconnect is dropped before injection. The claim commits before the
// events write rather than in the same transaction because
// connectors.InjectEvent owns its own events insert: a crash between the
// two loses one frame instead of firing twice.
func (s *Service) Deliver(ctx context.Context, f cloudlink.WebhookIngress) error {
	claimed, err := s.claim(ctx, f.ID)
	if err != nil {
		return fmt.Errorf("webhook %s: %w", f.ID, err)
	}
	if !claimed {
		return nil // duplicate: dropped before injection
	}
	if len(f.Body) > bodyCap {
		s.drop()
		s.log.Warn("hook.dropped", "hook_id", f.ID, "reason", "body over cap")
		return nil // acked, never retried
	}
	payload := map[string]any{
		"hook_id":      f.ID, // the ingress id of this delivery
		"label":        hookLabel(f.Path),
		"method":       f.Method,
		"path":         f.Path,
		"content_type": f.ContentType,
		"headers":      f.Headers,
		"body":         f.Body, // stays a string; jobs call json(event.body)
		"received_at":  f.ReceivedAt,
	}
	if _, err := s.conn.InjectNative(ctx, "webhook", payload, connectors.NativeMeta{SourceID: f.ID}); err != nil {
		return fmt.Errorf("webhook %s inject: %w", f.ID, err)
	}
	// hook.received carries the id and the path, never the body: the bus
	// is a notification stream, not a log of untrusted payloads.
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "hook.received", Entity: "hook", EntityID: f.ID, Action: "received",
		Attrs: map[string]string{"hook_id": f.ID, "path": f.Path},
	}); err != nil {
		s.log.Warn("bus append", "topic", "hook.received", "err", err)
	}
	return nil
}

// claim records the frame id as seen and reports whether this call owns
// the delivery. A zero RowsAffected means a duplicate.
func (s *Service) claim(ctx context.Context, id string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO cloud_cursors (stream, last_seq, updated_at) VALUES (?, 0, ?)
		ON CONFLICT(stream) DO NOTHING`, "ingress:"+id, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ConnectorRow is the synthetic connectors-list row for the webhook
// connector (plan J). It carries the events capability only. The status is
// listening only while the cloud link socket is up. The counters come from
// the events table over the last 24 hours; duplicates drop at the claim and
// are not counted.
func (s *Service) ConnectorRow() map[string]any {
	// Both counts are best effort: a failed read renders zero.
	var hooks, events int64
	_ = s.st.DB().QueryRow(`SELECT COUNT(*) FROM cloud_hooks`).Scan(&hooks)
	since := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	_ = s.st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE connector='webhook' AND received_at >= ?`, since).Scan(&events)
	status := "stopped"
	if s.hosted.Status().Connected {
		status = "listening"
	}
	s.mu.Lock()
	drops := s.drops
	s.mu.Unlock()
	return map[string]any{
		"id": "webhook", "name": "webhook",
		"provenance": "native", "managed": false,
		"capabilities": []string{"events"},
		"events": map[string]any{
			"status":   status,
			"metrics":  map[string]any{"hooks": hooks, "dropped": drops},
			"counters": map[string]any{"events": events, "duplicates": int64(0), "parse_errors": int64(0)},
		},
	}
}

// drop counts one deliberately discarded frame.
func (s *Service) drop() {
	s.mu.Lock()
	s.drops++
	s.mu.Unlock()
}

// hookLabel is the first path segment of the posted path without the
// slash: "/deploy" → "deploy", "/deploy/x" → "deploy", "" or "/" → "".
func hookLabel(path string) string {
	p := path
	if len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	for i := range p {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return p
}
