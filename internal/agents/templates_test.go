package agents

import (
	"strings"
	"testing"
)

func TestEmbeddedTemplatesRender(t *testing.T) {
	c := NewCatalog()
	list := c.List()
	if len(list) != 5 {
		t.Fatalf("templates = %d, want 5", len(list))
	}
	files, err := c.Render(RenderOpts{Template: "research", AgentID: "research", Title: "Research", Accept: []string{"admins"}, Inputs: map[string]string{"competitors": "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	byRel := map[string][]byte{}
	for _, f := range files {
		byRel[f.Rel] = f.Content
	}
	agent := string(byRel["AGENT.md"])
	if !strings.Contains(agent, "title: Research") || !strings.Contains(agent, `"admins"`) || strings.Contains(agent, "{{agent_title}}") {
		t.Fatalf("agent render = %q", agent)
	}
	job := string(byRel["weekly-brief/JOB.md"])
	if !strings.Contains(job, "disabled: true") {
		t.Fatalf("starter job not disabled: %q", job)
	}
}
