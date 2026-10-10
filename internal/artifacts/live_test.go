package artifacts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

type fakeSources struct {
	mu    sync.Mutex
	calls []connectors.CallOpts
}

func (f *fakeSources) Call(_ context.Context, c connectors.CallOpts) (connectors.CallResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if c.Operation == "broken" {
		return connectors.CallResult{}, errors.New("timeout")
	}
	return connectors.CallResult{Rows: &connectors.Rows{Columns: []string{"day", "total"}, Values: [][]string{{"mon", "1200.5"}}}}, nil
}

func (f *fakeSources) List(context.Context, string, string) ([]connectors.OperationView, error) {
	return []connectors.OperationView{
		{Operation: manifest.Operation{Name: "query", ReadOnly: true}},
		{Operation: manifest.Operation{Name: "broken", ReadOnly: true}},
		{Operation: manifest.Operation{Name: "send"}},
	}, nil
}

func TestCanonicalSources(t *testing.T) {
	t.Parallel()
	a := []Source{{Name: "b", Input: map[string]any{"y": 1, "x": 2}}, {Name: "a"}}
	b := []Source{{Name: "a"}, {Name: "b", Input: map[string]any{"x": 2, "y": 1}}}
	if CanonicalSources(a) != CanonicalSources(b) {
		t.Fatalf("order changed the form:\n%s\n%s", CanonicalSources(a), CanonicalSources(b))
	}
	if a[0].Name != "b" {
		t.Fatal("CanonicalSources must not sort its argument")
	}
}

func TestLiveCheck(t *testing.T) {
	t.Parallel()
	if errs := liveSpecFor("query").Check(); len(errs) != 0 {
		t.Fatalf("valid spec: %v", errs)
	}
	bad := liveSpecFor("query")
	bad.Params[0].Default = "90d"
	bad.Sources[0].Name = "Bad-Name"
	if errs := bad.Check(); len(errs) < 2 {
		t.Fatalf("want two errors, got %v", errs)
	}
	if _, _, err := liveSpecFor("query").Inputs(map[string]string{"nope": "1"}); err == nil {
		t.Fatal("an unknown parameter must fail")
	}
	given, values, err := liveSpecFor("query").Inputs(map[string]string{"period": "30d"})
	if err != nil || given["period"] != "30d" || values["period"] != "30" {
		t.Fatalf("inputs %v %v %v", given, values, err)
	}
}

func TestLiveApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, al := newLiveService(t)
	run := Origin{Kind: OriginRun, RunID: "r1", OwnerKind: OwnerJob, OwnerID: "job1"}

	if v := putLive(t, s, run, liveSpecFor("query")); v.Approval != ApprovalPending {
		t.Fatalf("first agent spec: %s", v.Approval)
	}
	id := mustID(t, s)
	if _, err := s.Refresh(ctx, RefreshOpts{ID: id}); !errors.Is(err, ErrPendingApproval) {
		t.Fatalf("refresh before approval: %v", err)
	}
	if len(al.got) != 1 || al.got[0].Subject != approvalSubject(id, 1) {
		t.Fatalf("approval item: %+v", al.got)
	}
	a, err := s.Decide(ctx, id, 1, true, OriginUser)
	if err != nil || a.LiveVersion == nil || *a.LiveVersion != 1 {
		t.Fatalf("decide: %+v %v", a, err)
	}
	if len(al.resolved) != 1 {
		t.Fatal("decide must resolve the inbox item")
	}

	// A new template with the same spec inherits the approval.
	v, err := s.Put(ctx, PutOpts{Workspace: "ws1", Name: "sales", FileName: "sales.md", Origin: run,
		Content: strings.NewReader("# new {{ table .sa }}")})
	if err != nil || v.Approval != ApprovalApproved || v.Live == nil {
		t.Fatalf("inherit: %+v %v", v, err)
	}

	// A changed source waits again, and a refresh uses the last allowed version.
	if v := putLive(t, s, run, liveSpecFor("query", "query")); v.Approval != ApprovalPending {
		t.Fatalf("changed spec: %s", v.Approval)
	}
	sn, err := s.Refresh(ctx, RefreshOpts{ID: id})
	if err != nil || sn.VersionN != 2 {
		t.Fatalf("refresh on the live version: %+v %v", sn, err)
	}
	if _, err := s.Decide(ctx, id, 3, false, OriginUser); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Get(ctx, id); *a.LiveVersion != 2 {
		t.Fatalf("deny moved the live version to %d", *a.LiveVersion)
	}

	// A save of the user is approved at once.
	base := 3
	v, err = s.Put(ctx, PutOpts{Workspace: "ws1", Name: "sales", FileName: "sales.md", Base: &base,
		Content: strings.NewReader("x"), Origin: Origin{Kind: OriginUser}, Live: liveSpecFor("query", "query")})
	if err != nil || v.Approval != ApprovalApproved {
		t.Fatalf("user spec: %+v %v", v, err)
	}
}

