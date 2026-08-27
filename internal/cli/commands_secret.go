package cli

import (
	"fmt"

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
					for _, n := range body.Secrets {
						fmt.Fprintln(outWriter(c), n)
					}
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
