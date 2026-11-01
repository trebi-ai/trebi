package runs_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/visor"
)

// purgeHost records the Purge calls; the other visor.Host methods are not
// used by the prune.
type purgeHost struct {
	visor.Host
	mu    sync.Mutex
	specs []agentvisor.PurgeSpec
}

func (h *purgeHost) Purge(_ context.Context, p agentvisor.PurgeSpec) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.specs = append(h.specs, p)
	return nil
}

type pruneSeed struct {
	id, job, status, session, visorID, conversation string
	age                                             time.Duration
	resumableMS                                     int64
}

func TestPruneHistory(t *testing.T) {
	svc, st := pruneFixture(t)
	host := &purgeHost{}
	svc.SetSessionHost(host)
	now := time.Now().UTC()
	h := time.Hour
	seedRuns(t, st, now, []pruneSeed{
		// Job a keeps 2 (the default). The running and parked runs are not counted.
		{id: "a1", job: "a", status: "complete", age: 10 * h, session: "s1", visorID: "v1"},
		{id: "a2", job: "a", status: "failed", age: 9 * h, session: "shared", visorID: "v2"},
		{id: "a3", job: "a", status: "running", age: 8 * h},
		{id: "a4", job: "a", status: "parked", age: 7 * h},
		{id: "a5", job: "a", status: "skipped", age: 6 * h, resumableMS: (10 * h).Milliseconds()},
		{id: "a6", job: "a", status: "timed_out", age: 5 * h, session: "shared"},
		{id: "a7", job: "a", status: "complete", age: 4 * h},
		// Job b sets keep_runs 0: it keeps all.
		{id: "b1", job: "b", status: "complete", age: 10 * h},
		{id: "b2", job: "b", status: "complete", age: 9 * h},
		{id: "b3", job: "b", status: "complete", age: 8 * h},
		// Agent c sets keep_runs 1. c1 is the last run of its conversation.
		{id: "c1", job: "c", status: "exited", age: 10 * h, session: "s3", visorID: "v3", conversation: "k1"},
		{id: "c2", job: "c", status: "complete", age: 9 * h, conversation: "k2"},
	})
	policy := runs.Retention{Default: 2, Jobs: map[string]int{"b": 0, "c": 1}}

	n, err := svc.PruneHistory(t.Context(), policy, now)
	if err != nil {
		t.Fatal(err)
	}
	// a5 is in its resumable window, so it waits; a1 and a2 go.
	if want := []string{"a1", "a2", "c1"}; n != len(want) || !slices.Equal(prunedIDs(t, st), want) {
		t.Fatalf("pruned %d %v, want %v", n, prunedIDs(t, st), want)
	}

	r, err := svc.Get(t.Context(), "a1")
	if err != nil || r.PrunedAt == nil || r.Tokens.Input != 42 || r.Status != runs.StatusComplete {
		t.Fatalf("pruned row: %+v %v", r, err)
	}
	for _, id := range []string{"a1", "a7"} {
		var events int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM run_events WHERE run_id=?`, id).Scan(&events); err != nil {
			t.Fatal(err)
		}
		gone := id == "a1"
		for _, p := range []string{filepath.Join(home.RunsLogDir(), id+".log"), filepath.Join(home.RunsLogDir(), id+"-env"), home.RunDir(id)} {
			if _, err := os.Stat(p); os.IsNotExist(err) != gone {
				t.Errorf("%s: exists %v, want gone %v", p, !os.IsNotExist(err), gone)
			}
		}
		if (events == 0) != gone {
			t.Errorf("%s: %d events, want gone %v", id, events, gone)
		}
	}

	// a2 shares its vendor session with a6, which stays; c1 is the last run
	// of its conversation. Both keep their vendor files.
	want := []agentvisor.PurgeSpec{
		{Session: "v1", Harness: "claude", VendorID: "s1"},
		{Session: "v3"},
		{Session: "v2"},
	}
	if !slices.Equal(host.specs, want) {
		t.Fatalf("purges %+v, want %+v", host.specs, want)
	}

	// A second pass does no work.
	if n, err := svc.PruneHistory(t.Context(), policy, now); err != nil || n != 0 {
		t.Fatalf("second pass: %d %v", n, err)
	}
	// After its window, a5 goes.
	if n, err := svc.PruneHistory(t.Context(), policy, now.Add(5*h)); err != nil || n != 1 {
		t.Fatalf("after the window: %d %v", n, err)
	}
	// keep_runs 0 everywhere does nothing.
	if n, err := svc.PruneHistory(t.Context(), runs.Retention{}, now.Add(5*h)); err != nil || n != 0 {
		t.Fatalf("keep all: %d %v", n, err)
	}
}

// TestPruneHistoryNoHost prunes only the runs with no session while no
// session host is set.
func TestPruneHistoryNoHost(t *testing.T) {
	svc, st := pruneFixture(t)
	now := time.Now().UTC()
	seedRuns(t, st, now, []pruneSeed{
		{id: "a1", job: "a", status: "complete", age: 3 * time.Hour, visorID: "v1"},
		{id: "a2", job: "a", status: "complete", age: 2 * time.Hour},
		{id: "a3", job: "a", status: "complete", age: time.Hour},
	})
	n, err := svc.PruneHistory(t.Context(), runs.Retention{Default: 1}, now)
	if err != nil || n != 1 || !slices.Equal(prunedIDs(t, st), []string{"a2"}) {
		t.Fatalf("pruned %d %v %v, want [a2]", n, prunedIDs(t, st), err)
	}
}

// TestPruneHistoryBatches prunes more runs than one batch in one pass.
func TestPruneHistoryBatches(t *testing.T) {
	svc, st := pruneFixture(t)
	now := time.Now().UTC()
	seeds := make([]pruneSeed, 0, 620)
	for i := range 620 {
		seeds = append(seeds, pruneSeed{id: fmt.Sprintf("r%04d", i), job: "a", status: "complete", age: time.Duration(620-i) * time.Minute})
	}
	seedRuns(t, st, now, seeds)
	n, err := svc.PruneHistory(t.Context(), runs.Retention{Default: 20}, now)
	if err != nil || n != 600 {
		t.Fatalf("pruned %d %v, want 600", n, err)
	}
	var left int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM run_events`).Scan(&left); err != nil || left != 20 {
		t.Fatalf("events left %d %v, want 20", left, err)
	}
}

