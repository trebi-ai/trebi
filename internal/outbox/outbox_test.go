package outbox_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/store"
)

// scripted fails each Deliver with the next error of errs, then succeeds.
type scripted struct {
	mu      sync.Mutex
	errs    []error
	calls   int
	changed []outbox.Status
}

func (s *scripted) Deliver(context.Context, outbox.Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func (s *scripted) Changed(_ context.Context, r outbox.Row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changed = append(s.changed, r.Status)
}

var policy = outbox.Policy{Base: 5 * time.Second, Max: 20 * time.Second, Attempts: 3} //nolint:gochecknoglobals // test constant

func TestPolicyBackoff(t *testing.T) {
	t.Parallel()
	for attempts, want := range map[int]time.Duration{0: 5 * time.Second, 1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second, 4: 20 * time.Second, 70: 20 * time.Second} {
		if got := policy.Backoff(attempts); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}

// TestEnqueueDedupe: a repeat DedupeKey returns the stored row and emits no
// second frame.
func TestEnqueueDedupe(t *testing.T) {
	t.Parallel()
	ob, st := newService(t, &scripted{})
	first := enqueue(t, ob, "k1")
	again := enqueue(t, ob, "k1")
	if again.ID != first.ID || first.MaxAttempts != policy.Attempts || first.Status != outbox.StatusPending {
		t.Fatalf("first %+v again %+v", first, again)
	}
	if n := topics(t, st, "outbox.queued"); n != 1 {
		t.Fatalf("queued frames = %d, want 1", n)
	}
	if other := enqueue(t, ob, ""); other.ID == first.ID {
		t.Fatal("an empty DedupeKey must not dedupe")
	}
}

// TestSendDueBackoffAndFail: each failure keeps the body and the error and
// waits Base<<(n-1); the last attempt fails the row.
func TestSendDueBackoffAndFail(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	snd := &scripted{errs: []error{boom, boom, boom}}
	ob, st := newService(t, snd)
	r := enqueue(t, ob, "")
	ctx := t.Context()
	t0 := time.Now()

	if err := ob.SendDue(ctx, t0); err != nil {
		t.Fatal(err)
	}
	got, _ := ob.Get(ctx, r.ID)
	if got.Status != outbox.StatusPending || got.Attempts != 1 || got.ErrorMessage != "boom" || got.Body != "hello" {
		t.Fatalf("after one failure %+v", got)
	}
	if want := t0.Add(5 * time.Second).Truncate(time.Millisecond); got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(want) {
		t.Fatalf("next = %v, want %v", got.NextAttemptAt, want)
	}
	// Not due yet: no attempt.
	if err := ob.SendDue(ctx, t0.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if snd.calls != 1 {
		t.Fatalf("calls = %d before the due time", snd.calls)
	}
	if err := ob.SendDue(ctx, t0.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	got, _ = ob.Get(ctx, r.ID)
	if want := t0.Add(15 * time.Second).Truncate(time.Millisecond); got.Attempts != 2 || !got.NextAttemptAt.Equal(want) {
		t.Fatalf("second failure %+v, want next %v", got, want)
	}
	if err := ob.SendDue(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = ob.Get(ctx, r.ID)
	if got.Status != outbox.StatusFailed || got.Attempts != 3 || got.NextAttemptAt != nil || got.Body != "hello" {
		t.Fatalf("failed row %+v", got)
	}
	if n := topics(t, st, "outbox.failed"); n != 1 {
		t.Fatalf("failed frames = %d, want 1", n)
	}
	want := []outbox.Status{outbox.StatusPending, outbox.StatusPending, outbox.StatusFailed}
	if len(snd.changed) != len(want) || snd.changed[2] != outbox.StatusFailed {
		t.Fatalf("watcher saw %v, want %v", snd.changed, want)
	}
}

// TestPermanentFailsAtOnce: a Permanent error and a Kind with no Sender
// both fail the row on the first attempt.
func TestPermanentFailsAtOnce(t *testing.T) {
	t.Parallel()
	ob, _ := newService(t, &scripted{errs: []error{outbox.Permanent{Err: errors.New("no channel")}}})
	ctx := t.Context()
	r := enqueue(t, ob, "")
	got, err := ob.SendNow(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != outbox.StatusFailed || got.Attempts != 1 || got.ErrorMessage != "no channel" {
		t.Fatalf("permanent %+v", got)
	}
	orphan, err := ob.Enqueue(ctx, outbox.Row{Kind: outbox.KindEmail, Purpose: outbox.PurposeSend, Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = ob.SendNow(ctx, orphan.ID); got.Status != outbox.StatusFailed {
		t.Fatalf("no sender %+v", got)
	}
}

// TestSendMarksSentAndDropsBody: a sent row drops its body and its error.
func TestSendMarksSentAndDropsBody(t *testing.T) {
	t.Parallel()
	ob, st := newService(t, &scripted{errs: []error{errors.New("once")}})
	ctx := t.Context()
	r := enqueue(t, ob, "")
	if err := ob.SendDue(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ob.SendDue(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ := ob.Get(ctx, r.ID)
	if got.Status != outbox.StatusSent || got.Body != "" || got.ErrorMessage != "" || got.SentAt == nil || got.Attempts != 2 {
		t.Fatalf("sent row %+v", got)
	}
	if n := topics(t, st, "outbox.sent"); n != 1 {
		t.Fatalf("sent frames = %d, want 1", n)
	}
	// SendNow on a sent row returns it unchanged.
	if again, err := ob.SendNow(ctx, r.ID); err != nil || again.Revision != got.Revision {
		t.Fatalf("SendNow on sent row = %+v, %v", again, err)
	}
}

// TestStaleClaimReleased: a claim older than 2m goes back to pending.
func TestStaleClaimReleased(t *testing.T) {
	t.Parallel()
	snd := &scripted{}
	ob, st := newService(t, snd)
	ctx := t.Context()
	r := enqueue(t, ob, "")
	now := time.Now()
	if _, err := st.DB().ExecContext(ctx, `UPDATE outbox SET status='sending', claimed_at=? WHERE id=?`,
		now.UTC().Format("2006-01-02T15:04:05.000Z07:00"), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := ob.SendDue(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if snd.calls != 0 {
		t.Fatal("a fresh claim was sent")
	}
	if err := ob.SendDue(ctx, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := ob.Get(ctx, r.ID); snd.calls != 1 || got.Status != outbox.StatusSent {
		t.Fatalf("stale claim: calls %d row %+v", snd.calls, got)
	}
}

// TestCanceledContextKeepsRow: a canceled context sends nothing and changes
// no row.
func TestCanceledContextKeepsRow(t *testing.T) {
	t.Parallel()
	ob, _ := newService(t, &scripted{})
	r := enqueue(t, ob, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ob.SendDue(ctx, time.Now()); err == nil {
		t.Fatal("SendDue on a canceled context must return the error")
	}
	if got, _ := ob.Get(context.Background(), r.ID); got.Status != outbox.StatusPending || got.Attempts != 0 {
		t.Fatalf("row changed: %+v", got)
	}
}

// TestRetryCancelGuards: retry needs failed or canceled; cancel needs
// pending or failed.
func TestRetryCancelGuards(t *testing.T) {
	t.Parallel()
	ob, st := newService(t, &scripted{errs: []error{outbox.Permanent{Err: errors.New("x")}}})
	ctx := t.Context()
	r := enqueue(t, ob, "")
	if _, err := ob.Retry(ctx, r.ID); !errors.Is(err, outbox.ErrConflict) {
		t.Fatalf("retry pending = %v, want conflict", err)
	}
	canceled, err := ob.Cancel(ctx, r.ID)
	if err != nil || canceled.Status != outbox.StatusCanceled {
		t.Fatalf("cancel = %+v, %v", canceled, err)
	}
	if n := topics(t, st, "outbox.canceled"); n != 1 {
		t.Fatalf("canceled frames = %d", n)
	}
	if _, err := ob.Cancel(ctx, r.ID); !errors.Is(err, outbox.ErrConflict) {
		t.Fatalf("cancel canceled = %v, want conflict", err)
	}
	retried, err := ob.Retry(ctx, r.ID)
	if err != nil || retried.Status != outbox.StatusPending || retried.Attempts != 0 || retried.ErrorMessage != "" {
		t.Fatalf("retry = %+v, %v", retried, err)
	}
	if _, err := ob.Retry(ctx, "missing"); !errors.Is(err, outbox.ErrNotFound) {
		t.Fatalf("retry missing = %v", err)
	}
	if _, err := ob.Cancel(ctx, "missing"); !errors.Is(err, outbox.ErrNotFound) {
		t.Fatalf("cancel missing = %v", err)
	}
}

// TestListFilterAndPage: filters narrow the rows and the cursor pages them.
func TestListFilterAndPage(t *testing.T) {
	t.Parallel()
	ob, _ := newService(t, &scripted{})
	ctx := t.Context()
	var ids []string
	for range 3 {
		ids = append(ids, enqueue(t, ob, "").ID)
	}
	if _, err := ob.Enqueue(ctx, outbox.Row{Kind: outbox.KindEmail, Purpose: outbox.PurposeAlert, WorkspaceID: "ws2", InboxID: "in1"}); err != nil {
		t.Fatal(err)
	}
	page, next, err := ob.List(ctx, outbox.Filter{Kind: outbox.KindChannel, Limit: 2})
	if err != nil || len(page) != 2 || next == "" || page[0].ID != ids[2] {
		t.Fatalf("page 1 = %d rows next %q err %v", len(page), next, err)
	}
	page, next, err = ob.List(ctx, outbox.Filter{Kind: outbox.KindChannel, Limit: 2, Cursor: next})
	if err != nil || len(page) != 1 || next != "" || page[0].ID != ids[0] {
		t.Fatalf("page 2 = %+v next %q err %v", page, next, err)
	}
	if rows, _, _ := ob.List(ctx, outbox.Filter{InboxID: "in1"}); len(rows) != 1 || rows[0].Purpose != outbox.PurposeAlert {
		t.Fatalf("inbox filter = %+v", rows)
	}
	if rows, _, _ := ob.List(ctx, outbox.Filter{RunIDs: []string{}}); len(rows) != 0 {
		t.Fatalf("empty run scope = %d rows", len(rows))
	}
}

// TestPrune drops sent and canceled rows after 30 days and keeps the rest.
func TestPrune(t *testing.T) {
	t.Parallel()
	ob, _ := newService(t, &scripted{})
	ctx := t.Context()
	sent := enqueue(t, ob, "")
	if _, err := ob.SendNow(ctx, sent.ID); err != nil {
		t.Fatal(err)
	}
	pending := enqueue(t, ob, "")
	if err := ob.Prune(ctx, time.Now().Add(31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := ob.Get(ctx, sent.ID); !errors.Is(err, outbox.ErrNotFound) {
		t.Fatalf("sent row kept: %v", err)
	}
	if _, err := ob.Get(ctx, pending.ID); err != nil {
		t.Fatalf("pending row pruned: %v", err)
	}
}

func newService(t *testing.T, snd *scripted) (*outbox.Service, *store.Store) {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ob := outbox.New(st)
	ob.Register(outbox.KindChannel, snd, policy)
	return ob, st
}

func enqueue(t *testing.T, ob *outbox.Service, dedupe string) outbox.Row {
	t.Helper()
	r, err := ob.Enqueue(t.Context(), outbox.Row{
		Kind: outbox.KindChannel, Purpose: outbox.PurposeReply, WorkspaceID: "ws1",
		Instance: "telegram", Room: "chat1", Body: "hello", DedupeKey: dedupe,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func topics(t *testing.T, st *store.Store, topic string) int {
	t.Helper()
	got, err := st.ListBusWith(t.Context(), store.ListBusOpts{Topics: []string{topic}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return len(got)
}
