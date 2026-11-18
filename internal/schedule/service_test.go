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

func TestDefaultTimeZone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	zone := "Europe/Berlin"
	open := func() (*store.Store, *schedule.Service) {
		st, err := store.OpenPath(filepath.Join(t.TempDir(), "s.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st, schedule.New(st, schedule.WithDefaultTimeZone(func() string { return zone }))
	}

	st, svc := open()
	if err := svc.RecomputeFromJob(ctx, "ws1", dailyAt8(t), from); err != nil {
		t.Fatal(err)
	}
	if due, tz := nextDueOf(t, st); due != "2026-06-01T06:00:00Z" || tz != "" {
		t.Fatalf("berlin: due %s zone %q, want 06:00Z and an empty stored zone", due, tz)
	}
	rows, err := svc.List(ctx, schedule.ListOpts{})
	if err != nil || len(rows) != 1 || rows[0].Timezone != "Europe/Berlin" {
		t.Fatalf("list = %+v %v", rows, err)
	}

	// A change of the default moves the row after a recompute.
	zone = "America/Sao_Paulo"
	if err := svc.RecomputeDefaultZone(ctx); err != nil {
		t.Fatal(err)
	}
	due, _ := nextDueOf(t, st)
	next, err := time.Parse(time.RFC3339, due)
	if err != nil {
		t.Fatal(err)
	}
	if h := next.In(time.FixedZone("BRT", -3*3600)).Hour(); h != 8 {
		t.Fatalf("after recompute: due %s is not 08:00 in Sao Paulo", due)
	}
	if rows, _ := svc.List(ctx, schedule.ListOpts{}); rows[0].Timezone != "America/Sao_Paulo" {
		t.Fatalf("list zone = %q", rows[0].Timezone)
	}

	// With no default, the zone is UTC.
	st, _ = open()
	plain := schedule.New(st)
	if err := plain.RecomputeFromJob(ctx, "ws1", dailyAt8(t), from); err != nil {
		t.Fatal(err)
	}
	if due, _ := nextDueOf(t, st); due != "2026-06-01T08:00:00Z" {
		t.Fatalf("utc: due %s", due)
	}
}

func TestExplicitTimeZoneIgnoresDefault(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := schedule.New(st, schedule.WithDefaultTimeZone(func() string { return "Europe/Berlin" }))
	j, err := job.ParseBytes("/ws/demo/JOB.md", []byte("---\nworking_dir: .\nharness: claude\ntriggers:\n  - cron: \"0 8 * * *\"\n    timezone: UTC\n---\nhi\n"))
	if err != nil {
		t.Fatal(err)
	}
	j.Name, j.ID = "demo", "jb_00000000d2"
	if err := svc.RecomputeFromJob(context.Background(), "ws1", j, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if due, tz := nextDueOf(t, st); due != "2026-06-01T08:00:00Z" || tz != "UTC" {
		t.Fatalf("due %s zone %q", due, tz)
	}
}

func dailyAt8(t *testing.T) *job.Job {
	t.Helper()
	j, err := job.ParseBytes("/ws/demo/JOB.md", []byte("---\nworking_dir: .\nharness: claude\ntriggers:\n  - cron: \"0 8 * * *\"\n---\nhi\n"))
	if err != nil {
		t.Fatal(err)
	}
	j.Name, j.ID = "demo", "jb_00000000d2"
	return j
}

func nextDueOf(t *testing.T, st *store.Store) (due, zone string) {
	t.Helper()
	if err := st.DB().QueryRow(`SELECT next_due_at, timezone FROM schedules WHERE job_id='jb_00000000d2'`).Scan(&due, &zone); err != nil {
		t.Fatal(err)
	}
	return due, zone
}

const archiveJobMD = `---
working_dir: .
harness: claude
triggers:
  - every: 1h
---
hi
`

func TestRecomputeDeletesArchived(t *testing.T) {
	t.Parallel()
	svc, st, j := archiveSchedEnv(t)
	ctx := context.Background()
	from := time.Now().UTC()
	if err := svc.RecomputeFromJob(ctx, "ws1", j, from); err != nil {
		t.Fatal(err)
	}
	if scheduleCount(t, st) != 1 {
		t.Fatal("want one schedule row")
	}
	for _, archived := range []func(){
		func() { j.ArchivedAt = "2026-11-18T10:00:00Z" },
		func() { j.ArchivedAt, j.OwnerArchivedAt = "", "2026-11-18T10:00:00Z" },
	} {
		archived()
		if err := svc.RecomputeFromJob(ctx, "ws1", j, from); err != nil {
			t.Fatal(err)
		}
		if n := scheduleCount(t, st); n != 0 {
			t.Fatalf("archived job keeps %d rows", n)
		}
		j.ArchivedAt, j.OwnerArchivedAt = "", ""
		if err := svc.RecomputeFromJob(ctx, "ws1", j, from); err != nil {
			t.Fatal(err)
		}
		var next string
		if err := st.DB().QueryRow(`SELECT next_due_at FROM schedules`).Scan(&next); err != nil {
			t.Fatalf("unarchive made no row: %v", err)
		}
		if due, _ := time.Parse(time.RFC3339, next); !due.After(from) {
			t.Fatalf("next_due_at %s is not in the future: catch-up fire", next)
		}
	}
}

func TestClaimDueSkipsArchivedJobAndOwner(t *testing.T) {
	t.Parallel()
	svc, st, _ := archiveSchedEnv(t)
	ctx := context.Background()
	const at = "2026-11-18T10:00:00Z"
	idx := func(id, kind, owner, archivedAt string) {
		t.Helper()
		if _, err := st.DB().Exec(`INSERT INTO job_index (id, workspace_id, name, path, kind, owner_id, archived_at, updated_at)
			VALUES (?, 'ws1', ?, ?, ?, ?, NULLIF(?, ''), ?)`, id, id, id, kind, owner, archivedAt, at); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`INSERT INTO schedules (workspace_id, job_id, idx, spec, next_due_at, updated_at)
			VALUES ('ws1', ?, 0, '@every 1h', '2020-01-01T00:00:00Z', ?)`, id, at); err != nil {
			t.Fatal(err)
		}
	}
	idx("agt_old", "agent", "", at)
	idx("jb_own", "job", "agt_live", at)
	idx("jb_kid", "job", "agt_old", "")
	idx("agt_live", "agent", "", "")
	idx("jb_ok", "job", "agt_live", "")
	due, err := svc.ClaimDue(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range due {
		got[r.JobID] = true
	}
	if len(got) != 2 || !got["agt_live"] || !got["jb_ok"] {
		t.Fatalf("claimed %v, want agt_live and jb_ok", got)
	}
}

func archiveSchedEnv(t *testing.T) (*schedule.Service, *store.Store, *job.Job) {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	j, err := job.ParseBytes("/ws/demo/JOB.md", []byte(archiveJobMD))
	if err != nil {
		t.Fatal(err)
	}
	j.Name, j.ID = "demo", "jb_00000000d1"
	return schedule.New(st), st, j
}

func scheduleCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM schedules`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
