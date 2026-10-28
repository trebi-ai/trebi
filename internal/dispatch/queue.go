package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/store"
)

// LaneCap is the number of pending rows one lane holds. A resume row does
// not count.
const LaneCap = 10

// MaxAttempts caps how many launch attempts a queue row may take. A wait
// is not an attempt.
const MaxAttempts = 5

// Launch kinds, one spelling with runs (event_queue.kind, runs.kind).
const (
	KindJob       = "job"
	KindAssistant = "assistant"
	KindAgent     = "agent"
)

var (
	// ErrNotFound is a missing queue row.
	ErrNotFound = errors.New("queue item not found")
	// ErrDone is a mutation against a finished row.
	ErrDone = errors.New("queue item is done")
	// ErrLaneFull is an enqueue into a lane that holds LaneCap pending rows.
	ErrLaneFull = errors.New("lane full")
)

// QueueItem is an event_queue row.
type QueueItem struct {
	ID              string `json:"id"`
	Connector       string `json:"connector"`
	WorkspaceID     string `json:"workspace_id"`
	JobID           string `json:"job_id"`
	JobName         string `json:"job_name"`
	Kind            string `json:"kind"`
	OnIndex         int    `json:"on_index"`
	ConversationKey string `json:"conversation_key,omitempty"`
	// FollowUp marks an event that continues an open conversation.
	FollowUp    bool              `json:"follow_up,omitempty"`
	EventID     string            `json:"event_id,omitempty"`
	EventRaw    string            `json:"event_raw,omitempty"`
	Status      QueueStatus       `json:"status"`
	BlockReason BlockReason       `json:"block_reason,omitempty"`
	Prompt      string            `json:"prompt,omitempty"`
	Vars        map[string]string `json:"vars,omitempty"`
	// Inputs are the filled job inputs of an event run.
	Inputs  map[string]string `json:"inputs,omitempty"`
	TraceID string            `json:"trace_id,omitempty"`
	// HopID is the agent chain hop that this row delivers (connector chain).
	HopID         string     `json:"hop_id,omitempty"`
	RunID         string     `json:"run_id,omitempty"`
	ResumeOf      string     `json:"resume_of,omitempty"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	ErrorMessage  string     `json:"error_message,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	// BatchCount is the event count of a batch row (batch: <duration>).
	BatchCount int `json:"batch_count,omitempty"`
	// Launch is set on a manual or scheduled launch that waits for a live run.
	Launch *LaunchRequest `json:"-"`
}

// PromptVars are the vars that fill the job prompt: the event vars, then
// the filled inputs, which win.
func (item *QueueItem) PromptVars() map[string]string {
	if len(item.Inputs) == 0 {
		return item.Vars
	}
	out := make(map[string]string, len(item.Vars)+len(item.Inputs))
	maps.Copy(out, item.Vars)
	maps.Copy(out, item.Inputs)
	return out
}

// Connectors of queue rows that are not events.
const (
	ConnectorManual   = "manual"
	ConnectorSchedule = "schedule"
	ConnectorChain    = "chain"
)

// LaunchRequest is a manual or scheduled launch that waits for a live run.
type LaunchRequest struct {
	Reason         string            `json:"reason"` // manual | schedule | report
	Harness        string            `json:"harness,omitempty"`
	Runtime        string            `json:"runtime,omitempty"`
	Prompt         string            `json:"prompt,omitempty"` // expanded prompt of a job run
	Wake           *LaunchWake       `json:"wake,omitempty"`   // operator body and message
	Inputs         map[string]string `json:"inputs,omitempty"`
	ConversationID string            `json:"conversation_id,omitempty"`
	Event          *LaunchEvent      `json:"event,omitempty"`
}

// LaunchEvent is the stored event of a run that a person starts from it.
type LaunchEvent struct {
	ID        string            `json:"id"`
	Connector string            `json:"connector,omitempty"`
	OnIndex   int               `json:"on_index"`
	Vars      map[string]string `json:"vars,omitempty"`
}

type LaunchWake struct {
	Body        string `json:"body,omitempty"`
	ExtraPrompt string `json:"extra_prompt,omitempty"`
	Message     string `json:"message,omitempty"`
}

// QueueStatus / QueueResult / BlockReason — trebi event_queue, verbatim.
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

// DeferReason is the error_message of a row that waits for a later attempt.
type DeferReason string

