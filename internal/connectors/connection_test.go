package connectors_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"gopkg.in/yaml.v3"
)

// TestValueForms proves a Value reads a string and a list and writes one
// string for one item.
func TestValueForms(t *testing.T) {
	t.Parallel()
	var sub connectors.Subscription
	if err := yaml.Unmarshal([]byte("id: sub_x\nvalues: {team: ENG, resources: [Issue, Comment]}\n"), &sub); err != nil {
		t.Fatal(err)
	}
	want := map[string]connectors.Value{"team": {"ENG"}, "resources": {"Issue", "Comment"}}
	if !reflect.DeepEqual(sub.Values, want) {
		t.Fatalf("yaml values = %+v", sub.Values)
	}
	out, err := yaml.Marshal(sub)
	if err != nil || !strings.Contains(string(out), "team: ENG") {
		t.Fatalf("yaml = %s, %v", out, err)
	}
	b, err := json.Marshal(sub.Values)
	if err != nil || string(b) != `{"resources":["Issue","Comment"],"team":"ENG"}` {
		t.Fatalf("json = %s, %v", b, err)
	}
	var back map[string]connectors.Value
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, want) {
		t.Fatalf("json back = %+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"a":1}`), &back); err == nil {
		t.Fatal("a number decoded")
	}
}

// TestPatchMakesSubscriptionIDs proves a subscription with no id gets one
// and a bad id fails validation.
func TestPatchMakesSubscriptionIDs(t *testing.T) {
	c := connectors.Connection{ID: connectors.NewConnectionID(), Type: "x", Slug: "x", From: connectors.FromInline}
	c.Subscriptions = []connectors.Subscription{{ID: "bad"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "subscriptions[0].id") {
		t.Fatalf("bad id: %v", err)
	}
	if id := connectors.NewSubscriptionID(); !strings.HasPrefix(id, "sub_") || len(id) != 30 {
		t.Fatalf("id = %q", id)
	}
}
