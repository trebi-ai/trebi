package chains

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Error codes. The API maps each one to an HTTP status.
const (
	CodeValidation  = "validation"
	CodeForbidden   = "forbidden"
	CodeNotFound    = "not_found"
	CodeCycle       = "chain_cycle"
	CodeLimit       = "chain_limit"
	CodeStopped     = "chain_stopped"
	CodeUnavailable = "agent_unavailable"
	CodeRate        = "rate_limited"
)

// Error is a refused chain call. Message is text that a model can act on.
type Error struct {
	Code    string
	Message string
	Reason  RefuseReason
	HopID   string
	TraceID string
}

func (e *Error) Error() string { return e.Message }

// ErrNotFound is a missing chain or hop.
var ErrNotFound = errors.New("not found")

type workspaceDir interface {
	TeamAgents(ctx context.Context, wsID string) ([]workspaces.TeamAgent, error)
	ChainLimits(ctx context.Context, wsID string) (Limits, bool, error)
}

type sender interface {
	SendHop(ctx context.Context, o conversations.HopSend) (*conversations.HopResult, error)
	TurnReply(ctx context.Context, convID, runID string, since time.Time) (string, error)
}

type runReader interface {
	Get(ctx context.Context, id string) (*runs.Run, error)
	Turn(ctx context.Context, runID string, n int) (*runs.Turn, error)
	TraceUsage(ctx context.Context, traceID string) (runs.Spend, error)
	AddEvent(ctx context.Context, runID, kind, source string, detail any)
	TurnAfter(ctx context.Context, runID string, reason runs.RunReason, since time.Time) (int, error)
	Deliver(ctx context.Context, runID string, extra ...runs.ParkDelivery) (runs.DeliverResult, error)
	Kill(ctx context.Context, runID string) error
}

// Service owns hops, chain limits, and reply delivery.
type Service struct {
	st   *store.Store
	dir  workspaceDir
	conv sender
	runs runReader
	req  requester
	log  *slog.Logger

	transcripts transcripts
	alerts      alerter
	now         func() time.Time
}

// NewService builds the chains service.
// A nil req refuses every hop at a limit.
func NewService(st *store.Store, dir workspaceDir, conv sender, rs runReader, req requester, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{st: st, dir: dir, conv: conv, runs: rs, req: req, log: log, now: time.Now}
}

const hopCols = `id, trace_id, workspace_id, kind, status, refuse_reason, depth, path_json,
	from_kind, from_agent_id, from_run_id, from_turn, from_conversation_id,
	to_agent_id, to_run_id, to_turn, to_conversation_id, reply_of, conversation_id,
	message_id, queue_id, request_id, delivery_state, preview, text, context,
	created_at, updated_at, revision`

type scanner interface{ Scan(dest ...any) error }

func scanHop(row scanner) (*Hop, error) {
	var h Hop
	var kind, status, reason, path, created, updated string
	err := row.Scan(&h.ID, &h.TraceID, &h.Workspace, &kind, &status, &reason, &h.Depth, &path,
		&h.From.Kind, &h.From.AgentID, &h.From.RunID, &h.From.Turn, &h.From.ConversationID,
		&h.To.AgentID, &h.To.RunID, &h.To.Turn, &h.To.ConversationID, &h.ReplyOf, &h.ConversationID,
		&h.MessageID, &h.QueueID, &h.RequestID, &h.DeliveryState, &h.Preview, &h.Text, &h.Context,
		&created, &updated, &h.Revision)
	if err != nil {
		return nil, err
	}
	h.Kind, h.Status, h.RefuseReason = Kind(kind), HopStatus(status), RefuseReason(reason)
	_ = json.Unmarshal([]byte(path), &h.pathIDs) // a bad row reads as an empty path
	h.To.Kind = EndAgent
	if h.To.AgentID == "" {
		// A reply to the chain root; names() sets the root kind.
		h.To.Kind = EndHuman
	}
	h.CreatedAt, h.UpdatedAt = store.ParseTS(created), store.ParseTS(updated)
	return &h, nil
}

