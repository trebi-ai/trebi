package cli

import (
	"encoding/json"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func harnessCommand() *cli.Command {
	return &cli.Command{
		Name:  "harness",
		Usage: "harness tools (jobi tool subset over API)",
		Subcommands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list harnesses",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
					&cli.BoolFlag{Name: "check", Usage: "note missing homes (exit 1 if any absent)"},
				},
				Action: runHarnessList,
			},
			{
				Name:      "ensure",
				Usage:     "ensure harness home + lifecycle install",
				ArgsUsage: "<name>",
				Action: func(c *cli.Context) error {
					name := c.Args().Get(0)
					if name == "" {
						return fmt.Errorf("name required")
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var out map[string]any
					if err := client.DoHTTP(BuildHarnessEnsure(name), &out); err != nil {
						return err
					}
					fmt.Fprintf(outWriter(c), "ensured %s home=%v\n", name, out["home"])
					return nil
				},
			},
			{
				Name:  "auth",
				Usage: "harness auth state (daemon mark; not full vendor login UX)",
				Subcommands: []*cli.Command{
					{
						Name:      "status",
						Usage:     "auth probe from harness_state",
						ArgsUsage: "[name]",
						Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
						Action:    runHarnessAuthStatus,
					},
					{
						Name:      "mark",
						Usage:     "POST auth_ok=true (no interactive vendor login over HTTP)",
						ArgsUsage: "<name>",
						Action: func(c *cli.Context) error {
							name := c.Args().Get(0)
							if name == "" {
								return fmt.Errorf("name required")
							}
							client, err := clientFor(c)
							if err != nil {
								return err
							}
							return client.DoHTTP(BuildHarnessAuthPost(name), nil)
						},
					},
				},
			},
		},
	}
}

func runHarnessList(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Harnesses []map[string]any `json:"harnesses"`
	}
	if err := client.GetJSON("/v1/harnesses", &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	headers := []string{"Name", "Present", "Home", "Bin"}
	if c.Bool("check") {
		headers = append(headers, "Note")
	}
	tbl := newTable(w, headers)
	bad := false
	for _, h := range body.Harnesses {
		present := mapStr(h, "present", "Present")
		if present == "" {
			present = "-"
		}
		row := table.Row{
			mapStr(h, "name", "Name"),
			colorStatus(w, present),
			dash(mapStr(h, "home", "Home")),
			dash(mapStr(h, "bin_path", "BinPath", "bin")),
		}
		if c.Bool("check") {
			note := ""
			if present == "false" || present == "0" || present == "-" {
				note = "missing"
				bad = true
			}
			row = append(row, dash(note))
		}
		tbl.AppendRow(row)
	}
	renderTable(tbl)
	if c.Bool("check") && bad {
		return fmt.Errorf("one or more harnesses missing")
	}
	return nil
}

func runHarnessAuthStatus(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	name := c.Args().Get(0)
	if name != "" {
		var body map[string]any
		if err := client.DoHTTP(BuildHarnessAuthGet(name), &body); err != nil {
			return err
		}
		if c.Bool("json") {
			enc := json.NewEncoder(outWriter(c))
			enc.SetIndent("", "  ")
			return enc.Encode(body)
		}
		fmt.Fprintf(outWriter(c), "%s auth_ok=%v mode=%v\n", name, body["auth_ok"], body["mode"])
		return nil
	}
	// All known harnesses from list
	var list struct {
		Harnesses []map[string]any `json:"harnesses"`
	}
	if err := client.GetJSON("/v1/harnesses", &list); err != nil {
		return err
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Name", "AuthOK", "Mode"})
	for _, h := range list.Harnesses {
		n := mapStr(h, "name", "Name")
		var body map[string]any
		if err := client.DoHTTP(BuildHarnessAuthGet(n), &body); err != nil {
			tbl.AppendRow(table.Row{n, "error", "-"})
			continue
		}
		tbl.AppendRow(table.Row{n, fmt.Sprint(body["auth_ok"]), dash(mapStr(body, "mode"))})
	}
	renderTable(tbl)
	return nil
}
