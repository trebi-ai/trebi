package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func jobCommand() *cli.Command {
	return &cli.Command{
		Name:  "job",
		Usage: "manage jobs",
		Commands: append(jobManageSubcommands(),
			runCommand(),
			jobFileSubcommand(),
			&cli.Command{
				Name:      "status",
				Usage:     "show job + latest run summary",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				},
				Action: runJobStatus,
			},
			&cli.Command{
				Name:      "tail",
				Usage:     "tail log of the latest run for a job",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "lines", Aliases: []string{"n"}, Value: 200, Usage: "trailing lines"},
				},
				Action: runJobTail,
			},
			&cli.Command{
				Name:      "pause",
				Usage:     "pause (disable) a job",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{ifMatchFlag()},
				Action:    runJobPause,
			},
			&cli.Command{
				Name:      "unpause",
				Usage:     "unpause a job",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{ifMatchFlag()},
				Action:    runJobUnpause,
			},
			&cli.Command{
				Name:      "rename",
				Usage:     "rename the last part of a job name; the folder moves and the ID stays",
				ArgsUsage: "<name> <to>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				},
				Action: runJobRename,
			},
			&cli.Command{
				Name:      "move",
				Usage:     "give a job another agent or folder; the ID, schedules, state, memory, and runs stay",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
					&cli.StringFlag{Name: "agent", Usage: "new owner agent; omit to keep the owner"},
					&cli.BoolFlag{Name: "no-agent", Usage: "take the job away from its agent"},
					&cli.StringFlag{Name: "folder", Usage: "folder under the agent; \"\" removes it; omit to keep it"},
					&cli.BoolFlag{Name: "dry-run", Usage: "show what changes for the job; do not move"},
					&cli.BoolFlag{Name: "json", Usage: "print the reply as JSON"},
				},
				Action: runJobMove,
			},
			&cli.Command{
				Name:      "validate",
				Usage:     "validate job frontmatter",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runJobValidate,
			},
			memorySubcommands(jobMemoryOwner()),
		),
	}
}

// jobManageSubcommands returns the job definition commands: list, show, create, delete, schedule.
func jobManageSubcommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "list",
			Usage: "list jobs",
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				&cli.BoolFlag{Name: "detailed", Aliases: []string{"d"}, Usage: "extra columns (path, status)"},
				&cli.BoolFlag{Name: "schedule", Usage: "add SPEC, TIMEZONE, NEXT, LAST columns"},
				&cli.StringFlag{Name: "kind", Value: "job", Usage: "job|agent|all (default job)"},
				&cli.StringFlag{Name: "search", Usage: "substring match on job name"},
				&cli.StringFlag{Name: "harness", Usage: "harness filter (claude, codex, …)"},
				&cli.BoolFlag{Name: "active", Usage: "keep only active (or, with --active=false, paused) jobs"},
			},
			Action: runJobList,
		},
		{
			Name:      "show",
			Usage:     "show job detail",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON (always on)"},
			},
			Action: runJobShow,
		},
		{
			Name:      "create",
			Usage:     "create a job (--file or stdin for full text, or --prompt and a trigger flag)",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "file", Usage: "JOB.md path"},
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				&cli.StringFlag{Name: "prompt", Usage: "job body; the daemon composes the front matter"},
				&cli.StringFlag{Name: "harness", Usage: "harness that runs the job"},
				&cli.StringFlag{Name: "description", Usage: "job description"},
				&cli.StringFlag{Name: "cron", Usage: "cron trigger, for example \"0 3 * * *\""},
				&cli.StringFlag{Name: "every", Usage: "interval trigger, for example 15m"},
				&cli.StringFlag{Name: "on", Usage: "event trigger: the connector name"},
				&cli.StringFlag{Name: "match", Usage: "event match expression, with --on"},
				&cli.StringFlag{Name: "batch", Usage: "with --on, run once per window with all new events, for example 10m"},
				&cli.StringFlag{Name: "agent", Usage: "owning agent short name; lands the job under agents/<name>/"},
				&cli.StringFlag{Name: "folder", Usage: "folder of the job under its agent, with --agent"},
				&cli.BoolFlag{Name: "validate", Usage: "check the definition; write nothing"},
			},
			Action: runJobCreate,
		},
		{
			Name:      "set",
			Usage:     "replace JOB.md (content via --file or stdin)",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "file", Usage: "JOB.md path"},
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				ifMatchFlag(),
			},
			Action: runJobSet,
		},
		{
			Name:      "patch",
			Usage:     "patch JOB.md frontmatter fields",
			ArgsUsage: "<name> KEY=VALUE …",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
				ifMatchFlag(),
			},
			Action: runJobPatch,
		},
		{
			Name:      "delete",
			Usage:     "delete a job file (JOB.md); --purge removes the folder",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				forceFlag("required; deletes the job definition"),
				&cli.BoolFlag{Name: "purge", Usage: "remove the job folder, not only JOB.md"},
				ifMatchFlag(),
			},
			Action: runJobDelete,
		},
		{
			Name:      "schedule",
			Usage:     "set schedule: enable|disable|cron|@every",
			ArgsUsage: "<name> <value>",
			Action:    runJobSchedule,
		},
	}
}

