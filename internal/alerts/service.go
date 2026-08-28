// Package alerts implements AlertService → inbox items (backend-0808 §7).
package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/store"
)

// ErrNotFound is returned when an inbox item id does not exist.
var ErrNotFound = errors.New("not found")

// Kind is a closed alert kind.
type Kind string

const (
	KindWaiting              Kind = "waiting"
	KindAuthMissing          Kind = "auth_missing"
	KindLimitExceeded        Kind = "limit_exceeded"
	KindUsageWarning         Kind = "usage_warning"
	KindIntegrationUnhealthy Kind = "integration_unhealthy"
	KindRunFailed            Kind = "run_failed"
	KindResumeExhausted      Kind = "resume_exhausted"
	KindMachineDown          Kind = "machine_down"
)

// InboxKind is the inbox bucket an alert lands in.
type InboxKind string

const (
	InboxApproval InboxKind = "approval"
	InboxFailure  InboxKind = "failure"
	InboxHealth   InboxKind = "health"
	InboxWatchdog InboxKind = "watchdog"
)

// InboxStatus is the durable read/resolved state.
type InboxStatus string

const (
	InboxUnread   InboxStatus = "unread"
	InboxRead     InboxStatus = "read"
	InboxResolved InboxStatus = "resolved"
)

// Severity is the §7 paging severity.
type Severity string

const (
	SeverityAttention Severity = "attention"
	SeverityError     Severity = "error"
	SeverityBlocker   Severity = "blocker"
	SeverityCritical  Severity = "critical"
)

// Action kinds — typed so [fix] is executable, not a label.
const (
	ActionOpenSession = "open_session"
	ActionViewRun     = "view_run"
	ActionRetry       = "retry"
	ActionReAuth      = "re_auth"
	ActionFix         = "fix"
)

// InboxAction is one executable action on an inbox item (§3.12).
type InboxAction struct {
	Kind  string `json:"kind"`
	Route string `json:"route"`
	Label string `json:"label"`
}

// InboxItem is the §3.12 wire struct.
type InboxItem struct {
	ID          string        `json:"id"`
	Kind        InboxKind     `json:"kind"`
	Status      InboxStatus   `json:"status"`
	Subject     string        `json:"subject,omitempty"`
	Title       string        `json:"title"`
	Body        string        `json:"body,omitempty"`
	Hint        string        `json:"hint,omitempty"`
	Workspace   string        `json:"workspace,omitempty"`
	Job         string        `json:"job,omitempty"`
	RunID       string        `json:"run_id,omitempty"`
	Integration string        `json:"integration,omitempty"`
	AlertKind   string        `json:"alert_kind,omitempty"`
	EscalatesAt *time.Time    `json:"escalates_at,omitempty"`
	Actions     []InboxAction `json:"actions"`
	CreatedAt   time.Time     `json:"created_at"`
	ResolvedAt  *time.Time    `json:"resolved_at,omitempty"`
	TraceID     string        `json:"trace_id"`
	Revision    int64         `json:"revision"`
	UpdatedAt   time.Time     `json:"updated_at,omitempty"`
}

// KindSpec is one row of the §7 registry.
type KindSpec struct {
	Kind       Kind
	Severity   Severity
	InboxKind  InboxKind
	Cooldown   time.Duration
	EndReasons []string // B20 allow-list; empty = no gate
}

// RaiseOpts is the ListOpts-style argument to Raise.
type RaiseOpts struct {
	Kind        Kind
	Subject     string
	Title       string
	Body        string
	Hint        string
	RunID       string
	WorkspaceID string
	JobName     string
	Integration string
	Harness     string
	TraceID     string
	EndReason   string
	SessionID   string
}

// ListOpts filters inbox lists.
type ListOpts struct {
	Status string
	Kind   string
	Limit  int
	Cursor string
}

// Options configures the consumer and notifier.
type Options struct {
	TickInterval    time.Duration
	WaitingAfter    time.Duration
	WaitingDisabled bool
	UsageWarnAt     int
	Notify          []NotifyChannel
	Now             func() time.Time
	Doctor          func() []DoctorFinding
}

