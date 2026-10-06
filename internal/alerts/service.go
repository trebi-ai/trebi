// Package alerts implements AlertService → inbox items (backend-0808 §7).
package alerts

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/requests"
	"github.com/trebi-ai/trebi/internal/store"
)

// ErrNotFound is returned when an inbox item id does not exist.
var ErrNotFound = errors.New("not found")

// Kind is a closed alert kind.
type Kind string

const (
	KindWaiting            Kind = "waiting"
	KindAuthMissing        Kind = "auth_missing"
	KindLimitExceeded      Kind = "limit_exceeded"
	KindUsageWarning       Kind = "usage_warning"
	KindConnectorUnhealthy Kind = "connector_unhealthy"
	KindRunFailed          Kind = "run_failed"
	KindAgentEscalation    Kind = "agent_escalation"
	KindBudgetExceeded     Kind = "budget_exceeded"
	KindMemoryInvalid      Kind = "memory_invalid"
	KindResumeExhausted    Kind = "resume_exhausted"
	KindMachineDown        Kind = "machine_down"
	KindAgentProposal      Kind = "agent_proposal"
	KindSendFailed         Kind = "send_failed"
	KindActionsInvalid     Kind = "actions_invalid"
	// KindConnectorAuthRequired is a channel that needs a login.
	KindConnectorAuthRequired Kind = "connector_auth_required"
	// KindConnectorUpdate is a catalog version that waits for the user.
	KindConnectorUpdate Kind = "connector_update_available"
	// KindRequest is a job request that waits for a person.
	KindRequest Kind = "request"
	// KindRequestUndelivered is an answer that could not reach the job.
	KindRequestUndelivered Kind = "request_delivery_failed"
	// KindTriggerFlood is a job whose queue is full, so Trebi drops events.
	KindTriggerFlood Kind = "trigger_flood"
)

// InboxKind is the inbox bucket an alert lands in.
type InboxKind string

const (
	InboxApproval   InboxKind = "approval"
	InboxFailure    InboxKind = "failure"
	InboxHealth     InboxKind = "health"
	InboxWatchdog   InboxKind = "watchdog"
	InboxEscalation InboxKind = "escalation"
	InboxRequest    InboxKind = "request"
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
	ActionReview      = "review"
	ActionViewOutbox  = "view_outbox"
	ActionLogIn       = "log_in"
	ActionUpdate      = "update"
)

// Delivery states of an inbox item. Empty means no delivery ran.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
	DeliveryInboxOnly = "inbox_only"
)

// Delivery is one outbox row of an alert route.
type Delivery struct {
	Step   int        `json:"step"`
	Via    string     `json:"via"`
	Target string     `json:"target,omitempty"`
	Status string     `json:"status"`
	Error  string     `json:"error,omitempty"`
	SentAt *time.Time `json:"sent_at,omitempty"`
}

// InboxAction is one executable action on an inbox item (§3.12).
type InboxAction struct {
	Kind  string `json:"kind"`
	Route string `json:"route"`
	Label string `json:"label"`
}

// InboxItem is the §3.12 wire struct.
type InboxItem struct {
	ID             string        `json:"id"`
	Kind           InboxKind     `json:"kind"`
	Status         InboxStatus   `json:"status"`
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
	ResolvedAt     *time.Time    `json:"resolved_at,omitempty"`
	TraceID        string        `json:"trace_id"`
	Delivery       string        `json:"delivery,omitempty"`
	Deliveries     []Delivery    `json:"deliveries,omitempty"`
	Revision       int64         `json:"revision"`
	UpdatedAt      time.Time     `json:"updated_at,omitempty"`
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
	// JobID and JobName name the job or agent. Raise fills one from the other.
	JobID     string
	JobName   string
	Connector string
	Harness   string
	TraceID   string
	EndReason string
	SessionID string
	RequestID string
	// EscalateAt arms the escalation sweep at this time instead of the
	// immediate notify chain (plan M5). Zero leaves the item tray-only.
	EscalateAt time.Time
	// TrayOnly skips every notify step.
	TrayOnly bool
}

