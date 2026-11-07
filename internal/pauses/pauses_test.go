package pauses

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/store"
)

type endLog struct{ ended []Pause }

func (e *endLog) PauseEnded(_ context.Context, p Pause) { e.ended = append(e.ended, p) }

func TestCover(t *testing.T) {
	t.Parallel()
	in := func(d time.Duration) *string {
		s := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC).Add(d).Format(time.RFC3339)
		return &s
	}
	active := Active{
		{ID: "ws", Scope: ScopeWorkspace, Target: "ws_1", Until: in(time.Hour)},
		{ID: "h", Scope: ScopeHarness, Target: "claude", Until: in(3 * time.Hour)},
		{ID: "agent", Scope: ScopeAgent, Target: "agt_1"},
	}
	cases := []struct {
		name string
		sub  Subject
		want string
	}{
		{"workspace", Subject{WorkspaceID: "ws_1", Harness: "codex", Kind: dispatch.KindJob}, "ws"},
		{"harness ends later", Subject{WorkspaceID: "ws_1", Harness: "claude", Kind: dispatch.KindJob}, "h"},
		{"variant is its own target", Subject{WorkspaceID: "ws_2", Harness: "claude-work", Kind: dispatch.KindJob}, ""},
		{"no end wins", Subject{WorkspaceID: "ws_1", AgentID: "agt_1", Harness: "claude", Kind: dispatch.KindJob}, "agent"},
		{"assistant never held", Subject{WorkspaceID: "ws_1", Harness: "claude", Kind: dispatch.KindAssistant}, ""},
		{"other", Subject{WorkspaceID: "ws_2", Harness: "codex", Kind: dispatch.KindJob}, ""},
	}
	for _, c := range cases {
		got := active.Cover(c.sub)
		if (got == nil && c.want != "") || (got != nil && got.ID != c.want) {
			t.Errorf("%s: got %+v, want %q", c.name, got, c.want)
		}
	}
	all := append(Active{{ID: "all", Scope: ScopeAll}}, active...)
	if got := all.Cover(Subject{WorkspaceID: "ws_9", Kind: dispatch.KindJob}); got == nil || got.ID != "all" {
		t.Errorf("all: got %+v", got)
	}
}

func TestSetUpdateResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, now := newTestService(t)
	hook := &endLog{}
	s.SetEndHook(hook)

	if _, _, err := s.Set(ctx, SetOpts{Scope: ScopeWorkspace}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no target: %v", err)
	}
	if _, _, err := s.Set(ctx, SetOpts{Scope: ScopeAll, Until: at(*now, -time.Minute)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("past end: %v", err)
	}
	p, created, err := s.Set(ctx, SetOpts{Scope: ScopeHarness, Target: "claude", Until: at(*now, time.Hour), CreatedBy: "me"})
	if err != nil || !created || !p.Active || p.Label != "claude" || p.Missed != MissedSkip || p.Source != SourceUser {
		t.Fatalf("set: %+v created=%v err=%v", p, created, err)
	}
	again, created, err := s.Set(ctx, SetOpts{Scope: ScopeHarness, Target: "claude", Missed: MissedOnce})
	if err != nil || created || again.ID != p.ID || again.Until != nil || again.Revision != 2 {
		t.Fatalf("second set: %+v created=%v err=%v", again, created, err)
	}
	note := "tokens"
	upd, err := s.Update(ctx, p.ID, UpdateOpts{Until: at(*now, 2*time.Hour), Note: &note})
	if err != nil || upd.Note != "tokens" || upd.Until == nil || upd.Missed != MissedOnce {
		t.Fatalf("update: %+v err=%v", upd, err)
	}
	ended, err := s.Resume(ctx, p.ID)
	if err != nil || ended.Active || ended.EndedBy != EndedByUser || len(hook.ended) != 1 {
		t.Fatalf("resume: %+v err=%v hook=%d", ended, err, len(hook.ended))
	}
	if _, err := s.Update(ctx, p.ID, UpdateOpts{Note: &note}); !errors.Is(err, ErrEnded) {
		t.Fatalf("update ended: %v", err)
	}
	if list, _ := s.List(ctx, ListOpts{}); len(list) != 0 {
		t.Fatalf("active list: %+v", list)
	}
	if list, _ := s.List(ctx, ListOpts{All: true}); len(list) != 1 {
		t.Fatalf("all list: %+v", list)
	}
	// A new pause of the same scope is a new row.
	if p2, created, err := s.Set(ctx, SetOpts{Scope: ScopeHarness, Target: "claude"}); err != nil || !created || p2.ID == p.ID {
		t.Fatalf("new pause: %+v %v %v", p2, created, err)
	}
}

