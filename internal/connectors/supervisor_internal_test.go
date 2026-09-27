package connectors

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// TestBackoffDelayDoublesAndCaps pins the restart backoff: 1s, 2s, 4s, … up
// to a 60s cap.
func TestBackoffDelayDoublesAndCaps(t *testing.T) {
	t.Parallel()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second}
	for i, w := range want {
		if got := backoffDelay(i + 1); got != w {
			t.Fatalf("attempt %d: %v want %v", i+1, got, w)
		}
	}
}

// TestProcHealthWindow proves four non-zero exits inside five minutes emit
// connector.unhealthy once, and that an old exit leaves the window.
func TestProcHealthWindow(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	p := &proc{}
	for i := 0; i < unhealthyExits-1; i++ {
		if p.recordExit(t0.Add(time.Duration(i) * 30 * time.Second)) {
			t.Fatalf("exit %d crossed early", i+1)
		}
	}
	if !p.recordExit(t0.Add(90 * time.Second)) {
		t.Fatal("the fourth exit did not cross into unhealthy")
	}
	if p.recordExit(t0.Add(2 * time.Minute)) {
		t.Fatal("a fifth exit re-emitted connector.unhealthy")
	}
	if !p.clearUnhealthy() {
		t.Fatal("clearUnhealthy must report the outage")
	}
	if p.clearUnhealthy() {
		t.Fatal("clearUnhealthy reported the same outage twice")
	}

	// Three exits, then one six minutes later: the old ones leave the window.
	q := &proc{}
	for i := 0; i < unhealthyExits-1; i++ {
		q.recordExit(t0.Add(time.Duration(i) * time.Second))
	}
	if q.recordExit(t0.Add(6 * time.Minute)) {
		t.Fatal("pruned exits still counted")
	}
}

// TestPollTicksIngestEachBatch proves a poll connector runs its command once
// per tick and ingests a new batch every time. The tick clock is injected,
// so the test does not wait for the ten-second poll floor.
func TestPollTicksIngestEachBatch(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ws := workspaces.New(st)
	svc := New(st, ws, nil)

	// Every run appends one id to the file, so each tick carries a new row.
	counter := filepath.Join(dir, "n")
	command := `n=$(cat ` + counter + ` 2>/dev/null || echo 0); n=$((n+1)); echo $n > ` + counter + `; echo "{\"id\":\"ev$n\"}"`

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	ticks := 0
	svc.sleep = func(ctx context.Context, d time.Duration) bool {
		now = now.Add(d)
		ticks++
		if ticks >= 2 {
			_ = svc.Stop("ticker")
			return true
		}
		return false
	}

	d := Definition{Name: "ticker", Events: &Events{Command: command, Format: "json", ID: "id", Poll: Duration(10 * time.Second)}}
	if err := svc.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background(), "ticker"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ticks >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ticks < 2 {
		t.Fatal("the poll loop did not reach two ticks")
	}

	// Two ticks, two ids ingested, and the cursor at the second.
	var ids []string
	rows, err := st.DB().Query(`SELECT source_id FROM events ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 2 || ids[0] != "ev1" || ids[1] != "ev2" {
		t.Fatalf("ingested %v", ids)
	}
	if cur, err := svc.GetCursor(context.Background(), "ticker"); err != nil || cur.LastEventID != "ev2" {
		t.Fatalf("cursor = %+v err = %v", cur, err)
	}
}
