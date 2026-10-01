package connectors

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
// ingest path of the connection. A protocol connector takes a typed event.
// It refuses a reserved name. A source id that is already stored returns
// ErrDuplicate.
func (s *Service) InjectEvent(ctx context.Context, ref string, event any) (string, error) {
	if err := s.GuardReserved(ref); err != nil {
		return "", err
	}
	if err := s.GuardMutate(ref); err != nil {
		return "", err
	}
	c, err := s.Lookup(ref)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	rec := newRecord(c.Type, c.ID, string(raw), event)
	if r, err := s.Resolve(c.ID, ""); err == nil && r.Manifest.Protocol() == manifest.ProtocolConnector {
		var ev protocol.Event
		if err := decodeStrict(raw, &ev); err != nil || ev.ID == "" || ev.Type == "" {
			return "", &ValidationError{Name: c.Slug, Fields: []FieldError{{Field: "event", Message: "must be a trebi-connector/1 event with id and type"}}}
		}
		rec = protocolRecord(c.Type, c.ID, ev, false)
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
	s.dispatchEvent(ctx, rec)
	return rec.id, nil
}

// newRecord builds an event record with fresh ids. connector is the type
// and instance the connection id.
func newRecord(connector, instance, raw string, payload any) eventRecord {
	return eventRecord{
		id:         ulid.Make().String(),
		connector:  connector,
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
	var sourceID, identity, room, thread, sender, eventType sql.NullString
	var catchUp int
	err := s.st.DB().QueryRowContext(ctx, `SELECT connector, instance, COALESCE(raw_json,''), COALESCE(trace_id,''),
		source_id, identity, room, thread, sender, event_type, catch_up FROM events WHERE id=?`, eventID).
		Scan(&rec.connector, &rec.instance, &rec.raw, &rec.trace, &sourceID, &identity, &room, &thread, &sender, &eventType, &catchUp)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrEventNotFound, eventID)
	}
	if err != nil {
		return err
	}
	if rec.instance == "" {
		rec.instance = rec.connector
	}
	rec.id = eventID
	rec.receivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.sourceID, rec.identity = sourceID.String, identity.String
	rec.room, rec.thread = room.String, thread.String
	rec.sender, rec.eventType = sender.String, eventType.String
	rec.catchUp = catchUp != 0
	rec.payload = decodeStored(rec.raw)
	s.dispatchEvent(ctx, rec)
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
	rec := newRecord(name, name, string(raw), payload)
	rec.sourceID, rec.identity = meta.SourceID, meta.Identity
	rec.room, rec.thread = meta.Room, meta.Thread
	rec.sender, rec.eventType = meta.Sender, meta.Type
	inserted, err := s.recordEvent(ctx, rec, recordOpts{})
	if err != nil {
		return "", err
	}
	if inserted {
		s.dispatchEvent(ctx, rec)
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
		s.log.Warn("connector.parse_error", "connector", p.id, "line", truncate(line, 200))
		return
	}
	raw, _ := json.Marshal(payload)
	rec := newRecord(p.res.Type, p.id, string(raw), payload)
	rec.catchUp = catchUp
	fillLinesMeta(&rec, ev)
	s.store(ctx, p, rec, true)
}

