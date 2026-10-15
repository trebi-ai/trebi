package cli

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/connectors"
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
				Usage:     "show one connection",
				ArgsUsage: "<connector>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "resolve the connector for this workspace"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runConnectorShow,
			},
			{
				Name:      "set",
				Usage:     "update a connection (PATCH); with --manifest-file, create it when it does not exist",
				ArgsUsage: "<connector>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "slug", Usage: "new slug; jobs, agents, workspace layers, and notify routes that name the old slug change with it"},
					&cli.StringFlag{Name: "title", Usage: "display name; an empty value restores the catalog title"},
					&cli.StringFlag{Name: "version", Usage: "catalog version to install and use; for a link, the version of its branch head"},
					&cli.StringFlag{Name: "track", Usage: "update track: exact|patch|minor|latest"},
					&cli.StringSliceFlag{Name: "input", Aliases: []string{"i"}, Usage: "KEY=VALUE setup input (repeatable; merges into the stored inputs); secret:NAME keeps a reference"},
					&cli.StringFlag{Name: "patch-file", Usage: "YAML or JSON merge patch on the manifest; a file that holds null removes it"},
					&cli.StringFlag{Name: "manifest-file", Usage: "YAML or JSON inline manifest; a file that holds null removes it"},
				},
				Action: runConnectorSet,
			},
			{
				Name:      "apply",
				Usage:     "create or update one connection from a YAML or JSON file",
				ArgsUsage: "--file <path>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "connection file: {type, slug, title, from, version, track, inputs, patch, manifest, scope}"},
					scopeFlag(),
				},
				Action: runConnectorApply,
			},
			{
				Name:      "scope",
				Usage:     "show or set the workspaces that see a connection (global, or a list of workspace names)",
				ArgsUsage: "<connector> [global|<workspace>,<workspace>...]",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runConnectorScope,
			},
			{
				Name:      "delete",
				Aliases:   []string{"remove"},
				Usage:     "delete a connection and its folder",
				ArgsUsage: "<connector>",
				Flags:     []cli.Flag{forceFlag("required; stops the event stream")},
				Action:    runConnectorRemove,
			},
			connectorQueueAlias(),
		}, slices.Concat(connectorCatalogCommands(), connectorVersionCommands(), connectorProcessCommands(), connectorSessionCommands(), connectorOperationCommands())...),
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
			Name:      "check-link",
			Usage:     "check a GitHub link to a connector folder; installs nothing (exit 1 when it cannot install)",
			ArgsUsage: "<link>",
			Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
			Action:    runConnectorCheckLink,
		},
		{
			Name:      "add",
			Aliases:   []string{"install"},
			Usage:     "add a connection of a catalog type, a registry server, or a GitHub link; run it again for another account",
			ArgsUsage: "<type|link>",
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "check-only", Usage: "link: check the folder and install nothing"},
				&cli.BoolFlag{Name: "force", Usage: "link: install when a conformance suite fails"},
				&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "link: do not ask before the install"},
				&cli.StringFlag{Name: "registry", Usage: "registry name"},
				&cli.StringFlag{Name: "version", Usage: "server version (default: latest)"},
				&cli.StringFlag{Name: "track", Usage: "update track of a Trebi catalog connector: exact|patch|minor|latest"},
				&cli.StringFlag{Name: "slug", Usage: "slug of the new connection (default: the type, then <type>-2, ...)"},
				&cli.StringFlag{Name: "title", Usage: "display name of the new connection"},
				&cli.IntFlag{Name: "package", Value: -1, Usage: "package index to install"},
				&cli.IntFlag{Name: "remote", Value: -1, Usage: "remote index to install"},
				&cli.StringSliceFlag{Name: "input", Aliases: []string{"i"}, Usage: "NAME=VALUE for an env, header, argument, or variable input (repeatable); secret:NAME keeps a reference"},
				scopeFlag(),
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
			Usage:     "list the catalog versions of the type of a connection",
			ArgsUsage: "<connector>",
			Flags:     []cli.Flag{jsonFlag()},
			Action:    runConnectorVersions,
		},
		{
			Name:      "check-updates",
			Usage:     "check the catalog for new versions now (every connection with no argument)",
			ArgsUsage: "[connector]",
			Flags:     []cli.Flag{jsonFlag()},
			Action:    runConnectorCheckUpdates,
		},
		{
			Name:      "update",
			Usage:     "move a connection to a catalog version (default: the newest on its track)",
			ArgsUsage: "<connector>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "version", Usage: "catalog version"},
				jsonFlag(),
			},
			Action: runConnectorUpdate,
		},
		{
			Name:      "capabilities",
			Usage:     "show what a connection offers: commands, MCP tools, events, channel features, and skill",
			ArgsUsage: "<connector>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "apply the layer of this workspace"},
				jsonFlag(),
			},
			Action: runConnectorCapabilities,
		},
		{
			Name:   "gc",
			Usage:  "remove installed packages that no connection uses",
			Flags:  []cli.Flag{&cli.BoolFlag{Name: "dry-run", Usage: "list the versions only"}, jsonFlag()},
			Action: runConnectorGC,
		},
		{
			Name:  "package",
			Usage: "install and list connector packages with no connection (for programs such as perception)",
			Commands: []*cli.Command{
				{
					Name:      "install",
					Usage:     "install a package of a catalog type or a GitHub link; creates no connection",
					ArgsUsage: "<type|link>",
					Flags: []cli.Flag{
						&cli.StringFlag{Name: "version", Usage: "catalog version (default: the newest)"},
						&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "link: do not ask before the install"},
						jsonFlag(),
					},
					Action: runConnectorPackageInstall,
				},
				{
					Name:   "list",
					Usage:  "list the installed packages, the connections that use them, and their holders",
					Flags:  []cli.Flag{jsonFlag()},
					Action: runConnectorPackageList,
				},
				{
					Name:      "keep",
					Usage:     "replace the packages that one holder keeps; gc does not remove them",
					ArgsUsage: "<holder> [type@version...]",
					Flags:     []cli.Flag{jsonFlag()},
					Action:    runConnectorPackageKeep,
				},
			},
		},
	}
}

