package connectors

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// CountEvent adds one new events row to the lifetime stats of its
// connection and to the kept count of its type. Call it in the transaction
// that inserts the row.
func CountEvent(ctx context.Context, tx *sql.Tx, instance, typ, at string) error {
	if err := countType(ctx, tx, instance, typ, at, false); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connection_stats (instance, events_total, first_event_at, last_event_at)
		VALUES (?,1,?,?)
		ON CONFLICT (instance) DO UPDATE SET events_total=connection_stats.events_total+1, last_event_at=excluded.last_event_at`,
		instance, at, at); err != nil {
		return fmt.Errorf("count event of %s: %w", instance, err)
	}
	return nil
}

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
	r, rerr := s.Resolve(c.ID, "")
	if rerr == nil && r.Manifest.Protocol() == manifest.ProtocolConnector {
		var ev protocol.Event
		if err := decodeStrict(raw, &ev); err != nil || ev.ID == "" || ev.Type == "" {
			return "", &ValidationError{Name: c.Slug, Fields: []FieldError{{Field: "event", Message: "must be a trebi-connector/1 event with id and type"}}}
		}
		rec = protocolRecord(c.Type, c.ID, ev, false)
		rec.root = s.root(ctx, c.ID, ev)
	} else if rerr == nil && r.Manifest.Events != nil {
		rec.text = string(raw)
		if err := fillLinesMeta(&rec, r.Manifest); err != nil {
			return "", &ValidationError{Name: c.Slug, Fields: []FieldError{{Field: "event", Message: err.Error()}}}
		}
	}
	if !c.Receive.Keeps(rec.eventType, declaredTypes(r.Manifest)) {
		return "", &ValidationError{Name: c.Slug, Fields: []FieldError{{Field: "event",
			Message: fmt.Sprintf("%s skips %s. Receive it first.", cmp.Or(c.Title, r.Title, c.Slug), cmp.Or(rec.eventType, "events of other types"))}}}
	}
	inserted, err := s.recordEvent(ctx, &rec, recordOpts{})
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
	rec, err := s.loadRecord(ctx, eventID)
	if err != nil {
		return err
	}
	rec.receivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.dispatchEvent(ctx, rec)
	return nil
}

// loadRecord reads one stored event. A stored trebi-connector/1 event gets
// its typed fields back from raw_json, and a mapped lines event from the
// current manifest. A missing id is ErrEventNotFound.
func (s *Service) loadRecord(ctx context.Context, eventID string) (eventRecord, error) {
	rec := eventRecord{id: eventID}
	var sourceID, identity, room, thread, sender, eventType, text, replyTo, root sql.NullString
	var catchUp int
	err := s.st.DB().QueryRowContext(ctx, `SELECT connector, instance, COALESCE(raw_json,''), COALESCE(trace_id,''),
		source_id, identity, room, thread, sender, event_type, catch_up, text, reply_to, root, received_at FROM events WHERE id=?`, eventID).
		Scan(&rec.connector, &rec.instance, &rec.raw, &rec.trace, &sourceID, &identity, &room, &thread, &sender, &eventType,
			&catchUp, &text, &replyTo, &root, &rec.receivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, fmt.Errorf("%w: %s", ErrEventNotFound, eventID)
	}
	if err != nil {
		return rec, err
	}
	if rec.instance == "" {
		rec.instance = rec.connector
	}
	rec.sourceID, rec.identity = sourceID.String, identity.String
	rec.room, rec.thread = room.String, thread.String
	rec.sender, rec.eventType = sender.String, eventType.String
	rec.text, rec.replyTo, rec.root = text.String, replyTo.String, root.String
	rec.catchUp = catchUp != 0
	rec.payload = decodeStored(rec.raw)
	if manifest.Reserved(rec.connector) {
		return rec, nil
	}
	var ev protocol.Event
	if decodeStrict([]byte(rec.raw), &ev) == nil && ev.ID != "" && ev.Type != "" {
		rec.fillProtocol(ev)
	} else if r, err := s.Resolve(rec.instance, ""); err == nil && r.Manifest.Protocol() == manifest.ProtocolLines && r.Manifest.Events.MapsMessage() {
		// The room name and the built-in vars have no column.
		_ = fillLinesMeta(&rec, r.Manifest) // a stored event keeps a type that the manifest dropped since
	}
	return rec, nil
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
	rec.text, rec.root = meta.Text, cmp.Or(meta.Thread, meta.SourceID)
	inserted, err := s.recordEvent(ctx, &rec, recordOpts{})
	if err != nil {
		return "", err
	}
	if inserted {
		s.dispatchEvent(ctx, rec)
	}
	return rec.id, nil
}

// fillLinesMeta reads the declared paths of a lines connector from a
// decoded payload. It fills the same fields as a trebi-connector/1 event: the
// identity is the identity path, else the sender id; the sender is the
// sender name, else the sender id. A missing path leaves its field empty, and
// a missing text path keeps the text of the record. A type that events.types
// does not declare is errUnknownType.
func fillLinesMeta(rec *eventRecord, m manifest.Manifest) error {
	ev := m.Events
	get := func(path string) string {
		v, _ := lookupPath(rec.payload, path)
		return v
	}
	senderID := get(ev.Sender)
	rec.sourceID = get(ev.ID)
	rec.identity = cmp.Or(get(ev.Identity), senderID)
	rec.room, rec.roomName, rec.thread = get(ev.Room), get(ev.RoomName), get(ev.Thread)
	rec.sender = cmp.Or(get(ev.SenderName), senderID)
	rec.eventType = get(ev.Type)
	if text, ok := lookupPath(rec.payload, ev.Text); ok {
		rec.text = text
	}
	rec.builtins = ev.MapsMessage()
	rec.root = cmp.Or(rec.thread, rec.sourceID)
	if rec.eventType != "" && !m.AcceptsType(rec.eventType) {
		return fmt.Errorf("%w: %q", errUnknownType, rec.eventType)
	}
	return nil
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
	rec.text = line
	if err := fillLinesMeta(&rec, p.res.Manifest); err != nil {
		p.addParseError()
		s.log.Warn("connector.parse_error", "connector", p.id, "reason", "unknown_type", "type", rec.eventType)
		return
	}
	if s.store(ctx, p, rec, true) && rec.room != "" {
		s.touchThread(ctx, p.id, rec.room, rec.thread, "", "")
	}
}

// protocolRecord maps a typed event to its record. The sender id is the
// identity for allowlists.
func protocolRecord(connector, instance string, ev protocol.Event, catchUp bool) eventRecord {
	raw, _ := json.Marshal(ev)
	rec := newRecord(connector, instance, string(raw), decodeStored(string(raw)))
	rec.catchUp = catchUp
	rec.fillProtocol(ev)
	rec.root = eventRoot(ev, "")
	return rec
}

// fillProtocol sets the typed fields of a trebi-connector/1 event.
func (r *eventRecord) fillProtocol(ev protocol.Event) {
	r.builtins = true
	r.sourceID, r.eventType, r.ts = ev.ID, ev.Type, ev.TS
	r.text, r.replyTo = ev.Text, ev.ReplyTo
	if ev.Room != nil {
		r.room, r.roomName, r.roomKind = ev.Room.ID, ev.Room.Name, ev.Room.Kind
	}
	if ev.Thread != nil {
		r.thread = ev.Thread.ID
	}
	if ev.Sender != nil {
		r.identity = ev.Sender.ID
		r.sender = cmp.Or(ev.Sender.Name, ev.Sender.ID)
		r.bot, r.self = ev.Sender.Bot, ev.Sender.Self
	}
}

// vars is the built-in var set of a protocol event or a mapped lines event
// (plan A.12). Other events have none.
func (r eventRecord) vars() map[string]string {
	if !r.builtins {
		return nil
	}
	return map[string]string{
		"text": r.text, "sender": r.sender, "sender_id": r.identity,
		"room": r.room, "room_name": r.roomName, "thread": r.thread,
		"message_id": r.sourceID, "reply_to": r.replyTo, "connection": r.slug,
	}
}

// conversationKey is the lane key of the event (plan A.8). With continues,
// it is <room>/<root>, or the room alone when the root is the room. Else it
// is <room>/<thread>, or the room. A threadless channel ignores the root, so
// a quote-reply does not split a group.
func (r eventRecord) conversationKey(continues bool) string {
	if continues && r.root != "" && !r.threadless {
		if r.root == r.room {
			return r.room
		}
		return r.room + "/" + r.root
	}
	if r.thread != "" {
		return r.room + "/" + r.thread
	}
	return r.room
}

// root is the conversation root of a protocol event (plan A.7). A reply to
// a stored message takes the root of that message.
func (s *Service) root(ctx context.Context, instance string, ev protocol.Event) string {
	parent := ""
	if ev.ReplyTo != "" && (ev.Thread == nil || ev.Thread.ID == "") {
		// A missing parent row is normal: the parent is older than the store.
		_ = s.st.DB().QueryRowContext(ctx, `SELECT COALESCE(root, source_id) FROM events WHERE instance=? AND source_id=?`,
			instance, ev.ReplyTo).Scan(&parent)
	}
	return eventRoot(ev, parent)
}

// eventRoot is the root rule of plan A.7. parent is the root of the stored
// message that ev replies to, or empty.
func eventRoot(ev protocol.Event, parent string) string {
	switch {
	case ev.Thread != nil && ev.Thread.ID != "":
		return ev.Thread.ID
	case ev.ReplyTo != "":
		return cmp.Or(parent, ev.ReplyTo)
	case ev.Room != nil && ev.Room.Kind == protocol.RoomDM:
		return ev.Room.ID
	}
	return ev.ID
}

// ingestProtocol records one typed event of a protocol instance (Part D.3).
// An unknown type is dropped. An event of the account itself (its message,
// its ack reaction) is stored and not dispatched.
func (s *Service) ingestProtocol(ctx context.Context, p *proc, ev protocol.Event, catchUp bool) {
	s.appendLog(p.id, "event", ev.ID+" "+ev.Type)
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
	ev.FillReaction()
	rec := protocolRecord(p.res.Type, p.id, ev, catchUp)
	rec.root = s.root(ctx, p.id, ev)
	self := ev.Sender != nil && ev.Sender.Self
	if s.store(ctx, p, rec, !self) && ev.Room != nil {
		thread, title := "", ""
		if ev.Thread != nil {
			thread, title = ev.Thread.ID, ev.Thread.Title
		}
		s.touchThread(ctx, p.id, ev.Room.ID, thread, title, ev.TS)
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
	return p.res.Manifest.AcceptsType(t)
}

// store records an ingested event, moves the connector cursor, counts it,
// and dispatches it when asked. It reports whether the row is new. An event
// that the filter of the connection skips is not stored: the cursor moves
// and the skip is counted.
func (s *Service) store(ctx context.Context, p *proc, rec eventRecord, dispatch bool) bool {
	if !s.receiveOf(p.id).Keeps(rec.eventType, declaredTypes(p.res.Manifest)) {
		if err := s.skipEvent(ctx, rec); err != nil {
			s.log.Warn("connector.skip", "connector", p.id, "type", rec.eventType, "error", err.Error())
		}
		return false
	}
	inserted, err := s.recordEvent(ctx, &rec, recordOpts{advancePosition: true})
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
// view reads threads without a call to the adapter. An empty at is now.
func (s *Service) touchThread(ctx context.Context, instance, room, thread, title, at string) {
	if at == "" {
		at = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO channel_threads (instance, room, thread, title, last_activity_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT (instance, room, thread) DO UPDATE SET last_activity_at=excluded.last_activity_at,
			title=CASE WHEN excluded.title <> '' THEN excluded.title ELSE channel_threads.title END`,
		instance, room, thread, title, at); err != nil {
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
// the unique source id drops it. The id and receive time are minted under
// the bus lock, so ids commit in order and an `after` reader skips none.
func (s *Service) recordEvent(ctx context.Context, rec *eventRecord, opts recordOpts) (inserted bool, err error) {
	name := rec.connector
	summary := rec.raw
	if len(summary) > 200 {
		summary = summary[:200]
	}
	s.st.LockBus()
	rec.id = ulid.Make().String()
	rec.receivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	err = s.st.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO events
			(id, connector, instance, received_at, summary, raw_json, trace_id, source_id, identity,
			room, thread, sender, event_type, catch_up, text, reply_to, root)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (instance, source_id) WHERE source_id IS NOT NULL DO NOTHING`,
			rec.id, name, rec.instance, rec.receivedAt, summary, rec.raw, rec.trace,
			store.NullStr(rec.sourceID), store.NullStr(rec.identity), store.NullStr(rec.room),
			store.NullStr(rec.thread), store.NullStr(rec.sender), store.NullStr(rec.eventType), boolInt(rec.catchUp),
			store.NullStr(rec.text), store.NullStr(rec.replyTo), store.NullStr(rec.root))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		inserted = n == 1
		if !inserted {
			return nil
		}
		if err := CountEvent(ctx, tx, rec.instance, rec.eventType, rec.receivedAt); err != nil {
			return err
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
		if opts.advancePosition {
			if err := advanceConnectorCursor(rec.instance, rec.sourceID); err != nil {
				return true, err
			}
		}
	}
	return inserted, nil
}

// advanceConnectorCursor moves the connector cursor of a connection to a
// source id. An empty id is no move.
func advanceConnectorCursor(instance, sourceID string) error {
	if sourceID == "" {
		return nil
	}
	cur := Cursor{LastEventID: sourceID, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := NewFolder(instance).SetCursor(cur); err != nil {
		return fmt.Errorf("connector cursor: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// dispatchEvent matches one event against the routes of its connection. A
// route names the current slug of the connection, so history survives a
// slug change. A workspace that turned the connection off or that is
// outside its scope gets no fire. A hosted webhook fires only the routes of
// the workspace of its hook.
func (s *Service) dispatchEvent(ctx context.Context, ev eventRecord) {
	if s.ws == nil {
		return
	}
	slug := s.SlugOf(ev.instance)
	if slug == "" {
		return
	}
	ev.slug = slug
	ev.threadless = s.Threadless(ev.instance)
	var hookWS string
	if ev.instance == NativeWebhook {
		hookWS, _ = lookupPath(ev.payload, "workspace_id")
	}
	for _, t := range s.triggersFor(ctx, slug) {
		if !s.visibleIn(t.row.WorkspaceID, slug) || (hookWS != "" && t.row.WorkspaceID != hookWS) {
			continue
		}
		if t.row.Kind == dispatch.KindAgent && ev.room != "" {
			// The owner leaves a room to the job that replies in it.
			if rj, ok := s.ReplyJob(ctx, t.row.WorkspaceID, slug, ev.room); ok && rj.OwnerID == t.row.ID {
				s.insertOutcome(ctx, ev, t.row, t.tr.Route, dispatch.OutcomeNoMatch, "", "room is handled by job "+rj.Name)
				continue
			}
		}
		s.dispatchTrigger(ctx, ev, t)
	}
}

// triggersFor lists the live routes of one connector.
func (s *Service) triggersFor(ctx context.Context, connector string) []cachedTrig {
	var out []cachedTrig
	for _, t := range s.triggerIndex(ctx)[connector] {
		if t.live() {
			out = append(out, t)
		}
	}
	return out
}

// RouteTrigger finds a route by id in the index of one workspace.
func (s *Service) RouteTrigger(ctx context.Context, workspaceID, routeID string) (workspaces.JobRow, job.Trigger, bool) {
	if s.ws == nil || routeID == "" {
		return workspaces.JobRow{}, job.Trigger{}, false
	}
	for _, list := range s.triggerIndex(ctx) {
		for _, t := range list {
			if t.row.WorkspaceID == workspaceID && t.tr.Route == routeID {
				return t.row, t.tr, true
			}
		}
	}
	return workspaces.JobRow{}, job.Trigger{}, false
}

// targetKey is a route target in one workspace, by short name.
type targetKey struct{ ws, name string }

// triggerIndex maps each connection slug to its routes, with inactive
// targets, broken routes, and routes that get nothing. The index is cached
// on a fingerprint of the job index and the routes files.
func (s *Service) triggerIndex(ctx context.Context) map[string][]cachedTrig {
	rows, err := s.ws.ListJobs(ctx, "")
	if err != nil {
		return nil
	}
	files, err := s.ws.RouteRows(ctx)
	if err != nil {
		return nil
	}
	fp := triggerFingerprint(rows, files)
	s.trigMu.Lock()
	defer s.trigMu.Unlock()
	if fp == s.trigFP && s.trigByConn != nil {
		return s.trigByConn
	}
	targets := make(map[targetKey]workspaces.JobRow, len(rows))
	for _, row := range rows {
		if row.Kind == dispatch.KindJob && row.OwnerID == "" {
			continue // a job outside an agent is no target
		}
		targets[targetKey{row.WorkspaceID, strings.TrimPrefix(row.Name, "agents/")}] = row
	}
	parsed := map[string]*job.Job{}
	by := map[string][]cachedTrig{}
	claims := map[roomClaim]workspaces.JobRow{}
	for _, rr := range files {
		for _, r := range rr.File.Routes {
			t := s.cacheRoute(rr, r, targets, parsed)
			if t == nil {
				continue
			}
			conn := rr.File.Connection
			by[conn] = append(by[conn], *t)
			if t.row.Kind != dispatch.KindJob || !t.active || t.broken != "" || !t.tr.RepliesInChat() {
				continue
			}
			for _, room := range t.tr.Rooms {
				// Routes are in file order, so the first route keeps a room.
				k := roomClaim{rr.WorkspaceID, conn, room}
				if _, taken := claims[k]; !taken {
					claims[k] = t.row
				}
			}
		}
	}
	s.trigFP = fp
	s.trigByConn = by
	s.claims = claims
	return by
}

// cacheRoute joins one route to its target. It returns nil for a route
// whose target is archived.
func (s *Service) cacheRoute(rr workspaces.RouteRow, r routing.Route, targets map[targetKey]workspaces.JobRow, parsed map[string]*job.Job) *cachedTrig {
	t := &cachedTrig{route: r, mode: rr.File.Mode, tr: r.Trigger(rr.File.Connection)}
	t.disabled = t.tr.Disabled
	if r.Assistant {
		t.row = workspaces.JobRow{
			ID: rr.WorkspaceID, WorkspaceID: rr.WorkspaceID, Name: routing.Assistant,
			Kind: dispatch.KindAssistant, Active: true, Hash: rr.File.Hash,
		}
		t.j, t.active = &job.Job{Name: routing.Assistant}, true
		if r.Broken() {
			t.broken = routing.CodeInvalidRoute + ": " + r.Fault
		}
		return t
	}
	row, ok := targets[targetKey{rr.WorkspaceID, r.TargetName()}]
	switch {
	case !ok:
		t.row = workspaces.JobRow{WorkspaceID: rr.WorkspaceID, Name: r.IndexName(), Kind: dispatch.KindAgent}
		if r.Job != "" {
			t.row.Kind = dispatch.KindJob
		}
		t.active = true
		t.broken = routing.CodeRouteTargetMissing + ": " + r.TargetName() + " does not exist"
	case row.Archived():
		return nil
	default:
		t.row, t.active = row, row.Active
	}
	if r.Broken() {
		t.broken = routing.CodeInvalidRoute + ": " + r.Fault
	}
	if !ok {
		return t
	}
	j, done := parsed[row.Path]
	if !done {
		var err error
		if j, err = job.ParseFile(row.Path); err != nil {
			j = nil
		}
		parsed[row.Path] = j
	}
	if j == nil && t.broken == "" {
		t.broken = routing.CodeRouteTargetMissing + ": " + r.TargetName() + " does not parse"
	}
	t.j = j
	return t
}

// ReplyJob returns the job that replies in one room of a connector, if any.
func (s *Service) ReplyJob(ctx context.Context, wsID, slug, room string) (workspaces.JobRow, bool) {
	if s.ws == nil || room == "" {
		return workspaces.JobRow{}, false
	}
	s.triggerIndex(ctx)
	s.trigMu.Lock()
	defer s.trigMu.Unlock()
	row, ok := s.claims[roomClaim{wsID, slug, room}]
	return row, ok
}

// triggerFingerprint changes when a job file, the active or archive state
// of a job, or a routes file changes.
func triggerFingerprint(jobs []workspaces.JobRow, files []workspaces.RouteRow) string {
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
		if row.Archived() {
			b.WriteString("!arch")
		}
		b.WriteByte(';')
	}
	for _, rr := range files {
		b.WriteString(rr.WorkspaceID)
		b.WriteString("/routes/")
		b.WriteString(rr.File.Connection)
		b.WriteByte('@')
		b.WriteString(rr.File.Hash)
		b.WriteByte(';')
	}
	return b.String()
}

// dispatchTrigger gates one route (room, sender, from, contains, extract,
// match) and enqueues. Policy is checked at the claim, and the launch
// records the fire. A broken route starts no run. With routing on, a chosen
// route that passes waits for a router; this build has none.
func (s *Service) dispatchTrigger(ctx context.Context, ev eventRecord, t cachedTrig) {
	if s.st.IsDraining() {
		return
	}
	row, j, tr := t.row, t.j, t.tr
	in := GateInput{Type: ev.eventType, Room: ev.room, Identity: ev.identity, Text: ev.text, Bot: ev.bot, Vars: ev.vars()}
	if t.broken != "" {
		res := GateFor(tr, ev.connector).Eval(ctx, in, ev.payload)
		if res.Matched {
			s.insertOutcome(ctx, ev, row, tr.Route, dispatch.OutcomeError, "", t.broken)
			return
		}
		s.insertOutcome(ctx, ev, row, tr.Route, failedOutcome(res), "", res.Error)
		return
	}
	continues := tr.Continues() && row.Kind == dispatch.KindJob
	key := ev.conversationKey(continues)
	if continues {
		open, err := s.conversationOpen(ctx, ev, row, j)
		if err != nil {
			s.log.Warn("conversation lookup", "job", j.Name, "error", err)
			s.insertOutcome(ctx, ev, row, tr.Route, dispatch.OutcomeError, "", "")
			return
		}
		if open && ev.bot {
			// A bot reply in an open conversation is not a follow-up (plan A.9).
			s.insertOutcome(ctx, ev, row, tr.Route, dispatch.OutcomeNoMatch, "", "")
			return
		}
		in.FollowUp = open
	}
	res := GateFor(tr, ev.connector).WithInputs(j, tr).Eval(ctx, in, ev.payload)
	if !res.Matched {
		s.insertOutcome(ctx, ev, row, tr.Route, failedOutcome(res), "", res.Error)
		return
	}
	if tr.Manual {
		// A person starts the run from the event.
		s.insertOutcome(ctx, ev, row, tr.Route, dispatch.OutcomeManual, "", "")
		return
	}
	if t.mode == routing.ModeOn && t.route.Gets == routing.GetsChosen {
		s.insertOutcome(ctx, ev, row, tr.Route, dispatch.OutcomeNoMatch, "", "routing is on and this build has no router")
		return
	}
	vars := ev.vars()
	if vars != nil || res.Vars != nil {
		vars = mergeVars(vars, res.Vars)
	}
	if ck := res.Vars["conversation_key"]; ck != "" {
		key = ck
	}
	if tr.Batched() {
		s.joinBatch(ctx, ev, row, j, tr, vars)
		return
	}
	var inputs map[string]string
	if res.Filled != nil {
		inputs = res.Filled.Declared(j.Inputs)
	}
	item := &dispatch.QueueItem{
		Connector:       ev.instance,
		WorkspaceID:     row.WorkspaceID,
		JobID:           row.ID,
		JobName:         j.Name,
		Kind:            row.Kind,
		RouteID:         tr.Route,
		ConversationKey: key,
		FollowUp:        res.FollowUp,
		EventID:         ev.id,
		EventRaw:        ev.raw,
		Vars:            vars,
		Inputs:          inputs,
		Status:          dispatch.QueuePending,
		TraceID:         ev.trace,
	}
	err := s.queue.Enqueue(ctx, item)
	if errors.Is(err, dispatch.ErrLaneFull) {
		// Drop the new event, not an old one, so the order stays correct.
		s.insertOutcome(ctx, ev, row, tr.Route, dispatch.OutcomeDropped, "", "")
		if s.onFlood != nil {
			s.onFlood(ctx, row.WorkspaceID, j.Name, ev.instance)
		}
		return
	}
	if err != nil {
		s.log.Warn("enqueue", "error", err)
		return
	}
	s.insertOutcome(ctx, ev, row, tr.Route, dispatch.OutcomeQueued, item.ID, "")
}

// failedOutcome is the outcome of an event that a gate stopped.
func failedOutcome(res GateResult) dispatch.EventOutcome {
	switch {
	case res.Error != "":
		return dispatch.OutcomeError
	case res.Stage == StageSender:
		// The sender gate is security layer 1; a room miss is scope only.
		return dispatch.OutcomeBlocked
	}
	return dispatch.OutcomeNoMatch
}

// joinBatch adds a matched event to the batch row of its route. The row
// takes the trace id of its first event; each event keeps its own trace on
// its outcome row.
func (s *Service) joinBatch(ctx context.Context, ev eventRecord, row workspaces.JobRow, j *job.Job, tr job.Trigger, vars map[string]string) {
	item := &dispatch.QueueItem{
		Connector:       ev.instance,
		WorkspaceID:     row.WorkspaceID,
		JobID:           row.ID,
		JobName:         j.Name,
		Kind:            row.Kind,
		RouteID:         tr.Route,
		ConversationKey: dispatch.BatchLaneKey(tr.Route),
		EventID:         ev.id,
		Vars:            vars,
		TraceID:         ev.trace,
	}
	queueID, err := s.queue.JoinBatch(ctx, item, tr.BatchWindow(), s.batchClock)
	if errors.Is(err, dispatch.ErrLaneFull) {
		s.insertOutcome(ctx, ev, row, tr.Route, dispatch.OutcomeDropped, "", "")
		if s.onFlood != nil {
			s.onFlood(ctx, row.WorkspaceID, j.Name, ev.instance)
		}
		return
	}
	if err != nil {
		s.log.Warn("batch join", "job", j.Name, "error", err)
		s.insertOutcome(ctx, ev, row, tr.Route, dispatch.OutcomeError, "", "")
		return
	}
	s.insertOutcome(ctx, ev, row, tr.Route, dispatch.OutcomeQueued, queueID, "")
}

// conversationOpen reports whether r continues an open conversation of a
// job with replies: continue (plan A.9). A start message never does. On a
// threadless channel, only a DM has follow-ups.
func (s *Service) conversationOpen(ctx context.Context, r eventRecord, row workspaces.JobRow, j *job.Job) (bool, error) {
	if s.convs == nil || r.root == "" || r.root == r.sourceID || (r.threadless && r.roomKind != protocol.RoomDM) {
		return false, nil
	}
	within := time.Duration(j.LaunchDefaults().ContinueMS) * time.Millisecond
	return s.convs.ConversationOpen(ctx, row.WorkspaceID, row.ID, r.conversationKey(true), within)
}

// mergeVars copies base, then over on top: an extract var wins over a
// built-in var.
func mergeVars(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	maps.Copy(out, base)
	maps.Copy(out, over)
	return out
}

// RecapLine is one stored message of a conversation.
type RecapLine struct {
	EventID string
	Sender  string
	Text    string
	Self    bool
	At      time.Time
}

// Recap returns the last n stored messages with this root, oldest first.
func (s *Service) Recap(ctx context.Context, instance, root string, n int) ([]RecapLine, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, COALESCE(sender,''), COALESCE(text,''), COALESCE(raw_json,''), received_at
		FROM events WHERE instance=? AND root=? ORDER BY id DESC LIMIT ?`, instance, root, n)
	if err != nil {
		return nil, fmt.Errorf("recap %s: %w", root, err)
	}
	defer rows.Close()
	var out []RecapLine
	for rows.Next() {
		var l RecapLine
		var raw, at string
		if err := rows.Scan(&l.EventID, &l.Sender, &l.Text, &raw, &at); err != nil {
			return nil, err
		}
		l.At, _ = time.Parse(time.RFC3339Nano, at) // a bad time reads as zero
		var ev protocol.Event
		if json.Unmarshal([]byte(raw), &ev) == nil && ev.Sender != nil {
			l.Self = ev.Sender.Self
			if t, err := time.Parse(time.RFC3339Nano, ev.TS); err == nil {
				l.At = t
			}
		}
		out = append(out, l)
	}
	slices.Reverse(out)
	return out, rows.Err()
}

