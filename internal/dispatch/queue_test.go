package dispatch_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
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
		JobID:       "receipts", JobName: "receipts",
		RouteID:  "rt_0123456789",
		EventRaw: `{"text":"hi"}`,
		Vars:     map[string]string{"from": "a"},
	}
	if err := q.Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	if item.ID == "" {
		t.Fatal("id")
	}

	claimed, err := q.ClaimNext(ctx, "receipts", dispatch.KindJob, "")
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
	c2, err := q.ClaimNext(ctx, "receipts", dispatch.KindJob, "")
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
		item := &dispatch.QueueItem{ID: id, Connector: "x", WorkspaceID: "ws1", JobID: "j", JobName: "j", EventRaw: "{}"}
		if err := q.Enqueue(ctx, item); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx, `UPDATE event_queue SET created_at=? WHERE id=?`, created, id); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	for range ids {
		it, err := q.ClaimNext(ctx, "j", dispatch.KindJob, "")
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

	item := &dispatch.QueueItem{Connector: "x", WorkspaceID: "ws1", JobID: "j", JobName: "j", EventRaw: "{}"}
	if err := q.Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			it, err := q.ClaimNext(ctx, "j", dispatch.KindJob, "")
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
		item := &dispatch.QueueItem{Connector: "x", WorkspaceID: "ws1", JobID: "j", JobName: "j", EventRaw: "{}"}
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
	if it, err := q.ClaimNext(ctx, "j", dispatch.KindJob, ""); err != nil || it == nil || it.ID != b.ID {
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
	item := &dispatch.QueueItem{Connector: "x", WorkspaceID: "ws1", JobID: "j", JobName: "j", EventRaw: "{}"}
	if err := q.Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	until := time.Date(2030, 1, 1, 0, 0, 5, 200_000_000, time.UTC)
	for i := 0; i < 8; i++ {
		if _, err := st.DB().ExecContext(ctx, `UPDATE event_queue SET next_attempt_at=NULL WHERE id=?`, item.ID); err != nil {
			t.Fatal(err)
		}
		if it, err := q.ClaimNext(ctx, "j", dispatch.KindJob, ""); err != nil || it == nil {
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
	item := &dispatch.QueueItem{Connector: "x", WorkspaceID: "ws1", JobID: "j", JobName: "j", EventRaw: "{}"}
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

// TestLaneClaimsHead proves FIFO in every lane: a head row in backoff holds
// the rows behind it, with or without a conversation key.
func TestLaneClaimsHead(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ctx := context.Background()
	q := dispatch.NewQueue(st)
	enqueue := func(conv string) *dispatch.QueueItem {
		it := &dispatch.QueueItem{Connector: "x", WorkspaceID: "ws1", JobID: "j", JobName: "j", ConversationKey: conv, EventRaw: "{}"}
		if err := q.Enqueue(ctx, it); err != nil {
			t.Fatal(err)
		}
		return it
	}
	for _, conv := range []string{"chat", ""} {
		head, _ := enqueue(conv), enqueue(conv)
		if it, err := q.ClaimNext(ctx, "j", dispatch.KindJob, conv); err != nil || it == nil || it.ID != head.ID {
			t.Fatalf("conv=%q first claim=%v err=%v", conv, it, err)
		}
		if err := q.Defer(ctx, head.ID, time.Now().Add(time.Hour), dispatch.DeferRetry); err != nil {
			t.Fatal(err)
		}
		it, err := q.ClaimNext(ctx, "j", dispatch.KindJob, conv)
		if err != nil {
			t.Fatal(err)
		}
		if it != nil {
			t.Fatalf("conv=%q claimed %s behind a head row in backoff", conv, it.ID)
		}
	}
}

// TestEnqueueLaneCap proves the cap: the row after LaneCap pending rows
// fails with ErrLaneFull and inserts nothing, a resume row ignores the cap,
// and another lane is not affected.
func TestEnqueueLaneCap(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ctx := context.Background()
	q := dispatch.NewQueue(st)
	item := func(conv string) *dispatch.QueueItem {
		return &dispatch.QueueItem{Connector: "x", WorkspaceID: "ws1", JobID: "j", JobName: "j", ConversationKey: conv, EventRaw: "{}"}
	}
	for i := 0; i < dispatch.LaneCap; i++ {
		if err := q.Enqueue(ctx, item("")); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Enqueue(ctx, item("")); !errors.Is(err, dispatch.ErrLaneFull) {
		t.Fatalf("err=%v, want ErrLaneFull", err)
	}
	resume := item("")
	resume.ResumeOf = "run-1"
	if err := q.Enqueue(ctx, resume); err != nil {
		t.Fatalf("resume row: %v", err)
	}
	if err := q.Enqueue(ctx, item("other")); err != nil {
		t.Fatalf("other lane: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM event_queue WHERE conversation_key=''`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != dispatch.LaneCap+1 {
		t.Fatalf("rows=%d, want %d", n, dispatch.LaneCap+1)
	}
}

// TestLaunchRequestRoundTrip proves a queued launch keeps its request.
func TestLaunchRequestRoundTrip(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ctx := context.Background()
	q := dispatch.NewQueue(st)
	in := &dispatch.QueueItem{Connector: dispatch.ConnectorManual, WorkspaceID: "ws1", JobID: "j", JobName: "j", Launch: &dispatch.LaunchRequest{
		Reason: "manual", Prompt: "hi", Inputs: map[string]string{"a": "1"}, Wake: &dispatch.LaunchWake{Body: "b"},
	}}
	if err := q.Enqueue(ctx, in); err != nil {
		t.Fatal(err)
	}
	it, err := q.ClaimNext(ctx, "j", dispatch.KindJob, "")
	if err != nil || it == nil || it.Launch == nil {
		t.Fatalf("claim=%+v err=%v", it, err)
	}
	if it.Launch.Reason != "manual" || it.Launch.Prompt != "hi" || it.Launch.Inputs["a"] != "1" || it.Launch.Wake.Body != "b" {
		t.Fatalf("launch=%+v", it.Launch)
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

func TestBatchNextTick(t *testing.T) {
	t.Parallel()
	at := func(hm string) time.Time {
		v, err := time.Parse("15:04", hm)
		if err != nil {
			t.Fatal(err)
		}
		return time.Date(2026, 10, 12, v.Hour(), v.Minute(), 0, 0, time.UTC)
	}
	clock := dispatch.NewBatchClock(at("12:00"))
	for _, tc := range []struct{ t, want string }{
		{"12:03", "12:10"},
		{"12:10", "12:20"},
		{"11:55", "12:00"},
		{"12:00", "12:10"},
		{"13:59", "14:00"},
	} {
		if got := clock.NextTick(at(tc.t), 10*time.Minute); !got.Equal(at(tc.want)) {
			t.Errorf("NextTick(%s) = %s, want %s", tc.t, got.Format("15:04"), tc.want)
		}
	}
}

func (b *batchQueue) join(t *testing.T, routeID, eventID string) string {
	t.Helper()
	id, err := b.q.JoinBatch(context.Background(), &dispatch.QueueItem{
		Connector: "slack", WorkspaceID: "ws1", JobID: "j", JobName: "j", RouteID: routeID,
		ConversationKey: dispatch.BatchLaneKey(routeID), EventID: eventID, Vars: map[string]string{"text": eventID},
	}, 10*time.Minute, b.clock)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestJoinBatch proves the batch row: joins in one window share a row that
// is due at the tick, a claimed row takes no more events, two triggers keep
// two rows, and List carries the count.
func TestJoinBatch(t *testing.T) {
	t.Parallel()
	b := newBatchQueue(t)
	ctx := context.Background()
	b.now = b.now.Add(3 * time.Minute)
	first := b.join(t, "rt_a", "e1")
	b.now = b.now.Add(time.Minute)
	if b.join(t, "rt_a", "e2") != first || b.join(t, "rt_a", "e3") != first {
		t.Fatal("joins in one window must share one row")
	}
	if other := b.join(t, "rt_b", "e4"); other == first {
		t.Fatal("a second batch trigger must have its own row")
	}
	list, err := b.q.List(ctx, dispatch.ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var row dispatch.QueueItem
	for _, it := range list {
		if it.ID == first {
			row = it
		}
	}
	want := time.Date(2026, 10, 12, 12, 10, 0, 0, time.UTC)
	if row.BatchCount != 3 || row.NextAttemptAt == nil || !row.NextAttemptAt.Equal(want) || row.EventID != "" {
		t.Fatalf("row = %+v, want count 3 due %s", row, want)
	}
	events, err := b.q.BatchEvents(ctx, first)
	if err != nil || len(events) != 3 || events[0].EventID != "e1" || events[2].Vars["text"] != "e3" {
		t.Fatalf("events = %+v %v", events, err)
	}
	if it, err := b.q.ClaimNext(ctx, "j", dispatch.KindJob, dispatch.BatchLaneKey("rt_a")); err != nil || it != nil {
		t.Fatalf("claim before the tick = %+v %v", it, err)
	}
	b.now = want
	it, err := b.q.ClaimNext(ctx, "j", dispatch.KindJob, dispatch.BatchLaneKey("rt_a"))
	if err != nil || it == nil || it.ID != first {
		t.Fatalf("claim at the tick = %+v %v", it, err)
	}
	b.now = b.now.Add(time.Second)
	next := b.join(t, "rt_a", "e5")
	if next == first {
		t.Fatal("a join after the claim must open a new row")
	}
	if events, _ := b.q.BatchEvents(ctx, first); len(events) != 3 {
		t.Fatalf("claimed row events = %d, want 3", len(events))
	}
}

// TestJoinBatchMax proves that the join that fills a row makes it due now,
// and that the next join opens a new row.
func TestJoinBatchMax(t *testing.T) {
	t.Parallel()
	b := newBatchQueue(t)
	ctx := context.Background()
	b.now = b.now.Add(time.Minute)
	var first string
	for i := range dispatch.BatchMax {
		id := b.join(t, "rt_a", "e"+strconv.Itoa(i))
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("join %d opened a new row", i)
		}
	}
	it, err := b.q.ClaimNext(ctx, "j", dispatch.KindJob, dispatch.BatchLaneKey("rt_a"))
	if err != nil || it == nil || it.ID != first {
		t.Fatalf("a full row must be due now: %+v %v", it, err)
	}
	if b.join(t, "rt_a", "over") == first {
		t.Fatal("the join after a full row must open a new row")
	}
}

// TestJoinBatchLaneCap proves that the lane cap counts batch rows, not
// events.
func TestJoinBatchLaneCap(t *testing.T) {
	t.Parallel()
	b := newBatchQueue(t)
	ctx := context.Background()
	lane := dispatch.BatchLaneKey("rt_a")
	err := b.st.WithTx(ctx, func(tx *sql.Tx) error {
		for i := range dispatch.LaneCap {
			id := ulid.Make().String()
			if _, err := tx.Exec(`INSERT INTO event_queue (id, connector, workspace_id, job_id, kind, conversation_key, status, created_at, updated_at)
				VALUES (?, 'slack', 'ws1', 'j', 'job', ?, 'pending', ?, ?)`, id, lane, b.now.Format(time.RFC3339Nano), b.now.Format(time.RFC3339Nano)); err != nil {
				return err
			}
			for n := range dispatch.BatchMax {
				if _, err := tx.Exec(`INSERT INTO event_queue_batch (queue_id, event_id, added_at) VALUES (?,?,?)`,
					id, strconv.Itoa(i)+"-"+strconv.Itoa(n), b.now.Format(time.RFC3339Nano)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.q.JoinBatch(ctx, &dispatch.QueueItem{
		Connector: "slack", WorkspaceID: "ws1", JobID: "j", JobName: "j", ConversationKey: lane, EventID: "late",
	}, 10*time.Minute, b.clock)
	if !errors.Is(err, dispatch.ErrLaneFull) {
		t.Fatalf("err = %v, want ErrLaneFull", err)
	}
}

func TestBatchRender(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 12, 12, 3, 0, 0, time.Local).UTC().Format(time.RFC3339Nano)
	events := []dispatch.BatchEvent{
		{EventID: "e1", ReceivedAt: at, Sender: "Ana", RoomName: "general", Text: "hello\nthere", Vars: map[string]string{"connection": "slack-work"}},
		{EventID: "e2", ReceivedAt: at, Summary: `{"kind":"build"}`, Vars: map[string]string{"connection": "slack-work"}},
		{EventID: "e3", ReceivedAt: at, Sender: "Bo", Room: "C1", Text: strings.Repeat("x", 400), Vars: map[string]string{"connection": "slack-work"}},
	}
	b := dispatch.Batch{Events: events, File: "/runs/r1/events.json"}
	lines := strings.Split(b.Lines(), "\n")
	if len(lines) != 3 || lines[0] != "- 12:03 Ana in general: hello there" || lines[1] != `- 12:03 {"kind":"build"}` ||
		lines[2] != "- 12:03 Bo in C1: "+strings.Repeat("x", 300)+"…" {
		t.Fatalf("lines = %q", lines)
	}
	vars := b.Vars()
	if vars["event_count"] != "3" || vars["events_file"] != b.File || vars["connection"] != "slack-work" {
		t.Fatalf("vars = %v", vars)
	}
	if !strings.HasPrefix(b.Block(), "New events (3):\n- 12:03 Ana") || !strings.HasSuffix(b.Block(), "\nAll events: /runs/r1/events.json") {
		t.Fatalf("block = %q", b.Block())
	}
	many := dispatch.Batch{File: "/f"}
	for i := range 60 {
		many.Events = append(many.Events, dispatch.BatchEvent{EventID: strconv.Itoa(i), Text: "t"})
	}
	lines = strings.Split(many.Lines(), "\n")
	if len(lines) != 51 || lines[50] != "… and 10 more in /f" {
		t.Fatalf("60 events: %d lines, last %q", len(lines), lines[len(lines)-1])
	}
	data, err := b.JSON()
	if err != nil || !strings.Contains(string(data), `"event_id": "e1"`) {
		t.Fatalf("json = %s %v", data, err)
	}
}

// batchQueue is a queue on a fake clock and a batch clock that starts at
// the same time.
type batchQueue struct {
	st    *store.Store
	q     *dispatch.Queue
	now   time.Time
	clock dispatch.BatchClock
}

func newBatchQueue(t *testing.T) *batchQueue {
	t.Helper()
	b := &batchQueue{st: openTestStore(t), now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	b.q = dispatch.NewQueueWithClock(b.st, func() time.Time { return b.now })
	b.clock = dispatch.NewBatchClock(b.now)
	return b
}

func TestBatchLaneKey(t *testing.T) {
	t.Parallel()
	a, b := dispatch.BatchLaneKey("rt_aaaaaaaaaa"), dispatch.BatchLaneKey("rt_bbbbbbbbbb")
	if a == b {
		t.Fatal("two routes share a batch lane")
	}
	if !dispatch.IsBatchLane(a) || dispatch.IsBatchLane("conversation-key") {
		t.Fatalf("IsBatchLane: %q", a)
	}
}
