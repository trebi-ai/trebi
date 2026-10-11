package alerts

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/store"
)

// ErrInvalid wraps a bad inbox filter or apply request.
var ErrInvalid = errors.New("invalid")

// MaxApplyIDs is the most ids one Apply call takes.
const MaxApplyIDs = 500

// InboxView selects open items (inbox) or closed items (done).
type InboxView string

const (
	ViewInbox InboxView = "inbox"
	ViewDone  InboxView = "done"
)

// InboxOp is one change of the read or Done state. OpUpdate and OpPrune
// occur only on the bus.
type InboxOp string

const (
	OpRead      InboxOp = "read"
	OpUnread    InboxOp = "unread"
	OpArchive   InboxOp = "archive"
	OpUnarchive InboxOp = "unarchive"
	OpUpdate    InboxOp = "update"
	OpPrune     InboxOp = "prune"
)

// Who closed an item.
const (
	DoneBySystem = "system"
	DoneByUser   = "user"
)

// InboxItem is the §3.12 wire struct. Read, Done, and NeedsAction are
// derived from the stored state.
type InboxItem struct {
	ID             string        `json:"id"`
	Kind           InboxKind     `json:"kind"`
	Read           bool          `json:"read"`
	Done           bool          `json:"done"`
	NeedsAction    bool          `json:"needs_action"`
	ReadAt         *time.Time    `json:"read_at,omitempty"`
	DoneAt         *time.Time    `json:"done_at,omitempty"`
	DoneBy         string        `json:"done_by,omitempty"`
	Subject        string        `json:"subject,omitempty"`
	Title          string        `json:"title"`
	Body           string        `json:"body,omitempty"`
	Hint           string        `json:"hint,omitempty"`
	Workspace      string        `json:"workspace,omitempty"`
	JobID          string        `json:"job_id,omitempty"`
	JobName        string        `json:"job_name,omitempty"`
	RunID          string        `json:"run_id,omitempty"`
	Connector      string        `json:"connector,omitempty"`
	AlertKind      string        `json:"alert_kind,omitempty"`
	EmailMessageID string        `json:"email_message_id,omitempty"`
	ProposalID     string        `json:"proposal_id,omitempty"`
	RequestID      string        `json:"request_id,omitempty"`
	EscalatesAt    *time.Time    `json:"escalates_at,omitempty"`
	Actions        []InboxAction `json:"actions"`
	CreatedAt      time.Time     `json:"created_at"`
	TraceID        string        `json:"trace_id"`
	Delivery       string        `json:"delivery,omitempty"`
	Deliveries     []Delivery    `json:"deliveries,omitempty"`
	Revision       int64         `json:"revision"`
	UpdatedAt      time.Time     `json:"updated_at,omitempty"`
}

// InboxFilter selects items for List and for Apply. A zero View is the inbox.
type InboxFilter struct {
	View      InboxView `json:"view,omitempty"`
	Kind      InboxKind `json:"kind,omitempty"`
	Workspace string    `json:"workspace,omitempty"`
	Unread    bool      `json:"unread,omitempty"`
	Query     string    `json:"q,omitempty"`
}

// ListOpts is one page of a filtered inbox list.
type ListOpts struct {
	InboxFilter
	Limit  int
	Cursor string
}

// InboxCounts are the badge counts of one workspace scope.
type InboxCounts struct {
	Inbox  int               `json:"inbox"`
	Done   int               `json:"done"`
	Unread map[InboxKind]int `json:"unread"` // open and unread, per kind
}

// InboxPage is one List result.
type InboxPage struct {
	Items      []InboxItem `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
	Counts     InboxCounts `json:"counts"`
}

// InboxSelector names the items of one Apply: ids or a filter, not both.
type InboxSelector struct {
	IDs     []string
	Filter  *InboxFilter
	Confirm bool
}

// PendingRef is an open approval or request that an archive would hide.
type PendingRef struct {
	ID        string    `json:"id"`
	Kind      InboxKind `json:"kind"`
	AlertKind string    `json:"alert_kind"`
	Title     string    `json:"title"`
	RunID     string    `json:"run_id,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
}

