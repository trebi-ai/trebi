package alerts_test

import (
	"context"
	"testing"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestSkillCreatedInbox(t *testing.T) {
	t.Parallel()
	svc, st := testSvc(t, alerts.Options{})
	ctx := context.Background()
	appendEv := func(topic, id string, attrs map[string]string) {
		t.Helper()
		if _, err := st.Append(ctx, store.BusEvent{Topic: topic, Entity: "skill", EntityID: id, Action: "x", Attrs: attrs}); err != nil {
			t.Fatal(err)
		}
	}
	appendEv("skill.created", "agent/A1/deploy", map[string]string{"scope": "agent", "status": "pending", "actor_kind": "agent", "job": "support", "note": "share it"})
	appendEv("skill.created", "job/J1/report", map[string]string{"scope": "job", "status": "active", "actor_kind": "job", "job": "daily"})
	appendEv("skill.created", "global/mine", map[string]string{"scope": "global", "status": "active", "actor_kind": "user"})
	svc.Consume(ctx)

	got := skillItems(t, svc)
	if len(got) != 2 {
		t.Fatalf("items %v", got)
	}
	dep := got["skill/agent/A1/deploy"]
	if dep.Title != "support made a new skill: deploy" || len(dep.Actions) != 3 || dep.Actions[2].Kind != alerts.ActionApprove ||
		dep.Actions[2].Route != "/v1/skills/deploy/approve?scope=agent&owner=A1" || dep.Actions[0].Route != "/skills/deploy?scope=agent&owner=A1&dialog=scope" {
		t.Fatalf("pending item %+v", dep)
	}
	if rep := got["skill/job/J1/report"]; len(rep.Actions) != 2 {
		t.Fatalf("active item %+v", rep)
	}

	appendEv("skill.updated", "agent/A1/deploy", map[string]string{"status": "active"})
	appendEv("skill.moved", "workspace/W1/report", map[string]string{"from": "job/J1/report", "to": "workspace/W1/report"})
	svc.Consume(ctx)
	if got := skillItems(t, svc); len(got) != 0 {
		t.Fatalf("approve and move must close the items: %v", got)
	}
}

func skillItems(t *testing.T, svc *alerts.Service) map[string]alerts.InboxItem {
	t.Helper()
	items, err := listAll(context.Background(), svc, alerts.ListOpts{InboxFilter: alerts.InboxFilter{View: alerts.ViewInbox, Kind: alerts.InboxApproval}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]alerts.InboxItem{}
	for _, it := range items {
		out[it.Subject] = it
	}
	return out
}