// NotifyChannel is one named shell template in the notifier chain.
type NotifyChannel struct {
	Name    string
	Command string
}

// DoctorFinding is one system doctor row that may land in the inbox.
type DoctorFinding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// Service consumes domain events and creates inbox items.
type Service struct {
	st   *store.Store
	log  *slog.Logger
	opts Options

	mu         sync.Mutex
	lastRaised map[string]time.Time
	digest     map[string]int
	waiting    map[string]waitingEp
}

type waitingEp struct {
	since   time.Time
	runID   string
	ws, job string
	trace   string
	raised  bool
}

type scanner interface {
	Scan(dest ...any) error
}

const inboxCols = `id, kind, status, COALESCE(subject,''), COALESCE(title,''), COALESCE(body,''),
	COALESCE(hint,''), COALESCE(run_id,''), COALESCE(workspace_id,''), COALESCE(job,''),
	COALESCE(alert_kind,''), COALESCE(integration,''), COALESCE(actions,''), COALESCE(trace_id,''),
	escalates_at, revision, created_at, updated_at, resolved_at`

// New creates an alerts service. opts may be zero.
func New(st *store.Store, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.UsageWarnAt <= 0 {
		opts.UsageWarnAt = 80
	}
	return &Service{
		st:         st,
		log:        slog.Default().With("source", "alerts"),
		opts:       opts,
		lastRaised: map[string]time.Time{},
		digest:     map[string]int{},
		waiting:    map[string]waitingEp{},
	}
}

// Raise creates or updates an inbox item after registry/dedupe/cooldown.
func (s *Service) Raise(ctx context.Context, opts RaiseOpts) (string, error) {
	spec, ok := Lookup(opts.Kind)
	if !ok {
		return "", fmt.Errorf("unknown alert kind %q", opts.Kind)
	}
	if !spec.allowsEndReason(opts.EndReason) {
		return "", nil
	}
	if opts.Subject == "" {
		opts.Subject = defaultSubject(opts)
	}
	if opts.Title == "" {
		opts.Title = defaultTitle(opts)
	}
	active, err := s.findActive(ctx, opts.Kind, opts.Subject)
	if err != nil {
		return "", err
	}
	if active != nil {
		if err := s.updateActive(ctx, active, opts); err != nil {
			return "", err
		}
		return active.ID, s.suppress(ctx, opts, "dedupe")
	}
	if s.inCooldown(spec, opts) {
		return "", s.suppress(ctx, opts, "cooldown")
	}
	return s.insertItem(ctx, spec, opts)
}