// ListOpts filters inbox lists.
type ListOpts struct {
	Workspace string
	Status    string
	Kind      string
	Limit     int
	Cursor    string
}

// Options configures the consumer and notifier.
type Options struct {
	TickInterval    time.Duration
	WaitingAfter    time.Duration
	WaitingDisabled bool
	// ApprovalAfter is how long an approval item sits in the tray before it
	// mails (plan M5). Zero mails at once; ApprovalDisabled never mails.
	ApprovalAfter    time.Duration
	ApprovalDisabled bool
	// ApprovalQuestion returns the pending approval's question for the item
	// body, or "". Nil leaves the body empty.
	ApprovalQuestion func(ctx context.Context, runID string) string
	// Request returns a request for its inbox body and mail. Nil leaves the
	// body empty.
	Request func(ctx context.Context, id string) (requests.Request, error)
	// ProposalContent returns reviewed/proposed content for a proposal email.
	ProposalContent func(ctx context.Context, id string) (base, proposed string, ok bool)
	UsageWarnAt     int
	// Email writes alert mail rows. Nil leaves email steps unusable.
	Email *email.Service
	// Router resolves the notify route of an alert. Outbox holds and sends
	// its rows. A nil value of either keeps every alert in the inbox.
	Router *notify.Resolver
	Outbox *outbox.Service
	Shadow bool // consume bus / write inbox; skip outbound notify
	Drain  bool // same notify suppression as Shadow
	Now    func() time.Time
	Doctor func() []DoctorFinding
	// Actions checks connector actions in the doctor. Nil skips the check.
	Actions ActionsChecker
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

	deliverCh     chan deliverJob
	running       atomic.Bool
	failureRouter FailureRouter
}

// FailureRouter lets the owner of a job take a failure before the inbox.
type FailureRouter interface {
	RouteFailure(ctx context.Context, f RunFailure) (handled bool, err error)
}

type RunFailure struct {
	RunID, WorkspaceID, JobID, JobName, OwnerID, EndReason string
}

func (s *Service) SetFailureRouter(r FailureRouter) { s.failureRouter = r }

// SetDoctor wires the doctor snapshot provider to this service's store.
func (s *Service) SetDoctor(fn func() []DoctorFinding) { s.opts.Doctor = fn }

func (s *Service) SetProposalContent(fn func(ctx context.Context, id string) (string, string, bool)) {
	s.opts.ProposalContent = fn
}

type waitingEp struct {
	since   time.Time
	runID   string
	ws      string
	jobID   string
	jobName string
	trace   string
	raised  bool
}

type scanner interface {
	Scan(dest ...any) error
}

