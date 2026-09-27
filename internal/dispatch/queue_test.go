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
		Connector:   "whatsapp",
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

	claimed, err := q.ClaimNext(ctx, "ws1", "receipts", dispatch.KindJob, "")
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != item.ID {
		t.Fatalf("%+v", claimed)
	}
	if claimed.Status != dispatch.QueueClaimed {
		t.Fatalf("status=%s", claimed.Status)
	}
	if claimed.Vars["from"] != "a" {
		t.Fatalf("vars=%v want from=a", claimed.Vars)
	}

	// second claim empty
	c2, err := q.ClaimNext(ctx, "ws1", "receipts", dispatch.KindJob, "")
	if err != nil || c2 != nil {
		t.Fatalf("c2=%v err=%v", c2, err)
	}

	if err := q.MarkDone(ctx, item.ID, dispatch.ResultLaunched, "run1"); err != nil {
		t.Fatal(err)
	}
	list, err := q.List(ctx, dispatch.ListOpts{Status: "done", Limit: 10})
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
		item := &dispatch.QueueItem{ID: id, Connector: "x", WorkspaceID: "ws1", Job: "j", EventRaw: "{}"}
		if err := q.Enqueue(ctx, item); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx, `UPDATE event_queue SET created_at=? WHERE id=?`, created, id); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	for range ids {
		it, err := q.ClaimNext(ctx, "ws1", "j", dispatch.KindJob, "")
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

	item := &dispatch.QueueItem{Connector: "x", WorkspaceID: "ws1", Job: "j", EventRaw: "{}"}
	if err := q.Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			it, err := q.ClaimNext(ctx, "ws1", "j", dispatch.KindJob, "")
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
	if attempts != 0 {
		t.Fatalf("attempts=%d, want 0: a claim is not an attempt", attempts)
	}
}

func TestRequeueAndCancel(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ctx := context.Background()
	q := dispatch.NewQueue(st)

	enqueue := func() *dispatch.QueueItem {
		item := &dispatch.QueueItem{Connector: "x", WorkspaceID: "ws1", Job: "j", EventRaw: "{}"}
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
	if it, err := q.ClaimNext(ctx, "ws1", "j", dispatch.KindJob, ""); err != nil || it == nil || it.ID != b.ID {
		t.Fatalf("it=%v err=%v", it, err)
	}
	if n, err := q.MarkAttempt(ctx, b.ID); err != nil || n != 1 {
		t.Fatalf("MarkAttempt=%d err=%v, want 1", n, err)
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

// TestWaitIsNotAnAttempt proves the wait path of decision 3: Defer keeps
// attempts, rounds the due time up, and MarkDone clears the wait reason.
// List shows attempts, next_attempt_at, and error_message.
func TestWaitIsNotAnAttempt(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ctx := context.Background()
	q := dispatch.NewQueue(st)
	item := &dispatch.QueueItem{Connector: "x", WorkspaceID: "ws1", Job: "j", EventRaw: "{}"}
	if err := q.Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	until := time.Date(2030, 1, 1, 0, 0, 5, 200_000_000, time.UTC)
	for i := 0; i < 8; i++ {
		if _, err := st.DB().ExecContext(ctx, `UPDATE event_queue SET next_attempt_at=NULL WHERE id=?`, item.ID); err != nil {
			t.Fatal(err)
		}
		if it, err := q.ClaimNext(ctx, "ws1", "j", dispatch.KindJob, ""); err != nil || it == nil {
			t.Fatalf("claim %d: it=%v err=%v", i, it, err)
		}
		if err := q.Defer(ctx, item.ID, until, dispatch.DeferPolicy); err != nil {
			t.Fatal(err)
		}
	}
	list, err := q.List(ctx, dispatch.ListOpts{})
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	got := list[0]
	if got.Attempts != 0 || got.ErrorMessage != "policy" || got.NextAttemptAt == nil {
		t.Fatalf("attempts=%d error=%q next=%v, want 0 policy set", got.Attempts, got.ErrorMessage, got.NextAttemptAt)
	}
	if want := until.Truncate(time.Second).Add(time.Second); !got.NextAttemptAt.Equal(want) {
		t.Fatalf("next_attempt_at=%s, want %s", got.NextAttemptAt, want)
	}
	if err := q.MarkDone(ctx, item.ID, dispatch.ResultLaunched, "run1"); err != nil {
		t.Fatal(err)
	}
	list, _ = q.List(ctx, dispatch.ListOpts{})
	if list[0].ErrorMessage != "" {
		t.Fatalf("error_message=%q after done, want empty", list[0].ErrorMessage)
	}
}

// TestMarkDeadKeepsMessage proves a final error ends the row as dead with
// its message.
func TestMarkDeadKeepsMessage(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ctx := context.Background()
	q := dispatch.NewQueue(st)
	item := &dispatch.QueueItem{Connector: "x", WorkspaceID: "ws1", Job: "j", EventRaw: "{}"}
	if err := q.Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkDead(ctx, item.ID, "actions invalid: ghost"); err != nil {
		t.Fatal(err)
	}
	var status, result, msg string
	if err := st.DB().QueryRowContext(ctx, `SELECT status, result, error_message FROM event_queue WHERE id=?`, item.ID).
		Scan(&status, &result, &msg); err != nil {
		t.Fatal(err)
	}
	if status != "done" || result != "dead" || msg != "actions invalid: ghost" {
		t.Fatalf("status=%s result=%s msg=%q", status, result, msg)
	}
	if err := q.MarkDead(ctx, "missing", "x"); err == nil {
		t.Fatal("MarkDead of missing row must fail")
	}
}

// TestConversationLaneClaimsHead proves FIFO inside a conversation lane: a
// head row in backoff holds the rows behind it. A lane without a key does
// not wait.
func TestConversationLaneClaimsHead(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ctx := context.Background()
	q := dispatch.NewQueue(st)
	enqueue := func(conv string) *dispatch.QueueItem {
		it := &dispatch.QueueItem{Connector: "x", WorkspaceID: "ws1", Job: "j", ConversationKey: conv, EventRaw: "{}"}
		if err := q.Enqueue(ctx, it); err != nil {
			t.Fatal(err)
		}
		return it
	}
	for _, conv := range []string{"chat", ""} {
		head, next := enqueue(conv), enqueue(conv)
		if it, err := q.ClaimNext(ctx, "ws1", "j", dispatch.KindJob, conv); err != nil || it == nil || it.ID != head.ID {
			t.Fatalf("conv=%q first claim=%v err=%v", conv, it, err)
		}
		if err := q.Defer(ctx, head.ID, time.Now().Add(time.Hour), dispatch.DeferRetry); err != nil {
			t.Fatal(err)
		}
		it, err := q.ClaimNext(ctx, "ws1", "j", dispatch.KindJob, conv)
		if err != nil {
			t.Fatal(err)
		}
		if conv != "" && it != nil {
			t.Fatalf("conv lane claimed %s behind a head row in backoff", it.ID)
		}
		if conv == "" && (it == nil || it.ID != next.ID) {
			t.Fatalf("unkeyed lane claim=%v, want %s", it, next.ID)
		}
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
