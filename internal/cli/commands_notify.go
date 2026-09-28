package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func notifyCommand() *cli.Command {
	scopeFlags := func(extra ...cli.Flag) []cli.Flag {
		return append([]cli.Flag{
			&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace name or id"},
			&cli.StringFlag{Name: "agent", Usage: "agent name, for example agents/cmo"},
			&cli.StringFlag{Name: "job", Usage: "job name (needs a workspace)"},
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		}, extra...)
	}
	return &cli.Command{
		Name:  "notify",
		Usage: "alert routes: show the route of a scope, send a test along it",
		Commands: []*cli.Command{
			{Name: "route", Usage: "show the resolved alert route", Flags: scopeFlags(), Action: runNotifyRoute},
			{
				Name: "test", Usage: "send one test message per usable step",
				Flags:  scopeFlags(&cli.IntFlag{Name: "step", Value: -1, Usage: "test only this step index"}),
				Action: runNotifyTest,
			},
		},
	}
}

func notifyScope(c *cli.Command) request.NotifyScope {
	sc := request.NotifyScope{Agent: c.String("agent"), Job: c.String("job")}
	if w := c.String("workspace"); w != "" {
		sc.Workspace = resolveWorkspaceHint(c, w)
	}
	return sc
}

func runNotifyRoute(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildNotifyRoutePath(notifyScope(c)), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "Source:  %s\n", dash(mapStr(body, "source")))
	fmt.Fprintf(w, "Contact: %s (%s)\n", dash(joinAny(body["contact"])), mapStr(body, "contact_source"))
	tbl := newTable(w, []string{"Step", "Via", "To", "Targets", "Problem"})
	steps, _ := body["steps"].([]any)
	for i, s := range steps {
		st, _ := s.(map[string]any)
		tbl.AppendRow(table.Row{i, mapStr(st, "via"), dash(mapStr(st, "to")), dash(joinAny(st["targets"])), dash(mapStr(st, "problem"))})
	}
	renderTable(tbl)
	if len(steps) == 0 {
		fmt.Fprintln(w, "no steps: alerts stay in the inbox")
	}
	return nil
}

func runNotifyTest(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var step *int
	if n := c.Int("step"); n >= 0 {
		step = &n
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildNotifyTest(notifyScope(c), step), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Step", "Status", "Outbox", "Error"})
	results, _ := body["results"].([]any)
	for _, r := range results {
		res, _ := r.(map[string]any)
		tbl.AppendRow(table.Row{mapStr(res, "step"), colorStatus(w, mapStr(res, "status")), dash(mapStr(res, "outbox_id")), dash(mapStr(res, "error"))})
	}
	renderTable(tbl)
	return nil
}

// joinAny joins a JSON string list for display.
func joinAny(v any) string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, fmt.Sprint(x))
	}
	return strings.Join(out, ", ")
}
