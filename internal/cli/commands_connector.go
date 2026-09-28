package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

func connectorCommand() *cli.Command {
	return &cli.Command{
		Name:  "connector",
		Usage: "manage connectors",
		Commands: append([]*cli.Command{
			{
				Name:  "list",
				Usage: "list connectors",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.StringFlag{Name: "capability", Usage: "filter: actions|events|channel"},
					&cli.StringFlag{Name: "provenance", Usage: "filter: native|managed|inherited"},
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "resolve each row for this workspace"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runConnectorList,
			},
			{
				Name:      "show",
				Usage:     "show one connector",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "resolve the connector for this workspace"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runConnectorShow,
			},
			{
				Name:      "set",
				Usage:     "update a connector (PATCH); with --manifest-file, create it when it does not exist",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "version", Usage: "catalog version"},
					&cli.StringFlag{Name: "track", Usage: "update track: exact|patch|minor|latest"},
					&cli.StringSliceFlag{Name: "input", Aliases: []string{"i"}, Usage: "KEY=VALUE setup input (repeatable; merges into the stored inputs); secret:NAME keeps a reference"},
					&cli.StringFlag{Name: "patch-file", Usage: "YAML or JSON merge patch on the manifest; a file that holds null removes it"},
					&cli.StringFlag{Name: "manifest-file", Usage: "YAML or JSON inline manifest; a file that holds null removes it"},
				},
				Action: runConnectorSet,
			},
			{
				Name:      "apply",
				Usage:     "write one connector entry from a YAML or JSON file (PUT)",
				ArgsUsage: "--file <path>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "entry file: {name, from, version, track, inputs, patch, manifest}"},
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
			connectorQueueAlias(),
		}, slices.Concat(connectorCatalogCommands(), connectorVersionCommands(), connectorProcessCommands(), connectorSessionCommands())...),
	}
}

// connectorCatalogCommands search the MCP registries and install from them.
func connectorCatalogCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:      "search",
			Usage:     "search MCP registries for installable servers",
			ArgsUsage: "<query>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "registry", Usage: "registry name (default: every configured registry)"},
				&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (next_cursor of the previous page)"},
				&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 30, Usage: "max rows"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorSearch,
		},
		{
			Name:      "entry",
			Usage:     "show one registry server: packages, remotes, and their inputs",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "registry", Usage: "registry name"},
				&cli.StringFlag{Name: "version", Usage: "server version (default: latest)"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorEntry,
		},
		{
			Name:      "install",
			Usage:     "install a registry server as a connector",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "registry", Usage: "registry name"},
				&cli.StringFlag{Name: "version", Usage: "server version (default: latest)"},
				&cli.StringFlag{Name: "track", Usage: "update track of a Trebi catalog connector: exact|patch|minor|latest"},
				&cli.StringFlag{Name: "as", Usage: "connector name (default: derived from the server name)"},
				&cli.IntFlag{Name: "package", Value: -1, Usage: "package index to install"},
				&cli.IntFlag{Name: "remote", Value: -1, Usage: "remote index to install"},
				&cli.StringSliceFlag{Name: "input", Aliases: []string{"i"}, Usage: "NAME=VALUE for an env, header, argument, or variable input (repeatable); secret:NAME keeps a reference"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorInstall,
		},
	}
}

// connectorVersionCommands list, check, and apply catalog versions, and
// show what one connector offers.
func connectorVersionCommands() []*cli.Command {
	jsonFlag := func() cli.Flag { return &cli.BoolFlag{Name: "json", Usage: "emit JSON"} }
	return []*cli.Command{
		{
			Name:      "versions",
			Usage:     "list the catalog versions of a connector",
			ArgsUsage: "<name>",
			Flags:     []cli.Flag{jsonFlag()},
			Action:    runConnectorVersions,
		},
		{
			Name:      "check-updates",
			Usage:     "check the catalog for new versions now (every connector with no name)",
			ArgsUsage: "[name]",
			Flags:     []cli.Flag{jsonFlag()},
			Action:    runConnectorCheckUpdates,
		},
		{
			Name:      "update",
			Usage:     "move a connector to a catalog version (default: the newest on its track)",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "version", Usage: "catalog version"},
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "update the override of this workspace"},
				jsonFlag(),
			},
			Action: runConnectorUpdate,
		},
		{
			Name:      "capabilities",
			Usage:     "show what a connector offers: commands, MCP tools, events, channel features, and skill",
			ArgsUsage: "<name>",
			Flags:     append(connectorInstanceFlags(), jsonFlag()),
			Action:    runConnectorCapabilities,
		},
		{
			Name:   "gc",
			Usage:  "remove installed versions that no connector uses",
			Flags:  []cli.Flag{&cli.BoolFlag{Name: "dry-run", Usage: "list the versions only"}, jsonFlag()},
			Action: runConnectorGC,
		},
	}
}

