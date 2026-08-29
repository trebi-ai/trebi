package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func integrationCommand() *cli.Command {
	return &cli.Command{
		Name:  "integration",
		Usage: "manage integrations (jobi sources)",
		Subcommands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list integrations",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runIntegrationList,
			},
			{
				Name:      "show",
				Usage:     "show one integration",
				ArgsUsage: "<name>",
				Action:    runIntegrationShow,
			},
			{
				Name:      "set",
				Usage:     "create or update an integration",
				ArgsUsage: "<name> <command...>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "format", Aliases: []string{"f"}, Value: "line", Usage: "json|line"},
					&cli.StringFlag{Name: "type", Value: "stream", Usage: "stream|cli|mcp"},
					&cli.StringFlag{Name: "cwd", Usage: "working directory"},
					&cli.StringSliceFlag{Name: "env", Aliases: []string{"e"}, Usage: "KEY=VALUE (repeatable)"},
				},
				Action: runIntegrationSet,
			},
			{
				Name:      "remove",
				Usage:     "delete an integration",
				ArgsUsage: "<name>",
				Aliases:   []string{"rm", "delete"},
				Action:    runIntegrationRemove,
			},
			{
				Name:      "connect",
				Usage:     "start an integration (begin listening; inverse of disconnect)",
				ArgsUsage: "<name>",
				Action:    runIntegrationConnect,
			},
			{
				Name:      "disconnect",
				Usage:     "stop an integration (same as stop; halt the listening process)",
				ArgsUsage: "<name>",
				Action:    runIntegrationDisconnect,
			},
			{
				Name:      "stop",
				Usage:     "stop a running integration (alias of disconnect)",
				ArgsUsage: "<name>",
				Action:    runIntegrationStop,
			},
			{
				Name:      "restart",
				Usage:     "restart an integration",
				ArgsUsage: "<name>",
				Action:    runIntegrationRestart,
			},
			{
				Name:  "cursor",
				Usage: "show or set integration_cursors.last_event_id (replay position)",
				Subcommands: []*cli.Command{
					{
						Name:      "show",
						Usage:     "show last_event_id",
						ArgsUsage: "<name>",
						Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
						Action:    runIntegrationCursorShow,
					},
					{
						Name:      "set",
						Usage:     "set last_event_id",
						ArgsUsage: "<name> <last-event-id>",
						Action:    runIntegrationCursorSet,
					},
					{
						Name:      "reset",
						Usage:     "reset last_event_id to empty (replay from start)",
						ArgsUsage: "<name>",
						Action:    runIntegrationCursorReset,
					},
				},
			},
			{
				Name:      "logs",
				Usage:     "show integration log stream",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "follow (SSE; may block)"},
				},
				Action: runIntegrationLogs,
			},
			{
				Name:      "test",
				Usage:     "inject a test event into an integration",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "event", Aliases: []string{"e"}, Usage: "event JSON object (default {\"test\":true})"},
				},
				Action: runIntegrationTest,
			},
			integrationQueueAlias(),
		},
	}
}

func integrationQueueAlias() *cli.Command {
	q := queueCommand()
	q.Hidden = true
	return q
}

func queueCommand() *cli.Command {
	return &cli.Command{
		Name:  "queue",
		Usage: "inspect the dispatch queue",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "status", Usage: "filter by status"},
			&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (queue id)"},
			&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		},
		Action: func(c *cli.Context) error {
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			path := BuildQueueListPath(c.String("status"), c.String("cursor"), c.Int("limit"))
			var body struct {
				Items []map[string]any `json:"items"`
			}
			if err := client.GetJSON(path, &body); err != nil {
				return err
			}
			if c.Bool("json") {
				enc := json.NewEncoder(outWriter(c))
				enc.SetIndent("", "  ")
				return enc.Encode(body)
			}
			w := outWriter(c)
			// QueueItem has no json tags → PascalCase keys from the API.
			tbl := newTable(w, []string{"ID", "Status", "Integration", "Job", "Conversation", "Block", "Run", "Created"})
			for _, it := range body.Items {
				status := mapStr(it, "Status", "status")
				tbl.AppendRow(table.Row{
					mapStr(it, "ID", "id"),
					colorStatus(w, status),
					dash(mapStr(it, "Integration", "integration")),
					dash(mapStr(it, "Job", "job")),
					dash(mapStr(it, "ConversationKey", "conversation_key")),
					dash(mapStr(it, "BlockReason", "block_reason")),
					dash(mapStr(it, "RunID", "run_id")),
					dash(mapStr(it, "CreatedAt", "created_at")),
				})
			}
			renderTable(tbl)
			return nil
		},
		Subcommands: []*cli.Command{
			{
				Name:      "requeue",
				Usage:     "requeue a blocked/done row to pending",
				ArgsUsage: "<id>",
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.PostJSON("/v1/queue/"+c.Args().Get(0)+"/requeue", nil, nil)
				},
			},
			{
				Name:      "cancel",
				Usage:     "cancel a queue row",
				ArgsUsage: "<id>",
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.PostJSON("/v1/queue/"+c.Args().Get(0)+"/cancel", nil, nil)
				},
			},
		},
	}
}

