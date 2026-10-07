package runs

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
)

var (
	// ErrNoSession reports a resume of a run with no session id.
	ErrNoSession = errors.New("run has no session to resume")
	// ErrResumeUnsupported wraps the harness errors for a session that cannot
	// resume (agentwire and tmux). A caller falls back to a fresh run.
	ErrResumeUnsupported = errors.New("session resume not supported")
	// ErrNotParked reports Unpark of a run that is not parked.
	ErrNotParked = errors.New("run is not parked")
)

// ParkGate is one reason a run can park: a request or a chain ask. Open
// reports items that the run still waits for and items ready to deliver.
// Claim takes the ready items once; Settle records the resume result.
type ParkGate interface {
	Name() string
	Open(ctx context.Context, runID string) (waiting, ready bool, err error)
	RunEnded(ctx context.Context, runID string, st Status)
	Claim(ctx context.Context, runID string) ([]ParkDelivery, error)
	Settle(ctx context.Context, runID string, d []ParkDelivery, res ResumeResult) error
	// Stale resets lost claims and returns the runs with ready items.
	Stale(ctx context.Context) ([]string, error)
	PromptHint(opts LaunchOpts) string
}

// ParkDelivery is the part of one resume prompt that one gate gives.
type ParkDelivery struct {
	Gate    string
	IDs     []string // request or hop IDs
	Prompt  string
	Capsule string // the prompt of a fresh run; "" uses Prompt
	At      time.Time
	Reason  RunReason
	// The chain frame of a reply: the new turn continues it.
	HopID   string
	TraceID string
	Path    []string
}

// ResumeResult is the outcome of one resume.
type ResumeResult struct {
	RunID string // the run that took the prompt
	Turn  int
	Path  string // resume|fresh
	Err   error
}

// Resumer continues a parked session, or starts a fresh run when the
// harness cannot resume.
type Resumer interface {
	Resume(ctx context.Context, prevID string, o ResumeOpts) (*Run, error)
	Fresh(ctx context.Context, prevID string, o ResumeOpts) (*Run, error)
}

type runGetter interface {
	Get(ctx context.Context, id string) (*Run, error)
}

// Parker resumes a parked run once, when no gate waits for an item.
type Parker struct {
	runs   runGetter
	resume Resumer
	gates  []ParkGate
	log    *slog.Logger

	// One wake per run at a time. A wake that comes in while one runs (a
	// launch failure ends the turn again) sets again, and the first repeats.
	mu    sync.Mutex
	busy  map[string]bool
	again map[string]bool
}

// NewParker builds the park coordinator.
func NewParker(rs runGetter, r Resumer, gates ...ParkGate) *Parker {
	return &Parker{runs: rs, resume: r, gates: gates, log: slog.Default().With("source", "parker"),
		busy: map[string]bool{}, again: map[string]bool{}}
}

// Wake resumes runID when it is parked, no gate waits, and a gate has items
// ready.
func (p *Parker) Wake(ctx context.Context, runID string) error {
	p.mu.Lock()
	if p.busy[runID] {
		p.again[runID] = true
		p.mu.Unlock()
		return nil
	}
	p.busy[runID] = true
	p.mu.Unlock()
	for {
		err := p.wake(ctx, runID)
		p.mu.Lock()
		if p.again[runID] {
			delete(p.again, runID)
			p.mu.Unlock()
			continue
		}
		delete(p.busy, runID)
		p.mu.Unlock()
		return err
	}
}

// Recover runs at boot: each gate resets its lost claims, then every run
// with ready items wakes.
func (p *Parker) Recover(ctx context.Context) error {
	seen := map[string]bool{}
	for _, g := range p.gates {
		ids, err := g.Stale(ctx)
		if err != nil {
			return fmt.Errorf("%s gate: %w", g.Name(), err)
		}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			if err := p.Wake(ctx, id); err != nil {
				p.log.Warn("park recover", "run_id", id, "err", err)
			}
		}
	}
	return nil
}

func (p *Parker) wake(ctx context.Context, runID string) error {
	run, err := p.runs.Get(ctx, runID)
	if err != nil {
		return fmt.Errorf("run %s: %w", runID, err)
	}
	if run.Status != StatusParked {
		return nil
	}
	for _, g := range p.gates {
		waiting, _, err := g.Open(ctx, runID)
		if err != nil {
			return fmt.Errorf("%s gate: %w", g.Name(), err)
		}
		if waiting {
			return nil
		}
	}
	claimed := map[ParkGate][]ParkDelivery{}
	var all []ParkDelivery
	for _, g := range p.gates {
		d, err := g.Claim(ctx, runID)
		if err != nil {
			p.log.Warn("park claim", "gate", g.Name(), "run_id", runID, "err", err)
			continue
		}
		if len(d) > 0 {
			claimed[g] = d
			all = append(all, d...)
		}
	}
	if len(all) == 0 {
		return nil
	}
	res := p.launch(ctx, run, all)
	for g, d := range claimed {
		if err := g.Settle(ctx, runID, d, res); err != nil {
			p.log.Warn("park settle", "gate", g.Name(), "run_id", runID, "err", err)
		}
	}
	if res.Err != nil {
		return fmt.Errorf("resume run %s: %w", runID, res.Err)
	}
	return nil
}

