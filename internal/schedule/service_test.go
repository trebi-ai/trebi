package schedule_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestRecomputeAndClaim(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := schedule.New(st)
	ctx := context.Background()

	j, err := job.ParseBytes("/ws/demo/JOB.md", []byte(`---
working_dir: .
harness: claude
triggers:
  - cron: "0 0 1 1 *"
  - every: 1h
---
hi
`))
	if err != nil {
		t.Fatal(err)
	}
	j.Name = "demo"
	j.ID = "jb_00000000d1"
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := svc.RecomputeFromJob(ctx, "ws1", j, from); err != nil {
		t.Fatal(err)
	}

	// Force a due time in the past for claim.
	_, err = st.DB().Exec(`UPDATE schedules SET next_due_at=? WHERE job_id='jb_00000000d1'`, "2020-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	due, err := svc.ClaimDue(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) < 1 {
		t.Fatal("expected due schedules")
	}
	if err := svc.MarkFired(ctx, due[0], time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestRecomputePreservesDueTimes(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := schedule.New(st)
	ctx := context.Background()

	j, err := job.ParseBytes("/ws/demo/JOB.md", []byte(`---
working_dir: .
harness: claude
triggers:
  - every: 1h
---
hi
`))
	if err != nil {
		t.Fatal(err)
	}
	j.Name = "demo"
	j.ID = "jb_00000000d1"
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := svc.RecomputeFromJob(ctx, "ws1", j, from); err != nil {
		t.Fatal(err)
	}
	var next1 string
	if err := st.DB().QueryRow(`SELECT next_due_at FROM schedules WHERE job_id='jb_00000000d1'`).Scan(&next1); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecomputeFromJob(ctx, "ws1", j, time.Time{}); err != nil {
		t.Fatal(err)
	}
	var next2 string
	if err := st.DB().QueryRow(`SELECT next_due_at FROM schedules WHERE job_id='jb_00000000d1'`).Scan(&next2); err != nil {
		t.Fatal(err)
	}
	if next1 != next2 {
		t.Fatalf("next_due_at changed across no-op rebuild: %s -> %s", next1, next2)
	}
}

func TestNextDueUsesTimezone(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	utc := schedule.NextDue("0 9 * * *", from, "UTC")
	sp := schedule.NextDue("0 9 * * *", from, "America/Sao_Paulo")
	if utc.Equal(sp) {
		t.Fatalf("timezone ignored: both %s", utc)
	}
}
