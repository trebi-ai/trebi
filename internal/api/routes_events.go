package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// eventRow is one scanned events table row.
type eventRow struct {
	id, connector, at, sum, trace, raw string
	instance, eventType, sourceID      string
	identity, room, thread, sender     string
	slug                               string
	catchUp                            bool
}

// base is the shared row fields of the list and get shapes.
func (e eventRow) base() map[string]any {
	return map[string]any{
		"id": e.id, "connector": e.connector, "at": e.at, "received_at": e.at,
		"summary": e.sum, "trace_id": e.trace,
		"instance": e.instance, "event_type": e.eventType, "source_id": e.sourceID,
		"identity": e.identity, "room": e.room, "thread": e.thread, "sender": e.sender,
		"catch_up": e.catchUp, "slug": e.slug,
	}
}

// queueResult is the queue state joined into event outcomes.
type queueResult struct {
	status, result, runID string
}

//nolint:dupl // route tables stay explicit and colocated with area handlers.
func (s *Server) routeEvents(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/rules/test", s.handleRulesTest)
	mux.HandleFunc("POST /v1/rules/test-batch", s.handleRulesTestBatch)
	mux.HandleFunc("POST /v1/rules/fire", s.handleRulesFire)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("GET /v1/events/{id}", s.handleEventGet)
	mux.HandleFunc("GET /v1/events/{id}/jobs", s.handleEventJobs)
	mux.HandleFunc("POST /v1/events/{id}/retrigger", s.handleEventRetrigger)
}

// enrichEventOutcomes loads event_outcomes for eventIDs and derives wire outcome
// from queue status when a queue_id is present (B15/B18). A set workspace drops
// outcomes of other workspaces; a set outcome drops other outcomes, so listed
// rows carry the filtered value only.
func (s *Server) enrichEventOutcomes(ctx context.Context, workspace, outcome string, eventIDs []string) map[string][]map[string]any {
	out := make(map[string][]map[string]any, len(eventIDs))
	if len(eventIDs) == 0 {
		return out
	}
	placeholders, args := inClause(eventIDs)
	q := `SELECT event_id, workspace_id, job_id, ` + store.JobNameSQL("event_outcomes.job_id") + `, route_id, outcome, COALESCE(error_message,''), COALESCE(queue_id,'')
		FROM event_outcomes WHERE event_id IN (` + placeholders + `) ORDER BY event_id, route_id`
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	type raw struct {
		eid, ws, jobID, jobName, outcome, detail, qid string
		routeID                                       string
	}
	var raws []raw
	for rows.Next() {
		var r raw
		if rows.Scan(&r.eid, &r.ws, &r.jobID, &r.jobName, &r.routeID, &r.outcome, &r.detail, &r.qid) == nil {
			if workspace != "" && r.ws != workspace {
				continue
			}
			if outcome != "" && (r.outcome != outcome || (outcome == string(dispatch.OutcomeManual) && r.qid != "")) {
				continue
			}
			raws = append(raws, r)
		}
	}
	// Collect queue ids for join
	qids := make([]string, 0)
	for _, r := range raws {
		if r.qid != "" {
			qids = append(qids, r.qid)
		}
	}
	qinfo := s.queueInfoByID(ctx, qids)
	for _, r := range raws {
		entry := map[string]any{
			"workspace": r.ws,
			"job_id":    r.jobID,
			"job_name":  r.jobName,
			"route_id":  r.routeID,
			"outcome":   r.outcome,
		}
		if r.detail != "" {
			entry["outcome_detail"] = r.detail
		}
		if r.qid != "" {
			entry["queue_id"] = r.qid
			if r.outcome == string(dispatch.OutcomeManual) {
				// A person started the run; the queue row has its state.
				entry["by"] = string(dispatch.OutcomeManual)
			}
			if qi, ok := qinfo[r.qid]; ok {
				// Derive wire outcome from queue state at read time (B15)
				entry["outcome"] = deriveOutcomeFromQueue(r.outcome, qi)
				entry["queue"] = qi
				if runID, _ := qi["run_id"].(string); runID != "" {
					entry["run_id"] = runID
				}
			}
		}
		out[r.eid] = append(out[r.eid], entry)
	}
	return out
}

