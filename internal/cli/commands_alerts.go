package cli

import (
	"encoding/json"

	"github.com/urfave/cli/v2"
)

func alertsCommand() *cli.Command {
	return &cli.Command{
		Name:  "alerts",
		Usage: "view alert.* bus events",
		Action: func(c *cli.Context) error {
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			var body map[string]any
			if err := client.GetJSON("/v1/alerts", &body); err != nil {
				return err
			}
			enc := json.NewEncoder(outWriter(c))
			enc.SetIndent("", "  ")
			return enc.Encode(body)
		},
	}
}
