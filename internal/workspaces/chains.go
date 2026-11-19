package workspaces

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"gopkg.in/yaml.v3"
)

// ChainLimits are the limits of one agent chain (plan 2026-10-06 D.1).
// MaxUSD 0 is no cost limit.
type ChainLimits struct {
	MaxHops     int     `json:"max_hops" yaml:"max_hops"`
	MaxTurns    int     `json:"max_turns" yaml:"max_turns"`
	MaxUSD      float64 `json:"max_usd" yaml:"max_usd"`
	PairPerHour int     `json:"pair_per_hour" yaml:"pair_per_hour"`
}

// DefaultChainLimits are the limits when trebi.yaml has no agents.chains key.
func DefaultChainLimits() ChainLimits {
	return ChainLimits{MaxHops: 5, MaxTurns: 20, PairPerHour: 10}
}

// ChainConfig is the resolved agents.chains block on the workspace wire.
type ChainConfig struct {
	Enabled bool `json:"enabled"`
	ChainLimits
}

// ChainPatch is a partial agents.chains update. Nil fields stay.
type ChainPatch struct {
	Enabled     *bool    `json:"enabled"`
	MaxHops     *int     `json:"max_hops"`
	MaxTurns    *int     `json:"max_turns"`
	MaxUSD      *float64 `json:"max_usd"`
	PairPerHour *int     `json:"pair_per_hour"`
}

// trebiYAMLAgents is the agents block of trebi.yaml.
type trebiYAMLAgents struct {
	Chains *chainYAML `yaml:"chains,omitempty"`
}

// chainYAML keeps unset keys as nil, so a file holds only what a person set.
type chainYAML struct {
	Enabled     *bool    `yaml:"enabled,omitempty"`
	MaxHops     *int     `yaml:"max_hops,omitempty"`
	MaxTurns    *int     `yaml:"max_turns,omitempty"`
	MaxUSD      *float64 `yaml:"max_usd,omitempty"`
	PairPerHour *int     `yaml:"pair_per_hour,omitempty"`
}

// config resolves the block with the defaults.
func (c *chainYAML) config() ChainConfig {
	out := ChainConfig{Enabled: true, ChainLimits: DefaultChainLimits()}
	if c == nil {
		return out
	}
	if c.Enabled != nil {
		out.Enabled = *c.Enabled
	}
	if c.MaxHops != nil {
		out.MaxHops = *c.MaxHops
	}
	if c.MaxTurns != nil {
		out.MaxTurns = *c.MaxTurns
	}
	if c.MaxUSD != nil {
		out.MaxUSD = *c.MaxUSD
	}
	if c.PairPerHour != nil {
		out.PairPerHour = *c.PairPerHour
	}
	return out
}

// ErrChainLimits marks an agents.chains value out of range.
var ErrChainLimits = errors.New("agents.chains")

// Validate checks the value ranges of D.1.
func (l ChainLimits) Validate() error {
	switch {
	case l.MaxHops < 1 || l.MaxHops > 20:
		return fmt.Errorf("%w.max_hops must be 1 to 20", ErrChainLimits)
	case l.MaxTurns < 1 || l.MaxTurns > 500:
		return fmt.Errorf("%w.max_turns must be 1 to 500", ErrChainLimits)
	case l.MaxUSD < 0:
		return fmt.Errorf("%w.max_usd must not be negative", ErrChainLimits)
	case l.PairPerHour < 1 || l.PairPerHour > 1000:
		return fmt.Errorf("%w.pair_per_hour must be 1 to 1000", ErrChainLimits)
	}
	return nil
}

// apply merges p into the block and validates the result.
func (y *trebiYAML) applyChains(p *ChainPatch) error {
	if p == nil {
		return nil
	}
	if y.Agents == nil {
		y.Agents = &trebiYAMLAgents{}
	}
	if y.Agents.Chains == nil {
		y.Agents.Chains = &chainYAML{}
	}
	c := y.Agents.Chains
	if p.Enabled != nil {
		c.Enabled = p.Enabled
	}
	if p.MaxHops != nil {
		c.MaxHops = p.MaxHops
	}
	if p.MaxTurns != nil {
		c.MaxTurns = p.MaxTurns
	}
	if p.MaxUSD != nil {
		c.MaxUSD = p.MaxUSD
	}
	if p.PairPerHour != nil {
		c.PairPerHour = p.PairPerHour
	}
	return c.config().Validate()
}