const (
	DeferPolicy    DeferReason = "policy"
	DeferTurn      DeferReason = "turn_active"
	DeferRunActive DeferReason = "run_active"
	// DeferHarnessUpdate waits while Trebi updates the harness.
	DeferHarnessUpdate DeferReason = "harness_update"
	DeferRetry         DeferReason = "retry"
)

// waitReasons is the SQL list of defer reasons that are waits, not errors.
const waitReasons = `('policy','turn_active','run_active','harness_update')`

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
	OutcomeNoMatch   EventOutcome = "no_match"
	OutcomeError     EventOutcome = "error"
	OutcomeQueued    EventOutcome = "queued"
	OutcomeBlocked   EventOutcome = "blocked"
	OutcomeLaunched  EventOutcome = "launched"
	OutcomeCoalesced EventOutcome = "coalesced"
	OutcomeSkipped   EventOutcome = "skipped"
	OutcomeDead      EventOutcome = "dead"
	// OutcomeDropped is an event that found its lane full.
	OutcomeDropped EventOutcome = "dropped"
	// OutcomeManual is an event that a manual trigger takes: it waits for a
	// person to start the run.
	OutcomeManual EventOutcome = "manual"
)

// Queue owns event_queue rows for one store.
type Queue struct {
	st  *store.Store
	log *slog.Logger
	now func() time.Time
}

// NewQueue binds a queue to a store.
func NewQueue(st *store.Store) *Queue {
	return NewQueueWithClock(st, time.Now)
}

// NewQueueWithClock binds a queue to a store and a clock. The clock decides
// when a deferred row is due; tests replace it.
func NewQueueWithClock(st *store.Store, now func() time.Time) *Queue {
	if now == nil {
		now = time.Now
	}
	return &Queue{st: st, log: slog.Default().With("source", "queue"), now: now}
}

// Enqueue inserts a pending queue row + bus event. A pending row that is not
// a resume returns ErrLaneFull and inserts nothing when its lane holds
// LaneCap pending rows.
func (q *Queue) Enqueue(ctx context.Context, item *QueueItem) error {
	if item.ID == "" {
		item.ID = ulid.Make().String()
	}
	if item.Kind == "" {
		item.Kind = KindJob
	}
	if item.Status == "" {
		item.Status = QueuePending
	}
	if item.TraceID == "" {
		item.TraceID = ulid.Make().String()
	}
	item.CreatedAt = q.now().UTC()
	now := item.CreatedAt.Format(time.RFC3339Nano)

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
	var launchJSON *string
	if item.Launch != nil {
		b, _ := json.Marshal(item.Launch)
		s := string(b)
		launchJSON = &s
	}
	var inputsJSON *string
	if len(item.Inputs) > 0 {
		b, _ := json.Marshal(item.Inputs)
		s := string(b)
		inputsJSON = &s
	}

	_, err := q.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if item.Status == QueuePending && item.ResumeOf == "" {
			if err := laneHasRoom(tx, item); err != nil {
				return err
			}
		}
		var next *string
		if item.NextAttemptAt != nil {
			v := item.NextAttemptAt.UTC().Format(time.RFC3339)
			next = &v
		}
		_, err := tx.Exec(`INSERT INTO event_queue (
			id, connector, workspace_id, job_id, kind, on_index, conversation_key,
			event_id, event_raw, vars_json, prompt, status, block_reason, resume_of,
			trace_id, launch_json, follow_up, created_at, updated_at, hop_id, next_attempt_at, inputs_json
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			item.ID, item.Connector, item.WorkspaceID, item.JobID, item.Kind, item.OnIndex,
			item.ConversationKey, eventID, store.NullStr(item.EventRaw), varsJSON, item.Prompt,
			string(item.Status), store.NullStr(string(item.BlockReason)), store.NullStr(item.ResumeOf),
			item.TraceID, launchJSON, item.FollowUp, now, now, item.HopID, next, inputsJSON)
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

// laneHasRoom returns ErrLaneFull when the lane of item holds LaneCap
// pending rows that are not resumes.
func laneHasRoom(tx *sql.Tx, item *QueueItem) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM event_queue
		WHERE status='pending' AND resume_of IS NULL
		AND job_id=? AND kind=? AND conversation_key=?`,
		item.JobID, item.Kind, item.ConversationKey).Scan(&n); err != nil {
		return err
	}
	if n >= LaneCap {
		return ErrLaneFull
	}
	return nil
}

