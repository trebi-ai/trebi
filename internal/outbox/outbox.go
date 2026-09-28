// Package outbox owns the durable pipe for outgoing messages (plan
// 2026-09-28 Part A). One table holds every email and connector channel
// send. The service claims due rows, backs off, and emits the bus frames.
// It does not know how to send: each Kind registers a Sender.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/store"
)

// Kind is the send path of a row.
type Kind string

// Status is the delivery state of a row.
type Status string

// Purpose is why a row exists.
type Purpose string

const (
	KindEmail   Kind = "email"
	KindChannel Kind = "channel"

	StatusPending  Status = "pending"
	StatusSending  Status = "sending"
	StatusSent     Status = "sent"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"

	PurposeReply  Purpose = "reply"
	PurposeAlert  Purpose = "alert"
	PurposeReport Purpose = "report"
	PurposeSend   Purpose = "send"
	PurposeTest   Purpose = "test"
)

var (
	// ErrNotFound is a missing row.
	ErrNotFound = errors.New("outbox row not found")
	// ErrConflict is an action the row status does not allow.
	ErrConflict = errors.New("outbox row status does not allow this action")
	// ErrNoSender is a row whose Kind has no registered Sender.
	ErrNoSender = errors.New("outbox kind has no sender")
)

const (
	dueBatch       = 20
	staleClaim     = 2 * time.Minute
	retention      = 30 * 24 * time.Hour
	defaultMax     = 5
	defaultLimit   = 50
	maxLimit       = 500
	errorMessageMx = 4000
)

