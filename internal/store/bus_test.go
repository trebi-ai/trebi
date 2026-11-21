package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryUnique(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, e := range AllEvents() {
		if seen[e.Name] {
			t.Errorf("duplicate event name: %s", e.Name)
		}
		seen[e.Name] = true
		if !e.Log {
			t.Errorf("%s: Log must be true for registry entries", e.Name)
		}
		parts := strings.Split(e.Name, ".")
		if len(parts) < 2 || len(parts) > 4 {
			t.Errorf("%s: want 2–4 dotted segments, got %d", e.Name, len(parts))
		}
		for _, p := range parts {
			if p != strings.ToLower(p) || p == "" {
				t.Errorf("%s: segment %q invalid", e.Name, p)
			}
		}
	}
}

func TestBusTopicsSubsetOfRegistry(t *testing.T) {
	t.Parallel()
	set := eventSet()
	for _, topic := range BusTopics() {
		meta, ok := set[topic]
		if !ok {
			t.Errorf("bus topic %s not in registry", topic)
			continue
		}
		if !meta.Bus {
			t.Errorf("BusTopics returned %s but meta.Bus=false", topic)
		}
	}
}

func TestIsRegistered(t *testing.T) {
	t.Parallel()
	if !IsRegistered("run.created") {
		t.Fatal("run.created should be registered")
	}
	if !IsRegistered(TopicSystemReset) {
		t.Fatal("system.reset synthetic should be registered")
	}
	if !IsBusTopic("onboarding.updated") {
		t.Fatal("onboarding.updated is a bus topic")
	}
	if !IsBusTopic("system.notifications_requested") {
		t.Fatal("system.notifications_requested is a bus topic")
	}
	if !IsBusTopic("system.notifications_changed") {
		t.Fatal("system.notifications_changed is a bus topic")
	}
	if !IsBusTopic("event.routed") {
		t.Fatal("event.routed is a bus topic")
	}
	if IsRegistered("not.a.real.event") {
		t.Fatal("unknown event should not be registered")
	}
	if IsBusTopic(TopicSystemReset) {
		t.Fatal("system.reset must not be storeable")
	}
	if !IsBusTopic("run.failed") {
		t.Fatal("run.failed is a bus topic")
	}
}

func TestListBusExactTopic(t *testing.T) {
	t.Parallel()
	s, err := OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, topic := range []string{"a.b", "a.b.c"} {
		if _, err := s.DB().Exec(`INSERT INTO event_log (event_id, topic, ts) VALUES (?, ?, '2026-01-01T00:00:00Z')`, topic, topic); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	exact, err := s.ListBusWith(ctx, ListBusOpts{Topics: []string{"a.b"}, Exact: true})
	if err != nil || len(exact) != 1 || exact[0].Topic != "a.b" {
		t.Fatalf("exact: %+v %v", exact, err)
	}
	family, err := s.ListBusWith(ctx, ListBusOpts{Topics: []string{"a.b"}})
	if err != nil || len(family) != 2 {
		t.Fatalf("family: %+v %v", family, err)
	}
	rows, err := s.DB().Query(`EXPLAIN QUERY PLAN SELECT seq FROM event_log WHERE seq > ? AND topic IN (?) ORDER BY seq ASC LIMIT ?`, 0, "a.b", 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if !strings.Contains(plan.String(), "idx_bus_topic") {
		t.Fatalf("plan does not use idx_bus_topic:\n%s", plan.String())
	}
}

func TestWithBusTxWakes(t *testing.T) {
	t.Parallel()
	s, err := OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	ch := s.BusWake()
	if err := s.WithBusTx(ctx, func(*sql.Tx) error { return errors.New("boom") }); err == nil {
		t.Fatal("want error")
	}
	select {
	case <-ch:
		t.Fatal("woke after a failed tx")
	default:
	}
	if err := s.WithBusTx(ctx, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("no wake after commit")
	}
}