// JoinBatch adds one event to the pending batch row of its trigger. With no
// pending row, it opens one that comes due at the next tick of clock; the
// lane cap applies to that insert. The join that fills the row to BatchMax
// makes it due now. A row that a worker claimed is not changed: the event
// opens a new row. item carries the row fields, item.EventID and item.Vars
// the event. It returns the id of the batch row.
func (q *Queue) JoinBatch(ctx context.Context, item *QueueItem, window time.Duration, clock BatchClock) (string, error) {
	if item.Kind == "" {
		item.Kind = KindJob
	}
	at := q.now().UTC()
	now := at.Format(time.RFC3339Nano)
	var varsJSON *string
	if item.Vars != nil {
		b, _ := json.Marshal(item.Vars)
		v := string(b)
		varsJSON = &v
	}
	var queueID, action string
	var count int
	q.st.LockBus()
	defer q.st.UnlockBus()
	err := q.st.WithTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT id FROM event_queue
			WHERE status='pending' AND resume_of IS NULL AND workspace_id=? AND job_id=? AND kind=? AND conversation_key=?
			AND (SELECT COUNT(*) FROM event_queue_batch b WHERE b.queue_id = event_queue.id) < ?
			ORDER BY id DESC LIMIT 1`,
			item.WorkspaceID, item.JobID, item.Kind, item.ConversationKey, BatchMax).Scan(&queueID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := laneHasRoom(tx, item); err != nil {
				return err
			}
			queueID, action = ulid.Make().String(), "created"
			if item.TraceID == "" {
				item.TraceID = ulid.Make().String()
			}
			if _, err := tx.Exec(`INSERT INTO event_queue (
				id, connector, workspace_id, job_id, kind, on_index, conversation_key,
				prompt, status, trace_id, follow_up, created_at, updated_at, hop_id, next_attempt_at
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				queueID, item.Connector, item.WorkspaceID, item.JobID, item.Kind, item.OnIndex,
				item.ConversationKey, item.Prompt, string(QueuePending), item.TraceID, false, now, now, item.HopID,
				clock.NextTick(at, window).UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			action = "updated"
		}
		if _, err := tx.Exec(`INSERT INTO event_queue_batch (queue_id, event_id, vars_json, added_at) VALUES (?,?,?,?)
			ON CONFLICT (queue_id, event_id) DO NOTHING`, queueID, item.EventID, varsJSON, now); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM event_queue_batch WHERE queue_id=?`, queueID).Scan(&count); err != nil {
			return err
		}
		if count >= BatchMax {
			if _, err := tx.Exec(`UPDATE event_queue SET next_attempt_at=?, updated_at=? WHERE id=? AND status='pending'`,
				at.Format(time.RFC3339), now, queueID); err != nil {
				return err
			}
		} else if _, err := tx.Exec(`UPDATE event_queue SET updated_at=? WHERE id=?`, now, queueID); err != nil {
			return err
		}
		_, err = store.AppendInTx(tx, store.BusEvent{
			Topic: "queue.updated", TraceID: item.TraceID, Entity: "queue", EntityID: queueID, Action: action,
			Attrs: map[string]string{"status": string(QueuePending), "batch_count": strconv.Itoa(count)},
		})
		return err
	})
	if err != nil {
		return "", err
	}
	q.st.NotifyBus()
	item.ID = queueID
	return queueID, nil
}

// BatchEvents returns the events of a batch row in join order, with the
// fields of their stored events rows. An event row that is gone keeps only
// its id and vars.
func (q *Queue) BatchEvents(ctx context.Context, queueID string) ([]BatchEvent, error) {
	rows, err := q.st.DB().QueryContext(ctx, `SELECT b.event_id, COALESCE(b.vars_json,''), b.added_at,
		COALESCE(e.connector,''), COALESCE(e.received_at,''), COALESCE(e.sender,''), COALESCE(e.room,''),
		COALESCE(e.thread,''), COALESCE(e.text,''), COALESCE(e.summary,''), COALESCE(e.raw_json,'')
		FROM event_queue_batch b LEFT JOIN events e ON e.id = b.event_id
		WHERE b.queue_id=? ORDER BY b.added_at, b.event_id`, queueID)
	if err != nil {
		return nil, fmt.Errorf("batch events of %s: %w", queueID, err)
	}
	defer rows.Close()
	var out []BatchEvent
	for rows.Next() {
		var ev BatchEvent
		var vars, added, raw string
		if err := rows.Scan(&ev.EventID, &vars, &added, &ev.Connector, &ev.ReceivedAt, &ev.Sender, &ev.Room,
			&ev.Thread, &ev.Text, &ev.Summary, &raw); err != nil {
			return nil, err
		}
		if vars != "" {
			_ = json.Unmarshal([]byte(vars), &ev.Vars) // a bad row reads as no vars
		}
		ev.RoomName = ev.Vars["room_name"]
		ev.AddedAt, _ = time.Parse(time.RFC3339Nano, added) // a bad time reads as zero
		ev.setRaw(raw)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ClaimNext claims one pending row for a lane (FIFO). Concurrent-safe via RowsAffected.
// Orders by id: ids are ULIDs (time-sortable); created_at text mis-sorts
// because RFC3339Nano strips trailing fractional zeros. A lane claims only
// its head row, so a row in backoff or in a wait holds the rows behind it.
// The claim does not change attempts; MarkAttempt counts a launch.
func (q *Queue) ClaimNext(ctx context.Context, jobID, kind, conversationKey string) (*QueueItem, error) {
	at := q.now().UTC()
	now := at.Format(time.RFC3339Nano)
	// next_attempt_at is written as RFC3339 (second precision); compare it
	// against the same shape so the lexical order is the time order.
	due := at.Format(time.RFC3339)
	query := `SELECT id, connector, workspace_id, job_id, ` + store.JobNameSQL("event_queue.job_id") + `, kind, on_index, conversation_key,
			COALESCE(event_id,''), COALESCE(event_raw,''), status, COALESCE(prompt,''), COALESCE(trace_id,''),
			COALESCE(resume_of,''), attempts, COALESCE(vars_json,''), COALESCE(launch_json,''), follow_up, created_at, hop_id,
			COALESCE(inputs_json,'')
			FROM event_queue
			WHERE status='pending' AND job_id=? AND kind=? AND conversation_key=?
			AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
			AND id = (SELECT MIN(id) FROM event_queue
			WHERE status='pending' AND job_id=? AND kind=? AND conversation_key=?)`
	args := []any{jobID, kind, conversationKey, due, jobID, kind, conversationKey}
	var item QueueItem
	q.st.LockBus()
	defer q.st.UnlockBus()
	err := q.st.WithTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRow(query, args...)
		var created, status, varsJSON, launchJSON, inputsJSON string
		if err := row.Scan(&item.ID, &item.Connector, &item.WorkspaceID, &item.JobID, &item.JobName, &item.Kind,
			&item.OnIndex, &item.ConversationKey, &item.EventID, &item.EventRaw, &status, &item.Prompt,
			&item.TraceID, &item.ResumeOf, &item.Attempts, &varsJSON, &launchJSON, &item.FollowUp, &created, &item.HopID,
			&inputsJSON); err != nil {
			return err
		}
		item.Status = QueueStatus(status)
		if varsJSON != "" {
			_ = json.Unmarshal([]byte(varsJSON), &item.Vars)
		}
		if inputsJSON != "" {
			_ = json.Unmarshal([]byte(inputsJSON), &item.Inputs)
		}
		if launchJSON != "" {
			item.Launch = &LaunchRequest{}
			if err := json.Unmarshal([]byte(launchJSON), item.Launch); err != nil {
				return fmt.Errorf("parse queue item %s launch_json: %w", item.ID, err)
			}
		}
		createdAt, parseErr := time.Parse(time.RFC3339Nano, created)
		if parseErr != nil {
			return fmt.Errorf("parse queue item %s created_at: %w", item.ID, parseErr)
		}
		item.CreatedAt = createdAt
		res, err := tx.Exec(`UPDATE event_queue SET status='claimed', claimed_at=?, updated_at=? WHERE id=? AND status='pending'`,
			now, now, item.ID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errClaimLost
		}
		item.Status = QueueClaimed
		_, err = store.AppendInTx(tx, store.BusEvent{
			Topic: "queue.updated", Entity: "queue", EntityID: item.ID, Action: "updated",
			Attrs: map[string]string{"status": "claimed"},
		})
		return err
	})
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, errClaimLost) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

var errClaimLost = fmt.Errorf("claim lost to concurrent worker")

// MarkAttempt adds 1 to attempts just before a launch. It returns the new
// count.
func (q *Queue) MarkAttempt(ctx context.Context, id string) (int, error) {
	now := q.now().UTC().Format(time.RFC3339Nano)
	var n int
	_, err := q.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE event_queue SET attempts=attempts+1, updated_at=? WHERE id=?`, now, id); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT attempts FROM event_queue WHERE id=?`, id).Scan(&n); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrNotFound, id)
			}
			return err
		}
		return nil
	}, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: id, Action: "updated",
		Attrs: map[string]string{"status": string(QueueClaimed)},
	})
	return n, err
}

// RecordDone inserts item as a finished row with result and run_id: the
// row of a run that started outside the queue. The drain never claims it.
func (q *Queue) RecordDone(ctx context.Context, item *QueueItem, result QueueResult, runID string) error {
	item.Status = QueueDone
	if err := q.Enqueue(ctx, item); err != nil {
		return err
	}
	return q.MarkDone(ctx, item.ID, result, runID)
}

// MarkDone marks a queue row done with result and optional run_id. It
// clears a wait reason, because the wait is over.
func (q *Queue) MarkDone(ctx context.Context, id string, result QueueResult, runID string) error {
	now := q.now().UTC().Format(time.RFC3339Nano)
	_, err := q.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE event_queue SET status='done', result=?, run_id=?, done_at=?, updated_at=?,
			error_message=CASE WHEN error_message IN `+waitReasons+` THEN NULL ELSE error_message END
			WHERE id=?`,
			string(result), store.NullStr(runID), now, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil
	}, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: id, Action: "updated",
		Attrs: map[string]string{"status": "done", "result": string(result)},
	})
	return err
}