// Row is one outgoing message and its delivery state. Body is empty after
// the row is sent.
type Row struct {
	ID             string            `json:"id"`
	Kind           Kind              `json:"kind"`
	Purpose        Purpose           `json:"purpose"`
	WorkspaceID    string            `json:"workspace_id,omitempty"`
	Instance       string            `json:"instance,omitempty"`
	Room           string            `json:"room,omitempty"`
	Thread         string            `json:"thread,omitempty"`
	Recipients     string            `json:"recipients,omitempty"`
	Subject        string            `json:"subject,omitempty"`
	Body           string            `json:"body,omitempty"`
	Env            map[string]string `json:"-"`
	ConversationID string            `json:"conversation_id,omitempty"`
	RunID          string            `json:"run_id,omitempty"`
	InboxID        string            `json:"inbox_id,omitempty"`
	EmailRowID     string            `json:"email_row_id,omitempty"`
	DedupeKey      string            `json:"dedupe_key,omitempty"`
	RouteStep      *int              `json:"route_step,omitempty"`
	Route          []notify.Step     `json:"route,omitempty"`
	Status         Status            `json:"status"`
	HoldReason     string            `json:"hold_reason,omitempty"`
	Attempts       int               `json:"attempts"`
	MaxAttempts    int               `json:"max_attempts"`
	NextAttemptAt  *time.Time        `json:"next_attempt_at,omitempty"`
	ClaimedAt      *time.Time        `json:"claimed_at,omitempty"`
	ErrorMessage   string            `json:"error,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	SentAt         *time.Time        `json:"sent_at,omitempty"`
	Revision       int64             `json:"revision"`
}

// Connector is the connector name of the instance: the part before '@'.
func (r Row) Connector() string {
	name, _, _ := strings.Cut(r.Instance, "@")
	return name
}

// Sender delivers one row. email.Service and channels.Service implement it.
type Sender interface {
	Deliver(ctx context.Context, r Row) error
}

// Watcher is an optional Sender method set. The outbox calls Changed after
// each committed status change of a row of that Kind.
type Watcher interface {
	Changed(ctx context.Context, r Row)
}

// Permanent marks an error that no retry can fix.
type Permanent struct{ Err error }

func (p Permanent) Error() string { return p.Err.Error() }
func (p Permanent) Unwrap() error { return p.Err }

// Hold keeps a row pending with a hold reason and no attempt count, until
// Release wakes it. A channel whose session needs a login returns it.
type Hold struct {
	Reason string
	Err    error
}

func (h Hold) Error() string { return h.Err.Error() }
func (h Hold) Unwrap() error { return h.Err }

// RetryAfter is a failure whose next attempt waits at least After.
type RetryAfter struct {
	After time.Duration
	Err   error
}

func (r RetryAfter) Error() string { return r.Err.Error() }
func (r RetryAfter) Unwrap() error { return r.Err }

// HoldAuthRequired is the hold reason of a channel that needs a login.
const HoldAuthRequired = "auth_required"

// Policy is the retry shape of one Kind.
type Policy struct {
	Base, Max time.Duration
	Attempts  int
}

// Backoff is the wait after the given failed attempt count (1-based).
func (p Policy) Backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := p.Base << (attempts - 1)
	if d > p.Max || d <= 0 {
		d = p.Max
	}
	return d
}

// Filter narrows List. Empty fields match every row.
type Filter struct {
	Kind           Kind
	Status         Status
	Purpose        Purpose
	WorkspaceID    string
	ConversationID string
	InboxID        string
	RunIDs         []string // any of; used for the agent-token scope
	Cursor         string
	Limit          int
}

// Service owns the outbox table.
type Service struct {
	st  *store.Store
	log *slog.Logger
	now func() time.Time

	mu       sync.RWMutex
	senders  map[Kind]Sender
	policies map[Kind]Policy
}

// New builds the service. Register the senders before the worker starts.
func New(st *store.Store) *Service {
	return &Service{
		st: st, now: time.Now,
		log:      slog.Default().With("source", "outbox"),
		senders:  map[Kind]Sender{},
		policies: map[Kind]Policy{},
	}
}

// WithClock replaces the clock. Tests use it.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Register installs the Sender and the retry Policy of one Kind.
func (s *Service) Register(k Kind, snd Sender, p Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.senders[k] = snd
	s.policies[k] = p
}

// Enqueue writes one pending row and emits outbox.queued. A row with a
// DedupeKey that already exists is not written again: Enqueue returns the
// stored row and no frame.
func (s *Service) Enqueue(ctx context.Context, r Row) (Row, error) {
	return s.EnqueueTx(ctx, nil, r)
}

// EnqueueTx is Enqueue with a caller write in the same transaction. The
// write runs first; an error from it rolls back both.
func (s *Service) EnqueueTx(ctx context.Context, write func(tx *sql.Tx, r Row) error, r Row) (Row, error) {
	if r.ID == "" {
		r.ID = ulid.Make().String()
	}
	if r.MaxAttempts <= 0 {
		if _, p, ok := s.sender(r.Kind); ok && p.Attempts > 0 {
			r.MaxAttempts = p.Attempts
		} else {
			r.MaxAttempts = defaultMax
		}
	}
	r.Status = StatusPending
	r.CreatedAt = s.now().UTC()
	r.Revision = 1
	env, _ := json.Marshal(nonNilEnv(r.Env))
	var route any
	if r.Route != nil {
		b, _ := json.Marshal(r.Route)
		route = string(b)
	}
	var routeStep any
	if r.RouteStep != nil {
		routeStep = *r.RouteStep
	}
	inserted := false
	err := s.commit(ctx, func(tx *sql.Tx) (Row, string, error) {
		if write != nil {
			if err := write(tx, r); err != nil {
				return Row{}, "", err
			}
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO outbox (id, kind, purpose, workspace_id, instance, room, thread, recipients, subject, body,
  env_json, conversation_id, run_id, inbox_id, email_row_id, dedupe_key, route_step, route_json,
  status, attempts, max_attempts, created_at, revision)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,1)
ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`,
			r.ID, string(r.Kind), string(r.Purpose), r.WorkspaceID, r.Instance, r.Room, r.Thread,
			r.Recipients, r.Subject, r.Body, string(env), store.NullStr(r.ConversationID), store.NullStr(r.RunID),
			store.NullStr(r.InboxID), store.NullStr(r.EmailRowID), store.NullStr(r.DedupeKey),
			routeStep, route, string(StatusPending), r.MaxAttempts, fmtTS(r.CreatedAt))
		if err != nil {
			return Row{}, "", fmt.Errorf("outbox insert: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Row{}, "", errNoChange
		}
		inserted = true
		return r, "outbox.queued", nil
	})
	if errors.Is(err, errNoChange) {
		return s.byDedupe(ctx, r.DedupeKey)
	}
	if err != nil {
		return Row{}, err
	}
	if inserted {
		s.log.InfoContext(ctx, "outbox.queued", "row", r.ID, "kind", r.Kind, "purpose", r.Purpose)
	}
	return s.Get(ctx, r.ID)
}