func TestLiveRejectsWriteOperation(t *testing.T) {
	t.Parallel()
	s, _, _ := newLiveService(t)
	_, err := s.Put(context.Background(), PutOpts{Workspace: "ws1", Name: "sales", FileName: "sales.md",
		Content: strings.NewReader("x"), Origin: Origin{Kind: OriginUser, OwnerKind: OwnerAssistant}, Live: liveSpecFor("send")})
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("a write operation must fail: %v", err)
	}
	_, err = s.Put(context.Background(), PutOpts{Workspace: "ws1", Name: "data", FileName: "data.csv",
		Content: strings.NewReader("a,b"), Origin: Origin{Kind: OriginUser, OwnerKind: OwnerAssistant}, Live: liveSpecFor("query")})
	if !errors.As(err, &inv) {
		t.Fatalf("a csv template must fail: %v", err)
	}
}

func TestLiveRefresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, src, _ := newLiveService(t)
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	putLive(t, s, Origin{Kind: OriginUser, OwnerKind: OwnerAssistant}, liveSpecFor("query", "broken"))
	id := mustID(t, s)

	sn, err := s.Refresh(ctx, RefreshOpts{ID: id, Inputs: map[string]string{"period": "30d"}})
	if err != nil {
		t.Fatal(err)
	}
	if sn.Errors["sb"] != "timeout" || sn.Errors["sa"] != "" || sn.DataSHA256 == "" {
		t.Fatalf("one failed source must keep the other data: %+v", sn)
	}
	for _, c := range src.calls {
		if c.Input["days"] != "30" || !c.RequireReadOnly {
			t.Fatalf("call %+v", c)
		}
	}
	data, err := s.SnapshotData(ctx, sn)
	if err != nil || data["sa"].Rows == nil || data["sb"].Error != "timeout" {
		t.Fatalf("data %+v %v", data, err)
	}

	var rl *RateLimitError
	if _, err := s.Refresh(ctx, RefreshOpts{ID: id}); !errors.As(err, &rl) {
		t.Fatalf("a second refresh of the user within 10s: %v", err)
	}
	if _, err := s.Refresh(ctx, RefreshOpts{ID: id, Reason: ReasonSchedule}); err != nil {
		t.Fatalf("a scheduled refresh has no limit: %v", err)
	}
	now = now.Add(11 * time.Second)
	if _, err := s.Refresh(ctx, RefreshOpts{ID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, RefreshOpts{ID: id, Inputs: map[string]string{"period": "1y"}}); err == nil {
		t.Fatal("an unknown enum value must fail")
	}

	body, _, got, err := s.Bound(ctx, id, 1, "")
	if err != nil || got == nil || !strings.Contains(string(body), "| mon | 1200.5 |") {
		t.Fatalf("bound %q %+v %v", body, got, err)
	}
}

func TestBoundHTMLEscapesScript(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newLiveService(t)
	_, err := s.Put(ctx, PutOpts{Workspace: "ws1", Name: "page", FileName: "page.html", Content: strings.NewReader("<p>hi</p>"),
		Origin: Origin{Kind: OriginUser, OwnerKind: OwnerAssistant}, Live: liveSpecFor("query")})
	if err != nil {
		t.Fatal(err)
	}
	id := mustID(t, s)
	if _, err := s.Refresh(ctx, RefreshOpts{ID: id, Inputs: map[string]string{"period": "30d"}}); err != nil {
		t.Fatal(err)
	}
	body, _, _, err := s.Bound(ctx, id, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	head, rest, _ := strings.Cut(string(body), "\n")
	if !strings.HasPrefix(head, "<script>window.trebi=") || rest != "<p>hi</p>" {
		t.Fatalf("html bind %q", body)
	}
	if strings.Count(head, "</") != 1 {
		t.Fatalf("data must not close the script: %q", head)
	}
}

func liveSpecFor(ops ...string) *Live {
	l := &Live{Params: []Param{{Name: "period", Label: "Period", Type: ParamEnum, Options: []string{"7d", "30d"},
		Values: map[string]string{"7d": "7", "30d": "30"}, Default: "7d"}}}
	for i, op := range ops {
		l.Sources = append(l.Sources, Source{Name: "s" + string(rune('a'+i)), Connection: "db", Operation: op,
			Input: map[string]any{"days": "{param.period}"}})
	}
	return l
}

func newLiveService(t *testing.T) (*Service, *fakeSources, *fakeAlerts) {
	t.Helper()
	src, al := &fakeSources{}, &fakeAlerts{}
	s := newTestService(t, nil, nil).WithSources(src).WithAlerts(al)
	return s, src, al
}

func putLive(t *testing.T, s *Service, o Origin, l *Live) Version {
	t.Helper()
	v, err := s.Put(context.Background(), PutOpts{Workspace: "ws1", Name: "sales", FileName: "sales.md",
		Content: strings.NewReader("{{ table .sa }}"), Origin: o, Live: l})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
