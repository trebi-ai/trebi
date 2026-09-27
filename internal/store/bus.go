package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// BusEvent is a row pending insert into event_log (within a tx).
type BusEvent struct {
	Topic    string
	TraceID  string
	Entity   string
	EntityID string
	Action   string
	Revision int64
	Attrs    any // marshaled to JSON; nil ok
}

// AppendInTx inserts one bus event inside an existing transaction.
// Caller must hold Store.LockBus() around the whole entity+bus transaction
// when concurrent writers exist, or use Store.Append which locks.
func AppendInTx(tx *sql.Tx, ev BusEvent) (int64, error) {
	if !IsBusTopic(ev.Topic) {
		return 0, fmt.Errorf("unregistered bus topic: %s", ev.Topic)
	}
	var attrsJSON *string
	if ev.Attrs != nil {
		b, err := json.Marshal(ev.Attrs)
		if err != nil {
			return 0, err
		}
		s := string(b)
		attrsJSON = &s
	}
	eventID := ulid.Make().String()
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	res, err := tx.Exec(`INSERT INTO event_log (event_id, topic, ts, trace_id, entity, entity_id, action, revision, attrs_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		eventID, ev.Topic, ts, NullStr(ev.TraceID), NullStr(ev.Entity), NullStr(ev.EntityID),
		NullStr(ev.Action), nullInt64(ev.Revision), attrsJSON)
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	return seq, err
}

// Append commits a single bus event in its own transaction (serialized).
func (s *Store) Append(ctx context.Context, ev BusEvent) (int64, error) {
	s.LockBus()
	defer s.UnlockBus()
	var seq int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		seq, err = AppendInTx(tx, ev)
		return err
	})
	if err == nil {
		s.NotifyBus()
	}
	return seq, err
}

// AppendEntity writes an entity mutation + bus event in one transaction.
func (s *Store) AppendEntity(ctx context.Context, write func(tx *sql.Tx) error, ev BusEvent) (int64, error) {
	s.LockBus()
	defer s.UnlockBus()
	var seq int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := write(tx); err != nil {
			return err
		}
		var err error
		seq, err = AppendInTx(tx, ev)
		return err
	})
	if err == nil {
		s.NotifyBus()
	}
	return seq, err
}

// ListBusOpts filters and orders event_log rows for ListBusWith.
// After/Topics/Limit match ListBus. Since and Desc are extra; Desc must stay
// off on the consumer cursor path (Consume reads oldest-first). A set
// Workspace keeps rows tagged with it plus untagged (global) rows.
type ListBusOpts struct {
	After     int64
	Topics    []string
	Limit     int
	Since     time.Time // inclusive; zero = no lower bound
	Desc      bool      // newest first; default oldest first
	Workspace string    // matches attrs_json $.workspace; missing attr = global
}

// ListBus returns events with seq > after, optional topic filter (prefix match on families), oldest first.
func (s *Store) ListBus(ctx context.Context, after int64, topics []string, limit int) ([]StreamEvent, error) {
	return s.ListBusWith(ctx, ListBusOpts{After: after, Topics: topics, Limit: limit})
}

// ListBusWith returns event_log rows matching opts.
func (s *Store) ListBusWith(ctx context.Context, opts ListBusOpts) ([]StreamEvent, error) {
	if opts.Limit <= 0 {
		opts.Limit = 100
	}
	q := `SELECT seq, event_id, topic, ts, trace_id, entity, entity_id, action, revision, attrs_json
		FROM event_log WHERE seq > ?`
	args := []any{opts.After}
	if len(opts.Topics) > 0 {
		// Match exact or family prefix: "run" matches "run.created"
		var parts []string
		for _, t := range opts.Topics {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			parts = append(parts, "(topic = ? OR topic LIKE ?)")
			args = append(args, t, t+".%")
		}
		if len(parts) > 0 {
			q += " AND (" + strings.Join(parts, " OR ") + ")"
		}
	}
	if !opts.Since.IsZero() {
		q += " AND ts >= ?"
		args = append(args, opts.Since.UTC().Format("2006-01-02T15:04:05.000000Z"))
	}
	if opts.Workspace != "" {
		// Rows without the attr are global and stay visible (A.1).
		q += ` AND (json_extract(attrs_json, '$.workspace') = ? OR json_extract(attrs_json, '$.workspace') IS NULL)`
		args = append(args, opts.Workspace)
	}
	if opts.Desc {
		q += " ORDER BY seq DESC LIMIT ?"
	} else {
		q += " ORDER BY seq ASC LIMIT ?"
	}
	args = append(args, opts.Limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StreamEvent
	for rows.Next() {
		ev, err := scanStreamEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// OldestSeq returns the minimum retained seq (0 if empty).
func (s *Store) OldestSeq(ctx context.Context) (int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MIN(seq) FROM event_log`).Scan(&v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// PruneBus deletes event_log rows older than retention.
func (s *Store) PruneBus(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention).Format("2006-01-02T15:04:05.000000Z")
	res, err := s.db.ExecContext(ctx, `DELETE FROM event_log WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// NullStr maps "" to SQL NULL. One copy for all packages (C6).
func NullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// StreamEvent — bus wire frame (§4.2).
type StreamEvent struct {
	Seq      int64           `json:"seq"`
	Topic    string          `json:"topic"`
	TS       time.Time       `json:"ts"`
	TraceID  string          `json:"trace_id,omitempty"`
	Entity   string          `json:"entity,omitempty"`
	ID       string          `json:"id,omitempty"`
	Action   string          `json:"action,omitempty"`
	Revision int64           `json:"revision,omitempty"`
	Attrs    json.RawMessage `json:"attrs,omitempty"`
}

// EventMeta describes one registered event name.
type EventMeta struct {
	Name string
	Bus  bool // also a bus topic (★)
	Log  bool // ops log (always true for registry entries)
}

// Synthetic bus-only topic (never stored, never logged).
const TopicSystemReset = "system.reset"

// AllEvents is the single source of truth. Lint tests assert bus topics ⊆ this set.
func AllEvents() []EventMeta { //nolint:funlen // static data table
	return []EventMeta{
		// system
		{Name: "system.started", Bus: true, Log: true},
		{Name: "system.stopping", Bus: true, Log: true},
		{Name: "system.drain.entered", Bus: true, Log: true},
		{Name: "system.drain.progress", Bus: true, Log: true},
		{Name: "system.drain.ready", Bus: true, Log: true},
		{Name: "system.drain.cancelled", Bus: true, Log: true},
		{Name: "doctor.fix_applied", Bus: false, Log: true},
		// api
		{Name: "api.request", Bus: false, Log: true},
		{Name: "api.auth_failed", Bus: false, Log: true},
		// workspace
		{Name: "workspace.created", Bus: true, Log: true},
		{Name: "workspace.updated", Bus: true, Log: true},
		{Name: "workspace.deleted", Bus: true, Log: true},
		{Name: "workspace.watch_failed", Bus: false, Log: true},
		// assistant sessions (plan 2026-09-26 F.4)
		{Name: "assistant.settings.updated", Bus: true, Log: true},
		{Name: "assistant.session.started", Bus: false, Log: true},
		{Name: "assistant.session.closed", Bus: false, Log: true},
		{Name: "assistant.permission.timeout", Bus: false, Log: true},
		// agent (folder-scoped operator, AGENT.md)
		{Name: "agent.created", Bus: true, Log: true},
		{Name: "agent.updated", Bus: true, Log: true},
		{Name: "agent.renamed", Bus: true, Log: true},
		{Name: "agent.deleted", Bus: true, Log: true},
		{Name: "agent.woken", Bus: false, Log: true},
		{Name: "agent.escalated", Bus: true, Log: true},
		{Name: "agent.parse_failed", Bus: false, Log: true},
		{Name: "proposal.created", Bus: true, Log: true},
		{Name: "proposal.decided", Bus: true, Log: true},
		// job
		{Name: "job.created", Bus: true, Log: true},
		{Name: "job.updated", Bus: true, Log: true},
		{Name: "job.deleted", Bus: true, Log: true},
		{Name: "job.parse_failed", Bus: false, Log: true},
		{Name: "memory.updated", Bus: false, Log: true},
		{Name: "memory.reset", Bus: false, Log: true},
		{Name: "memory.migrated", Bus: false, Log: true},
		{Name: "memory.context_set", Bus: false, Log: true},
		{Name: "memory.state_set", Bus: false, Log: true},
		{Name: "memory.lesson_added", Bus: false, Log: true},
		{Name: "memory.lesson_removed", Bus: false, Log: true},
		{Name: "memory.log_added", Bus: false, Log: true},
		{Name: "memory.log_removed", Bus: false, Log: true},
		{Name: "memory.invalid", Bus: true, Log: true},
		{Name: "template.installed", Bus: false, Log: true},
		// file
		{Name: "file.created", Bus: true, Log: true},
		{Name: "file.updated", Bus: true, Log: true},
		{Name: "file.deleted", Bus: true, Log: true},
		// run
		{Name: "run.created", Bus: true, Log: true},
		{Name: "run.updated", Bus: true, Log: true},
		{Name: "run.completed", Bus: false, Log: true},
		{Name: "run.failed", Bus: true, Log: true},
		{Name: "run.killed", Bus: false, Log: true},
		{Name: "run.resumed", Bus: false, Log: true},
		{Name: "run.replayed", Bus: false, Log: true},
		{Name: "run.nudged", Bus: false, Log: true},
		{Name: "run.coalesced", Bus: false, Log: true},
		{Name: "run.parked", Bus: false, Log: true},
		{Name: "run.resume_exhausted", Bus: true, Log: true},
		{Name: "run.auto_resumed", Bus: false, Log: true},
		{Name: "run.event", Bus: true, Log: true},
		// schedule
		{Name: "schedule.updated", Bus: true, Log: true},
		{Name: "schedule.fired", Bus: false, Log: true},
		{Name: "schedule.catch_up", Bus: true, Log: true},
		{Name: "schedule.catch_up_skipped", Bus: true, Log: true},
		// dispatch
		{Name: "event.created", Bus: true, Log: true},
		// queue
		{Name: "queue.updated", Bus: true, Log: true},
		{Name: "queue.enqueued", Bus: false, Log: true},
		{Name: "queue.claimed", Bus: false, Log: true},
		{Name: "queue.blocked", Bus: false, Log: true},
		{Name: "queue.done", Bus: false, Log: true},
		{Name: "queue.dead", Bus: false, Log: true},
		{Name: "queue.pruned", Bus: false, Log: true},
		// connector
		{Name: "connector.created", Bus: true, Log: true},
		{Name: "connector.updated", Bus: true, Log: true},
		{Name: "connector.deleted", Bus: true, Log: true},
		{Name: "connector.started", Bus: false, Log: true},
		{Name: "connector.stopped", Bus: false, Log: true},
		{Name: "connector.restarted", Bus: false, Log: true},
		{Name: "connector.exit", Bus: false, Log: true},
		{Name: "connector.unhealthy", Bus: true, Log: true},
		{Name: "connector.recovered", Bus: true, Log: true},
		{Name: "connector.catchup.started", Bus: true, Log: true},
		{Name: "connector.catchup.completed", Bus: true, Log: true},
		{Name: "connector.catchup.skipped", Bus: true, Log: true},
		{Name: "connector.parse_error", Bus: false, Log: true},
		// harness/auth
		{Name: "harness.updated", Bus: true, Log: true},
		{Name: "harness.installed", Bus: false, Log: true},
		{Name: "harness.pinned", Bus: false, Log: true},
		{Name: "harness.limit_exceeded", Bus: true, Log: true},
		{Name: "harness.limit_cleared", Bus: true, Log: true},
		{Name: "auth.probe.failed", Bus: true, Log: true},
		{Name: "auth.login_started", Bus: false, Log: true},
		{Name: "auth.login_completed", Bus: false, Log: true},
		// skills
		{Name: "skill.created", Bus: true, Log: true},
		{Name: "skill.updated", Bus: true, Log: true},
		{Name: "skill.deleted", Bus: true, Log: true},
		{Name: "skill.projection_changed", Bus: false, Log: true},
		{Name: "skill.forked", Bus: false, Log: true},
		// secrets/config
		{Name: "secret.updated", Bus: true, Log: true},
		{Name: "secret.deleted", Bus: true, Log: true},
		{Name: "secret.referenced_missing", Bus: false, Log: true},
		{Name: "config.updated", Bus: true, Log: true},
		// conversations
		{Name: "conversation.created", Bus: true, Log: true},
		{Name: "conversation.updated", Bus: true, Log: true},
		{Name: "conversation.deleted", Bus: true, Log: true},
		{Name: "conversation.switched", Bus: false, Log: true},
		{Name: "conversation.demarcated", Bus: false, Log: true},
		// channel replies (plan 2026-09-27 G)
		{Name: "channel.queued", Bus: true, Log: true},
		{Name: "channel.sent", Bus: true, Log: true},
		{Name: "channel.send_failed", Bus: true, Log: true},
		// agent
		{Name: "session.state", Bus: true, Log: true},
		// alerts/inbox
		{Name: "alert.raised", Bus: true, Log: true},
		{Name: "alert.suppressed", Bus: true, Log: true},
		{Name: "alert.delivered", Bus: true, Log: true},
		{Name: "inbox.created", Bus: true, Log: true},
		{Name: "inbox.updated", Bus: true, Log: true},
		{Name: "inbox.resolved", Bus: false, Log: true},
		// approvals
		{Name: "approval.created", Bus: true, Log: true},
		{Name: "approval.decided", Bus: true, Log: true},
		// email
		{Name: "email.sent", Bus: true, Log: true},
		{Name: "email.received", Bus: true, Log: true},
		// bus
		{Name: "bus.pruned", Bus: false, Log: true},
		{Name: "bus.slow_consumer", Bus: false, Log: true},
		// cloud
		{Name: "cloud.linked", Bus: true, Log: true},
		{Name: "cloud.unlinked", Bus: true, Log: true},
		{Name: "cloud.connected", Bus: true, Log: true},
		{Name: "cloud.disconnected", Bus: true, Log: true},
		{Name: "cloud.push_failed", Bus: false, Log: true},
		{Name: "cloud.entitlement", Bus: true, Log: true},
		// hosted webhooks
		{Name: "hook.created", Bus: true, Log: true},
		{Name: "hook.deleted", Bus: true, Log: true},
		{Name: "hook.received", Bus: true, Log: true},
	}
}

// BusTopics returns all event names that ride the bus.
func BusTopics() []string {
	var out []string
	for _, e := range AllEvents() {
		if e.Bus {
			out = append(out, e.Name)
		}
	}
	return out
}

// eventByName indexes AllEvents for O(1) IsBusTopic / IsRegistered.
var eventByName = func() map[string]EventMeta { //nolint:gochecknoglobals // static registry index, never mutated
	events := AllEvents()
	m := make(map[string]EventMeta, len(events))
	for _, e := range events {
		m[e.Name] = e
	}
	return m
}()

// IsRegistered reports whether name is in the registry (or the synthetic reset).
func IsRegistered(name string) bool {
	if name == TopicSystemReset {
		return true
	}
	_, ok := eventByName[name]
	return ok
}

// IsBusTopic reports whether name may be written to event_log (not system.reset).
func IsBusTopic(name string) bool {
	e, ok := eventByName[name]
	return ok && e.Bus
}

func eventSet() map[string]EventMeta {
	m := make(map[string]EventMeta, len(eventByName))
	for k, v := range eventByName {
		m[k] = v
	}
	return m
}

// scanStreamEvent maps one event_log row to a StreamEvent.
func scanStreamEvent(rows *sql.Rows) (StreamEvent, error) {
	var (
		seq                               int64
		eventID, topic, ts                string
		traceID, entity, entityID, action sql.NullString
		revision                          sql.NullInt64
		attrs                             sql.NullString
	)
	if err := rows.Scan(&seq, &eventID, &topic, &ts, &traceID, &entity, &entityID, &action, &revision, &attrs); err != nil {
		return StreamEvent{}, err
	}
	t, _ := time.Parse("2006-01-02T15:04:05.000000Z", ts)
	if t.IsZero() {
		t, _ = time.Parse(time.RFC3339Nano, ts)
	}
	ev := StreamEvent{
		Seq:   seq,
		Topic: topic,
		TS:    t,
	}
	if traceID.Valid {
		ev.TraceID = traceID.String
	}
	if entity.Valid {
		ev.Entity = entity.String
	}
	if entityID.Valid {
		ev.ID = entityID.String
	}
	if action.Valid {
		ev.Action = action.String
	}
	if revision.Valid {
		ev.Revision = revision.Int64
	}
	if attrs.Valid && attrs.String != "" {
		ev.Attrs = json.RawMessage(attrs.String)
	}
	return ev, nil
}

func nullInt64(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
