package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
)

// Session statuses.
const (
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusWaiting  = "waiting"
	StatusIdle     = "idle"
	StatusClosed   = "closed"
	StatusError    = "error"
	// StatusPaused is a turn that a daemon restart stopped. It has no
	// process; the manager resumes it after boot.
	StatusPaused = "paused"
)

// ErrNoSession reports a conversation with no assistant session row.
var ErrNoSession = errors.New("no assistant session for this conversation")

// Record is one row of assistant_sessions: the runtime state of one
// assistant conversation.
type Record struct {
	ConversationID string    `json:"conversation_id"`
	WorkspaceID    string    `json:"workspace_id"`
	Harness        string    `json:"harness"`
	Model          string    `json:"model,omitempty"`
	Effort         string    `json:"effort,omitempty"`
	Mode           string    `json:"mode"`
	VendorID       string    `json:"vendor_id,omitempty"`
	Status         string    `json:"status"`
	Error          string    `json:"error,omitempty"`
	Turn           int       `json:"turn"`
	Prompts        int       `json:"prompts"`
	TokensIn       int64     `json:"tokens_in"`
	TokensOut      int64     `json:"tokens_out"`
	TokensCached   int64     `json:"tokens_cached"`
	CostUSD        float64   `json:"cost_usd"`
	ActorTokenHash string    `json:"-"`
	LastMessage    string    `json:"last_message,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Store owns the assistant_sessions and conversation_events rows.
type Store struct {
	st *store.Store
}

// NewStore builds an assistant store over the shared store.
func NewStore(st *store.Store) *Store { return &Store{st: st} }

const sessionCols = `conversation_id, workspace_id, harness, model, effort, mode, vendor_id,
	status, error, turn, prompts, tokens_in, tokens_out, tokens_cached, cost_usd,
	COALESCE(actor_token_hash,''), last_message, created_at, updated_at`

func scanSession(row interface{ Scan(...any) error }) (Record, error) {
	var r Record
	var created, updated string
	err := row.Scan(&r.ConversationID, &r.WorkspaceID, &r.Harness, &r.Model, &r.Effort,
		&r.Mode, &r.VendorID, &r.Status, &r.Error, &r.Turn, &r.Prompts,
		&r.TokensIn, &r.TokensOut, &r.TokensCached, &r.CostUSD,
		&r.ActorTokenHash, &r.LastMessage, &created, &updated)
	if err != nil {
		return Record{}, err
	}
	r.CreatedAt = store.ParseTS(created)
	r.UpdatedAt = store.ParseTS(updated)
	return r, nil
}

// Create inserts one session row.
func (s *Store) Create(ctx context.Context, rec Record) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	_, err := s.st.DB().ExecContext(ctx, `INSERT INTO assistant_sessions
		(conversation_id, workspace_id, harness, model, effort, mode, vendor_id, status, error,
		 turn, prompts, tokens_in, tokens_out, tokens_cached, cost_usd, actor_token_hash,
		 last_message, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ConversationID, rec.WorkspaceID, rec.Harness, rec.Model, rec.Effort, rec.Mode,
		rec.VendorID, rec.Status, rec.Error, rec.Turn, rec.Prompts,
		rec.TokensIn, rec.TokensOut, rec.TokensCached, rec.CostUSD, rec.ActorTokenHash,
		rec.LastMessage, rec.CreatedAt.UTC().Format(time.RFC3339Nano), now)
	return err
}

