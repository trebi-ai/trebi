// Action resolution: the runs side of Part F.3. The consumer owns the seam,
// so runs declares the result type and the interface; connectors implements
// it.
package runs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// ConnectorAction is one connector's contribution to a run.
type ConnectorAction struct {
	Name     string
	MCP      *agentwire.MCPServer // nil for CLI-only actions
	Env      map[string]string    // connector env, secrets expanded
	BinDir   string               // installed binaries; first on PATH
	Skill    string               // connector skill name, empty when none
	SkillDir string               // empty when the connector names no skill
	Commands []string             // for the prompt hint
}

// ActionResolver resolves connector names to run actions.
type ActionResolver interface {
	ResolveActions(ctx context.Context, workspaceID string, names []string) ([]ConnectorAction, error)
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
	var bins []string
	for _, a := range actions {
		for k, v := range a.Env {
			out[k] = v
		}
		if a.BinDir != "" {
			bins = append(bins, a.BinDir)
		}
	}
	if len(bins) > 0 {
		out["PATH"] = strings.Join(append(bins, os.Getenv("PATH")), string(os.PathListSeparator))
	}
	return out
}

// basePath is the PATH a run starts from: the action PATH, else the daemon
// PATH.
func basePath(actEnv map[string]string) string {
	if p := actEnv["PATH"]; p != "" {
		return p
	}
	return os.Getenv("PATH")
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

// ActionsSupported checks decision 8 for one harness on one runtime. CLI
// actions (env plus skill) work everywhere; only MCP actions are gated. The
// CLI runtime asks agent-wire how the harness takes MCP servers. The pane
// runtime needs the tool's MCPInjector, so the launch path can not disagree
// with this check.
func (s *Service) ActionsSupported(ctx context.Context, harness, runtime string, hasMCP bool) error {
	if !hasMCP {
		return nil
	}
	tool := tmux.Detect(harness)
	if runtime == "cli" {
		// pi takes MCP servers only in a per-run home, which trebi does not
		// give it (Part M).
		if tool.Name() != tmux.Pi {
			det, err := s.agentwireRuntime().Detect(ctx, agentwire.Harness(tool.Name()))
			if err == nil && det.Features.MCPInject != "" && det.Features.MCPInject != agentwire.MCPNone {
				return nil
			}
		}
	} else if _, ok := tool.(tmux.MCPInjector); ok {
		return nil
	}
	return fmt.Errorf("%w: %s on the %s runtime", ErrActionsUnsupported, harness, runtime)
}

// KitSource finds the kit of the agent that owns a job. *workspaces.Service
// satisfies it.
type KitSource interface {
	OwnerKit(ctx context.Context, workspaceID, name string) *job.KitYAML
}

// SetKitSource wires the owner kit lookup for EffectiveActions.
func (s *Service) SetKitSource(k KitSource) {
	s.kits = k
}

// EffectiveActions returns the connector names, harness, and runtime that a
// launch of this job uses. j.Name is the job index name.
func (s *Service) EffectiveActions(ctx context.Context, ws string, j job.Job) (names []string, harness, runtime string, err error) {
	var kit *job.KitYAML
	if s.kits != nil {
		kit = s.kits.OwnerKit(ctx, ws, j.Name)
	}
	harness = j.ResolvedHarness()
	runtime = "tui"
	if s.wantsCLI(tmux.Detect(harness), LaunchOpts{CreateOpts: CreateOpts{Runtime: j.Runtime}}) {
		runtime = "cli"
	}
	return connectorNames(j.Connectors, kit), harness, runtime, nil
}

// connectorNames is the connector list of a launch: the job list replaces
// the kit list when it is not empty.
func connectorNames(jobList []string, kit *job.KitYAML) []string {
	if len(jobList) == 0 && kit != nil {
		return kit.Connectors
	}
	return jobList
}