// SendDue releases stale claims, then claims and delivers up to 20 due
// rows. Delivery is at least once: a crash after a send and before the
// mark sends the row again.
func (s *Service) SendDue(ctx context.Context, now time.Time) error {
	if err := s.releaseStale(ctx, now); err != nil {
		return err
	}
	ids, err := s.dueIDs(ctx, now)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.attempt(ctx, id, now); err != nil && !errors.Is(err, errNoChange) {
			s.log.WarnContext(ctx, "outbox attempt", "row", id, "error", err.Error())
		}
	}
	return nil
}

// SendNow makes one synchronous attempt on a pending row and returns the
// row after it. A row that is not pending is returned as it is.
func (s *Service) SendNow(ctx context.Context, id string) (Row, error) {
	r, err := s.attempt(ctx, id, s.now())
	if errors.Is(err, errNoChange) {
		return s.Get(ctx, id)
	}
	return r, err
}

// Retry sets a failed or canceled row back to pending with attempts 0.
func (s *Service) Retry(ctx context.Context, id string) (Row, error) {
	return s.transition(ctx, id, "outbox.queued", []Status{StatusFailed, StatusCanceled}, `
UPDATE outbox SET status='pending', attempts=0, next_attempt_at=NULL, claimed_at=NULL,
  error_message=NULL, hold_reason='', revision=revision+1
WHERE id=? AND status IN ('failed','canceled')`)
}

// Cancel stops a pending or failed row. A sent row cannot be canceled.
func (s *Service) Cancel(ctx context.Context, id string) (Row, error) {
	return s.transition(ctx, id, "outbox.canceled", []Status{StatusPending, StatusFailed}, `
UPDATE outbox SET status='canceled', next_attempt_at=NULL, claimed_at=NULL, revision=revision+1
WHERE id=? AND status IN ('pending','failed')`)
}

