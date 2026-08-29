package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func jobCommand() *cli.Command {
	return &cli.Command{
		Name:  "job",
		Usage: "manage jobs",
		Subcommands: append(jobManageSubcommands(),
			runCommand(),
			jobFileSubcommand(),
			&cli.Command{
				Name:      "status",
				Usage:     "show job + latest run summary",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runJobStatus,
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
				Action:    runJobPause,
			},
			&cli.Command{
				Name:      "unpause",
				Usage:     "unpause a job",
				ArgsUsage: "<name>",
				Action:    runJobUnpause,
			},
			&cli.Command{
				Name:      "validate",
				Usage:     "validate job frontmatter",
				ArgsUsage: "<name>",
				Action:    runJobValidate,
			},
			&cli.Command{
				Name:  "memory",
				Usage: "job MEMORY.md ledger",
				Subcommands: []*cli.Command{
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
						},
						Action: runJobMemoryAdd,
					},
					{
						Name:      "remove",
						Usage:     "delete a ledger entry",
						ArgsUsage: "<name> <entry-id>",
						Action:    runJobMemoryRemove,
					},
					{
						Name:      "set",
						Usage:     "replace MEMORY.md (--file or stdin)",
						ArgsUsage: "<name>",
						Flags:     []cli.Flag{&cli.StringFlag{Name: "file", Usage: "path to MEMORY.md"}},
						Action:    runJobMemorySet,
					},
					{
						Name:      "clear",
						Usage:     "delete MEMORY.md (destroys the ledger)",
						ArgsUsage: "<name>",
						Flags:     []cli.Flag{&cli.BoolFlag{Name: "force", Aliases: []string{"f"}, Usage: "required; destroys the ledger"}},
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
				&cli.StringFlag{Name: "kind", Value: "job", Usage: "job|assistant|all (default job)"},
			},
			Action: runJobList,
		},
		{
			Name:      "show",
			Usage:     "show job detail",
			ArgsUsage: "<name>",
			Action:    runJobShow,
		},
		{
			Name:      "create",
			Usage:     "create a job (content via --file or stdin)",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "file", Usage: "JOB.md path"},
				&cli.StringFlag{Name: "workspace", Usage: "workspace id"},
			},
			Action: runJobCreate,
		},
		{
			Name:      "delete",
			Usage:     "delete a job file (JOB.md); --purge removes the folder",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "force", Aliases: []string{"f"}, Usage: "skip confirm (always true over API)"},
				&cli.BoolFlag{Name: "purge", Usage: "remove the job folder, not only JOB.md"},
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

