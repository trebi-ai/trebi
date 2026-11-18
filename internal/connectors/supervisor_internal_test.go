package connectors

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/connectors/protocol/fakeadapter"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/procgroup"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/waittest"
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
		ctx, cancel := context.WithTimeout(context.Background(), waittest.D(5*time.Second))
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
	waittest.Until(t, msg, cond)
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
	if _, err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "pipe"); err != nil {
		t.Fatal(err)
	}
	p := svc.procByName(svc.IDOf("pipe"))
	waitFor(t, func() bool { return p.snapshot().status == statusListening }, "not listening")
	start := time.Now()
	if err := svc.Stop("pipe"); err != nil {
		t.Fatal(err)
	}
	if p.alive() {
		t.Fatal("the supervisor outlived Stop")
	}
	if d := time.Since(start); d > waittest.D(3*time.Second) {
		t.Fatalf("Stop took %v", d)
	}
}

// TestLineTooLongKillsGroup proves an over-long line kills the group, so the
// run ends and the restart policy applies.
func TestLineTooLongKillsGroup(t *testing.T) {
	_, svc := newInternalService(t)
	ctx := context.Background()
	command := `head -c 5000000 /dev/zero | tr '\0' a; echo; sleep 30`
	d := linesEntry("long", manifest.Events{Command: command, Format: "line", Restart: manifest.RestartNever})
	if _, err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "long"); err != nil {
		t.Fatal(err)
	}
	p := svc.procByName(svc.IDOf("long"))
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
	if _, err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "flaky"); err != nil {
		t.Fatal(err)
	}
	p := svc.procByName(svc.IDOf("flaky"))
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
	if svc.procByName(svc.IDOf("flaky")) != p {
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
	if n := svc.procByName(svc.IDOf("flaky")).snapshot().restarts; n != 2 {
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
	if _, err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "loop"); err != nil {
		t.Fatal(err)
	}
	p := svc.procByName(svc.IDOf("loop"))
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
	if _, err := svc.Create(ctx, d); err != nil {
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
	if _, err := svc.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background(), "ticker"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return tickCount() >= 2 }, "the poll loop to reach two ticks")
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

