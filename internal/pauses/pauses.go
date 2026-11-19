// Package pauses owns the pauses: a hold on new work of a harness, a
// workspace, an agent, or everything. A pause does not change the job
// switches. The launch paths ask Cover before they start new work.
package pauses

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/store"
)

// Scope is what a pause covers.
type Scope string

const (
	ScopeAll       Scope = "all"
	ScopeWorkspace Scope = "workspace"
	ScopeHarness   Scope = "harness"
	ScopeAgent     Scope = "agent"
)

// Missed tells what a schedule fire does that a pause skipped.
type Missed string

const (
	// MissedSkip drops the fire.
	MissedSkip Missed = "skip"
	// MissedOnce runs each skipped job once when the pause ends.
	MissedOnce Missed = "once"
)

// Source tells who made a pause.
type Source string

const (
	SourceUser  Source = "user"
	SourceLimit Source = "limit"
)

// EndedBy tells why a pause ended.
type EndedBy string

const (
	EndedByUser   EndedBy = "user"
	EndedByExpiry EndedBy = "expiry"
)

// CreatedByTrebi is the created_by of an automatic pause.
const CreatedByTrebi = "trebi"

// UnknownResetHold is the length of an automatic pause on a hard limit
// with no known reset time.
const UnknownResetHold = time.Hour

var (
	// ErrNotFound is a pause id with no row.
	ErrNotFound = errors.New("pause not found")
	// ErrInvalid is a pause request that does not validate.
	ErrInvalid = errors.New("invalid pause")
	// ErrEnded is a change to a pause that ended.
	ErrEnded = errors.New("pause ended")
)

// Pause is the wire shape of one pause.
type Pause struct {
	ID     string `json:"id"`
	Scope  Scope  `json:"scope"`
	Target string `json:"target"`
	// Label names the target for a person: the workspace, harness, or
	// agent name, or "Everything".
	Label        string  `json:"label"`
	Until        *string `json:"until"`
	Missed       Missed  `json:"missed"`
	Source       Source  `json:"source"`
	Note         string  `json:"note"`
	CreatedBy    string  `json:"created_by"`
	CreatedAt    string  `json:"created_at"`
	EndedAt      *string `json:"ended_at"`
	EndedBy      EndedBy `json:"ended_by,omitempty"`
	SkippedFires int     `json:"skipped_fires"`
	Revision     int64   `json:"revision"`
	Active       bool    `json:"active"`
}

// Hold is the short form of a pause on a job or agent row (held_by).
type Hold struct {
	PauseID string  `json:"pause_id"`
	Scope   Scope   `json:"scope"`
	Target  string  `json:"target"`
	Label   string  `json:"label"`
	Until   *string `json:"until"`
}

// Hold returns the held_by form of p.
func (p Pause) Hold() *Hold {
	return &Hold{PauseID: p.ID, Scope: p.Scope, Target: p.Target, Label: p.Label, Until: p.Until}
}

// until returns the end time; zero when the pause has no end.
func (p Pause) until() time.Time {
	if p.Until == nil {
		return time.Time{}
	}
	return store.ParseTS(*p.Until)
}

// endsAfter reports whether p ends after q. No end is the latest.
func (p Pause) endsAfter(q Pause) bool {
	pu, qu := p.until(), q.until()
	switch {
	case qu.IsZero():
		return false
	case pu.IsZero():
		return true
	}
	return pu.After(qu)
}

// Subject is the work that a launch path asks about.
type Subject struct {
	WorkspaceID string
	AgentID     string
	Harness     string
	Kind        string
}

// Active is the set of the pauses that hold work now.
type Active []Pause

// Cover returns the pause that holds s and ends last, or nil. The
// assistant is never held.
func (a Active) Cover(s Subject) *Pause {
	if s.Kind == dispatch.KindAssistant {
		return nil
	}
	var best *Pause
	for i := range a {
		p := a[i]
		if !p.covers(s) {
			continue
		}
		if best == nil || p.endsAfter(*best) {
			best = &a[i]
		}
	}
	return best
}