// RecordHandRun points the outcome row of an event and a route at the
// queue row of a run that a person started from the event. The outcome
// stays manual; a reader follows the queue row.
func (s *Service) RecordHandRun(ctx context.Context, eventID, wsID, jobID, routeID, queueID string) {
	s.upsertOutcome(ctx, eventID, wsID, jobID, routeID, dispatch.OutcomeManual, queueID, "", time.Now().UTC().Format(time.RFC3339Nano))
}

// insertOutcome records one event_outcomes row (best-effort, logged on
// error). A redispatch replaces the earlier outcome of the same route.
func (s *Service) insertOutcome(ctx context.Context, ev eventRecord, row workspaces.JobRow, routeID string, outcome dispatch.EventOutcome, queueID, msg string) {
	s.upsertOutcome(ctx, ev.id, row.WorkspaceID, row.ID, routeID, outcome, queueID, msg, ev.receivedAt)
}

// upsertOutcome writes the outcome row of one event and one route.
func (s *Service) upsertOutcome(ctx context.Context, eid, wsID, jobID, routeID string, outcome dispatch.EventOutcome, queueID, msg, now string) {
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO event_outcomes
		(event_id, workspace_id, job_id, route_id, outcome, queue_id, error_message, created_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (event_id, workspace_id, job_id, route_id) DO UPDATE SET
			outcome=excluded.outcome, queue_id=excluded.queue_id, error_message=excluded.error_message,
			created_at=excluded.created_at`,
		eid, wsID, jobID, routeID, string(outcome), store.NullStr(queueID), store.NullStr(msg), now); err != nil {
		s.log.Warn("event outcome insert", "job_id", jobID, "outcome", outcome, "error", err)
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