// InboxApplyResult lists the changed ids. Pending is set, and nothing
// changes, when an archive without confirm holds open approvals or requests.
type InboxApplyResult struct {
	Updated []string     `json:"updated"`
	Pending []PendingRef `json:"pending,omitempty"`
}

// InboxUpdate is the attrs of an inbox.updated bus frame.
type InboxUpdate struct {
	Op    InboxOp  `json:"op"`
	IDs   []string `json:"ids"`
	Count int      `json:"count"`
	By    string   `json:"by,omitempty"`
}

func inboxKinds() []InboxKind {
	return []InboxKind{InboxApproval, InboxRequest, InboxFailure, InboxHealth, InboxWatchdog, InboxEscalation}
}

// pendingAlertKinds hold a run or a request that waits for an answer.
func pendingAlertKind(k string) bool {
	return k == string(KindWaiting) || k == string(KindAgentProposal) || k == string(KindRequest)
}

// needsAction is the "Needs you" rule: an open approval or request, or an
// open blocker or critical alert.
func needsAction(kind InboxKind, alertKind string, done bool) bool {
	if done {
		return false
	}
	if kind == InboxApproval || kind == InboxRequest {
		return true
	}
	spec, ok := Lookup(Kind(alertKind))
	return ok && (spec.Severity == SeverityBlocker || spec.Severity == SeverityCritical)
}

// Check validates the view and the kind.
func (f InboxFilter) Check() error {
	if f.View != "" && f.View != ViewInbox && f.View != ViewDone {
		return fmt.Errorf("%w: view %q: want inbox or done", ErrInvalid, f.View)
	}
	if f.Kind != "" && !slices.Contains(inboxKinds(), f.Kind) {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalid, f.Kind)
	}
	return nil
}

// where is the SQL condition of the filter, after "WHERE 1=1".
func (f InboxFilter) where() (string, []any) {
	q := ` AND done_at IS NULL`
	if f.View == ViewDone {
		q = ` AND done_at IS NOT NULL`
	}
	var args []any
	if f.Workspace != "" {
		q += ` AND (workspace_id = ? OR workspace_id = '' OR workspace_id IS NULL)`
		args = append(args, f.Workspace)
	}
	if f.Kind != "" {
		q += ` AND kind=?`
		args = append(args, string(f.Kind))
	}
	if f.Unread {
		q += ` AND read_at IS NULL`
	}
	if text := strings.TrimSpace(f.Query); text != "" {
		like := "%" + likeEscaper.Replace(strings.ToLower(text)) + "%"
		q += ` AND (LOWER(COALESCE(title,'')) LIKE ? ESCAPE '\' OR LOWER(COALESCE(subject,'')) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(body,'')) LIKE ? ESCAPE '\')`
		args = append(args, like, like, like)
	}
	return q, args
}

//nolint:gochecknoglobals // immutable replacer
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (o InboxOp) valid() bool {
	return o == OpRead || o == OpUnread || o == OpArchive || o == OpUnarchive
}

// changes reports whether the op changes a row in this state.
func (o InboxOp) changes(read, done bool) bool {
	switch o {
	case OpRead:
		return !read
	case OpUnread:
		return read
	case OpArchive:
		return !done
	case OpUnarchive:
		return done
	}
	return false
}

// set is the SET clause of the op.
func (o InboxOp) set(now string) (string, []any) {
	switch o {
	case OpRead:
		return `read_at=?`, []any{now}
	case OpUnread:
		return `read_at=NULL`, nil
	case OpArchive:
		return `done_at=?, done_by=?`, []any{now, DoneByUser}
	default:
		return `done_at=NULL, done_by=NULL`, nil
	}
}

func (sel InboxSelector) check() error {
	switch {
	case len(sel.IDs) > 0 && sel.Filter != nil:
		return fmt.Errorf("%w: set ids or filter, not both", ErrInvalid)
	case len(sel.IDs) == 0 && sel.Filter == nil:
		return fmt.Errorf("%w: set ids or filter", ErrInvalid)
	case len(sel.IDs) > MaxApplyIDs:
		return fmt.Errorf("%w: %d ids; the limit is %d", ErrInvalid, len(sel.IDs), MaxApplyIDs)
	case sel.Filter != nil:
		return sel.Filter.Check()
	}
	return nil
}

