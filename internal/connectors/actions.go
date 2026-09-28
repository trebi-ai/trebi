package connectors

import (
	"context"
	"errors"
	"fmt"

	"github.com/trebi-ai/trebi/internal/runs"
)

// ResolveActions resolves connector names into the actions a run receives,
// with the layers of the workspace. An unknown name, an inherited name, a
// disabled name, or a connector with no actions block is an error; runs
// turns it into ErrActionsInvalid.
func (s *Service) ResolveActions(_ context.Context, workspaceID string, names []string) ([]runs.ConnectorAction, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]runs.ConnectorAction, 0, len(names))
	for _, name := range names {
		r, err := s.Resolve(name, workspaceID)
		if errors.Is(err, ErrNotFound) {
			if row, gerr := s.Get(name); gerr == nil && fmt.Sprint(row["provenance"]) == "inherited" {
				return nil, fmt.Errorf("connector %q is an inherited MCP server; the harness bridge already gives it to the run", name)
			}
			return nil, fmt.Errorf("connector %q not found", name)
		}
		if err != nil {
			return nil, fmt.Errorf("connector %q: %w", name, err)
		}
		a := r.Manifest.Actions
		if a == nil {
			return nil, fmt.Errorf("connector %q has no actions block", name)
		}
		act := runs.ConnectorAction{Name: name, BinDir: r.BinDir()}
		if a.MCP != nil {
			srv, missing, err := r.MCPServer()
			if err != nil {
				return nil, err
			}
			s.logMissingSecrets(name, missing)
			act.MCP = srv
		}
		if a.CLI != nil {
			env, missing := r.Env()
			s.logMissingSecrets(name, missing)
			act.Env = env
			act.Commands = a.CLI.Commands
			if dir := r.SkillDir(); dir != "" {
				act.Skill = name
				act.SkillDir = dir
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
