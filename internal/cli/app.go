// Package cli is the trebi command surface (API client + local service commands).
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/platform"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

const appStateKey = "trebi.cli.state"

// NewApp builds the root CLI.
func NewApp() *cli.Command {
	app := &cli.Command{
		Name:    "trebi",
		Usage:   "Trebi — LLM task runner (daemon + CLI)",
		Version: "0.1.0",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "workspace",
				Aliases: []string{"w"},
				Sources: cli.EnvVars("TREBI_WORKSPACE"),
				Usage:   "workspace id/name (sticky context if omitted; remote links route the API)",
			},
		},
		Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
			if c.Root().Metadata == nil {
				c.Root().Metadata = map[string]any{}
			}
			c.Root().Metadata[appStateKey] = newAppState(c.String("workspace"))
			return ctx, nil
		},
		Commands: []*cli.Command{
			serviceCommand(),
			workspaceCommand(),
			jobCommand(),
			runsCommand(),
			doctorCommand(),
			busCommand(),
			configCommand(),
			assistantCommand(),
			integrationCommand(),
			harnessCommand(),
			skillCommand(),
			secretCommand(),
			inboxCommand(),
			eventsCommand(),
			fileCommand(),
			conversationCommand(),
			alertsCommand(),
			agentHookCommand(),
			queueCommand(),
			rulesCommand(),
		},
	}
	sortCommands(app.Commands)
	return app
}

// sortCommands orders help subcommands alphabetically at every nesting level.
func sortCommands(cmds []*cli.Command) {
	slices.SortFunc(cmds, func(a, b *cli.Command) int {
		an, bn := "", ""
		if a != nil {
			an = a.Name
		}
		if b != nil {
			bn = b.Name
		}
		return strings.Compare(an, bn)
	})
	for _, c := range cmds {
		if c != nil && len(c.Commands) > 0 {
			sortCommands(c.Commands)
		}
	}
}

// appState is the immutable startup context shared by all command actions.
type appState struct {
	workspace string
	sticky    *StickyContext
	stickyErr error
	linked    []LinkedWorkspace
	linkedErr error

	client         *APIClient
	clientErr      error
	localClient    *APIClient
	localClientErr error
	config         *config.Config
	configErr      error
	platform       *platform.Service
	log            *slog.Logger
}

func newAppState(workspaceHint string) *appState {
	workspaceHint = strings.TrimSpace(workspaceHint)
	sticky, stickyErr := LoadStickyContext()
	linked, linkedErr := LoadLinkedWorkspaces()
	state := &appState{
		workspace: workspaceHint,
		sticky:    sticky,
		stickyErr: stickyErr,
		linked:    linked,
		linkedErr: linkedErr,
		platform:  platform.NewService(""),
		log:       slog.Default().With("source", "cli"),
	}
	if stickyErr != nil {
		state.clientErr = stickyErr
	} else {
		state.client, state.clientErr = loadAPIClientWithContext(
			workspaceHint, sticky, linked, os.Getenv("TREBI_URL"), os.Getenv("TREBI_TOKEN"),
		)
	}
	state.localClient, state.localClientErr = loadLocalAPIClientWithEnv(
		os.Getenv("TREBI_URL"), os.Getenv("TREBI_TOKEN"),
	)
	state.config, state.configErr = config.Load()
	state.workspace = effectiveWorkspaceFor(workspaceHint, sticky, linked)
	return state
}

func appStateFor(c *cli.Command) *appState {
	if c != nil {
		if state, ok := c.Root().Metadata[appStateKey].(*appState); ok && state != nil {
			return state
		}
		return newAppState(c.String("workspace"))
	}
	return newAppState("")
}

func clientFor(c *cli.Command) (*APIClient, error) {
	state := appStateFor(c)
	if state.clientErr != nil {
		return nil, state.clientErr
	}
	if state.client == nil {
		return nil, fmt.Errorf("API client is not configured")
	}
	return state.client, nil
}

func localClientFor(c *cli.Command) (*APIClient, error) {
	state := appStateFor(c)
	if state.localClientErr != nil {
		return nil, state.localClientErr
	}
	if state.localClient == nil {
		return nil, fmt.Errorf("local API client is not configured")
	}
	return state.localClient, nil
}

