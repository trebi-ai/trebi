package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
				Name:      "validate",
				Usage:     "validate job frontmatter",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runJobValidate,
			},
			&cli.Command{
				Name:  "memory",
				Usage: "job MEMORY.md ledger",
				Commands: []*cli.Command{
					{
						Name:      "show",
						Usage:     "print MEMORY.md",
						ArgsUsage: "<name>",
						Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
						Action:    runJobMemory,
					},
					{
						Name:      "add",
						Usage:     "append a ledger entry",
						ArgsUsage: "<name>",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "text", Usage: "entry text", Required: true},
							&cli.StringFlag{Name: "date", Usage: "YYYY-MM-DD (default today)"},
							ifMatchFlag(),
						},
						Action: runJobMemoryAdd,
					},
					{
						Name:      "delete",
						Aliases:   []string{"remove"},
						Usage:     "delete a ledger entry",
						ArgsUsage: "<name> <entry-id>",
						Flags:     []cli.Flag{ifMatchFlag()},
						Action:    runJobMemoryRemove,
					},
					{
						Name:      "set",
						Usage:     "replace MEMORY.md (--file or stdin)",
						ArgsUsage: "<name>",
						Flags:     []cli.Flag{&cli.StringFlag{Name: "file", Usage: "path to MEMORY.md"}, ifMatchFlag()},
						Action:    runJobMemorySet,
					},
					{
						Name:      "clear",
						Usage:     "delete MEMORY.md (destroys the ledger)",
						ArgsUsage: "<name>",
						Flags:     []cli.Flag{forceFlag("required; destroys the ledger"), ifMatchFlag()},
						Action:    runJobMemoryClear,
					},
				},
			},
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
				&cli.StringFlag{Name: "kind", Value: "job", Usage: "job|assistant|agent|all (default job)"},
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
				&cli.StringFlag{Name: "agent", Usage: "owning agent short name; lands the job under agents/<name>/"},
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
	if kind == "assistant" && c.Bool("schedule") {
		return fmt.Errorf("--schedule is not valid with --kind assistant (assistants have no triggers)")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if kind == "assistant" {
		return runListAssistants(c, client)
	}
	if kind == "agent" {
		return runListAgents(c, client, "jobs")
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
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
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
	enc := json.NewEncoder(outWriter(c))
	enc.SetIndent("", "  ")
	return enc.Encode(body)
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
		Event: c.String("on"), Match: c.String("match"), Agent: c.String("agent"),
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
		patch[k] = v
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
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	fmt.Fprintf(outWriter(c), "ok=%v error=%v\n", body["ok"], body["error"])
	return nil
}

func runJobMemory(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi job memory show <name>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := request.BuildJobMemoryGetPath(name, workspaceFor(c))
	var body map[string]any
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	fmt.Fprint(outWriter(c), mapStr(body, "content"))
	return nil
}

func runJobMemoryAdd(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi job memory add <name> --text <text>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := request.BuildJobMemoryEntryAdd(name, workspaceFor(c), c.String("date"), c.String("text"), c.String("if-match"))
	return client.DoHTTP(req, nil)
}

func runJobMemoryRemove(_ context.Context, c *cli.Command) error {
	name, eid := c.Args().Get(0), c.Args().Get(1)
	if name == "" || eid == "" {
		return fmt.Errorf("usage: trebi job memory remove <name> <entry-id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildJobMemoryEntryDelete(name, workspaceFor(c), eid, c.String("if-match")), nil)
}

func runJobMemorySet(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi job memory set <name> --file <path>")
	}
	content, err := readFileOrStdin(c.String("file"))
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildJobMemoryPut(name, workspaceFor(c), content, c.String("if-match")), nil)
}

func runJobMemoryClear(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi job memory clear <name>")
	}
	confirm, err := confirmed(c, "destroys the memory ledger for "+name)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildJobMemoryClear(name, workspaceFor(c), c.String("if-match"), confirm), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "cleared memory for %s\n", name)
	return nil
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
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(out)
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

// runListAssistants lists the singleton assistant of each workspace.
func runListAssistants(c *cli.Command, client *APIClient) error {
	w := outWriter(c)
	var wss struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := client.GetJSON(request.BuildWorkspacesListPath(0), &wss); err != nil {
		return err
	}
	var rows []map[string]any
	skipped := 0
	for _, ws := range wss.Workspaces {
		id := mapStr(ws, "id")
		var a map[string]any
		if err := client.GetJSON(request.BuildAssistantGetPath(id), &a); err != nil {
			skipped++
			continue
		}
		rows = append(rows, map[string]any{
			"kind": "assistant", "name": mapStr(a, "name", "nickname", "title"),
			"workspace_id": id, "workspace": mapStr(ws, "name"),
			"harness": mapStr(a, "harness"), "path": mapStr(a, "path"),
			"active": mapStr(a, "active"), "status": mapStr(a, "status"),
		})
	}
	if c.Bool("json") {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"jobs": rows}); err != nil {
			return err
		}
	} else {
		headers := []string{"Name", "Workspace", "Harness", "Active"}
		if c.Bool("detailed") {
			headers = append(headers, "Status", "Path")
		}
		tbl := newTable(w, headers)
		for _, j := range rows {
			r := table.Row{
				dash(mapStr(j, "name")),
				mapStr(j, "workspace", "workspace_id"),
				dash(mapStr(j, "harness")),
				colorStatus(w, mapStr(j, "active")),
			}
			if c.Bool("detailed") {
				r = append(r, dash(mapStr(j, "status")), dash(mapStr(j, "path")))
			}
			tbl.AppendRow(r)
		}
		renderTable(tbl)
	}
	if skipped > 0 {
		var ew io.Writer = os.Stderr
		if c.Root().ErrWriter != nil {
			ew = c.Root().ErrWriter
		}
		fmt.Fprintf(ew, "%d workspace(s) unreachable\n", skipped)
	}
	return nil
}
