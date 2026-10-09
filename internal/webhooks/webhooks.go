// Package webhooks owns the hosted webhook object and the ingress sink
// that turns a webhook frame into one event the job pipeline can match.
//
// The event payload has a fixed shape because every match expression
// depends on it:
//
//	{
//	  "hook_id":      "<id of the hook that got the request>",
//	  "workspace_id": "<workspace of the hook>",
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
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/entitle"
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
	MintHook(ctx context.Context, req cloudlink.MintHookRequest) (cloudlink.Hook, error)
	DeleteHook(ctx context.Context, id string) error
	Status() cloudlink.Status
	Entitlement() entitle.State
}

// Service is the webhook object family plus the ingress sink.
type Service struct {
	st     *store.Store
	conn   *connectors.Service
	hosted HostedHooks
	log    *slog.Logger

	mu    sync.Mutex
	drops int64

	ensureMu   sync.Mutex
	ensureWant map[string]manifest.Webhook // the newest wanted set; nil when none waits
	ensuring   bool                        // a worker goroutine runs
	mintFailed map[string]bool             // connection id → the last mint failed
	hooks      map[string]connHook         // cache of connection_hooks by connection id
}

// connHook is one row of connection_hooks.
type connHook struct {
	hookID string
	url    string
	secret string
	spec   string
}

const bodyCap = 1 << 20 // the cloud caps the body at the same size

var (
	_ HostedHooks             = (*cloudlink.Service)(nil)
	_ connectors.HookProvider = (*Service)(nil)
)

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
	minted, err := s.hosted.MintHook(ctx, cloudlink.MintHookRequest{Label: label})
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
	if conn := s.connectionOf(f.HookID); conn != "" {
		return s.deliverConnection(ctx, conn, f)
	}
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
		"hook_id":      f.HookID,
		"workspace_id": s.workspaceOf(ctx, f.HookID),
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
		Attrs: map[string]string{"hook_id": f.HookID, "path": f.Path},
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
		"id": "webhook", "type": "webhook", "slug": "webhook", "title": "Webhooks", "status": status,
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

// workspaceOf returns the workspace of a native hook row, or "".
func (s *Service) workspaceOf(ctx context.Context, hookID string) string {
	if hookID == "" {
		return ""
	}
	var ws string
	if err := s.st.DB().QueryRowContext(ctx, `SELECT workspace_id FROM cloud_hooks WHERE id = ?`, hookID).Scan(&ws); err != nil {
		return ""
	}
	return ws
}

// deliverConnection queues one request for the adapter of a connection. The
// queue removes a repeated id, and the frame is acked after the insert.
func (s *Service) deliverConnection(ctx context.Context, conn string, f cloudlink.WebhookIngress) error {
	if len(f.Body) > bodyCap {
		s.drop()
		s.log.Warn("hook.dropped", "hook_id", f.HookID, "connection", conn, "reason", "body over cap")
		return nil
	}
	req := protocol.WebhookRequest{
		ID: f.ID, Method: f.Method, Path: f.Path, Query: f.Query, ContentType: f.ContentType,
		Headers: f.Headers, Body: f.Body, ReceivedAt: f.ReceivedAt, Handshake: f.Handshake,
	}
	if req.Headers == nil {
		req.Headers = map[string]string{}
	}
	if err := s.conn.EnqueueWebhook(ctx, conn, req); err != nil {
		return err
	}
	handshake := "false"
	if f.Handshake {
		handshake = "true"
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "hook.received", Entity: "hook", EntityID: f.ID, Action: "received",
		Attrs: map[string]string{"hook_id": f.HookID, "path": f.Path, "connection": conn, "handshake": handshake},
	}); err != nil {
		s.log.Warn("bus append", "topic", "hook.received", "err", err)
	}
	return nil
}

// loadHooks fills the cache of connection_hooks once. The caller holds
// s.mu.
func (s *Service) loadHooks() {
	if s.hooks != nil {
		return
	}
	s.hooks = map[string]connHook{}
	rows, err := s.st.DB().Query(`SELECT connection_id, hook_id, url, secret, spec FROM connection_hooks`)
	if err != nil {
		s.log.Warn("connection hooks", "err", err)
		return
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var h connHook
		if rows.Scan(&id, &h.hookID, &h.url, &h.secret, &h.spec) == nil {
			s.hooks[id] = h
		}
	}
}

// connectionOf returns the connection that owns a hook id, or "".
func (s *Service) connectionOf(hookID string) string {
	if hookID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadHooks()
	for id, h := range s.hooks {
		if h.hookID == hookID {
			return id
		}
	}
	return ""
}

// Hook returns the hosted hook of a connection, or nil.
func (s *Service) Hook(id string) *protocol.Webhook {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadHooks()
	h, ok := s.hooks[id]
	if !ok {
		return nil
	}
	return &protocol.Webhook{URL: h.url, Secret: h.secret}
}

