package harness

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

var usageNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func newTestUsage(t *testing.T) *UsageStore {
	t.Helper()
	u := NewUsageStore(openStore(t))
	u.now = func() time.Time { return usageNow }
	return u
}

func limitExceeded(t *testing.T, u *UsageStore, h string) int {
	t.Helper()
	var n sql.NullInt64
	err := u.st.DB().QueryRow(`SELECT limit_exceeded FROM harness_state WHERE name=?`, h).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return int(n.Int64)
}

func TestUsageRecordReplaceExpire(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	u := newTestUsage(t)
	weekly := agentwire.LimitWindow{Key: agentwire.WindowWeekly, Minutes: 10080, UsedPercent: 40, ResetsAt: usageNow.Add(48 * time.Hour)}
	five := agentwire.LimitWindow{Key: agentwire.WindowFiveHour, Minutes: 300, UsedPercent: 20, ResetsAt: usageNow.Add(-time.Minute)}
	if err := u.Record(ctx, "codex", agentwire.Limits{Plan: "plus", Windows: []agentwire.LimitWindow{weekly, five}}, UsageSourceProbe, time.Time{}); err != nil {
		t.Fatal(err)
	}
	weekly.UsedPercent = 55
	if err := u.Record(ctx, "codex", agentwire.Limits{Plan: "plus", Windows: []agentwire.LimitWindow{weekly}}, UsageSourceRun, time.Time{}); err != nil {
		t.Fatal(err)
	}
	list, err := u.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Harness != "codex" || list[0].Plan != "plus" || len(list[0].Windows) != 2 {
		t.Fatalf("list=%+v", list)
	}
	got5, gotW := list[0].Windows[0], list[0].Windows[1]
	if got5.Key != agentwire.WindowFiveHour || got5.UsedPercent != 0 || got5.ResetsAt != "" || got5.Source != UsageSourceProbe {
		t.Fatalf("expired window=%+v", got5)
	}
	if gotW.UsedPercent != 55 || gotW.Source != UsageSourceRun || gotW.ResetsAt != "2026-10-10T12:00:00Z" || gotW.ObservedAt != "2026-10-08T12:00:00Z" {
		t.Fatalf("weekly=%+v", gotW)
	}
	if n := countTopic(t, u.st, "harness.usage_updated"); n != 2 {
		t.Fatalf("events=%d", n)
	}
}

func TestUsageRejectedFlag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	u := newTestUsage(t)
	full := agentwire.LimitWindow{Key: agentwire.WindowFiveHour, Minutes: 300, UsedPercent: 100}
	if err := u.Record(ctx, "codex", agentwire.Limits{Rejected: true, Windows: []agentwire.LimitWindow{full}}, UsageSourceRun, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if limitExceeded(t, u, "codex") != 1 {
		t.Fatal("rejected limit did not set the flag")
	}
	if err := u.Record(ctx, "codex", agentwire.Limits{Windows: []agentwire.LimitWindow{full}}, UsageSourceRun, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if limitExceeded(t, u, "codex") != 1 {
		t.Fatal("a full window cleared the flag")
	}
	full.UsedPercent = 10
	if err := u.Record(ctx, "codex", agentwire.Limits{Windows: []agentwire.LimitWindow{full}}, UsageSourceRun, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if limitExceeded(t, u, "codex") != 0 {
		t.Fatal("window below 100 did not clear the flag")
	}
}

type fakeLimits struct {
	l   agentwire.Limits
	err error
	n   int
}

func (f *fakeLimits) Limits(context.Context, agentwire.Harness, agentwire.ModelQuery) (agentwire.Limits, error) {
	f.n++
	return f.l, f.err
}

const rolloutFiveHour = `{"timestamp":"2026-10-08T11:00:00.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","plan_type":"plus","primary":{"used_percent":12.5,"window_minutes":300,"resets_at":1791471600},"secondary":{"used_percent":30,"window_minutes":10080,"resets_in_seconds":7200}}}}
`

const rolloutWeeklyOnly = `{"timestamp":"2026-10-08T11:30:00.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"plan_type":"free","primary":{"used_percent":70,"window_minutes":10080,"resets_at":1791471600},"secondary":null}}}
`

func writeRollout(t *testing.T, dir, body string) {
	t.Helper()
	p := filepath.Join(dir, "2026", "10", "08", "rollout-2026-10-08T11-00-00-x.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"type":"session_meta"}`+"\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUsagePollerProbeAndRollout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	u := newTestUsage(t)
	reader := &fakeLimits{l: agentwire.Limits{Windows: []agentwire.LimitWindow{{Key: agentwire.WindowFiveHour, Minutes: 300, UsedPercent: 5}}}}
	dir := t.TempDir()
	p := NewUsagePoller(u, reader, nil, time.Hour).WithSessionDirs(dir)

	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(ctx); !errors.Is(err, ErrRefreshTooSoon) {
		t.Fatalf("second refresh err=%v", err)
	}
	list, _ := u.List(ctx)
	if len(list) != 1 || list[0].Windows[0].Source != UsageSourceProbe {
		t.Fatalf("probe list=%+v", list)
	}

	// The rollout line (11:00) is older than the stored probe (12:00).
	reader.err, reader.l = errors.New("app-server down"), agentwire.Limits{}
	writeRollout(t, dir, rolloutFiveHour)
	if err := p.poll(ctx); err == nil {
		t.Fatal("want the probe error when the rollout is older")
	}

	u2 := newTestUsage(t)
	p2 := NewUsagePoller(u2, reader, nil, time.Hour).WithSessionDirs(dir)
	if err := p2.poll(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ = u2.List(ctx)
	if len(list) != 1 || list[0].Plan != "plus" || len(list[0].Windows) != 2 {
		t.Fatalf("rollout list=%+v", list)
	}
	w5, ww := list[0].Windows[0], list[0].Windows[1]
	if w5.Key != agentwire.WindowFiveHour || w5.UsedPercent != 12.5 || w5.Scope != "" || w5.Source != UsageSourceSessionFile || w5.ObservedAt != "2026-10-08T11:00:00Z" {
		t.Fatalf("five_hour=%+v", w5)
	}
	if ww.Key != agentwire.WindowWeekly || ww.ResetsAt != "2026-10-08T13:00:00Z" {
		t.Fatalf("weekly=%+v", ww)
	}

	dir3 := t.TempDir()
	writeRollout(t, dir3, rolloutWeeklyOnly)
	u3 := newTestUsage(t)
	if err := NewUsagePoller(u3, reader, nil, time.Hour).WithSessionDirs(dir3).poll(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ = u3.List(ctx)
	if len(list) != 1 || len(list[0].Windows) != 1 || list[0].Windows[0].Key != agentwire.WindowWeekly || list[0].Windows[0].UsedPercent != 70 {
		t.Fatalf("weekly only=%+v", list)
	}
}
