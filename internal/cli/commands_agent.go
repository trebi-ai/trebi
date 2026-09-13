package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

// agentCommand is the CLI surface of the workspace agents collection. The
// hidden `trebi agent-hook` command is a harness lifecycle hook; it shares
// only the word "agent".
func agentCommand() *cli.Command {
	return &cli.Command{
		Name:  "agent",
		Usage: "folder-scoped agents (agents/<name>/AGENT.md); unrelated to the hidden agent-hook command",
		Commands: append(agentManageSubcommands(),
			agentWakeCommand(),
			agentMemoryCommand(),
		),
	}
}

// agentManageSubcommands returns the agent definition commands: list, create,
// show, set, patch, delete.
func agentManageSubcommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "list",
			Usage: "list agents (all workspaces, or --workspace)",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: func(_ context.Context, c *cli.Command) error {
				client, err := clientFor(c)
				if err != nil {
					return err
				}
				return runListAgents(c, client, "agents")
			},
		},
		{
			Name:      "create",
			ArgsUsage: "<name>",
			Usage:     "create an agent (PUT upsert; a bare body seeds AGENT.md)",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				&cli.StringFlag{Name: "prompt", Usage: "agent body; the daemon composes the front matter"},
				&cli.StringFlag{Name: "harness", Usage: "harness that wakes the agent"},
				&cli.StringFlag{Name: "description", Usage: "agent description"},
				&cli.StringFlag{Name: "cron", Usage: "cron trigger, for example \"0 3 * * *\""},
				&cli.StringFlag{Name: "every", Usage: "interval trigger, for example 15m"},
				&cli.StringFlag{Name: "on", Usage: "event trigger: the connector name"},
				&cli.StringFlag{Name: "match", Usage: "event match expression, with --on"},
				&cli.StringFlag{Name: "file", Usage: "AGENT.md path (full control)"},
			},
			Action: runAgentCreate,
		},
		{
			Name:      "show",
			ArgsUsage: "<name>",
			Usage:     "show one agent (name is the short name)",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
			},
			Action: runAgentShow,
		},
		{
			Name:      "set",
			ArgsUsage: "<name>",
			Usage:     "replace AGENT.md (content via --file or stdin)",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				&cli.StringFlag{Name: "file", Usage: "AGENT.md path"},
				ifMatchFlag(),
			},
			Action: runAgentSet,
		},
		{
			Name:      "patch",
			ArgsUsage: "<name> [--disabled] [KEY=VALUE …]",
			Usage:     "patch AGENT.md frontmatter fields",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				&cli.BoolFlag{Name: "disabled", Usage: "disable the agent (--disabled=false enables it)"},
				ifMatchFlag(),
			},
			Action: runAgentPatch,
		},
		{
			Name:      "delete",
			Aliases:   []string{"remove"},
			ArgsUsage: "<name>",
			Usage:     "delete AGENT.md; --purge removes agents/<name>/ with the owned jobs",
			Flags: []cli.Flag{
				forceFlag("required; deletes the agent definition"),
				&cli.BoolFlag{Name: "purge", Usage: "remove the whole agent folder, owned jobs included"},
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				ifMatchFlag(),
			},
			Action: runAgentDelete,
		},
	}
}

func agentWakeCommand() *cli.Command {
	return &cli.Command{
		Name:      "wake",
		ArgsUsage: "<name>",
		Usage:     "wake the agent (AGENT.md → kind=agent run)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
			&cli.StringFlag{Name: "extra-prompt", Aliases: []string{"e"}, Usage: "append text to the agent prompt (server-side)"},
			&cli.StringFlag{Name: "prompt", Usage: "replace the agent prompt"},
			&cli.StringSliceFlag{Name: "input", Aliases: []string{"i"}, Usage: "KEY=VALUE placeholder (repeatable)"},
			&cli.StringFlag{Name: "harness", Usage: "override the harness"},
			&cli.StringFlag{Name: "conversation", Usage: "attach the run to a conversation id"},
			&cli.BoolFlag{Name: "test", Usage: "dry_run: evaluate gates, print plan, do not launch"},
			&cli.BoolFlag{Name: "attach", Aliases: []string{"a"}, Usage: "after launch: attach (tmux local or WebSocket remote)"},
			&cli.IntFlag{Name: "wait", Usage: "wait milliseconds for the run to end (max 120000)"},
		},
		Action: runAgentWake,
	}
}

