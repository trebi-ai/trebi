package alerts_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/store"
)

// inboxFixture raises one open approval and two health items.
type inboxFixture struct {
	svc                    *alerts.Service
	st                     *store.Store
	approval, usage, limit string
}

func newInboxFixture(t *testing.T) inboxFixture {
	t.Helper()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	f := inboxFixture{svc: svc, st: st}
	var err error
	if f.approval, err = svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindWaiting, Subject: "run/r1", RunID: "r1", Title: "Agent waiting"}); err != nil {
		t.Fatal(err)
	}
	if f.usage, err = svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindUsageWarning, Subject: "harness/a", Title: "usage"}); err != nil {
		t.Fatal(err)
	}
	if f.limit, err = svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindLimitExceeded, Harness: "grok", Title: "limit"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f inboxFixture) get(t *testing.T, id string) *alerts.InboxItem {
	t.Helper()
	it, err := f.svc.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (f inboxFixture) apply(t *testing.T, op alerts.InboxOp, sel alerts.InboxSelector) alerts.InboxApplyResult {
	t.Helper()
	res, err := f.svc.Apply(context.Background(), op, sel)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func ids(v ...string) alerts.InboxSelector { return alerts.InboxSelector{IDs: v} }

func TestInboxApply(t *testing.T) {
	t.Parallel()
	health := &alerts.InboxFilter{Kind: alerts.InboxHealth}
	tests := []struct {
		name  string
		setup func(t *testing.T, f inboxFixture)
		op    alerts.InboxOp
		sel   func(f inboxFixture) alerts.InboxSelector
		want  func(f inboxFixture) []string // updated ids, sorted
		check func(t *testing.T, f inboxFixture)
	}{
		{
			name: "read on ids",
			op:   alerts.OpRead, sel: func(f inboxFixture) alerts.InboxSelector { return ids(f.usage) },
			want: func(f inboxFixture) []string { return []string{f.usage} },
			check: func(t *testing.T, f inboxFixture) {
				if it := f.get(t, f.usage); !it.Read || it.ReadAt == nil || it.Done || it.Revision != 2 {
					t.Fatalf("%+v", it)
				}
				if f.get(t, f.limit).Read {
					t.Fatal("other item read")
				}
			},
		},
		{
			name:  "read on a filter",
			op:    alerts.OpRead,
			sel:   func(inboxFixture) alerts.InboxSelector { return alerts.InboxSelector{Filter: health} },
			want:  func(f inboxFixture) []string { return sorted(f.usage, f.limit) },
			check: func(t *testing.T, f inboxFixture) { mustRead(t, f, f.approval, false) },
		},
		{
			name:  "read skips rows that are read",
			setup: func(t *testing.T, f inboxFixture) { f.apply(t, alerts.OpRead, ids(f.usage)) },
			op:    alerts.OpRead,
			sel:   func(f inboxFixture) alerts.InboxSelector { return ids(f.usage, f.limit) },
			want:  func(f inboxFixture) []string { return []string{f.limit} },
		},
		{
			name:  "unread on ids",
			setup: func(t *testing.T, f inboxFixture) { f.apply(t, alerts.OpRead, ids(f.usage)) },
			op:    alerts.OpUnread,
			sel:   func(f inboxFixture) alerts.InboxSelector { return ids(f.usage) },
			want:  func(f inboxFixture) []string { return []string{f.usage} },
			check: func(t *testing.T, f inboxFixture) { mustRead(t, f, f.usage, false) },
		},
		{
			name: "unread on a Done item keeps it Done",
			setup: func(t *testing.T, f inboxFixture) {
				f.apply(t, alerts.OpRead, ids(f.usage))
				f.apply(t, alerts.OpArchive, ids(f.usage))
			},
			op:   alerts.OpUnread,
			sel:  func(f inboxFixture) alerts.InboxSelector { return ids(f.usage) },
			want: func(f inboxFixture) []string { return []string{f.usage} },
			check: func(t *testing.T, f inboxFixture) {
				if it := f.get(t, f.usage); it.Read || !it.Done || it.DoneBy != alerts.DoneByUser {
					t.Fatalf("%+v", it)
				}
			},
		},
		{
			name: "archive on ids",
			op:   alerts.OpArchive, sel: func(f inboxFixture) alerts.InboxSelector { return ids(f.usage) },
			want: func(f inboxFixture) []string { return []string{f.usage} },
			check: func(t *testing.T, f inboxFixture) {
				if it := f.get(t, f.usage); !it.Done || it.DoneAt == nil || it.DoneBy != alerts.DoneByUser || it.Read {
					t.Fatalf("%+v", it)
				}
			},
		},
		{
			name: "archive on a filter",
			op:   alerts.OpArchive, sel: func(inboxFixture) alerts.InboxSelector { return alerts.InboxSelector{Filter: health} },
			want: func(f inboxFixture) []string { return sorted(f.usage, f.limit) },
		},
		{
			name:  "unarchive on ids",
			setup: func(t *testing.T, f inboxFixture) { f.apply(t, alerts.OpArchive, ids(f.usage)) },
			op:    alerts.OpUnarchive,
			sel:   func(f inboxFixture) alerts.InboxSelector { return ids(f.usage) },
			want:  func(f inboxFixture) []string { return []string{f.usage} },
			check: func(t *testing.T, f inboxFixture) {
				if it := f.get(t, f.usage); it.Done || it.DoneAt != nil || it.DoneBy != "" {
					t.Fatalf("%+v", it)
				}
			},
		},
		{
			name:  "unarchive on the Done view",
			setup: func(t *testing.T, f inboxFixture) { f.apply(t, alerts.OpArchive, ids(f.usage, f.limit)) },
			op:    alerts.OpUnarchive,
			sel: func(inboxFixture) alerts.InboxSelector {
				return alerts.InboxSelector{Filter: &alerts.InboxFilter{View: alerts.ViewDone}}
			},
			want: func(f inboxFixture) []string { return sorted(f.usage, f.limit) },
		},
		{
			name: "archive of an open approval without confirm changes nothing",
			op:   alerts.OpArchive,
			sel:  func(inboxFixture) alerts.InboxSelector { return alerts.InboxSelector{Filter: &alerts.InboxFilter{}} },
			want: func(inboxFixture) []string { return nil },
			check: func(t *testing.T, f inboxFixture) {
				for _, id := range []string{f.approval, f.usage, f.limit} {
					if f.get(t, id).Done {
						t.Fatalf("%s archived", id)
					}
				}
			},
		},
		{
			name: "archive of an open approval with confirm",
			op:   alerts.OpArchive,
			sel: func(f inboxFixture) alerts.InboxSelector {
				return alerts.InboxSelector{IDs: []string{f.approval}, Confirm: true}
			},
			want: func(f inboxFixture) []string { return []string{f.approval} },
			check: func(t *testing.T, f inboxFixture) {
				if it := f.get(t, f.approval); !it.Done || it.NeedsAction {
					t.Fatalf("%+v", it)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newInboxFixture(t)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			res := f.apply(t, tt.op, tt.sel(f))
			got := slices.Sorted(slices.Values(res.Updated))
			if want := tt.want(f); !slices.Equal(got, want) {
				t.Fatalf("updated %v want %v", got, want)
			}
			if tt.check != nil {
				tt.check(t, f)
			}
		})
	}
}

func TestInboxApplyPending(t *testing.T) {
	t.Parallel()
	f := newInboxFixture(t)
	res := f.apply(t, alerts.OpArchive, ids(f.approval, f.usage))
	if len(res.Updated) != 0 || len(res.Pending) != 1 {
		t.Fatalf("%+v", res)
	}
	p := res.Pending[0]
	if p.ID != f.approval || p.Kind != alerts.InboxApproval || p.AlertKind != string(alerts.KindWaiting) || p.RunID != "r1" {
		t.Fatalf("pending %+v", p)
	}
}

func TestInboxApplyErrors(t *testing.T) {
	t.Parallel()
	f := newInboxFixture(t)
	many := make([]string, alerts.MaxApplyIDs+1)
	for i := range many {
		many[i] = fmt.Sprintf("id%d", i)
	}
	tests := []struct {
		name string
		op   alerts.InboxOp
		sel  alerts.InboxSelector
		want error
	}{
		{"unknown op", "resolve", ids(f.usage), alerts.ErrInvalid},
		{"bus-only op", alerts.OpPrune, ids(f.usage), alerts.ErrInvalid},
		{"neither ids nor filter", alerts.OpRead, alerts.InboxSelector{}, alerts.ErrInvalid},
		{"ids and filter", alerts.OpRead, alerts.InboxSelector{IDs: []string{f.usage}, Filter: &alerts.InboxFilter{}}, alerts.ErrInvalid},
		{"too many ids", alerts.OpRead, alerts.InboxSelector{IDs: many}, alerts.ErrInvalid},
		{"bad view", alerts.OpRead, alerts.InboxSelector{Filter: &alerts.InboxFilter{View: "all"}}, alerts.ErrInvalid},
		{"bad kind", alerts.OpRead, alerts.InboxSelector{Filter: &alerts.InboxFilter{Kind: "spam"}}, alerts.ErrInvalid},
		{"unknown id", alerts.OpRead, ids(f.usage, "01MISSING"), alerts.ErrNotFound},
	}
	for _, tt := range tests {
		if _, err := f.svc.Apply(context.Background(), tt.op, tt.sel); !errors.Is(err, tt.want) {
			t.Fatalf("%s: err %v want %v", tt.name, err, tt.want)
		}
	}
	mustRead(t, f, f.usage, false)
}

func TestInboxApplyBus(t *testing.T) {
	t.Parallel()
	f := newInboxFixture(t)
	ctx := context.Background()
	f.apply(t, alerts.OpRead, ids(f.usage, f.limit))
	ev := lastInboxUpdate(t, f.st)
	if ev.Op != alerts.OpRead || ev.Count != 2 || len(ev.IDs) != 2 {
		t.Fatalf("%+v", ev)
	}
	if err := f.svc.ResolveSubject(ctx, "run/r1", "auto"); err != nil {
		t.Fatal(err)
	}
	ev = lastInboxUpdate(t, f.st)
	if ev.Op != alerts.OpArchive || ev.By != alerts.DoneBySystem || !slices.Equal(ev.IDs, []string{f.approval}) {
		t.Fatalf("%+v", ev)
	}
}

func lastInboxUpdate(t *testing.T, st *store.Store) alerts.InboxUpdate {
	t.Helper()
	evs, err := st.ListBus(context.Background(), 0, []string{"inbox.updated"}, 1000)
	if err != nil || len(evs) == 0 {
		t.Fatalf("bus %v %v", evs, err)
	}
	var u alerts.InboxUpdate
	if err := json.Unmarshal(evs[len(evs)-1].Attrs, &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestRaiseAfterArchive(t *testing.T) {
	t.Parallel()
	f := newInboxFixture(t)
	ctx := context.Background()
	again, err := f.svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindUsageWarning, Subject: "harness/a", Title: "usage"})
	if err != nil || again != f.usage {
		t.Fatalf("raise while open: %s want %s (%v)", again, f.usage, err)
	}
	f.apply(t, alerts.OpArchive, ids(f.usage))
	fresh, err := f.svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindUsageWarning, Subject: "harness/a", Title: "usage"})
	if err != nil || fresh == f.usage {
		t.Fatalf("raise after archive: %s (%v)", fresh, err)
	}
	if it := f.get(t, fresh); it.Done || it.Read {
		t.Fatalf("%+v", it)
	}
}

func TestAutoResolveKeepsRead(t *testing.T) {
	t.Parallel()
	f := newInboxFixture(t)
	ctx := context.Background()
	f.apply(t, alerts.OpRead, ids(f.approval))
	if _, err := f.st.Append(ctx, store.BusEvent{Topic: "session.state", Attrs: map[string]string{"run_id": "r1", "state": "running"}}); err != nil {
		t.Fatal(err)
	}
	f.svc.Consume(ctx)
	if it := f.get(t, f.approval); !it.Done || it.DoneBy != alerts.DoneBySystem || !it.Read {
		t.Fatalf("%+v", it)
	}
	if it := f.get(t, f.usage); it.Done {
		t.Fatalf("other item closed %+v", it)
	}
}

func TestInboxPrune(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	svc, st := testSvc(t, alerts.Options{DoneRetention: 24 * time.Hour, Now: func() time.Time { return now }})
	ctx := context.Background()
	raise := func(subject string) string {
		id, err := svc.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindUsageWarning, Subject: subject, Title: subject})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	old, recent, open := raise("old"), raise("recent"), raise("open")
	if _, err := svc.Apply(ctx, alerts.OpArchive, ids(old, recent)); err != nil {
		t.Fatal(err)
	}
	setDone := func(id string, at time.Time) {
		if _, err := st.DB().Exec(`UPDATE inbox_items SET done_at=?, created_at=? WHERE id=?`,
			at.Format(time.RFC3339Nano), at.Add(-time.Hour).Format(time.RFC3339Nano), id); err != nil {
			t.Fatal(err)
		}
	}
	setDone(old, now.Add(-48*time.Hour))
	setDone(recent, now.Add(-time.Hour))
	if _, err := st.DB().Exec(`UPDATE inbox_items SET created_at=? WHERE id=?`, now.Add(-90*24*time.Hour).Format(time.RFC3339Nano), open); err != nil {
		t.Fatal(err)
	}
	n, err := svc.Prune(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
	if _, err := svc.Get(ctx, old); !errors.Is(err, alerts.ErrNotFound) {
		t.Fatalf("old item stays: %v", err)
	}
	for _, id := range []string{recent, open} {
		if _, err := svc.Get(ctx, id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if ev := lastInboxUpdate(t, st); ev.Op != alerts.OpPrune || !slices.Equal(ev.IDs, []string{old}) || ev.Count != 1 {
		t.Fatalf("%+v", ev)
	}
}

func TestInboxListFilters(t *testing.T) {
	t.Parallel()
	svc, _ := testSvc(t, alerts.Options{})
	ctx := context.Background()
	raise := func(o alerts.RaiseOpts) string {
		id, err := svc.Raise(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	alphaUsage := raise(alerts.RaiseOpts{Kind: alerts.KindUsageWarning, Subject: "u/alpha", Title: "Disk almost full", WorkspaceID: "alpha"})
	betaUsage := raise(alerts.RaiseOpts{Kind: alerts.KindUsageWarning, Subject: "u/beta", Title: "Usage", Body: "the DISK is 90% used", WorkspaceID: "beta"})
	global := raise(alerts.RaiseOpts{Kind: alerts.KindMachineDown, Subject: "m/1", Title: "Machine down"})
	approval := raise(alerts.RaiseOpts{Kind: alerts.KindWaiting, Subject: "run/r1", RunID: "r1", Title: "Agent waiting", WorkspaceID: "alpha"})
	done := raise(alerts.RaiseOpts{Kind: alerts.KindUsageWarning, Subject: "u/done", Title: "Old disk note", WorkspaceID: "alpha"})
	if _, err := svc.Apply(ctx, alerts.OpRead, ids(betaUsage)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Apply(ctx, alerts.OpArchive, ids(done)); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		f    alerts.InboxFilter
		want []string
	}{
		{"open", alerts.InboxFilter{}, sorted(alphaUsage, betaUsage, global, approval)},
		{"done", alerts.InboxFilter{View: alerts.ViewDone}, []string{done}},
		{"workspace keeps global rows", alerts.InboxFilter{Workspace: "alpha"}, sorted(alphaUsage, global, approval)},
		{"kind", alerts.InboxFilter{Kind: alerts.InboxApproval}, []string{approval}},
		{"unread", alerts.InboxFilter{Unread: true}, sorted(alphaUsage, global, approval)},
		{"q in title and body, any case", alerts.InboxFilter{Query: "disk"}, sorted(alphaUsage, betaUsage)},
		{"q in subject", alerts.InboxFilter{Query: "run/r1"}, []string{approval}},
		{"q percent is literal", alerts.InboxFilter{Query: "90%"}, []string{betaUsage}},
		{"q underscore is literal", alerts.InboxFilter{Query: "a_m"}, nil},
		{"q on the Done view", alerts.InboxFilter{View: alerts.ViewDone, Query: "disk"}, []string{done}},
		{"q with workspace", alerts.InboxFilter{Workspace: "beta", Query: "disk"}, []string{betaUsage}},
	}
	for _, tt := range tests {
		page, err := svc.List(ctx, alerts.ListOpts{InboxFilter: tt.f})
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		var got []string
		for _, it := range page.Items {
			got = append(got, it.ID)
		}
		if slices.Sort(got); !slices.Equal(got, tt.want) {
			t.Fatalf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}

	page, err := svc.List(ctx, alerts.ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	c := page.Counts
	if c.Inbox != 4 || c.Done != 1 || c.Unread[alerts.InboxHealth] != 1 || c.Unread[alerts.InboxWatchdog] != 1 ||
		c.Unread[alerts.InboxApproval] != 1 || len(c.Unread) != 6 {
		t.Fatalf("counts %+v", c)
	}
	scoped, err := svc.Counts(ctx, "beta")
	if err != nil || scoped.Inbox != 2 || scoped.Done != 0 || scoped.Unread[alerts.InboxHealth] != 0 || scoped.Unread[alerts.InboxWatchdog] != 1 {
		t.Fatalf("beta counts %+v %v", scoped, err)
	}
	for _, it := range page.Items {
		want := it.ID == approval || it.ID == global
		if it.NeedsAction != want {
			t.Fatalf("needs_action of %s = %v", it.Title, it.NeedsAction)
		}
	}
	if _, err := svc.List(ctx, alerts.ListOpts{InboxFilter: alerts.InboxFilter{View: "all"}}); !errors.Is(err, alerts.ErrInvalid) {
		t.Fatalf("bad view: %v", err)
	}
}

func TestDoctorFixAction(t *testing.T) {
	t.Parallel()
	svc, _ := testSvc(t, alerts.Options{})
	ctx := context.Background()
	svc.SyncDoctor(ctx, []alerts.DoctorFinding{
		{ID: "harness-claude", Severity: "warn", Code: "harness_home_missing", Message: "no home", Fix: alerts.FixHarnessHome, Harness: "claude"},
		{ID: "harness-update-failed-claude", Severity: "warn", Code: "harness_update_failed", Message: "update failed"},
	})
	items, _ := listAll(ctx, svc, alerts.ListOpts{})
	bySubject := map[string]alerts.InboxItem{}
	for _, it := range items {
		bySubject[it.Subject] = it
	}
	fix := bySubject["doctor/harness-claude"].Actions
	if len(fix) != 1 || fix[0].Kind != alerts.ActionFix || fix[0].Route != "/v1/system/doctor/harness-claude/fix" || fix[0].Label != "Fix" {
		t.Fatalf("fix actions %+v", fix)
	}
	if a := bySubject["doctor/harness-update-failed-claude"].Actions; len(a) != 0 {
		t.Fatalf("no-fix finding has actions %+v", a)
	}
}

func TestDoctorUserArchive(t *testing.T) {
	t.Parallel()
	svc, _ := testSvc(t, alerts.Options{})
	ctx := context.Background()
	finding := []alerts.DoctorFinding{{ID: "config-missing", Severity: "warn", Code: "config_missing", Message: "no config"}}
	svc.SyncDoctor(ctx, finding)
	open, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{View: alerts.ViewInbox}})
	if len(open) != 1 {
		t.Fatalf("%+v", open)
	}
	if _, err := svc.Apply(ctx, alerts.OpArchive, ids(open[0].ID)); err != nil {
		t.Fatal(err)
	}
	svc.SyncDoctor(ctx, finding)
	if again, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{View: alerts.ViewInbox}}); len(again) != 0 {
		t.Fatalf("archived finding came back while it stays: %+v", again)
	}
	// The finding clears, then returns: it shows again.
	svc.SyncDoctor(ctx, nil)
	svc.SyncDoctor(ctx, finding)
	if back, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{View: alerts.ViewInbox}}); len(back) != 1 {
		t.Fatalf("returned finding not shown: %+v", back)
	}
}

func TestAuthMissingLogIn(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "auth.probe.failed", Entity: "harness", EntityID: "opencode", Action: "updated",
		Attrs: map[string]string{"harness": "opencode", "status": "missing"},
	}); err != nil {
		t.Fatal(err)
	}
	svc.Consume(ctx)
	items, _ := listAll(ctx, svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Kind: alerts.InboxHealth}})
	if len(items) != 1 {
		t.Fatalf("%+v", items)
	}
	it, err := svc.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "OpenCode needs you to log in" {
		t.Fatalf("title %q", it.Title)
	}
	if len(it.Actions) != 1 || it.Actions[0].Label != "Log in" || it.Actions[0].Route != "/v1/harnesses/opencode/login" {
		t.Fatalf("actions %+v", it.Actions)
	}
	for _, s := range []string{it.Body, it.Hint, it.Title} {
		if strings.Contains(s, "trebi ") {
			t.Fatalf("CLI text in %q", s)
		}
	}
	if !strings.Contains(it.Body, "OpenCode") || !it.NeedsAction {
		t.Fatalf("%+v", it)
	}
}

