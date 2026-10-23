// Action resolution: the runs side of Part F.3. The consumer owns the seam,
// so runs declares the result type and the interface; connectors implements
// it.
package runs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// ConnectorAction is one connector's contribution to a run.
type ConnectorAction struct {
	ID       string // connection id; connectors_json stores it
	Slug     string
	Title    string
	MCP      *agentwire.MCPServer // nil for CLI-only actions
	Env      map[string]string    // connector env, secrets expanded
	BinDir   string               // installed binaries; first on PATH
	Skill    string               // connector skill name, empty when none
	SkillDir string               // empty when the connector names no skill
	Commands []string             // for the prompt hint
	Hint     string               // full prompt text; replaces the command line
	RunToken bool                 // the MCP server is the Trebi proxy; it gets the run token
	Note     string               // run log line at launch
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
	// ErrConnectorLogin is a connection that needs a new login.
	ErrConnectorLogin = errors.New("connection needs a login")
)

// EmailSource finds the address an agent sends from. *email.Service
// satisfies it.
type EmailSource interface {
	AgentAddress(ctx context.Context, workspaceID, agent string) (string, error)
}

// WithEmail wires the address lookup for the built-in email action.
func (s *Service) WithEmail(e EmailSource) *Service {
	s.email = e
	return s
}

// emailAction is the built-in email connector of one launch. The env makes
// `trebi email send` find the workspace and the sender, so the prompt
// command needs neither flag. A job sends as its owner agent.
func (s *Service) emailAction(ctx context.Context, opts LaunchOpts) ConnectorAction {
	agent := strings.TrimPrefix(job.OwnerOf(opts.JobName), "agents/")
	if opts.Kind == KindAgent {
		agent = strings.TrimPrefix(opts.JobName, "agents/")
	}
	env := map[string]string{"TREBI_WORKSPACE": opts.WorkspaceID}
	from := "the workspace address"
	if agent != "" {
		env["TREBI_AGENT"] = agent
		from = "the " + agent + " agent address"
		if s.email != nil {
			if addr, err := s.email.AgentAddress(ctx, opts.WorkspaceID, agent); err == nil && addr != "" {
				from += " (" + addr + ")"
			}
		}
	}
	return ConnectorAction{
		ID: job.EmailConnector, Slug: job.EmailConnector, Title: "Email", Env: env,
		Hint: "Email: you send email from " + from + ". Send one message with:\n" +
			"  trebi email send --to <address> --subject \"<subject>\" --text \"<Markdown body>\"\n" +
			"Repeat --to for more recipients. Trebi renders the Markdown as the HTML part (see `trebi guide email-format`). " +
			"For a layout Markdown cannot give, use --html-file <file> instead of --text, never both; Trebi makes the plain-text part from the HTML. Add --attach <file> to attach a file (repeatable).\n" +
			"Trebi sends only to workspace admins and the email allowlist. If Trebi refuses a recipient, report it. Do not use a different mail tool.\n" +
			"When you answer a message, your final answer is the reply. Use this command only for a separate message.",
	}
}

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
		if a.Hint != "" {
			lines = append(lines, a.Hint)
			continue
		}
		if len(a.Commands) == 0 && a.Skill == "" {
			continue
		}
		name := a.Slug
		if a.Title != "" && a.Title != a.Slug {
			name += " (" + a.Title + ")"
		}
		switch {
		case len(a.Commands) > 0 && a.Skill != "":
			lines = append(lines, "Connector "+name+": use "+strings.Join(a.Commands, ", ")+" (skill "+a.Skill+").")
		case len(a.Commands) > 0:
			lines = append(lines, "Connector "+name+": use "+strings.Join(a.Commands, ", ")+".")
		default:
			lines = append(lines, "Connector "+name+": skill "+a.Skill+".")
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

// ConnectorLoginError is a launch that needs a connection with no login.
// It matches ErrConnectorLogin.
type ConnectorLoginError struct {
	Connector string // the connection id
	Title     string
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

func (e *ConnectorLoginError) Error() string { return "log in again to " + e.Title }

// Is matches ErrConnectorLogin.
func (e *ConnectorLoginError) Is(target error) bool { return target == ErrConnectorLogin }

// connectorNames is the connector list of a launch: the kit list, then the
// names the job adds.
func connectorNames(jobList []string, kit *job.KitYAML) []string {
	var kitList []string
	if kit != nil {
		kitList = kit.Connectors
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range slices.Concat(kitList, jobList) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
