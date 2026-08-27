package alerts_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestRaiseInbox(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := alerts.New(st)
	ctx := context.Background()

	id, err := svc.Raise(ctx, alerts.KindWaiting, "run/1", "Agent waiting", "approve?", "open attach", "run1", "ws", "job")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("empty id")
	}
	items, err := svc.ListInbox(ctx, "", "", 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("%v %v", items, err)
	}
	events, err := st.ListBus(ctx, 0, []string{"alert"}, 10)
	if err != nil || len(events) < 1 {
		t.Fatalf("bus: %v %v", events, err)
	}
}
