package cli

import (
	"cmp"
	"context"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func outboxCommand() *cli.Command {
	idAction := func(verb string, build func(string) request.HTTPRequest) cli.ActionFunc {
		return func(_ context.Context, c *cli.Command) error {
			id := c.Args().Get(0)
			if id == "" {
				return fmt.Errorf("usage: trebi outbox %s <id>", verb)
			}
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			var row map[string]any
			if err := client.DoHTTP(build(id), &row); err != nil {
				return err
			}
			if c.Bool("json") {
				return encodeJSON(c, row)
			}
			if verb == "show" {
				printOutboxRow(c, row)
				return nil
			}
			fmt.Fprintf(outWriter(c), "outbox %s %s\n", mapStr(row, "id"), mapStr(row, "status"))
			return nil
		}
	}
	jsonFlag := []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}}
	return &cli.Command{
		Name:  "outbox",
		Usage: "outgoing messages: channel replies, email, alerts",
		Commands: []*cli.Command{
			{
				Name:    "list",
				Aliases: []string{"ls"},
				Usage:   "list outbox rows, newest first",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "kind", Usage: "email|channel"},
					&cli.StringFlag{Name: "status", Usage: "pending|sending|sent|failed|canceled"},
					&cli.StringFlag{Name: "purpose", Usage: "reply|send|alert|report|test"},
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace name or id"},
					&cli.StringFlag{Name: "conversation", Usage: "conversation id"},
					&cli.StringFlag{Name: "inbox", Usage: "inbox item id"},
					&cli.StringFlag{Name: "run", Usage: "run id"},
					&cli.StringFlag{Name: "query", Aliases: []string{"q"}, Usage: "text in room, recipients, connection, subject, or body"},
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (row id)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runOutboxList,
			},
			{Name: "show", Usage: "show one outbox row", ArgsUsage: "<id>", Flags: jsonFlag, Action: idAction("show", request.BuildOutboxGet)},
			{Name: "retry", Usage: "send a failed or canceled row again", ArgsUsage: "<id>", Flags: jsonFlag, Action: idAction("retry", request.BuildOutboxRetry)},
			{Name: "cancel", Usage: "stop a pending or failed row", ArgsUsage: "<id>", Flags: jsonFlag, Action: idAction("cancel", request.BuildOutboxCancel)},
		},
	}
}

func runOutboxList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	q := request.OutboxQuery{
		Kind: c.String("kind"), Status: c.String("status"), Purpose: c.String("purpose"),
		Conversation: c.String("conversation"), Inbox: c.String("inbox"), Run: c.String("run"),
		Query: c.String("query"), Cursor: c.String("cursor"), Limit: c.Int("limit"),
	}
	if w := c.String("workspace"); w != "" {
		q.Workspace = resolveWorkspaceHint(c, w)
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildOutboxListPath(q), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	rows, _ := body["outbox"].([]any)
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Kind", "Purpose", "Status", "Attempts", "To", "Error"})
	for _, r := range rows {
		row, _ := r.(map[string]any)
		tbl.AppendRow(table.Row{
			mapStr(row, "id"), mapStr(row, "kind"), mapStr(row, "purpose"),
			colorStatus(w, mapStr(row, "status")), mapStr(row, "attempts"),
			dash(outboxTo(row)), dash(truncateDisplay(mapStr(row, "error"), 60)),
		})
	}
	renderTable(tbl)
	if next := mapStr(body, "next_cursor"); next != "" {
		fmt.Fprintf(w, "next cursor: %s\n", next)
	}
	return nil
}

// outboxTo is the address column of one outbox row: the room, else the
// email recipients.
func outboxTo(row map[string]any) string {
	return cmp.Or(mapStr(row, "room"), mapStr(row, "recipients"))
}

func printOutboxRow(c *cli.Command, row map[string]any) {
	w := outWriter(c)
	for _, f := range []struct{ label, key string }{
		{"ID", "id"}, {"Kind", "kind"}, {"Purpose", "purpose"}, {"Status", "status"},
		{"Attempts", "attempts"}, {"Workspace", "workspace_id"}, {"Instance", "instance"},
		{"Room", "room"}, {"Thread", "thread"}, {"Recipients", "recipients"}, {"Hold", "hold_reason"}, {"Subject", "subject"}, {"Next", "next_attempt_at"},
		{"Sent", "sent_at"}, {"Error", "error"}, {"Body", "body"},
	} {
		if v := mapStr(row, f.key); v != "" {
			fmt.Fprintf(w, "%-10s %s\n", f.label+":", v)
		}
	}
}
