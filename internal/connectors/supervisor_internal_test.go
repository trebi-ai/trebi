package connectors

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
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

// newInternalService opens a store and a service under a temp home. The
// cleanup stops every supervisor before the store closes.
func newInternalService(t *testing.T) (*store.Store, *Service) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	svc := New(st, workspaces.New(st), nil)
	t.Cleanup(func() {
		svc.StopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := svc.Wait(ctx); err != nil {
			t.Errorf("supervisors did not end: %v", err)
		}
		_ = st.Close()
	})
	return st, svc
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func topicCount(t *testing.T, st *store.Store, topic string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic=?`, topic).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestStopEndsPipelineSupervisor proves Stop reaches a pipeline child that
// holds stdout open, so the supervisor goroutine ends.
func TestStopEndsPipelineSupervisor(t *testing.T) {
	_, svc := newInternalService(t)
	ctx := context.Background()
	d := linesEntry("pipe", manifest.Events{Command: "sleep 30 | cat", Format: "json"})
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "pipe"); err != nil {
		t.Fatal(err)
	}
	p := svc.procByName("pipe")
	waitFor(t, func() bool { return p.snapshot().status == statusListening }, "not listening")
	start := time.Now()
	if err := svc.Stop("pipe"); err != nil {
		t.Fatal(err)
	}
	if p.alive() {
		t.Fatal("the supervisor outlived Stop")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Stop took %v", d)
	}
}

// TestLineTooLongKillsGroup proves an over-long line kills the group, so the
// run ends and the restart policy applies.
func TestLineTooLongKillsGroup(t *testing.T) {
	_, svc := newInternalService(t)
	ctx := context.Background()
	command := `head -c 2000000 /dev/zero | tr '\0' a; echo; sleep 30`
	d := linesEntry("long", manifest.Events{Command: command, Format: "line", Restart: manifest.RestartNever})
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "long"); err != nil {
		t.Fatal(err)
	}
	p := svc.procByName("long")
	waitFor(t, func() bool { return !p.alive() }, "the supervisor did not end")
	snap := p.snapshot()
	if snap.status != statusCrashed || snap.parseErrors != 1 {
		t.Fatalf("status %q parse errors %d", snap.status, snap.parseErrors)
	}
}

// TestStartAfterPollFailureKeepsOneSupervisor proves a failed poll leaves
// the status at backoff, a second Start keeps the one supervisor, and
// Restart keeps the restart counter.
func TestStartAfterPollFailureKeepsOneSupervisor(t *testing.T) {
	_, svc := newInternalService(t)
	ctx := context.Background()
	var mu sync.Mutex
	sleeps := 0
	svc.sleep = func(ctx context.Context, _ time.Duration) bool {
		mu.Lock()
		sleeps++
		mu.Unlock()
		<-ctx.Done()
		return true
	}
	d := linesEntry("flaky", manifest.Events{Command: "exit 3", Format: "json", Poll: manifest.Duration(10 * time.Second)})
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "flaky"); err != nil {
		t.Fatal(err)
	}
	p := svc.procByName("flaky")
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return sleeps == 1
	}, "no poll sleep")
	if st := p.snapshot().status; st != statusBackoff {
		t.Fatalf("status %q, want backoff", st)
	}
	if err := svc.Start(ctx, "flaky"); err != nil {
		t.Fatal(err)
	}
	if svc.procByName("flaky") != p {
		t.Fatal("Start replaced a live supervisor")
	}
	if err := svc.Restart(ctx, "flaky"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Restart(ctx, "flaky"); err != nil {
		t.Fatal(err)
	}
	if p.alive() {
		t.Fatal("Restart left the old supervisor running")
	}
	if n := svc.procByName("flaky").snapshot().restarts; n != 2 {
		t.Fatalf("restarts %d, want 2", n)
	}
}

// TestHealthWindowRecoversAndBackoffResets proves the health window belongs
// to one run: crash runs do not recover, a run that lasts the window emits
// connector.recovered, and the next backoff starts again at 1s.
func TestHealthWindowRecoversAndBackoffResets(t *testing.T) {
	st, svc := newInternalService(t)
	ctx := context.Background()
	dir := t.TempDir()
	counter, stop := filepath.Join(dir, "n"), filepath.Join(dir, "stop")
	// Runs 1-4 fail at once. Run 5 lives until the stop file exists, then
	// fails. Later runs wait to be killed.
	command := `n=$(cat ` + counter + ` 2>/dev/null || echo 0); n=$((n+1)); echo $n > ` + counter + `
if [ $n -le 4 ]; then exit 1; fi
if [ $n -eq 5 ]; then while [ ! -f ` + stop + ` ]; do sleep 0.02; done; exit 1; fi
exec sleep 30`

	var mu sync.Mutex
	var delays []time.Duration
	release := make(chan struct{})
	svc.sleep = func(ctx context.Context, d time.Duration) bool {
		if d == unhealthyWindow {
			select {
			case <-ctx.Done():
				return true
			case <-release:
				return false
			}
		}
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return ctx.Err() != nil
	}
	backoffs := func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(delays)
	}

	d := linesEntry("loop", manifest.Events{Command: command, Format: "json", Restart: manifest.RestartOnFailure})
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "loop"); err != nil {
		t.Fatal(err)
	}
	p := svc.procByName("loop")
	waitFor(t, func() bool { return len(backoffs()) == 4 && p.snapshot().status == statusListening }, "run 5 did not start")
	if n := topicCount(t, st, "connector.unhealthy"); n != 1 {
		t.Fatalf("unhealthy events %d, want 1", n)
	}
	if n := topicCount(t, st, "connector.recovered"); n != 0 {
		t.Fatalf("a crash run recovered: %d", n)
	}

	close(release) // run 5 lasts the health window
	waitFor(t, func() bool { return topicCount(t, st, "connector.recovered") == 1 }, "no connector.recovered")
	if err := os.WriteFile(stop, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(backoffs()) == 5 }, "run 5 did not end")
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, time.Second}
	if got := backoffs(); !slices.Equal(got, want) {
		t.Fatalf("backoffs %v, want %v", got, want)
	}
}

// TestCatchupBeforeEachLiveStart proves a restart after backoff runs the
// catch-up command again.
func TestCatchupBeforeEachLiveStart(t *testing.T) {
	_, svc := newInternalService(t)
	ctx := context.Background()
	marks := filepath.Join(t.TempDir(), "catchup")
	svc.sleep = func(ctx context.Context, d time.Duration) bool {
		if d == unhealthyWindow {
			<-ctx.Done()
			return true
		}
		return ctx.Err() != nil
	}
	d := linesEntry("cu", manifest.Events{
		Command: "exit 1", Format: "json", ID: "id", Restart: manifest.RestartOnFailure,
		Catchup: &manifest.Catchup{Command: "echo x >> " + marks, Timeout: manifest.Duration(5 * time.Second)},
	})
	if err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.PutCursor(ctx, "cu", "ev1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "cu"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		b, _ := os.ReadFile(marks)
		return strings.Count(string(b), "x") >= 3
	}, "catch-up did not run before each live start")
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

	var mu sync.Mutex
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	ticks := 0
	svc.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	svc.sleep = func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		now = now.Add(d)
		ticks++
		n := ticks
		mu.Unlock()
		if n >= 2 {
			<-ctx.Done() // the test stops the supervisor
			return true
		}
		return false
	}
	tickCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return ticks
	}

	d := linesEntry("ticker", manifest.Events{Command: command, Format: "json", ID: "id", Poll: manifest.Duration(10 * time.Second)})
	if err := svc.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background(), "ticker"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && tickCount() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if tickCount() < 2 {
		t.Fatal("the poll loop did not reach two ticks")
	}
	// Stop ends the supervisor before the deferred store close.
	if err := svc.Stop("ticker"); err != nil {
		t.Fatal(err)
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

func TestParseEventTime(t *testing.T) {
	t.Parallel()
	cases := map[string]time.Time{
		"2026-09-27T10:00:00Z":     time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC),
		"2026-09-27T10:00:00.25Z":  time.Date(2026, 9, 27, 10, 0, 0, 250_000_000, time.UTC),
		"1727380000":               time.Unix(1727380000, 0).UTC(),
		"1727380000.123456":        time.Unix(1727380000, 123_456_000).UTC(),
		"1727380000.5":             time.Unix(1727380000, 500_000_000).UTC(),
		"1727380000123":            time.UnixMilli(1727380000123).UTC(),
		"1727380000123.5":          time.UnixMilli(1727380000123).Add(500 * time.Microsecond).UTC(),
		"1727380000.1234567891234": time.Unix(1727380000, 123_456_789).UTC(),
		"abc":                      {},
		"1727380000.x":             {},
		"1727380000.-5":            {},
		"":                         {},
	}
	for raw, want := range cases {
		if got := parseEventTime(raw); !got.Equal(want) {
			t.Errorf("parseEventTime(%q) = %v; want %v", raw, got, want)
		}
	}
}

// TestParseEntriesDataLine proves the strict-decode error names the
// connector and the line in the file.
func TestParseEntriesDataLine(t *testing.T) {
	t.Parallel()
	data := "feed:\n  from: inline\n\nmail:\n  from: inline\n  frm: x\n"
	_, err := parseEntries([]byte(data))
	if err == nil || !strings.Contains(err.Error(), `connector "mail": line 6: field frm not found`) {
		t.Fatalf("err = %v", err)
	}
	entries, err := parseEntries([]byte(""))
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty file: %v %v", entries, err)
	}
	entries, err = parseEntries([]byte("feed:\n  from: catalog\n  version: 1.2.0\n"))
	if err != nil || entries["feed"].Name != "feed" || entries["feed"].Version != "1.2.0" {
		t.Fatalf("valid file: %v %v", entries, err)
	}
}

// linesEntry is an inline entry with one lines events block.
func linesEntry(name string, ev manifest.Events) Entry {
	return Entry{Name: name, From: FromInline, Manifest: &manifest.Manifest{
		Schema: manifest.SchemaID, Name: name, Title: name, Description: "Test connector.", Version: "1.0.0", Events: &ev,
	}}
}
