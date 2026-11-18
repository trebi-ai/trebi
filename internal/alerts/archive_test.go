package alerts_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestCloseForJobs(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	failure, waiting := archiveItems(t, svc, st, "job_a", "running")
	otherFailure, _ := archiveItems(t, svc, st, "job_b", "running")

	n, err := svc.CloseForJobs(ctx, []string{"job_a"}, "archived")
	if err != nil || n != 1 {
		t.Fatalf("closed %d, err %v", n, err)
	}
	f, _ := svc.Get(ctx, failure)
	if !f.Done || f.DoneBy != alerts.DoneBySystem {
		t.Fatalf("failure %+v", f)
	}
	if w, _ := svc.Get(ctx, waiting); w.Done {
		t.Fatal("waiting item of a live run must stay open")
	}
	if o, _ := svc.Get(ctx, otherFailure); o.Done {
		t.Fatal("item of another job must stay open")
	}

	// A waiting item of a run that ended closes.
	_, waitingEnded := archiveItems(t, svc, st, "job_c", "complete")
	if n, err := svc.CloseForJobs(ctx, []string{"job_c"}, "archived"); err != nil || n != 2 {
		t.Fatalf("closed %d, err %v", n, err)
	}
	if w, _ := svc.Get(ctx, waitingEnded); !w.Done {
		t.Fatal("waiting item of an ended run must close")
	}
}

func TestArchiveEventClosesJobAlerts(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	failure, _ := archiveItems(t, svc, st, "job_a", "running")
	other, _ := archiveItems(t, svc, st, "job_b", "running")
	frame := func(id, archived string) {
		raw, _ := json.Marshal(map[string]string{"archived": archived})
		svc.HandleEvent(ctx, store.StreamEvent{Topic: "job.updated", ID: id, Attrs: raw})
	}
	frame("job_a", "false")
	if f, _ := svc.Get(ctx, failure); f.Done {
		t.Fatal("an unarchive frame closes nothing")
	}
	frame("job_a", "true")
	if f, _ := svc.Get(ctx, failure); !f.Done {
		t.Fatal("an archive frame closes the item")
	}
	if o, _ := svc.Get(ctx, other); o.Done {
		t.Fatal("item of another job must stay open")
	}
}

// archiveItems raises a failure item and a waiting item for a job. The
// waiting item belongs to a run in the given status.
func archiveItems(t *testing.T, svc *alerts.Service, st *store.Store, jobID, runStatus string) (failure, waiting string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB().Exec(`INSERT OR REPLACE INTO runs (id,workspace_id,job_id,kind,reason,status,harness,trace_id,started_at,updated_at)
		VALUES ('run-'||?,'ws',?,'job','manual',?,'claude','t',datetime(),datetime())`, jobID, jobID, runStatus); err != nil {
		t.Fatal(err)
	}
	var err error
	failure, err = svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindJobStalled, Subject: "stalled/ws/" + jobID, WorkspaceID: "ws", JobID: jobID, Title: "stalled"})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err = svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, Subject: "run/run-" + jobID, RunID: "run-" + jobID, WorkspaceID: "ws", JobID: jobID, Title: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	return failure, waiting
}