// Get returns one session row.
func (s *Store) Get(ctx context.Context, convID string) (Record, error) {
	row := s.st.DB().QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM assistant_sessions WHERE conversation_id = ?`, convID)
	r, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNoSession
	}
	return r, err
}

// Update runs fn over the record in one transaction and writes the row back.
// It emits conversation.updated with the new status and harness.
func (s *Store) Update(ctx context.Context, convID string, fn func(*Record)) (Record, error) {
	var out Record
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+sessionCols+` FROM assistant_sessions WHERE conversation_id = ?`, convID)
		r, err := scanSession(row)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		fn(&r)
		r.UpdatedAt = time.Now().UTC()
		_, err = tx.ExecContext(ctx, `UPDATE assistant_sessions SET
			workspace_id=?, harness=?, model=?, effort=?, mode=?, vendor_id=?, status=?, error=?,
			turn=?, prompts=?, tokens_in=?, tokens_out=?, tokens_cached=?, cost_usd=?,
			actor_token_hash=?, last_message=?, updated_at=?
			WHERE conversation_id=?`,
			r.WorkspaceID, r.Harness, r.Model, r.Effort, r.Mode, r.VendorID, r.Status, r.Error,
			r.Turn, r.Prompts, r.TokensIn, r.TokensOut, r.TokensCached, r.CostUSD,
			r.ActorTokenHash, r.LastMessage, r.UpdatedAt.UTC().Format(time.RFC3339Nano), convID)
		if err != nil {
			return err
		}
		attrs := map[string]string{"status": r.Status, "harness": r.Harness}
		if _, err := store.AppendInTx(tx, store.BusEvent{
			Topic: "conversation.updated", Entity: "conversation", EntityID: convID,
			Action: "updated", Attrs: attrs,
		}); err != nil {
			return err
		}
		out = r
		return nil
	})
	if err == nil {
		s.st.NotifyBus()
	}
	return out, err
}

// List returns the session rows of one workspace, newest activity first.
func (s *Store) List(ctx context.Context, workspaceID string) ([]Record, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT `+sessionCols+` FROM assistant_sessions WHERE workspace_id = ?
		 ORDER BY updated_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PauseStale runs at boot. A row that a crash left in a turn becomes paused
// when it has a vendor or visor session to continue, else closed. A visor
// session can still live, so its row keeps the token. It returns the paused
// and the closed counts.
func (s *Store) PauseStale(ctx context.Context) (paused, closed int, err error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err = s.st.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE assistant_sessions SET status = ?, updated_at = ?,
			 actor_token_hash = CASE WHEN COALESCE(visor_session_id,'') = '' THEN '' ELSE actor_token_hash END
			 WHERE status IN (?,?,?) AND (vendor_id != '' OR COALESCE(visor_session_id,'') != '')`,
			StatusPaused, now, StatusStarting, StatusRunning, StatusWaiting)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		paused = int(n)
		res, err = tx.ExecContext(ctx,
			`UPDATE assistant_sessions SET status = ?, updated_at = ? WHERE status IN (?,?,?)`,
			StatusClosed, now, StatusStarting, StatusRunning, StatusWaiting)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		closed = int(n)
		return nil
	})
	return paused, closed, err
}

// SetVisor records the visor session of a conversation and the last visor
// event that the assistant handled. An empty id clears both.
func (s *Store) SetVisor(ctx context.Context, convID, sessionID string, seq uint64) error {
	_, err := s.st.DB().ExecContext(ctx, `UPDATE assistant_sessions SET visor_session_id=?, visor_seq=? WHERE conversation_id=?`,
		store.NullStr(sessionID), int64(seq), convID) //nolint:gosec // a seq stays far below 2^63
	return err
}

// SetVisorSeq records the last visor event that the assistant handled.
func (s *Store) SetVisorSeq(ctx context.Context, convID string, seq uint64) error {
	_, err := s.st.DB().ExecContext(ctx, `UPDATE assistant_sessions SET visor_seq=? WHERE conversation_id=?`, int64(seq), convID) //nolint:gosec // a seq stays far below 2^63
	return err
}

