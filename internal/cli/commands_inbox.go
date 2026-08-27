package cli

import (
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func inboxCommand() *cli.Command {
	return &cli.Command{
		Name:  "inbox",
		Usage: "inbox items",
		Subcommands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list inbox",
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var body struct {
						Items []map[string]any `json:"items"`
					}
					if err := client.GetJSON("/v1/inbox", &body); err != nil {
						return err
					}
					w := outWriter(c)
					tbl := newTable(w, []string{"ID", "Kind", "Status", "Title", "Created"})
					for _, it := range body.Items {
						tbl.AppendRow(table.Row{
							mapStr(it, "id"), mapStr(it, "kind"), colorStatus(w, mapStr(it, "status")),
							mapStr(it, "title"), mapStr(it, "created_at"),
						})
					}
					renderTable(tbl)
					return nil
				},
			},
			{
				Name:      "read",
				Usage:     "mark read",
				ArgsUsage: "<id>",
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.PostJSON("/v1/inbox/"+c.Args().Get(0)+"/read", nil, nil)
				},
			},
		},
	}
}