// List returns inbox items newest-first. Body is omitted (list stays thin).
func (s *Service) List(ctx context.Context, opts ListOpts) ([]InboxItem, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	if opts.Limit > 200 {
		opts.Limit = 200
	}
	q := `SELECT ` + inboxCols + ` FROM inbox_items WHERE 1=1`
	var args []any
	if opts.Status != "" {
		q += ` AND status=?`
		args = append(args, opts.Status)
	}
	if opts.Kind != "" {
		q += ` AND kind=?`
		args = append(args, opts.Kind)
	}
	if opts.Cursor != "" {
		q += ` AND id < ?`
		args = append(args, opts.Cursor)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, opts.Limit)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InboxItem{}
	for rows.Next() {
		item, err := scanInbox(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

// Get returns one inbox item including body.
func (s *Service) Get(ctx context.Context, id string) (*InboxItem, error) {
	row := s.st.DB().QueryRowContext(ctx, `SELECT `+inboxCols+` FROM inbox_items WHERE id=?`, id)
	item, err := scanInbox(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return item, err
}

// SetStatus updates one item. how is auto|manual when status is resolved.
func (s *Service) SetStatus(ctx context.Context, id string, status InboxStatus, how string) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if status == InboxResolved {
			res, err = tx.Exec(`UPDATE inbox_items SET status=?, updated_at=?, resolved_at=?, revision=revision+1 WHERE id=?`,
				string(status), nowStr, nowStr, id)
		} else {
			res, err = tx.Exec(`UPDATE inbox_items SET status=?, updated_at=?, revision=revision+1 WHERE id=?`,
				string(status), nowStr, id)
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	}, store.BusEvent{
		Topic: "inbox.updated", Entity: "inbox", EntityID: id, Action: "updated",
		Attrs: map[string]string{"status": string(status)},
	})
	if err != nil {
		return err
	}
	if status == InboxResolved {
		s.log.Info("inbox.resolved", "id", id, "how", how)
	}
	return nil
}

// ReadAll marks every unread item read.
func (s *Service) ReadAll(ctx context.Context) (int64, error) {
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE inbox_items SET status='read', updated_at=?, revision=revision+1 WHERE status='unread'`, nowStr)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, nil
	}
	_, err = s.st.Append(ctx, store.BusEvent{
		Topic: "inbox.updated", Entity: "inbox", Action: "updated",
		Attrs: map[string]string{"status": "read", "count": fmt.Sprint(n)},
	})
	return n, err
}

// ListAlerts returns alert.* bus rows, newest first, optionally since.
func (s *Service) ListAlerts(ctx context.Context, since time.Time, limit int) ([]store.StreamEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	events, err := s.st.ListBus(ctx, 0, []string{"alert"}, limit)
	if err != nil {
		return nil, err
	}
	if since.IsZero() {
		return events, nil
	}
	out := events[:0]
	for _, ev := range events {
		if !ev.TS.Before(since) {
			out = append(out, ev)
		}
	}
	return out, nil
}

// ResolveSubject auto-resolves the active item for subject, if any.
func (s *Service) ResolveSubject(ctx context.Context, subject, how string) error {
	item, err := s.findActiveBySubject(ctx, subject)
	if err != nil || item == nil {
		return err
	}
	return s.SetStatus(ctx, item.ID, InboxResolved, how)
}
func (s *Service) cooldownKey(opts RaiseOpts) string {
	return string(opts.Kind) + "|" + opts.Subject
}

func (s *Service) inCooldown(spec KindSpec, opts RaiseOpts) bool {
	if spec.Cooldown <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.lastRaised[s.cooldownKey(opts)]
	return ok && s.opts.Now().Sub(at) < spec.Cooldown
}

func (s *Service) markRaised(opts RaiseOpts) {
	s.mu.Lock()
	s.lastRaised[s.cooldownKey(opts)] = s.opts.Now()
	s.digest[s.cooldownKey(opts)] = 1
	s.mu.Unlock()
}

func (s *Service) suppress(ctx context.Context, opts RaiseOpts, reason string) error {
	_, err := s.st.Append(ctx, store.BusEvent{
		Topic: "alert.suppressed", Entity: "alert", EntityID: opts.Subject, Action: "suppressed",
		TraceID: opts.TraceID,
		Attrs: map[string]string{
			"kind": string(opts.Kind), "subject": opts.Subject, "reason": reason,
		},
	})
	return err
}

func (s *Service) findActive(ctx context.Context, kind Kind, subject string) (*InboxItem, error) {
	row := s.st.DB().QueryRowContext(ctx, `
		SELECT `+inboxCols+`
		FROM inbox_items WHERE alert_kind=? AND subject=? AND status != 'resolved'
		ORDER BY created_at DESC LIMIT 1`, string(kind), subject)
	item, err := scanInbox(row, false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return item, err
}

func (s *Service) insertItem(ctx context.Context, spec KindSpec, opts RaiseOpts) (string, error) {
	id := ulid.Make().String()
	now := s.opts.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	actions := defaultActions(opts)
	actJSON, _ := json.Marshal(actions)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_items (
			id, kind, status, subject, title, body, hint, run_id, workspace_id, job,
			alert_kind, integration, actions, trace_id, revision, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
			id, string(spec.InboxKind), string(InboxUnread), opts.Subject, opts.Title, opts.Body, opts.Hint,
			store.NullStr(opts.RunID), store.NullStr(opts.WorkspaceID), store.NullStr(opts.JobName),
			string(opts.Kind), store.NullStr(opts.Integration), string(actJSON), store.NullStr(opts.TraceID),
			nowStr, nowStr)
		return err
	}, store.BusEvent{
		Topic: "alert.raised", Entity: "alert", EntityID: id, Action: "created",
		TraceID: opts.TraceID, Revision: 1,
		Attrs: map[string]string{
			"kind": string(opts.Kind), "subject": opts.Subject, "message": opts.Title, "hint": opts.Hint,
		},
	})
	if err != nil {
		return "", err
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "inbox.created", Entity: "inbox", EntityID: id, Action: "created", Revision: 1,
		TraceID: opts.TraceID,
		Attrs:   map[string]string{"kind": string(spec.InboxKind), "status": "unread"},
	}); err != nil {
		s.log.Warn("bus append", "topic", "inbox.created", "error", err)
	}
	s.markRaised(opts)
	s.deliver(ctx, opts, id)
	return id, nil
}

func (s *Service) updateActive(ctx context.Context, item *InboxItem, opts RaiseOpts) error {
	s.mu.Lock()
	s.digest[s.cooldownKey(opts)]++
	n := s.digest[s.cooldownKey(opts)]
	s.mu.Unlock()
	title := opts.Title
	if n > 1 {
		title = fmt.Sprintf("%s (%d×)", opts.Title, n)
	}
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	actions := defaultActions(opts)
	actJSON, _ := json.Marshal(actions)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE inbox_items SET title=?, body=?, hint=?, actions=?,
			revision=revision+1, updated_at=? WHERE id=?`,
			title, opts.Body, opts.Hint, string(actJSON), nowStr, item.ID)
		return err
	}, store.BusEvent{
		Topic: "inbox.updated", Entity: "inbox", EntityID: item.ID, Action: "updated",
		Attrs: map[string]string{"status": string(item.Status)},
	})
	return err
}

