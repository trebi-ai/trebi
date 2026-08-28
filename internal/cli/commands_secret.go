package cli

import (
	"encoding/json"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func secretCommand() *cli.Command {
	return &cli.Command{
		Name:  "secret",
		Usage: "manage secrets",
		Subcommands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list secret names",
				Flags: []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var body struct {
						Secrets []string `json:"secrets"`
					}
					if err := client.GetJSON("/v1/secrets", &body); err != nil {
						return err
					}
					if c.Bool("json") {
						enc := json.NewEncoder(outWriter(c))
						enc.SetIndent("", "  ")
						return enc.Encode(body)
					}
					w := outWriter(c)
					tbl := newTable(w, []string{"Name"})
					for _, n := range body.Secrets {
						tbl.AppendRow(table.Row{n})
					}
					renderTable(tbl)
					return nil
				},
			},
			{
				Name:      "set",
				Usage:     "set secret value",
				ArgsUsage: "<name> <value>",
				Action: func(c *cli.Context) error {
					if c.NArg() < 2 {
						return fmt.Errorf("name and value required")
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.PutJSON("/v1/secrets/"+c.Args().Get(0), map[string]string{"value": c.Args().Get(1)}, nil)
				},
			},
			{
				Name:      "delete",
				Usage:     "delete secret",
				ArgsUsage: "<name>",
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.Delete("/v1/secrets/" + c.Args().Get(0))
				},
			},
		},
	}
}
