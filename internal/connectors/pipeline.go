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
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// InjectEvent handles a synthetic event (`connector test`) through the same
// ingest path of the global instance. A protocol connector takes a typed
// event. It refuses a reserved name. A source id that is already stored
// returns ErrDuplicate.
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
	rec := newRecord(name, string(raw), event)
	if r, err := s.Resolve(name, ""); err == nil && r.Manifest.Protocol() == manifest.ProtocolConnector {
		var ev protocol.Event
		if err := decodeStrict(raw, &ev); err != nil || ev.ID == "" || ev.Type == "" {
			return "", &ValidationError{Name: name, Fields: []FieldError{{Field: "event", Message: "must be a trebi-connector/1 event with id and type"}}}
		}
		rec = protocolRecord(name, ev, false)
	} else if err == nil && r.Manifest.Events != nil {
		fillLinesMeta(&rec, r.Manifest.Events)
	}
	inserted, err := s.recordEvent(ctx, rec, recordOpts{})
	if err != nil {
		return "", err
	}
	if !inserted {
		return "", fmt.Errorf("%w: %q", ErrDuplicate, rec.sourceID)
	}
	s.dispatchEvent(ctx, name, rec)
	return rec.id, nil
}

// newRecord builds an event record with fresh ids.
func newRecord(instance, raw string, payload any) eventRecord {
	return eventRecord{
		id:         ulid.Make().String(),
		instance:   instance,
		raw:        raw,
		trace:      ulid.Make().String(),
		receivedAt: time.Now().UTC().Format(time.RFC3339Nano),
		payload:    payload,
	}
}

// Redispatch matches one stored event against the current triggers again.
// It writes no new events row, so it works for every connector, native ones
// included, and for an event whose source id is already stored.
func (s *Service) Redispatch(ctx context.Context, eventID string) error {
	var rec eventRecord
	var name string
	var sourceID, identity, room, thread, sender, eventType sql.NullString
	var catchUp int
	err := s.st.DB().QueryRowContext(ctx, `SELECT connector, instance, COALESCE(raw_json,''), COALESCE(trace_id,''),
		source_id, identity, room, thread, sender, event_type, catch_up FROM events WHERE id=?`, eventID).
		Scan(&name, &rec.instance, &rec.raw, &rec.trace, &sourceID, &identity, &room, &thread, &sender, &eventType, &catchUp)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrEventNotFound, eventID)
	}
	if err != nil {
		return err
	}
	if rec.instance == "" {
		rec.instance = name
	}
	rec.id = eventID
	rec.receivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.sourceID, rec.identity = sourceID.String, identity.String
	rec.room, rec.thread = room.String, thread.String
	rec.sender, rec.eventType = sender.String, eventType.String
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
	rec := newRecord(name, string(raw), payload)
	rec.sourceID, rec.identity = meta.SourceID, meta.Identity
	rec.room, rec.thread = meta.Room, meta.Thread
	rec.sender, rec.eventType = meta.Sender, meta.Type
	inserted, err := s.recordEvent(ctx, rec, recordOpts{})
	if err != nil {
		return "", err
	}
	if inserted {
		s.dispatchEvent(ctx, name, rec)
	}
	return rec.id, nil
}

// fillLinesMeta reads the declared id and identity paths of a lines
// connector from a decoded payload.
func fillLinesMeta(rec *eventRecord, ev *manifest.Events) {
	rec.sourceID, _ = lookupPath(rec.payload, ev.ID)
	rec.identity, _ = lookupPath(rec.payload, ev.Identity)
}

// ingest decodes one lines output line, records it, and dispatches a new
// event. A duplicate is counted and not dispatched. A failed insert is not
// dispatched.
func (s *Service) ingest(ctx context.Context, p *proc, line string, catchUp bool) {
	ev := p.res.Manifest.Events
	payload := decodeLine(ev, line)
	if payload == nil {
		p.addParseError()
		s.log.Warn("connector.parse_error", "connector", p.instance, "line", truncate(line, 200))
		return
	}
	raw, _ := json.Marshal(payload)
	rec := newRecord(p.instance, string(raw), payload)
	rec.catchUp = catchUp
	fillLinesMeta(&rec, ev)
	s.store(ctx, p, rec, true)
}

// protocolRecord maps a typed event to its record. The sender id is the
// identity for allowlists.
func protocolRecord(instance string, ev protocol.Event, catchUp bool) eventRecord {
	raw, _ := json.Marshal(ev)
	rec := newRecord(instance, string(raw), decodeStored(string(raw)))
	rec.sourceID, rec.eventType, rec.catchUp = ev.ID, ev.Type, catchUp
	if ev.Room != nil {
		rec.room = ev.Room.ID
	}
	if ev.Thread != nil {
		rec.thread = ev.Thread.ID
	}
	if ev.Sender != nil {
		rec.identity = ev.Sender.ID
		rec.sender = ev.Sender.Name
		if rec.sender == "" {
			rec.sender = ev.Sender.ID
		}
	}
	return rec
}