func (s *Service) autoResolve(ctx context.Context, kind Kind, subject, how string) {
	item, err := s.findActive(ctx, kind, subject)
	if err != nil || item == nil {
		return
	}
	if err := s.SetStatus(ctx, item.ID, InboxResolved, how); err != nil {
		s.log.Warn("auto-resolve", "kind", kind, "subject", subject, "error", err)
	}
}

func (k Kind) String() string { return string(k) }

func (s KindSpec) allowsEndReason(reason string) bool {
	if len(s.EndReasons) == 0 {
		return true
	}
	for _, r := range s.EndReasons {
		if r == reason {
			return true
		}
	}
	return false
}

// Lookup returns the registry row for kind.
func Lookup(kind Kind) (KindSpec, bool) {
	s, ok := kindSpecs()[kind]
	return s, ok
}

func kindSpecs() map[Kind]KindSpec { //nolint:funlen // static §7 table
	return map[Kind]KindSpec{
		KindWaiting: {
			Kind: KindWaiting, Severity: SeverityAttention, InboxKind: InboxApproval,
		},
		KindAuthMissing: {
			Kind: KindAuthMissing, Severity: SeverityBlocker, InboxKind: InboxHealth,
			Cooldown: 5 * time.Minute,
		},
		KindLimitExceeded: {
			Kind: KindLimitExceeded, Severity: SeverityBlocker, InboxKind: InboxHealth,
			Cooldown: 15 * time.Minute,
		},
		KindUsageWarning: {
			Kind: KindUsageWarning, Severity: SeverityAttention, InboxKind: InboxHealth,
		},
		KindIntegrationUnhealthy: {
			Kind: KindIntegrationUnhealthy, Severity: SeverityBlocker, InboxKind: InboxHealth,
			Cooldown: 8 * time.Hour,
		},
		KindRunFailed: {
			Kind: KindRunFailed, Severity: SeverityError, InboxKind: InboxFailure,
			Cooldown:   time.Hour,
			EndReasons: []string{"idle_timeout", "max_duration", "failed", "session_gone", "launch"},
		},
		KindResumeExhausted: {
			Kind: KindResumeExhausted, Severity: SeverityError, InboxKind: InboxFailure,
		},
		KindMachineDown: {
			Kind: KindMachineDown, Severity: SeverityCritical, InboxKind: InboxWatchdog,
		},
	}
}