const inboxCols = `id, kind, COALESCE(subject,''), COALESCE(title,''), COALESCE(body,''),
	COALESCE(hint,''), COALESCE(run_id,''), COALESCE(workspace_id,''), COALESCE(job_id,''),
	COALESCE((SELECT ji.name FROM job_index ji WHERE ji.id = inbox_items.job_id), ''),
	COALESCE(alert_kind,''), COALESCE(connector,''), COALESCE(actions,''), COALESCE(trace_id,''),
	COALESCE(email_message_id,''), escalates_at, revision, created_at, updated_at,
	read_at, done_at, COALESCE(done_by,''), COALESCE(delivery,''), COALESCE(request_id,'')`

func scanInbox(row scanner, includeBody bool) (*InboxItem, error) {
	var it InboxItem
	var body, acts string
	var esc, readAt, doneAt sql.NullString
	var created, updated string
	err := row.Scan(&it.ID, &it.Kind, &it.Subject, &it.Title, &body, &it.Hint, &it.RunID, &it.Workspace, &it.JobID, &it.JobName,
		&it.AlertKind, &it.Connector, &acts, &it.TraceID, &it.EmailMessageID, &esc, &it.Revision, &created, &updated,
		&readAt, &doneAt, &it.DoneBy, &it.Delivery, &it.RequestID)
	if err != nil {
		return nil, err
	}
	if id, ok := strings.CutPrefix(it.Subject, "proposal/"); ok {
		it.ProposalID = id
	}
	if includeBody {
		it.Body = body
	}
	it.Actions = unmarshalActions(acts)
	if it.Delivery == DeliveryFailed {
		it.Actions = append(it.Actions, InboxAction{Kind: ActionViewOutbox, Route: "/history/outbox?inbox=" + it.ID, Label: "View outbox"})
	}
	it.CreatedAt = parseTime(created)
	it.UpdatedAt = parseTime(updated)
	it.EscalatesAt = nullTime(esc)
	it.ReadAt = nullTime(readAt)
	it.DoneAt = nullTime(doneAt)
	it.Read, it.Done = it.ReadAt != nil, it.DoneAt != nil
	it.NeedsAction = needsAction(it.Kind, it.AlertKind, it.Done)
	return &it, nil
}

func nullTime(v sql.NullString) *time.Time {
	if !v.Valid {
		return nil
	}
	t := parseTime(v.String)
	return &t
}

