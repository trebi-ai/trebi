package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/chains"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

// askPollStep stays under the 30s client timeout.
const askPollStep = 25 * time.Second

// agentCommand is the CLI surface of the workspace agents collection. The
// hidden `trebi agent-hook` command is a harness lifecycle hook; it shares
// only the word "agent".
func agentCommand() *cli.Command {
	return &cli.Command{
		Name:  "agent",
		Usage: "folder-scoped agents (agents/<name>/AGENT.md); unrelated to the hidden agent-hook command",
		Commands: append(agentManageSubcommands(),
			agentAskCommand(chains.KindAsk),
			agentAskCommand(chains.KindHandoff),
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
				&cli.BoolFlag{Name: "all", Usage: "list agents across every workspace"},
				&cli.StringFlag{Name: "for", Usage: "only agents whose name, title, or description has one of these words"},
				&cli.BoolFlag{Name: "archived", Usage: "list only archived agents"},
				&cli.BoolFlag{Name: "include-archived", Usage: "list archived agents with the others"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: func(_ context.Context, c *cli.Command) error {
				client, err := clientFor(c)
				if err != nil {
					return err
				}
				return runListAgents(c, client, "agents", c.Bool("all"))
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
				&cli.StringFlag{Name: "description", Usage: "required: one line that says what to ask this agent for (a --template gives a default)"},
				&cli.StringFlag{Name: "cron", Usage: "cron trigger, for example \"0 3 * * *\""},
				&cli.StringFlag{Name: "every", Usage: "interval trigger, for example 15m"},
				&cli.StringFlag{Name: "title", Usage: "display name"},
				&cli.StringFlag{Name: "accept", Usage: "comma-separated email.accept entries"},
				&cli.StringFlag{Name: "template", Usage: "embedded agent template id"},
				&cli.StringSliceFlag{Name: "input", Usage: "KEY=VALUE template input (repeatable)"},
				&cli.StringFlag{Name: "file", Usage: "AGENT.md path (full control)"},
			},
			Action: runAgentCreate,
		},
		{
			Name:   "templates",
			Usage:  "list embedded agent templates",
			Flags:  []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
			Action: runAgentTemplates,
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
			Usage:     "delete an agent and its folder; an agent with jobs needs --move-jobs-to or --purge",
			Flags: []cli.Flag{
				forceFlag("required; deletes the agent and its folder"),
				&cli.StringFlag{Name: "move-jobs-to", Usage: "move the jobs of the agent to this agent first"},
				&cli.BoolFlag{Name: "purge", Usage: "delete the jobs of the agent with it"},
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				ifMatchFlag(),
			},
			Action: runAgentDelete,
		},
		{
			Name:      "rename",
			ArgsUsage: "<id> <new-id>",
			Usage:     "rename an agent folder and rewrite stored references",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				&cli.BoolFlag{Name: "dry-run", Usage: "show the new email address and the agents that ask this one; do not move"},
			},
			Action: runAgentRename,
		},
		{
			Name:      "archive",
			ArgsUsage: "<id>",
			Usage:     "archive an agent and its jobs; they keep their files and runs but do not start",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				ifMatchFlag(),
			},
			Action: func(_ context.Context, c *cli.Command) error { return runAgentArchive(c, true) },
		},
		{
			Name:      "unarchive",
			ArgsUsage: "<id>",
			Usage:     "restore an archived agent and its jobs",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				ifMatchFlag(),
			},
			Action: func(_ context.Context, c *cli.Command) error { return runAgentArchive(c, false) },
		},
		{
			Name:  "folder",
			Usage: "manage the job folders of an agent",
			Commands: []*cli.Command{{
				Name:      "rename",
				ArgsUsage: "<agent> <from> <to>",
				Usage:     "move every job of the agent from one folder to another; an empty <to> removes the folder",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				},
				Action: runAgentFolderRename,
			}},
		},
	}
}

func agentWakeCommand() *cli.Command {
	return &cli.Command{
		Name:      "wake",
		ArgsUsage: "<name>",
		Usage:     "wake the agent (AGENT.md → kind=agent run)",
		Flags: append([]cli.Flag{
			&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
			&cli.StringFlag{Name: "extra-prompt", Aliases: []string{"e"}, Usage: "append text to the agent prompt (server-side)"},
			&cli.StringFlag{Name: "prompt", Usage: "replace the agent prompt"},
			&cli.StringSliceFlag{Name: "input", Aliases: []string{"i"}, Usage: "KEY=VALUE placeholder (repeatable)"},
			&cli.StringFlag{Name: "harness", Usage: "override the harness"},
			&cli.StringFlag{Name: "conversation", Usage: "attach the run to a conversation id"},
			&cli.BoolFlag{Name: "test", Usage: "dry_run: evaluate gates, print plan, do not launch"},
			&cli.BoolFlag{Name: "attach", Aliases: []string{"a"}, Usage: "after launch: attach (local pane or remote WebSocket screen)"},
		}, launchWaitFlags()...),
		Action: runAgentWake,
	}
}

