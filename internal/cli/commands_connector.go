package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
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
					&cli.StringFlag{Name: "capability", Usage: "filter: actions|events|channel"},
					&cli.StringFlag{Name: "provenance", Usage: "filter: native|managed|inherited"},
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
				Usage:     "create or update a connector (PATCH; flags overlay the stored definition)",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "cwd", Usage: "working directory"},
					&cli.StringSliceFlag{Name: "env", Aliases: []string{"e"}, Usage: "KEY=VALUE (repeatable; merges into the stored env)"},
					&cli.BoolFlag{Name: "no-events", Usage: "remove the events block"},
					&cli.StringFlag{Name: "events-command", Usage: "events.command; implies the events block"},
					&cli.StringFlag{Name: "format", Usage: "events.format: line|json (default line)"},
					&cli.StringFlag{Name: "restart", Usage: "events.restart: always|on-failure|never"},
					&cli.DurationFlag{Name: "poll", Usage: "events.poll: poll interval such as 15m; 0 runs live"},
					&cli.StringFlag{Name: "id", Usage: "events.id: dot path to the source event id"},
					&cli.StringFlag{Name: "ts", Usage: "events.ts: dot path used to sort catch-up output"},
					&cli.StringFlag{Name: "identity", Usage: "events.identity: dot path to the sender"},
					&cli.StringFlag{Name: "catchup-command", Usage: "events.catchup.command"},
					&cli.IntFlag{Name: "catchup-max", Usage: "events.catchup.max_events"},
					&cli.DurationFlag{Name: "catchup-timeout", Usage: "events.catchup.timeout, e.g. 60s"},
					&cli.BoolFlag{Name: "no-actions", Usage: "remove the actions block"},
					&cli.StringFlag{Name: "mcp-command", Usage: "actions.mcp.command; implies the actions block"},
					&cli.StringSliceFlag{Name: "mcp-arg", Usage: "actions.mcp.args (repeatable)"},
					&cli.StringSliceFlag{Name: "mcp-env", Usage: "actions.mcp.env KEY=VALUE (repeatable)"},
					&cli.StringFlag{Name: "mcp-url", Usage: "actions.mcp.url"},
					&cli.StringSliceFlag{Name: "mcp-header", Usage: "actions.mcp.headers KEY=VALUE (repeatable)"},
					&cli.StringSliceFlag{Name: "cli-command", Usage: "actions.cli.commands (repeatable)"},
					&cli.StringFlag{Name: "skill", Usage: "actions.cli.skill"},
					&cli.BoolFlag{Name: "no-channel", Usage: "remove the channel block"},
					&cli.StringFlag{Name: "channel-ref", Usage: "channel.ref"},
					&cli.StringFlag{Name: "send", Usage: "channel.send template"},
					&cli.StringFlag{Name: "typing", Usage: "channel.typing template"},
					&cli.StringFlag{Name: "ack", Usage: "channel.ack template"},
				},
				Action: runConnectorSet,
			},
			{
				Name:      "apply",
				Usage:     "write one connector definition from a YAML or JSON file (PUT)",
				ArgsUsage: "--file <path>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "definition file"},
				},
				Action: runConnectorApply,
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
				return encodeJSON(c, body)
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
	path := request.BuildConnectorsListPath(c.Int("limit"), c.String("capability"), c.String("provenance"))
	var body map[string]any
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	if list, ok := body["connectors"].([]any); ok {
		tbl := newTable(w, []string{"Name", "Capabilities", "Provenance", "Status", "Detail"})
		for _, raw := range list {
			m, ok := raw.(map[string]any)
			if !ok {
				tbl.AppendRow(table.Row{fmt.Sprint(raw), "-", "-", "-", "-"})
				continue
			}
			tbl.AppendRow(table.Row{
				mapStr(m, "name", "Name", "id"),
				dash(connectorCapabilities(m)),
				dash(mapStr(m, "provenance", "Provenance")),
				colorStatus(w, connectorStatus(m)),
				dash(connectorDetail(m)),
			})
		}
		renderTable(tbl)
		return nil
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

// connectorCapabilities lists the block keys present in a definition row.
func connectorCapabilities(m map[string]any) string {
	var caps []string
	for _, k := range []string{"actions", "events", "channel"} {
		if b, ok := m[k].(map[string]any); ok && len(b) > 0 {
			caps = append(caps, k)
		}
	}
	return strings.Join(caps, ",")
}

// connectorStatus reads the lifecycle status; a definition without events has
// no supervisor and no status.
func connectorStatus(m map[string]any) string {
	st, _ := m["status"].(string)
	return st
}

// connectorDetail names the process behind the row: the events command, the
// MCP server, or the CLI commands.
func connectorDetail(m map[string]any) string {
	if ev, ok := m["events"].(map[string]any); ok {
		if cmd := mapStr(ev, "command"); cmd != "" {
			return cmd
		}
	}
	acts, ok := m["actions"].(map[string]any)
	if !ok {
		return strings.Join(connectorCLICommands(m), " ")
	}
	if mcp, ok := acts["mcp"].(map[string]any); ok {
		if u := mapStr(mcp, "url"); u != "" {
			return u
		}
		if cmd := mapStr(mcp, "command"); cmd != "" {
			args := stringList(mcp["args"])
			return strings.TrimSpace(cmd + " " + strings.Join(args, " "))
		}
	}
	if cli, ok := acts["cli"].(map[string]any); ok {
		return strings.Join(stringList(cli["commands"]), " ")
	}
	return ""
}

func connectorCLICommands(m map[string]any) []string {
	acts, _ := m["actions"].(map[string]any)
	cli, _ := acts["cli"].(map[string]any)
	return stringList(cli["commands"])
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, raw := range list {
		if s, ok := raw.(string); ok {
			out = append(out, s)
		}
	}
	return out
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
	return encodeJSON(c, body)
}

func runConnectorSet(_ context.Context, c *cli.Command) error {
	if c.NArg() < 1 {
		return fmt.Errorf("usage: trebi connector set <name> [flags]")
	}
	name := c.Args().Get(0)
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	row, err := fetchConnectorRow(client, name)
	if err != nil {
		return err
	}
	patch := request.ConnectorPatch{Name: name}
	touched := false

	if c.IsSet("env") {
		env := existingEnv(row)
		kv, err := parseKeyValues(c.StringSlice("env"))
		if err != nil {
			return fmt.Errorf("env %w", err)
		}
		for k, v := range kv {
			env[k] = v
		}
		patch.Env = env
		touched = true
	}
	if c.IsSet("cwd") {
		patch.Cwd = c.String("cwd")
		touched = true
	}

	switch {
	case c.Bool("no-events"):
		patch.Events = json.RawMessage("null")
		touched = true
	case eventsFlagsSet(c):
		ev := existingBlock(row, "events")
		if c.IsSet("events-command") {
			ev["command"] = c.String("events-command")
		}
		if c.IsSet("format") {
			ev["format"] = c.String("format")
		}
		if c.IsSet("restart") {
			ev["restart"] = c.String("restart")
		}
		if c.IsSet("poll") {
			ev["poll_ms"] = c.Duration("poll").Milliseconds()
		}
		if c.IsSet("id") {
			ev["id"] = c.String("id")
		}
		if c.IsSet("ts") {
			ev["ts"] = c.String("ts")
		}
		if c.IsSet("identity") {
			ev["identity"] = c.String("identity")
		}
		if c.IsSet("catchup-command") || c.IsSet("catchup-max") || c.IsSet("catchup-timeout") {
			cu := existingBlock(ev, "catchup")
			if c.IsSet("catchup-command") {
				cu["command"] = c.String("catchup-command")
			}
			if c.IsSet("catchup-max") {
				cu["max_events"] = c.Int("catchup-max")
			}
			if c.IsSet("catchup-timeout") {
				cu["timeout_ms"] = c.Duration("catchup-timeout").Milliseconds()
			}
			ev["catchup"] = cu
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		patch.Events = raw
		touched = true
	}

	switch {
	case c.Bool("no-actions"):
		patch.Actions = json.RawMessage("null")
		touched = true
	case actionsFlagsSet(c):
		acts := existingBlock(row, "actions")
		if mcpFlagsSet(c) {
			mcp := existingBlock(acts, "mcp")
			if c.IsSet("mcp-command") {
				mcp["command"] = c.String("mcp-command")
			}
			if c.IsSet("mcp-url") {
				mcp["url"] = c.String("mcp-url")
			}
			if c.IsSet("mcp-arg") {
				mcp["args"] = c.StringSlice("mcp-arg")
			}
			if c.IsSet("mcp-env") {
				env, err := parseKeyValues(c.StringSlice("mcp-env"))
				if err != nil {
					return fmt.Errorf("mcp-env %w", err)
				}
				mcp["env"] = env
			}
			if c.IsSet("mcp-header") {
				headers, err := parseKeyValues(c.StringSlice("mcp-header"))
				if err != nil {
					return fmt.Errorf("mcp-header %w", err)
				}
				mcp["headers"] = headers
			}
			acts["mcp"] = mcp
		}
		if c.IsSet("cli-command") || c.IsSet("skill") {
			cli := existingBlock(acts, "cli")
			if c.IsSet("cli-command") {
				cli["commands"] = c.StringSlice("cli-command")
			}
			if c.IsSet("skill") {
				cli["skill"] = c.String("skill")
			}
			acts["cli"] = cli
		}
		raw, err := json.Marshal(acts)
		if err != nil {
			return err
		}
		patch.Actions = raw
		touched = true
	}

	switch {
	case c.Bool("no-channel"):
		patch.Channel = json.RawMessage("null")
		touched = true
	case channelFlagsSet(c):
		ch := existingBlock(row, "channel")
		for flag, key := range map[string]string{
			"channel-ref": "ref", "send": "send", "typing": "typing", "ack": "ack",
		} {
			if c.IsSet(flag) {
				ch[key] = c.String(flag)
			}
		}
		raw, err := json.Marshal(ch)
		if err != nil {
			return err
		}
		patch.Channel = raw
		touched = true
	}

	if !touched {
		return fmt.Errorf("no fields given; pass flags or --no-events/--no-actions/--no-channel")
	}
	if err := client.DoHTTP(patch.Build(), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s set\n", name)
	return nil
}

// fetchConnectorRow reads one connector row; an unknown name returns an empty
// map so set can create it.
func fetchConnectorRow(client *APIClient, name string) (map[string]any, error) {
	var row map[string]any
	err := client.GetJSON(request.BuildConnectorGetPath(name), &row)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return map[string]any{}, nil
		}
		return nil, err
	}
	return row, nil
}

// existingBlock returns a copy of one definition block so a patch overlays
// the stored value instead of replacing it with only the flags given.
func existingBlock(row map[string]any, key string) map[string]any {
	block, ok := row[key].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	out := make(map[string]any, len(block))
	for k, v := range block {
		out[k] = v
	}
	return out
}

// existingEnv reads the stored env map so --env merges instead of replacing.
func existingEnv(row map[string]any) map[string]string {
	block, _ := row["env"].(map[string]any)
	out := make(map[string]string, len(block))
	for k, v := range block {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func eventsFlagsSet(c *cli.Command) bool {
	for _, f := range []string{"events-command", "format", "restart", "poll", "id", "ts", "identity",
		"catchup-command", "catchup-max", "catchup-timeout"} {
		if c.IsSet(f) {
			return true
		}
	}
	return false
}

func mcpFlagsSet(c *cli.Command) bool {
	for _, f := range []string{"mcp-command", "mcp-arg", "mcp-env", "mcp-url", "mcp-header"} {
		if c.IsSet(f) {
			return true
		}
	}
	return false
}

func actionsFlagsSet(c *cli.Command) bool {
	return mcpFlagsSet(c) || c.IsSet("cli-command") || c.IsSet("skill")
}

func channelFlagsSet(c *cli.Command) bool {
	for _, f := range []string{"channel-ref", "send", "typing", "ack"} {
		if c.IsSet(f) {
			return true
		}
	}
	return false
}

func runConnectorApply(_ context.Context, c *cli.Command) error {
	path := c.String("file")
	if path == "" {
		return fmt.Errorf("usage: trebi connector apply --file <path>")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	def := map[string]any{}
	if err := json.Unmarshal(raw, &def); err != nil {
		def = map[string]any{}
		if yerr := yaml.Unmarshal(raw, &def); yerr != nil {
			return fmt.Errorf("parse %s: %v", path, yerr)
		}
	}
	name, _ := def["name"].(string)
	if name == "" && len(def) == 1 {
		// connectors.yaml form: a one-entry name: definition map.
		for k, v := range def {
			if m, ok := v.(map[string]any); ok {
				name, def = k, m
			}
		}
	}
	if name == "" {
		return fmt.Errorf("%s: add a top-level name, or one name: definition entry", path)
	}
	def["name"] = name
	if err := normalizeDurationKeys(def); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildConnectorReplace(name, def), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s applied\n", name)
	return nil
}

// normalizeDurationKeys maps the YAML text forms (poll, timeout,
// typing_interval, typing_max) onto the JSON millisecond keys the API reads.
func normalizeDurationKeys(def map[string]any) error {
	if ev, ok := def["events"].(map[string]any); ok {
		if err := moveDurationKey(ev, "poll", "poll_ms"); err != nil {
			return err
		}
		if cu, ok := ev["catchup"].(map[string]any); ok {
			if err := moveDurationKey(cu, "timeout", "timeout_ms"); err != nil {
				return err
			}
		}
	}
	if ch, ok := def["channel"].(map[string]any); ok {
		if err := moveDurationKey(ch, "typing_interval", "typing_interval_ms"); err != nil {
			return err
		}
		if err := moveDurationKey(ch, "typing_max", "typing_max_ms"); err != nil {
			return err
		}
	}
	return nil
}

func moveDurationKey(m map[string]any, from, to string) error {
	v, ok := m[from]
	if !ok {
		return nil
	}
	delete(m, from)
	switch t := v.(type) {
	case string:
		d, err := time.ParseDuration(strings.TrimSpace(t))
		if err != nil {
			return fmt.Errorf("%s: %w", from, err)
		}
		m[to] = d.Milliseconds()
	case int, int64, float64:
		m[to] = t
	default:
		return fmt.Errorf("%s: want a duration such as 15m", from)
	}
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
		return encodeJSON(c, body)
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
		return encodeJSON(c, body)
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
	return encodeJSON(c, body)
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