func (s *Service) queryHops(ctx context.Context, where string, args ...any) ([]*Hop, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+hopCols+` FROM chain_hops `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Hop
	for rows.Next() {
		h, err := scanHop(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// hop reads one hop with no names.
func (s *Service) hop(ctx context.Context, id string) (*Hop, error) {
	h, err := scanHop(s.st.DB().QueryRowContext(ctx, `SELECT `+hopCols+` FROM chain_hops WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("hop %s: %w", id, ErrNotFound)
	}
	return h, err
}

func (s *Service) stamp() string { return s.now().UTC().Format(time.RFC3339Nano) }

// insertHop writes a new hop and its bus event.
func (s *Service) insertHop(ctx context.Context, h *Hop) error {
	now := s.now().UTC()
	h.CreatedAt, h.UpdatedAt, h.Revision = now, now, 1
	if h.DeliveryState == "" {
		h.DeliveryState = DeliveryNone
	}
	path, _ := json.Marshal(nonNil(h.pathIDs))
	ts := now.Format(time.RFC3339Nano)
	idem := h.idem
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO chain_hops (id, trace_id, workspace_id, kind, status, refuse_reason,
			depth, path_json, from_kind, from_agent_id, from_run_id, from_turn, from_conversation_id,
			to_agent_id, to_run_id, to_turn, to_conversation_id, reply_of, conversation_id, message_id,
			queue_id, request_id, delivery_state, idempotency_key, preview, text, context, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			h.ID, h.TraceID, h.Workspace, string(h.Kind), string(h.Status), string(h.RefuseReason),
			h.Depth, string(path), h.From.Kind, h.From.AgentID, h.From.RunID, h.From.Turn, h.From.ConversationID,
			h.To.AgentID, h.To.RunID, h.To.Turn, h.To.ConversationID, h.ReplyOf, h.ConversationID, h.MessageID,
			h.QueueID, h.RequestID, h.DeliveryState, idem, h.Preview, h.Text, h.Context, ts, ts)
		return err
	}, h.event("hop.created", "created"))
	return err
}

// hopUpdate is a set of column changes on one hop.
type hopUpdate map[string]any

// updateHop writes the changes, bumps the revision, and emits hop.updated.
// When from is not empty, the row changes only from one of those states; the
// bool reports whether it changed.
func (s *Service) updateHop(ctx context.Context, id string, set hopUpdate, from ...HopStatus) (bool, error) {
	cols := make([]string, 0, len(set)+2)
	args := make([]any, 0, len(set)+4)
	for _, k := range slices.Sorted(maps.Keys(set)) {
		cols = append(cols, k+"=?")
		args = append(args, set[k])
	}
	cols = append(cols, "updated_at=?", "revision=revision+1")
	args = append(args, s.stamp(), id)
	q := `UPDATE chain_hops SET ` + strings.Join(cols, ", ") + ` WHERE id=?`
	if len(from) > 0 {
		q += ` AND status IN (` + strings.TrimSuffix(strings.Repeat("?,", len(from)), ",") + `)`
		for _, st := range from {
			args = append(args, string(st))
		}
	}
	changed := false
	err := s.tx(ctx, func(tx *sql.Tx) ([]store.BusEvent, error) {
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, nil
		}
		changed = true
		h, err := scanHop(tx.QueryRowContext(ctx, `SELECT `+hopCols+` FROM chain_hops WHERE id=?`, id))
		if err != nil {
			return nil, err
		}
		return []store.BusEvent{h.event("hop.updated", "updated")}, nil
	})
	return changed, err
}

// tx runs write in one transaction and appends the bus events it returns.
func (s *Service) tx(ctx context.Context, write func(tx *sql.Tx) ([]store.BusEvent, error)) error {
	s.st.LockBus()
	defer s.st.UnlockBus()
	n := 0
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		evs, err := write(tx)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if _, err := store.AppendInTx(tx, ev); err != nil {
				return err
			}
		}
		n = len(evs)
		return nil
	})
	if err == nil && n > 0 {
		s.st.NotifyBus()
	}
	return err
}

func (h *Hop) event(topic, action string) store.BusEvent {
	return store.BusEvent{
		Topic: topic, TraceID: h.TraceID, Entity: "hop", EntityID: h.ID, Action: action, Revision: h.Revision,
		Attrs: map[string]string{
			"workspace": h.Workspace, "trace_id": h.TraceID, "kind": string(h.Kind), "status": string(h.Status),
			"from_agent": h.From.AgentID, "to_agent": h.To.AgentID, "from_run_id": h.From.RunID,
			"to_run_id": h.To.RunID, "refuse_reason": string(h.RefuseReason),
		},
	}
}

func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// preview is the first 200 characters of text on one line.
func preview(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > 200 {
		return string(r[:200])
	}
	return text
}