func agentMemoryCommand() *cli.Command {
	return memorySubcommands(memoryOwner{
		noun: "agent",
		resolve: func(c *cli.Command) (*APIClient, string, string, error) {
			name, err := agentNameArg(c)
			if err != nil {
				return nil, "", "", err
			}
			client, err := clientFor(c)
			if err != nil {
				return nil, "", "", err
			}
			wsID, err := resolveWorkspaceID(c, client)
			if err != nil {
				return nil, "", "", err
			}
			return client, name, wsID, nil
		},
		req: request.AgentMemoryRequests(),
	})
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
		Title: c.String("title"), Template: c.String("template"),
	}
	if raw := c.String("accept"); raw != "" {
		spec.EmailAccept = strings.Split(raw, ",")
	}
	if inputs, err := parseKeyValues(c.StringSlice("input")); err == nil {
		spec.Inputs = inputs
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

// agentAskCommand is `trebi agent ask` or `trebi agent handoff`.
func agentAskCommand(kind chains.Kind) *cli.Command {
	cmd := &cli.Command{
		Name:      string(kind),
		ArgsUsage: "<name> <text>",
		Usage:     "ask an agent of the workspace; inside a run, end your turn after it",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
			&cli.StringFlag{Name: "context", Usage: "what the agent needs to know"},
			&cli.StringFlag{Name: "idempotency-key", Usage: "a repeat with the same key returns the first hop"},
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		},
		Action: func(_ context.Context, c *cli.Command) error { return runAgentAsk(c, kind) },
	}
	if kind == chains.KindHandoff {
		cmd.Usage = "give work to an agent of the workspace; you get no reply"
	} else {
		cmd.Flags = append(cmd.Flags, &cli.DurationFlag{Name: "wait", Usage: "outside a run: wait up to this time for the reply (max 2m)"})
	}
	return cmd
}