// connectorProcessCommands run and inspect the process of an instance.
func connectorProcessCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:      "connect",
			Usage:     "start a connector (begin listening; inverse of disconnect)",
			ArgsUsage: "<name>",
			Flags:     connectorInstanceFlags(),
			Action:    runConnectorConnect,
		},
		{
			Name:      "disconnect",
			Usage:     "halt the listening process (inverse of connect)",
			ArgsUsage: "<name>",
			Flags:     connectorInstanceFlags(),
			Action:    runConnectorDisconnect,
		},
		{
			Name:      "restart",
			Usage:     "restart a connector",
			ArgsUsage: "<name>",
			Flags:     connectorInstanceFlags(),
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
					Flags:     append(connectorInstanceFlags(), &cli.BoolFlag{Name: "json", Usage: "emit JSON"}),
					Action:    runConnectorCursorShow,
				},
				{
					Name:      "set",
					Usage:     "set last_event_id",
					ArgsUsage: "<name> <last-event-id>",
					Flags:     connectorInstanceFlags(),
					Action:    runConnectorCursorSet,
				},
				{
					Name:      "reset",
					Usage:     "reset last_event_id to empty (replay from start)",
					ArgsUsage: "<name>",
					Flags:     connectorInstanceFlags(),
					Action:    runConnectorCursorReset,
				},
			},
		},
		{
			Name:      "logs",
			Usage:     "show connector log stream",
			ArgsUsage: "<name>",
			Flags: append(connectorInstanceFlags(),
				&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "follow (SSE; may block)"}),
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
	}
}

// connectorInstanceFlags select the instance a command acts on. With
// neither flag, the command uses the global instance.
func connectorInstanceFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "instance", Usage: "instance key: <name> or <name>@<workspace>"},
		&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "use the instance that serves this workspace"},
	}
}

// connectorRef reads <name>, --instance, and --workspace.
func connectorRef(c *cli.Command) (request.ConnectorRef, error) {
	name := c.Args().Get(0)
	if name == "" {
		return request.ConnectorRef{}, fmt.Errorf("name required")
	}
	return request.ConnectorRef{Name: name, Instance: c.String("instance"), Workspace: c.String("workspace")}, nil
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
	path := request.BuildConnectorsListPath(c.Int("limit"), c.String("capability"), c.String("provenance"), c.String("workspace"))
	var body map[string]any
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	if loadErr, ok := body["load_error"].(string); ok && loadErr != "" {
		fmt.Fprintf(w, "connectors.yaml does not load; writes are refused until you fix it:\n  %s\n\n", loadErr)
	}
	list, _ := body["connectors"].([]any)
	tbl := newTable(w, []string{"Name", "Version", "Track", "Capabilities", "Status", "Auth", "Instances"})
	for _, raw := range list {
		if m, ok := raw.(map[string]any); ok {
			tbl.AppendRow(connectorTableRow(w, m))
		}
	}
	renderTable(tbl)
	return nil
}

// connectorTableRow is one list row: name, version, track, capabilities,
// status, auth state, and the count of instances.
func connectorTableRow(w io.Writer, m map[string]any) table.Row {
	instances, _ := m["instances"].([]any)
	auth, _ := m["auth"].(map[string]any)
	return table.Row{
		mapStr(m, "name", "id"),
		dash(mapStr(m, "version")),
		dash(mapStr(m, "track")),
		dash(strings.Join(stringList(m["capabilities"]), ",")),
		colorStatus(w, connectorStatus(m)),
		dash(mapStr(auth, "state")),
		len(instances),
	}
}

