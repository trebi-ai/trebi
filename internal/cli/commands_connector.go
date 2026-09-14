package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func connectorCommand() *cli.Command {
	return &cli.Command{
		Name:  "connector",
		Usage: "manage connectors",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list connectors",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runConnectorList,
			},
			{
				Name:      "show",
				Usage:     "show one connector",
				ArgsUsage: "<name>",
				Action:    runConnectorShow,
			},
			{
				Name:      "set",
				Usage:     "create or update a connector",
				ArgsUsage: "<name> <command...>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "format", Aliases: []string{"f"}, Usage: "json|line (default line)"},
					&cli.StringFlag{Name: "type", Usage: "stream|cli|mcp (default stream)"},
					&cli.StringFlag{Name: "cwd", Usage: "working directory"},
					&cli.StringSliceFlag{Name: "env", Aliases: []string{"e"}, Usage: "KEY=VALUE (repeatable)"},
					&cli.StringSliceFlag{Name: "facet", Usage: "facet name (repeatable)"},
				},
				Action: runConnectorSet,
			},
			{
				Name:      "delete",
				Aliases:   []string{"remove"},
				Usage:     "delete a connector",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{forceFlag("required; stops the event stream")},
				Action:    runConnectorRemove,
			},
			{
				Name:      "connect",
				Usage:     "start a connector (begin listening; inverse of disconnect)",
				ArgsUsage: "<name>",
				Action:    runConnectorConnect,
			},
			{
				Name:      "disconnect",
				Usage:     "halt the listening process (alias: stop)",
				ArgsUsage: "<name>",
				Action:    runConnectorDisconnect,
			},
			{
				Name:      "stop",
				Usage:     "stop a running connector (alias of disconnect)",
				ArgsUsage: "<name>",
				Action:    runConnectorStop,
			},
			{
				Name:      "restart",
				Usage:     "restart a connector",
				ArgsUsage: "<name>",
				Action:    runConnectorRestart,
			},
			{
				Name:  "cursor",
				Usage: "show or set connector_cursors.last_event_id (replay position)",
				Commands: []*cli.Command{
					{
						Name:      "show",
						Usage:     "show last_event_id",
						ArgsUsage: "<name>",
						Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
						Action:    runConnectorCursorShow,
					},
					{
						Name:      "set",
						Usage:     "set last_event_id",
						ArgsUsage: "<name> <last-event-id>",
						Action:    runConnectorCursorSet,
					},
					{
						Name:      "reset",
						Usage:     "reset last_event_id to empty (replay from start)",
						ArgsUsage: "<name>",
						Action:    runConnectorCursorReset,
					},
				},
			},
			{
				Name:      "logs",
				Usage:     "show connector log stream",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "follow (SSE; may block)"},
				},
				Action: runConnectorLogs,
			},
			{
				Name:      "test",
				Usage:     "inject a test event into a connector",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "event", Aliases: []string{"e"}, Usage: "event JSON object (default {\"test\":true})"},
				},
				Action: runConnectorTest,
			},
			connectorQueueAlias(),
		},
	}
}

func connectorQueueAlias() *cli.Command {
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
		Action: func(_ context.Context, c *cli.Command) error {
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			path := request.BuildQueueListPath(c.String("status"), c.String("cursor"), c.Int("limit"))
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
			tbl := newTable(w, []string{"ID", "Status", "Connector", "Job", "Conversation", "Block", "Run", "Created"})
			for _, it := range body.Items {
				status := mapStr(it, "Status", "status")
				tbl.AppendRow(table.Row{
					mapStr(it, "ID", "id"),
					colorStatus(w, status),
					dash(mapStr(it, "Connector", "connector")),
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
		Commands: []*cli.Command{
			{
				Name:      "requeue",
				Usage:     "requeue a blocked/done row to pending",
				ArgsUsage: "<id>",
				Action: func(_ context.Context, c *cli.Command) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildQueueRequeue(c.Args().Get(0)), nil)
				},
			},
			{
				Name:      "cancel",
				Usage:     "cancel one queue row, or several",
				ArgsUsage: "<id> [id...]",
				Action: func(_ context.Context, c *cli.Command) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					ids := c.Args().Slice()
					if len(ids) == 0 {
						return fmt.Errorf("usage: trebi queue cancel <id> [id...]")
					}
					if len(ids) == 1 {
						return client.DoHTTP(request.BuildQueueCancel(ids[0]), nil)
					}
					return client.DoHTTP(request.BuildQueueCancelBulk(ids), nil)
				},
			},
		},
	}
}