func (s *Server) queueInfoByID(ctx context.Context, ids []string) map[string]map[string]any {
	out := make(map[string]map[string]any)
	if len(ids) == 0 {
		return out
	}
	ph, args := inClause(ids)
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT id, status, COALESCE(block_reason,''), attempts, COALESCE(run_id,''), COALESCE(result,''), next_attempt_at
		FROM event_queue WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, status, br, runID, result string
		var attempts int
		var next sql.NullString
		if rows.Scan(&id, &status, &br, &attempts, &runID, &result, &next) != nil {
			continue
		}
		m := map[string]any{
			"status":   status,
			"attempts": attempts,
		}
		if br != "" {
			m["block_reason"] = br
		}
		if runID != "" {
			m["run_id"] = runID
		}
		if result != "" {
			m["result"] = result
		}
		if next.Valid && next.String != "" {
			if t, err := time.Parse(time.RFC3339Nano, next.String); err == nil {
				m["next_attempt_at"] = t.UTC().Format(time.RFC3339Nano)
			} else {
				m["next_attempt_at"] = next.String
			}
		}
		out[id] = m
	}
	return out
}

// eventFilter is the shared filter set of GET /v1/events. A connection slug
// or id sets instance; any other connector value sets connType.
type eventFilter struct {
	instance, connType, since, workspace, outcome, catchUp, text string
	// acceptsJob is the job id of ?accepts=: an event that a trigger of
	// the job took.
	acceptsJob string
}

func (s *Server) parseEventFilter(r *http.Request) (eventFilter, error) {
	q := r.URL.Query()
	since, err := parseTimeFilter(q.Get("since"))
	if err != nil {
		return eventFilter{}, fmt.Errorf("since: %w", err)
	}
	f := eventFilter{
		since:     since,
		workspace: s.workspaceID(r.Context(), q.Get("workspace")),
		outcome:   q.Get("outcome"),
		catchUp:   q.Get("catch_up"),
		text:      strings.TrimSpace(q.Get("q")),
	}
	if f.catchUp != "" && f.catchUp != "0" && f.catchUp != "1" {
		return eventFilter{}, errors.New("catch_up must be 0 or 1")
	}
	if ref := q.Get("accepts"); ref != "" {
		ws, name, ok := strings.Cut(ref, "/")
		if !ok {
			return eventFilter{}, errors.New("accepts must be <workspace>/<job>")
		}
		row, err := s.findJobRow(r.Context(), s.workspaceID(r.Context(), ws), name)
		if err != nil {
			return eventFilter{}, fmt.Errorf("accepts: %w", err)
		}
		f.acceptsJob = row.ID
	}
	if connector := q.Get("connector"); connector != "" {
		if id := s.connectors.IDOf(connector); id != "" {
			f.instance = id
		} else {
			f.connType = connector
		}
	}
	return f, nil
}

// where returns the SQL conditions of the filter, each with a leading AND.
func (f eventFilter) where() (string, []any) {
	var b strings.Builder
	var args []any
	add := func(cond string, vals ...any) {
		b.WriteString(" AND " + cond)
		args = append(args, vals...)
	}
	if f.instance != "" {
		add("instance = ?", f.instance)
	}
	if f.connType != "" {
		add("connector = ?", f.connType)
	}
	if f.since != "" {
		add("received_at >= ?", f.since)
	}
	if f.catchUp != "" {
		n, _ := strconv.Atoi(f.catchUp) // parse checked it: 0 or 1
		add("catch_up = ?", n)
	}
	// Workspace/outcome ride event_outcomes: an event with no outcome row is
	// global and shows only when no workspace is set (A.3.2).
	if f.workspace != "" || f.outcome != "" {
		var conds []string
		var vals []any
		if f.workspace != "" {
			conds = append(conds, "o.workspace_id = ?")
			vals = append(vals, f.workspace)
		}
		if f.outcome != "" {
			conds = append(conds, "o.outcome = ?")
			vals = append(vals, f.outcome)
			if f.outcome == string(dispatch.OutcomeManual) {
				conds = append(conds, "o.queue_id IS NULL")
			}
		}
		add(`EXISTS (SELECT 1 FROM event_outcomes o WHERE o.event_id = events.id AND `+strings.Join(conds, " AND ")+`)`, vals...)
	}
	if f.acceptsJob != "" {
		add(`EXISTS (SELECT 1 FROM event_outcomes a WHERE a.event_id = events.id AND a.job_id = ? AND a.outcome <> ?)`,
			f.acceptsJob, string(dispatch.OutcomeNoMatch))
	}
	if f.text != "" {
		like := "%" + strings.ToLower(f.text) + "%"
		add(`(LOWER(COALESCE(summary,'')) LIKE ? OR LOWER(COALESCE(raw_json,'')) LIKE ?)`, like, like)
	}
	return b.String(), args
}