// MarkDead marks a queue row done with result=dead and keeps msg as the
// error. A final error takes no retry.
func (q *Queue) MarkDead(ctx context.Context, id, msg string) error {
	now := q.now().UTC().Format(time.RFC3339Nano)
	_, err := q.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE event_queue SET status='done', result=?, error_message=?, done_at=?, updated_at=? WHERE id=?`,
			string(ResultDead), store.NullStr(msg), now, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil
	}, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: id, Action: "updated",
		Attrs: map[string]string{"status": string(QueueDone), "result": string(ResultDead)},
	})
	return err
}

// Defer returns a claimed or pending row to pending with the first allowed
// attempt time, rounded up to the next whole second. The launch records the
// fire, so a policy delay never spends the rate budget.
func (q *Queue) Defer(ctx context.Context, id string, until time.Time, reason DeferReason) error {
	now := q.now().UTC().Format(time.RFC3339)
	// next_attempt_at has second precision: rounded down, the row comes due
	// before the policy allows it and takes one more claim.
	if r := until.Truncate(time.Second); r.Before(until) {
		until = r.Add(time.Second)
	}
	_, err := q.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE event_queue SET status='pending', next_attempt_at=?, error_message=?, updated_at=? WHERE id=?`,
			until.UTC().Format(time.RFC3339), store.NullStr(string(reason)), now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil
	}, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: id, Action: "updated",
		Attrs: map[string]string{
			"status": "pending", "next_attempt_at": until.UTC().Format(time.RFC3339), "reason": string(reason),
		},
	})
	return err
}