// List returns one page newest first, with the badge counts of the
// workspace scope. Body is omitted (the list stays thin). The Done view
// sorts by close time.
func (s *Service) List(ctx context.Context, opts ListOpts) (InboxPage, error) {
	if err := opts.Check(); err != nil {
		return InboxPage{}, err
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	opts.Limit = min(opts.Limit, 200)
	where, args := opts.where()
	order := ` ORDER BY id DESC`
	if opts.Cursor != "" {
		if opts.View == ViewDone {
			where += ` AND (done_at < (SELECT done_at FROM inbox_items WHERE id=?)
				OR (done_at = (SELECT done_at FROM inbox_items WHERE id=?) AND id < ?))`
			args = append(args, opts.Cursor, opts.Cursor, opts.Cursor)
		} else {
			where += ` AND id < ?`
			args = append(args, opts.Cursor)
		}
	}
	if opts.View == ViewDone {
		order = ` ORDER BY done_at DESC, id DESC`
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+inboxCols+` FROM inbox_items WHERE 1=1`+where+order+` LIMIT ?`,
		append(args, opts.Limit)...)
	if err != nil {
		return InboxPage{}, err
	}
	defer rows.Close()
	page := InboxPage{Items: []InboxItem{}}
	for rows.Next() {
		item, err := scanInbox(rows, false)
		if err != nil {
			return InboxPage{}, err
		}
		page.Items = append(page.Items, *item)
	}
	if err := rows.Err(); err != nil {
		return InboxPage{}, err
	}
	if len(page.Items) == opts.Limit {
		page.NextCursor = page.Items[len(page.Items)-1].ID
	}
	page.Counts, err = s.Counts(ctx, opts.Workspace)
	return page, err
}

// Counts returns the badge counts. A set workspace keeps its rows plus
// global rows.
func (s *Service) Counts(ctx context.Context, workspace string) (InboxCounts, error) {
	scope, args := "", []any{}
	if workspace != "" {
		scope = ` AND (workspace_id = ? OR workspace_id = '' OR workspace_id IS NULL)`
		args = append(args, workspace)
	}
	c := InboxCounts{Unread: map[InboxKind]int{}}
	for _, k := range inboxKinds() {
		c.Unread[k] = 0
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT kind, done_at IS NOT NULL, read_at IS NOT NULL, COUNT(*)
		FROM inbox_items WHERE 1=1`+scope+` GROUP BY 1, 2, 3`, args...)
	if err != nil {
		return c, fmt.Errorf("inbox counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind InboxKind
		var done, read bool
		var n int
		if err := rows.Scan(&kind, &done, &read, &n); err != nil {
			return c, err
		}
		switch {
		case done:
			c.Done += n
		case !read:
			c.Inbox += n
			c.Unread[kind] += n
		default:
			c.Inbox += n
		}
	}
	return c, rows.Err()
}

// Get returns one inbox item including body.
func (s *Service) Get(ctx context.Context, id string) (*InboxItem, error) {
	row := s.st.DB().QueryRowContext(ctx, `SELECT `+inboxCols+` FROM inbox_items WHERE id=?`, id)
	item, err := scanInbox(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil || s.opts.Outbox == nil {
		return item, err
	}
	rows, _, err := s.opts.Outbox.List(ctx, outbox.Filter{InboxID: id, Limit: 100})
	if err != nil {
		return nil, fmt.Errorf("inbox %s deliveries: %w", id, err)
	}
	for _, r := range rows {
		if r.RouteStep == nil {
			continue
		}
		item.Deliveries = append(item.Deliveries, Delivery{
			Step: *r.RouteStep, Via: cmp.Or(r.Instance, notify.ViaEmail), Target: cmp.Or(r.Room, r.Recipients),
			Status: string(r.Status), Error: r.ErrorMessage, SentAt: r.SentAt,
		})
	}
	slices.SortFunc(item.Deliveries, func(a, b Delivery) int { return cmp.Compare(a.Step, b.Step) })
	return item, nil
}

// applyRow is one candidate row of an Apply.
type applyRow struct {
	PendingRef
	read, done bool
}

// Apply runs op on the selected items in one transaction and emits one
// inbox.updated frame. Rows that the op does not change are left out of
// Updated. An unknown id fails the whole call with ErrNotFound.
func (s *Service) Apply(ctx context.Context, op InboxOp, sel InboxSelector) (InboxApplyResult, error) {
	res := InboxApplyResult{Updated: []string{}}
	if !op.valid() {
		return res, fmt.Errorf("%w: op %q: want read, unread, archive, or unarchive", ErrInvalid, op)
	}
	if err := sel.check(); err != nil {
		return res, err
	}
	now := s.opts.Now().UTC().Format(time.RFC3339Nano)
	s.st.LockBus()
	defer s.st.UnlockBus()
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		cands, err := selectApplyRows(ctx, tx, sel)
		if err != nil {
			return err
		}
		var ids []string
		var pending []PendingRef
		for _, c := range cands {
			if !op.changes(c.read, c.done) {
				continue
			}
			ids = append(ids, c.ID)
			if op == OpArchive && pendingAlertKind(c.AlertKind) {
				pending = append(pending, c.PendingRef)
			}
		}
		if len(pending) > 0 && !sel.Confirm {
			res.Pending = pending
			return nil
		}
		if len(ids) == 0 {
			return nil
		}
		set, setArgs := op.set(now)
		for chunk := range slices.Chunk(ids, MaxApplyIDs) {
			args := append(append(slices.Clone(setArgs), now), anySlice(chunk)...)
			if _, err := tx.ExecContext(ctx, `UPDATE inbox_items SET `+set+`, updated_at=?, revision=revision+1
				WHERE id IN (`+placeholders(len(chunk))+`)`, args...); err != nil {
				return fmt.Errorf("inbox %s: %w", op, err)
			}
		}
		res.Updated = ids
		_, err = store.AppendInTx(tx, inboxUpdated(op, ids, ""))
		return err
	})
	return res, err
}

// selectApplyRows reads the candidate rows of a selector, newest first.
func selectApplyRows(ctx context.Context, tx *sql.Tx, sel InboxSelector) ([]applyRow, error) {
	q := `SELECT id, kind, COALESCE(alert_kind,''), COALESCE(title,''), COALESCE(run_id,''), COALESCE(request_id,''),
		read_at IS NOT NULL, done_at IS NOT NULL FROM inbox_items WHERE 1=1`
	var args []any
	if sel.Filter != nil {
		where, wargs := sel.Filter.where()
		q, args = q+where, wargs
	} else {
		q += ` AND id IN (` + placeholders(len(sel.IDs)) + `)`
		args = anySlice(sel.IDs)
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("inbox select: %w", err)
	}
	defer rows.Close()
	var out []applyRow
	seen := map[string]bool{}
	for rows.Next() {
		var r applyRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.AlertKind, &r.Title, &r.RunID, &r.RequestID, &r.read, &r.done); err != nil {
			return nil, err
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range sel.IDs {
		if !seen[id] {
			return nil, fmt.Errorf("inbox item %s: %w", id, ErrNotFound)
		}
	}
	return out, nil
}

// Close marks one open item Done by the system: its source no longer
// needs the user. The read state stays. how names the cause in the log.
func (s *Service) Close(ctx context.Context, id, how string) error {
	now := s.opts.Now().UTC().Format(time.RFC3339Nano)
	var changed bool
	s.st.LockBus()
	defer s.st.UnlockBus()
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE inbox_items SET done_at=?, done_by=?, updated_at=?, revision=revision+1
			WHERE id=? AND done_at IS NULL`, now, DoneBySystem, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM inbox_items WHERE id=?`, id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return nil
		}
		changed = true
		_, err = store.AppendInTx(tx, inboxUpdated(OpArchive, []string{id}, DoneBySystem))
		return err
	})
	if err == nil && changed {
		s.log.Info("inbox.resolved", "id", id, "how", how)
	}
	return err
}

// Prune deletes the Done items that closed before now minus the retention.
func (s *Service) Prune(ctx context.Context, now time.Time) (int, error) {
	cutoff := now.Add(-s.opts.DoneRetention).UTC().Format(time.RFC3339Nano)
	var ids []string
	s.st.LockBus()
	defer s.st.UnlockBus()
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM inbox_items WHERE done_at IS NOT NULL AND done_at < ? ORDER BY id`, cutoff)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close() // the scan error wins
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM inbox_items WHERE done_at IS NOT NULL AND done_at < ?`, cutoff); err != nil {
			return err
		}
		_, err = store.AppendInTx(tx, inboxUpdated(OpPrune, ids, ""))
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("inbox prune: %w", err)
	}
	return len(ids), nil
}

// prune runs Prune at most once an hour from Tick.
func (s *Service) prune(ctx context.Context) {
	now := s.opts.Now()
	s.mu.Lock()
	due := now.Sub(s.lastPrune) >= time.Hour
	if due {
		s.lastPrune = now
	}
	s.mu.Unlock()
	if !due {
		return
	}
	if n, err := s.Prune(ctx, now); err != nil {
		s.log.Warn("inbox prune", "error", err)
	} else if n > 0 {
		s.log.Info("inbox prune", "count", n)
	}
}

// inboxUpdated is the inbox.updated frame of one change. One id also sets
// the entity id.
func inboxUpdated(op InboxOp, ids []string, by string) store.BusEvent {
	ev := store.BusEvent{
		Topic: "inbox.updated", Entity: "inbox", Action: "updated",
		Attrs: InboxUpdate{Op: op, IDs: ids, Count: len(ids), By: by},
	}
	if len(ids) == 1 {
		ev.EntityID = ids[0]
	}
	return ev
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anySlice(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
