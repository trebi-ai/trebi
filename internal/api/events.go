package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
)

// eventRow is one scanned events table row.
type eventRow struct {
	id, integ, at, sum, trace, raw string
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
	mux.HandleFunc("GET /v1/inbox", s.handleInbox)
	mux.HandleFunc("GET /v1/inbox/{id}", s.handleInboxGet)
	mux.HandleFunc("POST /v1/inbox/{id}/read", s.handleInboxRead)
	mux.HandleFunc("POST /v1/inbox/read-all", s.handleInboxReadAll)
	mux.HandleFunc("POST /v1/inbox/{id}/resolve", s.handleInboxResolve)
	mux.HandleFunc("GET /v1/approvals", s.handleApprovals)
	mux.HandleFunc("GET /v1/agents/state", s.handleAgentState)
	mux.HandleFunc("GET /v1/stats/runs", s.handleStats)
	mux.HandleFunc("GET /v1/alerts", s.handleAlerts)
	mux.HandleFunc("POST /v1/agent-hook/{event}", s.handleAgentHook)
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
	q := `SELECT id, integration, received_at, COALESCE(summary,''), COALESCE(trace_id,''), COALESCE(raw_json,'')
		FROM events WHERE 1=1`
	args := []any{}
	if cursor != "" {
		q += ` AND id < ?`
		args = append(args, cursor)
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
		if rows.Scan(&e.id, &e.integ, &e.at, &e.sum, &e.trace, &e.raw) == nil {
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
		"id": e.id, "integration": e.integ, "at": e.at, "received_at": e.at,
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
			"id": e.id, "integration": e.integ, "at": e.at, "received_at": e.at,
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
			patchOutcome(o, q)
			outcomesByEvent[eid][i] = o
		}
	}
}

// patchOutcome overwrites one outcome entry with the current queue state.
func patchOutcome(o map[string]any, q queueResult) {
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
	var integ, at, sum, raw, trace string
	err := s.st.DB().QueryRowContext(r.Context(), `
		SELECT integration, received_at, COALESCE(summary,''), COALESCE(raw_json,''), COALESCE(trace_id,'')
		FROM events WHERE id=?`, id).Scan(&integ, &at, &sum, &raw, &trace)
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
		"id": id, "integration": integ, "at": at, "received_at": at, "summary": sum,
		"raw": rawMsg, "trace_id": trace, "outcomes": outs,
	})
}