func pruneFixture(t *testing.T) (*runs.Service, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return runs.New(st, nil, nil), st
}

func seedRuns(t *testing.T, st *store.Store, now time.Time, seeds []pruneSeed) {
	t.Helper()
	for _, s := range seeds {
		started := now.Add(-s.age).Format(time.RFC3339Nano)
		ended := now.Add(-s.age + time.Minute).Format(time.RFC3339Nano)
		if _, err := st.DB().Exec(`INSERT INTO runs (id, workspace_id, job_id, status, started_at, ended_at, trace_id, updated_at,
			session_id, harness, visor_session_id, conversation_key, resumable_ms, tokens_input)
			VALUES (?, 'ws', ?, ?, ?, ?, 'tr', ?, NULLIF(?,''), 'claude', NULLIF(?,''), NULLIF(?,''), ?, 42)`,
			s.id, s.job, s.status, started, ended, started, s.session, s.visorID, s.conversation, s.resumableMS); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`INSERT INTO run_events (run_id, at, kind, source) VALUES (?, ?, 'assistant', 'wire')`, s.id, started); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{filepath.Join(home.RunsLogDir(), s.id+".log"), filepath.Join(home.RunsLogDir(), s.id+"-env"), filepath.Join(home.RunDir(s.id), "events.json")} {
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func prunedIDs(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT id FROM runs WHERE pruned_at IS NOT NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // test read
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}