func agentMemoryCommand() *cli.Command {
	return &cli.Command{
		Name:  "memory",
		Usage: "agent MEMORY.md ledger",
		Commands: []*cli.Command{
			{
				Name:      "show",
				ArgsUsage: "<name>",
				Usage:     "print MEMORY.md",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runAgentMemory,
			},
			{
				Name:      "add",
				ArgsUsage: "<name>",
				Usage:     "append a ledger entry",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
					&cli.StringFlag{Name: "text", Usage: "entry text", Required: true},
					&cli.StringFlag{Name: "date", Usage: "YYYY-MM-DD (default today)"},
					ifMatchFlag(),
				},
				Action: runAgentMemoryAdd,
			},
			{
				Name:      "delete",
				Aliases:   []string{"remove"},
				ArgsUsage: "<name> <entry-id>",
				Usage:     "delete a ledger entry",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
					ifMatchFlag(),
				},
				Action: runAgentMemoryRemove,
			},
			{
				Name:      "set",
				ArgsUsage: "<name>",
				Usage:     "replace MEMORY.md (--file or stdin)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
					&cli.StringFlag{Name: "file", Usage: "path to MEMORY.md"},
					ifMatchFlag(),
				},
				Action: runAgentMemorySet,
			},
			{
				Name:      "clear",
				ArgsUsage: "<name>",
				Usage:     "delete MEMORY.md (destroys the ledger)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
					forceFlag("required; destroys the ledger"),
					ifMatchFlag(),
				},
				Action: runAgentMemoryClear,
			},
		},
	}
}

func agentNameArg(c *cli.Command) (string, error) {
	name := c.Args().Get(0)
	if name == "" {
		return "", fmt.Errorf("name required")
	}
	return name, nil
}

func runAgentCreate(_ context.Context, c *cli.Command) error {
	name, err := agentNameArg(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	spec := request.AgentSpec{
		Name: name, Workspace: wsID,
		Prompt: c.String("prompt"), Harness: c.String("harness"), Description: c.String("description"),
		Cron: c.String("cron"), Every: c.String("every"),
		Event: c.String("on"), Match: c.String("match"),
	}
	if c.String("file") != "" || stdinPiped() {
		read, err := readFileOrStdin(c.String("file"))
		if err != nil {
			return err
		}
		spec.Content = read
	}
	return client.DoHTTP(spec.Build(), nil)
}

func runAgentShow(_ context.Context, c *cli.Command) error {
	name, err := agentNameArg(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildAgentGetPath(wsID, name), &body); err != nil {
		return err
	}
	return encodeJSON(c, body)
}

func runAgentSet(_ context.Context, c *cli.Command) error {
	name, err := agentNameArg(c)
	if err != nil {
		return err
	}
	content, err := readFileOrStdin(c.String("file"))
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildAgentPut(wsID, name, content, c.String("if-match")), nil)
}

