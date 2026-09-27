package connectors

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// InjectEvent handles a synthetic event (`connector test`, retrigger, rules
// fire) through the same ingest path. It refuses a reserved name.
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
	inserted, err := s.recordEvent(ctx, name, rec)
	if err != nil {
		return "", err
	}
	if inserted {
		s.dispatchEvent(ctx, name, rec)
	}
	return rec.id, nil
}

// InjectNative records one event from a native connector (email, webhook).
// The caller supplies the identity fields the connector knows natively.
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
		channelRef: meta.ChannelRef,
	}
	inserted, err := s.recordEvent(ctx, name, rec)
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
		rec.channelRef, _ = lookupPath(rec.payload, d.Channel.Ref)
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
	inserted, err := s.recordEvent(ctx, name, rec)
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

// recordEvent stores one durable events row. When the row is new and carries
// a source id, the connector cursor moves in the same transaction.
func (s *Service) recordEvent(ctx context.Context, name string, rec eventRecord) (inserted bool, err error) {
	summary := rec.raw
	if len(summary) > 200 {
		summary = summary[:200]
	}
	s.st.LockBus()
	err = s.st.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO events
			(id, connector, received_at, summary, raw_json, trace_id, source_id, identity, channel_ref, catch_up)
			VALUES (?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (connector, source_id) WHERE source_id IS NOT NULL DO NOTHING`,
			rec.id, name, rec.receivedAt, summary, rec.raw, rec.trace,
			store.NullStr(rec.sourceID), store.NullStr(rec.identity), store.NullStr(rec.channelRef), boolInt(rec.catchUp))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		inserted = n == 1
		if !inserted {
			return nil
		}
		if rec.sourceID != "" {
			now := time.Now().UTC().Format(time.RFC3339)
			if _, err := tx.Exec(`INSERT INTO connector_cursors (name, last_event_id, updated_at)
				VALUES (?,?,?)
				ON CONFLICT(name) DO UPDATE SET last_event_id=excluded.last_event_id, updated_at=excluded.updated_at`,
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
			if tr.Family != "event" && tr.Event == "" {
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
	if len(tr.Allowlist) > 0 && !allowlistPasses(tr.Allowlist, ev.identity) {
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
	} else if ev.channelRef != "" {
		item.ConversationKey = ev.channelRef
	}
	if err := s.queue.Enqueue(ctx, item); err != nil {
		s.log.Warn("enqueue", "error", err)
		return
	}
	s.insertOutcome(ctx, ev.id, row.WorkspaceID, j.Name, tr.Index, string(dispatch.OutcomeQueued), item.ID, ev.receivedAt)
}

// insertOutcome records one event_outcomes row (best-effort, logged on error).
func (s *Service) insertOutcome(ctx context.Context, eid, wsID, jobName string, onIndex int, outcome, queueID, now string) {
	var qid any
	if queueID != "" {
		qid = queueID
	}
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO event_outcomes
		(event_id, workspace_id, job, on_index, outcome, queue_id, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (event_id, workspace_id, job, on_index) DO NOTHING`, eid, wsID, jobName, onIndex, outcome, qid, now); err != nil {
		s.log.Warn("event outcome insert", "job", jobName, "outcome", outcome, "error", err)
	}
}

// allowlistPasses compares the declared identity with the entries; the gate
// denies when no entry matches, and a connector with no identity is closed.
func allowlistPasses(allow []string, identity string) bool {
	if identity == "" {
		return false
	}
	for _, a := range allow {
		if a != "" && a == identity {
			return true
		}
	}
	return false
}