// ingestProtocol records one typed event of a protocol instance (Part D.3).
// An unknown type is dropped. A message from the account itself is stored
// and not dispatched.
func (s *Service) ingestProtocol(ctx context.Context, p *proc, ev protocol.Event, catchUp bool) {
	appendConnectorLog(p.instance, "event", ev.ID+" "+ev.Type)
	if ev.ID == "" || ev.Type == "" {
		p.addParseError()
		s.log.Warn("connector.parse_error", "connector", p.instance, "reason", "missing_id_or_type")
		return
	}
	if !p.knowsType(ev.Type) {
		p.addParseError()
		s.log.Warn("connector.parse_error", "connector", p.instance, "reason", "unknown_type", "type", ev.Type)
		return
	}
	rec := protocolRecord(p.instance, ev, catchUp)
	self := ev.Type == manifest.EventMessage && ev.Sender != nil && ev.Sender.Self
	if s.store(ctx, p, rec, !self) && ev.Room != nil {
		s.touchThread(ctx, p.instance, ev)
	}
}

// knowsType reports whether the session or the manifest declares the type.
func (p *proc) knowsType(t string) bool {
	_, init := p.session()
	if init != nil && len(init.Events) > 0 {
		for _, d := range init.Events {
			if d.Type == t {
				return true
			}
		}
		return false
	}
	m := p.res.Manifest
	return len(m.Events.Types) == 0 || m.HasEventType(t)
}

// store records an ingested event, moves the connector cursor, counts it,
// and dispatches it when asked. It reports whether the row is new.
func (s *Service) store(ctx context.Context, p *proc, rec eventRecord, dispatch bool) bool {
	inserted, err := s.recordEvent(ctx, rec, recordOpts{advancePosition: true})
	if err != nil {
		p.addParseError()
		s.log.Warn("connector.parse_error", "connector", p.instance, "error", err.Error())
		return false
	}
	if !inserted {
		p.addDuplicate()
		return false
	}
	p.markEvent(time.Now().UTC())
	if dispatch {
		s.dispatchEvent(ctx, p.res.Name, rec)
	}
	return true
}

// touchThread upserts the channel_threads row of an event, so the rooms
// view reads threads without a call to the adapter.
func (s *Service) touchThread(ctx context.Context, instance string, ev protocol.Event) {
	thread, title := "", ""
	if ev.Thread != nil {
		thread, title = ev.Thread.ID, ev.Thread.Title
	}
	at := ev.TS
	if at == "" {
		at = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO channel_threads (instance, room, thread, title, last_activity_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT (instance, room, thread) DO UPDATE SET last_activity_at=excluded.last_activity_at,
			title=CASE WHEN excluded.title <> '' THEN excluded.title ELSE channel_threads.title END`,
		instance, ev.Room.ID, thread, title, at); err != nil {
		s.log.Warn("channel thread upsert", "connector", instance, "error", err)
	}
}

// KnownThreads lists the threads the daemon saw in a room, newest first.
// Email writes them too, so one reader serves every channel.
func (s *Service) KnownThreads(ctx context.Context, instance, room string, limit int) ([]protocol.Thread, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT thread, title, participants_json, COALESCE(last_activity_at,'')
		FROM channel_threads WHERE instance=? AND room=? ORDER BY last_activity_at DESC LIMIT ?`, instance, room, limit)
	if err != nil {
		return nil, fmt.Errorf("threads of %s: %w", instance, err)
	}
	defer rows.Close()
	out := []protocol.Thread{}
	for rows.Next() {
		var t protocol.Thread
		var parts string
		if err := rows.Scan(&t.ID, &t.Title, &parts, &t.LastActivityAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(parts), &t.Participants) // a bad row reads as no participants
		out = append(out, t)
	}
	return out, rows.Err()
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
func (s *Service) recordEvent(ctx context.Context, rec eventRecord, opts recordOpts) (inserted bool, err error) {
	name, _ := splitInstance(rec.instance)
	summary := rec.raw
	if len(summary) > 200 {
		summary = summary[:200]
	}
	s.st.LockBus()
	err = s.st.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO events
			(id, connector, instance, received_at, summary, raw_json, trace_id, source_id, identity,
			room, thread, sender, event_type, catch_up)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (instance, source_id) WHERE source_id IS NOT NULL DO NOTHING`,
			rec.id, name, rec.instance, rec.receivedAt, summary, rec.raw, rec.trace,
			store.NullStr(rec.sourceID), store.NullStr(rec.identity), store.NullStr(rec.room),
			store.NullStr(rec.thread), store.NullStr(rec.sender), store.NullStr(rec.eventType), boolInt(rec.catchUp))
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
				rec.instance, rec.sourceID, now); err != nil {
				return err
			}
		}
		_, err = store.AppendInTx(tx, store.BusEvent{
			Topic: "event.created", Entity: "event", EntityID: rec.id, Action: "created",
			Attrs: map[string]string{"connector": name, "instance": rec.instance},
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
// and the assistant. A trigger fires only in the workspaces that the event
// instance serves.
func (s *Service) dispatchEvent(ctx context.Context, name string, ev eventRecord) {
	if s.ws == nil {
		return
	}
	for _, t := range s.triggersFor(ctx, name) {
		if ev.instance != "" && s.InstanceFor(name, t.row.WorkspaceID) != ev.instance {
			continue
		}
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