// Wait returns a claimed row to pending after an attempt that did not
// launch, because a run of the same job is live. It undoes the attempt count
// of MarkAttempt, so a wait is not an attempt.
func (q *Queue) Wait(ctx context.Context, id string, until time.Time, reason DeferReason) error {
	if _, err := q.st.DB().ExecContext(ctx, `UPDATE event_queue SET attempts=CASE WHEN attempts > 0 THEN attempts-1 ELSE 0 END WHERE id=?`, id); err != nil {
		return err
	}
	return q.Defer(ctx, id, until, reason)
}

// MarkBlocked sets status=blocked with a block_reason (auth/limit/policy/job_missing).
// Leaves the row claimable again later by resetting to pending when unblocked
// (callers may requeue). Blocked rows do not launch.
func (q *Queue) MarkBlocked(ctx context.Context, id string, reason BlockReason) error {
	now := q.now().UTC().Format(time.RFC3339Nano)
	_, err := q.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE event_queue SET status='blocked', block_reason=?, updated_at=? WHERE id=?`,
			string(reason), now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil
	}, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: id, Action: "updated",
		Attrs: map[string]string{"status": "blocked", "block_reason": string(reason)},
	})
	if err == nil {
		q.log.Info("queue.blocked", "queue_id", id, "block_reason", string(reason))
	}
	return err
}

// Requeue resets a row to pending and clears block_reason, next_attempt_at,
// and a wait reason. Attempts are preserved. Any non-done status can
// requeue; 'done' rows cannot ('dead' exists only as a result on done rows).
func (q *Queue) Requeue(ctx context.Context, id string) error {
	now := q.now().UTC().Format(time.RFC3339Nano)
	_, err := q.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRow(`SELECT status FROM event_queue WHERE id=?`, id).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrNotFound, id)
			}
			return err
		}
		if status == string(QueueDone) {
			return fmt.Errorf("%w: %s", ErrDone, id)
		}
		_, err := tx.Exec(`UPDATE event_queue SET status='pending', block_reason=NULL, next_attempt_at=NULL,
			error_message=CASE WHEN error_message IN `+waitReasons+` THEN NULL ELSE error_message END,
			updated_at=? WHERE id=?`,
			now, id)
		return err
	}, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: id, Action: "updated",
		Attrs: map[string]string{"status": "pending"},
	})
	return err
}

// Cancel marks a row done with result=skipped. Errors if missing or already done.
func (q *Queue) Cancel(ctx context.Context, id string) error {
	now := q.now().UTC().Format(time.RFC3339Nano)
	_, err := q.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRow(`SELECT status FROM event_queue WHERE id=?`, id).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrNotFound, id)
			}
			return err
		}
		if status == string(QueueDone) {
			return fmt.Errorf("%w: %s", ErrDone, id)
		}
		_, err := tx.Exec(`UPDATE event_queue SET status='done', result=?, done_at=?, updated_at=? WHERE id=?`,
			string(ResultSkipped), now, now, id)
		return err
	}, store.BusEvent{
		Topic: "queue.updated", Entity: "queue", EntityID: id, Action: "updated",
		Attrs: map[string]string{"status": "done", "result": string(ResultSkipped)},
	})
	return err
}

// ReclaimStale resets claimed rows older than d back to pending.
func (q *Queue) ReclaimStale(ctx context.Context, d time.Duration) (int64, error) {
	if d <= 0 {
		d = 2 * time.Minute
	}
	cutoff := q.now().UTC().Add(-d).Format(time.RFC3339Nano)
	now := q.now().UTC().Format(time.RFC3339Nano)
	res, err := q.st.DB().ExecContext(ctx,
		`UPDATE event_queue SET status='pending', updated_at=? WHERE status='claimed' AND claimed_at IS NOT NULL AND claimed_at < ?`,
		now, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListOpts filters event_queue lists.
type ListOpts struct {
	Status    string
	Workspace string
	Cursor    string
	Limit     int
}

// List returns queue rows newest-first. A set Workspace keeps rows of that
// workspace plus global rows (empty workspace_id).
func (q *Queue) List(ctx context.Context, opts ListOpts) ([]QueueItem, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	query := `SELECT id, connector, workspace_id, job_id, ` + store.JobNameSQL("event_queue.job_id") + `, kind, on_index, conversation_key,
		COALESCE(event_id,''), COALESCE(event_raw,''), status, COALESCE(block_reason,''),
		COALESCE(prompt,''), COALESCE(trace_id,''), COALESCE(run_id,''), attempts,
		COALESCE(next_attempt_at,''), COALESCE(error_message,''), follow_up, created_at, hop_id,
		(SELECT COUNT(*) FROM event_queue_batch b WHERE b.queue_id = event_queue.id)
		FROM event_queue WHERE 1=1`
	var args []any
	if opts.Status != "" {
		query += ` AND status=?`
		args = append(args, opts.Status)
	}
	if opts.Workspace != "" {
		query += ` AND (workspace_id = ? OR workspace_id = '' OR workspace_id IS NULL)`
		args = append(args, opts.Workspace)
	}
	if opts.Cursor != "" {
		query += ` AND id < ?`
		args = append(args, opts.Cursor)
	}
	query += ` ORDER BY id DESC LIMIT ?` // ULID order == chronological; created_at text is not
	args = append(args, opts.Limit)
	rows, err := q.st.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueItem
	for rows.Next() {
		var it QueueItem
		var stStr, br, next, created string
		if err := rows.Scan(&it.ID, &it.Connector, &it.WorkspaceID, &it.JobID, &it.JobName, &it.Kind, &it.OnIndex,
			&it.ConversationKey, &it.EventID, &it.EventRaw, &stStr, &br, &it.Prompt, &it.TraceID, &it.RunID,
			&it.Attempts, &next, &it.ErrorMessage, &it.FollowUp, &created, &it.HopID, &it.BatchCount); err != nil {
			return nil, err
		}
		if next != "" {
			if t, perr := time.Parse(time.RFC3339, next); perr == nil {
				it.NextAttemptAt = &t
			}
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