func runConnectorList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildConnectorsListPath(c.Int("limit")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	if list, ok := body["connectors"].([]any); ok {
		tbl := newTable(w, []string{"Name", "Type", "Provenance", "Status", "Command"})
		for _, raw := range list {
			m, ok := raw.(map[string]any)
			if !ok {
				tbl.AppendRow(table.Row{fmt.Sprint(raw), "-", "-", "-", "-"})
				continue
			}
			tbl.AppendRow(table.Row{
				mapStr(m, "name", "Name", "id"),
				dash(mapStr(m, "type", "Type")),
				dash(mapStr(m, "provenance", "Provenance")),
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

func runConnectorShow(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildConnectorGetPath(name), &body); err != nil {
		return err
	}
	enc := json.NewEncoder(outWriter(c))
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

func runConnectorSet(_ context.Context, c *cli.Command) error {
	if c.NArg() < 2 {
		return fmt.Errorf("usage: trebi connector set <name> <command...>")
	}
	name := c.Args().Get(0)
	cmdParts := c.Args().Tail()
	command := strings.Join(cmdParts, " ")
	env, err := parseKeyValues(c.StringSlice("env"))
	if err != nil {
		return fmt.Errorf("env %w", err)
	}
	req := request.ConnectorSpec{
		Name: name, Command: command, Format: c.String("format"),
		Type: c.String("type"), Cwd: c.String("cwd"),
		Env: env, Facets: c.StringSlice("facet"),
	}.Build()
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(req, nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s set\n", name)
	return nil
}

func runConnectorRemove(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	confirm, err := confirmed(c, "deletes connector "+name+" and stops its event stream")
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildConnectorDelete(name, confirm), nil)
}

func runConnectorConnect(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildConnectorConnect(name), nil)
}

func runConnectorDisconnect(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildConnectorDisconnect(name), nil)
}

func runConnectorStop(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildConnectorStop(name), nil)
}

func runConnectorCursorShow(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorCursorGet(name), &body); err != nil {
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

func runConnectorCursorSet(_ context.Context, c *cli.Command) error {
	name, last := c.Args().Get(0), c.Args().Get(1)
	if name == "" || last == "" {
		return fmt.Errorf("usage: trebi connector cursor set <name> <last-event-id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildConnectorCursorSet(name, last), nil)
}

func runConnectorCursorReset(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector cursor reset <name>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildConnectorCursorReset(name), nil)
}

func runConnectorRestart(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildConnectorRestart(name), nil)
}

func runConnectorLogs(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := request.BuildConnectorLogsPath(name, c.Bool("follow"))
	if c.Bool("follow") {
		return client.StreamGET(path, outWriter(c))
	}
	raw, err := client.GetRaw(path)
	if err != nil {
		return err
	}
	// Non-follow may be SSE lines or JSON {"log":"…"}.
	var wrap struct {
		Log string `json:"log"`
	}
	if json.Unmarshal(raw, &wrap) == nil && wrap.Log != "" {
		fmt.Fprint(outWriter(c), wrap.Log)
		if !strings.HasSuffix(wrap.Log, "\n") {
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

func runConnectorTest(_ context.Context, c *cli.Command) error {
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
	req := request.BuildConnectorTest(name, event)
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
		Name:    "event",
		Aliases: []string{"events"},
		Usage:   "trigger events",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list durable events",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (event id)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.StringFlag{Name: "connector", Usage: "connector name filter"},
					&cli.StringFlag{Name: "since", Usage: "RFC3339 time or a duration such as 24h"},
					&cli.StringFlag{Name: "search", Usage: "text match on summary or payload"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runEventsList,
			},
			{
				Name:      "show",
				Usage:     "show one event",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runEventsShow,
			},
			{
				Name:      "retrigger",
				Usage:     "re-inject a stored event into its connector",
				ArgsUsage: "<id>",
				Action:    runEventsRetrigger,
			},
		},
	}
}

func runEventsList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := request.BuildEventsListPath(c.String("cursor"), c.Int("limit"),
		c.String("connector"), c.String("since"), c.String("search"))
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
	tbl := newTable(w, []string{"ID", "Connector", "Summary", "Received"})
	for _, e := range body.Events {
		tbl.AppendRow(table.Row{
			mapStr(e, "id"), mapStr(e, "connector"),
			dash(mapStr(e, "summary")), mapStr(e, "received_at"),
		})
	}
	renderTable(tbl)
	return nil
}

func runEventsShow(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi events show <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := request.BuildEventGet(id)
	var body map[string]any
	if err := client.DoHTTP(req, &body); err != nil {
		return err
	}
	enc := json.NewEncoder(outWriter(c))
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

func runEventsRetrigger(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi events retrigger <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildEventsRetrigger(id), nil)
}