// HookState tells why a connection has no hook.
func (s *Service) HookState(id string) connectors.WebhookState {
	if s.Hook(id) != nil {
		return connectors.WebhookState{State: connectors.WebhookReady}
	}
	if reason := s.unavailable(); reason != "" {
		return connectors.WebhookState{State: connectors.WebhookUnavailable, Reason: reason}
	}
	s.ensureMu.Lock()
	failed := s.mintFailed[id]
	s.ensureMu.Unlock()
	if failed {
		return connectors.WebhookState{State: connectors.WebhookUnavailable, Reason: connectors.WebhookMintFailed}
	}
	return connectors.WebhookState{State: connectors.WebhookUnavailable}
}

// unavailable returns why no hook can be minted, or "".
func (s *Service) unavailable() string {
	ent := s.hosted.Entitlement()
	switch {
	case !ent.Linked:
		return connectors.WebhookNotLinked
	case !ent.Has(entitle.HostedIngress):
		return connectors.WebhookNoPlan
	}
	return ""
}

// EnsureHooks makes connection_hooks match want in one background
// goroutine. A newer call replaces an older one that waits.
func (s *Service) EnsureHooks(ctx context.Context, want map[string]manifest.Webhook) {
	s.ensureMu.Lock()
	defer s.ensureMu.Unlock()
	s.ensureWant = want
	if s.ensuring {
		return
	}
	s.ensuring = true
	go s.ensureLoop(ctx) // ends when no newer call waits
}

func (s *Service) ensureLoop(ctx context.Context) {
	for {
		s.ensureMu.Lock()
		want := s.ensureWant
		s.ensureWant = nil
		if want == nil {
			s.ensuring = false
			s.ensureMu.Unlock()
			return
		}
		s.ensureMu.Unlock()
		if s.ensure(ctx, want) {
			s.conn.HooksChanged(ctx)
		}
	}
}

// ensure mints, re-mints, and revokes rows. It reports whether a row
// changed.
func (s *Service) ensure(ctx context.Context, want map[string]manifest.Webhook) bool {
	s.mu.Lock()
	s.loadHooks()
	have := make(map[string]connHook, len(s.hooks))
	for id, h := range s.hooks {
		have[id] = h
	}
	s.mu.Unlock()
	changed := false
	for id, h := range have {
		if _, ok := want[id]; !ok {
			changed = true
			s.revoke(ctx, h.hookID)
			s.deleteRow(ctx, id)
		}
	}
	if s.unavailable() != "" {
		return changed
	}
	for id, spec := range want {
		b, _ := json.Marshal(spec) //nolint:errcheck // a struct of strings always encodes
		h, ok := have[id]
		if ok && h.spec == string(b) {
			continue
		}
		if s.mint(ctx, id, spec, string(b)) {
			changed = true
			if ok {
				s.revoke(ctx, h.hookID)
			}
		}
	}
	return changed
}

// mint asks the cloud for one hook and writes its row.
func (s *Service) mint(ctx context.Context, id string, spec manifest.Webhook, specJSON string) bool {
	minted, err := s.hosted.MintHook(ctx, cloudlink.MintHookRequest{Label: id, Headers: spec.Headers, Handshake: spec.Handshake})
	s.ensureMu.Lock()
	if s.mintFailed == nil {
		s.mintFailed = map[string]bool{}
	}
	s.mintFailed[id] = err != nil
	s.ensureMu.Unlock()
	if err != nil {
		s.log.Warn("connection hook mint", "connection", id, "err", err)
		return false
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) //nolint:errcheck // crypto/rand never fails on supported platforms
	h := connHook{hookID: minted.ID, url: minted.URL, secret: base64.RawURLEncoding.EncodeToString(secret), spec: specJSON}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO connection_hooks (connection_id, hook_id, url, secret, spec, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(connection_id) DO UPDATE SET
		hook_id = excluded.hook_id, url = excluded.url, secret = excluded.secret, spec = excluded.spec, created_at = excluded.created_at`,
		id, h.hookID, h.url, h.secret, h.spec, now); err != nil {
		s.log.Warn("connection hook save", "connection", id, "err", err)
		s.revoke(ctx, minted.ID)
		return false
	}
	s.mu.Lock()
	s.hooks[id] = h
	s.mu.Unlock()
	return true
}

// revoke deletes one hook in the cloud. A failure leaves an orphan hook
// that routes to no connection.
func (s *Service) revoke(ctx context.Context, hookID string) {
	if err := s.hosted.DeleteHook(ctx, hookID); err != nil && !errors.Is(err, cloudlink.ErrNotLinked) {
		s.log.Warn("connection hook revoke", "hook_id", hookID, "err", err)
	}
}

func (s *Service) deleteRow(ctx context.Context, id string) {
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM connection_hooks WHERE connection_id = ?`, id); err != nil {
		s.log.Warn("connection hook delete", "connection", id, "err", err)
	}
	s.mu.Lock()
	delete(s.hooks, id)
	s.mu.Unlock()
}

// ReleaseAll deletes every connection hook row without a cloud call. The
// link is gone, so the cloud dropped the hooks.
func (s *Service) ReleaseAll(ctx context.Context) error {
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM connection_hooks`); err != nil {
		return fmt.Errorf("release connection hooks: %w", err)
	}
	s.mu.Lock()
	s.hooks = map[string]connHook{}
	s.mu.Unlock()
	return nil
}
