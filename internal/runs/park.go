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

	agentwire "github.com/trebi-ai/agent-wire"

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

// liveWriter writes a prompt to the live session of a running run.
type liveWriter interface {
	hasLive(runID string) bool
	writeLive(ctx context.Context, runID, text, origin string) (agentwire.Delivery, error)
}

// Parker delivers messages and ready items to a run: to its live session,
// or by a resume when the run parked or ended.
type Parker struct {
	runs   runGetter
	resume Resumer
	gates  []ParkGate
	log    *slog.Logger

	// One delivery per run at a time. A wake that comes in while one runs (a
	// launch failure ends the turn again) sets again, and the first repeats.
	// A delivery with a message waits for its turn.
	mu    sync.Mutex
	idle  *sync.Cond
	busy  map[string]bool
	again map[string]bool
}

// NewParker builds the park coordinator.
func NewParker(rs runGetter, r Resumer, gates ...ParkGate) *Parker {
	p := &Parker{runs: rs, resume: r, gates: gates, log: slog.Default().With("source", "parker"),
		busy: map[string]bool{}, again: map[string]bool{}}
	p.idle = sync.NewCond(&p.mu)
	return p
}

// DeliverResult is the outcome of one Deliver. Delivery is empty when there
// was nothing to deliver.
type DeliverResult struct {
	Delivery string `json:"delivery"` // steered|prompted|held|resumed
	RunID    string `json:"run_id"`
	Turn     int    `json:"turn"`
}

// Delivery values that Deliver adds to the agentwire ones.
const (
	DeliveryResumed = "resumed"
	DeliveryPane    = "pane"
)

// Wake delivers the ready items of runID at turn end and at boot.
func (p *Parker) Wake(ctx context.Context, runID string) error {
	_, err := p.Deliver(ctx, runID)
	return err
}

