package cli

import (
	"context"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func machineCommand() *cli.Command {
	jsonFlag := []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}}
	return &cli.Command{
		Name:  "machine",
		Usage: "machines that run Trebi, and what each can run",
		Commands: []*cli.Command{
			{Name: "list", Aliases: []string{"ls"}, Usage: "list the machines, this machine first", Flags: jsonFlag, Action: runMachineList},
			{Name: "show", Usage: "show one machine and what it can run", ArgsUsage: "[id|self]", Flags: jsonFlag, Action: runMachineShow},
		},
	}
}

func runMachineList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildMachinesPath(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	rows, _ := body["machines"].([]any)
	tbl := newTable(outWriter(c), []string{"ID", "Name", "OS", "Arch", "Self", "Last seen"})
	for _, r := range rows {
		row, _ := r.(map[string]any)
		self := ""
		if row["is_self"] == true {
			self = "yes"
		}
		tbl.AppendRow(table.Row{mapStr(row, "id"), mapStr(row, "name"), mapStr(row, "os"), mapStr(row, "arch"), self, dash(mapStr(row, "last_seen"))})
	}
	renderTable(tbl)
	return nil
}

func runMachineShow(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		id = "self"
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := client.DoHTTP(request.BuildMachineGet(id), &m); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, m)
	}
	w := outWriter(c)
	for _, f := range []struct{ label, key string }{
		{"ID", "id"}, {"Name", "name"}, {"OS", "os"}, {"Arch", "arch"},
		{"Device", "device_id"}, {"Last seen", "last_seen"},
	} {
		if v := mapStr(m, f.key); v != "" {
			fmt.Fprintf(w, "%-10s %s\n", f.label+":", v)
		}
	}
	caps, _ := m["capabilities"].(map[string]any)
	if caps == nil {
		return nil
	}
	if v, _ := caps["versions"].(map[string]any); v != nil {
		fmt.Fprintf(w, "%-10s %s\n", "Trebi:", mapStr(v, "trebi"))
	}
	fmt.Fprintln(w)
	tbl := newTable(w, []string{"Kind", "Name", "Version", "State"})
	for _, k := range []string{"harnesses", "connectors"} {
		items, _ := caps[k].([]any)
		for _, it := range items {
			row, _ := it.(map[string]any)
			kind := "harness"
			if k == "connectors" {
				kind = "connector"
			}
			tbl.AppendRow(table.Row{kind, mapStr(row, "name"), dash(mapStr(row, "version")), colorStatus(w, mapStr(row, "state"))})
		}
	}
	renderTable(tbl)
	return nil
}