func TestExpireAndMisses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, now := newTestService(t)
	hook := &endLog{}
	s.SetEndHook(hook)
	p, _, err := s.Set(ctx, SetOpts{Scope: ScopeAll, Until: at(*now, time.Hour), Missed: MissedOnce})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Skipped(ctx, p, "ws_1", "jb_1", 0); err != nil {
			t.Fatal(err)
		}
	}
	if c, _ := s.Cover(ctx, "ws_1", "jb_1", dispatch.KindJob, "claude"); c == nil || c.ID != p.ID {
		t.Fatalf("cover: %+v", c)
	}
	if out, _ := s.ExpireDue(ctx); len(out) != 0 {
		t.Fatalf("early expiry: %+v", out)
	}
	*now = now.Add(time.Hour)
	if c, _ := s.Cover(ctx, "ws_1", "jb_1", dispatch.KindJob, "claude"); c != nil {
		t.Fatalf("cover after end: %+v", c)
	}
	out, err := s.ExpireDue(ctx)
	if err != nil || len(out) != 1 || out[0].EndedBy != EndedByExpiry || out[0].SkippedFires != 2 || len(hook.ended) != 1 {
		t.Fatalf("expire: %+v err=%v", out, err)
	}
	misses, err := s.TakeMisses(ctx, p.ID)
	if err != nil || len(misses) != 1 || misses[0].JobID != "jb_1" {
		t.Fatalf("misses: %+v err=%v", misses, err)
	}
	if misses, _ := s.TakeMisses(ctx, p.ID); len(misses) != 0 {
		t.Fatalf("misses taken twice: %+v", misses)
	}
}