const inboxCols = `id, kind, status, COALESCE(subject,''), COALESCE(title,''), COALESCE(body,''),
	COALESCE(hint,''), COALESCE(run_id,''), COALESCE(workspace_id,''), COALESCE(job_id,''),
	COALESCE((SELECT ji.name FROM job_index ji WHERE ji.id = inbox_items.job_id), ''),
	COALESCE(alert_kind,''), COALESCE(connector,''), COALESCE(actions,''), COALESCE(trace_id,''),
	COALESCE(email_message_id,''), escalates_at, revision, created_at, updated_at, resolved_at,
	COALESCE(delivery,''), COALESCE(request_id,'')`

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
		deliverCh:  make(chan deliverJob, deliverQueueSize),
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
	if err := s.fillJob(ctx, &opts); err != nil {
		return "", err
	}
	if opts.Subject == "" {
		opts.Subject = opts.defaultSubject()
	}
	if opts.Title == "" {
		opts.Title = opts.defaultTitle()
	}
	opts.Title = sanitizeText(opts.Title)
	opts.Body = sanitizeText(opts.Body)
	opts.Hint = sanitizeText(opts.Hint)
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
	if opts.Workspace != "" {
		q += ` AND (workspace_id = ? OR workspace_id = '' OR workspace_id IS NULL)`
		args = append(args, opts.Workspace)
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

// ReadAll marks every unread item read. A set workspace scopes the update to
// that workspace plus global rows; empty covers everything.
func (s *Service) ReadAll(ctx context.Context, workspace string) (int64, error) {
	return s.setAll(ctx, "unread", InboxRead, "", workspace)
}

// ResolveAll resolves every open item. An empty kind covers every kind.
func (s *Service) ResolveAll(ctx context.Context, kind, workspace string) (int64, error) {
	return s.setAll(ctx, "", InboxResolved, kind, workspace)
}

// ListAlerts returns alert.* bus rows, newest first, optionally since.
// A set workspace keeps rows tagged with it plus untagged (global) rows.
func (s *Service) ListAlerts(ctx context.Context, since time.Time, limit int, workspace string) ([]store.StreamEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.st.ListBusWith(ctx, store.ListBusOpts{
		Topics:    []string{"alert"},
		Limit:     limit,
		Since:     since,
		Desc:      true,
		Workspace: workspace,
	})
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

// alertAttrs copies attrs and tags the alert's workspace when set. A global
// alert carries no attr, so json_extract filters treat it as global (A.1).
func alertAttrs(opts RaiseOpts, attrs map[string]string) map[string]string {
	if opts.WorkspaceID != "" {
		attrs["workspace"] = opts.WorkspaceID
	}
	return attrs
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
		Attrs: alertAttrs(opts, map[string]string{
			"kind": string(opts.Kind), "subject": opts.Subject, "reason": reason,
		}),
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
	actions := opts.defaultActions(id)
	actJSON, _ := json.Marshal(actions)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_items (
			id, kind, status, subject, title, body, hint, run_id, workspace_id, job_id,
			alert_kind, connector, actions, trace_id, request_id, revision, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
			id, string(spec.InboxKind), string(InboxUnread), opts.Subject, opts.Title, opts.Body, opts.Hint,
			store.NullStr(opts.RunID), store.NullStr(opts.WorkspaceID), store.NullStr(opts.JobID),
			string(opts.Kind), store.NullStr(opts.Connector), string(actJSON), store.NullStr(opts.TraceID),
			store.NullStr(opts.RequestID), nowStr, nowStr)
		return err
	}, store.BusEvent{
		Topic: "alert.raised", Entity: "alert", EntityID: id, Action: "created",
		TraceID: opts.TraceID, Revision: 1,
		Attrs: alertAttrs(opts, map[string]string{
			"kind": string(opts.Kind), "subject": opts.Subject, "message": opts.Title, "hint": opts.Hint,
		}),
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
	// An escalation-marked item skips the immediate notify chain: it sits in
	// the tray and the sweep mails it at EscalateAt (plan M5).
	if !opts.EscalateAt.IsZero() {
		esc := opts.EscalateAt.UTC().Format(time.RFC3339Nano)
		if _, err := s.st.DB().ExecContext(ctx, `UPDATE inbox_items SET escalates_at=? WHERE id=?`, esc, id); err != nil {
			s.log.Warn("escalate mark", "id", id, "error", err)
		}
		return id, nil
	}
	if opts.TrayOnly {
		return id, nil
	}
	s.enqueueDeliver(ctx, opts, id)
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
	actions := opts.defaultActions(item.ID)
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
		KindConnectorUnhealthy: {
			Kind: KindConnectorUnhealthy, Severity: SeverityBlocker, InboxKind: InboxHealth,
			Cooldown: 8 * time.Hour,
		},
		KindRunFailed: {
			Kind: KindRunFailed, Severity: SeverityError, InboxKind: InboxFailure,
			Cooldown:   time.Hour,
			EndReasons: []string{"idle_timeout", "max_duration", "failed", "session_gone", "launch"},
		},
		KindAgentEscalation: {
			Kind: KindAgentEscalation, Severity: SeverityAttention, InboxKind: InboxEscalation,
		},
		KindBudgetExceeded: {
			Kind: KindBudgetExceeded, Severity: SeverityAttention, InboxKind: InboxHealth,
			Cooldown: 720 * time.Hour,
		},
		KindMemoryInvalid: {
			Kind: KindMemoryInvalid, Severity: SeverityAttention, InboxKind: InboxHealth,
			Cooldown: 24 * time.Hour,
		},
		KindResumeExhausted: {
			Kind: KindResumeExhausted, Severity: SeverityError, InboxKind: InboxFailure,
		},
		KindMachineDown: {
			Kind: KindMachineDown, Severity: SeverityCritical, InboxKind: InboxWatchdog,
		},
		KindAgentProposal: {
			Kind: KindAgentProposal, Severity: SeverityAttention, InboxKind: InboxApproval,
		},
		KindSendFailed: {
			Kind: KindSendFailed, Severity: SeverityError, InboxKind: InboxFailure,
			Cooldown: 10 * time.Minute,
		},
		KindActionsInvalid: {
			Kind: KindActionsInvalid, Severity: SeverityError, InboxKind: InboxHealth,
			Cooldown: time.Hour,
		},
		KindConnectorAuthRequired: {
			Kind: KindConnectorAuthRequired, Severity: SeverityBlocker, InboxKind: InboxHealth,
		},
		KindTriggerFlood: {
			Kind: KindTriggerFlood, Severity: SeverityAttention, InboxKind: InboxHealth,
			Cooldown: time.Hour,
		},
		KindConnectorUpdate: {
			Kind: KindConnectorUpdate, Severity: SeverityAttention, InboxKind: InboxHealth,
		},
		KindRequest: {
			Kind: KindRequest, Severity: SeverityAttention, InboxKind: InboxRequest,
		},
		KindRequestUndelivered: {
			Kind: KindRequestUndelivered, Severity: SeverityError, InboxKind: InboxFailure,
		},
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
	var sub, title, body, hint, runID, ws, jobID, jobName, ak, connector, acts, trace, emailMsgID string
	var esc, resolved sql.NullString
	var created, updated string
	err := row.Scan(&it.ID, &it.Kind, &it.Status, &sub, &title, &body, &hint, &runID, &ws, &jobID, &jobName,
		&ak, &connector, &acts, &trace, &emailMsgID, &esc, &it.Revision, &created, &updated, &resolved, &it.Delivery, &it.RequestID)
	if err != nil {
		return nil, err
	}
	it.Subject, it.Title, it.Hint = sub, title, hint
	it.RunID, it.Workspace, it.JobID, it.JobName = runID, ws, jobID, jobName
	it.AlertKind, it.Connector, it.TraceID = ak, connector, trace
	it.EmailMessageID = emailMsgID
	if strings.HasPrefix(sub, "proposal/") {
		it.ProposalID = strings.TrimPrefix(sub, "proposal/")
	}
	if includeBody {
		it.Body = body
	}
	it.Actions = unmarshalActions(acts)
	if it.Delivery == DeliveryFailed {
		it.Actions = append(it.Actions, InboxAction{Kind: ActionViewOutbox, Route: "/outbox?inbox=" + it.ID, Label: "View outbox"})
	}
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

func parseTime(s string) time.Time { return store.ParseTS(s) }

// jobRef is the job part of a subject: the ID, which a rename keeps.
func (o RaiseOpts) jobRef() string { return firstNonEmpty(o.JobID, o.JobName) }

// fillJob resolves JobID from JobName (an index name or a short agent name)
// or JobName from JobID. A name with no live row keeps an empty JobID.
func (s *Service) fillJob(ctx context.Context, o *RaiseOpts) error {
	db := s.st.DB()
	switch {
	case o.JobID == "" && o.JobName != "" && o.WorkspaceID != "":
		err := db.QueryRowContext(ctx, `SELECT id FROM job_index WHERE workspace_id=? AND (name=? OR name=?) AND retired_at IS NULL
			ORDER BY kind LIMIT 1`, o.WorkspaceID, o.JobName, "agents/"+o.JobName).Scan(&o.JobID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	case o.JobID != "" && o.JobName == "":
		err := db.QueryRowContext(ctx, `SELECT name FROM job_index WHERE id=?`, o.JobID).Scan(&o.JobName)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}

func (o RaiseOpts) defaultSubject() string {
	switch o.Kind {
	case KindWaiting:
		return "run/" + o.RunID
	case KindAuthMissing:
		return "harness/" + o.Harness
	case KindLimitExceeded, KindUsageWarning:
		return "harness/" + firstNonEmpty(o.Harness, o.Connector)
	case KindConnectorUnhealthy, KindConnectorAuthRequired, KindConnectorUpdate:
		return "connector/" + o.Connector
	case KindRunFailed:
		return "job/" + o.WorkspaceID + "/" + o.jobRef()
	case KindAgentEscalation:
		return "agent/" + o.WorkspaceID + "/" + o.jobRef()
	case KindBudgetExceeded:
		return "budget/" + o.WorkspaceID + "/" + o.jobRef()
	case KindMemoryInvalid:
		return "memory/" + o.WorkspaceID + "/" + o.jobRef()
	case KindResumeExhausted:
		if o.SessionID != "" {
			return "session/" + o.SessionID
		}
		return "run/" + o.RunID
	case KindMachineDown:
		return "machine"
	case KindSendFailed:
		return "outbox"
	case KindActionsInvalid:
		return "actions/" + o.WorkspaceID + "/" + o.jobRef()
	case KindTriggerFlood:
		return "flood/" + o.WorkspaceID + "/" + o.jobRef()
	case KindRequest, KindRequestUndelivered:
		return "request/" + o.RequestID
	}
	return o.Kind.String()
}

//nolint:gochecknoglobals // static table
var defaultTitles = map[Kind]string{
	KindWaiting:               "Agent waiting",
	KindAuthMissing:           "Auth missing",
	KindLimitExceeded:         "Harness limit exceeded",
	KindUsageWarning:          "Harness usage warning",
	KindConnectorUnhealthy:    "Connector unhealthy",
	KindRunFailed:             "Run failed",
	KindMemoryInvalid:         "Memory invalid",
	KindResumeExhausted:       "Resume exhausted",
	KindMachineDown:           "Machine down",
	KindSendFailed:            "Send failed",
	KindActionsInvalid:        "Connector actions invalid",
	KindConnectorAuthRequired: "Connector needs a login",
	KindConnectorUpdate:       "Connector update available",
	KindRequest:               "Request",
	KindTriggerFlood:          "Too many events",
}

func (o RaiseOpts) defaultTitle() string {
	if o.Kind == KindRequestUndelivered {
		return "Could not continue " + o.JobName
	}
	if t, ok := defaultTitles[o.Kind]; ok {
		return t
	}
	return string(o.Kind)
}

func (o RaiseOpts) defaultActions(id string) []InboxAction {
	switch o.Kind {
	case KindRequest:
		return []InboxAction{{Kind: ActionReview, Route: "/inbox/" + id, Label: "Open"}}
	case KindWaiting:
		if o.RunID == "" {
			return []InboxAction{}
		}
		return []InboxAction{{Kind: ActionOpenSession, Route: "/v1/runs/" + o.RunID + "/attach", Label: "Open session"}}
	case KindRunFailed:
		return runFailedActions(o.RunID)
	case KindConnectorAuthRequired:
		route := "/connectors/" + url.PathEscape(o.Connector) + "?tab=login"
		return []InboxAction{{Kind: ActionLogIn, Route: route, Label: "Log in"}}
	case KindConnectorUpdate:
		return []InboxAction{{Kind: ActionUpdate, Route: "/v1/connectors/" + url.PathEscape(o.Connector) + "/update", Label: "Update"}}
	case KindAuthMissing:
		name := firstNonEmpty(o.Harness, o.Connector)
		if name == "" {
			return []InboxAction{}
		}
		return []InboxAction{{Kind: ActionReAuth, Route: "/v1/harnesses/" + name + "/login", Label: "Re-auth"}}
	case KindAgentProposal:
		if o.Subject == "" {
			return []InboxAction{}
		}
		return []InboxAction{{Kind: ActionReview, Route: "/inbox?proposal=" + strings.TrimPrefix(o.Subject, "proposal/"), Label: "Review"}}
	}
	return []InboxAction{}
}

// notifyEnv is the extra env of a connector alert send.
func (o RaiseOpts) notifyEnv(id string) map[string]string {
	return map[string]string{
		"TREBI_ALERT_KIND":      string(o.Kind),
		"TREBI_ALERT_TOOL":      o.Harness,
		"TREBI_ALERT_TASK_ID":   o.RunID,
		"TREBI_ALERT_JOB":       o.JobName,
		"TREBI_ALERT_WORKSPACE": o.WorkspaceID,
		"TREBI_ALERT_MESSAGE":   o.Title,
		"TREBI_ALERT_HINT":      o.Hint,
		"TREBI_ALERT_BODY":      o.Body,
		"TREBI_ALERT_RESUME":    id,
		"TREBI_ALERT_CONNECTOR": o.Connector,
	}
}

// pushText is the short text of a connector alert.
func (o RaiseOpts) pushText(id string) string {
	text := o.Title
	if o.Body != "" {
		text += ": " + o.Body
	}
	return text + " (inbox " + id + ")"
}

// scope is the notify scope of the alert.
func (o RaiseOpts) scope() notify.Scope {
	return notify.Scope{WorkspaceID: o.WorkspaceID, Job: o.JobName}
}

// emailSubject is the stable mail subject for this alert. The alert
// subject key rides along, so a repeat alert of the same fault threads
// with the previous one and two faults never share a thread.
func (o RaiseOpts) emailSubject() string {
	return "[trebi] " + o.Title + ": " + o.Subject
}

// emailText is the plain-text body of the alert mail.
func (o RaiseOpts) emailText(id string) string {
	var b strings.Builder
	for _, line := range []string{o.Body, o.Hint} {
		if line != "" {
			b.WriteString(line)
			b.WriteString("\n\n")
		}
	}
	fmt.Fprintf(&b, "kind: %s\n", o.Kind)
	if o.WorkspaceID != "" {
		fmt.Fprintf(&b, "workspace: %s\n", o.WorkspaceID)
	}
	if o.JobName != "" {
		fmt.Fprintf(&b, "job: %s\n", o.JobName)
	}
	if o.RunID != "" {
		fmt.Fprintf(&b, "run: %s\n", o.RunID)
	}
	fmt.Fprintf(&b, "inbox: %s\n", id)
	return b.String()
}

// setAll moves every item of one status, and optionally one kind, to status.
// An empty from covers unread and read alike. A set workspace scopes the
// update to that workspace plus global rows; empty covers everything.
func (s *Service) setAll(ctx context.Context, from string, to InboxStatus, kind, workspace string) (int64, error) {
	nowStr := s.opts.Now().UTC().Format(time.RFC3339Nano)
	q := `UPDATE inbox_items SET status=?, updated_at=?, revision=revision+1 WHERE status<>?`
	args := []any{string(to), nowStr, string(to)}
	if from != "" {
		q += ` AND status=?`
		args = append(args, from)
	}
	if kind != "" {
		q += ` AND kind=?`
		args = append(args, kind)
	}
	if workspace != "" {
		q += ` AND (workspace_id = ? OR workspace_id = '' OR workspace_id IS NULL)`
		args = append(args, workspace)
	}
	var n int64
	s.st.LockBus()
	defer s.st.UnlockBus()
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(q, args...)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		if n == 0 {
			return nil
		}
		attrs := map[string]string{"status": string(to), "count": fmt.Sprint(n)}
		if kind != "" {
			attrs["kind"] = kind
		}
		_, err = store.AppendInTx(tx, store.BusEvent{
			Topic: "inbox.updated", Entity: "inbox", Action: "updated", Attrs: attrs,
		})
		return err
	})
	return n, err
}
