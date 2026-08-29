package cli

import (
	"encoding/json"
	"fmt"

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
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (event id)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runEventsList,
			},
			{
				Name:      "retrigger",
				Usage:     "re-inject a stored event into its integration",
				ArgsUsage: "<id>",
				Action:    runEventsRetrigger,
			},
		},
	}
}

func runEventsList(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := BuildEventsListPath(c.String("cursor"), c.Int("limit"))
	var body struct {
		Events     []map[string]any `json:"events"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
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
}

func runEventsRetrigger(c *cli.Context) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi events retrigger <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildEventsRetrigger(id), nil)
}
