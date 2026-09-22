// Hosted webhooks (plan 2026-09-22 Part I): mint, list, and revoke the
// URLs a sender posts to. A job fires from the delivery with
// `on: [{event: webhook}]`.
package cli

import (
	"context"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func hooksCommand() *cli.Command {
	return &cli.Command{
		Name:  "hooks",
		Usage: "hosted webhooks for job triggers",
		Commands: []*cli.Command{
			{
				Name:  "create",
				Usage: "mint a hosted webhook and print its URL once",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Required: true, Usage: "workspace id or name"},
					&cli.StringFlag{Name: "label", Required: true, Usage: "short name for the hook"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runHookCreate,
			},
			{
				Name:  "list",
				Usage: "list the hosted webhooks (URLs masked)",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "reveal", Usage: "show full URLs and write an audit entry"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runHookList,
			},
			{
				Name:      "delete",
				Usage:     "revoke one hosted webhook",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{forceFlag("required; deliveries to its URL stop")},
				Action:    runHookDelete,
			},
		},
	}
}

func runHookCreate(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildHookCreate(c.String("workspace"), c.String("label")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "Hook %s created for workspace %s.\n",
		dash(mapStr(body, "id")), dash(mapStr(body, "workspace_id")))
	fmt.Fprintf(w, "URL:  %s\n", dash(mapStr(body, "url")))
	fmt.Fprintln(w, "The URL is shown once. Handle it like a webhook secret.")
	return nil
}

func runHookList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Hooks []map[string]any `json:"hooks"`
	}
	if err := client.GetJSON(request.BuildHooksPath(c.Bool("reveal")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	tbl := newTable(outWriter(c), []string{"ID", "Workspace", "Label", "URL", "Created"})
	for _, h := range body.Hooks {
		tbl.AppendRow(table.Row{
			mapStr(h, "id"), mapStr(h, "workspace_id"), mapStr(h, "label"),
			mapStr(h, "url"), mapStr(h, "created_at"),
		})
	}
	renderTable(tbl)
	return nil
}

func runHookDelete(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi hooks delete <id>")
	}
	confirm, err := confirmed(c, "revokes hook "+id+"; deliveries to its URL stop")
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildHookDelete(id, confirm), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "Hook %s revoked.\n", id)
	return nil
}
