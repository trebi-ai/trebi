package requests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/launch"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tools"
)

// park ends the run the way runs.Terminate does when the gate reports an
// open request.
func (h *harness) park(t *testing.T, runID string) {
	t.Helper()
	open, err := h.svc.HasOpen(context.Background(), runID)
	if err != nil || !open {
		t.Fatalf("HasOpen %s: %v %v", runID, open, err)
	}
	h.rs.setStatus(runID, runs.StatusParked)
	h.svc.RunEnded(context.Background(), runID, runs.StatusParked)
}

func TestAnswerWhileRunLive(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{})
	h.approve(t, r.ID)
	if n := len(h.launch.Calls()); n != 0 {
		t.Fatalf("launch while live: %d", n)
	}
	h.park(t, "RUN1")
	h.svc.RunEnded(context.Background(), "RUN1", runs.StatusParked)
	calls := h.launch.Calls()
	if len(calls) != 1 || calls[0].path != "resume" || calls[0].reason != runs.ReasonRequest {
		t.Fatalf("calls %+v", calls)
	}
	if got := h.get(t, r.ID); got.DeliveryState != DeliveryDelivered || got.DeliveredRunID == "" || got.DeliveryPath != "resume" {
		t.Fatalf("row %+v", got)
	}
	if open, _ := h.svc.HasOpen(context.Background(), "RUN1"); open {
		t.Fatal("delivered request still open")
	}
}

func TestDeliverWaitsForAllOpen(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	a := h.create(t, actor, CreateOpts{})
	b := h.create(t, actor, CreateOpts{Type: TypeConfirm, Title: "Second"})
	h.park(t, "RUN1")
	h.approve(t, a.ID)
	if n := len(h.launch.Calls()); n != 0 {
		t.Fatalf("launch with one open: %d", n)
	}
	h.approve(t, b.ID)
	calls := h.launch.Calls()
	if len(calls) != 1 || !strings.Contains(calls[0].prompt, "answered 2 requests") ||
		strings.Index(calls[0].prompt, a.ID) > strings.Index(calls[0].prompt, b.ID) {
		t.Fatalf("calls %+v", calls)
	}
	if h.topics(t, "request.delivered") != 2 {
		t.Fatal("want two delivered events")
	}
}

func TestDeliverClaimRace(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{})
	h.approve(t, r.ID)
	h.rs.setStatus("RUN1", runs.StatusParked)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := h.svc.deliver(context.Background(), "RUN1"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := len(h.launch.Calls()); n != 1 {
		t.Fatalf("launches %d, want 1", n)
	}
}

func TestDeliverAfterHour(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{})
	h.park(t, "RUN1")
	h.clock.add(time.Hour)
	h.approve(t, r.ID)
	calls := h.launch.Calls()
	if len(calls) != 1 || calls[0].prevID != "RUN1" || !strings.Contains(calls[0].prompt, "2026-09-28T11:00:00Z") {
		t.Fatalf("calls %+v", calls)
	}
}

func TestDeliverAfterRestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{})
	h.park(t, "RUN1")
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	h.open(t)
	if got := h.get(t, r.ID); got.SessionID != "sess-RUN1" {
		t.Fatalf("session %q", got.SessionID)
	}
	h.approve(t, r.ID)
	if calls := h.launch.Calls(); len(calls) != 1 || calls[0].prevID != "RUN1" {
		t.Fatalf("calls %+v", calls)
	}
}

func TestRecoverStaleClaim(t *testing.T) {
	t.Parallel()
	for _, turn := range []int{0, 2} {
		h := newHarness(t)
		r := h.create(t, h.run(t, "RUN1"), CreateOpts{})
		h.rs.setStatus("RUN1", runs.StatusParked)
		// A crash between the claim and the launch leaves the row claimed.
		if _, err := h.st.DB().Exec(`UPDATE requests SET status='answered', answer_json='{"decision":"approve","by":"user","via":"ui","at":"2026-09-28T10:00:00Z"}',
  delivery_state='claimed', claimed_at=? WHERE id=?`, fmtTime(h.clock.now()), r.ID); err != nil {
			t.Fatal(err)
		}
		h.rs.turn = turn
		ctx := context.Background()
		if err := h.svc.Recover(ctx); err != nil || len(h.launch.Calls()) != 0 {
			t.Fatalf("fresh claim: %v, calls %d", err, len(h.launch.Calls()))
		}
		h.clock.add(3 * time.Minute)
		if err := h.svc.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		got := h.get(t, r.ID)
		wantCalls := 1
		if turn > 0 {
			wantCalls = 0
			if got.DeliveredRunID != "RUN1" {
				t.Errorf("delivered run %q", got.DeliveredRunID)
			}
		}
		if got.DeliveryState != DeliveryDelivered || len(h.launch.Calls()) != wantCalls {
			t.Errorf("turn %d: state %s calls %d", turn, got.DeliveryState, len(h.launch.Calls()))
		}
	}
}

func TestDeliverFallbackFresh(t *testing.T) {
	t.Parallel()
	for _, err := range []error{runs.ErrResumeUnsupported, runs.ErrNoSession} {
		h := newHarness(t)
		h.launch.resumeErr = err
		r := h.create(t, h.run(t, "RUN1"), CreateOpts{Type: TypeReview, Title: "Send it", Payload: Payload{Draft: &Draft{Text: "draft one"}}})
		h.park(t, "RUN1")
		if _, err := h.svc.Answer(context.Background(), r.ID, Answer{Decision: DecisionEdit, Text: "draft two", By: ByUser, Via: ViaUI}, 0); err != nil {
			t.Fatal(err)
		}
		calls := h.launch.Calls()
		if len(calls) != 2 || calls[1].path != "fresh" || !strings.Contains(calls[1].prompt, "The earlier session is gone") ||
			!strings.Contains(calls[1].prompt, "draft one") || !strings.Contains(calls[1].prompt, "draft two") {
			t.Fatalf("%v: calls %+v", err, calls)
		}
		if got := h.get(t, r.ID); got.DeliveryPath != "fresh" || got.DeliveryState != DeliveryDelivered {
			t.Fatalf("row %+v", got)
		}
	}
}

func TestDeliverNoSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	run, _ := h.rs.Get(context.Background(), "RUN1")
	run.SessionID = ""
	h.rs.put(*run)
	r := h.create(t, actor, CreateOpts{})
	h.park(t, "RUN1")
	h.approve(t, r.ID)
	calls := h.launch.Calls()
	if len(calls) != 1 || calls[0].path != "fresh" {
		t.Fatalf("calls %+v", calls)
	}
	if got := h.get(t, r.ID); got.DeliveryPath != "fresh" || got.DeliveryState != DeliveryDelivered {
		t.Fatalf("row %+v", got)
	}
}

func TestDeliverFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.launch.resumeErr = launch.ErrJobMissing
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{})
	h.park(t, "RUN1")
	h.approve(t, r.ID)
	got := h.get(t, r.ID)
	if got.DeliveryState != DeliveryFailed || got.DeliveryError == "" || h.topics(t, "request.delivery_failed") != 1 {
		t.Fatalf("row %+v", got)
	}
	h.svc.RunEnded(context.Background(), "RUN1", runs.StatusParked)
	if err := h.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(h.launch.Calls()); n != 1 {
		t.Fatalf("retry: %d launches", n)
	}
	if open, _ := h.svc.HasOpen(context.Background(), "RUN1"); open {
		t.Fatal("failed delivery still open")
	}
}

func TestExpirySweep(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{Type: TypeConfirm, Title: "x", ExpiresIn: time.Hour})
	h.park(t, "RUN1")
	ctx := context.Background()
	if err := h.svc.Sweep(ctx); err != nil || h.get(t, r.ID).Status != StatusPending {
		t.Fatalf("early sweep: %v", err)
	}
	h.clock.add(2 * time.Hour)
	if err := h.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got := h.get(t, r.ID)
	calls := h.launch.Calls()
	if got.Status != StatusExpired || got.Answer == nil || got.Answer.Decision != DecisionExpired ||
		len(calls) != 1 || !strings.Contains(calls[0].prompt, "Decision: expired.") {
		t.Fatalf("row %+v calls %+v", got, calls)
	}
}

func TestSweepRemovesOldReports(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	run, _ := h.rs.Get(context.Background(), "RUN1")
	if err := os.WriteFile(filepath.Join(run.WorkingDir, "r.html"), []byte("<p>x</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.create(t, actor, CreateOpts{Type: TypeConfirm, Title: "x", Report: "r.html", Delivery: DeliveryPoll})
	h.approve(t, r.ID)
	h.clock.add(defaultRetain + time.Hour)
	if err := h.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Reports().Open(r.ID, "html"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("report kept: %v", err)
	}
	if got := h.get(t, r.ID); got.ReportKind != "" {
		t.Fatalf("report_kind %q", got.ReportKind)
	}
}

func TestDismissUnparks(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	a := h.create(t, actor, CreateOpts{})
	b := h.create(t, actor, CreateOpts{Type: TypeConfirm, Title: "Second"})
	h.park(t, "RUN1")
	ctx := context.Background()
	if _, err := h.svc.Dismiss(ctx, a.ID, ByUser); err != nil {
		t.Fatal(err)
	}
	if run, _ := h.rs.Get(ctx, "RUN1"); run.Status != runs.StatusParked {
		t.Fatalf("status %s with one open", run.Status)
	}
	if _, err := h.svc.Dismiss(ctx, b.ID, ByUser); err != nil {
		t.Fatal(err)
	}
	run, _ := h.rs.Get(ctx, "RUN1")
	if run.Status != runs.StatusComplete || run.EndReason != runs.EndRequestDismissed || len(h.launch.Calls()) != 0 {
		t.Fatalf("run %s/%s calls %d", run.Status, run.EndReason, len(h.launch.Calls()))
	}
}

func TestPlanLapseStillDelivers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{})
	h.park(t, "RUN1")
	h.grant.ok.Store(false)
	h.approve(t, r.ID)
	if n := len(h.launch.Calls()); n != 1 {
		t.Fatalf("launches %d", n)
	}
}

func TestChainedRequests(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	q1 := h.create(t, h.run(t, "RUN1"), CreateOpts{})
	h.park(t, "RUN1")
	h.approve(t, q1.ID)
	r2 := h.launch.Calls()[0]
	next := h.get(t, q1.ID).DeliveredRunID
	if r2.prevID != "RUN1" || next == "" {
		t.Fatalf("first resume %+v", r2)
	}
	q2 := h.create(t, tools.Actor{RunID: next, Kind: "job", Job: "mail", WorkspaceID: "ws1"}, CreateOpts{Type: TypeConfirm, Title: "Second step"})
	if q2.SessionID != "sess-RUN1" {
		t.Fatalf("chained session %q", q2.SessionID)
	}
	h.park(t, next)
	h.approve(t, q2.ID)
	calls := h.launch.Calls()
	if len(calls) != 2 || calls[1].prevID != next || !strings.Contains(calls[1].prompt, q2.ID) || strings.Contains(calls[1].prompt, q1.ID) {
		t.Fatalf("calls %+v", calls)
	}
}
