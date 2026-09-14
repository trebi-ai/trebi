package cli

import (
	"context"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func inboxCommand() *cli.Command {
	return &cli.Command{
		Name:  "inbox",
		Usage: "inbox items",
		Commands: []*cli.Command{
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
				Action: func(_ context.Context, c *cli.Command) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildInboxRead(c.Args().Get(0)), nil)
				},
			},
			{
				Name:  "read-all",
				Usage: "mark all unread items read",
				Action: func(_ context.Context, c *cli.Command) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildInboxReadAll(), nil)
				},
			},
			{
				Name:  "resolve-all",
				Usage: "resolve every open item",
				Flags: []cli.Flag{&cli.StringFlag{Name: "kind", Usage: "resolve one kind only"}},
				Action: func(_ context.Context, c *cli.Command) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildInboxResolveAll(c.String("kind")), nil)
				},
			},
			{
				Name:      "resolve",
				Usage:     "manually dismiss an inbox item",
				ArgsUsage: "<id>",
				Action: func(_ context.Context, c *cli.Command) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildInboxResolve(c.Args().Get(0)), nil)
				},
			},
		},
	}
}

func runInboxList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := client.GetJSON(request.BuildInboxListPath(c.String("status"), c.String("kind"), c.String("cursor"), c.Int("limit")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
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

func runInboxShow(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi inbox show <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildInboxGet(id), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
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

func alertsCommand() *cli.Command {
	return &cli.Command{
		Name:  "alerts",
		Usage: "per-firing alert feed (not the inbox queue you drain)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "since", Usage: "RFC3339 lower bound"},
			limitFlag(),
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		},
		Action: runAlerts,
	}
}

func runAlerts(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Alerts []map[string]any `json:"alerts"`
	}
	if err := client.GetJSON(request.BuildAlertsPath(c.String("since"), c.Int("limit")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
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