// protocolRecord maps a typed event to its record. The sender id is the
// identity for allowlists.
func protocolRecord(connector, instance string, ev protocol.Event, catchUp bool) eventRecord {
	raw, _ := json.Marshal(ev)
	rec := newRecord(connector, instance, string(raw), decodeStored(string(raw)))
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
	appendConnectorLog(p.id, "event", ev.ID+" "+ev.Type)
	if ev.ID == "" || ev.Type == "" {
		p.addParseError()
		s.log.Warn("connector.parse_error", "connector", p.id, "reason", "missing_id_or_type")
		return
	}
	if !p.knowsType(ev.Type) {
		p.addParseError()
		s.log.Warn("connector.parse_error", "connector", p.id, "reason", "unknown_type", "type", ev.Type)
		return
	}
	rec := protocolRecord(p.res.Type, p.id, ev, catchUp)
	self := ev.Type == manifest.EventMessage && ev.Sender != nil && ev.Sender.Self
	if s.store(ctx, p, rec, !self) && ev.Room != nil {
		s.touchThread(ctx, p.id, ev)
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
		s.log.Warn("connector.parse_error", "connector", p.id, "error", err.Error())
		return false
	}
	if !inserted {
		p.addDuplicate()
		return false
	}
	p.markEvent(time.Now().UTC())
	if dispatch {
		s.dispatchEvent(ctx, rec)
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
	// advancePosition moves the connector cursor to the new source id.
	// Only the supervisor ingest sets it; synthetic and native events do
	// not.
	advancePosition bool
}

// recordEvent stores one durable events row. When the row is new, carries a
// source id, and opts.advancePosition is set, the connector cursor moves
// after the commit. A crash between the two replays at most one event, and
// the unique source id drops it.
func (s *Service) recordEvent(ctx context.Context, rec eventRecord, opts recordOpts) (inserted bool, err error) {
	name := rec.connector
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
		if opts.advancePosition && rec.sourceID != "" {
			cur := Cursor{LastEventID: rec.sourceID, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
			if err := NewFolder(rec.instance).SetCursor(cur); err != nil {
				return true, fmt.Errorf("connector cursor: %w", err)
			}
		}
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
// and the assistant. A trigger names the current slug of the connection, so
// history survives a slug change. A workspace that turned the connection off
// gets no fire.
func (s *Service) dispatchEvent(ctx context.Context, ev eventRecord) {
	if s.ws == nil {
		return
	}
	slug := s.SlugOf(ev.instance)
	if slug == "" {
		return
	}
	for _, t := range s.triggersFor(ctx, slug) {
		if s.disabledIn(t.row.WorkspaceID, slug) {
			continue
		}
		s.dispatchTrigger(ctx, ev, t.row, t.j, t.tr)
	}
}

// triggersFor lists the live event triggers of one connector.
func (s *Service) triggersFor(ctx context.Context, connector string) []cachedTrig {
	var out []cachedTrig
	for _, t := range s.triggerIndex(ctx)[connector] {
		if t.live() {
			out = append(out, t)
		}
	}
	return out
}

// triggerIndex maps each connector to its event triggers, inactive jobs and
// disabled triggers included. The index is cached on a fingerprint of the
// job index and the assistant blocks.
func (s *Service) triggerIndex(ctx context.Context) map[string][]cachedTrig {
	jobs, err := s.ws.ListJobs(ctx, "")
	if err != nil {
		return nil
	}
	// Assistant triggers come from the trebi.yaml assistant block, not the
	// job index (plan E).
	var assistant []workspaces.AssistantTriggerRow
	assistant, _ = s.ws.AssistantTriggerRows(ctx)
	fp := triggerFingerprint(jobs, assistant)
	s.trigMu.Lock()
	defer s.trigMu.Unlock()
	if fp == s.trigFP && s.trigByConn != nil {
		return s.trigByConn
	}
	by := map[string][]cachedTrig{}
	for _, row := range jobs {
		j, err := job.ParseFile(row.Path)
		if err != nil || j == nil {
			continue
		}
		for _, tr := range j.Triggers {
			if tr.Family != "event" || tr.Event == "" {
				continue
			}
			by[tr.Event] = append(by[tr.Event], cachedTrig{row: row, j: j, tr: tr, active: row.Active, disabled: tr.Disabled})
		}
	}
	for _, at := range assistant {
		row := workspaces.JobRow{
			WorkspaceID: at.WorkspaceID, Name: "assistant",
			Kind: dispatch.KindAssistant, Active: true, Hash: at.Hash,
		}
		j := &job.Job{Name: "assistant"}
		for _, tr := range at.Triggers {
			if tr.Family != "event" || tr.Event == "" {
				continue
			}
			by[tr.Event] = append(by[tr.Event], cachedTrig{row: row, j: j, tr: tr, active: true, disabled: tr.Disabled})
		}
	}
	s.trigFP = fp
	s.trigByConn = by
	return by
}

// triggerFingerprint changes when a job file, the active flag of a job, or
// an assistant block changes.
func triggerFingerprint(jobs []workspaces.JobRow, assistant []workspaces.AssistantTriggerRow) string {
	var b strings.Builder
	for _, row := range jobs {
		b.WriteString(row.WorkspaceID)
		b.WriteByte('/')
		b.WriteString(row.Name)
		b.WriteByte('@')
		b.WriteString(row.Hash)
		if !row.Active {
			b.WriteString("!off")
		}
		b.WriteByte(';')
	}
	for _, at := range assistant {
		b.WriteString(at.WorkspaceID)
		b.WriteString("/assistant@")
		b.WriteString(at.Hash)
		b.WriteByte(';')
	}
	return b.String()
}

// dispatchTrigger gates one trigger (room, sender, extract, match) and
// enqueues. Policy is checked at the claim, and the launch records the fire.
func (s *Service) dispatchTrigger(ctx context.Context, ev eventRecord, row workspaces.JobRow, j *job.Job, tr job.Trigger) {
	if s.st.IsDraining() {
		return
	}
	res := GateFor(tr, ev.connector).Eval(ctx, ev.room, ev.identity, ev.payload)
	if !res.Matched {
		outcome := dispatch.OutcomeNoMatch
		switch {
		case res.Error != "":
			outcome = dispatch.OutcomeError
		case res.Stage == StageSender:
			// The sender gate is security layer 1; a room miss is scope only.
			outcome = dispatch.OutcomeBlocked
		}
		s.insertOutcome(ctx, ev.id, row.WorkspaceID, j.Name, tr.Index, string(outcome), "", ev.receivedAt)
		return
	}
	item := &dispatch.QueueItem{
		Connector:   ev.instance,
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
	err := s.queue.Enqueue(ctx, item)
	if errors.Is(err, dispatch.ErrLaneFull) {
		// Drop the new event, not an old one, so the order stays correct.
		s.insertOutcome(ctx, ev.id, row.WorkspaceID, j.Name, tr.Index, string(dispatch.OutcomeDropped), "", ev.receivedAt)
		if s.onFlood != nil {
			s.onFlood(ctx, row.WorkspaceID, j.Name, ev.instance)
		}
		return
	}
	if err != nil {
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

// decodeLine parses one lines-protocol output line per the declared format.
// A json connector returns nil for a line that is not valid json.
func decodeLine(e *manifest.Events, line string) any {
	if e.Format == manifest.FormatJSON {
		// UseNumber keeps a large id exact for the id and identity paths.
		var m any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil || dec.More() {
			return nil
		}
		return m
	}
	return map[string]any{"line": line}
}

// lookupPath reads a dotted path from a decoded JSON value. A numeric
// segment indexes an array. It renders strings, numbers, and booleans for
// comparison; anything else misses.
func lookupPath(v any, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	cur := v
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return "", false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return "", false
			}
			cur = node[i]
		default:
			return "", false
		}
	}
	return identityString(cur), identityString(cur) != ""
}

// identityString renders a scalar for comparison; "" means absent or not a
// scalar.
func identityString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return ""
}
