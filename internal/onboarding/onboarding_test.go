package onboarding_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/trebi-ai/trebi/internal/hometest"
	"github.com/trebi-ai/trebi/internal/onboarding"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestMain(m *testing.M) { hometest.Main(m) }

func TestGetWithNoRow(t *testing.T) {
	t.Parallel()
	st, err := onboarding.New(openTest(t)).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Step != onboarding.StepYou || st.Revision != 0 || st.CompletedAt != nil || st.Hidden == nil || len(st.Hidden) != 0 {
		t.Fatalf("state = %+v", st)
	}
}

func TestUpdateFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := onboarding.New(openTest(t))
	step, job, done := onboarding.StepAI, "job_1", true

	st, err := svc.Update(ctx, onboarding.Patch{Step: &step, FirstJobID: &job, Hide: []onboarding.Card{onboarding.CardPro, onboarding.CardSetUp, onboarding.CardPro}})
	if err != nil {
		t.Fatal(err)
	}
	if st.Step != step || st.FirstJobID != job || st.Revision != 1 || !slices.Equal(st.Hidden, []onboarding.Card{onboarding.CardPro, onboarding.CardSetUp}) {
		t.Fatalf("after first patch: %+v", st)
	}

	st, err = svc.Update(ctx, onboarding.Patch{Completed: &done, Unhide: []onboarding.Card{onboarding.CardPro}})
	if err != nil {
		t.Fatal(err)
	}
	if st.CompletedAt == nil || st.Step != onboarding.StepDone || st.Revision != 2 || !slices.Equal(st.Hidden, []onboarding.Card{onboarding.CardSetUp}) {
		t.Fatalf("after second patch: %+v", st)
	}

	got, err := svc.Get(ctx)
	if err != nil || got.Revision != 2 || got.CompletedAt == nil || got.FirstJobID != job {
		t.Fatalf("Get = %+v %v", got, err)
	}

	notDone := false
	if st, err = svc.Update(ctx, onboarding.Patch{Completed: &notDone}); err != nil || st.CompletedAt != nil {
		t.Fatalf("clear completed: %+v %v", st, err)
	}
}

func TestUpdateStaleRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := onboarding.New(openTest(t))
	step := onboarding.StepAI
	if _, err := svc.Update(ctx, onboarding.Patch{Step: &step}); err != nil {
		t.Fatal(err)
	}
	stale, current := int64(0), int64(1)
	next := onboarding.StepFirstJob
	if _, err := svc.Update(ctx, onboarding.Patch{Step: &next, Revision: &stale}); !errors.Is(err, onboarding.ErrRevision) {
		t.Fatalf("stale revision: %v", err)
	}
	if st, err := svc.Update(ctx, onboarding.Patch{Step: &next, Revision: &current}); err != nil || st.Revision != 2 {
		t.Fatalf("current revision: %+v %v", st, err)
	}
}

func TestUpdateRejectsUnknownValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := onboarding.New(openTest(t))
	bad := onboarding.Step("nope")
	if _, err := svc.Update(ctx, onboarding.Patch{Step: &bad}); !errors.Is(err, onboarding.ErrInvalid) {
		t.Fatalf("step: %v", err)
	}
	if _, err := svc.Update(ctx, onboarding.Patch{Hide: []onboarding.Card{"nope"}}); !errors.Is(err, onboarding.ErrInvalid) {
		t.Fatalf("card: %v", err)
	}
	if st, _ := svc.Get(ctx); st.Revision != 0 {
		t.Fatalf("a rejected patch wrote: %+v", st)
	}
}

func TestRestartKeepsWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := onboarding.New(st)
	done, job := true, "job_1"
	if _, err := svc.Update(ctx, onboarding.Patch{Completed: &done, FirstJobID: &job, Hide: []onboarding.Card{onboarding.CardPro}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE system_flags SET value = json_set(value, '$.workspace_id', 'ws_1') WHERE key = 'onboarding'`); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Restart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Step != onboarding.StepYou || got.CompletedAt != nil || got.WorkspaceID != "ws_1" || got.FirstJobID != job ||
		!slices.Equal(got.Hidden, []onboarding.Card{onboarding.CardPro}) || got.Revision != 2 {
		t.Fatalf("restart = %+v", got)
	}
}

func TestUpdateEmitsBus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := onboarding.New(st)
	step := onboarding.StepAI
	if _, err := svc.Update(ctx, onboarding.Patch{Step: &step}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	evs, err := st.ListBus(ctx, 0, []string{"onboarding.updated"}, 10)
	if err != nil || len(evs) != 2 {
		t.Fatalf("bus rows = %d %v", len(evs), err)
	}
	if evs[0].Revision != 1 || evs[1].Revision != 2 || evs[0].Entity != "onboarding" {
		t.Fatalf("frames = %+v", evs)
	}
}

func openTest(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