func (p Pause) covers(s Subject) bool {
	switch p.Scope {
	case ScopeAll:
		return true
	case ScopeWorkspace:
		return s.WorkspaceID != "" && p.Target == s.WorkspaceID
	case ScopeHarness:
		return s.Harness != "" && p.Target == s.Harness
	case ScopeAgent:
		return s.AgentID != "" && p.Target == s.AgentID
	}
	return false
}

// EndHook gets each pause that ended.
type EndHook interface {
	PauseEnded(ctx context.Context, p Pause)
}

// Service reads and writes the pauses.
type Service struct {
	st    *store.Store
	onEnd EndHook
	now   func() time.Time
	log   *slog.Logger
}

// New builds the service.
func New(st *store.Store) *Service {
	return &Service{st: st, now: time.Now, log: slog.Default().With("source", "pauses")}
}

// SetEndHook sets the hook that releases held work when a pause ends.
func (s *Service) SetEndHook(h EndHook) { s.onEnd = h }

// SetClock sets the clock. Tests only.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

const pauseCols = `p.id, p.scope, p.target, p.until_at, p.missed, p.source, p.note, p.created_by, p.created_at,
	p.ended_at, COALESCE(p.ended_by,''), p.skipped_fires, p.revision,
	CASE p.scope WHEN 'all' THEN 'Everything'
		WHEN 'workspace' THEN COALESCE((SELECT w.name FROM workspaces w WHERE w.id = p.target), p.target)
		WHEN 'agent' THEN COALESCE((SELECT j.name FROM job_index j WHERE j.id = p.target), p.target)
		ELSE p.target END`

// Get returns one pause.
func (s *Service) Get(ctx context.Context, id string) (Pause, error) {
	p, err := s.scan(s.st.DB().QueryRowContext(ctx, `SELECT `+pauseCols+` FROM pauses p WHERE p.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Pause{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return p, err
}

// ListOpts filters List.
type ListOpts struct {
	// All also returns the pauses that ended, newest first.
	All   bool
	Limit int
}

// List returns the active pauses, or with All the recent pauses too.
func (s *Service) List(ctx context.Context, o ListOpts) ([]Pause, error) {
	if !o.All {
		return s.Active(ctx)
	}
	if o.Limit <= 0 {
		o.Limit = 100
	}
	return s.query(ctx, `ORDER BY p.created_at DESC, p.rowid DESC LIMIT ?`, o.Limit)
}

// Active returns the pauses that hold work now.
func (s *Service) Active(ctx context.Context) (Active, error) {
	open, err := s.query(ctx, `WHERE p.ended_at IS NULL ORDER BY p.created_at DESC, p.rowid DESC`)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(open, func(p Pause) bool { return !p.Active }), nil
}

// Cover returns the pause that holds the work of jobID, or nil. It finds
// the agent of a job from the job index.
func (s *Service) Cover(ctx context.Context, workspaceID, jobID, kind, harness string) (*Pause, error) {
	if kind == dispatch.KindAssistant {
		return nil, nil
	}
	active, err := s.Active(ctx)
	if err != nil || len(active) == 0 {
		return nil, err
	}
	sub := Subject{WorkspaceID: workspaceID, Harness: harness, Kind: kind}
	if kind == dispatch.KindAgent {
		sub.AgentID = jobID
	} else if jobID != "" && slices.ContainsFunc(active, func(p Pause) bool { return p.Scope == ScopeAgent }) {
		err := s.st.DB().QueryRowContext(ctx, `SELECT owner_id FROM job_index WHERE id = ?`, jobID).Scan(&sub.AgentID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return active.Cover(sub), nil
}

// SetOpts is a request to pause a scope.
type SetOpts struct {
	Scope  Scope
	Target string
	// Until is the end; nil means "until I resume".
	Until     *time.Time
	Missed    Missed
	Source    Source
	Note      string
	CreatedBy string
}

func (o *SetOpts) validate(now time.Time) error {
	switch o.Scope {
	case ScopeAll:
		o.Target = ""
	case ScopeWorkspace, ScopeHarness, ScopeAgent:
		if strings.TrimSpace(o.Target) == "" {
			return fmt.Errorf("%w: target is required for scope %s", ErrInvalid, o.Scope)
		}
	default:
		return fmt.Errorf("%w: scope must be all, workspace, harness, or agent", ErrInvalid)
	}
	if o.Missed == "" {
		o.Missed = MissedSkip
	}
	if o.Missed != MissedSkip && o.Missed != MissedOnce {
		return fmt.Errorf("%w: missed must be skip or once", ErrInvalid)
	}
	if o.Source == "" {
		o.Source = SourceUser
	}
	if o.Until != nil && !o.Until.After(now) {
		return fmt.Errorf("%w: the end time is in the past", ErrInvalid)
	}
	return nil
}

// Set pauses a scope. When the scope already has an open pause, Set
// changes its end, missed runs, and note, and created is false.
func (s *Service) Set(ctx context.Context, o SetOpts) (p Pause, created bool, err error) {
	now := s.now().UTC()
	if err := o.validate(now); err != nil {
		return Pause{}, false, err
	}
	var id string
	err = s.st.DB().QueryRowContext(ctx, `SELECT id FROM pauses WHERE scope = ? AND target = ? AND ended_at IS NULL`,
		o.Scope, o.Target).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id, created = newID(), true
	case err != nil:
		return Pause{}, false, err
	}
	ts := now.Format(time.RFC3339)
	topic, action := "pause.updated", "updated"
	if created {
		topic, action = "pause.created", "created"
	}
	var rev int64 = 1
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		if created {
			_, err := tx.ExecContext(ctx, `INSERT INTO pauses (id, scope, target, until_at, missed, source, note, created_by,
				created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				id, o.Scope, o.Target, tsOrNil(o.Until), o.Missed, o.Source, o.Note, o.CreatedBy, ts, ts)
			return err
		}
		return tx.QueryRowContext(ctx, `UPDATE pauses SET until_at = ?, missed = ?, source = ?, note = ?,
			revision = revision + 1, updated_at = ? WHERE id = ? RETURNING revision`,
			tsOrNil(o.Until), o.Missed, o.Source, o.Note, ts, id).Scan(&rev)
	}, store.BusEvent{Topic: topic, Entity: "pause", EntityID: id, Action: action, Revision: rev,
		Attrs: map[string]any{"scope": o.Scope, "target": o.Target, "source": o.Source}})
	if err != nil {
		return Pause{}, false, err
	}
	p, err = s.Get(ctx, id)
	if err == nil {
		s.log.InfoContext(ctx, topic, "id", id, "scope", o.Scope, "target", o.Target, "until", tsOrNil(o.Until))
	}
	return p, created, err
}