// Get returns one row, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id string) (Row, error) {
	r, err := scanRow(s.st.DB().QueryRowContext(ctx, `SELECT `+rowCols+` FROM outbox WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	return r, err
}

// List returns rows newest first and the cursor of the next page.
func (s *Service) List(ctx context.Context, f Filter) ([]Row, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)
	q := `SELECT ` + rowCols + ` FROM outbox WHERE 1=1`
	var args []any
	add := func(clause, v string) {
		if v != "" {
			q += clause
			args = append(args, v)
		}
	}
	add(` AND kind=?`, string(f.Kind))
	add(` AND status=?`, string(f.Status))
	add(` AND purpose=?`, string(f.Purpose))
	add(` AND workspace_id=?`, f.WorkspaceID)
	add(` AND conversation_id=?`, f.ConversationID)
	add(` AND inbox_id=?`, f.InboxID)
	add(` AND id < ?`, f.Cursor)
	if f.RunIDs != nil {
		if len(f.RunIDs) == 0 {
			return []Row{}, "", nil
		}
		q += ` AND run_id IN (?` + repeatPlaceholders(len(f.RunIDs)-1) + `)`
		for _, id := range f.RunIDs {
			args = append(args, id)
		}
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("outbox list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Row{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, nil
}

// Prune deletes sent and canceled rows older than 30 days.
func (s *Service) Prune(ctx context.Context, now time.Time) error {
	cutoff := fmtTS(now.Add(-retention))
	if _, err := s.st.DB().ExecContext(ctx,
		`DELETE FROM outbox WHERE status IN ('sent','canceled') AND created_at < ?`, cutoff); err != nil {
		return fmt.Errorf("outbox prune: %w", err)
	}
	return nil
}

// errNoChange stops a write that changed no row, so no frame goes out.
var errNoChange = errors.New("outbox row unchanged")

// attempt claims one pending due row, delivers it, and marks the result.
func (s *Service) attempt(ctx context.Context, id string, now time.Time) (Row, error) {
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE outbox SET status='sending', claimed_at=? WHERE id=? AND status='pending'`, fmtTS(now), id)
	if err != nil {
		return Row{}, fmt.Errorf("outbox claim: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Row{}, errNoChange
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		return Row{}, err
	}
	snd, pol, ok := s.sender(r.Kind)
	var sendErr error
	if !ok {
		sendErr = Permanent{Err: fmt.Errorf("%w: %s", ErrNoSender, r.Kind)}
	} else {
		sendErr = snd.Deliver(ctx, r)
	}
	if sendErr == nil {
		return s.markSent(ctx, r)
	}
	if ctx.Err() != nil {
		// Shutdown: give the claim back so the next start sends it.
		_, _ = s.st.DB().ExecContext(context.WithoutCancel(ctx), // best effort; the stale sweep also frees it
			`UPDATE outbox SET status='pending', claimed_at=NULL WHERE id=? AND status='sending'`, id)
		return r, ctx.Err()
	}
	var hold Hold
	if errors.As(sendErr, &hold) {
		return s.markHeld(ctx, r, hold)
	}
	attempts := r.Attempts + 1
	var perm Permanent
	if errors.As(sendErr, &perm) || attempts >= r.MaxAttempts {
		return s.markFailed(ctx, r, attempts, sendErr.Error())
	}
	wait := pol.Backoff(attempts)
	var ra RetryAfter
	if errors.As(sendErr, &ra) && ra.After > wait {
		wait = ra.After
	}
	return s.markRetry(ctx, r, attempts, now.Add(wait), sendErr.Error())
}

func (s *Service) markHeld(ctx context.Context, r Row, h Hold) (Row, error) {
	var out Row
	err := s.commit(ctx, func(tx *sql.Tx) (Row, string, error) {
		if _, err := tx.ExecContext(ctx, `
UPDATE outbox SET status='pending', hold_reason=?, claimed_at=NULL, next_attempt_at=NULL,
  error_message=?, revision=revision+1
WHERE id=?`, h.Reason, truncate(h.Error()), r.ID); err != nil {
			return Row{}, "", err
		}
		row, err := scanRow(tx.QueryRowContext(ctx, `SELECT `+rowCols+` FROM outbox WHERE id=?`, r.ID))
		out = row
		return row, "outbox.queued", err
	})
	if err == nil {
		s.log.InfoContext(ctx, "outbox.queued", "row", r.ID, "hold_reason", h.Reason)
		s.changed(ctx, out)
	}
	return out, err
}

// Release wakes the rows of a connector instance that wait for a login. It
// returns the count of rows it woke.
func (s *Service) Release(ctx context.Context, instance string) (int, error) {
	res, err := s.st.DB().ExecContext(ctx, `
UPDATE outbox SET hold_reason='', next_attempt_at=NULL, revision=revision+1
WHERE status='pending' AND hold_reason <> '' AND instance=?`, instance)
	if err != nil {
		return 0, fmt.Errorf("outbox release: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.log.InfoContext(ctx, "outbox.queued", "instance", instance, "released", n)
	}
	return int(n), nil
}

func (s *Service) markSent(ctx context.Context, r Row) (Row, error) {
	var out Row
	err := s.commit(ctx, func(tx *sql.Tx) (Row, string, error) {
		if _, err := tx.ExecContext(ctx, `
UPDATE outbox SET status='sent', sent_at=?, body=NULL, env_json='{}', error_message=NULL,
  attempts=attempts+1, claimed_at=NULL, next_attempt_at=NULL, revision=revision+1
WHERE id=?`, fmtTS(s.now()), r.ID); err != nil {
			return Row{}, "", err
		}
		row, err := scanRow(tx.QueryRowContext(ctx, `SELECT `+rowCols+` FROM outbox WHERE id=?`, r.ID))
		out = row
		return row, "outbox.sent", err
	})
	if err == nil {
		s.log.InfoContext(ctx, "outbox.sent", "row", r.ID, "kind", r.Kind, "purpose", r.Purpose)
		s.changed(ctx, out)
	}
	return out, err
}

func (s *Service) markRetry(ctx context.Context, r Row, attempts int, next time.Time, msg string) (Row, error) {
	var out Row
	err := s.commit(ctx, func(tx *sql.Tx) (Row, string, error) {
		if _, err := tx.ExecContext(ctx, `
UPDATE outbox SET status='pending', attempts=?, next_attempt_at=?, claimed_at=NULL,
  error_message=?, revision=revision+1
WHERE id=?`, attempts, fmtTS(next), truncate(msg), r.ID); err != nil {
			return Row{}, "", err
		}
		row, err := scanRow(tx.QueryRowContext(ctx, `SELECT `+rowCols+` FROM outbox WHERE id=?`, r.ID))
		out = row
		return row, "outbox.queued", err
	})
	if err == nil {
		s.changed(ctx, out)
	}
	return out, err
}

func (s *Service) markFailed(ctx context.Context, r Row, attempts int, msg string) (Row, error) {
	var out Row
	err := s.commit(ctx, func(tx *sql.Tx) (Row, string, error) {
		if _, err := tx.ExecContext(ctx, `
UPDATE outbox SET status='failed', attempts=?, next_attempt_at=NULL, claimed_at=NULL,
  error_message=?, revision=revision+1
WHERE id=?`, attempts, truncate(msg), r.ID); err != nil {
			return Row{}, "", err
		}
		row, err := scanRow(tx.QueryRowContext(ctx, `SELECT `+rowCols+` FROM outbox WHERE id=?`, r.ID))
		out = row
		return row, "outbox.failed", err
	})
	if err == nil {
		s.log.WarnContext(ctx, "outbox.failed", "row", r.ID, "kind", r.Kind, "purpose", r.Purpose, "error", msg)
		s.changed(ctx, out)
	}
	return out, err
}

// transition runs one guarded status update. A row in another status is
// ErrConflict; a missing row is ErrNotFound.
func (s *Service) transition(ctx context.Context, id, topic string, from []Status, update string) (Row, error) {
	var out Row
	err := s.commit(ctx, func(tx *sql.Tx) (Row, string, error) {
		res, err := tx.ExecContext(ctx, update, id)
		if err != nil {
			return Row{}, "", err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Row{}, "", errNoChange
		}
		row, err := scanRow(tx.QueryRowContext(ctx, `SELECT `+rowCols+` FROM outbox WHERE id=?`, id))
		out = row
		return row, topic, err
	})
	if errors.Is(err, errNoChange) {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return Row{}, gerr
		}
		return Row{}, fmt.Errorf("%w (want %v)", ErrConflict, from)
	}
	if err == nil {
		s.changed(ctx, out)
	}
	return out, err
}

// changed tells the Kind's Watcher about one committed status change.
func (s *Service) changed(ctx context.Context, r Row) {
	if snd, _, ok := s.sender(r.Kind); ok {
		if w, ok := snd.(Watcher); ok {
			w.Changed(ctx, r)
		}
	}
}

// commit runs one row write and its bus frame in one transaction. write
// returns the row after the change and the topic of the frame.
func (s *Service) commit(ctx context.Context, write func(tx *sql.Tx) (Row, string, error)) error {
	s.st.LockBus()
	defer s.st.UnlockBus()
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		r, topic, err := write(tx)
		if err != nil {
			return err
		}
		_, err = store.AppendInTx(tx, frame(r, topic))
		return err
	})
	if err == nil {
		s.st.NotifyBus()
	}
	return err
}

