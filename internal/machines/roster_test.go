package machines

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

type fakeCloud struct {
	roster    cloudlink.Roster
	rosterErr error
	pubErr    error
	published [][]cloudlink.WorkspaceIndexItem
}

func (f *fakeCloud) Roster(context.Context) (cloudlink.Roster, error) { return f.roster, f.rosterErr }

func (f *fakeCloud) PublishWorkspaces(_ context.Context, items []cloudlink.WorkspaceIndexItem) error {
	if f.pubErr != nil {
		return f.pubErr
	}
	f.published = append(f.published, items)
	return nil
}

type fakeWorkspaces []workspaces.Workspace

func (f fakeWorkspaces) List(context.Context) ([]workspaces.Workspace, error) { return f, nil }

func TestSyncRoster(t *testing.T) {
	testHome(t)
	ctx := context.Background()
	st := openStore(t)
	link := &fakeLink{id: "dev_self", name: "mini"}
	cloud := &fakeCloud{roster: cloudlink.Roster{Limit: 5, Machines: []cloudlink.RosterMachine{
		{DeviceID: "dev_self", Name: "mini"},
		{DeviceID: "dev_a", Name: "studio", LastSeen: strp("2026-10-30T12:00:00Z"), Capabilities: &cloudlink.Capabilities{OS: "darwin", Arch: "arm64"}},
		{DeviceID: "dev_b", Name: ""},
	}}}
	svc := New(st, nil, nil, nil, link).WithCloud(cloud, nil)

	if err := svc.SyncRoster(ctx); err != nil {
		t.Fatal(err)
	}
	rows := remoteRows(t, svc)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	a := rows["dev_a"]
	if a.Name != "studio" || a.OS != "darwin" || a.Arch != "arm64" || a.LastSeen == nil || *a.LastSeen != "2026-10-30T12:00:00Z" || a.Revision != 1 {
		t.Fatalf("dev_a = %+v", a)
	}
	if rows["dev_b"].Name != "dev_b" {
		t.Fatalf("dev_b name = %q", rows["dev_b"].Name)
	}
	if n := busCount(t, st, "machine.created"); n != 3 { // self + 2
		t.Fatalf("machine.created = %d", n)
	}

	// The same roster changes nothing.
	before := busCount(t, st, "machine.updated")
	if err := svc.SyncRoster(ctx); err != nil {
		t.Fatal(err)
	}
	if n := busCount(t, st, "machine.updated"); n != before {
		t.Fatalf("machine.updated after same roster = %d, want %d", n, before)
	}

	// A rename updates the row, and a missing machine goes.
	cloud.roster.Machines = []cloudlink.RosterMachine{{DeviceID: "dev_a", Name: "studio 2"}}
	if err := svc.SyncRoster(ctx); err != nil {
		t.Fatal(err)
	}
	rows = remoteRows(t, svc)
	if len(rows) != 1 || rows["dev_a"].Name != "studio 2" || rows["dev_a"].Revision != 2 || rows["dev_a"].LastSeen != nil {
		t.Fatalf("rows after change = %+v", rows)
	}
	if n := busCount(t, st, "machine.deleted"); n != 1 {
		t.Fatalf("machine.deleted = %d", n)
	}

	// A failed read keeps the cache.
	cloud.rosterErr = errors.New("offline")
	if err := svc.SyncRoster(ctx); err == nil {
		t.Fatal("want error")
	}
	if len(remoteRows(t, svc)) != 1 {
		t.Fatal("failed read dropped rows")
	}

	// Without a link only the self row stays.
	link.id, link.name = "", ""
	if err := svc.SyncRoster(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].IsSelf {
		t.Fatalf("unlinked list = %+v", list)
	}
}

func TestPublishWorkspaces(t *testing.T) {
	testHome(t)
	ctx := context.Background()
	st := openStore(t)
	link := &fakeLink{}
	cloud := &fakeCloud{}
	ws := fakeWorkspaces{
		{ID: "ws_1", Name: "Ellis\x07 Labs", Handle: "ellis-labs", Machine: &workspaces.MachineRef{IsSelf: true}},
		{ID: "ws_2", Name: "  ", Handle: "Bad Handle!"},
		{ID: "ws_far", Name: "Remote", Machine: &workspaces.MachineRef{IsSelf: false}},
		{ID: "bad id", Name: "Bad"},
		{ID: "ws_long", Name: strings.Repeat("é", 130)},
	}
	svc := New(st, nil, nil, nil, link).WithCloud(cloud, ws)

	// No link: no send.
	if err := svc.PublishWorkspaces(ctx); err != nil || len(cloud.published) != 0 {
		t.Fatalf("unlinked publish = %v, %d", err, len(cloud.published))
	}

	link.id = "dev_self"
	if err := svc.PublishWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cloud.published) != 1 {
		t.Fatalf("published = %d", len(cloud.published))
	}
	got := cloud.published[0]
	if len(got) != 3 {
		t.Fatalf("items = %+v", got)
	}
	if got[0] != (cloudlink.WorkspaceIndexItem{ID: "ws_1", Name: "Ellis Labs", Handle: "ellis-labs"}) {
		t.Fatalf("item 0 = %+v", got[0])
	}
	if got[1] != (cloudlink.WorkspaceIndexItem{ID: "ws_2", Name: "ws_2"}) {
		t.Fatalf("item 1 = %+v", got[1])
	}
	if n := len([]rune(got[2].Name)); n != indexNameMax {
		t.Fatalf("long name runes = %d", n)
	}

	// The same index is not sent again.
	if err := svc.PublishWorkspaces(ctx); err != nil || len(cloud.published) != 1 {
		t.Fatalf("repeat publish = %v, %d", err, len(cloud.published))
	}

	// A failed send retries on the next call.
	svc.WithCloud(cloud, ws[:1])
	cloud.pubErr = errors.New("offline")
	if err := svc.PublishWorkspaces(ctx); err == nil {
		t.Fatal("want error")
	}
	cloud.pubErr = nil
	if err := svc.PublishWorkspaces(ctx); err != nil || len(cloud.published) != 2 || len(cloud.published[1]) != 1 {
		t.Fatalf("retry = %v, %+v", err, cloud.published)
	}

	// A new link sends the index again.
	link.id = ""
	_ = svc.PublishWorkspaces(ctx)
	link.id = "dev_other"
	if err := svc.PublishWorkspaces(ctx); err != nil || len(cloud.published) != 3 {
		t.Fatalf("relink = %v, %d", err, len(cloud.published))
	}
}

func strp(s string) *string { return &s }

func remoteRows(t *testing.T, svc *Service) map[string]Machine {
	t.Helper()
	list, err := svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Machine{}
	for _, m := range list {
		if !m.IsSelf && m.DeviceID != nil {
			out[*m.DeviceID] = m
		}
	}
	return out
}