// queryEvents reads one page of filtered events. Forward pages read id > from
// in ascending order; backward pages read id < from, newest first.
func (s *Server) queryEvents(ctx context.Context, f eventFilter, from string, forward bool, limit int) ([]eventRow, error) {
	q := `SELECT id, connector, received_at, COALESCE(summary,''), COALESCE(trace_id,''), COALESCE(raw_json,''),
		instance, COALESCE(event_type,''), COALESCE(source_id,''), COALESCE(identity,''),
		COALESCE(room,''), COALESCE(thread,''), COALESCE(sender,''), catch_up
		FROM events WHERE 1=1`
	var args []any
	cmp, order := "<", "DESC"
	if forward {
		cmp, order = ">", "ASC"
	}
	if from != "" {
		q += ` AND id ` + cmp + ` ?`
		args = append(args, from)
	}
	cond, condArgs := f.where()
	q += cond + ` ORDER BY id ` + order + ` LIMIT ?`
	args = append(append(args, condArgs...), limit)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evs []eventRow
	for rows.Next() {
		var e eventRow
		if rows.Scan(&e.id, &e.connector, &e.at, &e.sum, &e.trace, &e.raw,
			&e.instance, &e.eventType, &e.sourceID, &e.identity, &e.room, &e.thread, &e.sender, &e.catchUp) == nil {
			e.slug = s.connectors.SlugOf(e.instance)
			evs = append(evs, e)
		}
	}
	return evs, rows.Err()
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	f, err := s.parseEventFilter(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	q := r.URL.Query()
	if q.Has("after") || q.Get("follow") == "1" {
		s.handleEventsAfter(w, r, f)
		return
	}
	cursor, ok := parseULIDCursor(w, r, "event")
	if !ok {
		return
	}
	limit := parseLimit(r)
	evs, err := s.queryEvents(r.Context(), f, cursor, false, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	ids := make([]string, 0, len(evs))
	for _, e := range evs {
		ids = append(ids, e.id)
	}
	outcomesByEvent := s.enrichEventOutcomes(r.Context(), f.workspace, f.outcome, ids)
	s.patchDoneResults(r.Context(), outcomesByEvent)

	// List: one entry per outcome (event × job), collapse all-no_match to single
	list := []map[string]any{}
	for _, e := range evs {
		list = append(list, e.listEntries(outcomesByEvent[e.id])...)
	}
	resp := map[string]any{"events": list}
	if len(evs) == limit {
		resp["next_cursor"] = evs[len(evs)-1].id
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleEventsAfter reads events forward from ?after= in id order. Event ids
// are ULIDs, so id order is time order. Each row has the GET /v1/events/{id}
// shape. With follow=1 the response is an NDJSON stream that stays open.
func (s *Server) handleEventsAfter(w http.ResponseWriter, r *http.Request, f eventFilter) {
	q := r.URL.Query()
	after := q.Get("after")
	if q.Has("cursor") {
		writeErr(w, http.StatusBadRequest, "validation", "use after or cursor, not both")
		return
	}
	if after != "" {
		if _, err := ulid.Parse(after); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", "malformed after: not an event id")
			return
		}
	}
	limit := parseLimit(r)
	if q.Get("follow") == "1" {
		s.followEvents(w, r, f, after, limit)
		return
	}
	rows, next, err := s.eventsAfter(r.Context(), f, after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": rows, "next_after": next})
}

// eventsAfter returns up to limit events with id > after, oldest first, and
// the id to read after next. With no rows, the next id is after.
func (s *Server) eventsAfter(ctx context.Context, f eventFilter, after string, limit int) ([]map[string]any, string, error) {
	evs, err := s.queryEvents(ctx, f, after, true, limit)
	if err != nil {
		return nil, after, err
	}
	ids := make([]string, 0, len(evs))
	for _, e := range evs {
		ids = append(ids, e.id)
	}
	outs := s.enrichEventOutcomes(ctx, f.workspace, f.outcome, ids)
	s.patchDoneResults(ctx, outs)
	rows := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		rows = append(rows, e.detail(outs[e.id]))
		after = e.id
	}
	return rows, after, nil
}

// eventKeepalive is the NDJSON line that a follow stream writes while idle.
// It has no id, so a reader can tell it from an event row.
const eventKeepalive = "{\"keepalive\":true}\n"

// followEvents writes events with id > after as NDJSON lines until the client
// disconnects or the server stops. A bus append wakes it; the poll tick also
// catches rows written with no bus frame.
func (s *Server) followEvents(w http.ResponseWriter, r *http.Request, f eventFilter, after string, limit int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// WebKit holds a fetch until the first body byte.
	fmt.Fprint(w, eventKeepalive)
	flusher.Flush()

	enc := json.NewEncoder(w)
	lastWrite := time.Now()
	lag := time.NewTicker(streamPollInterval)
	defer lag.Stop()
	for {
		wake := s.st.BusWake()
		rows, next, err := s.eventsAfter(r.Context(), f, after, limit)
		if err != nil {
			if r.Context().Err() == nil {
				s.log.Warn("events.follow", "error", err)
			}
			return
		}
		for _, row := range rows {
			if err := enc.Encode(row); err != nil {
				return
			}
		}
		after = next
		switch {
		case len(rows) > 0:
			flusher.Flush()
			lastWrite = time.Now()
			if len(rows) == limit {
				continue
			}
		case time.Since(lastWrite) >= streamKeepalive:
			fmt.Fprint(w, eventKeepalive)
			flusher.Flush()
			lastWrite = time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-wake:
		case <-lag.C:
		}
	}
}

// listEntries expands one event into list rows: one row per interesting
// outcome, a single collapsed row when every outcome is no_match.
func (e eventRow) listEntries(outs []map[string]any) []map[string]any {
	base := e.base()
	if len(outs) == 0 {
		base["outcomes"] = []any{}
		return []map[string]any{base}
	}
	allNoMatch := true
	for _, o := range outs {
		if o["outcome"] != string(dispatch.OutcomeNoMatch) {
			allNoMatch = false
			break
		}
	}
	if allNoMatch {
		base["outcome"] = string(dispatch.OutcomeNoMatch)
		base["outcomes"] = outs
		return []map[string]any{base}
	}
	var list []map[string]any
	for _, o := range outs {
		if o["outcome"] == string(dispatch.OutcomeNoMatch) {
			continue // skip pure no_match when mixed (list is per interesting outcome)
		}
		row := e.base()
		row["workspace"] = o["workspace"]
		row["job_id"] = o["job_id"]
		row["job_name"] = o["job_name"]
		row["route_id"] = o["route_id"]
		row["outcome"] = o["outcome"]
		if qid, ok := o["queue_id"]; ok {
			row["queue_id"] = qid
		}
		if rid, ok := o["run_id"]; ok {
			row["run_id"] = rid
		}
		if q, ok := o["queue"]; ok {
			row["queue"] = q
		}
		list = append(list, row)
	}
	return list
}

func (s *Server) patchDoneResults(ctx context.Context, outcomesByEvent map[string][]map[string]any) {
	qids := collectQueueIDs(outcomesByEvent)
	if len(qids) == 0 {
		return
	}
	byID := s.queueResultsByID(ctx, qids)
	for eid, outs := range outcomesByEvent {
		for i, o := range outs {
			qid, _ := o["queue_id"].(string)
			q, ok := byID[qid]
			if !ok {
				continue
			}
			q.patchInto(o)
			outcomesByEvent[eid][i] = o
		}
	}
}

// patchInto overwrites one outcome entry with the current queue state.
func (q queueResult) patchInto(o map[string]any) {
	qi, _ := o["queue"].(map[string]any)
	if qi == nil {
		qi = map[string]any{}
	}
	qi["status"] = q.status
	if q.result != "" {
		qi["result"] = q.result
	}
	if q.runID != "" {
		qi["run_id"] = q.runID
		o["run_id"] = q.runID
	}
	o["queue"] = qi
	o["outcome"] = deriveOutcomeFromQueueWithResult(q.status, q.result, fmtSprint(o["outcome"]))
}

// collectQueueIDs gathers the queue ids referenced by the outcome entries.
func collectQueueIDs(outcomesByEvent map[string][]map[string]any) []string {
	var qids []string
	for _, outs := range outcomesByEvent {
		for _, o := range outs {
			if qid, ok := o["queue_id"].(string); ok && qid != "" {
				qids = append(qids, qid)
			}
		}
	}
	return qids
}

func (s *Server) queueResultsByID(ctx context.Context, qids []string) map[string]queueResult {
	byID := map[string]queueResult{}
	ph, args := inClause(qids)
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id, status, COALESCE(result,''), COALESCE(run_id,'') FROM event_queue WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return byID
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var q queueResult
		if rows.Scan(&id, &q.status, &q.result, &q.runID) == nil {
			byID[id] = q
		}
	}
	return byID
}

// inClause builds "?,?,..." placeholders and args for a SQL IN clause.
func inClause(ids []string) (string, []any) {
	args := make([]any, len(ids))
	ph := ""
	for i, id := range ids {
		args[i] = id
		if i > 0 {
			ph += ","
		}
		ph += "?"
	}
	return ph, args
}

// deriveOutcomeFromQueue maps queue status → wire EventOutcome for post-enqueue rows.
// pending→queued, blocked→blocked, claimed→queued, done→result, dead→dead.
func deriveOutcomeFromQueue(stored string, qi map[string]any) string {
	status, _ := qi["status"].(string)
	result, _ := qi["result"].(string)
	return deriveOutcomeFromQueueWithResult(status, result, stored)
}

func deriveOutcomeFromQueueWithResult(status, result, stored string) string {
	switch dispatch.QueueStatus(status) {
	case dispatch.QueuePending, dispatch.QueueClaimed:
		return string(dispatch.OutcomeQueued)
	case dispatch.QueueBlocked:
		return string(dispatch.OutcomeBlocked)
	case dispatch.QueueDone:
		if result != "" {
			return result // launched|coalesced|skipped|dead
		}
		if stored != "" && stored != string(dispatch.OutcomeQueued) {
			return stored
		}
		return string(dispatch.OutcomeLaunched)
	case dispatch.QueueDead:
		return string(dispatch.OutcomeDead)
	default:
		if stored != "" {
			return stored
		}
		return string(dispatch.OutcomeQueued)
	}
}

func fmtSprint(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *Server) handleEventGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	e := eventRow{id: id}
	err := s.st.DB().QueryRowContext(r.Context(), `
		SELECT connector, received_at, COALESCE(summary,''), COALESCE(raw_json,''), COALESCE(trace_id,''),
		instance, COALESCE(event_type,''), COALESCE(source_id,''), COALESCE(identity,''),
		COALESCE(room,''), COALESCE(thread,''), COALESCE(sender,''), catch_up
		FROM events WHERE id=?`, id).Scan(&e.connector, &e.at, &e.sum, &e.raw, &e.trace,
		&e.instance, &e.eventType, &e.sourceID, &e.identity, &e.room, &e.thread, &e.sender, &e.catchUp)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "event not found")
		return
	}
	e.slug = s.connectors.SlugOf(e.instance)
	outcomesBy := s.enrichEventOutcomes(r.Context(), "", "", []string{id})
	s.patchDoneResults(r.Context(), outcomesBy)
	writeJSON(w, http.StatusOK, e.detail(outcomesBy[id]))
}

