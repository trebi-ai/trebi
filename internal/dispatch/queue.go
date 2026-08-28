package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// QueueItem is an event_queue row.
type QueueItem struct {
	ID              string
	Integration     string
	WorkspaceID     string
	Job             string
	Kind            string
	OnIndex         int
	ConversationKey string
	EventID         string
	EventRaw        string
	Status          QueueStatus
	BlockReason     BlockReason
	Prompt          string
	Vars            map[string]string
	TraceID         string
	RunID           string
	ResumeOf        string
	CreatedAt       time.Time
}

// QueueStatus / QueueResult / BlockReason — jobi event_queue, verbatim.
type QueueStatus string

const (
	QueuePending QueueStatus = "pending"
	QueueBlocked QueueStatus = "blocked"
	QueueClaimed QueueStatus = "claimed"
	QueueDone    QueueStatus = "done"
	QueueDead    QueueStatus = "dead"
)

type QueueResult string

const (
	ResultLaunched  QueueResult = "launched"
	ResultCoalesced QueueResult = "coalesced"
	ResultSkipped   QueueResult = "skipped"
	ResultDead      QueueResult = "dead"
)

type BlockReason string

const (
	BlockAuthMissing   BlockReason = "auth_missing"
	BlockLimitExceeded BlockReason = "limit_exceeded"
	BlockPolicy        BlockReason = "policy"
	BlockJobMissing    BlockReason = "job_missing"
	BlockGate          BlockReason = "gate"
)

// EventOutcome — wire enum (B15).
type EventOutcome string

const (
	OutcomeNoMatch     EventOutcome = "no_match"
	OutcomeDebounced   EventOutcome = "debounced"
	OutcomeRateLimited EventOutcome = "rate_limited"
	OutcomeError       EventOutcome = "error"
	OutcomeQueued      EventOutcome = "queued"
	OutcomeBlocked     EventOutcome = "blocked"
	OutcomeLaunched    EventOutcome = "launched"
	OutcomeCoalesced   EventOutcome = "coalesced"
	OutcomeSkipped     EventOutcome = "skipped"
	OutcomeDead        EventOutcome = "dead"
)

// Queue owns event_queue rows for one store.
type Queue struct {
	st  *store.Store
	log *slog.Logger
}

// NewQueue binds a queue to a store.
func NewQueue(st *store.Store) *Queue {
	return &Queue{st: st, log: slog.Default().With("source", "queue")}
}

// Enqueue inserts a pending queue row + bus event.
func (q *Queue) Enqueue(ctx context.Context, item *QueueItem) error {
	if item.ID == "" {
		item.ID = ulid.Make().String()
	}
	if item.Kind == "" {
		item.Kind = runs.KindJob
	}
	if item.Status == "" {
		item.Status = QueuePending
	}
	if item.TraceID == "" {
		item.TraceID = ulid.Make().String()
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	item.CreatedAt = time.Now().UTC()

	var varsJSON *string
	if item.Vars != nil {
		b, _ := json.Marshal(item.Vars)
		s := string(b)
		varsJSON = &s
	}
	var eventID *string
	if item.EventID != "" {
		eventID = &item.EventID
	}

	_, err := q.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO event_queue (
			id, integration, workspace_id, job, kind, on_index, conversation_key,
			event_id, event_raw, vars_json, prompt, status, block_reason, resume_of,
			trace_id, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			item.ID, item.Integration, item.WorkspaceID, item.Job, item.Kind, item.OnIndex,
			item.ConversationKey, eventID, store.NullStr(item.EventRaw), varsJSON, item.Prompt,
			string(item.Status), store.NullStr(string(item.BlockReason)), store.NullStr(item.ResumeOf),
			item.TraceID, now, now)
		return err
	}, store.BusEvent{
		Topic:    "queue.updated",
		TraceID:  item.TraceID,
		Entity:   "queue",
		EntityID: item.ID,
		Action:   "created",
		Attrs:    map[string]string{"status": string(item.Status)},
	})
	return err
}

// ClaimNext claims one pending row for a lane (FIFO). Concurrent-safe via RowsAffected.
// Orders by id: ids are ULIDs (time-sortable); created_at text mis-sorts
// because RFC3339Nano strips trailing fractional zeros.
func (q *Queue) ClaimNext(ctx context.Context, workspaceID, job, kind, conversationKey string) (*QueueItem, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var item QueueItem
	err := q.st.WithTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRow(`SELECT id, integration, workspace_id, job, kind, on_index, conversation_key,
			COALESCE(event_id,''), COALESCE(event_raw,''), status, COALESCE(prompt,''), COALESCE(trace_id,''), created_at
			FROM event_queue
			WHERE status='pending' AND workspace_id=? AND job=? AND kind=? AND conversation_key=?
			ORDER BY id ASC LIMIT 1`, workspaceID, job, kind, conversationKey)
		var created, status string
		if err := row.Scan(&item.ID, &item.Integration, &item.WorkspaceID, &item.Job, &item.Kind,
			&item.OnIndex, &item.ConversationKey, &item.EventID, &item.EventRaw, &status, &item.Prompt,
			&item.TraceID, &created); err != nil {
			return err
		}
		item.Status = QueueStatus(status)
		createdAt, parseErr := time.Parse(time.RFC3339Nano, created)
		if parseErr != nil {
			return fmt.Errorf("parse queue item %s created_at: %w", item.ID, parseErr)
		}
		item.CreatedAt = createdAt
		res, err := tx.Exec(`UPDATE event_queue SET status='claimed', claimed_at=?, updated_at=?, attempts=attempts+1 WHERE id=? AND status='pending'`,
			now, now, item.ID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errClaimLost
		}
		item.Status = QueueClaimed
		return nil
	})
	if err == sql.ErrNoRows || err == errClaimLost {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := q.st.Append(ctx, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: item.ID, Action: "updated",
		Attrs: map[string]string{"status": "claimed"},
	}); err != nil {
		q.log.Warn("bus append", "error", err)
	}
	return &item, nil
}