// Visor gives the visor session of a conversation and the last visor event
// that the assistant handled.
func (s *Store) Visor(ctx context.Context, convID string) (string, uint64, error) {
	var id sql.NullString
	var seq int64
	err := s.st.DB().QueryRowContext(ctx, `SELECT visor_session_id, visor_seq FROM assistant_sessions WHERE conversation_id=?`, convID).Scan(&id, &seq)
	return id.String, uint64(seq), err //nolint:gosec // the column holds a seq, never negative
}

// Paused returns the paused rows of all workspaces, oldest activity first.
func (s *Store) Paused(ctx context.Context) ([]Record, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT `+sessionCols+` FROM assistant_sessions WHERE status = ? ORDER BY updated_at`, StatusPaused)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AppendEvent stores one event and returns its sequence number.
func (s *Store) AppendEvent(ctx context.Context, convID string, e Event) (int64, error) {
	var seq int64
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), 0) FROM conversation_events WHERE conversation_id = ?`,
			convID).Scan(&seq)
		if err != nil {
			return err
		}
		seq++
		e.Seq = seq
		body, err := json.Marshal(e)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_events
			(conversation_id, seq, turn, type, body, created_at) VALUES (?,?,?,?,?,?)`,
			convID, seq, e.Turn, e.Type, string(body),
			time.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// Events replays stored events after afterSeq, oldest first.
func (s *Store) Events(ctx context.Context, convID string, afterSeq int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT seq, body FROM conversation_events
		 WHERE conversation_id = ? AND seq > ? ORDER BY seq LIMIT ?`,
		convID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var seq int64
		var body string
		if err := rows.Scan(&seq, &body); err != nil {
			return nil, err
		}
		var e Event
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, fmt.Errorf("event %d: %w", seq, err)
		}
		e.Seq = seq
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastPromptEvent returns the newest user event that Trebi sent to the agent,
// or false when it has none. A harness echo has no delivery.
func (s *Store) LastPromptEvent(ctx context.Context, convID string) (Event, bool, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT seq, body FROM conversation_events
		 WHERE conversation_id = ? AND type = 'user' ORDER BY seq DESC LIMIT 50`, convID)
	if err != nil {
		return Event{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var body string
		if err := rows.Scan(&seq, &body); err != nil {
			return Event{}, false, err
		}
		var e Event
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return Event{}, false, fmt.Errorf("event %d: %w", seq, err)
		}
		if e.Delivery != "" {
			e.Seq = seq
			return e, true, nil
		}
	}
	return Event{}, false, rows.Err()
}

// ByTokenHash resolves a session actor token hash to its row. A closed or
// errored session has no live token.
func (s *Store) ByTokenHash(ctx context.Context, hash string) (Record, error) {
	if strings.TrimSpace(hash) == "" {
		return Record{}, ErrNoSession
	}
	row := s.st.DB().QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM assistant_sessions
		 WHERE actor_token_hash = ? AND status NOT IN (?, ?)`,
		hash, StatusClosed, StatusError)
	r, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNoSession
	}
	return r, err
}

// TokensToday sums the tokens of native sessions updated today. It backs the
// native daily cap.
func (s *Store) TokensToday(ctx context.Context, harness string) (int64, error) {
	var sum sql.NullInt64
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT SUM(tokens_in + tokens_out) FROM assistant_sessions
		 WHERE harness = ? AND updated_at >= ?`,
		harness, time.Now().UTC().Format("2006-01-02")).Scan(&sum)
	if err != nil {
		return 0, err
	}
	return sum.Int64, nil
}

// DeleteEvents removes the event log of one conversation.
func (s *Store) DeleteEvents(ctx context.Context, convID string) error {
	_, err := s.st.DB().ExecContext(ctx,
		`DELETE FROM conversation_events WHERE conversation_id = ?`, convID)
	return err
}

// DeleteSession removes one session row.
func (s *Store) DeleteSession(ctx context.Context, convID string) error {
	res, err := s.st.DB().ExecContext(ctx,
		`DELETE FROM assistant_sessions WHERE conversation_id = ?`, convID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSession
	}
	return nil
}