// frame is the thin bus frame of one row change.
func frame(r Row, topic string) store.BusEvent {
	attrs := map[string]string{
		"kind": string(r.Kind), "purpose": string(r.Purpose), "row_id": r.ID,
	}
	for k, v := range map[string]string{
		"workspace": r.WorkspaceID, "connector": r.Connector(), "instance": r.Instance,
		"conversation_id": r.ConversationID, "inbox_id": r.InboxID,
	} {
		if v != "" {
			attrs[k] = v
		}
	}
	if r.RouteStep != nil {
		attrs["route_step"] = strconv.Itoa(*r.RouteStep)
	}
	if topic == "outbox.failed" && r.ErrorMessage != "" {
		attrs["error"] = r.ErrorMessage
	}
	action := "updated"
	if topic == "outbox.queued" && r.Revision == 1 {
		action = "created"
	}
	return store.BusEvent{
		Topic: topic, Entity: "outbox", EntityID: r.ID, Action: action,
		Revision: r.Revision, Attrs: attrs,
	}
}

func (s *Service) releaseStale(ctx context.Context, now time.Time) error {
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE outbox SET status='pending', claimed_at=NULL WHERE status='sending' AND claimed_at < ?`,
		fmtTS(now.Add(-staleClaim))); err != nil {
		return fmt.Errorf("outbox release stale: %w", err)
	}
	return nil
}

func (s *Service) dueIDs(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.st.DB().QueryContext(ctx, `
SELECT id FROM outbox
WHERE status='pending' AND hold_reason='' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
ORDER BY id LIMIT ?`, fmtTS(now), dueBatch)
	if err != nil {
		return nil, fmt.Errorf("outbox due: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) byDedupe(ctx context.Context, key string) (Row, error) {
	r, err := scanRow(s.st.DB().QueryRowContext(ctx, `SELECT `+rowCols+` FROM outbox WHERE dedupe_key=?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	return r, err
}

