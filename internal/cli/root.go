// Package cli is the trebi command surface (API client + local service commands).
package cli

import (
	"fmt"

	"github.com/urfave/cli/v2"
)

// NewApp builds the root CLI.
func NewApp() *cli.App {
	return &cli.App{
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
			runCommand(),
			listCommand(),
			scheduleCommand(),
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
			alertsCommand(),
			agentHookCommand(),
			{
				Name:  "version",
				Usage: "print version",
				Action: func(c *cli.Context) error {
					fmt.Println("trebi 0.1.0")
					return nil
				},
			},
		},
	}
}
