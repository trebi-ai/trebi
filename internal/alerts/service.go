// Package alerts implements AlertService → inbox items (backend-0808 §7).
package alerts

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/store"
)

// Kind is a closed alert kind.
type Kind string

const (
	KindWaiting              Kind = "waiting"
	KindAuthMissing          Kind = "auth_missing"
	KindLimitExceeded        Kind = "limit_exceeded"
	KindUsageWarning         Kind = "usage_warning"
	KindIntegrationUnhealthy Kind = "integration_unhealthy"
	KindRunFailed            Kind = "run_failed"
	KindResumeExhausted      Kind = "resume_exhausted"
	KindMachineDown          Kind = "machine_down"
)

// Inbox kinds / status.
type InboxKind string

const (
	InboxApproval InboxKind = "approval"
	InboxFailure  InboxKind = "failure"
	InboxHealth   InboxKind = "health"
	InboxWatchdog InboxKind = "watchdog"
)

type InboxStatus string

const (
	InboxUnread   InboxStatus = "unread"
	InboxRead     InboxStatus = "read"
	InboxResolved InboxStatus = "resolved"
)

// Service consumes domain events and creates inbox items.
type Service struct {
	st  *store.Store
	log *slog.Logger
}

// New creates an alerts service.
func New(st *store.Store) *Service {
	return &Service{st: st, log: slog.Default().With("source", "alerts")}
}

// Raise creates an inbox item + alert.raised bus event.
func (s *Service) Raise(ctx context.Context, kind Kind, subject, title, body, hint string, runID, workspaceID, jobName string) (string, error) {
	id := ulid.Make().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	inboxKind := InboxHealth
	switch kind {
	case KindWaiting:
		inboxKind = InboxApproval
	case KindRunFailed, KindResumeExhausted:
		inboxKind = InboxFailure
	case KindMachineDown:
		inboxKind = InboxWatchdog
	}

	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_items (
			id, kind, status, subject, title, body, hint, run_id, workspace_id, job, alert_kind, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, string(inboxKind), string(InboxUnread), subject, title, body, hint,
			store.NullStr(runID), store.NullStr(workspaceID), store.NullStr(jobName), string(kind), now, now)
		return err
	}, store.BusEvent{
		Topic:    "alert.raised",
		Entity:   "alert",
		EntityID: id,
		Action:   "created",
		Attrs: map[string]string{
			"kind":    string(kind),
			"subject": subject,
			"message": title,
			"hint":    hint,
		},
	})
	if err != nil {
		return "", err
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "inbox.created", Entity: "inbox", EntityID: id, Action: "created",
		Attrs: map[string]string{"kind": string(inboxKind), "status": "unread"},
	}); err != nil {
		s.log.Warn("bus append", "error", err)
	}
	return id, nil
}

// ListInbox returns recent inbox items.
func (s *Service) ListInbox(ctx context.Context, status, kind string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, kind, status, COALESCE(title,''), COALESCE(alert_kind,''), created_at
		FROM inbox_items WHERE 1=1`
	var args []any
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	if kind != "" {
		q += ` AND kind=?`
		args = append(args, kind)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, kind, status, title, ak, created string
		if err := rows.Scan(&id, &kind, &status, &title, &ak, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "kind": kind, "status": status, "title": title, "alert_kind": ak, "created_at": created,
		})
	}
	return out, rows.Err()
}