// launch resumes the session with all deliveries in one prompt. A harness
// that cannot resume gets a fresh run with the capsule prompts.
func (p *Parker) launch(ctx context.Context, run *Run, all []ParkDelivery) ResumeResult {
	slices.SortStableFunc(all, func(a, b ParkDelivery) int { return a.At.Compare(b.At) })
	o := ResumeOpts{Reason: ReasonRequest}
	var prompts, capsules []string
	for _, d := range all {
		prompts = append(prompts, d.Prompt)
		capsules = append(capsules, cmp.Or(d.Capsule, d.Prompt))
		if d.Reason == ReasonReply && o.Reason != ReasonReply {
			o.Reason, o.HopID, o.TraceID, o.Path = ReasonReply, d.HopID, d.TraceID, d.Path
		}
	}
	o.Prompt = strings.Join(prompts, "\n\n")
	res := ResumeResult{Path: "resume"}
	var next *Run
	err := ErrNoSession
	if run.SessionID != "" {
		next, err = p.resume.Resume(ctx, run.ID, o)
	}
	if errors.Is(err, ErrResumeUnsupported) || errors.Is(err, ErrNoSession) {
		p.log.Info("park fresh run", "run_id", run.ID, "reason", err.Error())
		res.Path = "fresh"
		o.Prompt = strings.Join(capsules, "\n\n")
		next, err = p.resume.Fresh(ctx, run.ID, o)
	}
	res.Err = err
	if next != nil {
		res.RunID, res.Turn = next.ID, next.TurnCount
	}
	return res
}

// park returns the end reason of a run that must park, or "" when no gate
// has an item. An error reads as no item: a run must not park on a store fault.
func (p *Parker) park(ctx context.Context, runID string) EndReason {
	var end EndReason
	for _, g := range p.gates {
		waiting, ready, err := g.Open(ctx, runID)
		if err != nil {
			p.log.Warn("park gate", "gate", g.Name(), "run_id", runID, "err", err)
			continue
		}
		if !waiting && !ready {
			continue
		}
		if g.Name() == GateRequest {
			return EndRequest
		}
		end = EndAsk
	}
	return end
}

// Gate names.
const (
	GateRequest = "request"
	GateAsk     = "ask"
)

// SetParker wires the park coordinator. Nil (the default) never parks.
func (s *Service) SetParker(p *Parker) {
	s.mu.Lock()
	s.parker = p
	s.mu.Unlock()
}

func (s *Service) parkerOf() *Parker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.parker
}

// WakeParked resumes a parked run when all its items are ready.
func (s *Service) WakeParked(ctx context.Context, runID string) error {
	p := s.parkerOf()
	if p == nil {
		return nil
	}
	return p.Wake(ctx, runID)
}

// promptHints are the gate texts of a fresh job or agent launch. A resumed
// session saw them at its first launch, and the assistant uses its own tools.
func (s *Service) promptHints(opts LaunchOpts) []string {
	p := s.parkerOf()
	if p == nil || opts.ResumeSessionID != "" || (opts.Kind != "" && opts.Kind != KindJob && opts.Kind != KindAgent) {
		return nil
	}
	var out []string
	for _, g := range p.gates {
		out = append(out, g.PromptHint(opts))
	}
	return out
}

// parkEnd reports why a run must park, or "" when it need not.
func (s *Service) parkEnd(ctx context.Context, runID string) EndReason {
	p := s.parkerOf()
	if p == nil {
		return ""
	}
	return p.park(ctx, runID)
}

// mustPark reports whether a run waits for a request or a reply.
func (s *Service) mustPark(ctx context.Context, runID string) bool {
	return s.parkEnd(ctx, runID) != ""
}

// parkStatus changes a clean end to parked when a gate has an item. A
// failure status stays as it is.
func (s *Service) parkStatus(ctx context.Context, runID string, status Status, end EndReason) (Status, EndReason) {
	if status != StatusParked && status != StatusComplete && status != StatusExited {
		return status, end
	}
	if pe := s.parkEnd(ctx, runID); pe != "" {
		return StatusParked, pe
	}
	if status == StatusParked {
		return StatusComplete, end
	}
	return status, end
}

// runEnded tells each gate that a turn ended, then wakes the run.
func (s *Service) runEnded(ctx context.Context, runID string, st Status) {
	p := s.parkerOf()
	if p == nil {
		return
	}
	for _, g := range p.gates {
		g.RunEnded(ctx, runID, st)
	}
	if err := p.Wake(ctx, runID); err != nil {
		s.log.Warn("park wake", "run_id", runID, "err", err)
	}
}

// Unpark completes a parked run whose items closed with nothing to deliver
// (a dismiss). A run that still has an item stays parked.
func (s *Service) Unpark(ctx context.Context, runID string, end EndReason) error {
	if s.parkEnd(ctx, runID) != "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, end_reason = ?, revision = revision + 1, updated_at = ?
			WHERE id = ? AND status = ?`, string(StatusComplete), string(end), now, runID, string(StatusParked))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("unpark %s: %w", runID, ErrNotParked)
		}
		_, err = tx.ExecContext(ctx, `UPDATE run_turns SET status = ?, end_reason = ?
			WHERE run_id = ? AND n = (SELECT turn_count FROM runs WHERE id = ?) AND status = ?`,
			string(StatusComplete), string(end), runID, runID, string(StatusParked))
		return err
	}, store.BusEvent{
		Topic: "run.updated", Entity: "run", EntityID: runID, Action: "updated",
		Attrs: map[string]string{"status": string(StatusComplete), "end_reason": string(end)},
	})
	return err
}

// TurnAfter returns the number of the first turn of runID with reason that
// started at or after since. It is 0 when there is none.
func (s *Service) TurnAfter(ctx context.Context, runID string, reason RunReason, since time.Time) (int, error) {
	var n int
	err := s.st.DB().QueryRowContext(ctx, `SELECT n FROM run_turns WHERE run_id=? AND reason=? AND started_at>=?
		ORDER BY n LIMIT 1`, runID, string(reason), since.UTC().Format(time.RFC3339Nano)).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}
