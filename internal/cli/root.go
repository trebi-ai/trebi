// Package cli is the trebi command surface (API client + local service commands).
package cli

import (
	"sort"

	"github.com/urfave/cli/v2"
)

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