// Deliver sends extra (a person's message) and every ready item to runID
// now. A running run gets them on its live session. A parked or ended run
// resumes. With no extra, a parked run waits while a gate still waits.
func (p *Parker) Deliver(ctx context.Context, runID string, extra ...ParkDelivery) (DeliverResult, error) {
	p.mu.Lock()
	if len(extra) == 0 && p.busy[runID] {
		p.again[runID] = true
		p.mu.Unlock()
		return DeliverResult{}, nil
	}
	for p.busy[runID] {
		p.idle.Wait()
	}
	p.busy[runID] = true
	p.mu.Unlock()
	res, err := p.deliver(ctx, runID, extra)
	for {
		p.mu.Lock()
		if p.again[runID] {
			delete(p.again, runID)
			p.mu.Unlock()
			if _, aerr := p.deliver(ctx, runID, nil); aerr != nil {
				p.log.Warn("park wake", "run_id", runID, "err", aerr)
			}
			continue
		}
		delete(p.busy, runID)
		p.idle.Broadcast()
		p.mu.Unlock()
		return res, err
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

func (p *Parker) deliver(ctx context.Context, runID string, extra []ParkDelivery) (DeliverResult, error) {
	run, err := p.runs.Get(ctx, runID)
	if err != nil {
		return DeliverResult{}, fmt.Errorf("run %s: %w", runID, err)
	}
	if run.Status == StatusRunning {
		return p.deliverLive(ctx, run, extra)
	}
	if len(extra) == 0 {
		if run.Status != StatusParked {
			return DeliverResult{}, nil
		}
		for _, g := range p.gates {
			waiting, _, err := g.Open(ctx, runID)
			if err != nil {
				return DeliverResult{}, fmt.Errorf("%s gate: %w", g.Name(), err)
			}
			if waiting {
				return DeliverResult{}, nil
			}
		}
	} else if !run.Status.IsTerminal() && run.Status != StatusParked {
		return DeliverResult{}, fmt.Errorf("run %s is %s: %w", runID, run.Status, ErrRunActive)
	}
	claimed, all := p.claim(ctx, runID, func(ParkGate) bool { return true })
	all = append(all, extra...)
	if len(all) == 0 {
		return DeliverResult{}, nil
	}
	res := p.launch(ctx, run, all, len(extra) > 0)
	p.settle(ctx, runID, claimed, res)
	if res.Err != nil {
		return DeliverResult{}, fmt.Errorf("resume run %s: %w", runID, res.Err)
	}
	return DeliverResult{Delivery: DeliveryResumed, RunID: res.RunID, Turn: res.Turn}, nil
}

// deliverLive writes extra and the ready request answers to the live session
// of a running run. Chain replies wait for the turn end, so they keep their
// turn frame.
func (p *Parker) deliverLive(ctx context.Context, run *Run, extra []ParkDelivery) (DeliverResult, error) {
	lw, ok := p.runs.(liveWriter)
	if !ok || !lw.hasLive(run.ID) {
		if len(extra) > 0 {
			return DeliverResult{}, fmt.Errorf("run %s has no live session: %w", run.ID, ErrRunActive)
		}
		return DeliverResult{}, nil
	}
	claimed, all := p.claim(ctx, run.ID, func(g ParkGate) bool { return g.Name() == GateRequest })
	all = append(all, extra...)
	if len(all) == 0 {
		return DeliverResult{}, nil
	}
	prompt, _ := joinDeliveries(all)
	origin := OriginRequest
	if len(extra) > 0 {
		origin = OriginMessage
	}
	d, err := lw.writeLive(ctx, run.ID, prompt, origin)
	p.settle(ctx, run.ID, claimed, ResumeResult{RunID: run.ID, Turn: run.TurnCount, Path: "live", Err: err})
	if err != nil {
		return DeliverResult{}, fmt.Errorf("deliver to run %s: %w", run.ID, err)
	}
	return DeliverResult{Delivery: string(d), RunID: run.ID, Turn: run.TurnCount}, nil
}

// claim takes the ready items of the gates that pick selects.
func (p *Parker) claim(ctx context.Context, runID string, pick func(ParkGate) bool) (map[ParkGate][]ParkDelivery, []ParkDelivery) {
	claimed := map[ParkGate][]ParkDelivery{}
	var all []ParkDelivery
	for _, g := range p.gates {
		if !pick(g) {
			continue
		}
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
	return claimed, all
}

func (p *Parker) settle(ctx context.Context, runID string, claimed map[ParkGate][]ParkDelivery, res ResumeResult) {
	for g, d := range claimed {
		if err := g.Settle(ctx, runID, d, res); err != nil {
			p.log.Warn("park settle", "gate", g.Name(), "run_id", runID, "err", err)
		}
	}
}

// joinDeliveries sorts the deliveries by time and joins their prompts and
// their capsules.
func joinDeliveries(all []ParkDelivery) (prompt, capsule string) {
	slices.SortStableFunc(all, func(a, b ParkDelivery) int { return a.At.Compare(b.At) })
	var prompts, capsules []string
	for _, d := range all {
		prompts = append(prompts, d.Prompt)
		capsules = append(capsules, cmp.Or(d.Capsule, d.Prompt))
	}
	return strings.Join(prompts, "\n\n"), strings.Join(capsules, "\n\n")
}

// launch resumes the session with all deliveries in one prompt. A harness
// that cannot resume gets a fresh run with the capsule prompts, except for a
// person's message: it never starts a fresh run.
func (p *Parker) launch(ctx context.Context, run *Run, all []ParkDelivery, message bool) ResumeResult {
	prompt, capsule := joinDeliveries(all)
	o := ResumeOpts{Reason: ReasonRequest, Prompt: prompt}
	if message {
		o.Reason = ReasonMessage
	}
	// A reply keeps its chain frame, also when a message joins it.
	for _, d := range all {
		if d.Reason == ReasonReply {
			o.Reason, o.HopID, o.TraceID, o.Path = ReasonReply, d.HopID, d.TraceID, d.Path
			break
		}
	}
	res := ResumeResult{Path: "resume"}
	var next *Run
	err := ErrNoSession
	if run.SessionID != "" {
		next, err = p.resume.Resume(ctx, run.ID, o)
	}
	if message && (errors.Is(err, ErrResumeUnsupported) || errors.Is(err, ErrNoSession)) {
		res.Err = err
		return res
	}
	if errors.Is(err, ErrResumeUnsupported) || errors.Is(err, ErrNoSession) {
		p.log.Info("park fresh run", "run_id", run.ID, "reason", err.Error())
		res.Path = "fresh"
		o.Prompt = capsule
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

// Gate names. GateMessage names a person's message, which no gate owns.
const (
	GateRequest = "request"
	GateAsk     = "ask"
	GateMessage = "message"
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

// Deliver sends extra and the ready items of runID to its session in its
// current state. With no parker it does nothing.
func (s *Service) Deliver(ctx context.Context, runID string, extra ...ParkDelivery) (DeliverResult, error) {
	p := s.parkerOf()
	if p == nil {
		return DeliverResult{}, nil
	}
	return p.Deliver(ctx, runID, extra...)
}

// hasLive reports whether runID has a live CLI session.
func (s *Service) hasLive(runID string) bool { return s.cliRunFor(runID) != nil }

// writeLive writes text to the live CLI session of runID.
func (s *Service) writeLive(ctx context.Context, runID, text, origin string) (agentwire.Delivery, error) {
	cr := s.cliRunFor(runID)
	if cr == nil {
		return "", fmt.Errorf("run %s: %w", runID, ErrNotRunning)
	}
	return s.promptCLI(ctx, runID, cr, agentwire.Prompt{Text: text, Kind: "followup"}, origin)
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