func runJobList(c *cli.Context) error {
	kind, err := parseJobListKind(c.String("kind"))
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
	path := withQuery("/v1/jobs", workspaceQ(workspaceFor(c)))
	var body struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	body.Jobs = FilterJobsByKindFlag(body.Jobs, kind)
	if c.Bool("schedule") {
		sched, err := loadScheduleIndex(client)
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

// jobScheduleCols is the extra job-list columns from GET /v1/schedules.
type jobScheduleCols struct {
	Spec, Timezone, Next, Last string
}

func jobScheduleKey(j map[string]any) string {
	return mapStr(j, "workspace_id", "workspace") + "/" + mapStr(j, "name")
}

func loadScheduleIndex(client *APIClient) (map[string]jobScheduleCols, error) {
	var body struct {
		Schedules []map[string]any `json:"schedules"`
	}
	if err := client.GetJSON(BuildSchedulesListPath(200), &body); err != nil {
		return nil, err
	}
	return indexSchedules(body.Schedules), nil
}

func indexSchedules(rows []map[string]any) map[string]jobScheduleCols {
	out := map[string]jobScheduleCols{}
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

func runJobShow(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := withQuery("/v1/jobs/"+c.Args().Get(0), workspaceQ(workspaceFor(c)))
	var body map[string]any
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	enc := json.NewEncoder(outWriter(c))
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

func runJobCreate(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	content := "---\nharness: claude\n---\n\nDo the task.\n"
	if f := c.String("file"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		content = string(b)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.PostJSON("/v1/jobs", map[string]any{
		"name": name, "workspace": workspaceFor(c), "content": content,
	}, nil)
}

func runJobDelete(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := BuildJobDelete(name, workspaceFor(c), c.Bool("purge"))
	if err := client.DoHTTP(req, nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "deleted %s\n", name)
	return nil
}

func runJobPause(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildJobPause(c.Args().Get(0), workspaceFor(c), true), nil)
}

func runJobUnpause(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildJobPause(c.Args().Get(0), workspaceFor(c), false), nil)
}

func runJobValidate(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := withQuery("/v1/jobs/"+c.Args().Get(0)+"/validate", workspaceQ(workspaceFor(c)))
	var body map[string]any
	if err := client.PostJSON(path, nil, &body); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "ok=%v error=%v\n", body["ok"], body["error"])
	return nil
}

func runJobMemory(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi job memory show <name>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := withQuery("/v1/jobs/"+name+"/memory", workspaceQ(workspaceFor(c)))
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

func runJobMemoryAdd(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi job memory add <name> --text <text>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := BuildJobMemoryEntryAdd(name, workspaceFor(c), c.String("date"), c.String("text"))
	return client.DoHTTP(req, nil)
}

func runJobMemoryRemove(c *cli.Context) error {
	name, eid := c.Args().Get(0), c.Args().Get(1)
	if name == "" || eid == "" {
		return fmt.Errorf("usage: trebi job memory remove <name> <entry-id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildJobMemoryEntryDelete(name, workspaceFor(c), eid), nil)
}

func runJobMemorySet(c *cli.Context) error {
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
	return client.DoHTTP(BuildJobMemoryPut(name, workspaceFor(c), content), nil)
}

func runJobMemoryClear(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi job memory clear <name>")
	}
	if !c.Bool("force") {
		return fmt.Errorf("refusing to clear memory for %s without --force", name)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(BuildJobMemoryClear(name, workspaceFor(c)), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "cleared memory for %s\n", name)
	return nil
}

func runJobSchedule(c *cli.Context) error {
	if c.NArg() < 2 {
		return fmt.Errorf("usage: trebi job schedule <name> <enable|disable|cron|@every …>")
	}
	name := c.Args().Get(0)
	value := strings.Join(c.Args().Tail(), " ")
	ws := workspaceFor(c)
	plan, err := PlanJobSchedule(name, ws, value)
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
	if err := client.GetJSON(withQuery("/v1/jobs/"+name, workspaceQ(ws)), &got); err != nil {
		return err
	}
	md := mapStr(got, "markdown")
	if md == "" {
		return fmt.Errorf("job has no markdown payload")
	}
	newMD, err := ApplyScheduleMarkdown(md, value)
	if err != nil {
		return err
	}
	req := BuildJobPutMarkdown(name, ws, newMD)
	if err := client.DoHTTP(req, nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "schedule set: %s → %s\n", name, value)
	return nil
}

func runJobStatus(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws := workspaceFor(c)
	var job map[string]any
	if err := client.GetJSON(withQuery("/v1/jobs/"+name, workspaceQ(ws)), &job); err != nil {
		return err
	}
	path := BuildRunsListPath("", name, ws, "job", "", "", "", 5)
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

func runJobTail(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws := workspaceFor(c)
	path := BuildRunsListPath("", name, ws, "job", "", "", "", 1)
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
	text, err := fetchRunLogText(client, id)
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

// fetchRunLogText decodes GET /v1/runs/{id}/log JSON {"log":"…"} or falls back to transcript raw.
func fetchRunLogText(client *APIClient, id string) (string, error) {
	raw, err := client.GetRaw("/v1/runs/" + id + "/log")
	if err == nil {
		var wrap map[string]any
		if json.Unmarshal(raw, &wrap) == nil {
			if _, has := wrap["log"]; has {
				return fmt.Sprint(wrap["log"]), nil
			}
		}
		// Non-JSON envelope: return body as-is.
		if len(raw) > 0 {
			return string(raw), nil
		}
	}
	raw, err = client.GetRaw("/v1/runs/" + id + "/transcript")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// runListAssistants lists the singleton assistant of each workspace.
func runListAssistants(c *cli.Context, client *APIClient) error {
	w := outWriter(c)
	var wss struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := client.GetJSON("/v1/workspaces", &wss); err != nil {
		return err
	}
	var rows []map[string]any
	skipped := 0
	for _, ws := range wss.Workspaces {
		id := mapStr(ws, "id")
		var a map[string]any
		if err := client.GetJSON("/v1/workspaces/"+id+"/assistant", &a); err != nil {
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
		if c.App != nil && c.App.ErrWriter != nil {
			ew = c.App.ErrWriter
		}
		fmt.Fprintf(ew, "%d workspace(s) unreachable\n", skipped)
	}
	return nil
}