// UpdateOpts changes an open pause. A nil field keeps its value.
type UpdateOpts struct {
	// Until with ClearUntil false sets the end; ClearUntil sets no end.
	Until      *time.Time
	ClearUntil bool
	Missed     *Missed
	Note       *string
}

// Update changes the end, the missed runs, or the note of an open pause.
func (s *Service) Update(ctx context.Context, id string, o UpdateOpts) (Pause, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return Pause{}, err
	}
	if cur.EndedAt != nil {
		return Pause{}, fmt.Errorf("%w: %s", ErrEnded, id)
	}
	set := SetOpts{Scope: cur.Scope, Target: cur.Target, Missed: cur.Missed, Source: cur.Source, Note: cur.Note}
	if u := cur.until(); !u.IsZero() {
		set.Until = &u
	}
	switch {
	case o.ClearUntil:
		set.Until = nil
	case o.Until != nil:
		set.Until = o.Until
	}
	if o.Missed != nil {
		set.Missed = *o.Missed
	}
	if o.Note != nil {
		set.Note = *o.Note
	}
	// A person who changes an automatic pause owns it.
	if o.Until != nil || o.ClearUntil {
		set.Source = SourceUser
	}
	p, _, err := s.Set(ctx, set)
	return p, err
}

// Resume ends an open pause now.
func (s *Service) Resume(ctx context.Context, id string) (Pause, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return Pause{}, err
	}
	if cur.EndedAt != nil {
		return cur, nil
	}
	return s.end(ctx, cur, EndedByUser)
}

