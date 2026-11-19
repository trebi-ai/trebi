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

func TestReportScheduleRow(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "report.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := schedule.New(st)
	j := &job.Job{Name: "research", Kind: "agent", Report: &job.ReportYAML{Every: "168h"}}
	if err := svc.RecomputeFromJob(ctx, "ws1", j, time.Time{}); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.List(ctx, schedule.ListOpts{Workspace: "ws1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Idx != -1 || rows[0].Spec != "@every 168h" {
		t.Fatalf("rows = %+v", rows)
	}
	j.Report = nil
	if err := svc.RecomputeFromJob(ctx, "ws1", j, time.Time{}); err != nil {
		t.Fatal(err)
	}
	rows, err = svc.List(ctx, schedule.ListOpts{Workspace: "ws1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("report survived removal: %+v", rows)
	}
}