// connectorStatus reads the status of the first instance; a connector
// without events has no supervisor and no status.
func connectorStatus(m map[string]any) string {
	if ev, ok := m["events"].(map[string]any); ok {
		return mapStr(ev, "status")
	}
	return mapStr(m, "status")
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
	if err := client.GetJSON(request.BuildConnectorGetPath(name, c.String("workspace")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Name", "Version", "Track", "Capabilities", "Status", "Auth", "Instances"})
	tbl.AppendRow(connectorTableRow(w, body))
	renderTable(tbl)
	if e := mapStr(body, "resolve_error"); e != "" {
		fmt.Fprintf(w, "resolve error: %s\n", e)
	}
	instances, _ := body["instances"].([]any)
	if len(instances) == 0 {
		return nil
	}
	fmt.Fprintln(w)
	inst := newTable(w, []string{"Instance", "Workspace", "Status", "Last Event", "Error"})
	for _, raw := range instances {
		m, _ := raw.(map[string]any)
		inst.AppendRow(table.Row{
			mapStr(m, "key"), dash(mapStr(m, "workspace")), colorStatus(w, mapStr(m, "status")),
			dash(mapStr(m, "last_event_at")), dash(mapStr(m, "last_err")),
		})
	}
	renderTable(inst)
	return nil
}

func runConnectorSearch(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := request.BuildConnectorCatalogPath(c.Args().Get(0), c.String("registry"), c.String("cursor"), c.Int("limit"))
	var body map[string]any
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	entries, _ := body["entries"].([]any)
	tbl := newTable(w, []string{"Registry", "Name", "Version", "Description"})
	for _, raw := range entries {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		tbl.AppendRow(table.Row{
			dash(mapStr(m, "registry")),
			mapStr(m, "name"),
			dash(mapStr(m, "version")),
			dash(mapStr(m, "description")),
		})
	}
	renderTable(tbl)
	printNextCursor(c, body)
	var ew io.Writer = os.Stderr
	if c.Root().ErrWriter != nil {
		ew = c.Root().ErrWriter
	}
	errs, _ := body["registry_errors"].([]any)
	for _, raw := range errs {
		if m, ok := raw.(map[string]any); ok {
			fmt.Fprintf(ew, "warning: registry %s failed: %s\n", mapStr(m, "registry"), mapStr(m, "error"))
		}
	}
	stale, _ := body["stale_registries"].([]any)
	for _, r := range stale {
		fmt.Fprintf(ew, "warning: registry %v did not answer; its rows are from the cache\n", r)
	}
	return nil
}

func runConnectorEntry(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector entry <name> [--registry R] [--version V]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildConnectorCatalogEntryPath(c.String("registry"), name, c.String("version")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "%s %s (registry %s)\n", mapStr(body, "name"), dash(mapStr(body, "version")), dash(mapStr(body, "registry")))
	if d := mapStr(body, "description"); d != "" {
		fmt.Fprintln(w, d)
	}
	tbl := newTable(w, []string{"Target", "Index", "Source", "Inputs"})
	addRows := func(kind, listKey, inputsKey, sourceKey string) {
		list, _ := body[listKey].([]any)
		inputs, _ := body[inputsKey].([]any)
		for i, raw := range list {
			m, _ := raw.(map[string]any)
			var specs []any
			if i < len(inputs) {
				specs, _ = inputs[i].([]any)
			}
			tbl.AppendRow(table.Row{kind, i, dash(mapStr(m, sourceKey)), dash(entryInputs(specs))})
		}
	}
	addRows("package", "packages", "package_inputs", "identifier")
	addRows("remote", "remotes", "remote_inputs", "url")
	renderTable(tbl)
	return nil
}

// entryInputs lists input names; a trailing * marks a required input.
func entryInputs(specs []any) string {
	names := make([]string, 0, len(specs))
	for _, raw := range specs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		n := mapStr(m, "name")
		if n == "" {
			n = mapStr(m, "kind")
		}
		if req, _ := m["required"].(bool); req {
			n += "*"
		}
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

func runConnectorInstall(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector install <name> [flags]")
	}
	inputs, err := parseKeyValues(c.StringSlice("input"))
	if err != nil {
		return fmt.Errorf("input %w", err)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := request.ConnectorInstall{
		Registry: c.String("registry"), Name: name, Version: c.String("version"), Track: c.String("track"), As: c.String("as"),
		Package: c.Int("package"), Remote: c.Int("remote"), Inputs: inputs,
	}.Build()
	var body map[string]any
	if err := client.DoHTTP(req, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "connector %s installed\n", dash(mapStr(body, "name")))
	return nil
}

func runConnectorVersions(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector versions <name>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorVersions(name), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	tbl := newTable(outWriter(c), []string{"Version", "Published", "Installed", "Current", "Workspaces"})
	rows, _ := body["versions"].([]any)
	for _, raw := range rows {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		v := mapStr(m, "version")
		if b, _ := m["yanked"].(bool); b {
			v += " (yanked)"
		}
		var ws []string
		list, _ := m["workspaces"].([]any)
		for _, w := range list {
			ws = append(ws, fmt.Sprint(w))
		}
		tbl.AppendRow(table.Row{v, dash(mapStr(m, "published_at")), yesNo(m["installed"] == true), yesNo(m["current"] == true), dash(strings.Join(ws, ", "))})
	}
	renderTable(tbl)
	return nil
}

func runConnectorCheckUpdates(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorCheckUpdates(c.Args().Get(0)), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	rows, _ := body["results"].([]any)
	if len(rows) == 0 {
		fmt.Fprintln(outWriter(c), "no updates")
		return nil
	}
	tbl := newTable(outWriter(c), []string{"Name", "From", "To", "Result"})
	for _, raw := range rows {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		result := "available"
		if b, _ := m["applied"].(bool); b {
			result = "applied"
		}
		if e := mapStr(m, "error"); e != "" {
			result = "failed: " + e
		}
		tbl.AppendRow(table.Row{mapStr(m, "name"), dash(mapStr(m, "from")), dash(mapStr(m, "to")), result})
	}
	renderTable(tbl)
	return nil
}

func runConnectorUpdate(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector update <name> [--version V] [--workspace W]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorUpdate(name, c.String("version"), c.String("workspace")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "connector %s is at version %s\n", name, dash(mapStr(body, "version")))
	return nil
}

func runConnectorCapabilities(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorCapabilities(ref), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "%s %s (instance %s)\n", mapStr(body, "name"), dash(mapStr(body, "version")), dash(mapStr(body, "instance")))
	fmt.Fprintf(w, "capabilities: %s\n", dash(joinAny(body["capabilities"])))
	if a, ok := body["actions"].(map[string]any); ok {
		cmds, _ := a["commands"].([]any)
		for _, raw := range cmds {
			if m, ok := raw.(map[string]any); ok {
				fmt.Fprintf(w, "command %s: %s\n", mapStr(m, "command"), dashOr(mapStr(m, "path"), "not found"))
			}
		}
		if mcp, ok := a["mcp"].(map[string]any); ok {
			if e := mapStr(mcp, "error"); e != "" {
				fmt.Fprintf(w, "mcp error: %s\n", e)
			}
			for _, key := range []string{"tools", "prompts", "resources"} {
				list, _ := mcp[key].([]any)
				var names []string
				for _, raw := range list {
					if m, ok := raw.(map[string]any); ok {
						names = append(names, mapStr(m, "name"))
					}
				}
				fmt.Fprintf(w, "mcp %s: %s\n", key, dash(strings.Join(names, ", ")))
			}
		}
	}
	if ch, ok := body["channel"].(map[string]any); ok {
		fmt.Fprintf(w, "channel features: %s\n", dash(joinAny(ch["features"])))
	}
	if sk, ok := body["skill"].(map[string]any); ok {
		fmt.Fprintf(w, "skill %s: %s\n", mapStr(sk, "name"), dash(mapStr(sk, "description")))
	}
	return nil
}

func dashOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func runConnectorGC(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorGC(c.Bool("dry-run")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	list, _ := body["removed"].([]any)
	verb := "removed"
	if c.Bool("dry-run") {
		verb = "would remove"
	}
	if len(list) == 0 {
		fmt.Fprintln(outWriter(c), "nothing to remove")
	}
	for _, v := range list {
		fmt.Fprintf(outWriter(c), "%s %v\n", verb, v)
	}
	return nil
}

func runConnectorSet(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector set <name> [flags]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	patch, err := connectorPatchFlags(c, name)
	if err != nil {
		return err
	}
	var row map[string]any
	if c.IsSet("input") || patch.Manifest != nil {
		if row, err = fetchConnectorRow(client, name); err != nil {
			return err
		}
	}
	if len(row) == 0 && patch.Manifest != nil && string(patch.Manifest) != "null" {
		return createInlineConnector(c, client, patch)
	}
	if c.IsSet("input") {
		// The row shows stored values as ***, and the daemon keeps a *** value.
		inputs := map[string]string{}
		stored, _ := row["inputs"].(map[string]any)
		for k, v := range stored {
			inputs[k] = fmt.Sprint(v)
		}
		maps.Copy(inputs, patch.Inputs)
		patch.Inputs = inputs
	}
	if err := client.DoHTTP(patch.Build(), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s set\n", name)
	return nil
}

// connectorPatchFlags reads the set flags into a patch. Inputs hold only the
// flags given; the caller merges the stored inputs.
func connectorPatchFlags(c *cli.Command, name string) (request.ConnectorPatch, error) {
	p := request.ConnectorPatch{Name: name}
	if c.IsSet("version") {
		v := c.String("version")
		p.Version = &v
	}
	if c.IsSet("track") {
		t := c.String("track")
		p.Track = &t
	}
	if c.IsSet("input") {
		kv, err := parseKeyValues(c.StringSlice("input"))
		if err != nil {
			return p, fmt.Errorf("input %w", err)
		}
		p.Inputs = kv
	}
	var err error
	if p.Patch, err = readObjectFile(c.String("patch-file")); err != nil {
		return p, err
	}
	if p.Manifest, err = readObjectFile(c.String("manifest-file")); err != nil {
		return p, err
	}
	if p.Version == nil && p.Track == nil && p.Inputs == nil && p.Patch == nil && p.Manifest == nil {
		return p, fmt.Errorf("no fields given; pass --version, --track, --input, --patch-file, or --manifest-file")
	}
	return p, nil
}

// createInlineConnector creates an inline entry from the set flags.
func createInlineConnector(c *cli.Command, client *APIClient, p request.ConnectorPatch) error {
	entry := map[string]any{"name": p.Name, "from": "inline", "manifest": p.Manifest}
	if p.Version != nil {
		entry["version"] = *p.Version
	}
	if p.Track != nil {
		entry["track"] = *p.Track
	}
	if p.Inputs != nil {
		entry["inputs"] = p.Inputs
	}
	if p.Patch != nil && string(p.Patch) != "null" {
		entry["patch"] = p.Patch
	}
	if err := client.DoHTTP(request.BuildConnectorCreate(entry), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s created\n", p.Name)
	return nil
}

// readObjectFile reads a YAML or JSON file as JSON. An empty path returns
// nil; a file that holds null returns "null".
func readObjectFile(path string) (json.RawMessage, error) {
	if path == "" {
		return nil, nil
	}
	v, err := readDataFile(path)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return json.RawMessage("null"), nil
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("%s: want an object or null", path)
	}
	return json.Marshal(v)
}

// readDataFile parses a JSON or YAML file.
func readDataFile(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		return v, nil
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return v, nil
}

// fetchConnectorRow reads one connector row; an unknown name returns an empty
// map so set can create it.
func fetchConnectorRow(client *APIClient, name string) (map[string]any, error) {
	var row map[string]any
	err := client.GetJSON(request.BuildConnectorGetPath(name, ""), &row)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return map[string]any{}, nil
		}
		return nil, err
	}
	return row, nil
}

func runConnectorApply(_ context.Context, c *cli.Command) error {
	path := c.String("file")
	if path == "" {
		return fmt.Errorf("usage: trebi connector apply --file <path>")
	}
	v, err := readDataFile(path)
	if err != nil {
		return err
	}
	entry, _ := v.(map[string]any)
	name, _ := entry["name"].(string)
	if _, hasFrom := entry["from"]; name == "" && !hasFrom && len(entry) == 1 {
		// connectors.yaml form: a one-entry name: entry map.
		for k, v := range entry {
			if m, ok := v.(map[string]any); ok {
				name, entry = k, m
			}
		}
	}
	if name == "" {
		return fmt.Errorf("%s: add a top-level name, or one name: entry map", path)
	}
	entry["name"] = name
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildConnectorReplace(name, entry), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s applied\n", name)
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
	return doConnectorRef(c, request.BuildConnectorConnect)
}

func runConnectorDisconnect(_ context.Context, c *cli.Command) error {
	return doConnectorRef(c, request.BuildConnectorDisconnect)
}

func runConnectorRestart(_ context.Context, c *cli.Command) error {
	return doConnectorRef(c, request.BuildConnectorRestart)
}

func runConnectorCursorReset(_ context.Context, c *cli.Command) error {
	return doConnectorRef(c, request.BuildConnectorCursorReset)
}

// doConnectorRef sends one request that takes only the connector reference.
func doConnectorRef(c *cli.Command, build func(request.ConnectorRef) request.HTTPRequest) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(build(ref), nil)
}

func runConnectorCursorShow(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorCursorGet(ref), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Name", "Instance", "Last Event ID", "Updated"})
	tbl.AppendRow(table.Row{
		mapStr(body, "name"),
		dash(mapStr(body, "instance")),
		dash(mapStr(body, "last_event_id")),
		dash(mapStr(body, "updated_at")),
	})
	renderTable(tbl)
	return nil
}

func runConnectorCursorSet(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	last := c.Args().Get(1)
	if err != nil || last == "" {
		return fmt.Errorf("usage: trebi connector cursor set <name> <last-event-id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildConnectorCursorSet(ref, last), nil)
}

func runConnectorLogs(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := request.BuildConnectorLogsPath(ref, c.Bool("follow"))
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
				Usage:     "match a stored event against the current triggers again",
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