// ExpireDue ends each open pause whose end time is past.
func (s *Service) ExpireDue(ctx context.Context) ([]Pause, error) {
	open, err := s.query(ctx, `WHERE p.ended_at IS NULL AND p.until_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	var out []Pause
	for _, p := range open {
		if p.Active {
			continue
		}
		ended, err := s.end(ctx, p, EndedByExpiry)
		if err != nil {
			return out, err
		}
		out = append(out, ended)
	}
	return out, nil
}

// Skipped counts one schedule fire that p held. With missed once it
// keeps the fire so that the job runs once when p ends.
func (s *Service) Skipped(ctx context.Context, p Pause, workspaceID, jobID string, idx int) error {
	return s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE pauses SET skipped_fires = skipped_fires + 1 WHERE id = ?`, p.ID); err != nil {
			return err
		}
		if p.Missed != MissedOnce {
			return nil
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO pause_misses (pause_id, workspace_id, job_id, idx) VALUES (?, ?, ?, ?)
			ON CONFLICT (pause_id, workspace_id, job_id, idx) DO NOTHING`, p.ID, workspaceID, jobID, idx)
		return err
	})
}

// Miss is one schedule fire that runs once when its pause ends.
type Miss struct {
	WorkspaceID string
	JobID       string
	Idx         int
}

// TakeMisses returns the kept fires of a pause and deletes them.
func (s *Service) TakeMisses(ctx context.Context, pauseID string) ([]Miss, error) {
	var out []Miss
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT workspace_id, job_id, idx FROM pause_misses WHERE pause_id = ?`, pauseID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var m Miss
			if err := rows.Scan(&m.WorkspaceID, &m.JobID, &m.Idx); err != nil {
				rows.Close()
				return err
			}
			out = append(out, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM pause_misses WHERE pause_id = ?`, pauseID)
		return err
	})
	return out, err
}

// Limit is the usage state of one harness that AutoPause reads.
type Limit struct {
	Harness string
	Windows []harness.UsageWindow
	// Hit is a hard limit that the harness reported; ResetsAt is its reset
	// time when known. The store keeps no reset time for a hard limit now,
	// so a hit with no window reset holds for UnknownResetHold.
	Hit      bool
	ResetsAt string
}

// Reset is the next reset of the limit that holds the harness: the hard
// limit when it is hit, else the most used window. It is zero when no
// reset time is known.
func (l Limit) Reset(now time.Time) time.Time {
	if l.Hit {
		if r := store.ParseTS(l.ResetsAt); r.After(now) {
			return r.Truncate(time.Second)
		}
	}
	var best time.Time
	most := -1.0
	for _, w := range l.Windows {
		r := store.ParseTS(w.ResetsAt)
		if !r.After(now) {
			continue
		}
		if w.UsedPercent > most || (w.UsedPercent == most && r.After(best)) {
			best, most = r, w.UsedPercent
		}
	}
	return best.Truncate(time.Second)
}

// holdUntil is the end of an auto pause: the latest reset of the windows
// at pauseAt percent or over, and of a hard limit. It is zero when no
// limit is near. known is false when the end is the UnknownResetHold guess.
func (l Limit) holdUntil(now time.Time, pauseAt int) (until time.Time, known bool) {
	for _, w := range l.Windows {
		reset := store.ParseTS(w.ResetsAt)
		if w.UsedPercent >= float64(pauseAt) && reset.After(now) && reset.After(until) {
			until = reset
		}
	}
	if l.Hit {
		if reset := store.ParseTS(l.ResetsAt); reset.After(until) && reset.After(now) {
			until = reset
		}
		if until.IsZero() {
			return now.Add(UnknownResetHold).Truncate(time.Second), false
		}
	}
	return until.Truncate(time.Second), true
}

// AutoPause pauses a harness until its limit resets when a window is at
// pauseAt percent or over, or when the harness hit a hard limit. pauseAt
// 0 turns it off. A person who resumed a limit pause early keeps the
// harness running until that reset. A pause of a person is not changed.
// The bool is true when AutoPause made or extended a pause.
func (s *Service) AutoPause(ctx context.Context, l Limit, pauseAt int) (Pause, bool, error) {
	if pauseAt <= 0 || l.Harness == "" {
		return Pause{}, false, nil
	}
	now := s.now().UTC()
	until, known := l.holdUntil(now, pauseAt)
	if until.IsZero() {
		return Pause{}, false, nil
	}
	var resumed int
	err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pauses WHERE scope = 'harness' AND target = ?
		AND source = 'limit' AND ended_by = 'user' AND until_at > ?`, l.Harness, now.Format(time.RFC3339)).Scan(&resumed)
	if err != nil || resumed > 0 {
		return Pause{}, false, err
	}
	open, err := s.query(ctx, `WHERE p.scope = 'harness' AND p.target = ? AND p.ended_at IS NULL`, l.Harness)
	if err != nil {
		return Pause{}, false, err
	}
	if len(open) > 0 && open[0].Active {
		cur := open[0]
		// A guessed end does not move, so the pause can expire and clear the flag.
		if cur.Source != SourceLimit || !known || !until.After(cur.until()) {
			return cur, false, nil
		}
	}
	p, _, err := s.Set(ctx, SetOpts{Scope: ScopeHarness, Target: l.Harness, Until: &until, Missed: MissedSkip,
		Source: SourceLimit, Note: "Near the usage limit", CreatedBy: CreatedByTrebi})
	return p, err == nil, err
}

// ParseFor reads the length of a pause: a Go duration ("90m", "3h") or a
// whole number of days or weeks ("2d", "1w").
func ParseFor(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	unit := time.Duration(0)
	switch {
	case strings.HasSuffix(v, "d"):
		unit = 24 * time.Hour
	case strings.HasSuffix(v, "w"):
		unit = 7 * 24 * time.Hour
	}
	if unit > 0 {
		n, err := strconv.Atoi(v[:len(v)-1])
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%w: bad length %q", ErrInvalid, v)
		}
		return time.Duration(n) * unit, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%w: bad length %q", ErrInvalid, v)
	}
	return d, nil
}

