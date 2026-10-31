package cli

import (
	"context"
	"errors"
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
			{Name: "connect", Usage: "connect to another machine with the pairing link that it made (trebi mobile pair on that machine)", ArgsUsage: "<id> <link>", Flags: jsonFlag, Action: runMachineConnect},
			{Name: "disconnect", Usage: "remove the link to another machine", ArgsUsage: "<id>",
				Flags: []cli.Flag{forceFlag("required; the other machine must make a new pairing link to connect again")}, Action: runMachineDisconnect},
			{Name: "sessions", Usage: "list the sessions started outside Trebi on this machine", ArgsUsage: "[id|self]", Flags: jsonFlag, Action: runMachineSessions},
			{Name: "session", Usage: "show one session started outside Trebi, with its turns", ArgsUsage: "<session> [id|self]", Flags: jsonFlag, Action: runMachineSession},
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
	tbl := newTable(outWriter(c), []string{"ID", "Name", "OS", "Arch", "Self", "Link", "Last seen"})
	for _, r := range rows {
		row, _ := r.(map[string]any)
		self := ""
		if row["is_self"] == true {
			self = "yes"
		}
		link, _ := row["link"].(map[string]any)
		tbl.AppendRow(table.Row{mapStr(row, "id"), mapStr(row, "name"), mapStr(row, "os"), mapStr(row, "arch"), self, dash(mapStr(link, "state")), dash(mapStr(row, "last_seen"))})
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

func runMachineConnect(_ context.Context, c *cli.Command) error {
	id, link := c.Args().Get(0), c.Args().Get(1)
	if id == "" || link == "" {
		return errors.New("usage: trebi machine connect <id> <link>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := client.DoHTTP(request.BuildMachineLink(id, link), &m); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, m)
	}
	fmt.Fprintf(outWriter(c), "Connected to %s.\n", mapStr(m, "name"))
	return nil
}

func runMachineDisconnect(_ context.Context, c *cli.Command) error { //nolint:dupl // the confirm-gated delete of each CLI area
	id := c.Args().Get(0)
	if id == "" {
		return errors.New("usage: trebi machine disconnect <id>")
	}
	confirm, err := confirmed(c, "disconnects from machine "+id+"; it must make a new pairing link to connect again")
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildMachineUnlink(id, confirm), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "Disconnected from %s.\n", id)
	return nil
}

func runMachineSessions(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		id = "self"
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildMachineSessions(id), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	rows, _ := body["sessions"].([]any)
	tbl := newTable(w, []string{"ID", "Harness", "Folder", "State", "Turns", "Last activity"})
	for _, r := range rows {
		row, _ := r.(map[string]any)
		tbl.AppendRow(table.Row{mapStr(row, "id"), mapStr(row, "harness"), mapStr(row, "folder"), colorStatus(w, mapStr(row, "state")), mapStr(row, "turns"), mapStr(row, "last_activity")})
	}
	renderTable(tbl)
	return nil
}

func runMachineSession(_ context.Context, c *cli.Command) error {
	sess, id := c.Args().Get(0), c.Args().Get(1)
	if sess == "" {
		return fmt.Errorf("session required")
	}
	if id == "" {
		id = "self"
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildMachineSession(id, sess), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	s, _ := body["session"].(map[string]any)
	fmt.Fprintf(w, "%s  %s  %s  %s\n\n", mapStr(s, "id"), mapStr(s, "harness"), mapStr(s, "folder"), mapStr(s, "state"))
	turns, _ := body["turns"].([]any)
	for _, t := range turns {
		row, _ := t.(map[string]any)
		fmt.Fprintf(w, "[%s] %s\n", mapStr(row, "role"), mapStr(row, "text"))
	}
	return nil
}
