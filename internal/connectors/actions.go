package connectors

import (
	"context"
	"fmt"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/secrets"
)

// ResolveActions resolves connector names into the actions a run receives
// (Part F.3). An unknown name, an inherited name, or a connector with no
// actions block is an error; runs turns it into ErrActionsInvalid.
func (s *Service) ResolveActions(ctx context.Context, names []string) ([]runs.ConnectorAction, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]runs.ConnectorAction, 0, len(names))
	for _, name := range names {
		d, ok := s.Definition(name)
		if !ok {
			if row, err := s.Get(name); err == nil {
				if fmt.Sprint(row["provenance"]) == "inherited" {
					return nil, fmt.Errorf("connector %q is an inherited MCP server; the harness bridge already gives it to the run", name)
				}
				return nil, fmt.Errorf("connector %q has no definition", name)
			}
			return nil, fmt.Errorf("connector %q not found", name)
		}
		if d.Actions == nil {
			return nil, fmt.Errorf("connector %q has no actions block", name)
		}
		act := runs.ConnectorAction{Name: name}
		if m := d.Actions.MCP; m != nil {
			merged := make(map[string]string, len(d.Env)+len(m.Env))
			for k, v := range d.Env {
				merged[k] = v
			}
			for k, v := range m.Env {
				merged[k] = v
			}
			env, missing := secrets.ExpandRefs(merged)
			s.logMissingSecrets(name, missing)
			headers, missing := secrets.ExpandRefs(m.Headers)
			s.logMissingSecrets(name, missing)
			act.MCP = &agentwire.MCPServer{
				Name:    name,
				Command: m.Command,
				Args:    m.Args,
				Env:     env,
				URL:     m.URL,
				Headers: headers,
			}
		}
		if c := d.Actions.CLI; c != nil {
			env, missing := secrets.ExpandRefs(d.Env)
			s.logMissingSecrets(name, missing)
			act.Env = env
			act.Commands = c.Commands
			act.Skill = c.Skill
			if c.Skill != "" {
				if s.sk == nil {
					return nil, fmt.Errorf("connector %q skill %q: skills service unavailable", name, c.Skill)
				}
				if _, err := s.sk.Get(ctx, c.Skill, false); err != nil {
					return nil, fmt.Errorf("connector %q skill %q: %w", name, c.Skill, err)
				}
				act.SkillDir = s.sk.Dir(c.Skill)
			}
		}
		out = append(out, act)
	}
	return out, nil
}

func (s *Service) logMissingSecrets(connector string, missing []string) {
	for _, name := range missing {
		s.log.Info("secret.referenced_missing", "name", name, "connector", connector)
	}
}
