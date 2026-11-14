// Package onboarding owns the setup state of a first run: the step, the
// hidden Home cards, and the defaults that need no screen.
package onboarding

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
)

// Step is the screen of the setup flow.
type Step string

const (
	StepYou      Step = "you"
	StepAI       Step = "ai"
	StepFirstJob Step = "first_job"
	StepDone     Step = "done"
)

// Valid reports whether s is a known step.
func (s Step) Valid() bool {
	switch s {
	case StepYou, StepAI, StepFirstJob, StepDone:
		return true
	}
	return false
}

// Card is a Home card that the user can hide.
type Card string

const (
	CardFirstRun Card = "first_run"
	CardSetUp    Card = "set_up"
	CardPro      Card = "pro"
)

// Valid reports whether c is a known card.
func (c Card) Valid() bool {
	switch c {
	case CardFirstRun, CardSetUp, CardPro:
		return true
	}
	return false
}

// State is the onboarding record. Revision counts the writes.
type State struct {
	Step        Step       `json:"step"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Hidden      []Card     `json:"hidden"`
	FirstJobID  string     `json:"first_job_id,omitempty"`
	WorkspaceID string     `json:"workspace_id,omitempty"`
	Revision    int64      `json:"revision"`
}

// Patch is a partial update. A nil field stays. Revision, when set, must
// match the stored revision.
type Patch struct {
	Step       *Step   `json:"step,omitempty"`
	Completed  *bool   `json:"completed,omitempty"`
	Hide       []Card  `json:"hide,omitempty"`
	Unhide     []Card  `json:"unhide,omitempty"`
	FirstJobID *string `json:"first_job_id,omitempty"`
	Revision   *int64  `json:"revision,omitempty"`
}

// ErrRevision reports a patch made on a stale revision.
var ErrRevision = errors.New("onboarding: stale revision")

// ErrInvalid reports a patch with an unknown step or card.
var ErrInvalid = errors.New("onboarding: invalid value")

const flagKey = "onboarding"

// Service reads and writes the onboarding state in system_flags.
type Service struct {
	st  *store.Store
	log *slog.Logger
}

func New(st *store.Store) *Service {
	return &Service{st: st, log: slog.Default().With("source", "onboarding")}
}

func newState() State { return State{Step: StepYou, Hidden: []Card{}} }

// Get returns the state. A first run has no row and returns the first step.
func (s *Service) Get(ctx context.Context) (State, error) {
	var raw string
	err := s.st.DB().QueryRowContext(ctx, `SELECT value FROM system_flags WHERE key = ?`, flagKey).Scan(&raw)
	return decode(raw, err)
}

func decode(raw string, err error) (State, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return newState(), nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read onboarding state: %w", err)
	}
	st := newState()
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return State{}, fmt.Errorf("decode onboarding state: %w", err)
	}
	if st.Hidden == nil {
		st.Hidden = []Card{}
	}
	return st, nil
}

// Update applies p, bumps the revision, and publishes onboarding.updated.
func (s *Service) Update(ctx context.Context, p Patch) (State, error) {
	if err := p.validate(); err != nil {
		return State{}, err
	}
	return s.write(ctx, func(st *State) error {
		if p.Revision != nil && *p.Revision != st.Revision {
			return fmt.Errorf("%w: have %d, got %d", ErrRevision, st.Revision, *p.Revision)
		}
		p.apply(st)
		return nil
	})
}

// Restart returns to the first step. It keeps the hidden cards, the
// workspace, and the first job.
func (s *Service) Restart(ctx context.Context) (State, error) {
	return s.write(ctx, func(st *State) error {
		st.Step, st.CompletedAt = StepYou, nil
		return nil
	})
}

// setWorkspace stores the default workspace id.
func (s *Service) setWorkspace(ctx context.Context, id string) (State, error) {
	return s.write(ctx, func(st *State) error {
		st.WorkspaceID = id
		return nil
	})
}

// write runs mutate on the stored state, bumps the revision, and stores the
// row with its bus row in one transaction. The bus row carries the new
// revision, so the write cannot use AppendEntity (it takes the event first).
func (s *Service) write(ctx context.Context, mutate func(*State) error) (State, error) {
	var out State
	s.st.LockBus()
	defer s.st.UnlockBus()
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		var raw string
		scanErr := tx.QueryRowContext(ctx, `SELECT value FROM system_flags WHERE key = ?`, flagKey).Scan(&raw)
		cur, err := decode(raw, scanErr)
		if err != nil {
			return err
		}
		if err := mutate(&cur); err != nil {
			return err
		}
		cur.Revision++
		b, err := json.Marshal(cur)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO system_flags (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, flagKey, string(b)); err != nil {
			return err
		}
		out = cur
		_, err = store.AppendInTx(tx, store.BusEvent{
			Topic: "onboarding.updated", Entity: "onboarding", EntityID: flagKey, Action: "updated", Revision: cur.Revision,
		})
		return err
	})
	if err != nil {
		return State{}, err
	}
	s.st.NotifyBus()
	return out, nil
}

func (p Patch) validate() error {
	if p.Step != nil && !p.Step.Valid() {
		return fmt.Errorf("%w: unknown step %q", ErrInvalid, *p.Step)
	}
	for _, c := range slices.Concat(p.Hide, p.Unhide) {
		if !c.Valid() {
			return fmt.Errorf("%w: unknown card %q", ErrInvalid, c)
		}
	}
	return nil
}

func (p Patch) apply(st *State) {
	if p.Step != nil {
		st.Step = *p.Step
	}
	if p.Completed != nil {
		switch {
		case *p.Completed && st.CompletedAt == nil:
			now := time.Now().UTC()
			st.CompletedAt = &now
			if p.Step == nil {
				st.Step = StepDone
			}
		case !*p.Completed:
			st.CompletedAt = nil
		}
	}
	for _, c := range p.Hide {
		if !slices.Contains(st.Hidden, c) {
			st.Hidden = append(st.Hidden, c)
		}
	}
	for _, c := range p.Unhide {
		st.Hidden = slices.DeleteFunc(st.Hidden, func(h Card) bool { return h == c })
	}
	if p.FirstJobID != nil {
		st.FirstJobID = *p.FirstJobID
	}
}
