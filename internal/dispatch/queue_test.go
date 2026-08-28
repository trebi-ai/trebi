package dispatch_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestEnqueueClaimDone(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	q := dispatch.NewQueue(st)

	item := &dispatch.QueueItem{
		Integration: "whatsapp",
		WorkspaceID: "ws1",
		Job:         "receipts",
		OnIndex:     0,
		EventRaw:    `{"text":"hi"}`,
		Vars:        map[string]string{"from": "a"},
	}
	if err := q.Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	if item.ID == "" {
		t.Fatal("id")
	}

	claimed, err := q.ClaimNext(ctx, "ws1", "receipts", runs.KindJob, "")
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != item.ID {
		t.Fatalf("%+v", claimed)
	}
	if claimed.Status != dispatch.QueueClaimed {
		t.Fatalf("status=%s", claimed.Status)
	}

	// second claim empty
	c2, err := q.ClaimNext(ctx, "ws1", "receipts", runs.KindJob, "")
	if err != nil || c2 != nil {
		t.Fatalf("c2=%v err=%v", c2, err)
	}

	if err := q.MarkDone(ctx, item.ID, dispatch.ResultLaunched, "run1"); err != nil {
		t.Fatal(err)
	}
	list, err := q.List(ctx, "done", "", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
}

func TestClaimNextFIFOByULID(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ctx := context.Background()
	q := dispatch.NewQueue(st)

	// created_at strings mis-sort lexicographically: ".15Z" < ".1Z" ('5' < 'Z'),
	// but chronological order is .1 < .15 < .2. Ids are ULIDs in true order.
	createds := []string{
		"2026-08-09T10:00:00.1Z",
		"2026-08-09T10:00:00.15Z",
		"2026-08-09T10:00:00.2Z",
	}
	base := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	ids := make([]string, len(createds))
	for i, created := range createds {
		id := ulid.MustNew(ulid.Timestamp(base.Add(time.Duration(i)*time.Second)), nil).String()
		ids[i] = id
		item := &dispatch.QueueItem{ID: id, Integration: "x", WorkspaceID: "ws1", Job: "j", EventRaw: "{}"}
		if err := q.Enqueue(ctx, item); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx, `UPDATE event_queue SET created_at=? WHERE id=?`, created, id); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	for range ids {
		it, err := q.ClaimNext(ctx, "ws1", "j", runs.KindJob, "")
		if err != nil || it == nil {
			t.Fatalf("it=%v err=%v", it, err)
		}
		got = append(got, it.ID)
	}
	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Fatalf("claim order %v, want %v", got, ids)
	}
}

func TestClaimNextExactlyOnceUnderConcurrency(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ctx := context.Background()
	q := dispatch.NewQueue(st)

	item := &dispatch.QueueItem{Integration: "x", WorkspaceID: "ws1", Job: "j", EventRaw: "{}"}
	if err := q.Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			it, err := q.ClaimNext(ctx, "ws1", "j", runs.KindJob, "")
			if err != nil {
				t.Error(err)
				return
			}
			if it != nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := wins.Load(); n != 1 {
		t.Fatalf("wins=%d, want 1", n)
	}
	var attempts int
	if err := st.DB().QueryRowContext(ctx, `SELECT attempts FROM event_queue WHERE id=?`, item.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d, want 1", attempts)
	}
}

func TestRequeueAndCancel(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ctx := context.Background()
	q := dispatch.NewQueue(st)

	enqueue := func() *dispatch.QueueItem {
		item := &dispatch.QueueItem{Integration: "x", WorkspaceID: "ws1", Job: "j", EventRaw: "{}"}
		if err := q.Enqueue(ctx, item); err != nil {
			t.Fatal(err)
		}
		return item
	}
	rowState := func(id string) (status, result, blockReason string, doneAt any) {
		t.Helper()
		err := st.DB().QueryRowContext(ctx, `SELECT status, COALESCE(result,''), COALESCE(block_reason,''), done_at
			FROM event_queue WHERE id=?`, id).Scan(&status, &result, &blockReason, &doneAt)
		if err != nil {
			t.Fatal(err)
		}
		return
	}

	// Cancel a pending row.
	a := enqueue()
	if err := q.Cancel(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	status, result, _, doneAt := rowState(a.ID)
	if status != "done" || result != "skipped" || doneAt == nil {
		t.Fatalf("status=%s result=%s done_at=%v", status, result, doneAt)
	}
	// Cancel again fails; Requeue of done fails.
	if err := q.Cancel(ctx, a.ID); err == nil {
		t.Fatal("cancel of done row must fail")
	}
	if err := q.Requeue(ctx, a.ID); err == nil {
		t.Fatal("requeue of done row must fail")
	}

	// Missing rows fail.
	if err := q.Requeue(ctx, "missing"); err == nil {
		t.Fatal("requeue of missing row must fail")
	}
	if err := q.Cancel(ctx, "missing"); err == nil {
		t.Fatal("cancel of missing row must fail")
	}
	if err := q.MarkDone(ctx, "missing", dispatch.ResultLaunched, ""); err == nil {
		t.Fatal("MarkDone of missing row must fail")
	}
	if err := q.MarkBlocked(ctx, "missing", dispatch.BlockPolicy); err == nil {
		t.Fatal("MarkBlocked of missing row must fail")
	}

	// Requeue a blocked row: clears block_reason, keeps attempts.
	b := enqueue()
	if it, err := q.ClaimNext(ctx, "ws1", "j", runs.KindJob, ""); err != nil || it == nil || it.ID != b.ID {
		t.Fatalf("it=%v err=%v", it, err)
	}
	if err := q.MarkBlocked(ctx, b.ID, dispatch.BlockAuthMissing); err != nil {
		t.Fatal(err)
	}
	if err := q.Requeue(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	status, _, blockReason, _ := rowState(b.ID)
	if status != "pending" || blockReason != "" {
		t.Fatalf("status=%s block_reason=%q", status, blockReason)
	}
	var attempts int
	if err := st.DB().QueryRowContext(ctx, `SELECT attempts FROM event_queue WHERE id=?`, b.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d, want 1", attempts)
	}

	// Requeue revives a dead row.
	if _, err := st.DB().ExecContext(ctx, `UPDATE event_queue SET status='dead' WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.Requeue(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if status, _, _, _ := rowState(b.ID); status != "pending" {
		t.Fatalf("status=%s, want pending", status)
	}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