func runJobList(_ context.Context, c *cli.Command) error {
	kind, err := request.ParseJobListKind(c.String("kind"))
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if kind == "agent" {
		return runListAgents(c, client, "jobs", c.Bool("all"))
	}
	active := ""
	if c.IsSet("active") {
		active = strconv.FormatBool(c.Bool("active"))
	}
	path := request.BuildJobsListPath(workspaceFor(c), c.String("search"), c.String("harness"), active)
	var body struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	body.Jobs = request.FilterJobsByKindFlag(body.Jobs, kind)
	if c.Bool("schedule") {
		sched, err := client.ScheduleIndex()
		if err != nil {
			return err
		}
		for _, j := range body.Jobs {
			cols := sched[jobScheduleKey(j)]
			j["spec"] = cols.Spec
			j["timezone"] = cols.Timezone
			j["next_due_at"] = cols.Next
			j["last_fired_at"] = cols.Last
		}
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	showKind := kind == "all"
	headers := []string{}
	if showKind {
		headers = append(headers, "Kind")
	}
	headers = append(headers, "Name", "Workspace", "Harness", "Active")
	if c.Bool("detailed") {
		headers = append(headers, "Status", "Path")
	}
	if c.Bool("schedule") {
		headers = append(headers, "Spec", "Timezone", "Next", "Last")
	}
	tbl := newTable(w, headers)
	for _, j := range body.Jobs {
		row := table.Row{}
		if showKind {
			row = append(row, dash(mapStr(j, "kind")))
		}
		row = append(row,
			mapStr(j, "name"),
			mapStr(j, "workspace_id", "workspace"),
			dash(mapStr(j, "harness")),
			colorStatus(w, mapStr(j, "active")),
		)
		if c.Bool("detailed") {
			row = append(row, dash(mapStr(j, "status")), dash(mapStr(j, "path")))
		}
		if c.Bool("schedule") {
			row = append(row,
				dash(mapStr(j, "spec")),
				dash(mapStr(j, "timezone")),
				dash(mapStr(j, "next_due_at")),
				dash(mapStr(j, "last_fired_at")),
			)
		}
		tbl.AppendRow(row)
	}
	renderTable(tbl)
	return nil
}

// JobScheduleCols is the extra job-list columns from GET /v1/schedules.
type JobScheduleCols struct {
	Spec, Timezone, Next, Last string
}

func jobScheduleKey(j map[string]any) string {
	return mapStr(j, "workspace_id", "workspace") + "/" + mapStr(j, "name")
}

func indexSchedules(rows []map[string]any) map[string]JobScheduleCols {
	out := map[string]JobScheduleCols{}
	for _, s := range rows {
		key := mapStr(s, "workspace") + "/" + mapStr(s, "job")
		cur := out[key]
		spec := mapStr(s, "spec")
		if cur.Spec == "" {
			cur.Spec = spec
		} else if spec != "" {
			cur.Spec = cur.Spec + ", " + spec
		}
		tz := mapStr(s, "timezone")
		if cur.Timezone == "" {
			cur.Timezone = tz
		} else if tz != "" && cur.Timezone != tz && !strings.Contains(cur.Timezone, tz) {
			cur.Timezone = cur.Timezone + ", " + tz
		}
		next := mapStr(s, "next_due_at")
		if cur.Next == "" || (next != "" && next < cur.Next) {
			cur.Next = next
		}
		last := mapStr(s, "last_fired_at")
		if last > cur.Last {
			cur.Last = last
		}
		out[key] = cur
	}
	return out
}

func runJobShow(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := request.BuildJobGetPath(c.Args().Get(0), workspaceFlag(c))
	var body map[string]any
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	return encodeJSON(c, body)
}

func runJobCreate(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	spec := request.JobSpec{
		Name: name, Workspace: workspaceFlag(c), Prompt: c.String("prompt"),
		Harness: c.String("harness"), Description: c.String("description"),
		Cron: c.String("cron"), Every: c.String("every"),
		Event: c.String("on"), Match: c.String("match"), Batch: c.String("batch"), Agent: c.String("agent"),
		Folder: c.String("folder"),
	}
	switch {
	case c.String("file") != "" || stdinPiped():
		read, err := readFileOrStdin(c.String("file"))
		if err != nil {
			return err
		}
		spec.Content = read
	case spec.Prompt == "":
		// A template with no harness lets the daemon apply defaults.command.
		spec.Content = "---\n---\n\nDo the task.\n"
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if c.Bool("validate") {
		var body map[string]any
		if err := client.DoHTTP(spec.BuildValidate(), &body); err != nil {
			return err
		}
		return encodeJSON(c, body)
	}
	return client.DoHTTP(spec.Build(), nil)
}

func runJobSet(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	content, err := readFileOrStdin(c.String("file"))
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildJobPutMarkdown(name, workspaceFlag(c), content, c.String("if-match")), nil)
}

func runJobPatch(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" || c.NArg() < 2 {
		return fmt.Errorf("usage: trebi job patch <name> KEY=VALUE …")
	}
	fields, err := parseKeyValues(c.Args().Tail())
	if err != nil {
		return err
	}
	patch := make(map[string]any, len(fields))
	for k, v := range fields {
		patch[k] = patchValue(k, v)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildJobPatch(name, workspaceFlag(c), patch, c.String("if-match")), nil)
}

