package cli

import (
	"context"
	"fmt"

	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

// harnessVersionCommands are the update and keep commands of `trebi harness`.
func harnessVersionCommands() []*cli.Command {
	jsonFlag := &cli.BoolFlag{Name: "json", Usage: "emit JSON"}
	return []*cli.Command{
		{
			Name:      "update",
			Usage:     "update a harness program now",
			ArgsUsage: "<name>",
			Flags:     []cli.Flag{jsonFlag},
			Action: func(_ context.Context, c *cli.Command) error {
				return runHarnessVersion(c, request.BuildHarnessUpdate)
			},
		},
		{
			Name:      "keep",
			Usage:     "keep the current version (or --version) for Trebi runs",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "version", Usage: "the version to install and keep"},
				jsonFlag,
			},
			Action: func(_ context.Context, c *cli.Command) error {
				body := map[string]any{"updates": "keep"}
				if v := c.String("version"); v != "" {
					body["version"] = v
				}
				return runHarnessVersion(c, func(name string) request.HTTPRequest { return request.BuildHarnessPatch(name, body) })
			},
		},
		{
			Name:      "auto",
			Usage:     "let Trebi update a harness program automatically",
			ArgsUsage: "<name>",
			Flags:     []cli.Flag{jsonFlag},
			Action: func(_ context.Context, c *cli.Command) error {
				return runHarnessVersion(c, func(name string) request.HTTPRequest {
					return request.BuildHarnessPatch(name, map[string]any{"updates": "auto"})
				})
			},
		},
		{
			Name:  "versions",
			Usage: "harness program versions",
			Commands: []*cli.Command{{
				Name:      "check",
				Usage:     "read the installed and the latest version now",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{jsonFlag},
				Action: func(_ context.Context, c *cli.Command) error {
					return runHarnessVersion(c, request.BuildHarnessVersionsCheck)
				},
			}},
		},
	}
}

// runHarnessVersion sends one version request and prints the version part
// of the harness row.
func runHarnessVersion(c *cli.Command, build func(name string) request.HTTPRequest) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var row map[string]any
	if err := client.DoHTTP(build(name), &row); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, row)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "%s version=%v latest=%v updates=%v\n", name, row["version"], row["latest_version"], row["updates"])
	if truthy(row["updating"]) {
		fmt.Fprintln(w, "An update runs now.")
	}
	if lu, ok := row["last_update"].(map[string]any); ok {
		if truthy(lu["ok"]) {
			fmt.Fprintf(w, "last update: %v → %v ok at %v\n", lu["from"], lu["to"], lu["at"])
		} else {
			fmt.Fprintf(w, "last update: %v → %v failed at %v: %v\n", lu["from"], lu["to"], lu["at"], lu["error"])
		}
	}
	return nil
}
