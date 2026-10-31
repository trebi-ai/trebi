package machines

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/mobile"
	"github.com/trebi-ai/trebi/internal/relay"
	"github.com/trebi-ai/trebi/internal/secrets"
)

type fakeLinkCloud struct {
	enrollErr error
	dialErr   error
	left      []string
}

func (*fakeLinkCloud) APIHost() string    { return "staging-api.trebi.ai" }
func (*fakeLinkCloud) DeviceID() string   { return "dev_self" }
func (*fakeLinkCloud) DeviceName() string { return "mbp" }

func (f *fakeLinkCloud) EnrollPeer(context.Context, string, string) (cloudlink.Enrollment, error) {
	if f.enrollErr != nil {
		return cloudlink.Enrollment{}, f.enrollErr
	}
	return cloudlink.Enrollment{ClientID: "cl_1", Token: "trc_new"}, nil
}

func (f *fakeLinkCloud) LeaveClient(_ context.Context, token string) error {
	f.left = append(f.left, token)
	return nil
}

func (f *fakeLinkCloud) DialRelay(context.Context, string) (*cloudlink.RelaySocket, error) {
	return nil, f.dialErr
}

func TestLinkConnect(t *testing.T) {
	testHome(t)
	ctx := context.Background()
	st := openStore(t)
	dev := "dev_other"
	other := Machine{ID: "mch_other", Name: "Studio", DeviceID: &dev}

	cases := []struct {
		name  string
		m     Machine
		link  string
		cloud *fakeLinkCloud
		want  error
		left  []string
	}{
		{"self", Machine{ID: "mch_self", IsSelf: true}, pairLink(dev, "staging-api.trebi.ai"), &fakeLinkCloud{}, ErrBadLink, nil},
		{"not a link", other, "https://example.com/", &fakeLinkCloud{}, ErrBadLink, nil},
		{"other machine", other, pairLink("dev_3", "staging-api.trebi.ai"), &fakeLinkCloud{}, ErrBadLink, nil},
		{"other cloud", other, pairLink(dev, "api.trebi.ai"), &fakeLinkCloud{}, ErrBadLink, nil},
		{"used", other, pairLink(dev, "staging-api.trebi.ai"), &fakeLinkCloud{enrollErr: &cloudlink.APIError{Status: 401, Code: "bad_claim"}}, ErrLinkUsed, nil},
		{"no answer", other, pairLink(dev, "staging-api.trebi.ai"), &fakeLinkCloud{dialErr: errors.New("dial")}, nil, []string{"trc_new"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLinks(st, tc.cloud, "v0")
			err := l.Connect(ctx, tc.m, tc.link)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if strings.Join(tc.cloud.left, ",") != strings.Join(tc.left, ",") {
				t.Fatalf("left = %v", tc.cloud.left)
			}
			if _, err := l.creds(ctx, tc.m.ID); !errors.Is(err, ErrNoLink) {
				t.Fatalf("a failed connect saved a link: %v", err)
			}
		})
	}
}

func TestLinkFillAndDisconnect(t *testing.T) {
	testHome(t)
	ctx := context.Background()
	st := openStore(t)
	now := "2026-10-30T00:00:00Z"
	if _, err := st.DB().Exec(`INSERT INTO machines (id, name, is_self, created_at, updated_at) VALUES ('mch_self', 'Self', 1, ?, ?), ('mch_other', 'Studio', 0, ?, ?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	cloud := &fakeLinkCloud{}
	l := NewLinks(st, cloud, "v0")
	svc := New(st, fakeAuth{}, fakeLocator{}, fakeConns{}, nil).WithLinks(l)

	state := func() *Link {
		t.Helper()
		m, err := svc.Get(ctx, "mch_other")
		if err != nil {
			t.Fatal(err)
		}
		return m.Link
	}
	if got := state(); got == nil || got.State != LinkNone {
		t.Fatalf("before = %+v", got)
	}
	if _, err := l.Peer(ctx, "mch_other"); !errors.Is(err, ErrNoLink) {
		t.Fatalf("peer without link: %v", err)
	}

	other := Machine{ID: "mch_other", Name: "Studio"}
	before := busCount(t, st, "machine.updated")
	if err := l.save(ctx, other, relay.PeerCreds{DeviceID: "dev_other", ClientID: "cl_1", Token: "trc_1"}); err != nil {
		t.Fatal(err)
	}
	if got := state(); got == nil || got.State != LinkLinked || got.LinkedAt == nil {
		t.Fatalf("linked = %+v", got)
	}
	self, err := svc.Get(ctx, "mch_self")
	if err != nil || self.Link != nil {
		t.Fatalf("self link = %+v %v", self.Link, err)
	}
	if p, err := l.Peer(ctx, "mch_other"); err != nil || p == nil {
		t.Fatalf("peer = %v %v", p, err)
	}
	if n := busCount(t, st, "machine.updated") - before; n != 1 {
		t.Fatalf("machine.updated = %d", n)
	}

	if err := l.Disconnect(ctx, other); err != nil {
		t.Fatal(err)
	}
	if got := state(); got.State != LinkNone {
		t.Fatalf("after disconnect = %+v", got)
	}
	if _, err := secrets.Get(linkSecret("mch_other")); err == nil {
		t.Fatal("the link secret is still there")
	}
	if strings.Join(cloud.left, ",") != "trc_1" {
		t.Fatalf("left = %v", cloud.left)
	}
	if err := l.Disconnect(ctx, other); err != nil {
		t.Fatalf("second disconnect: %v", err)
	}
}

func pairLink(device, host string) string {
	key := make([]byte, 32)
	return mobile.PairLink(mobile.LinkParts{APIHost: host, DeviceID: device, DaemonKey: key, PSK: key,
		Claim: "trcl_x", OfferID: "off_1", Machine: "Studio"})
}
