package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestRouteWaitRaisesInbox(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := &ControlPlane{Alerts: alerts.New(st, alerts.Options{})}
	id, err := c.raiseRouteWait(context.Background(), connectors.RouteWait{
		Workspace: "ws_1", Connection: "gmail", ConnectionTitle: "Gmail", EventID: "evt_1", DecisionID: "dec_1", Summary: "From ann: Invoice",
		Choices: []connectors.RouteWaitChoice{
			{RouteID: "rt_a", Target: "sling-cs", TargetKind: "agent", Share: 0.5},
			{RouteID: "rt_b", Target: "sling-cmo/post", TargetKind: "job", Share: 0.3},
		},
	})
	if err != nil || id == "" {
		t.Fatalf("raise = %q, %v", id, err)
	}
	it, err := c.Alerts.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if it.Kind != alerts.InboxApproval || it.AlertKind != "route_wait" || it.EventID != "evt_1" || it.DecisionID != "dec_1" ||
		it.Workspace != "ws_1" || it.Connector != "gmail" || it.Title != "Choose who gets a Gmail message" {
		t.Fatalf("item %+v", it)
	}
	labels := make([]string, len(it.Actions))
	for i, a := range it.Actions {
		labels[i] = a.Label
	}
	if got := strings.Join(labels, "|"); got != "Wake sling-cs|Run sling-cmo/post|Ignore|Improve routing…" {
		t.Fatalf("labels %s", got)
	}
	var body map[string]any
	if err := json.Unmarshal(it.Actions[2].Body, &body); err != nil || body["ignore"] != true || body["to"] != "nobody" {
		t.Fatalf("ignore body %s", it.Actions[2].Body)
	}
	if it.Actions[0].Route != "/v1/events/evt_1/reroute" || it.Actions[3].Kind != alerts.ActionRouteReview {
		t.Fatalf("actions %+v", it.Actions)
	}
}