func runAgentPatch(_ context.Context, c *cli.Command) error {
	name, err := agentNameArg(c)
	if err != nil {
		return err
	}
	patch := map[string]any{}
	if c.IsSet("disabled") {
		patch["disabled"] = c.Bool("disabled")
	}
	fields, err := parseKeyValues(c.Args().Tail())
	if err != nil {
		return err
	}
	for k, v := range fields {
		patch[k] = v
	}
	if len(patch) == 0 {
		return fmt.Errorf("usage: trebi agent patch <name> --disabled [KEY=VALUE …]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildAgentPatch(wsID, name, patch, c.String("if-match")), nil)
}

func runAgentDelete(_ context.Context, c *cli.Command) error {
	name, err := agentNameArg(c)
	if err != nil {
		return err
	}
	what := "deletes agent " + name
	if c.Bool("purge") {
		what += " and its whole folder (owned jobs included)"
	}
	confirm, err := confirmed(c, what)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildAgentDelete(wsID, name, c.String("if-match"), c.Bool("purge"), confirm), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "deleted %s\n", name)
	return nil
}

func runAgentWake(ctx context.Context, c *cli.Command) error {
	name, err := agentNameArg(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	inputs, err := parseKeyValues(c.StringSlice("input"))
	if err != nil {
		return err
	}
	spec := request.RunSpec{
		Job:            name,
		Workspace:      wsID,
		Prompt:         c.String("prompt"),
		ExtraPrompt:    c.String("extra-prompt"),
		Inputs:         inputs,
		Harness:        c.String("harness"),
		ConversationID: c.String("conversation"),
		DryRun:         c.Bool("test"),
		WaitMS:         c.Int("wait"),
	}
	var out map[string]any
	if err := client.DoHTTP(spec.BuildAgentWake(), &out); err != nil {
		return err
	}
	if c.Bool("test") {
		return encodeJSON(c, out)
	}
	runID := mapStr(out, "id")
	fmt.Fprintf(outWriter(c), "run %v status=%v kind=agent\n", runID, out["status"])
	if c.Bool("attach") && runID != "" {
		return attachAfterLaunch(ctx, c, client, out)
	}
	return nil
}

func runAgentMemory(_ context.Context, c *cli.Command) error {
	name, err := agentNameArg(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildAgentMemoryGetPath(wsID, name), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprint(outWriter(c), mapStr(body, "content"))
	return nil
}

func runAgentMemoryAdd(_ context.Context, c *cli.Command) error {
	name, err := agentNameArg(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildAgentMemoryEntryAdd(wsID, name, c.String("date"), c.String("text"), c.String("if-match")), nil)
}

func runAgentMemoryRemove(_ context.Context, c *cli.Command) error {
	name, err := agentNameArg(c)
	if err != nil {
		return err
	}
	eid := c.Args().Get(1)
	if eid == "" {
		return fmt.Errorf("usage: trebi agent memory delete <name> <entry-id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildAgentMemoryEntryDelete(wsID, name, eid, c.String("if-match")), nil)
}

func runAgentMemorySet(_ context.Context, c *cli.Command) error {
	name, err := agentNameArg(c)
	if err != nil {
		return err
	}
	content, err := readFileOrStdin(c.String("file"))
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildAgentMemoryPut(wsID, name, content, c.String("if-match")), nil)
}

func runAgentMemoryClear(_ context.Context, c *cli.Command) error {
	name, err := agentNameArg(c)
	if err != nil {
		return err
	}
	confirm, err := confirmed(c, "destroys the memory ledger of agent "+name)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildAgentMemoryClear(wsID, name, c.String("if-match"), confirm), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "cleared memory of %s\n", name)
	return nil
}

// agentRows collects the agents of every workspace, or of the one the want
// filter picks. An unreachable workspace does not hide the rest.
func agentRows(client *APIClient, want string) ([]map[string]any, error) {
	var wss struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := client.GetJSON(request.BuildWorkspacesListPath(0), &wss); err != nil {
		return nil, err
	}
	var rows []map[string]any
	for _, ws := range wss.Workspaces {
		id := mapStr(ws, "id")
		if want != "" && want != id && want != mapStr(ws, "name") {
			continue
		}
		var body struct {
			Agents []map[string]any `json:"agents"`
		}
		if err := client.GetJSON(request.BuildAgentListPath(id), &body); err != nil {
			continue // one unreachable workspace must not hide the rest
		}
		for _, a := range body.Agents {
			a["kind"] = "agent"
			a["workspace"] = mapStr(ws, "name")
			rows = append(rows, a)
		}
	}
	return rows, nil
}

// runListAgents lists the agents of every workspace, or of the one the
// workspace flag picks. jsonKey names the envelope: "agents" for
// `trebi agent list`, "jobs" for `job list --kind agent`.
func runListAgents(c *cli.Command, client *APIClient, jsonKey string) error {
	w := outWriter(c)
	rows, err := agentRows(client, workspaceFlag(c))
	if err != nil {
		return err
	}
	if c.Bool("schedule") {
		sched, err := client.ScheduleIndex()
		if err != nil {
			return err
		}
		// Schedule rows key an agent by its index name (agents/<name>).
		for _, a := range rows {
			cols := sched[mapStr(a, "workspace_id")+"/"+request.AgentIndexName(mapStr(a, "name"))]
			a["spec"] = cols.Spec
			a["timezone"] = cols.Timezone
			a["next_due_at"] = cols.Next
			a["last_fired_at"] = cols.Last
		}
	}
	if c.Bool("json") {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{jsonKey: rows})
	}
	headers := []string{"Name", "Workspace", "Harness", "Active", "Jobs"}
	if c.Bool("detailed") {
		headers = append(headers, "Path")
	}
	if c.Bool("schedule") {
		headers = append(headers, "Spec", "Timezone", "Next", "Last")
	}
	tbl := newTable(w, headers)
	for _, a := range rows {
		jobs := ""
		if n, ok := a["job_count"].(float64); ok {
			jobs = strconv.Itoa(int(n))
		}
		r := table.Row{
			mapStr(a, "name"),
			mapStr(a, "workspace", "workspace_id"),
			dash(mapStr(a, "harness")),
			colorStatus(w, mapStr(a, "active")),
			dash(jobs),
		}
		if c.Bool("detailed") {
			r = append(r, dash(mapStr(a, "path")))
		}
		if c.Bool("schedule") {
			r = append(r,
				dash(mapStr(a, "spec")),
				dash(mapStr(a, "timezone")),
				dash(mapStr(a, "next_due_at")),
				dash(mapStr(a, "last_fired_at")),
			)
		}
		tbl.AppendRow(r)
	}
	renderTable(tbl)
	return nil
}