func runIntegrationList(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(BuildIntegrationsListPath(c.Int("limit")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	if list, ok := body["integrations"].([]any); ok {
		tbl := newTable(w, []string{"Name", "Type", "Format", "Status", "Command"})
		for _, raw := range list {
			m, ok := raw.(map[string]any)
			if !ok {
				tbl.AppendRow(table.Row{fmt.Sprint(raw), "-", "-", "-", "-"})
				continue
			}
			tbl.AppendRow(table.Row{
				mapStr(m, "name", "Name", "id"),
				dash(mapStr(m, "type", "Type")),
				dash(mapStr(m, "format", "Format")),
				colorStatus(w, mapStr(m, "status", "Status")),
				dash(mapStr(m, "command", "Command", "cmd")),
			})
		}
		renderTable(tbl)
		return nil
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

func runIntegrationShow(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON("/v1/integrations/"+name, &body); err != nil {
		return err
	}
	enc := json.NewEncoder(outWriter(c))
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

func runIntegrationSet(c *cli.Context) error {
	if c.NArg() < 2 {
		return fmt.Errorf("usage: trebi integration set <name> <command...>")
	}
	name := c.Args().Get(0)
	cmdParts := c.Args().Tail()
	command := strings.Join(cmdParts, " ")
	env := map[string]string{}
	for _, e := range c.StringSlice("env") {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			return fmt.Errorf("env %q: want KEY=VALUE", e)
		}
		env[k] = v
	}
	req := BuildIntegrationSet(name, command, c.String("format"), c.String("type"), c.String("cwd"), env)
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(req, nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "integration %s set\n", name)
	return nil
}

func runIntegrationRemove(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildIntegrationDelete(name), nil)
}

func runIntegrationConnect(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildIntegrationConnect(name), nil)
}

func runIntegrationDisconnect(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildIntegrationDisconnect(name), nil)
}

func runIntegrationStop(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildIntegrationStop(name), nil)
}

func runIntegrationCursorShow(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(BuildIntegrationCursorGet(name), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Name", "Last Event ID", "Updated"})
	tbl.AppendRow(table.Row{
		mapStr(body, "name"),
		dash(mapStr(body, "last_event_id")),
		dash(mapStr(body, "updated_at")),
	})
	renderTable(tbl)
	return nil
}

func runIntegrationCursorSet(c *cli.Context) error {
	name, last := c.Args().Get(0), c.Args().Get(1)
	if name == "" || last == "" {
		return fmt.Errorf("usage: trebi integration cursor set <name> <last-event-id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildIntegrationCursorSet(name, last), nil)
}

func runIntegrationCursorReset(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi integration cursor reset <name>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildIntegrationCursorReset(name), nil)
}

func runIntegrationRestart(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildIntegrationRestart(name), nil)
}

func runIntegrationLogs(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := BuildIntegrationLogsPath(name, c.Bool("follow"))
	if c.Bool("follow") {
		return client.StreamGET(path, outWriter(c))
	}
	raw, err := client.GetRaw(path)
	if err != nil {
		return err
	}
	// Non-follow may be SSE lines or JSON {"log":"…"}.
	if text := decodeLogBody(raw); text != "" {
		fmt.Fprint(outWriter(c), text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Fprintln(outWriter(c))
		}
		return nil
	}
	fmt.Fprint(outWriter(c), string(raw))
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		fmt.Fprintln(outWriter(c))
	}
	return nil
}

func decodeLogBody(raw []byte) string {
	var wrap struct {
		Log string `json:"log"`
	}
	if json.Unmarshal(raw, &wrap) == nil && wrap.Log != "" {
		return wrap.Log
	}
	return ""
}

func runIntegrationTest(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	var event any
	if s := c.String("event"); s != "" {
		if err := json.Unmarshal([]byte(s), &event); err != nil {
			return fmt.Errorf("event JSON: %w", err)
		}
	}
	req := BuildIntegrationTest(name, event)
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(req, &out); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "ok=%v\n", out["ok"])
	return nil
}

func eventsCommand() *cli.Command {
	return &cli.Command{
		Name:  "events",
		Usage: "trigger events",
		Subcommands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list durable events",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (event id)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runEventsList,
			},
			{
				Name:      "retrigger",
				Usage:     "re-inject a stored event into its integration",
				ArgsUsage: "<id>",
				Action:    runEventsRetrigger,
			},
		},
	}
}

func runEventsList(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := BuildEventsListPath(c.String("cursor"), c.Int("limit"))
	var body struct {
		Events     []map[string]any `json:"events"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Integration", "Summary", "Received"})
	for _, e := range body.Events {
		tbl.AppendRow(table.Row{
			mapStr(e, "id"), mapStr(e, "integration"),
			dash(mapStr(e, "summary")), mapStr(e, "received_at"),
		})
	}
	renderTable(tbl)
	return nil
}

func runEventsRetrigger(c *cli.Context) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi events retrigger <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildEventsRetrigger(id), nil)
}