const rowCols = `id, kind, purpose, workspace_id, instance, room, thread, recipients, subject, COALESCE(body,''), env_json,
COALESCE(conversation_id,''), COALESCE(run_id,''), COALESCE(inbox_id,''), COALESCE(email_row_id,''),
COALESCE(dedupe_key,''), route_step, COALESCE(route_json,''), status, hold_reason, attempts, max_attempts,
COALESCE(next_attempt_at,''), COALESCE(claimed_at,''), COALESCE(error_message,''), created_at,
COALESCE(sent_at,''), revision`

type scanner interface{ Scan(dest ...any) error }

func scanRow(sc scanner) (Row, error) {
	var r Row
	var kind, purpose, status, env, route, next, claimed, created, sent string
	var step sql.NullInt64
	if err := sc.Scan(&r.ID, &kind, &purpose, &r.WorkspaceID, &r.Instance, &r.Room, &r.Thread,
		&r.Recipients, &r.Subject, &r.Body, &env, &r.ConversationID, &r.RunID, &r.InboxID, &r.EmailRowID,
		&r.DedupeKey, &step, &route, &status, &r.HoldReason, &r.Attempts, &r.MaxAttempts, &next, &claimed, &r.ErrorMessage,
		&created, &sent, &r.Revision); err != nil {
		return Row{}, err
	}
	r.Kind, r.Purpose, r.Status = Kind(kind), Purpose(purpose), Status(status)
	if env != "" && env != "{}" {
		// The column is always json.Marshal output; a bad value degrades to none.
		_ = json.Unmarshal([]byte(env), &r.Env)
	}
	if route != "" {
		_ = json.Unmarshal([]byte(route), &r.Route) // same as env_json
	}
	if step.Valid {
		v := int(step.Int64)
		r.RouteStep = &v
	}
	r.NextAttemptAt, r.ClaimedAt, r.SentAt = parseOpt(next), parseOpt(claimed), parseOpt(sent)
	r.CreatedAt = store.ParseTS(created)
	return r, nil
}

func parseOpt(s string) *time.Time {
	if s == "" {
		return nil
	}
	t := store.ParseTS(s)
	return &t
}

// fmtTS writes a fixed-width UTC time, so text comparison orders it.
func fmtTS(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

func nonNilEnv(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func truncate(msg string) string {
	if len(msg) > errorMessageMx {
		return msg[:errorMessageMx]
	}
	return msg
}

func repeatPlaceholders(n int) string {
	out := make([]byte, 0, 2*n)
	for range n {
		out = append(out, ",?"...)
	}
	return string(out)
}

func (s *Service) sender(k Kind) (Sender, Policy, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snd, ok := s.senders[k]
	return snd, s.policies[k], ok
}
