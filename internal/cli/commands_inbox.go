package cli

import (
	"encoding/json"
	"fmt"

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
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "status", Usage: "unread|read|resolved"},
					&cli.StringFlag{Name: "kind", Usage: "approval|failure|health|watchdog"},
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (inbox id)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runInboxList,
			},
			{
				Name:      "show",
				Usage:     "show one inbox item",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runInboxShow,
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
					return client.DoHTTP(BuildInboxRead(c.Args().Get(0)), nil)
				},
			},
			{
				Name:  "read-all",
				Usage: "mark all unread items read",
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(BuildInboxReadAll(), nil)
				},
			},
			{
				Name:      "resolve",
				Usage:     "manually dismiss an inbox item",
				ArgsUsage: "<id>",
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(BuildInboxResolve(c.Args().Get(0)), nil)
				},
			},
		},
	}
}

func runInboxList(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := client.GetJSON(BuildInboxListPath(c.String("status"), c.String("kind"), c.String("cursor"), c.Int("limit")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
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
}

func runInboxShow(c *cli.Context) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi inbox show <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(BuildInboxGet(id), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "ID:      %s\n", mapStr(body, "id"))
	fmt.Fprintf(w, "Kind:    %s\n", mapStr(body, "kind"))
	fmt.Fprintf(w, "Status:  %s\n", mapStr(body, "status"))
	fmt.Fprintf(w, "Title:   %s\n", mapStr(body, "title"))
	if b := mapStr(body, "body"); b != "" {
		fmt.Fprintf(w, "Body:    %s\n", b)
	}
	if h := mapStr(body, "hint"); h != "" {
		fmt.Fprintf(w, "Hint:    %s\n", h)
	}
	return nil
}