func runAgentAsk(c *cli.Command, kind chains.Kind) error {
	args := c.Args().Slice()
	if len(args) < 2 {
		return fmt.Errorf("usage: trebi agent %s <name> <text>", kind)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	body := request.AgentAsk{Text: strings.Join(args[1:], " "), Context: c.String("context"), IdempotencyKey: c.String("idempotency-key")}
	inRun := os.Getenv("TREBI_RUN_TOKEN") != ""
	wait := min(c.Duration("wait"), chains.MaxWait)
	if inRun {
		wait = 0
	}
	// One call stays under the client timeout; the rest of the wait polls.
	body.WaitMS = int(min(wait, askPollStep).Milliseconds())
	req := request.BuildAgentAsk(wsID, args[0], body)
	if kind == chains.KindHandoff {
		req = request.BuildAgentHandoff(wsID, args[0], body)
	}
	var res chains.AskResult
	if err := client.DoHTTP(req, &res); err != nil {
		return err
	}
	if kind == chains.KindAsk && wait > 0 && len(res.Replies) == 0 {
		res.Replies = waitHopReplies(client, res.Hop, time.Now().Add(wait-askPollStep))
	}
	if c.Bool("json") {
		return encodeJSON(c, res)
	}
	printAskResult(outWriter(c), kind, args[0], res, inRun)
	return nil
}

func printAskResult(out io.Writer, kind chains.Kind, to string, res chains.AskResult, inRun bool) {
	verb := "Asked"
	if kind == chains.KindHandoff {
		verb = "Handed off to"
	}
	fmt.Fprintf(out, "%s %s: hop %s (%s)\n", verb, to, res.Hop.ID, res.Hop.Status)
	if res.RequestID != "" {
		fmt.Fprintf(out, "A person must approve more chain work: request %s\n", res.RequestID)
	}
	if inRun && kind == chains.KindAsk {
		fmt.Fprintln(out, "End your turn now. The reply starts your next turn.")
	}
	for _, r := range res.Replies {
		fmt.Fprintf(out, "\nReply from %s (hop %s):\n%s\n", r.From.Agent, r.ID, r.Text)
	}
}

// waitHopReplies polls the chain of an ask for its replies until deadline.
func waitHopReplies(client *APIClient, ask chains.Hop, deadline time.Time) []chains.Hop {
	for time.Now().Before(deadline) {
		var body struct {
			Hops []chains.Hop `json:"hops"`
		}
		if err := client.GetJSON(request.BuildChainGetPath(ask.TraceID), &body); err != nil {
			return nil
		}
		var out []chains.Hop
		for _, h := range body.Hops {
			if h.ReplyOf != ask.ID {
				continue
			}
			var full struct {
				Hop chains.Hop `json:"hop"`
			}
			if client.GetJSON(request.BuildHopGetPath(h.ID), &full) == nil {
				out = append(out, full.Hop)
			}
		}
		if len(out) > 0 {
			return out
		}
		time.Sleep(time.Second)
	}
	return nil
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

func runAgentTemplates(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Templates []map[string]any `json:"templates"`
	}
	if err := client.GetJSON("/v1/agent-templates", &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Title", "Jobs"})
	for _, t := range body.Templates {
		jobs := ""
		if raw, ok := t["jobs"].([]any); ok {
			parts := make([]string, 0, len(raw))
			for _, v := range raw {
				parts = append(parts, fmt.Sprint(v))
			}
			jobs = strings.Join(parts, ", ")
		}
		tbl.AppendRow(table.Row{mapStr(t, "id"), mapStr(t, "title"), jobs})
	}
	renderTable(tbl)
	return nil
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
		patch[k] = patchValue(k, v)
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
	d := request.AgentDelete{MoveJobsTo: c.String("move-jobs-to"), Purge: c.Bool("purge"), IfMatch: c.String("if-match")}
	what := "deletes agent " + name + " and its folder"
	switch {
	case d.MoveJobsTo != "" && d.Purge:
		return fmt.Errorf("use --move-jobs-to or --purge, not both")
	case d.MoveJobsTo != "":
		what = "moves the jobs of agent " + name + " to " + d.MoveJobsTo + ", then " + what
	case d.Purge:
		what += " with its jobs"
	}
	if d.Confirm, err = confirmed(c, what); err != nil {
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
	var out struct {
		Moved []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"moved"`
	}
	if err := client.DoHTTP(request.BuildAgentDelete(wsID, name, d), &out); err != nil {
		return err
	}
	for _, m := range out.Moved {
		fmt.Fprintf(outWriter(c), "moved %s to %s\n", m.From, m.To)
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
	return launchAndMaybeAttach(ctx, c, client, request.RunSpec{Job: name, Workspace: wsID}, "agent", request.RunSpec.BuildAgentWake)
}

func runAgentRename(_ context.Context, c *cli.Command) error {
	args := c.Args().Slice()
	if len(args) != 2 {
		return fmt.Errorf("usage: trebi agent rename <id> <new-id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildAgentRename(wsID, args[0], args[1], c.Bool("dry-run")), nil)
}

func runAgentArchive(c *cli.Command, archive bool) error {
	verb := "unarchive"
	if archive {
		verb = "archive"
	}
	if c.Args().Len() != 1 {
		return fmt.Errorf("usage: trebi agent %s <id>", verb)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	name := c.Args().Get(0)
	if err := client.DoHTTP(request.BuildAgentArchive(wsID, name, c.String("if-match"), archive), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "%sd %s\n", verb, name)
	return nil
}

func runAgentFolderRename(_ context.Context, c *cli.Command) error {
	args := c.Args().Slice()
	if len(args) != 3 {
		return fmt.Errorf("usage: trebi agent folder rename <agent> <from> <to>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	var out struct {
		From string `json:"from"`
		To   string `json:"to"`
		Jobs int    `json:"jobs"`
	}
	if err := client.DoHTTP(request.BuildAgentFolderRename(wsID, args[0], args[1], args[2]), &out); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "renamed folder %s -> %s (%d jobs)\n", out.From, out.To, out.Jobs)
	return nil
}

// agentRows collects the agents of every workspace, or of the one the want
// filter picks. An unreachable workspace does not hide the rest.
func agentRows(client *APIClient, want, forWords, archived string, all bool) ([]map[string]any, error) {
	if all {
		var all struct {
			Agents []map[string]any `json:"agents"`
		}
		if err := client.GetJSON(request.BuildAgentListAllPath("", forWords, archived), &all); err == nil {
			for _, a := range all.Agents {
				a["kind"] = "agent"
				a["workspace"] = mapStr(a, "workspace_id")
			}
			return all.Agents, nil
		}
		// Fall through to per-workspace aggregation for older daemons.
	}
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
		if err := client.GetJSON(request.BuildAgentListPath(id, forWords, archived), &body); err != nil {
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
func runListAgents(c *cli.Command, client *APIClient, jsonKey string, all bool) error {
	w := outWriter(c)
	archived, err := archivedFilter(c)
	if err != nil {
		return err
	}
	rows, err := agentRows(client, workspaceFlag(c), c.String("for"), archived, all)
	if err != nil {
		return err
	}
	if c.Bool("schedule") {
		sched, err := client.ScheduleIndex()
		if err != nil {
			return err
		}
		for _, a := range rows {
			cols := sched[mapStr(a, "workspace_id")+"/"+mapStr(a, "name")]
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
	headers := []string{"Name", "Description", "Workspace", "Harness", "Active", "Jobs"}
	showArchived := archived != ""
	if showArchived {
		headers = append(headers, "Archived")
	}
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
			dash(truncateDisplay(mapStr(a, "description"), 60)),
			mapStr(a, "workspace", "workspace_id"),
			dash(mapStr(a, "harness")),
			colorStatus(w, mapStr(a, "active")),
			dash(jobs),
		}
		if showArchived {
			r = append(r, dash(archivedDate(a)))
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