// connectorProcessCommands run and inspect the process of a connection.
func connectorProcessCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:      "connect",
			Usage:     "start a connection (begin listening; inverse of disconnect)",
			ArgsUsage: "<connector>",
			Action:    runConnectorConnect,
		},
		{
			Name:      "disconnect",
			Usage:     "halt the listening process (inverse of connect)",
			ArgsUsage: "<connector>",
			Action:    runConnectorDisconnect,
		},
		{
			Name:      "restart",
			Usage:     "restart a connection",
			ArgsUsage: "<connector>",
			Action:    runConnectorRestart,
		},
		{
			Name:  "cursor",
			Usage: "show or set the connector cursor last_event_id (replay position)",
			Commands: []*cli.Command{
				{
					Name:      "show",
					Usage:     "show last_event_id",
					ArgsUsage: "<connector>",
					Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
					Action:    runConnectorCursorShow,
				},
				{
					Name:      "set",
					Usage:     "set last_event_id",
					ArgsUsage: "<connector> <last-event-id>",
					Action:    runConnectorCursorSet,
				},
				{
					Name:      "reset",
					Usage:     "reset last_event_id to empty (replay from start)",
					ArgsUsage: "<connector>",
					Action:    runConnectorCursorReset,
				},
			},
		},
		{
			Name:      "logs",
			Usage:     "show connector log stream",
			ArgsUsage: "<connector>",
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "follow (SSE; may block)"},
			},
			Action: runConnectorLogs,
		},
		{
			Name:      "test",
			Usage:     "inject a test event into a connection",
			ArgsUsage: "<connector>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "event", Aliases: []string{"e"}, Usage: "event JSON object (default {\"test\":true})"},
			},
			Action: runConnectorTest,
		},
	}
}

