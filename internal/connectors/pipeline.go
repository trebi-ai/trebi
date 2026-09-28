package connectors

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// InjectEvent handles a synthetic event (`connector test`) through the same
// ingest path. It refuses a reserved name. A source id that is already
// stored returns ErrDuplicate.
func (s *Service) InjectEvent(ctx context.Context, name string, event any) (string, error) {
	if err := s.GuardReserved(name); err != nil {
		return "", err
	}
	if err := s.GuardMutate(name); err != nil {
		return "", err
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	rec := eventRecord{
		id:         ulid.Make().String(),
		raw:        string(raw),
		trace:      ulid.Make().String(),
		receivedAt: time.Now().UTC().Format(time.RFC3339Nano),
		payload:    event,
	}
	s.fillEventMeta(&rec, name)
	inserted, err := s.recordEvent(ctx, name, rec, recordOpts{})
	if err != nil {
		return "", err
	}
	if !inserted {
		return "", fmt.Errorf("%w: %q", ErrDuplicate, rec.sourceID)
	}
	s.dispatchEvent(ctx, name, rec)
	return rec.id, nil
}

// Redispatch matches one stored event against the current triggers again.
// It writes no new events row, so it works for every connector, native ones
// included, and for an event whose source id is already stored.
func (s *Service) Redispatch(ctx context.Context, eventID string) error {
	var rec eventRecord
	var name string
	var sourceID, identity, room, thread sql.NullString
	var catchUp int
	err := s.st.DB().QueryRowContext(ctx, `SELECT connector, COALESCE(raw_json,''), COALESCE(trace_id,''),
		source_id, identity, room, thread, catch_up FROM events WHERE id=?`, eventID).
		Scan(&name, &rec.raw, &rec.trace, &sourceID, &identity, &room, &thread, &catchUp)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrEventNotFound, eventID)
	}
	if err != nil {
		return err
	}
	rec.id = eventID
	rec.receivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.sourceID, rec.identity = sourceID.String, identity.String
	rec.room, rec.thread = room.String, thread.String
	rec.catchUp = catchUp != 0
	rec.payload = decodeStored(rec.raw)
	s.dispatchEvent(ctx, name, rec)
	return nil
}

// decodeStored decodes a stored raw_json value with numbers kept exact. A
// value that is not JSON stays a string.
func decodeStored(raw string) any {
	var v any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	return v
}

// InjectNative records one event from a native connector (email, webhook).
// The caller supplies the identity fields the connector knows natively. The
// native service owns its position row, so this never writes it.
func (s *Service) InjectNative(ctx context.Context, name string, payload any, meta NativeMeta) (string, error) {
	raw, _ := json.Marshal(payload)
	rec := eventRecord{
		id:         ulid.Make().String(),
		raw:        string(raw),
		trace:      ulid.Make().String(),
		receivedAt: time.Now().UTC().Format(time.RFC3339Nano),
		payload:    payload,
		sourceID:   meta.SourceID,
		identity:   meta.Identity,
		room:       meta.Room,
		thread:     meta.Thread,
	}
	inserted, err := s.recordEvent(ctx, name, rec, recordOpts{})
	if err != nil {
		return "", err
	}
	if inserted {
		s.dispatchEvent(ctx, name, rec)
	}
	return rec.id, nil
}

// fillEventMeta reads the declared id, identity, and channel ref paths from a
// decoded payload when a managed definition declares them.
func (s *Service) fillEventMeta(rec *eventRecord, name string) {
	d, ok := s.Definition(name)
	if !ok || d.Events == nil {
		return
	}
	if d.Events.ID != "" {
		rec.sourceID, _ = lookupPath(rec.payload, d.Events.ID)
	}
	if d.Events.Identity != "" {
		rec.identity, _ = lookupPath(rec.payload, d.Events.Identity)
	}
	if d.Channel != nil {
		rec.room, _ = lookupPath(rec.payload, d.Channel.Ref)
	}
}