// loadTrebiYAML parses the trebi.yaml of a workspace folder. A missing file
// is an empty config.
func loadTrebiYAML(dir string) (*trebiYAML, error) {
	data, err := os.ReadFile(filepath.Join(dir, "trebi.yaml"))
	if os.IsNotExist(err) {
		return &trebiYAML{}, nil
	}
	if err != nil {
		return nil, err
	}
	var y trebiYAML
	if err := yaml.Unmarshal(data, &y); err != nil {
		return nil, fmt.Errorf("trebi.yaml: %w", err)
	}
	return &y, nil
}

// chainConfig reads agents.chains of one workspace with the defaults.
func (s *Service) chainConfig(ws *Workspace) (ChainConfig, error) {
	y, err := loadTrebiYAML(ws.Path)
	if err != nil {
		return ChainConfig{}, err
	}
	if y.Agents == nil {
		return (*chainYAML)(nil).config(), nil
	}
	return y.Agents.Chains.config(), nil
}

// ChainLimits returns the chain limits of a workspace and whether agents of
// the workspace can ask each other.
func (s *Service) ChainLimits(ctx context.Context, wsID string) (ChainLimits, bool, error) {
	ws, err := s.Get(ctx, wsID)
	if err != nil {
		return ChainLimits{}, false, err
	}
	c, err := s.chainConfig(ws)
	if err != nil {
		return ChainLimits{}, false, err
	}
	return c.ChainLimits, c.Enabled, nil
}

// TeamAgent is one agent as other agents see it: the fields an ask checks.
type TeamAgent struct {
	ID          string
	Name        string // short name
	Description string
	Team        *job.TeamYAML
	Disabled    bool
	Archived    bool
	ParseError  string
}

// Available reports whether the agent can take an ask.
func (a TeamAgent) Available() bool {
	return !a.Disabled && !a.Archived && a.ParseError == "" && a.Team.InChains()
}

// TeamAgents lists the agents of a workspace, parsed from AGENT.md.
func (s *Service) TeamAgents(ctx context.Context, wsID string) ([]TeamAgent, error) {
	rows, err := s.ListJobs(ctx, wsID)
	if err != nil {
		return nil, err
	}
	return teamAgents(rows), nil
}

func teamAgents(rows []JobRow) []TeamAgent {
	out := make([]TeamAgent, 0, 4)
	for _, row := range rows {
		if row.Kind != "agent" {
			continue
		}
		a := TeamAgent{ID: row.ID, Name: row.Name, ParseError: row.ParseError, Disabled: !row.Active, Archived: row.ArchivedAt != ""}
		if j, err := job.ParseFile(row.Path); err == nil && j != nil {
			a.Description = j.Description
			a.Team = j.Team
			a.Disabled = j.Disabled
			a.ParseError = job.JoinParseError(row.ParseError, j.ParseError)
		} else if err != nil {
			a.ParseError = job.JoinParseError(row.ParseError, err.Error())
		}
		out = append(out, a)
	}
	return out
}

// teamCap bounds the team list of a wake.
const teamCap = 30

// teamSection renders the "Your team" wake section for the agent self, or ""
// when self takes no part in chains.
func teamSection(team []TeamAgent, self string) string {
	var me *TeamAgent
	for i := range team {
		if team[i].Name == self {
			me = &team[i]
		}
	}
	if me == nil || !me.Team.InChains() {
		return ""
	}
	var lines []string
	for _, a := range team {
		if a.Name == self || !a.Available() || !me.Team.Allows(a.Name) {
			continue
		}
		lines = append(lines, "- "+a.Name+": "+strings.TrimSpace(a.Description))
	}
	if len(lines) == 0 {
		return ""
	}
	cut := len(lines) > teamCap
	if cut {
		lines = lines[:teamCap]
	}
	var b strings.Builder
	b.WriteString("Other agents in this workspace. Ask one when the question is in its area.\n\n")
	b.WriteString(strings.Join(lines, "\n"))
	if cut {
		b.WriteString("\nRun `trebi agent list --for <words>` to find more.")
	}
	b.WriteString("\n\nTo ask: `trebi agent ask <name> \"<question>\" --context \"<facts they need>\"`. Then end your turn. The reply starts your next turn.\n")
	b.WriteString("To pass work on with no reply: `trebi agent handoff <name> \"<task>\" --context \"<facts>\"`.")
	return b.String()
}

// chainLines tells an agent inside a chain how far the chain is and which
// agents wait for it. "" outside a chain.
func chainLines(c *runs.WakeChain) string {
	if c == nil {
		return ""
	}
	line := fmt.Sprintf("This work is part of a chain: depth %d of %d, %d of %d turns used.", c.Depth, c.MaxHops, c.Turns, c.MaxTurns)
	if len(c.Waiting) > 0 {
		line += "\nDo not ask: " + strings.Join(c.Waiting, ", ") + ". They wait for you. Reply to them instead."
	}
	return line
}
