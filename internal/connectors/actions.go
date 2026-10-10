package connectors

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/runs"
)

// ResolveActions resolves connection slugs or ids into the actions a run
// receives, with the layer of the workspace. An unknown name, an inherited
// name, a disabled name, or a connector with no actions block is an error;
// runs turns it into ErrActionsInvalid. A CLI action gets the state and
// cache folders of its connection (D10), so one run can use the CLI actions
// of one connection per type only.
func (s *Service) ResolveActions(_ context.Context, workspaceID string, names []string) ([]runs.ConnectorAction, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]runs.ConnectorAction, 0, len(names))
	cliTypes := map[string]string{}
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
		act := runs.ConnectorAction{ID: r.ID, Slug: r.Slug, Title: r.Title, BinDir: r.BinDir()}
		if a.MCP == nil && a.CLI == nil && len(a.Operations) > 0 {
			// The daemon runs the operations; the run gets no binary and no secret (D13).
			act.BinDir, act.Hint = "", operationsHint(r)
			if dir := r.SkillDir(); dir != "" {
				act.Skill, act.SkillDir = r.Type, dir
			}
			out = append(out, act)
			continue
		}
		if a.MCP != nil {
			srv, missing, err := r.MCPServer()
			if err != nil {
				return nil, err
			}
			s.logMissingSecrets(name, missing)
			act.MCP = srv
		}
		if a.CLI != nil {
			if other, dup := cliTypes[r.Type]; dup && other != r.Slug {
				return nil, fmt.Errorf("connectors %q and %q are both %s connections; a run can use the CLI actions of one %s connection only", other, r.Slug, r.Type, r.Type)
			}
			cliTypes[r.Type] = r.Slug
			env, missing := r.Env()
			s.logMissingSecrets(name, missing)
			env["TREBI_STATE_DIR"] = r.StateDir()
			env["TREBI_CACHE_DIR"] = r.CacheDir()
			act.Env = env
			act.Commands = a.CLI.Commands
			if dir := r.SkillDir(); dir != "" {
				act.Skill = r.Type
				act.SkillDir = dir
			}
		}
		out = append(out, act)
	}
	return out, nil
}

// operationsHint tells the agent how to call the operations of r.
func operationsHint(r Resolved) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Connection %s (%s): run its operations with `trebi connector call %s <operation> --input key=value`.", r.Slug, r.Title, r.Slug)
	for _, op := range r.Manifest.Operations() {
		fmt.Fprintf(&b, "\n- %s: %s", op.Name, op.Label)
		if op.Input != nil && len(op.Input.Properties) > 0 {
			keys := slices.Sorted(maps.Keys(op.Input.Properties))
			fmt.Fprintf(&b, " (input: %s)", strings.Join(keys, ", "))
		}
	}
	return b.String()
}

func (s *Service) logMissingSecrets(connector string, missing []string) {
	for _, name := range missing {
		s.log.Info("secret.referenced_missing", "name", name, "connector", connector)
	}
}