// handleEventJobs lists the job triggers on the connection of a stored
// event, with the gate result of each for a run that a person starts. It
// runs the gates in memory and writes nothing.
func (s *Server) handleEventJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ev, err := s.connectors.StoredGateEvent(ctx, r.PathValue("id"))
	if err != nil {
		s.writeEventErr(w, err)
		return
	}
	outs := s.enrichEventOutcomes(ctx, "", "", []string{ev.ID})[ev.ID]
	s.patchDoneResults(ctx, map[string][]map[string]any{ev.ID: outs})
	jobs := []map[string]any{}
	for _, t := range s.connectors.EventTriggers(ctx, ev) {
		res := ev.RunGate(ctx, t.Job, t.Trigger, false)
		entry := map[string]any{
			"workspace": t.Row.WorkspaceID, "job": t.Row.Name, "title": t.Job.Title,
			"route_id": t.Trigger.Route, "manual": t.Trigger.Manual,
			"matched": res.Matched, "stage": res.Stage, "asks": []string{}, "ready": false,
		}
		if res.Error != "" {
			entry["error"] = res.Error
		}
		if f := res.Filled; f != nil {
			entry["asks"] = append([]string{}, f.Missing...)
			entry["ready"] = res.Matched && len(f.Missing) == 0 && len(f.Errors) == 0
		}
		for _, o := range outs {
			if o["job_id"] == t.Row.ID && o["route_id"] == t.Trigger.Route {
				entry["outcome"] = o["outcome"]
				if id, ok := o["run_id"]; ok {
					entry["run_id"] = id
				}
			}
		}
		jobs = append(jobs, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// detail is the GET /v1/events/{id} shape: the base fields, the raw payload,
// and every outcome.
func (e eventRow) detail(outs []map[string]any) map[string]any {
	if outs == nil {
		outs = []map[string]any{}
	}
	out := e.base()
	if e.raw != "" {
		out["raw"] = json.RawMessage(e.raw)
	} else {
		out["raw"] = nil
	}
	out["outcomes"] = outs
	return out
}

func (s *Server) handleEventRetrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.connectors.Redispatch(r.Context(), id); err != nil {
		s.writeEventErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retrigged": true, "event_id": id})
}

// maxRulesTestBatch caps the events of one rules/test-batch call.
const maxRulesTestBatch = 50

// ruleGate is the trigger side of a rules test body.
type ruleGate struct {
	Match     string   `json:"match"`
	Extract   string   `json:"extract"`
	Rooms     []string `json:"rooms"`
	Allowlist []string `json:"allowlist"`
	From      string   `json:"from"`
	Contains  []string `json:"contains"`
	Replies   string   `json:"replies"`
	// Batch is the batch window of the trigger. A match waits for the next
	// tick of the window.
	Batch string `json:"batch"`
	// Workspace and Job name the job of the trigger. With replies: continue,
	// an event of an open conversation of that job is a follow-up. A job
	// also fills its inputs from a matched event, with its route RouteID,
	// else its first route.
	Workspace string `json:"workspace"`
	Job       string `json:"job"`
	RouteID   string `json:"route_id"`
}

// check rejects a batch value that a trigger cannot hold.
func (g ruleGate) check() error {
	_, err := job.ParseBatch(g.Batch)
	return err
}

func (g ruleGate) gate() connectors.Gate {
	return connectors.Gate{
		Rooms: g.Rooms, Allowlist: g.Allowlist, From: g.From, Contains: g.Contains,
		Match: g.Match, Extract: g.Extract,
	}
}

// result is the wire form of one gate result. With a job, a match adds the
// filled inputs and the missing required inputs.
func (g ruleGate) result(ev connectors.GateEvent, res connectors.GateResult, j *job.Job, tr *job.Trigger) map[string]any {
	vars := res.Vars
	if res.Matched {
		vars = ev.Vars(res.Vars)
	}
	if vars == nil {
		vars = map[string]string{}
	}
	out := map[string]any{
		"matched": res.Matched, "stage": res.Stage, "vars": vars,
		"extracted": res.Extracted, "error": res.Error, "follow_up": res.FollowUp,
		"conversation_key": ev.ConversationKey(g.Replies == job.RepliesContinue, res.Vars),
	}
	if g.Batch != "" && g.Batch != job.BatchOff {
		out["batch"] = g.Batch
	}
	if j != nil && res.Matched {
		f := j.Fill(tr, &job.EventVars{Vars: vars, Payload: ev.Payload}, nil)
		out["inputs"], out["missing"] = f.Wire(), append([]string{}, f.Missing...)
	}
	return out
}

// ruleTrigger is the trigger of the route RouteID of the job of a rules
// test body, else the first route of the job; nil when there is none.
func (s *Server) ruleTrigger(ctx context.Context, g ruleGate, row workspaces.JobRow) *job.Trigger {
	if g.Job == "" {
		return nil
	}
	if g.RouteID != "" {
		if _, tr, ok := s.connectors.RouteTrigger(ctx, row.WorkspaceID, g.RouteID); ok {
			return &tr
		}
		return nil
	}
	routes, _ := s.ws.RoutesOf(ctx, row.WorkspaceID, row.Name)
	for _, lr := range routes {
		tr := lr.Route.Trigger(lr.Connection)
		return &tr
	}
	return nil
}

// ruleJob loads the job of a rules test body. Nil means no job: every event
// is the start of a conversation, and no inputs fill.
func (s *Server) ruleJob(ctx context.Context, g ruleGate) (*job.Job, workspaces.JobRow, error) {
	if g.Job == "" {
		return nil, workspaces.JobRow{}, nil
	}
	row, err := s.findJobRow(ctx, g.Workspace, g.Job)
	if err != nil {
		return nil, workspaces.JobRow{}, err
	}
	if row.Kind != dispatch.KindJob {
		return nil, workspaces.JobRow{}, nil
	}
	doc, err := readIndexedDoc(row.Path)
	if err != nil {
		return nil, workspaces.JobRow{}, err
	}
	doc.Job.Name = row.Name
	return doc.Job, row, nil
}

// evalRule runs the gate against ev. With a job and replies: continue, ev
// is a follow-up when it continues an open conversation of that job.
func (s *Server) evalRule(ctx context.Context, ev connectors.GateEvent, g ruleGate, j *job.Job, row workspaces.JobRow) (connectors.GateResult, error) {
	followUp := false
	if j != nil && g.Replies == job.RepliesContinue {
		var err error
		if followUp, err = s.connectors.FollowUp(ctx, ev, row, j); err != nil {
			return connectors.GateResult{}, err
		}
	}
	res := ev.Eval(ctx, g.gate(), followUp)
	res.FollowUp = followUp
	return res, nil
}

// handleRulesTest runs the gate of one trigger against a stored event
// (event_id) or a draft event (event).
func (s *Server) handleRulesTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ruleGate
		Event     any    `json:"event"`
		EventID   string `json:"event_id"`
		Connector string `json:"connector"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.EventID != "" && body.Event != nil {
		writeErr(w, http.StatusBadRequest, "validation", "set event_id or event, not both")
		return
	}
	if err := body.check(); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	j, jobRow, err := s.ruleJob(r.Context(), body.ruleGate)
	if err != nil {
		writeResolveErr(w, err, "job")
		return
	}
	var ev connectors.GateEvent
	if body.EventID != "" {
		var err error
		if ev, err = s.connectors.StoredGateEvent(r.Context(), body.EventID); err != nil {
			s.writeEventErr(w, err)
			return
		}
	} else {
		typ := body.Connector
		if c, err := s.connectors.Lookup(body.Connector); err == nil {
			typ = c.Type
		}
		ev = connectors.DraftGateEvent(typ, s.connectors.Threadless(body.Connector), body.Event)
	}
	res, err := s.evalRule(r.Context(), ev, body.ruleGate, j, jobRow)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := body.result(ev, res, j, s.ruleTrigger(r.Context(), body.ruleGate, jobRow))
	out["room"], out["identity"] = ev.Room, ev.Identity
	writeJSON(w, http.StatusOK, out)
}

// handleRulesTestBatch runs the gate of one trigger against up to 50 stored
// events, in order. A missing event is a result with an error.
func (s *Server) handleRulesTestBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ruleGate
		EventIDs []string `json:"event_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	switch {
	case len(body.EventIDs) == 0:
		writeErr(w, http.StatusBadRequest, "validation", "event_ids is required")
		return
	case len(body.EventIDs) > maxRulesTestBatch:
		writeErr(w, http.StatusBadRequest, "validation", fmt.Sprintf("event_ids: at most %d events", maxRulesTestBatch))
		return
	}
	if err := body.check(); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	j, jobRow, err := s.ruleJob(r.Context(), body.ruleGate)
	if err != nil {
		writeResolveErr(w, err, "job")
		return
	}
	tr := s.ruleTrigger(r.Context(), body.ruleGate, jobRow)
	results := make([]map[string]any, 0, len(body.EventIDs))
	for _, id := range body.EventIDs {
		var out map[string]any
		ev, err := s.connectors.StoredGateEvent(r.Context(), id)
		switch {
		case errors.Is(err, connectors.ErrEventNotFound):
			out = body.result(connectors.GateEvent{}, connectors.GateResult{Error: "event not found"}, nil, nil)
		case err != nil:
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		default:
			res, err := s.evalRule(r.Context(), ev, body.ruleGate, j, jobRow)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			out = body.result(ev, res, j, tr)
		}
		out["event_id"] = id
		results = append(results, out)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// writeEventErr writes 404 for a missing event, else 500.
func (s *Server) writeEventErr(w http.ResponseWriter, err error) {
	if errors.Is(err, connectors.ErrEventNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "event not found")
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal", err.Error())
}

func (s *Server) handleRulesFire(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Connector string `json:"connector"`
		Workspace string `json:"workspace"`
		Job       string `json:"job"`
		Match     string `json:"match"`
		Extract   string `json:"extract"`
		Event     any    `json:"event"`
		RouteID   string `json:"route_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.Connector == "" {
		body.Connector = "rules-fire"
	}
	// No job records the event only: it starts no run, and a later
	// retrigger can route it.
	var row workspaces.JobRow
	if body.Job != "" {
		var err error
		row, err = s.findIndexRow(r.Context(), body.Workspace, body.Job, func(row workspaces.JobRow) bool {
			return row.Name == body.Job
		}, "job", "agent")
		if err != nil {
			writeResolveErr(w, err, "job")
			return
		}
		if aerr := archivedLaunchErr(row); aerr != nil {
			writeLaunchErr(w, aerr)
			return
		}
	}
	res := dispatch.EvalMatch(r.Context(), body.Match, body.Extract, body.Event)
	if res.Error != "" {
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "error", "error": res.Error})
		return
	}
	if !res.Matched {
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "no_match"})
		return
	}
	raw, err := json.Marshal(body.Event)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	// Durable event row (same pipeline as connector handleLine)
	trace := ulid.Make().String()
	summary := string(raw)
	if len(summary) > 200 {
		summary = summary[:200]
	}
	// A connection keeps its type on the event row and its id on the queue.
	typ, instance := body.Connector, body.Connector
	if c, err := s.connectors.Lookup(body.Connector); err == nil {
		typ, instance = c.Type, c.ID
	}
	// Mint the id under the bus lock, so ids commit in order for `after` readers.
	s.st.LockBus()
	eid := ulid.Make().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err = s.st.WithTx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO events (id, connector, instance, received_at, summary, raw_json, trace_id)
			VALUES (?,?,?,?,?,?,?)`, eid, typ, instance, now, summary, string(raw), trace); err != nil {
			return err
		}
		return connectors.CountEvent(r.Context(), tx, instance, "", now)
	})
	s.st.UnlockBus()
	if err != nil {
		s.log.Warn("rules.fire event insert", "event_id", eid, "error", err)
	} else {
		s.st.NotifyBus()
	}
	if body.Job == "" {
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "recorded", "event_id": eid})
		return
	}

	item := &dispatch.QueueItem{
		Connector:   instance,
		WorkspaceID: row.WorkspaceID,
		JobID:       row.ID,
		JobName:     row.Name,
		Kind:        row.Kind,
		RouteID:     body.RouteID,
		EventID:     eid,
		EventRaw:    string(raw),
		Vars:        connectors.DraftGateEvent(typ, false, body.Event).Vars(res.Vars),
		Status:      dispatch.QueuePending,
		TraceID:     trace,
	}
	// Same rule as the connector pipeline: the extract names the lane.
	if ck := res.Vars["conversation_key"]; ck != "" {
		item.ConversationKey = ck
	}
	err = s.queue.Enqueue(r.Context(), item)
	if errors.Is(err, dispatch.ErrLaneFull) {
		if _, err := s.st.DB().ExecContext(r.Context(), `INSERT INTO event_outcomes
			(event_id, workspace_id, job_id, route_id, outcome, created_at)
			VALUES (?,?,?,?,?,?)
			ON CONFLICT (event_id, workspace_id, job_id, route_id) DO NOTHING`, eid, row.WorkspaceID, row.ID, body.RouteID,
			string(dispatch.OutcomeDropped), now); err != nil {
			s.log.Warn("rules.fire outcome insert", "event_id", eid, "error", err)
		}
		writeQueueFull(w)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if _, err := s.st.DB().ExecContext(r.Context(), `INSERT INTO event_outcomes
		(event_id, workspace_id, job_id, route_id, outcome, queue_id, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (event_id, workspace_id, job_id, route_id) DO NOTHING`, eid, row.WorkspaceID, row.ID, body.RouteID,
		string(dispatch.OutcomeQueued), item.ID, now); err != nil {
		s.log.Warn("rules.fire outcome insert", "event_id", eid, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"outcome": "queued", "queue_id": item.ID, "event_id": eid,
	})
}