var errClaimLost = fmt.Errorf("claim lost to concurrent worker")

// MarkDone marks a queue row done with result and optional run_id.
func (q *Queue) MarkDone(ctx context.Context, id string, result QueueResult, runID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := q.st.DB().ExecContext(ctx, `UPDATE event_queue SET status='done', result=?, run_id=?, done_at=?, updated_at=? WHERE id=?`,
		string(result), store.NullStr(runID), now, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("queue item %s not found", id)
	}
	if _, err := q.st.Append(ctx, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: id, Action: "updated",
		Attrs: map[string]string{"status": "done", "result": string(result)},
	}); err != nil {
		q.log.Warn("bus append", "error", err)
	}
	return nil
}

// MarkBlocked sets status=blocked with a block_reason (auth/limit/policy/job_missing).
// Leaves the row claimable again later by resetting to pending when unblocked
// (callers may requeue). Blocked rows do not launch.
func (q *Queue) MarkBlocked(ctx context.Context, id string, reason BlockReason) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := q.st.DB().ExecContext(ctx, `UPDATE event_queue SET status='blocked', block_reason=?, updated_at=? WHERE id=?`,
		string(reason), now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("queue item %s not found", id)
	}
	// queue.blocked is log-only; the bus rides queue.updated.
	q.log.Info("queue.blocked", "queue_id", id, "block_reason", string(reason))
	if _, err := q.st.Append(ctx, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: id, Action: "updated",
		Attrs: map[string]string{"status": "blocked", "block_reason": string(reason)},
	}); err != nil {
		q.log.Warn("bus append", "topic", "queue.updated", "error", err)
	}
	return nil
}

// Requeue resets a row to pending and clears block_reason + next_attempt_at.
// Attempts are preserved. Any non-done status can requeue; 'done' rows
// cannot ('dead' exists only as a result on done rows).
func (q *Queue) Requeue(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err := q.st.WithTx(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRow(`SELECT status FROM event_queue WHERE id=?`, id).Scan(&status); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("queue item %s not found", id)
			}
			return err
		}
		if status == string(QueueDone) {
			return fmt.Errorf("queue item %s is done", id)
		}
		_, err := tx.Exec(`UPDATE event_queue SET status='pending', block_reason=NULL, next_attempt_at=NULL, updated_at=? WHERE id=?`,
			now, id)
		return err
	})
	if err != nil {
		return err
	}
	if _, err := q.st.Append(ctx, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: id, Action: "updated",
		Attrs: map[string]string{"status": "pending"},
	}); err != nil {
		q.log.Warn("bus append", "error", err)
	}
	return nil
}

// Cancel marks a row done with result=skipped. Errors if missing or already done.
func (q *Queue) Cancel(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err := q.st.WithTx(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRow(`SELECT status FROM event_queue WHERE id=?`, id).Scan(&status); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("queue item %s not found", id)
			}
			return err
		}
		if status == string(QueueDone) {
			return fmt.Errorf("queue item %s already done", id)
		}
		_, err := tx.Exec(`UPDATE event_queue SET status='done', result=?, done_at=?, updated_at=? WHERE id=?`,
			string(ResultSkipped), now, now, id)
		return err
	})
	if err != nil {
		return err
	}
	if _, err := q.st.Append(ctx, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: id, Action: "updated",
		Attrs: map[string]string{"status": "done", "result": string(ResultSkipped)},
	}); err != nil {
		q.log.Warn("bus append", "error", err)
	}
	return nil
}

// List returns queue rows with optional status filter and ULID cursor.
func (q *Queue) List(ctx context.Context, status, cursor string, limit int) ([]QueueItem, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, integration, workspace_id, job, kind, on_index, conversation_key,
		COALESCE(event_id,''), COALESCE(event_raw,''), status, COALESCE(block_reason,''),
		COALESCE(prompt,''), COALESCE(trace_id,''), COALESCE(run_id,''), created_at
		FROM event_queue WHERE 1=1`
	var args []any
	if status != "" {
		query += ` AND status=?`
		args = append(args, status)
	}
	if cursor != "" {
		query += ` AND id < ?`
		args = append(args, cursor)
	}
	query += ` ORDER BY id DESC LIMIT ?` // ULID order == chronological; created_at text is not
	args = append(args, limit)
	rows, err := q.st.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueItem
	for rows.Next() {
		var it QueueItem
		var stStr, br, created string
		if err := rows.Scan(&it.ID, &it.Integration, &it.WorkspaceID, &it.Job, &it.Kind, &it.OnIndex,
			&it.ConversationKey, &it.EventID, &it.EventRaw, &stStr, &br, &it.Prompt, &it.TraceID, &it.RunID, &created); err != nil {
			return nil, err
		}
		it.Status = QueueStatus(stStr)
		it.BlockReason = BlockReason(br)
		it.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("parse queue item %s created_at: %w", it.ID, err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