func defaultSubject(opts RaiseOpts) string {
	switch opts.Kind {
	case KindWaiting:
		return "run/" + opts.RunID
	case KindAuthMissing:
		if opts.RunID != "" {
			return "run/" + opts.RunID
		}
		return "harness/" + opts.Harness
	case KindLimitExceeded, KindUsageWarning:
		return "harness/" + firstNonEmpty(opts.Harness, opts.Integration)
	case KindIntegrationUnhealthy:
		return "integration/" + opts.Integration
	case KindRunFailed:
		return "job/" + opts.WorkspaceID + "/" + opts.JobName
	case KindResumeExhausted:
		if opts.SessionID != "" {
			return "session/" + opts.SessionID
		}
		return "run/" + opts.RunID
	case KindMachineDown:
		return "machine"
	}
	return opts.Kind.String()
}

func defaultTitle(opts RaiseOpts) string {
	switch opts.Kind {
	case KindWaiting:
		return "Agent waiting"
	case KindAuthMissing:
		return "Auth missing"
	case KindLimitExceeded:
		return "Harness limit exceeded"
	case KindUsageWarning:
		return "Harness usage warning"
	case KindIntegrationUnhealthy:
		return "Integration unhealthy"
	case KindRunFailed:
		return "Run failed"
	case KindResumeExhausted:
		return "Resume exhausted"
	case KindMachineDown:
		return "Machine down"
	}
	return string(opts.Kind)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func defaultActions(opts RaiseOpts) []InboxAction {
	switch opts.Kind {
	case KindWaiting:
		if opts.RunID == "" {
			return []InboxAction{}
		}
		return []InboxAction{{Kind: ActionOpenSession, Route: "/v1/runs/" + opts.RunID + "/attach", Label: "Open session"}}
	case KindRunFailed:
		return runFailedActions(opts.RunID)
	case KindAuthMissing:
		name := firstNonEmpty(opts.Harness, opts.Integration)
		if name == "" {
			return []InboxAction{}
		}
		return []InboxAction{{Kind: ActionReAuth, Route: "/v1/harnesses/" + name + "/auth", Label: "Re-auth"}}
	}
	return []InboxAction{}
}

func runFailedActions(runID string) []InboxAction {
	if runID == "" {
		return []InboxAction{}
	}
	return []InboxAction{
		{Kind: ActionViewRun, Route: "/v1/runs/" + runID, Label: "View run"},
		{Kind: ActionRetry, Route: "/v1/runs/" + runID + "/retry", Label: "Retry"},
	}
}

func scanInbox(row scanner, includeBody bool) (*InboxItem, error) {
	var it InboxItem
	var sub, title, body, hint, runID, ws, job, ak, integ, acts, trace string
	var esc, resolved sql.NullString
	var created, updated string
	err := row.Scan(&it.ID, &it.Kind, &it.Status, &sub, &title, &body, &hint, &runID, &ws, &job,
		&ak, &integ, &acts, &trace, &esc, &it.Revision, &created, &updated, &resolved)
	if err != nil {
		return nil, err
	}
	it.Subject, it.Title, it.Hint = sub, title, hint
	it.RunID, it.Workspace, it.Job = runID, ws, job
	it.AlertKind, it.Integration, it.TraceID = ak, integ, trace
	if includeBody {
		it.Body = body
	}
	it.Actions = unmarshalActions(acts)
	it.CreatedAt = parseTime(created)
	it.UpdatedAt = parseTime(updated)
	if resolved.Valid {
		t := parseTime(resolved.String)
		it.ResolvedAt = &t
	}
	if esc.Valid {
		t := parseTime(esc.String)
		it.EscalatesAt = &t
	}
	return &it, nil
}

func unmarshalActions(raw string) []InboxAction {
	out := []InboxAction{}
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	if out == nil {
		return []InboxAction{}
	}
	return out
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, s)
	}
	return t
}