func configFor(c *cli.Command) (*config.Config, error) {
	state := appStateFor(c)
	if state.configErr != nil {
		return nil, state.configErr
	}
	if state.config == nil {
		return nil, fmt.Errorf("config is not loaded")
	}
	return state.config, nil
}

func workspaceFor(c *cli.Command) string {
	return resolveWorkspaceHint(c, appStateFor(c).workspace)
}

// workspaceFlag is the command --workspace / -w if set, else the global/sticky workspace.
func workspaceFlag(c *cli.Command) string {
	if w := strings.TrimSpace(c.String("workspace")); w != "" {
		return resolveWorkspaceHint(c, w)
	}
	return workspaceFor(c)
}

// resolveWorkspaceHint maps a name or id to a workspace id via sticky/linked
// context, then the daemon's workspace list.
func resolveWorkspaceHint(c *cli.Command, hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return ""
	}
	state := appStateFor(c)
	resolved := effectiveWorkspaceFor(hint, state.sticky, state.linked)
	client, err := clientFor(c)
	if err != nil {
		return resolved
	}
	if id := lookupWorkspaceID(client, resolved); id != "" {
		return id
	}
	return resolved
}

func lookupWorkspaceID(client *APIClient, hint string) string {
	if client == nil || hint == "" {
		return ""
	}
	var body struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := client.GetJSON(request.BuildWorkspacesListPath(), &body); err != nil {
		return ""
	}
	for _, ws := range body.Workspaces {
		id := mapStr(ws, "id")
		name := mapStr(ws, "name")
		if id == hint || equalFoldName(hint, name) {
			return id
		}
	}
	return ""
}

func stickyFor(c *cli.Command) (*StickyContext, error) {
	state := appStateFor(c)
	return state.sticky, state.stickyErr
}

func linkedFor(c *cli.Command) ([]LinkedWorkspace, error) {
	state := appStateFor(c)
	return state.linked, state.linkedErr
}

func loadAPIClientWithContext(hint string, sticky *StickyContext, linked []LinkedWorkspace, url, token string) (*APIClient, error) {
	if link, found := findLinked(linked, hint); found && hint != "" {
		return remoteClient(link.URL, link.Token), nil
	}
	if sticky != nil && sticky.IsRemote() {
		if hint == "" || hint == sticky.WorkspaceID || equalFoldName(hint, sticky.Name) {
			return remoteClient(sticky.URL, sticky.Token), nil
		}
		if link, found := findLinked(linked, hint); found {
			return remoteClient(link.URL, link.Token), nil
		}
	}
	if url != "" {
		return &APIClient{
			Base:  strings.TrimRight(url, "/"),
			Token: token,
			HTTP:  &http.Client{Timeout: 30 * time.Second},
		}, nil
	}
	return apiJSONClient()
}

func loadLocalAPIClientWithEnv(url, token string) (*APIClient, error) {
	if url != "" {
		return &APIClient{
			Base:  strings.TrimRight(url, "/"),
			Token: token,
			HTTP:  &http.Client{Timeout: 30 * time.Second},
		}, nil
	}
	return apiJSONClient()
}

func effectiveWorkspaceFor(hint string, sticky *StickyContext, linked []LinkedWorkspace) string {
	hint = strings.TrimSpace(hint)
	if hint != "" {
		if link, ok := findLinked(linked, hint); ok {
			return link.ID
		}
		if sticky != nil && equalFoldName(hint, sticky.Name) {
			return sticky.WorkspaceID
		}
		return hint
	}
	if sticky != nil {
		return sticky.WorkspaceID
	}
	return ""
}

func findLinked(all []LinkedWorkspace, idOrName string) (LinkedWorkspace, bool) {
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		return LinkedWorkspace{}, false
	}
	lower := strings.ToLower(idOrName)
	for _, w := range all {
		if w.ID == idOrName {
			return w, true
		}
		if w.Name != "" && strings.ToLower(w.Name) == lower {
			return w, true
		}
	}
	return LinkedWorkspace{}, false
}

func outWriter(c *cli.Command) io.Writer {
	if c != nil {
		if w := c.Root().Writer; w != nil {
			return w
		}
	}
	return os.Stdout
}

func readFileOrStdin(path string) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
