package cli

import (
	"encoding/json"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func alertsCommand() *cli.Command {
	return &cli.Command{
		Name:  "alerts",
		Usage: "per-firing alert feed (not the inbox queue you drain)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "since", Usage: "RFC3339 lower bound"},
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		},
		Action: runAlerts,
	}
}

func runAlerts(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Alerts []map[string]any `json:"alerts"`
	}
	if err := client.GetJSON(BuildAlertsPath(c.String("since")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Seq", "Topic", "Kind", "Subject", "Time"})
	for _, a := range body.Alerts {
		attrs, _ := a["attrs"].(map[string]any)
		tbl.AppendRow(table.Row{
			mapStr(a, "seq"),
			mapStr(a, "topic"),
			mapStr(attrs, "kind"),
			mapStr(attrs, "subject"),
			mapStr(a, "ts"),
		})
	}
	renderTable(tbl)
	return nil
}
