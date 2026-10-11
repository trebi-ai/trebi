package cli

import (
	"context"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func inboxCommand() *cli.Command {
	wsFlag := func() cli.Flag {
		return &cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id or name (global items always match)"}
	}
	kinds := "approval|request|failure|health|watchdog|escalation"
	return &cli.Command{
		Name:  "inbox",
		Usage: "inbox items",
		Commands: []*cli.Command{
			{
				Name:    "ls",
				Aliases: []string{"list"},
				Usage:   "list open items (or Done items with --done)",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "done", Usage: "list Done items"},
					&cli.BoolFlag{Name: "unread", Usage: "only unread items"},
					&cli.StringFlag{Name: "kind", Usage: kinds},
					&cli.StringFlag{Name: "query", Aliases: []string{"q"}, Usage: "text in title, subject, or body"},
					wsFlag(),
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
			inboxOpCommand("read", "mark items read", wsFlag(), kinds),
			inboxOpCommand("unread", "mark items unread", wsFlag(), kinds),
			inboxOpCommand("archive", "move items to Done", wsFlag(), kinds),
			inboxOpCommand("unarchive", "move Done items back to the inbox", wsFlag(), kinds),
		},
	}
}

// inboxOpCommand is one apply op on ids, or on every matching item with --all.
func inboxOpCommand(op, usage string, wsFlag cli.Flag, kinds string) *cli.Command {
	return &cli.Command{
		Name:      op,
		Usage:     usage,
		ArgsUsage: "<id>... | --all",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "all", Usage: "every item that matches --kind and --workspace"},
			&cli.StringFlag{Name: "kind", Usage: kinds + " (with --all)"},
			wsFlag,
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "archive items that still wait for an answer"},
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			body := request.InboxApply{Op: op, IDs: c.Args().Slice(), Confirm: c.Bool("yes")}
			switch {
			case c.Bool("all") && len(body.IDs) > 0:
				return fmt.Errorf("give ids or --all, not both")
			case c.Bool("all"):
				filter := map[string]any{}
				if op == "unarchive" {
					filter["view"] = "done"
				}
				if k := c.String("kind"); k != "" {
					filter["kind"] = k
				}
				if w := c.String("workspace"); w != "" {
					filter["workspace"] = resolveWorkspaceHint(c, w)
				}
				body.Filter = filter
			case len(body.IDs) == 0:
				return fmt.Errorf("usage: trebi inbox %s <id>... | --all", op)
			}
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			var out struct {
				Updated []string `json:"updated"`
			}
			if err := client.DoHTTP(request.BuildInboxApply(body), &out); err != nil {
				return err
			}
			if c.Bool("json") {
				return encodeJSON(c, out)
			}
			fmt.Fprintf(outWriter(c), "%s: %d item(s)\n", op, len(out.Updated))
			return nil
		},
	}
}

func runInboxList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	q := request.InboxQuery{
		Kind: c.String("kind"), Query: c.String("query"), Unread: c.Bool("unread"),
		Cursor: c.String("cursor"), Limit: c.Int("limit"),
	}
	if c.Bool("done") {
		q.View = "done"
	}
	if w := c.String("workspace"); w != "" {
		q.Workspace = resolveWorkspaceHint(c, w)
	}
	var body struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor,omitempty"`
		Counts     map[string]any   `json:"counts"`
	}
	if err := client.GetJSON(request.BuildInboxListPath(q), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Kind", "Read", "Title", "Created"})
	for _, it := range body.Items {
		read := "unread"
		if b, _ := it["read"].(bool); b {
			read = "read"
		}
		tbl.AppendRow(table.Row{
			mapStr(it, "id"), mapStr(it, "kind"), read, mapStr(it, "title"), mapStr(it, "created_at"),
		})
	}
	renderTable(tbl)
	if body.NextCursor != "" {
		fmt.Fprintf(w, "next: --cursor %s\n", body.NextCursor)
	}
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
	yes := func(k string) string {
		if b, _ := body[k].(bool); b {
			return "yes"
		}
		return "no"
	}
	w := outWriter(c)
	fmt.Fprintf(w, "ID:      %s\n", mapStr(body, "id"))
	fmt.Fprintf(w, "Kind:    %s\n", mapStr(body, "kind"))
	fmt.Fprintf(w, "Read:    %s\n", yes("read"))
	done := yes("done")
	if by := mapStr(body, "done_by"); by != "" {
		done += " (" + by + ")"
	}
	fmt.Fprintf(w, "Done:    %s\n", done)
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