func mustRead(t *testing.T, f inboxFixture, id string, want bool) {
	t.Helper()
	if got := f.get(t, id).Read; got != want {
		t.Fatalf("%s read %v want %v", id, got, want)
	}
}

func sorted(v ...string) []string { return slices.Sorted(slices.Values(v)) }

func TestRunNotices(t *testing.T) {
	t.Parallel()
	f := newInboxFixture(t)
	ctx := context.Background()
	got, err := f.svc.RunNotices(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InboxID != f.approval || got[0].Kind != "inbox" || got[0].Title != "Agent waiting" || got[0].AlertKind != string(alerts.KindWaiting) {
		t.Fatalf("notices: %+v", got)
	}
	none, err := f.svc.RunNotices(ctx, "other")
	if err != nil || len(none) != 0 {
		t.Fatalf("other run: %+v %v", none, err)
	}
}

func TestRouteWaitActions(t *testing.T) {
	t.Parallel()
	svc, _ := testSvc(t, alerts.Options{})
	ctx := context.Background()
	body := json.RawMessage(`{"workspace":"ws","to":"rt_a"}`)
	acts := []alerts.InboxAction{
		{Kind: alerts.ActionRouteChoose, Route: "/v1/events/evt_1/reroute", Label: "Wake alpha", Body: body},
		{Kind: alerts.ActionRouteChoose, Route: "/v1/events/evt_1/reroute", Label: "Run beta"},
		{Kind: alerts.ActionRouteChoose, Route: "/v1/events/evt_1/reroute", Label: "Ignore"},
		{Kind: alerts.ActionRouteReview, Route: "/routing/gmail?decision=dec_1", Label: "Improve routing…"},
	}
	id, err := svc.Raise(ctx, alerts.RaiseOpts{
		Kind: alerts.KindRouteWait, Subject: alerts.RouteWaitSubject("dec_1", "evt_1"), Body: "From a@b.com: Invoice",
		WorkspaceID: "ws", Connector: "gmail", Actions: acts,
	})
	if err != nil {
		t.Fatal(err)
	}
	it, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if it.Kind != alerts.InboxApproval || !it.NeedsAction || it.AlertKind != "route_wait" {
		t.Fatalf("%+v", it)
	}
	if len(it.Actions) != 4 || it.Actions[0].Label != "Wake alpha" || it.Actions[2].Label != "Ignore" || it.Actions[3].Kind != alerts.ActionRouteReview {
		t.Fatalf("actions %+v", it.Actions)
	}
	if string(it.Actions[0].Body) != string(body) {
		t.Fatalf("body %s", it.Actions[0].Body)
	}
	if !strings.HasPrefix(it.Title, "Choose who gets a ") || !strings.HasSuffix(it.Title, " message") {
		t.Fatalf("title %q", it.Title)
	}
	page, err := svc.List(ctx, alerts.ListOpts{InboxFilter: alerts.InboxFilter{AlertKind: "route_wait", Connector: "gmail"}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("filter: %v %v", page.Items, err)
	}
	page, err = svc.List(ctx, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Connector: "other"}})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("connector filter: %v %v", page.Items, err)
	}
}

func TestScanInboxRouteSubject(t *testing.T) {
	t.Parallel()
	svc, _ := testSvc(t, alerts.Options{})
	id, err := svc.Raise(context.Background(), alerts.RaiseOpts{
		Kind: alerts.KindRouteWait, Subject: alerts.RouteWaitSubject("dec_9", "evt_7"), Title: "Choose",
	})
	if err != nil {
		t.Fatal(err)
	}
	it, err := svc.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if it.DecisionID != "dec_9" || it.EventID != "evt_7" {
		t.Fatalf("%+v", it)
	}
}
