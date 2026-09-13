package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/dispatch"
)

// eventRow is one scanned events table row.
type eventRow struct {
	id, connector, at, sum, trace, raw string
}

// queueResult is the queue state joined into event outcomes.
type queueResult struct {
	status, result, runID string
}

//nolint:dupl // route tables stay explicit and colocated with area handlers.
func (s *Server) routeEvents(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/rules/test", s.handleRulesTest)
	mux.HandleFunc("POST /v1/rules/fire", s.handleRulesFire)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("GET /v1/events/{id}", s.handleEventGet)
	mux.HandleFunc("POST /v1/events/{id}/retrigger", s.handleEventRetrigger)
}

// enrichEventOutcomes loads event_outcomes for eventIDs and derives wire outcome
// from queue status when a queue_id is present (B15/B18).
func (s *Server) enrichEventOutcomes(ctx context.Context, eventIDs []string) map[string][]map[string]any {
	out := make(map[string][]map[string]any, len(eventIDs))
	if len(eventIDs) == 0 {
		return out
	}
	placeholders, args := inClause(eventIDs)
	q := `SELECT event_id, workspace_id, job, on_index, outcome, COALESCE(error_message,''), COALESCE(queue_id,'')
		FROM event_outcomes WHERE event_id IN (` + placeholders + `) ORDER BY event_id, on_index`
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	type raw struct {
		eid, ws, job, outcome, detail, qid string
		idx                                int
	}
	var raws []raw
	for rows.Next() {
		var r raw
		if rows.Scan(&r.eid, &r.ws, &r.job, &r.idx, &r.outcome, &r.detail, &r.qid) == nil {
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
			"job":       r.job,
			"on_index":  r.idx,
			"outcome":   r.outcome,
		}
		if r.detail != "" {
			entry["outcome_detail"] = r.detail
		}
		if r.qid != "" {
			entry["queue_id"] = r.qid
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

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	cursor, ok := parseULIDCursor(w, r, "event")
	if !ok {
		return
	}
	limit := parseLimit(r)
	since, err := parseTimeFilter(r.URL.Query().Get("since"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "since: "+err.Error())
		return
	}
	q := `SELECT id, connector, received_at, COALESCE(summary,''), COALESCE(trace_id,''), COALESCE(raw_json,'')
		FROM events WHERE 1=1`
	args := []any{}
	if cursor != "" {
		q += ` AND id < ?`
		args = append(args, cursor)
	}
	if connector := r.URL.Query().Get("connector"); connector != "" {
		q += ` AND connector = ?`
		args = append(args, connector)
	}
	if since != "" {
		q += ` AND received_at >= ?`
		args = append(args, since)
	}
	if text := strings.TrimSpace(r.URL.Query().Get("q")); text != "" {
		like := "%" + strings.ToLower(text) + "%"
		q += ` AND (LOWER(COALESCE(summary,'')) LIKE ? OR LOWER(COALESCE(raw_json,'')) LIKE ?)`
		args = append(args, like, like)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.st.DB().QueryContext(r.Context(), q, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer rows.Close()
	var evs []eventRow
	var ids []string
	for rows.Next() {
		var e eventRow
		if rows.Scan(&e.id, &e.connector, &e.at, &e.sum, &e.trace, &e.raw) == nil {
			evs = append(evs, e)
			ids = append(ids, e.id)
		}
	}
	// Enrich queue results with result column for done rows
	outcomesByEvent := s.enrichEventOutcomes(r.Context(), ids)
	// Patch done outcomes with actual result from queue
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

// listEntries expands one event into list rows: one row per interesting
// outcome, a single collapsed row when every outcome is no_match.
func (e eventRow) listEntries(outs []map[string]any) []map[string]any {
	base := map[string]any{
		"id": e.id, "connector": e.connector, "at": e.at, "received_at": e.at,
		"summary": e.sum, "trace_id": e.trace,
	}
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
		row := map[string]any{
			"id": e.id, "connector": e.connector, "at": e.at, "received_at": e.at,
			"summary": e.sum, "trace_id": e.trace,
			"workspace": o["workspace"], "job": o["job"], "on_index": o["on_index"],
			"outcome": o["outcome"],
		}
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
	var connector, at, sum, raw, trace string
	err := s.st.DB().QueryRowContext(r.Context(), `
		SELECT connector, received_at, COALESCE(summary,''), COALESCE(raw_json,''), COALESCE(trace_id,'')
		FROM events WHERE id=?`, id).Scan(&connector, &at, &sum, &raw, &trace)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "event not found")
		return
	}
	outcomesBy := s.enrichEventOutcomes(r.Context(), []string{id})
	s.patchDoneResults(r.Context(), outcomesBy)
	outs := outcomesBy[id]
	if outs == nil {
		outs = []map[string]any{}
	}
	var rawMsg any
	if raw != "" {
		rawMsg = json.RawMessage(raw)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "connector": connector, "at": at, "received_at": at, "summary": sum,
		"raw": rawMsg, "trace_id": trace, "outcomes": outs,
	})
}

func (s *Server) handleEventRetrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var connector, raw string
	err := s.st.DB().QueryRowContext(r.Context(), `SELECT connector, COALESCE(raw_json,'') FROM events WHERE id=?`, id).
		Scan(&connector, &raw)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "event not found")
		return
	}
	var event any
	_ = json.Unmarshal([]byte(raw), &event) // retrigger still fires with a raw string if json is corrupt
	_, err = s.connectors.InjectEvent(r.Context(), connector, event)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retrigged": true, "event_id": id})
}

