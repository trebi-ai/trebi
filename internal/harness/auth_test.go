package harness

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
)

func TestStampFlipOnly(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()
	svc := New(st, nil, Options{})

	res, err := svc.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthOK}, SourceLaunch)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Flipped || !res.OK {
		t.Fatalf("%+v", res)
	}
	res, err = svc.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthOK}, SourceLaunch)
	if err != nil {
		t.Fatal(err)
	}
	if res.Flipped {
		t.Fatal("second ok must not flip")
	}

	n := countTopic(t, st, "harness.updated")
	if n != 1 {
		t.Fatalf("harness.updated count=%d want 1", n)
	}

	res, err = svc.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthMissing}, SourceProbe)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Flipped || res.OK {
		t.Fatalf("1→0 %+v", res)
	}
	if countTopic(t, st, "harness.updated") != 2 {
		t.Fatal("expected second harness.updated on 1→0")
	}
}

func TestLocalOnlyDetails(t *testing.T) {
	t.Parallel()
	d := LocalOnlyDetails("cursor", AuthCaps{LoginModes: []string{LoginToken}, LocalCmd: "cursor-agent login"})
	if d["local_cmd"] != "cursor-agent login" {
		t.Fatalf("%v", d)
	}
	if d["hint"] == nil || d["hint"] == "" {
		t.Fatal("hint")
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func countTopic(t *testing.T, st *store.Store, topic string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic=?`, topic).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEarliestExpiry(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()
	svc := New(st, nil, Options{})
	soon := time.Now().UTC().Add(200 * time.Millisecond)
	_, err := svc.Stamp(ctx, AuthStatus{Tool: "claude", Status: AuthOK, ExpiresAt: &soon}, SourceProbe)
	if err != nil {
		t.Fatal(err)
	}
	name, at, err := svc.EarliestExpiry(ctx)
	if err != nil || name != "claude" || at.IsZero() {
		t.Fatalf("%s %v %v", name, at, err)
	}
}
