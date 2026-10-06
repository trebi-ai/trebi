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

func TestTemplateAgentDescription(t *testing.T) {
	c := NewCatalog()
	for _, tpl := range c.List() {
		if tpl.AgentDescription == "" {
			t.Fatalf("%s: no agent_description", tpl.ID)
		}
		files, err := c.Render(RenderOpts{Template: tpl.ID, AgentID: "a"})
		if err != nil {
			t.Fatalf("%s: %v", tpl.ID, err)
		}
		for _, f := range files {
			if f.Rel == "AGENT.md" && !strings.Contains(string(f.Content), "description: \""+tpl.AgentDescription+"\"") {
				t.Fatalf("%s: AGENT.md = %q", tpl.ID, f.Content)
			}
		}
	}
	files, err := c.Render(RenderOpts{Template: "blank", AgentID: "cfo", Description: "Invoices: vendor payments."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(files[0].Content)+string(files[1].Content)+string(files[2].Content), `description: "Invoices: vendor payments."`) {
		t.Fatalf("override not rendered: %v", files)
	}
	if _, err := c.Render(RenderOpts{Template: "blank", AgentID: "cfo", Description: "a\nb"}); err == nil {
		t.Fatal("two-line description accepted")
	}
}
