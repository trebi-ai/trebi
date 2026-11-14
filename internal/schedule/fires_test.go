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

func TestFiresInWindow(t *testing.T) {
	t.Parallel()
	svc, _ := firesEnv(t)
	ctx := context.Background()
	t0 := time.Now().UTC()
	if err := svc.RecomputeFromJob(ctx, "ws1", triggerJob(t, "jb_every", "every: 1h"), t0); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecomputeFromJob(ctx, "ws2", triggerJob(t, "jb_other", "every: 1h"), t0); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Fires(ctx, schedule.FiresOpts{Workspace: "ws1", From: t0.Add(-time.Hour), To: t0.Add(4*time.Hour + 30*time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("fires %d, want 4: %+v", len(got), got)
	}
	for i, f := range got {
		if f.JobID != "jb_every" || f.WorkspaceID != "ws1" || f.At.Before(t0) {
			t.Errorf("fire %d: %+v", i, f)
		}
		if i > 0 && f.At.Sub(got[i-1].At) != time.Hour {
			t.Errorf("fire %d is not 1h after the one before: %v", i, f.At)
		}
	}
	all, err := svc.Fires(ctx, schedule.FiresOpts{From: t0, To: t0.Add(90 * time.Minute)})
	if err != nil || len(all) != 2 {
		t.Fatalf("both workspaces: %d fires, err %v", len(all), err)
	}
	none, err := svc.Fires(ctx, schedule.FiresOpts{From: t0, To: t0.Add(30 * time.Minute)})
	if err != nil || len(none) != 0 {
		t.Fatalf("a window before the first due time: %d fires, err %v", len(none), err)
	}
}

func TestFiresUsesTimezone(t *testing.T) {
	t.Parallel()
	svc, _ := firesEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for id, zone := range map[string]string{"jb_utc": "UTC", "jb_sp": "America/Sao_Paulo"} {
		j := triggerJob(t, id, `cron: "0 9 * * *"`)
		j.Triggers[0].Timezone = zone
		if err := svc.RecomputeFromJob(ctx, "ws1", j, now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.Fires(ctx, schedule.FiresOpts{From: now, To: now.Add(49 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	at := map[string][]time.Time{}
	for _, f := range got {
		at[f.JobID] = append(at[f.JobID], f.At)
	}
	if len(at["jb_utc"]) < 2 || len(at["jb_sp"]) < 2 {
		t.Fatalf("fires per job: %v", at)
	}
	if h := at["jb_utc"][0].Hour(); h != 9 {
		t.Errorf("utc fire at hour %d, want 9", h)
	}
	if h := at["jb_sp"][0].Hour(); h != 12 {
		t.Errorf("Sao Paulo fire at hour %d UTC, want 12", h)
	}
}

func TestFiresSkipsArtifactAndReportRows(t *testing.T) {
	t.Parallel()
	svc, st := firesEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := svc.SetArtifact(ctx, "ws1", "art_1", "@every 1h", "UTC"); err != nil {
		t.Fatal(err)
	}
	due := now.Add(time.Hour).Format(time.RFC3339)
	if _, err := st.DB().Exec(`INSERT INTO schedules (workspace_id, job_id, idx, spec, timezone, catch_up, next_due_at, updated_at)
		VALUES ('ws1','agt_1',-1,'@every 1h','UTC',0,?,?)`, due, due); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecomputeFromJob(ctx, "ws1", triggerJob(t, "jb_real", "every: 1h"), now); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Fires(ctx, schedule.FiresOpts{From: now, To: now.Add(3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if f.JobID != "jb_real" {
			t.Fatalf("fire of %q: only a job schedule plans a fire", f.JobID)
		}
	}
	if len(got) == 0 {
		t.Fatal("no fire of the job")
	}
}

func TestFiresPerRowCap(t *testing.T) {
	t.Parallel()
	svc, _ := firesEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := svc.RecomputeFromJob(ctx, "ws1", triggerJob(t, "jb_fast", "every: 1m"), now); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Fires(ctx, schedule.FiresOpts{From: now, To: now.Add(24 * time.Hour), PerRow: 10})
	if err != nil || len(got) != 10 {
		t.Fatalf("fires %d, err %v, want the cap of 10", len(got), err)
	}
	def, err := svc.Fires(ctx, schedule.FiresOpts{From: now, To: now.Add(30 * 24 * time.Hour)})
	if err != nil || len(def) != 5000 {
		t.Fatalf("default cap: %d fires, err %v, want 5000", len(def), err)
	}
}

func firesEnv(t *testing.T) (*schedule.Service, *store.Store) {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return schedule.New(st), st
}

func triggerJob(t *testing.T, id, trigger string) *job.Job {
	t.Helper()
	j, err := job.ParseBytes("/ws/demo/JOB.md", []byte("---\nworking_dir: .\nharness: claude\ntriggers:\n  - "+trigger+"\n---\nhi\n"))
	if err != nil {
		t.Fatal(err)
	}
	j.Name, j.ID = id, id
	return j
}
