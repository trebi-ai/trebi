// Action resolution: the runs side of Part F.3. The consumer owns the seam,
// so runs declares the result type and the interface; connectors implements
// it.
package runs

import (
	"context"
	"errors"
	"strings"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// ConnectorAction is one connector's contribution to a run.
type ConnectorAction struct {
	Name     string
	MCP      *agentwire.MCPServer // nil for CLI-only actions
	Env      map[string]string    // connector env, secrets expanded
	Skill    string               // connector skill name, empty when none
	SkillDir string               // empty when the connector names no skill
	Commands []string             // for the prompt hint
}

// ActionResolver resolves connector names to run actions.
type ActionResolver interface {
	ResolveActions(ctx context.Context, names []string) ([]ConnectorAction, error)
}

var (
	// ErrActionsInvalid is a connector name that cannot supply actions.
	ErrActionsInvalid = errors.New("connector actions invalid")
	// ErrActionsUnsupported is a harness or runtime that cannot take an MCP action.
	ErrActionsUnsupported = errors.New("harness runtime cannot take MCP actions")
)

// WithActions wires the connector action resolver.
func (s *Service) WithActions(r ActionResolver) *Service {
	s.actions = r
	return s
}

// actionEnv merges the env of every resolved action. It is empty for an
// MCP-only connector, so MCP secrets never reach the shell of the run.
func actionEnv(actions []ConnectorAction) map[string]string {
	if len(actions) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, a := range actions {
		for k, v := range a.Env {
			out[k] = v
		}
	}
	return out
}

// mcpServers lists the MCP servers of the resolved actions.
func mcpServers(actions []ConnectorAction) []agentwire.MCPServer {
	var out []agentwire.MCPServer
	for _, a := range actions {
		if a.MCP != nil {
			out = append(out, *a.MCP)
		}
	}
	return out
}

// actionSkills lists the skill projections of the resolved actions.
func actionSkills(actions []ConnectorAction) []agentwire.Skill {
	var out []agentwire.Skill
	for _, a := range actions {
		if a.SkillDir != "" {
			out = append(out, agentwire.Skill{Name: a.Skill, Dir: a.SkillDir})
		}
	}
	return out
}

// connectorHint names every CLI action for the run prompt (Part F.4). It is
// added to every launch with actions, not only wake launches.
func connectorHint(actions []ConnectorAction) string {
	var lines []string
	for _, a := range actions {
		if len(a.Commands) == 0 && a.Skill == "" {
			continue
		}
		switch {
		case len(a.Commands) > 0 && a.Skill != "":
			lines = append(lines, "Connector "+a.Name+": use "+strings.Join(a.Commands, ", ")+" (skill "+a.Skill+").")
		case len(a.Commands) > 0:
			lines = append(lines, "Connector "+a.Name+": use "+strings.Join(a.Commands, ", ")+".")
		default:
			lines = append(lines, "Connector "+a.Name+": skill "+a.Skill+".")
		}
	}
	return strings.Join(lines, "\n")
}

// hasMCP reports whether any resolved action injects an MCP server.
func hasMCP(actions []ConnectorAction) bool {
	for _, a := range actions {
		if a.MCP != nil {
			return true
		}
	}
	return false
}

// supportsActions is the support matrix of decision 8. CLI actions (env plus
// skill) work everywhere; only MCP actions are gated. The pane answer is the
// MCPInjector test, so the launch path can not disagree with this check.
func supportsActions(tool tmux.Tool, cli bool) bool {
	if cli {
		switch tool.Name() {
		case tmux.Claude, tmux.Codex, tmux.OpenCode, tmux.Copilot, tmux.Cursor, tmux.Gemini:
			return true
		}
		return false
	}
	_, ok := tool.(tmux.MCPInjector)
	return ok
}