// TestEventsTotalSurvivesRestart proves the lifetime count survives a
// supervisor restart and a new Service, duplicates do not count, and a
// delete drops the stats row.
func TestEventsTotalSurvivesRestart(t *testing.T) {
	st, svc := newInternalService(t)
	ctx := context.Background()
	d := linesEntry("life", manifest.Events{
		Command: `printf '{"id":"a"}\n{"id":"b"}\n{"id":"a"}\n'; sleep 30`, Format: "json", ID: "id",
	})
	if _, err := svc.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	counters := func(s *Service) map[string]any {
		row, err := s.Get("life")
		if err != nil {
			t.Fatal(err)
		}
		return row["counters"].(map[string]any)
	}
	if err := svc.Start(ctx, "life"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return counters(svc)["duplicates"] == int64(1) }, "the duplicate did not arrive")
	if c := counters(svc); c["events"] != int64(2) || c["events_total"] != int64(2) {
		t.Fatalf("first run counters %v", c)
	}
	row, _ := svc.Get("life")
	if row["first_event_at"] == "" {
		t.Fatal("first_event_at is empty")
	}

	// The second run sends the same ids: all are duplicates.
	if err := svc.Stop("life"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "life"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return counters(svc)["duplicates"] == int64(3) }, "the second run did not ingest")
	if c := counters(svc); c["events"] != int64(0) || c["events_total"] != int64(2) {
		t.Fatalf("after restart counters %v", c)
	}
	if err := svc.Stop("life"); err != nil {
		t.Fatal(err)
	}

	fresh := New(st, workspaces.New(st), nil)
	if err := fresh.Load(); err != nil {
		t.Fatal(err)
	}
	if c := counters(fresh); c["events_total"] != int64(2) {
		t.Fatalf("new service counters %v", c)
	}

	if err := svc.Delete(ctx, "life"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM connection_stats`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d stats rows after delete", n)
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

// TestFolderLoadStrict proves the strict decode of connection.yaml names
// the file and the field, and a record in the wrong folder does not load.
func TestFolderLoadStrict(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	id := NewConnectionID()
	f := NewFolder(id)
	if err := f.Create(); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.Dir(), recordFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("id: " + id + "\ntype: feed\nslug: feed\nfrom: catalog\nversion: 1.2.0\nfrm: x\n")
	if _, err := f.Load(); err == nil || !strings.Contains(err.Error(), "connection.yaml") || !strings.Contains(err.Error(), "field frm not found") {
		t.Fatalf("unknown field: %v", err)
	}
	write("id: " + NewConnectionID() + "\ntype: feed\nslug: feed\nfrom: catalog\nversion: 1.2.0\n")
	if _, err := f.Load(); err == nil || !strings.Contains(err.Error(), "does not match the folder") {
		t.Fatalf("wrong folder: %v", err)
	}
	write("id: " + id + "\ntype: feed\nslug: feed\nfrom: catalog\nversion: 1.2.0\n")
	if c, err := f.Load(); err != nil || c.Slug != "feed" || c.Version != "1.2.0" {
		t.Fatalf("valid record: %+v %v", c, err)
	}
}

// linesEntry is an inline connection with one lines events block. The slug
// and the type are name.
func linesEntry(name string, ev manifest.Events) Connection {
	return Connection{Type: name, Slug: name, From: FromInline, Manifest: &manifest.Manifest{
		Schema: manifest.SchemaID, Name: name, Title: name, Description: "Test connector.", Version: "1.0.0", Events: &ev,
	}}
}

// TestLocalURL pins the hosts that a connector link may point to.
func TestLocalURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:7480/", true},
		{"http://localhost:8080/feeds", true},
		{"https://LOCALHOST/", true},
		{"http://100.64.0.1/", true},
		{"http://100.127.255.254:9000/x", true},
		{"https://perception.tail1234.ts.net/", true},
		{"http://100.128.0.1/", false},
		{"http://127.0.0.2/", false},
		{"http://192.168.1.10/", false},
		{"https://example.com/", false},
		{"https://ts.net/", false},
		{"https://evil.ts.net.example.com/", false},
		{"ftp://127.0.0.1/", false},
		{"javascript:alert(1)", false},
		{"http://user:pw@127.0.0.1/", false},
		{"", false},
		{"://bad", false},
	}
	for _, c := range cases {
		if got := localURL(c.url); got != c.want {
			t.Errorf("localURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// TestStatusLinks proves that a status keeps the last links when it has none,
// that an empty list clears them, and that a foreign link is dropped.
func TestStatusLinks(t *testing.T) {
	t.Parallel()
	s := &Service{log: slog.New(slog.DiscardHandler)}
	p := &proc{}
	ui := protocol.Link{Label: "Open", URL: "http://127.0.0.1:7480/"}
	links := s.localLinks("c1", []protocol.Link{ui, {Label: "Web", URL: "https://example.com/"}, {URL: "http://localhost/"}})
	if !slices.Equal(links, []protocol.Link{ui}) {
		t.Fatalf("kept %v", links)
	}
	if _, moved := p.setAuth(protocol.Status{State: protocol.StateConnected, Links: links}); !moved {
		t.Fatal("first links did not report a change")
	}
	if _, moved := p.setAuth(protocol.Status{State: protocol.StateConnected}); moved {
		t.Fatal("a status with no links changed the links")
	}
	if got := p.authStatus().Links; !slices.Equal(got, []protocol.Link{ui}) {
		t.Fatalf("links after a status with none: %v", got)
	}
	if _, moved := p.setAuth(protocol.Status{State: protocol.StateConnected, Links: s.localLinks("c1", []protocol.Link{})}); !moved {
		t.Fatal("an empty list did not report a change")
	}
	if got := p.authStatus().Links; len(got) != 0 {
		t.Fatalf("links after an empty list: %v", got)
	}
}

// TestRSSCache pins the memory read: pid 0 and a failed read give 0, all
// pids share one process table scan for rssTTL, and then it scans again.
func TestRSSCache(t *testing.T) {
	t.Parallel()
	scans := 0
	c := rssCache{snap: func(context.Context) (rssTree, error) {
		scans++
		return fakeTree{}, nil
	}}
	if got := c.read(0); got != 0 || scans != 0 {
		t.Fatalf("pid 0: %d, %d scans", got, scans)
	}
	if got := c.read(7); got != 7000 {
		t.Fatalf("read = %d", got)
	}
	if got, other := c.read(7), c.read(8); got != 7000 || other != 8000 || scans != 1 {
		t.Fatalf("reads = %d %d, %d scans", got, other, scans)
	}
	if got := c.read(9); got != 0 {
		t.Fatalf("failed read = %d", got)
	}
	c.at = time.Now().Add(-rssTTL)
	if c.read(7); scans != 2 {
		t.Fatalf("an old snapshot did not scan again: %d scans", scans)
	}
}

// fakeTree gives pid*1000 bytes, and no process for pid 9.
type fakeTree struct{}

func (fakeTree) RSS(_ context.Context, pid int) (uint64, error) {
	if pid == 9 {
		return 0, os.ErrNotExist
	}
	return uint64(pid) * 1000, nil //nolint:gosec // a test pid is positive
}

// TestGoStderrLongLine proves a stderr line over MaxLine is cut and the
// lines after it still reach the log.
func TestGoStderrLongLine(t *testing.T) {
	_, svc := newInternalService(t)
	in := strings.Repeat("x", protocol.MaxLine+10) + "\nafter\r\nlast"
	svc.goStderr("c1", strings.NewReader(in))
	tail := func() []string {
		lr := NewFolder("c1").OpenLog()
		defer lr.Close()
		lines, _ := lr.Tail(10)
		return lines
	}
	waitFor(t, func() bool { return len(tail()) == 3 }, "three stderr rows")
	lines := tail()
	if !strings.HasSuffix(lines[0], ` [cut]"}`) || !strings.Contains(lines[1], `"line":"after"`) || !strings.Contains(lines[2], `"line":"last"`) {
		t.Fatalf("rows = %.80q", lines)
	}
}