func TestAutoPause(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, now := newTestService(t)
	reset := now.Add(3 * time.Hour).Format(time.RFC3339)
	later := now.Add(5 * 24 * time.Hour).Format(time.RFC3339)
	win := func(pct float64, r string) harness.UsageWindow {
		return harness.UsageWindow{UsedPercent: pct, ResetsAt: r}
	}

	if _, ok, _ := s.AutoPause(ctx, Limit{Harness: "claude", Windows: []harness.UsageWindow{win(98, reset)}}, 99); ok {
		t.Fatal("below threshold paused")
	}
	if _, ok, _ := s.AutoPause(ctx, Limit{Harness: "claude", Windows: []harness.UsageWindow{win(100, reset)}}, 0); ok {
		t.Fatal("off paused")
	}
	p, ok, err := s.AutoPause(ctx, Limit{Harness: "claude", Windows: []harness.UsageWindow{win(99, reset), win(40, later)}}, 99)
	if err != nil || !ok || p.Source != SourceLimit || p.Until == nil || *p.Until != reset || p.CreatedBy != CreatedByTrebi {
		t.Fatalf("auto: %+v ok=%v err=%v", p, ok, err)
	}
	// The weekly window crosses too: the pause extends.
	p2, ok, _ := s.AutoPause(ctx, Limit{Harness: "claude", Windows: []harness.UsageWindow{win(99, reset), win(99, later)}}, 99)
	if !ok || p2.ID != p.ID || *p2.Until != later {
		t.Fatalf("extend: %+v ok=%v", p2, ok)
	}
	// An early resume keeps the harness running until that reset.
	if _, err := s.Resume(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.AutoPause(ctx, Limit{Harness: "claude", Windows: []harness.UsageWindow{win(100, later)}}, 99); ok {
		t.Fatal("paused again after an early resume")
	}
	// A hard limit with no reset time holds for one hour.
	p3, ok, _ := s.AutoPause(ctx, Limit{Harness: "codex", Hit: true}, 99)
	if !ok || p3.Until == nil || *p3.Until != now.Add(UnknownResetHold).Format(time.RFC3339) {
		t.Fatalf("hard limit: %+v ok=%v", p3, ok)
	}
	// The guessed end does not move on the next tick.
	*now = now.Add(time.Minute)
	if p4, ok, _ := s.AutoPause(ctx, Limit{Harness: "codex", Hit: true}, 99); ok || *p4.Until != *p3.Until {
		t.Fatalf("hard limit extended: %+v ok=%v", p4, ok)
	}
	// A pause of a person is not changed.
	user, _, _ := s.Set(ctx, SetOpts{Scope: ScopeHarness, Target: "grok"})
	got, ok, _ := s.AutoPause(ctx, Limit{Harness: "grok", Windows: []harness.UsageWindow{win(100, reset)}}, 99)
	if ok || got.ID != user.ID || got.Until != nil {
		t.Fatalf("user pause changed: %+v ok=%v", got, ok)
	}
}

func TestCoverAgentJobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO job_index (id, workspace_id, name, path, kind, owner_id, updated_at)
		VALUES ('agt_1','ws_1','agents/ellis','p','agent','', 'x'), ('jb_1','ws_1','agents/ellis/a','p','job','agt_1','x'),
		('jb_2','ws_1','b','p','job','','x')`); err != nil {
		t.Fatal(err)
	}
	p, _, err := s.Set(ctx, SetOpts{Scope: ScopeAgent, Target: "agt_1"})
	if err != nil || p.Label != "ellis" {
		t.Fatalf("set: %+v %v", p, err)
	}
	for _, c := range []struct {
		job, kind string
		held      bool
	}{{"agt_1", dispatch.KindAgent, true}, {"jb_1", dispatch.KindJob, true}, {"jb_2", dispatch.KindJob, false}} {
		got, err := s.Cover(ctx, "ws_1", c.job, c.kind, "claude")
		if err != nil || (got != nil) != c.held {
			t.Errorf("%s: got %+v err=%v", c.job, got, err)
		}
	}
}

func TestLimitReset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	soon, later := now.Add(time.Hour), now.Add(48*time.Hour)
	ts := func(t time.Time) string { return t.Format(time.RFC3339) }
	l := Limit{Windows: []harness.UsageWindow{{UsedPercent: 40, ResetsAt: ts(soon)}, {UsedPercent: 90, ResetsAt: ts(later)}}}
	if got := l.Reset(now); !got.Equal(later) {
		t.Errorf("most used: %v", got)
	}
	l.Hit, l.ResetsAt = true, ts(soon)
	if got := l.Reset(now); !got.Equal(soon) {
		t.Errorf("hard limit: %v", got)
	}
	if got := (Limit{}).Reset(now); !got.IsZero() {
		t.Errorf("unknown: %v", got)
	}
}

func TestParseFor(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]time.Duration{"90m": 90 * time.Minute, "3h": 3 * time.Hour, "2d": 48 * time.Hour, "1w": 7 * 24 * time.Hour} {
		if got, err := ParseFor(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0d", "-1h", "xd", "soon"} {
		if _, err := ParseFor(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func newTestService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // test cleanup
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	s := New(st)
	s.SetClock(func() time.Time { return now })
	return s, &now
}

func at(now time.Time, d time.Duration) *time.Time {
	t := now.Add(d)
	return &t
}
