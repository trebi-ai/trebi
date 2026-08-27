package cli

import (
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func eventsCommand() *cli.Command {
	return &cli.Command{
		Name:  "events",
		Usage: "trigger events",
		Subcommands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list durable events",
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var body struct {
						Events []map[string]any `json:"events"`
					}
					if err := client.GetJSON("/v1/events", &body); err != nil {
						return err
					}
					w := outWriter(c)
					tbl := newTable(w, []string{"ID", "Integration", "Summary", "Received"})
					for _, e := range body.Events {
						tbl.AppendRow(table.Row{
							mapStr(e, "id"), mapStr(e, "integration"),
							dash(mapStr(e, "summary")), mapStr(e, "received_at"),
						})
					}
					renderTable(tbl)
					return nil
				},
			},
		},
	}
}