// ingest decodes one output line, records it, and dispatches a new event.
// A duplicate is counted and not dispatched. A failed insert is not
// dispatched.
func (s *Service) ingest(ctx context.Context, name string, d Definition, line string, catchUp bool) {
	p := s.procByName(name)
	if d.Events == nil {
		return
	}
	payload := d.Events.decode(line)
	if payload == nil {
		if p != nil {
			p.addParseError()
		}
		s.log.Warn("connector.parse_error", "connector", name, "line", truncate(line, 200))
		return
	}
	raw, _ := json.Marshal(payload)
	rec := eventRecord{
		id:         ulid.Make().String(),
		raw:        string(raw),
		trace:      ulid.Make().String(),
		receivedAt: time.Now().UTC().Format(time.RFC3339Nano),
		payload:    payload,
		catchUp:    catchUp,
	}
	s.fillEventMeta(&rec, name)
	inserted, err := s.recordEvent(ctx, name, rec, recordOpts{advancePosition: true})
	if err != nil {
		if p != nil {
			p.addParseError()
		}
		s.log.Warn("connector.parse_error", "connector", name, "error", err.Error())
		return
	}
	if !inserted {
		if p != nil {
			p.addDuplicate()
		}
		return
	}
	if p != nil {
		p.markEvent(time.Now().UTC())
	}
	s.dispatchEvent(ctx, name, rec)
}

// recordOpts selects the side effects of recordEvent.
type recordOpts struct {
	// advancePosition moves connector_cursors to the new source id. Only
	// the supervisor ingest sets it; synthetic and native events do not.
	advancePosition bool
}

