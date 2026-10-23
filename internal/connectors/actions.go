package connectors

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/connectors/oauth"
	"github.com/trebi-ai/trebi/internal/runs"
)

// ResolveActions resolves connection slugs or ids into the actions a run
// receives, with the layer of the workspace. An unknown name, an inherited
// name, a disabled name, or a connector with no actions block is an error;
// runs turns it into ErrActionsInvalid. A CLI action gets the state and
// cache folders of its connection (D10), so one run can use the CLI actions
// of one connection per type only.
func (s *Service) ResolveActions(ctx context.Context, workspaceID string, names []string) ([]runs.ConnectorAction, error) {
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
		if err := s.mcpAction(ctx, name, r, &act); err != nil {
			return nil, err
		}
		if err := s.cliAction(name, r, &act, cliTypes); err != nil {
			return nil, err
		}
		out = append(out, act)
	}
	return out, nil
}

// cliAction sets the env, the commands, and the skill of a CLI action.
// cliTypes maps a connector type to the slug that already uses it.
func (s *Service) cliAction(name string, r Resolved, act *runs.ConnectorAction, cliTypes map[string]string) error {
	if r.Manifest.Actions.CLI == nil {
		return nil
	}
	if other, dup := cliTypes[r.Type]; dup && other != r.Slug {
		return fmt.Errorf("connectors %q and %q are both %s connections; a run can use the CLI actions of one %s connection only", other, r.Slug, r.Type, r.Type)
	}
	cliTypes[r.Type] = r.Slug
	env, missing := r.Env()
	s.logMissingSecrets(name, missing)
	env["TREBI_STATE_DIR"] = r.StateDir()
	env["TREBI_CACHE_DIR"] = r.CacheDir()
	act.Env = env
	act.Commands = r.Manifest.Actions.CLI.Commands
	if dir := r.SkillDir(); dir != "" {
		act.Skill = r.Type
		act.SkillDir = dir
	}
	return nil
}

// mcpAction sets the MCP server of an action.
func (s *Service) mcpAction(ctx context.Context, name string, r Resolved, act *runs.ConnectorAction) error {
	if r.Manifest.Actions.MCP == nil {
		return nil
	}
	srv, missing, err := r.MCPServer()
	if err != nil {
		return err
	}
	s.logMissingSecrets(name, missing)
	if err := s.onlineMCP(ctx, r, srv, act); err != nil {
		return err
	}
	act.MCP = srv
	return nil
}

// onlineMCP points the MCP server of an online connection at the Trebi
// proxy, or in header mode adds the token. A connection with no login
// fails the launch. Other connections stay as they are.
func (s *Service) onlineMCP(ctx context.Context, r Resolved, srv *agentwire.MCPServer, act *runs.ConnectorAction) error {
	_, set, err := s.oauthConn(r)
	if errors.Is(err, ErrNoOAuth) {
		return nil
	}
	if err != nil {
		return err
	}
	tok, err := s.oauth.Token(ctx, r.ID)
	if errors.Is(err, oauth.ErrLoginRequired) {
		return fmt.Errorf("%w: log in again to %s", runs.ErrConnectorLogin, r.Title)
	}
	if err != nil {
		return fmt.Errorf("connector %q: get the token: %w", r.Slug, err)
	}
	if set.Mode == OAuthHeader {
		srv.Headers = maps.Clone(srv.Headers)
		if srv.Headers == nil {
			srv.Headers = map[string]string{}
		}
		srv.Headers["Authorization"] = "Bearer " + tok
		if st, err := s.oauth.Status(r.ID); err == nil && !st.Expiry.IsZero() {
			act.Note = fmt.Sprintf("The token of %s expires at %s. After that time, the run cannot use %s.", r.Title, st.Expiry.UTC().Format(time.RFC3339), r.Title)
		}
		return nil
	}
	s.mu.Lock()
	base := s.localURL
	s.mu.Unlock()
	if base == "" {
		return fmt.Errorf("connector %q: Trebi does not know its own address", r.Slug)
	}
	srv.URL = base + "/v1/connectors/" + url.PathEscape(r.ID) + "/mcp"
	srv.Headers = map[string]string{}
	act.RunToken = true
	return nil
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
