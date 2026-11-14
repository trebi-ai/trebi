package store

import (
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