// connectorRef reads <connector>: a slug or a connection id.
func connectorRef(c *cli.Command) (request.ConnectorRef, error) {
	ref := c.Args().Get(0)
	if ref == "" {
		return request.ConnectorRef{}, fmt.Errorf("connector required: a slug or a connection id")
	}
	return request.ConnectorRef{Ref: ref}, nil
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
			tbl := newTable(w, []string{"ID", "Status", "Connector", "Job", "Events", "Conversation", "Block", "Run", "Created"})
			for _, it := range body.Items {
				status := mapStr(it, "Status", "status")
				tbl.AppendRow(table.Row{
					mapStr(it, "ID", "id"),
					colorStatus(w, status),
					dash(mapStr(it, "Connector", "connector")),
					dash(mapStr(it, "Job", "job")),
					cmp.Or(mapStr(it, "batch_count"), "1"),
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
		fmt.Fprintf(w, "some connections do not load; writes to them are refused until you fix them:\n  %s\n\n", loadErr)
	}
	list, _ := body["connectors"].([]any)
	tbl := newTable(w, connectorTableHeader())
	for _, raw := range list {
		if m, ok := raw.(map[string]any); ok {
			tbl.AppendRow(connectorTableRow(w, m))
		}
	}
	renderTable(tbl)
	return nil
}

// connectorTableHeader names the columns of connectorTableRow.
func connectorTableHeader() []string {
	return []string{"Slug", "Type", "Title", "Version", "Capabilities", "Status", "Auth"}
}

// connectorTableRow is one list row: slug, type, title, version,
// capabilities, status, and auth state.
func connectorTableRow(w io.Writer, m map[string]any) table.Row {
	auth, _ := m["auth"].(map[string]any)
	return table.Row{
		mapStr(m, "slug", "id"),
		dash(mapStr(m, "type")),
		dash(mapStr(m, "title")),
		dash(mapStr(m, "version")),
		dash(strings.Join(stringList(m["capabilities"]), ",")),
		colorStatus(w, mapStr(m, "status")),
		dash(mapStr(auth, "state")),
	}
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
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildConnectorGetPath(ref.Ref, c.String("workspace")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, connectorTableHeader())
	tbl.AppendRow(connectorTableRow(w, body))
	renderTable(tbl)
	fmt.Fprintf(w, "id: %s\n", mapStr(body, "id"))
	if sc := stringList(body["scope"]); len(sc) > 0 {
		fmt.Fprintf(w, "scope: %s\n", scopeText(sc, client.WorkspaceNames()))
	}
	if f := mapStr(body, "folder"); f != "" {
		fmt.Fprintf(w, "folder: %s\n", f)
	}
	for _, k := range []string{"last_event_at", "last_err", "load_error", "resolve_error"} {
		if v := mapStr(body, k); v != "" {
			fmt.Fprintf(w, "%s: %s\n", k, v)
		}
	}
	links, _ := body["links"].([]any)
	for _, raw := range links {
		if l, ok := raw.(map[string]any); ok {
			fmt.Fprintf(w, "link %s: %s\n", mapStr(l, "label"), mapStr(l, "url"))
		}
	}
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
		return fmt.Errorf("usage: trebi connector add <type|link> [--slug S] [--title T] [flags]")
	}
	inputs, err := parseKeyValues(c.StringSlice("input"))
	if err != nil {
		return fmt.Errorf("input %w", err)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if isGitHubLink(name, c.String("registry")) {
		return connectorAddLink(c, client, name, inputs)
	}
	if c.Bool("check-only") || c.Bool("force") {
		return fmt.Errorf("--check-only and --force apply to a GitHub link only")
	}
	req := request.ConnectorInstall{
		Registry: c.String("registry"), Name: name, Version: c.String("version"), Track: c.String("track"),
		Slug: c.String("slug"), Title: c.String("title"),
		Package: c.Int("package"), Remote: c.Int("remote"), Inputs: inputs,
		Scope: parseScope(c.String("scope")), Workspace: workspaceFor(c),
	}.Build()
	var body map[string]any
	if err := client.DoHTTP(req, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "connection %s added (%s)\n", dash(mapStr(body, "slug")), dash(mapStr(body, "id")))
	return nil
}

// isGitHubLink reports whether the argument of add is a GitHub link. A
// registry name such as ai.example/server has a dot in its owner, which a
// GitHub owner cannot have.
func isGitHubLink(arg, registry string) bool {
	if registry != "" || !strings.Contains(arg, "/") {
		return false
	}
	_, _, err := connectors.ParseGitHubLink(arg)
	return err == nil
}

func runConnectorCheckLink(_ context.Context, c *cli.Command) error {
	link := c.Args().Get(0)
	if link == "" {
		return fmt.Errorf("usage: trebi connector check-link <link> [--json]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var check connectors.SourceCheck
	if err := client.DoHTTP(request.BuildConnectorSourceCheck(link), &check); err != nil {
		return err
	}
	return reportSourceCheck(c, check)
}

// reportSourceCheck writes a link check and exits 1 when the link cannot
// install.
func reportSourceCheck(c *cli.Command, check connectors.SourceCheck) error {
	if c.Bool("json") {
		if err := encodeJSON(c, check); err != nil {
			return err
		}
	} else {
		printSourceCheck(outWriter(c), check)
	}
	if !check.CanInstall {
		return cli.Exit("", 1)
	}
	return nil
}

// connectorAddLink checks a link, asks the user to trust it, and installs
// it with the staging folder of the check.
func connectorAddLink(c *cli.Command, client *APIClient, link string, inputs map[string]string) error {
	if c.String("version") != "" || c.String("track") != "" || c.Int("package") >= 0 || c.Int("remote") >= 0 {
		return fmt.Errorf("a link installs the version of its commit; do not give --version, --track, --package, or --remote")
	}
	var check connectors.SourceCheck
	if err := client.DoHTTP(request.BuildConnectorSourceCheck(link), &check); err != nil {
		return err
	}
	if c.Bool("check-only") || !check.CanInstall {
		return reportSourceCheck(c, check)
	}
	out := outWriter(c)
	if !c.Bool("json") {
		printSourceCheck(out, check)
	}
	if !c.Bool("yes") && !trustLink(c) {
		return fmt.Errorf("not installed")
	}
	req := request.ConnectorInstall{
		Source: &request.ConnectorSource{URL: link, CheckID: check.ID}, Force: c.Bool("force"),
		Slug: c.String("slug"), Title: c.String("title"), Package: -1, Remote: -1, Inputs: inputs,
		Scope: parseScope(c.String("scope")), Workspace: workspaceFor(c),
	}.Build()
	var body map[string]any
	if err := client.DoHTTP(req, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(out, "connection %s added (%s)\n", dash(mapStr(body, "slug")), dash(mapStr(body, "id")))
	return nil
}

// trustLink asks once before a program from a link runs on this computer.
func trustLink(c *cli.Command) bool {
	var in io.Reader = os.Stdin
	if c.Root().Reader != nil {
		in = c.Root().Reader
	}
	var ew io.Writer = os.Stderr
	if c.Root().ErrWriter != nil {
		ew = c.Root().ErrWriter
	}
	fmt.Fprint(ew, "Trebi did not review this connector. It runs on this computer. Install? [y/N] ")
	line, _ := bufio.NewReader(in).ReadString('\n') // EOF reads as no
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// printSourceCheck writes the report of a link check.
func printSourceCheck(w io.Writer, check connectors.SourceCheck) {
	name, version := "-", "-"
	if check.Entry != nil {
		name, version = check.Entry.Name, check.Entry.Version
	}
	fmt.Fprintf(w, "connector: %s\nversion:   %s\ncommit:    %s\nsource:    %s\n", name, version, dash(check.Source.Commit), check.URL)
	if len(check.Found) > 0 {
		fmt.Fprintf(w, "connectors in sub-folders: %s\n", strings.Join(check.Found, ", "))
	}
	tbl := newTable(w, []string{"Group", "Status", "Message"})
	for _, r := range check.Results {
		tbl.AppendRow(table.Row{r.Group, r.Status, r.Message})
	}
	renderTable(tbl)
	verdict := "can install: yes"
	if !check.CanInstall {
		verdict = "can install: no"
	}
	fmt.Fprintln(w, verdict)
}

func runConnectorVersions(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector versions <connector>")
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
	tbl := newTable(outWriter(c), []string{"Version", "Published", "Installed", "Current", "Connections"})
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
		list, _ := m["connections"].([]any)
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
	tbl := newTable(outWriter(c), []string{"Slug", "From", "To", "Result"})
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
		tbl.AppendRow(table.Row{mapStr(m, "slug", "id"), dash(mapStr(m, "from")), dash(mapStr(m, "to")), result})
	}
	renderTable(tbl)
	return nil
}

func runConnectorUpdate(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector update <connector> [--version V]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorUpdate(name, c.String("version")), &body); err != nil {
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
	if err := client.DoHTTP(request.BuildConnectorCapabilities(ref, c.String("workspace")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "%s %s (%s, %s)\n", mapStr(body, "slug"), dash(mapStr(body, "version")), dash(mapStr(body, "type")), dash(mapStr(body, "id")))
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

func runConnectorPackageInstall(_ context.Context, c *cli.Command) error {
	arg := c.Args().Get(0)
	if arg == "" {
		return fmt.Errorf("usage: trebi connector package install <type|link> [--version V]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	src := request.ConnectorPackageSource{Catalog: arg, Version: c.String("version")}
	if isGitHubLink(arg, "") {
		if src.Version != "" {
			return fmt.Errorf("a link installs the version of its commit; do not give --version")
		}
		var check connectors.SourceCheck
		if err := client.DoHTTP(request.BuildConnectorSourceCheck(arg), &check); err != nil {
			return err
		}
		if !check.CanInstall {
			return reportSourceCheck(c, check)
		}
		if !c.Bool("json") {
			printSourceCheck(outWriter(c), check)
		}
		if !c.Bool("yes") && !trustLink(c) {
			return fmt.Errorf("not installed")
		}
		src = request.ConnectorPackageSource{URL: arg, CheckID: check.ID}
	}
	var pkg connectors.Package
	if err := client.DoHTTP(request.BuildConnectorPackageInstall(src), &pkg); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, pkg)
	}
	fmt.Fprintf(outWriter(c), "package %s installed in %s\n", pkg.Ref(), pkg.Dir)
	return nil
}

func runConnectorPackageList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Packages []connectors.Package `json:"packages"`
	}
	if err := client.DoHTTP(request.BuildConnectorPackages(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	tbl := newTable(outWriter(c), []string{"Type", "Version", "Connections", "Holders", "Program"})
	for _, p := range body.Packages {
		tbl.AppendRow(table.Row{p.Type, p.Version, dash(strings.Join(p.Connections, ", ")), dash(strings.Join(p.Holders, ", ")), dash(p.Bin)})
	}
	renderTable(tbl)
	return nil
}

func runConnectorPackageKeep(_ context.Context, c *cli.Command) error {
	holder := c.Args().Get(0)
	if holder == "" {
		return fmt.Errorf("usage: trebi connector package keep <holder> [type@version...]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorPackagesKeep(holder, c.Args().Slice()[1:]), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	kept, _ := body["packages"].([]any)
	fmt.Fprintf(outWriter(c), "%s keeps %d packages\n", holder, len(kept))
	return nil
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
		return fmt.Errorf("usage: trebi connector set <connector> [flags]")
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
	if patch.Slug != nil {
		name = *patch.Slug
	}
	fmt.Fprintf(outWriter(c), "connector %s set\n", name)
	return nil
}

// connectorPatchFlags reads the set flags into a patch. Inputs hold only the
// flags given; the caller merges the stored inputs.
func connectorPatchFlags(c *cli.Command, name string) (request.ConnectorPatch, error) {
	p := request.ConnectorPatch{Ref: name}
	if c.IsSet("slug") {
		v := c.String("slug")
		p.Slug = &v
	}
	if c.IsSet("title") {
		t := c.String("title")
		p.Title = &t
	}
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
	if p.Slug == nil && p.Title == nil && p.Version == nil && p.Track == nil && p.Inputs == nil && p.Patch == nil && p.Manifest == nil {
		return p, fmt.Errorf("no fields given; pass --slug, --title, --version, --track, --input, --patch-file, or --manifest-file")
	}
	return p, nil
}

// createInlineConnector creates an inline connection from the set flags. The
// argument is its slug, and the manifest name is its type.
func createInlineConnector(c *cli.Command, client *APIClient, p request.ConnectorPatch) error {
	entry := map[string]any{"slug": p.Ref, "from": "inline", "manifest": p.Manifest}
	if p.Slug != nil {
		entry["slug"] = *p.Slug
	}
	if p.Title != nil {
		entry["title"] = *p.Title
	}
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
	fmt.Fprintf(outWriter(c), "connector %s created\n", entry["slug"])
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

// fetchConnectorRow reads one connection row; an unknown ref returns an
// empty map so set and apply can create it.
func fetchConnectorRow(client *APIClient, ref string) (map[string]any, error) {
	var row map[string]any
	err := client.GetJSON(request.BuildConnectorGetPath(ref, ""), &row)
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
	conn, _ := v.(map[string]any)
	slug, _ := conn["slug"].(string)
	if slug == "" {
		return fmt.Errorf("%s: add a top-level slug", path)
	}
	if c.IsSet("scope") {
		conn["scope"] = parseScope(c.String("scope"))
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	row, err := fetchConnectorRow(client, slug)
	if err != nil {
		return err
	}
	if len(row) == 0 {
		if err := client.DoHTTP(request.BuildConnectorCreate(conn), nil); err != nil {
			return err
		}
		fmt.Fprintf(outWriter(c), "connector %s created\n", slug)
		return nil
	}
	p := request.ConnectorPatch{Ref: mapStr(row, "id")}
	for key, dst := range map[string]**string{"title": &p.Title, "version": &p.Version, "track": &p.Track} {
		if s, ok := conn[key].(string); ok {
			*dst = &s
		}
	}
	if in, ok := conn["inputs"].(map[string]any); ok {
		p.Inputs = map[string]string{}
		for k, v := range in {
			p.Inputs[k] = fmt.Sprint(v)
		}
	}
	for key, dst := range map[string]*json.RawMessage{"patch": &p.Patch, "manifest": &p.Manifest} {
		if raw, ok := conn[key]; ok {
			if *dst, err = json.Marshal(raw); err != nil {
				return err
			}
		}
	}
	switch sc := conn["scope"].(type) {
	case string:
		p.Scope = parseScope(sc)
	case []any:
		p.Scope = stringList(sc)
	case []string:
		p.Scope = sc
	}
	if err := client.DoHTTP(p.Build(), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s applied\n", slug)
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
	tbl := newTable(w, []string{"Slug", "ID", "Last Event ID", "Updated"})
	tbl.AppendRow(table.Row{
		dash(mapStr(body, "slug")),
		dash(mapStr(body, "id")),
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
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (event id); pages back from the newest event"},
					&cli.StringFlag{Name: "after", Usage: "event id; list the events after it, oldest first"},
					&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "keep reading and print new events as they arrive (from --after, else from the oldest event)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.StringFlag{Name: "connector", Usage: "filter: a connection slug or id, or a connector type"},
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "filter: events with a trigger outcome in this workspace"},
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

func runEventsList(ctx context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	q := request.EventsQuery{
		Cursor: c.String("cursor"), After: c.String("after"), Follow: c.Bool("follow"),
		Connector: c.String("connector"), Since: c.String("since"), Q: c.String("search"), Limit: c.Int("limit"),
	}
	if ws := strings.TrimSpace(c.String("workspace")); ws != "" {
		q.Workspace = resolveWorkspaceHint(c, ws)
	}
	if q.Follow {
		return followEventsList(ctx, c, client, q.Path())
	}
	var body map[string]any
	if err := client.GetJSON(q.Path(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	tbl := newTable(outWriter(c), []string{"ID", "Connector", "Summary", "Received"})
	events, _ := body["events"].([]any)
	for _, raw := range events {
		e, _ := raw.(map[string]any)
		tbl.AppendRow(table.Row{
			mapStr(e, "id"), mapStr(e, "connector"),
			dash(mapStr(e, "summary")), mapStr(e, "received_at"),
		})
	}
	renderTable(tbl)
	return nil
}

// followEventsList prints each event of an NDJSON follow stream until the
// stream ends or the user stops it. A line with no id is a keepalive.
func followEventsList(ctx context.Context, c *cli.Command, client *APIClient, path string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	w := outWriter(c)
	err := client.StreamLines(ctx, path, func(line []byte) error {
		var e map[string]any
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("decode event: %w", err)
		}
		if mapStr(e, "id") == "" {
			return nil
		}
		if c.Bool("json") {
			_, err := fmt.Fprintf(w, "%s\n", line)
			return err
		}
		_, err := fmt.Fprintf(w, "%s  %s  %s  %s\n", mapStr(e, "id"), mapStr(e, "connector"),
			mapStr(e, "received_at"), dash(mapStr(e, "summary")))
		return err
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
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

func scopeFlag() cli.Flag {
	return &cli.StringFlag{Name: "scope", Usage: "workspaces that see the connection: global, or workspace names separated by commas (default for an account: the current workspace)"}
}

// parseScope reads global or a comma list of workspace names. Empty is nil.
func parseScope(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// scopeText renders a scope with workspace names: global, or the names.
func scopeText(scope []string, names map[string]string) string {
	if len(scope) == 0 || (len(scope) == 1 && scope[0] == connectors.ScopeGlobal) {
		return connectors.ScopeGlobal
	}
	out := make([]string, len(scope))
	for i, id := range scope {
		out[i] = cmp.Or(names[id], id)
	}
	return strings.Join(out, ", ")
}

func runConnectorScope(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var row map[string]any
	if value := c.Args().Get(1); value != "" {
		scope := parseScope(value)
		if len(scope) == 0 {
			return fmt.Errorf("scope: give global or workspace names")
		}
		if err := client.DoHTTP(request.ConnectorPatch{Ref: ref.Ref, Scope: scope}.Build(), &row); err != nil {
			return err
		}
	} else if err := client.GetJSON(request.BuildConnectorGetPath(ref.Ref, ""), &row); err != nil {
		return err
	}
	scope := stringList(row["scope"])
	if c.Bool("json") {
		return encodeJSON(c, map[string]any{"id": row["id"], "slug": row["slug"], "scope": scope})
	}
	fmt.Fprintf(outWriter(c), "%s: %s\n", mapStr(row, "slug", "id"), scopeText(scope, client.WorkspaceNames()))
	return nil
}
