package mobile

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
)

type fakeCloud struct {
	mu        sync.Mutex
	target    Target
	revoked   []string
	revokeErr error
	calls     []string
}

func (c *fakeCloud) PairTarget() Target {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.target
}

func (c *fakeCloud) MintClaim(_ context.Context, uses int, ttl time.Duration) (string, time.Time, error) {
	return "trcl_claim", time.Now().Add(ttl), nil
}

func (c *fakeCloud) RevokeClient(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "revoke")
	c.revoked = append(c.revoked, id)
	return c.revokeErr
}

type fakeSessions struct {
	mu     sync.Mutex
	closed []string
	log    *[]string
}

func (f *fakeSessions) CloseClient(id, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id+":"+code)
	if f.log != nil {
		*f.log = append(*f.log, "close")
	}
}

func (f *fakeSessions) Connected(string) bool { return true }

func TestPairOfferLifecycle(t *testing.T) {
	svc, cloud, st := newTestService(t)
	ctx := context.Background()
	now := time.Now()
	svc.now = func() time.Time { return now }

	o, err := svc.Pair(ctx, PairInput{})
	if err != nil {
		t.Fatal(err)
	}
	if o.Uses != DefaultUses || o.ExpiresAt.Sub(now) > DefaultTTL+time.Second {
		t.Fatalf("defaults = %+v", o)
	}
	psk, ok := svc.OfferPSK(o.ID)
	if !ok || len(psk) != 32 {
		t.Fatal("offer psk missing")
	}

	// Limits.
	for _, in := range []PairInput{{TTL: 30 * time.Second}, {TTL: MaxTTL + time.Hour}, {Uses: MaxUses + 1}, {Uses: -1}} {
		if _, err := svc.Pair(ctx, in); !errors.Is(err, ErrValidation) {
			t.Errorf("pair %+v: err = %v, want validation", in, err)
		}
	}
	if _, err := svc.Pair(ctx, PairInput{TTL: MaxTTL, Uses: MaxUses}); err != nil {
		t.Fatalf("max offer: %v", err)
	}

	// Uses: a 2-use offer allows two redeems.
	two, err := svc.Pair(ctx, PairInput{Uses: 2, Name: "Lab tablet"})
	if err != nil {
		t.Fatal(err)
	}
	c1, err := svc.Redeem(ctx, two.ID, enroll("cloud-1"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if c1.Name != "Lab tablet" || c1.Scope != ScopeFull {
		t.Fatalf("client = %+v", c1)
	}
	if _, err := svc.Redeem(ctx, two.ID, enroll("cloud-2"), bytes.Repeat([]byte{2}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Redeem(ctx, two.ID, enroll("cloud-3"), bytes.Repeat([]byte{3}, 32)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("third redeem: %v", err)
	}
	if n := busCount(t, st, "mobile.paired"); n != 2 {
		t.Fatalf("mobile.paired = %d", n)
	}

	// Cancel.
	if err := svc.CancelOffer(o.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.OfferPSK(o.ID); ok {
		t.Fatal("cancelled offer still open")
	}
	if err := svc.CancelOffer(o.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel twice: %v", err)
	}

	// Expiry.
	short, err := svc.Pair(ctx, PairInput{TTL: MinTTL})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(MinTTL + time.Second)
	if _, ok := svc.OfferPSK(short.ID); ok {
		t.Fatal("expired offer still open")
	}

	// A restart (a new service) has no offers.
	live, err := svc.Pair(ctx, PairInput{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := New(st, cloud).OfferPSK(live.ID); ok {
		t.Fatal("offer survived a restart")
	}

	// The 409 cases.
	for _, tc := range []struct {
		target Target
		want   error
	}{
		{Target{}, ErrNotLinked},
		{Target{Linked: true}, ErrRemoteAccess},
		{Target{Linked: true, RemoteAccess: true}, ErrMobileOff},
	} {
		cloud.mu.Lock()
		cloud.target = tc.target
		cloud.mu.Unlock()
		if _, err := svc.Pair(ctx, PairInput{}); !errors.Is(err, tc.want) {
			t.Errorf("pair with %+v: %v, want %v", tc.target, err, tc.want)
		}
	}
}

func TestRemoveOrder(t *testing.T) {
	svc, cloud, st := newTestService(t)
	ctx := context.Background()
	var order []string
	hub := &fakeSessions{log: &order}
	svc.WithSessions(hub)
	cloud.revokeErr = errors.New("cloud down")

	o, err := svc.Pair(ctx, PairInput{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.Redeem(ctx, o.ID, enroll("cloud-1"), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.Lookup(c.PublicKey); !ok {
		t.Fatal("lookup before remove")
	}
	if err := svc.Remove(ctx, c.ID); err != nil {
		t.Fatalf("remove with a cloud fault: %v", err)
	}
	order = append(order, cloud.calls...)
	if strings.Join(order, ",") != "close,revoke" {
		t.Fatalf("order = %v", order)
	}
	if hub.closed[0] != c.ID+":revoked" {
		t.Fatalf("closed = %v", hub.closed)
	}
	if cloud.revoked[0] != "cloud-1" {
		t.Fatalf("revoked = %v", cloud.revoked)
	}
	if _, err := svc.Get(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after remove: %v", err)
	}
	if _, ok := svc.Lookup(c.PublicKey); ok {
		t.Fatal("lookup after remove")
	}
	if _, err := secrets.Get(pskSecret(c.ID)); err == nil {
		t.Fatal("psk secret not deleted")
	}
	if n := busCount(t, st, "mobile.removed"); n != 1 {
		t.Fatalf("mobile.removed = %d", n)
	}
	if err := svc.Remove(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove twice: %v", err)
	}

	// A cloud revoke removes locally with no cloud call.
	o, _ = svc.Pair(ctx, PairInput{})
	c2, err := svc.Redeem(ctx, o.ID, enroll("cloud-2"), bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc.Revoked(ctx, "cloud-2")
	if _, err := svc.Get(ctx, c2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked phone kept")
	}
	if len(cloud.revoked) != 1 {
		t.Fatalf("Revoked called the cloud: %v", cloud.revoked)
	}
}

func TestUpdateScope(t *testing.T) {
	svc, _, st := newTestService(t)
	ctx := context.Background()
	hub := &fakeSessions{}
	svc.WithSessions(hub)
	o, _ := svc.Pair(ctx, PairInput{})
	c, err := svc.Redeem(ctx, o.ID, enroll("cloud-1"), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	name := "Kitchen iPad"
	got, err := svc.Update(ctx, c.ID, UpdateInput{Name: &name})
	if err != nil || got.Name != name || len(hub.closed) != 0 {
		t.Fatalf("rename = %+v %v closed=%v", got, err, hub.closed)
	}
	read := ScopeRead
	got, err = svc.Update(ctx, c.ID, UpdateInput{Scope: &read})
	if err != nil || got.Scope != ScopeRead || len(hub.closed) != 1 {
		t.Fatalf("scope = %+v %v closed=%v", got, err, hub.closed)
	}
	bad := "admin"
	if _, err := svc.Update(ctx, c.ID, UpdateInput{Scope: &bad}); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad scope: %v", err)
	}
	if n := busCount(t, st, "mobile.updated"); n != 2 {
		t.Fatalf("mobile.updated = %d", n)
	}
}

// TestPSKNeverServed: the JSON of phones and offers, and the bus rows,
// hold no psk and no key bytes.
func TestPSKNeverServed(t *testing.T) {
	svc, _, st := newTestService(t)
	ctx := context.Background()
	svc.WithSessions(&fakeSessions{})
	o, err := svc.Pair(ctx, PairInput{})
	if err != nil {
		t.Fatal(err)
	}
	offerPSK, _ := svc.OfferPSK(o.ID)
	clientPSK := bytes.Repeat([]byte{0xA5}, 32)
	c, err := svc.Redeem(ctx, o.ID, enroll("cloud-9"), clientPSK)
	if err != nil {
		t.Fatal(err)
	}
	key, err := svc.StaticKey()
	if err != nil {
		t.Fatal(err)
	}
	list, _ := svc.List(ctx)
	one, _ := svc.Get(ctx, c.ID)
	looked, _ := svc.Lookup(c.PublicKey)
	o.Link = "" // the link is the one place the offer psk rides, on purpose
	var dump bytes.Buffer
	for _, v := range []any{list, one, looked, o} {
		raw, _ := json.Marshal(v)
		dump.Write(raw)
	}
	rows, err := st.DB().Query(`SELECT COALESCE(attrs_json, '') FROM event_log`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		dump.WriteString(s)
	}
	_ = rows.Close()
	text := dump.String()
	for name, secret := range map[string][]byte{"client psk": clientPSK, "offer psk": offerPSK, "static private": key.Private, "phone key": c.PublicKey} {
		for _, enc := range []string{base64.StdEncoding.EncodeToString(secret), base64.RawURLEncoding.EncodeToString(secret), hex.EncodeToString(secret)} {
			if strings.Contains(text, enc) {
				t.Errorf("%s leaked in served JSON", name)
			}
		}
	}
	// The static key persists as a 0600 secret and loads again.
	again, err := New(st, &fakeCloud{}).StaticKey()
	if err != nil || !bytes.Equal(again.Public, key.Public) {
		t.Fatalf("static key reload: %v", err)
	}
	fi, err := os.Stat(filepath.Join(home.SecretsDir(), staticKeySecret))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("static key file: %v %v", fi, err)
	}
}

func TestPairLink(t *testing.T) {
	svc, _, _ := newTestService(t)
	o, err := svc.Pair(context.Background(), PairInput{})
	if err != nil {
		t.Fatal(err)
	}
	base, frag, ok := strings.Cut(o.Link, "#")
	if !ok || base != PairBase {
		t.Fatalf("link = %q", o.Link)
	}
	q, err := url.ParseQuery(frag)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := svc.StaticKey()
	psk, _ := svc.OfferPSK(o.ID)
	k, _ := base64.RawURLEncoding.DecodeString(q.Get("k"))
	p, _ := base64.RawURLEncoding.DecodeString(q.Get("p"))
	if q.Get("v") != "1" || q.Get("a") != "api.trebi.ai" || q.Get("d") != "dev-1" || q.Get("c") != "trcl_claim" ||
		q.Get("o") != o.ID || q.Get("n") != "Fritz Mac" || !bytes.Equal(k, key.Public) || !bytes.Equal(p, psk) {
		t.Fatalf("fragment = %v", q)
	}
	if strings.ContainsAny(q.Get("k")+q.Get("p"), "=+/") {
		t.Fatal("keys are not base64url without padding")
	}
}

func newTestService(t *testing.T) (*Service, *fakeCloud, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cloud := &fakeCloud{target: Target{APIHost: "api.trebi.ai", DeviceID: "dev-1", Machine: "Fritz Mac", Linked: true, RemoteAccess: true, MobileOn: true}}
	return New(st, cloud), cloud, st
}

func busCount(t *testing.T, st *store.Store, topic string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic = ?`, topic).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func enroll(cloudID string) Enrollment {
	pub := bytes.Repeat([]byte{cloudID[len(cloudID)-1]}, 32)
	return Enrollment{CloudClientID: cloudID, Name: "Fritz's iPad", Platform: PlatformIOS, PublicKey: pub}
}