// TestSweepGroups proves the boot sweep kills a group that an old daemon
// recorded and removes the record.
func TestSweepGroups(t *testing.T) {
	_, svc := newInternalService(t)
	ctx := context.Background()
	cmd, _, _, err := procgroup.Command{Script: "sleep 30"}.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }() //nolint:errcheck // a killed group fails
	m, err := procgroup.MarkOf(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	f := NewFolder("c1")
	if err := f.Create(); err != nil {
		t.Fatal(err)
	}
	if err := f.SetGroup(m); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	svc.conns["c1"] = Connection{ID: "c1"}
	svc.mu.Unlock()
	svc.sweepGroups(ctx)
	select {
	case <-done:
	case <-time.After(waittest.D(3 * time.Second)):
		t.Fatal("the old group still runs")
	}
	if _, ok := f.TakeGroup(); ok {
		t.Fatal("the record stays")
	}
}

// TestStatusOutage pins the status outage: a bad state that lasts ten
// minutes emits connector.unhealthy once, and connected after it emits
// connector.recovered once. A crash-path outage and a process exit never
// emit a recovered from this path.
func TestStatusOutage(t *testing.T) {
	st, svc := newInternalService(t)
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var now time.Time
	svc.now = func() time.Time { return now }
	type step struct {
		min   int
		state string // a status; "tick" is a ping tick; "exit" ends the process; "crash" is a crash-path outage
	}
	cases := []struct {
		name      string
		steps     []step
		unhealthy string // the status attr of the one connector.unhealthy, "" for none
		recovered int
	}{
		{"connecting 9 min", []step{{0, "connecting"}, {9, "tick"}, {9, "connected"}, {20, "tick"}}, "", 0},
		{"connecting 10 min", []step{{0, "connecting"}, {10, "tick"}, {11, "tick"}, {30, "tick"}}, "connecting", 0},
		{"connecting then error", []step{{0, "connecting"}, {5, "error"}, {10, "tick"}}, "error", 0},
		{"error then connected", []step{{0, "error"}, {10, "tick"}, {11, "connected"}, {12, "connected"}}, "error", 1},
		{"rate limited", []step{{0, "rate_limited"}, {10, "tick"}}, "rate_limited", 0},
		{"login is not an outage", []step{{0, "connecting"}, {2, "auth_required"}, {11, "tick"}}, "", 0},
		{"crash path", []step{{0, "crash"}, {3, "connected"}}, "crashed", 0},
		{"exit during outage", []step{{0, "error"}, {10, "tick"}, {11, "exit"}, {12, "connected"}, {30, "tick"}}, "error", 0},
	}
	for i, c := range cases {
		p := &proc{id: fmt.Sprintf("c%d", i)}
		for _, s := range c.steps {
			now = base.Add(time.Duration(s.min) * time.Minute)
			switch s.state {
			case "tick":
				svc.checkOutage(p)
			case "exit":
				p.resetOutage()
			case "crash":
				p.unhealthy = true
				svc.emitUnhealthy(p, protocol.Status{State: statusCrashed})
			default:
				svc.applyStatus(p, protocol.Status{State: s.state, Message: "m-" + s.state})
			}
		}
		var states []string
		rows, err := st.DB().Query(`SELECT json_extract(attrs_json, '$.status') FROM event_log WHERE topic='connector.unhealthy' AND entity_id=?`, p.id)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			states = append(states, s)
		}
		_ = rows.Close() // read only
		var want []string
		if c.unhealthy != "" {
			want = []string{c.unhealthy}
		}
		if !slices.Equal(states, want) {
			t.Errorf("%s: unhealthy %v, want %v", c.name, states, want)
		}
		var rec int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic='connector.recovered' AND entity_id=?`, p.id).Scan(&rec); err != nil {
			t.Fatal(err)
		}
		if rec != c.recovered {
			t.Errorf("%s: recovered %d, want %d", c.name, rec, c.recovered)
		}
	}
}

// TestStatusOutageSession drives a live adapter that reports error. After
// ten minutes on the clock, a ping tick emits one connector.unhealthy with
// the title and the adapter message.
func TestStatusOutageSession(t *testing.T) {
	st, svc := newInternalService(t)
	var mu sync.Mutex
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	svc.pingEvery = 20 * time.Millisecond
	msg := "another client opened this WhatsApp session; restart the connector"
	sb := &fakeadapter.Sandbox{Status: &protocol.Status{State: protocol.StateError, Message: msg}}
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := sb.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	c := linesEntry("wa", manifest.Events{Protocol: manifest.ProtocolConnector, Command: `"` + os.Args[0] + `"`, Types: []manifest.EventType{{Type: "message"}}})
	c.Title = "WhatsApp Ellis"
	c.Inputs = map[string]string{fakeadapter.Env: "sandbox:" + path}
	ctx := context.Background()
	if _, err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "wa"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return svc.AuthState("wa").State == protocol.StateError }, "no error status")
	time.Sleep(100 * time.Millisecond) // a few ticks before the limit
	if n := topicCount(t, st, "connector.unhealthy"); n != 0 {
		t.Fatalf("unhealthy before ten minutes: %d", n)
	}
	mu.Lock()
	now = now.Add(statusOutageAfter)
	mu.Unlock()
	waitFor(t, func() bool { return topicCount(t, st, "connector.unhealthy") == 1 }, "no connector.unhealthy")
	var status, message, title string
	if err := st.DB().QueryRow(`SELECT json_extract(attrs_json, '$.status'), json_extract(attrs_json, '$.message'), json_extract(attrs_json, '$.title')
		FROM event_log WHERE topic='connector.unhealthy'`).Scan(&status, &message, &title); err != nil {
		t.Fatal(err)
	}
	if status != protocol.StateError || message != msg || title != "WhatsApp Ellis" {
		t.Fatalf("attrs %q %q %q", status, message, title)
	}
	time.Sleep(100 * time.Millisecond)
	if n := topicCount(t, st, "connector.unhealthy"); n != 1 {
		t.Fatalf("more ticks emitted it again: %d", n)
	}
}