func (s *Service) scan(sc interface{ Scan(...any) error }) (Pause, error) {
	var p Pause
	var until, ended sql.NullString
	if err := sc.Scan(&p.ID, &p.Scope, &p.Target, &until, &p.Missed, &p.Source, &p.Note, &p.CreatedBy, &p.CreatedAt,
		&ended, &p.EndedBy, &p.SkippedFires, &p.Revision, &p.Label); err != nil {
		return Pause{}, err
	}
	if until.Valid {
		p.Until = &until.String
	}
	if ended.Valid {
		p.EndedAt = &ended.String
	}
	u := p.until()
	p.Active = p.EndedAt == nil && (u.IsZero() || u.After(s.now()))
	return p, nil
}

func (s *Service) query(ctx context.Context, where string, args ...any) ([]Pause, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+pauseCols+` FROM pauses p `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pause{}
	for rows.Next() {
		p, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) end(ctx context.Context, p Pause, by EndedBy) (Pause, error) {
	ts := s.now().UTC().Format(time.RFC3339)
	var rev int64
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `UPDATE pauses SET ended_at = ?, ended_by = ?, revision = revision + 1, updated_at = ?
			WHERE id = ? AND ended_at IS NULL RETURNING revision`, ts, by, ts, p.ID).Scan(&rev)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrEnded, p.ID)
		}
		return err
	}, store.BusEvent{Topic: "pause.ended", Entity: "pause", EntityID: p.ID, Action: "ended", Revision: rev,
		Attrs: map[string]any{"scope": p.Scope, "target": p.Target, "ended_by": by, "skipped_fires": p.SkippedFires}})
	if errors.Is(err, ErrEnded) {
		return s.Get(ctx, p.ID)
	}
	if err != nil {
		return Pause{}, err
	}
	ended, err := s.Get(ctx, p.ID)
	if err != nil {
		return Pause{}, err
	}
	s.log.InfoContext(ctx, "pause.ended", "id", p.ID, "scope", p.Scope, "target", p.Target, "ended_by", by)
	if s.onEnd != nil {
		s.onEnd.PauseEnded(ctx, ended)
	}
	return ended, nil
}

func tsOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "pse_" + hex.EncodeToString(b[:])
}