func runJobDelete(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	confirm, err := confirmed(c, "deletes job "+name)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := request.BuildJobDelete(name, workspaceFor(c), c.String("if-match"), c.Bool("purge"), confirm)
	if err := client.DoHTTP(req, nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "deleted %s\n", name)
	return nil
}

func runJobPause(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildJobPause(c.Args().Get(0), workspaceFor(c), c.String("if-match"), true), nil)
}

func runJobUnpause(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildJobPause(c.Args().Get(0), workspaceFor(c), c.String("if-match"), false), nil)
}

func runJobRename(_ context.Context, c *cli.Command) error {
	args := c.Args().Slice()
	if len(args) != 2 {
		return fmt.Errorf("usage: trebi job rename <name> <to>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildJobRename(workspaceFlag(c), args[0], args[1]), nil)
}

func runJobMove(_ context.Context, c *cli.Command) error {
	if c.Args().Len() != 1 {
		return fmt.Errorf("usage: trebi job move <name> [--agent <name> | --no-agent] [--folder <folder>]")
	}
	m := request.JobMove{DryRun: c.Bool("dry-run")}
	switch {
	case c.IsSet("agent") && c.Bool("no-agent"):
		return fmt.Errorf("use --agent or --no-agent, not both")
	case c.Bool("no-agent"):
		none := ""
		m.Agent = &none
	case c.IsSet("agent"):
		agent := c.String("agent")
		m.Agent = &agent
	}
	if c.IsSet("folder") {
		folder := c.String("folder")
		m.Folder = &folder
	}
	if m.Agent == nil && m.Folder == nil {
		return fmt.Errorf("set --agent, --no-agent, or --folder")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out struct {
		From    string   `json:"from"`
		To      string   `json:"to"`
		Folder  string   `json:"folder"`
		Moved   bool     `json:"moved"`
		Changes []string `json:"changes"`
	}
	var raw json.RawMessage
	if err := client.DoHTTP(request.BuildJobMove(workspaceFlag(c), c.Args().First(), m), &raw); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	w := outWriter(c)
	verb := "moved"
	if m.DryRun {
		verb = "would move"
	}
	fmt.Fprintf(w, "%s %s -> %s", verb, out.From, out.To)
	if out.Folder != "" {
		fmt.Fprintf(w, " (folder %s)", out.Folder)
	}
	fmt.Fprintln(w)
	for _, ch := range out.Changes {
		fmt.Fprintf(w, "  - %s\n", ch)
	}
	return nil
}

func runJobValidate(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := request.BuildJobValidatePath(c.Args().Get(0), workspaceFlag(c))
	var body map[string]any
	if err := client.PostJSON(path, nil, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "ok=%v error=%v\n", body["ok"], body["error"])
	warnings, _ := body["warnings"].([]any)
	for _, w := range warnings {
		fmt.Fprintf(outWriter(c), "warning: %v\n", w)
	}
	return nil
}

func jobMemoryOwner() memoryOwner {
	return memoryOwner{
		noun: "job",
		resolve: func(c *cli.Command) (*APIClient, string, string, error) {
			client, err := clientFor(c)
			if err != nil {
				return nil, "", "", err
			}
			return client, c.Args().Get(0), workspaceFor(c), nil
		},
		req: request.JobMemoryRequests(),
	}
}

func runJobSchedule(_ context.Context, c *cli.Command) error {
	if c.NArg() < 2 {
		return fmt.Errorf("usage: trebi job schedule <name> <enable|disable|cron|@every …>")
	}
	name := c.Args().Get(0)
	value := strings.Join(c.Args().Tail(), " ")
	ws := workspaceFor(c)
	plan, err := request.PlanJobSchedule(name, ws, value)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if plan.Mode != "put_markdown" {
		if err := client.DoHTTP(plan.HTTP, nil); err != nil {
			return err
		}
		fmt.Fprintf(outWriter(c), "schedule %s: %s\n", name, plan.Mode)
		return nil
	}
	// GET markdown → rewrite triggers → PUT
	var got map[string]any
	if err := client.GetJSON(request.BuildJobGetPath(name, ws), &got); err != nil {
		return err
	}
	md := mapStr(got, "markdown")
	if md == "" {
		return fmt.Errorf("job has no markdown payload")
	}
	newMD, err := request.ApplyScheduleMarkdown(md, value)
	if err != nil {
		return err
	}
	req := request.BuildJobPutMarkdown(name, ws, newMD, mapStr(got, "hash"))
	if err := client.DoHTTP(req, nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "schedule set: %s → %s\n", name, value)
	return nil
}

func runJobStatus(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws := workspaceFlag(c)
	var job map[string]any
	if err := client.GetJSON(request.BuildJobGetPath(name, ws), &job); err != nil {
		return err
	}
	path := request.RunsQuery{Job: name, Workspace: ws, Kind: "job", Limit: 5}.Path()
	var runsBody struct {
		Runs []map[string]any `json:"runs"`
	}
	_ = client.GetJSON(path, &runsBody) // job status remains useful when history is unavailable
	out := map[string]any{"job": job, "recent_runs": runsBody.Runs}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "Name:      %s\n", mapStr(job, "name"))
	fmt.Fprintf(w, "Workspace: %s\n", mapStr(job, "workspace", "workspace_id"))
	fmt.Fprintf(w, "Harness:   %s\n", mapStr(job, "harness"))
	fmt.Fprintf(w, "Active:    %v\n", job["active"])
	fmt.Fprintf(w, "Path:      %s\n", mapStr(job, "path"))
	if len(runsBody.Runs) > 0 {
		r := runsBody.Runs[0]
		fmt.Fprintf(w, "Last run:  %s  status=%s\n", mapStr(r, "id"), mapStr(r, "status"))
	}
	return nil
}

func runJobTail(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws := workspaceFor(c)
	path := request.RunsQuery{Job: name, Workspace: ws, Kind: "job", Limit: 1}.Path()
	var runsBody struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := client.GetJSON(path, &runsBody); err != nil {
		return err
	}
	if len(runsBody.Runs) == 0 {
		return fmt.Errorf("no runs for job %q", name)
	}
	id := mapStr(runsBody.Runs[0], "id")
	text, err := client.RunLogText(id)
	if err != nil {
		return err
	}
	lines := strings.Split(text, "\n")
	n := c.Int("lines")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	fmt.Fprint(outWriter(c), out)
	if len(out) > 0 && !strings.HasSuffix(out, "\n") {
		fmt.Fprintln(outWriter(c))
	}
	return nil
}

// patchValue decodes contact, notify, env_file, and email_allowlist from JSON, so
// notify='[{"via":"email"}]' sends a list and null removes the key. A plain
// address stays a string.
func patchValue(key, v string) any {
	if key != "contact" && key != "notify" && key != "env_file" && key != "email_allowlist" {
		return v
	}
	var out any
	if json.Unmarshal([]byte(v), &out) != nil {
		return v
	}
	return out
}
