package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
)

func TestMigrateFreshSchema(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != store.SchemaVersion {
		t.Fatalf("schema version: got %d want %d", v, store.SchemaVersion)
	}
	tables := []string{
		"workspaces", "runs", "run_segments", "run_events", "event_log",
		"event_queue", "events", "event_outcomes", "schedules", "dispatch_policy",
		"connector_cursors", "connector_metrics", "inbox_items", "approvals",
		"skill_index", "harness_state", "job_index", "conversations", "system_flags",
	}
	for _, tbl := range tables {
		var name string
		err := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", tbl, err)
		}
	}
}

func TestBusAppendAndList(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	ctx := context.Background()

	seq, err := s.Append(ctx, store.BusEvent{
		Topic:    "run.created",
		Entity:   "run",
		EntityID: "01TEST",
		Action:   "created",
		Revision: 1,
		Attrs:    map[string]string{"job": "demo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if seq < 1 {
		t.Fatalf("seq=%d", seq)
	}
	if _, err := s.Append(ctx, store.BusEvent{Topic: "not.registered"}); err == nil {
		t.Fatal("expected error for unregistered topic")
	}

	events, err := s.ListBus(ctx, 0, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("len=%d", len(events))
	}
	if events[0].Topic != "run.created" || events[0].ID != "01TEST" {
		t.Fatalf("%+v", events[0])
	}

	events, err = s.ListBus(ctx, 0, []string{"job"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("filter should exclude run.*: %d", len(events))
	}
	events, err = s.ListBus(ctx, 0, []string{"run"}, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("run filter: %v len=%d", err, len(events))
	}
}

func TestEntityWriteAndBusOneTx(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	seq, err := s.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO workspaces (id, name, path, created_at, updated_at) VALUES (?,?,?,?,?)`,
			"ws_test", "test", "/tmp/ws", now, now)
		return err
	}, store.BusEvent{
		Topic:    "workspace.created",
		Entity:   "workspace",
		EntityID: "ws_test",
		Action:   "created",
		Revision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seq < 1 {
		t.Fatalf("seq=%d", seq)
	}

	var name string
	if err := s.DB().QueryRow(`SELECT name FROM workspaces WHERE id=?`, "ws_test").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "test" {
		t.Fatalf("name=%s", name)
	}
	events, err := s.ListBus(ctx, 0, []string{"workspace"}, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("bus: %v len=%d", err, len(events))
	}
}

func TestAppendEntityWriteFailureLeavesNoBusRow(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	ctx := context.Background()

	_, err := s.AppendEntity(ctx, func(tx *sql.Tx) error {
		return errors.New("write failed")
	}, store.BusEvent{
		Topic: "workspace.created", Entity: "workspace", EntityID: "ws_fail", Action: "created",
	})
	if err == nil {
		t.Fatal("expected write failure")
	}
	events, err := s.ListBus(ctx, 0, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("bus row must not commit when write fails: len=%d", len(events))
	}
}

func TestOutboxCrashImpossibleBetweenWriteAndAppend(t *testing.T) {
	t.Parallel()
	// If write succeeds but append fails, whole tx rolls back — entity absent.
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := s.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO workspaces (id, name, path, created_at, updated_at) VALUES (?,?,?,?,?)`,
			"ws_fail", "fail", "/tmp/fail", now, now)
		if err != nil {
			return err
		}
		// Force append failure via invalid topic after write.
		return nil
	}, store.BusEvent{Topic: "not.a.bus.topic"})
	if err == nil {
		t.Fatal("expected append failure")
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM workspaces WHERE id=?`, "ws_fail").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("workspace row must roll back when bus append fails")
	}
}

func TestGaplessSeqConcurrent(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	ctx := context.Background()
	const n = 50
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := s.Append(ctx, store.BusEvent{
				Topic:    "session.state",
				Entity:   "run",
				EntityID: "r1",
				Action:   "updated",
				Revision: 1,
			})
			done <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.ListBus(ctx, 0, nil, n+10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != n {
		t.Fatalf("got %d events want %d", len(events), n)
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq gap: index %d seq %d", i, ev.Seq)
		}
	}
	if !store.IsBusTopic("session.state") {
		t.Fatal("session.state must be bus topic")
	}
}

func TestListBusWithSinceAndDesc(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.Append(ctx, store.BusEvent{
			Topic: "alert.raised", Entity: "alert", EntityID: "a", Action: "created",
		}); err != nil {
			t.Fatal(err)
		}
	}
	asc, err := s.ListBus(ctx, 0, []string{"alert.raised"}, 20)
	if err != nil || len(asc) != 5 {
		t.Fatalf("asc %d %v", len(asc), err)
	}
	desc, err := s.ListBusWith(ctx, store.ListBusOpts{
		Topics: []string{"alert.raised"}, Limit: 2, Desc: true,
	})
	if err != nil || len(desc) != 2 {
		t.Fatalf("desc %d %v", len(desc), err)
	}
	if desc[0].Seq != asc[4].Seq || desc[1].Seq != asc[3].Seq {
		t.Fatalf("desc seq %d,%d want %d,%d", desc[0].Seq, desc[1].Seq, asc[4].Seq, asc[3].Seq)
	}
	got, err := s.ListBusWith(ctx, store.ListBusOpts{
		Topics: []string{"alert.raised"}, Limit: 2, Desc: true, Since: asc[2].TS,
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("since %d %v", len(got), err)
	}
	if got[0].Seq != asc[4].Seq || got[1].Seq != asc[3].Seq {
		t.Fatalf("since truncated: %d,%d", got[0].Seq, got[1].Seq)
	}
}

func TestPruneBus(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.Append(ctx, store.BusEvent{Topic: "config.updated", Attrs: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneBus(ctx, 365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("pruned %d", n)
	}
	n, err = s.PruneBus(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d want 1", n)
	}
}

func TestSystemFlags(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	if err := s.SetFlag("drain", "1"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.GetFlag("drain")
	if err != nil || !ok || v != "1" {
		t.Fatalf("get: %v ok=%v v=%s", err, ok, v)
	}
	if err := s.DeleteFlag("drain"); err != nil {
		t.Fatal(err)
	}
	_, ok, err = s.GetFlag("drain")
	if err != nil || ok {
		t.Fatal("flag should be gone")
	}
}

func openTest(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestIdempotentKeys(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	if _, found, err := st.Idempotent(ctx, "runs", "k1"); err != nil || found {
		t.Fatalf("unknown key: found=%v err=%v", found, err)
	}
	if got, err := st.RememberIdempotent(ctx, "runs", "k1", "run-1"); err != nil || got != "run-1" {
		t.Fatalf("remember: %q %v", got, err)
	}
	if got, err := st.RememberIdempotent(ctx, "runs", "k1", "run-2"); err != nil || got != "run-1" {
		t.Fatalf("first writer must win: %q %v", got, err)
	}
	if got, err := st.RememberIdempotent(ctx, "jobs", "k1", "job-1"); err != nil || got != "job-1" {
		t.Fatalf("scopes must not collide: %q %v", got, err)
	}
	if n, err := st.PruneIdempotent(ctx, -time.Minute); err != nil || n != 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
}