// recordEvent stores one durable events row. When the row is new, carries a
// source id, and opts.advancePosition is set, the connector cursor moves in
// the same transaction.
func (s *Service) recordEvent(ctx context.Context, name string, rec eventRecord, opts recordOpts) (inserted bool, err error) {
	summary := rec.raw
	if len(summary) > 200 {
		summary = summary[:200]
	}
	s.st.LockBus()
	err = s.st.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO events
			(id, connector, instance, received_at, summary, raw_json, trace_id, source_id, identity,
			room, thread, catch_up)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (instance, source_id) WHERE source_id IS NOT NULL DO NOTHING`,
			rec.id, name, name, rec.receivedAt, summary, rec.raw, rec.trace,
			store.NullStr(rec.sourceID), store.NullStr(rec.identity), store.NullStr(rec.room),
			store.NullStr(rec.thread), boolInt(rec.catchUp))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		inserted = n == 1
		if !inserted {
			return nil
		}
		if opts.advancePosition && rec.sourceID != "" {
			now := time.Now().UTC().Format(time.RFC3339)
			if _, err := tx.Exec(`INSERT INTO connector_cursors (instance, last_event_id, updated_at)
				VALUES (?,?,?)
				ON CONFLICT(instance) DO UPDATE SET last_event_id=excluded.last_event_id, updated_at=excluded.updated_at`,
				name, rec.sourceID, now); err != nil {
				return err
			}
		}
		_, err = store.AppendInTx(tx, store.BusEvent{
			Topic: "event.created", Entity: "event", EntityID: rec.id, Action: "created",
			Attrs: map[string]string{"connector": name},
		})
		return err
	})
	s.st.UnlockBus()
	if err != nil {
		return false, err
	}
	if inserted {
		s.st.NotifyBus()
	}
	return inserted, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// dispatchEvent matches one event against the triggers of all active jobs
// and the assistant.
func (s *Service) dispatchEvent(ctx context.Context, name string, ev eventRecord) {
	if s.ws == nil {
		return
	}
	for _, t := range s.triggersFor(ctx, name) {
		s.dispatchTrigger(ctx, name, ev, t.row, t.j, t.tr)
	}
}

func (s *Service) triggersFor(ctx context.Context, connector string) []cachedTrig {
	jobs, err := s.ws.ListJobs(ctx, "")
	if err != nil {
		return nil
	}
	// Assistant triggers come from the trebi.yaml assistant block, not the
	// job index (plan E).
	var assistant []workspaces.AssistantTriggerRow
	assistant, _ = s.ws.AssistantTriggerRows(ctx)
	var b strings.Builder
	for _, row := range jobs {
		if !row.Active {
			continue
		}
		b.WriteString(row.WorkspaceID)
		b.WriteByte('/')
		b.WriteString(row.Name)
		b.WriteByte('@')
		b.WriteString(row.Hash)
		b.WriteByte(';')
	}
	for _, at := range assistant {
		b.WriteString(at.WorkspaceID)
		b.WriteString("/assistant@")
		b.WriteString(at.Hash)
		b.WriteByte(';')
	}
	fp := b.String()
	s.trigMu.Lock()
	defer s.trigMu.Unlock()
	if fp == s.trigFP && s.trigByConn != nil {
		return s.trigByConn[connector]
	}
	by := map[string][]cachedTrig{}
	for _, row := range jobs {
		if !row.Active {
			continue
		}
		j, err := job.ParseFile(row.Path)
		if err != nil || j == nil {
			continue
		}
		for _, tr := range j.Triggers {
			if tr.Family != "event" || tr.Disabled || tr.Event == "" {
				continue
			}
			by[tr.Event] = append(by[tr.Event], cachedTrig{row: row, j: j, tr: tr})
		}
	}
	for _, at := range assistant {
		row := workspaces.JobRow{
			WorkspaceID: at.WorkspaceID, Name: "assistant",
			Kind: dispatch.KindAssistant, Active: true, Hash: at.Hash,
		}
		j := &job.Job{Name: "assistant"}
		for _, tr := range at.Triggers {
			if tr.Family != "event" || tr.Disabled || tr.Event == "" {
				continue
			}
			by[tr.Event] = append(by[tr.Event], cachedTrig{row: row, j: j, tr: tr})
		}
	}
	s.trigFP = fp
	s.trigByConn = by
	return by[connector]
}

// dispatchTrigger gates one trigger (allowlist, match) and enqueues. Policy
// is checked at the claim, and the launch records the fire.
func (s *Service) dispatchTrigger(ctx context.Context, name string, ev eventRecord, row workspaces.JobRow, j *job.Job, tr job.Trigger) {
	if s.st.IsDraining() {
		return
	}
	// Ingress allowlist (security layer 1): the declared identity is the only
	// field checked. A connector with no identity fails every allowlist.
	if len(tr.Allowlist) > 0 && !allowlistPasses(tr.Allowlist, ev.identity, name == "email") {
		s.insertOutcome(ctx, ev.id, row.WorkspaceID, j.Name, tr.Index, string(dispatch.OutcomeBlocked), "", ev.receivedAt)
		return
	}
	res := dispatch.EvalMatch(ctx, tr.Match, tr.Extract, ev.payload)
	if res.Error != "" || !res.Matched {
		outcome := string(dispatch.OutcomeNoMatch)
		if res.Error != "" {
			outcome = string(dispatch.OutcomeError)
		}
		s.insertOutcome(ctx, ev.id, row.WorkspaceID, j.Name, tr.Index, outcome, "", ev.receivedAt)
		return
	}
	item := &dispatch.QueueItem{
		Connector:   name,
		WorkspaceID: row.WorkspaceID,
		Job:         j.Name,
		Kind:        row.Kind,
		OnIndex:     tr.Index,
		EventID:     ev.id,
		EventRaw:    ev.raw,
		Vars:        res.Vars,
		Status:      dispatch.QueuePending,
		TraceID:     ev.trace,
	}
	if ck, ok := res.Vars["conversation_key"]; ok && ck != "" {
		item.ConversationKey = ck
	} else if ev.thread != "" {
		item.ConversationKey = ev.room + "/" + ev.thread
	} else if ev.room != "" {
		item.ConversationKey = ev.room
	}
	if err := s.queue.Enqueue(ctx, item); err != nil {
		s.log.Warn("enqueue", "error", err)
		return
	}
	s.insertOutcome(ctx, ev.id, row.WorkspaceID, j.Name, tr.Index, string(dispatch.OutcomeQueued), item.ID, ev.receivedAt)
}

// insertOutcome records one event_outcomes row (best-effort, logged on
// error). A redispatch replaces the earlier outcome of the same trigger.
func (s *Service) insertOutcome(ctx context.Context, eid, wsID, jobName string, onIndex int, outcome, queueID, now string) {
	var qid any
	if queueID != "" {
		qid = queueID
	}
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO event_outcomes
		(event_id, workspace_id, job, on_index, outcome, queue_id, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (event_id, workspace_id, job, on_index) DO UPDATE SET
			outcome=excluded.outcome, queue_id=excluded.queue_id, error_message=NULL, created_at=excluded.created_at`, eid, wsID, jobName, onIndex, outcome, qid, now); err != nil {
		s.log.Warn("event outcome insert", "job", jobName, "outcome", outcome, "error", err)
	}
}

// allowlistPasses compares the declared identity with the entries; the gate
// denies when no entry matches, and a connector with no identity is closed.
// An email address compares without case; every other identity is exact.
func allowlistPasses(allow []string, identity string, fold bool) bool {
	if identity == "" {
		return false
	}
	for _, a := range allow {
		if a == "" {
			continue
		}
		if a == identity || (fold && strings.EqualFold(strings.TrimSpace(a), identity)) {
			return true
		}
	}
	return false
}
