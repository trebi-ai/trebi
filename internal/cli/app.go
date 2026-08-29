// Package cli is the trebi command surface (API client + local service commands).
package cli

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/platform"
	"github.com/urfave/cli/v2"
)

const appStateKey = "trebi.cli.state"

// NewApp builds the root CLI.
func NewApp() *cli.App {
	app := &cli.App{
		Name:    "trebi",
		Usage:   "Trebi — LLM task runner (daemon + CLI)",
		Version: "0.1.0",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "workspace",
				Aliases: []string{"w"},
				EnvVars: []string{"TREBI_WORKSPACE"},
				Usage:   "workspace id/name (sticky context if omitted; remote links route the API)",
			},
		},
		Before: func(c *cli.Context) error {
			if c.App.Metadata == nil {
				c.App.Metadata = map[string]any{}
			}
			c.App.Metadata[appStateKey] = newAppState(c.String("workspace"))
			return nil
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
	sort.Sort(cli.CommandsByName(cmds))
	for _, c := range cmds {
		if c != nil && len(c.Subcommands) > 0 {
			sortCommands(c.Subcommands)
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

func appStateFor(c *cli.Context) *appState {
	if c != nil && c.App != nil && c.App.Metadata != nil {
		if state, ok := c.App.Metadata[appStateKey].(*appState); ok && state != nil {
			return state
		}
	}
	if c != nil {
		return newAppState(c.String("workspace"))
	}
	return newAppState("")
}

func clientFor(c *cli.Context) (*APIClient, error) {
	state := appStateFor(c)
	if state.clientErr != nil {
		return nil, state.clientErr
	}
	if state.client == nil {
		return nil, fmt.Errorf("API client is not configured")
	}
	return state.client, nil
}

func localClientFor(c *cli.Context) (*APIClient, error) {
	state := appStateFor(c)
	if state.localClientErr != nil {
		return nil, state.localClientErr
	}
	if state.localClient == nil {
		return nil, fmt.Errorf("local API client is not configured")
	}
	return state.localClient, nil
}

func configFor(c *cli.Context) (*config.Config, error) {
	state := appStateFor(c)
	if state.configErr != nil {
		return nil, state.configErr
	}
	if state.config == nil {
		return nil, fmt.Errorf("config is not loaded")
	}
	return state.config, nil
}

func workspaceFor(c *cli.Context) string {
	return appStateFor(c).workspace
}

func stickyFor(c *cli.Context) (*StickyContext, error) {
	state := appStateFor(c)
	return state.sticky, state.stickyErr
}

func linkedFor(c *cli.Context) ([]LinkedWorkspace, error) {
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

func outWriter(c *cli.Context) io.Writer {
	if c != nil && c.App != nil && c.App.Writer != nil {
		return c.App.Writer
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