func (s *Server) handleRulesTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Match   string `json:"match"`
		Extract string `json:"extract"`
		Event   any    `json:"event"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	res := dispatch.EvalMatch(r.Context(), body.Match, body.Extract, body.Event)
	writeJSON(w, http.StatusOK, map[string]any{
		"matched": res.Matched, "vars": res.Vars, "error": res.Error,
	})
}

// indexRowKind returns job_index.kind for (workspace, name). Empty when the row
// is absent; the queue then defaults the lane to job.
func (s *Server) indexRowKind(ctx context.Context, wsID, name string) string {
	var kind string
	_ = s.st.DB().QueryRowContext(ctx,
		`SELECT kind FROM job_index WHERE workspace_id=? AND name=?`,
		s.workspaceID(ctx, wsID), name).Scan(&kind)
	return kind
}

func (s *Server) handleRulesFire(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Connector string `json:"connector"`
		Workspace string `json:"workspace"`
		Job       string `json:"job"`
		Match     string `json:"match"`
		Extract   string `json:"extract"`
		Event     any    `json:"event"`
		OnIndex   int    `json:"on_index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if body.Connector == "" {
		body.Connector = "rules-fire"
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
	eid := ulid.Make().String()
	trace := ulid.Make().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	summary := string(raw)
	if len(summary) > 200 {
		summary = summary[:200]
	}
	if _, err := s.st.DB().ExecContext(r.Context(), `INSERT INTO events (id, connector, received_at, summary, raw_json, trace_id)
		VALUES (?,?,?,?,?,?)`, eid, body.Connector, now, summary, string(raw), trace); err != nil {
		s.log.Warn("rules.fire event insert", "event_id", eid, "error", err)
	}

	item := &dispatch.QueueItem{
		Connector:   body.Connector,
		WorkspaceID: body.Workspace,
		Job:         body.Job,
		Kind:        s.indexRowKind(r.Context(), body.Workspace, body.Job),
		OnIndex:     body.OnIndex,
		EventID:     eid,
		EventRaw:    string(raw),
		Vars:        res.Vars,
		Status:      dispatch.QueuePending,
		TraceID:     trace,
	}
	if err := s.queue.Enqueue(r.Context(), item); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if _, err := s.st.DB().ExecContext(r.Context(), `INSERT INTO event_outcomes
		(event_id, workspace_id, job, on_index, outcome, queue_id, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (event_id, workspace_id, job, on_index) DO NOTHING`, eid, body.Workspace, body.Job, body.OnIndex,
		string(dispatch.OutcomeQueued), item.ID, now); err != nil {
		s.log.Warn("rules.fire outcome insert", "event_id", eid, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"outcome": "queued", "queue_id": item.ID, "event_id": eid,
	})
}
