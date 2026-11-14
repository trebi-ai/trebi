package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/urfave/cli/v3"
)

func runsCommand() *cli.Command {
	return &cli.Command{
		Name:  "runs",
		Usage: "list and inspect runs",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list runs",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "status", Aliases: []string{"s"}, Usage: "status filter (running, complete, idle, waiting, …)"},
					&cli.StringFlag{Name: "job", Usage: "job name filter"},
					&cli.StringFlag{Name: "owner", Usage: "agent filter: its runs and the runs of its jobs"},
					&cli.StringFlag{Name: "kind", Usage: "kind filter (job|agent)"},
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name filter"},
					&cli.StringFlag{Name: "harness", Usage: "harness filter (claude, grok, cursor, …)"},
					&cli.StringFlag{Name: "model", Aliases: []string{"m"}, Usage: "model filter (tokens_model exact match)"},
					&cli.StringFlag{Name: "conversation", Usage: "substring match on conversation_key (client-side)"},
					&cli.StringFlag{Name: "end-reason", Usage: "end reason filter (sentinel, kill, auth, …)"},
					&cli.StringFlag{Name: "search", Usage: "text match on prompt preview, job, or id"},
					&cli.StringFlag{Name: "since", Usage: "RFC3339 time or a duration such as 24h"},
					&cli.StringFlag{Name: "until", Usage: "RFC3339 time or a duration such as 1h"},
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (run id)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 20, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runRunsList,
			},
			{
				Name:      "attach",
				Usage:     "attach to a run (local pane or remote WebSocket screen)",
				ArgsUsage: "<id>",
				Action:    runRunsAttach,
			},
			{
				Name:      "show",
				Usage:     "show a run",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON (always on)"}},
				Action:    runRunsShow,
			},
			{
				Name:      "wait",
				Usage:     "wait until a run ends, then show it; exit 0 complete, 1 failed, 2 parked, 3 still running",
				ArgsUsage: "<id>",
				Flags: []cli.Flag{
					&cli.DurationFlag{Name: "timeout", Usage: "stop waiting after this long, for example 9m (default: no limit)"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON (always on)"},
				},
				Action: runRunsWait,
			},
			{
				Name:      "watch",
				Usage:     "print the progress of a run until it ends; takes a run id or a job name; exit codes as runs wait",
				ArgsUsage: "<id|job>",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "next", Usage: "job: wait for the next run to start, then watch it"},
					&cli.DurationFlag{Name: "timeout", Usage: "stop watching after this long (default: no limit)"},
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name of the job"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSONL: one frame per line, then the run"},
				},
				Action: runRunsWatch,
			},
			{
				Name:      "turns",
				Usage:     "list the turns of a run (one per harness launch)",
				ArgsUsage: "<id>",
				Action:    runRunsTurns,
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON instead of a table"},
				},
			},
			{
				Name:      "timeline",
				Usage:     "show run timeline",
				ArgsUsage: "<id>",
				Action:    runRunsTimeline,
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON instead of a table"},
				},
			},
			{
				Name:      "transcript",
				Usage:     "show run transcript (parsed turns)",
				ArgsUsage: "<id>",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON (default)"},
					&cli.BoolFlag{Name: "raw", Usage: "print raw ndjson transcript"},
				},
				Action: runRunsTranscript,
			},
			{
				Name:      "logs",
				Aliases:   []string{"log"},
				Usage:     "print the harness log of one run",
				ArgsUsage: "<id>",
				Action: func(_ context.Context, c *cli.Command) error {
					id := c.Args().Get(0)
					if id == "" {
						return fmt.Errorf("usage: trebi runs logs <id>")
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					text, err := client.RunLogText(id)
					if err != nil {
						return err
					}
					fmt.Fprint(outWriter(c), text)
					return nil
				},
			},
			{
				Name:      "kill",
				Usage:     "kill one or more running runs",
				ArgsUsage: "<id>...",
				Action:    runRunsKill,
			},
			{
				Name:      "resume",
				Usage:     "resume a prior run session",
				ArgsUsage: "<id>",
				Action:    runRunsResume,
			},
			{
				Name:      "replay",
				Usage:     "replay a run (new launch from prior prompt)",
				ArgsUsage: "<id>",
				Action:    runRunsReplay,
			},
			{
				Name:      "retry",
				Usage:     "retry a failed run (resume, park, or rerun by end_reason)",
				ArgsUsage: "<id>",
				Action:    runRunsRetry,
			},
			{
				Name:      "mark",
				Usage:     "override the outcome of a finished run (success|failed|skipped)",
				ArgsUsage: "<id> [success|failed|skipped]",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "note", Usage: "why the outcome changed"},
					&cli.BoolFlag{Name: "clear", Usage: "drop the override and restore the machine status"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runRunsMark,
			},
			{
				Name:      "approve",
				Usage:     "decide an approval for a run",
				ArgsUsage: "<id>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "decision", Value: "allow"},
					&cli.StringFlag{Name: "approval", Usage: "approval id; defaults to the pending one"},
				},
				Action: runRunsApprove,
			},
			{
				Name:      "message",
				Usage:     "send text to a run; an ended run continues its session",
				ArgsUsage: "<id> <text...>",
				Flags: []cli.Flag{
					&cli.StringSliceFlag{Name: "attach", Usage: "image file to send with the message (repeatable)"},
				},
				Action: runRunsMessage,
			},
			{
				Name:  "stats",
				Usage: "run counts and cost for a range, plus month to date",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name filter"},
					&cli.StringFlag{Name: "job", Usage: "filter by job name"},
					&cli.StringFlag{Name: "range", Usage: "24h|7d|30d|mtd", Value: "30d"},
				},
				Action: runRunsStats,
			},
			{
				Name:  "activity",
				Usage: "runs, tokens, cost, and planned fires of each agent, per time bin",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name filter"},
					&cli.StringFlag{Name: "agent", Usage: "agent id or name filter"},
					&cli.StringFlag{Name: "range", Usage: "today|7d|30d", Value: "7d"},
				},
				Action: runRunsActivity,
			},
			{
				Name:   "sessions",
				Usage:  "in-memory session state of live runs (empty on a fresh daemon; not persisted status)",
				Flags:  []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: runRunsSessions,
			},
			{
				Name:  "approvals",
				Usage: "list run approvals",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (approval id)"},
					&cli.StringFlag{Name: "state", Usage: "pending|decided"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runApprovalsList,
			},
		},
	}
}

func runRunsAttach(ctx context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi runs attach <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var run map[string]any
	if err := client.GetJSON(request.BuildRunGetPath(id, 0), &run); err != nil {
		return err
	}
	return attachAfterLaunch(ctx, c, client, run)
}

func runRunsShow(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildRunGetPath(c.Args().Get(0), 0), &body); err != nil {
		return err
	}
	return encodeJSON(c, body)
}

// Exit codes of runs wait, runs watch, and job run --wait / --follow.
const (
	runExitComplete = 0
	runExitFailed   = 1
	runExitParked   = 2
	runExitRunning  = 3
)

// runExitCode maps a run to its exit code. A human outcome wins over the
// machine status.
func runExitCode(run map[string]any) int {
	status := runs.Status(mapStr(run, "status"))
	if o, ok := run["outcome"].(map[string]any); ok && mapStr(o, "status") != "" {
		status = runs.Status(mapStr(o, "status"))
	}
	switch status {
	case runs.StatusComplete, runs.StatusSkipped:
		return runExitComplete
	case runs.StatusParked:
		return runExitParked
	case runs.StatusRunning:
		return runExitRunning
	}
	return runExitFailed
}

// exitForRun returns a silent exit error for a run that did not complete.
func exitForRun(run map[string]any) error {
	if code := runExitCode(run); code != runExitComplete {
		return cli.Exit("", code)
	}
	return nil
}

func runRunsWait(ctx context.Context, c *cli.Command) error {
	if c.NArg() != 1 {
		return fmt.Errorf("usage: trebi runs wait <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	run, err := client.WaitRun(ctx, c.Args().First(), c.Duration("timeout"))
	if err != nil {
		return err
	}
	if err := encodeJSON(c, run); err != nil {
		return err
	}
	return exitForRun(run)
}

func runRunsWatch(ctx context.Context, c *cli.Command) error {
	if c.NArg() != 1 {
		return fmt.Errorf("usage: trebi runs watch <id|job>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	w := &runWatch{client: client, out: outWriter(c), json: c.Bool("json")}
	ctx, stop := w.withTimeout(ctx, c.Duration("timeout"))
	defer stop()
	runID, err := w.resolve(ctx, c.Args().First(), workspaceFlag(c), c.Bool("next"))
	if err != nil {
		return err
	}
	return w.watch(ctx, runID)
}

func runRunsTimeline(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildRunTimelinePath(c.Args().Get(0)), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	return renderTimeline(outWriter(c), body)
}

func runRunsTurns(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi runs turns <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Turns []struct {
			N          int    `json:"n"`
			Reason     string `json:"reason"`
			Status     string `json:"status"`
			EndReason  string `json:"end_reason"`
			StartedAt  string `json:"started_at"`
			DurationMS int64  `json:"duration_ms"`
			SessionID  string `json:"session_id"`
			Tokens     struct {
				CostUSD float64 `json:"cost_usd"`
			} `json:"tokens"`
		} `json:"turns"`
	}
	if c.Bool("json") {
		var raw map[string]any
		if err := client.GetJSON(request.BuildRunTurnsPath(id), &raw); err != nil {
			return err
		}
		return encodeJSON(c, raw)
	}
	if err := client.GetJSON(request.BuildRunTurnsPath(id), &body); err != nil {
		return err
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Turn", "Reason", "Status", "End", "Started", "Duration", "Cost", "Session"})
	for _, t := range body.Turns {
		tbl.AppendRow(table.Row{
			t.N, dash(t.Reason), colorStatus(w, t.Status), dash(t.EndReason), dash(t.StartedAt),
			(time.Duration(t.DurationMS) * time.Millisecond).Round(time.Second).String(),
			fmt.Sprintf("%.4f", t.Tokens.CostUSD), dash(t.SessionID),
		})
	}
	renderTable(tbl)
	return nil
}

func runRunsTranscript(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi runs transcript <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := request.BuildRunTranscript(id, c.Bool("raw"))
	if c.Bool("raw") {
		raw, err := client.GetRaw(req.Path)
		if err != nil {
			return err
		}
		_, err = outWriter(c).Write(raw)
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(req, &body); err != nil {
		return err
	}
	return encodeJSON(c, body)
}

func runRunsKill(_ context.Context, c *cli.Command) error {
	if c.NArg() < 1 {
		return fmt.Errorf("usage: trebi runs kill <id> [id...]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ids := c.Args().Slice()
	if len(ids) == 1 {
		return client.DoHTTP(request.BuildRunsKill(ids[0]), nil)
	}
	// The bulk route kills each id independently, so one bad id does not abort
	// the rest. It reports which ones failed.
	var out struct {
		Failed map[string]string `json:"failed"`
	}
	if err := client.DoHTTP(request.BuildRunsKillBulk(ids), &out); err != nil {
		return err
	}
	for id, msg := range out.Failed {
		fmt.Fprintf(os.Stderr, "%s: %s\n", id, msg)
	}
	if len(out.Failed) > 0 {
		return fmt.Errorf("%d of %d run(s) could not be killed", len(out.Failed), len(ids))
	}
	return nil
}

func runRunsResume(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildRunResume(c.Args().Get(0)), &out); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "run %v status=%v\n", out["id"], out["status"])
	return nil
}

func runRunsReplay(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildRunReplay(c.Args().Get(0)), &out); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "run %v status=%v\n", out["id"], out["status"])
	return nil
}

func runRunsRetry(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi runs retry <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := request.BuildRunsRetry(id)
	var out map[string]any
	if err := client.DoHTTP(req, &out); err != nil {
		return err
	}
	if action := mapStr(out, "action"); action != "" {
		fmt.Fprintf(outWriter(c), "retry %s action=%s\n", id, action)
		return nil
	}
	fmt.Fprintf(outWriter(c), "run %v status=%v\n", out["id"], out["status"])
	return nil
}

func runRunsMark(_ context.Context, c *cli.Command) error {
	args := c.Args().Slice()
	if len(args) == 0 {
		return fmt.Errorf("usage: trebi runs mark <id> success|failed|skipped")
	}
	id := args[0]
	clear := c.Bool("clear")
	status := ""
	if len(args) > 1 {
		if clear {
			return fmt.Errorf("--clear takes no outcome argument")
		}
		status = args[1]
	}
	if status == "" && !clear {
		return fmt.Errorf("usage: trebi runs mark <id> success|failed|skipped (or --clear)")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := request.BuildRunMark(id, status, c.String("note"), clear)
	var out map[string]any
	if err := client.DoHTTP(req, &out); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	if outcome, ok := out["outcome"].(map[string]any); ok {
		fmt.Fprintf(outWriter(c), "run %v status=%v outcome=%v\n", out["id"], out["status"], outcome["status"])
		return nil
	}
	fmt.Fprintf(outWriter(c), "run %v status=%v outcome=cleared\n", out["id"], out["status"])
	return nil
}

func runRunsApprove(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildRunApprove(c.Args().Get(0), c.String("decision"), c.String("approval")), nil)
}

func runRunsMessage(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi runs message <id> <text...>")
	}
	argText := strings.Join(c.Args().Tail(), " ")
	attach := c.StringSlice("attach")
	text := strings.TrimSpace(argText)
	if text == "" && len(attach) == 0 {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		if text, err = resolveRunMessageText(argText, string(b)); err != nil {
			return err
		}
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var ids []string
	if len(attach) > 0 {
		var run struct {
			Workspace string `json:"workspace"`
		}
		if err := client.GetJSON(request.BuildRunGetPath(id, 0), &run); err != nil {
			return err
		}
		if ids, err = client.UploadImages(run.Workspace, attach); err != nil {
			return err
		}
	}
	var res struct {
		Delivery string `json:"delivery"`
		RunID    string `json:"run_id"`
		Turn     int    `json:"turn"`
	}
	if err := client.DoHTTP(request.BuildRunMessage(id, text, ids...), &res); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "%s: run %s, turn %d\n", res.Delivery, res.RunID, res.Turn)
	return nil
}

func resolveRunMessageText(argText, stdinText string) (string, error) {
	argText = strings.TrimSpace(argText)
	stdinText = strings.TrimSpace(stdinText)
	switch {
	case argText != "" && stdinText != "":
		return "", fmt.Errorf("provide text as arguments or stdin, not both")
	case argText != "":
		return argText, nil
	case stdinText != "":
		return stdinText, nil
	default:
		return "", fmt.Errorf("text required")
	}
}

func runRunsStats(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	type totals struct {
		Runs                int     `json:"runs"`
		OK                  int     `json:"ok"`
		Failed              int     `json:"failed"`
		CostUSD             float64 `json:"cost_usd"`
		TokensBilled        int64   `json:"tokens_billed"`
		TokensCached        int64   `json:"tokens_cached"`
		TokensInput         int64   `json:"tokens_input"`
		TokensOutput        int64   `json:"tokens_output"`
		TokensCacheRead     int64   `json:"tokens_cache_read"`
		TokensCacheCreation int64   `json:"tokens_cache_creation"`
	}
	var body struct {
		Range   string `json:"range"`
		Totals  totals `json:"totals"`
		MTD     totals `json:"mtd"`
		Running int    `json:"running"`
	}
	req := request.BuildRunsStats(workspaceFlag(c), c.String("job"), c.String("range"))
	if c.Bool("json") {
		var raw map[string]any
		if err := client.DoHTTP(req, &raw); err != nil {
			return err
		}
		return encodeJSON(c, raw)
	}
	if err := client.DoHTTP(req, &body); err != nil {
		return err
	}
	tbl := newTable(outWriter(c), []string{"Metric", body.Range, "MTD"})
	tbl.AppendRow(table.Row{"runs", body.Totals.Runs, body.MTD.Runs})
	tbl.AppendRow(table.Row{"ok", body.Totals.OK, body.MTD.OK})
	tbl.AppendRow(table.Row{"failed", body.Totals.Failed, body.MTD.Failed})
	tbl.AppendRow(table.Row{"cost_usd", fmt.Sprintf("%.4f", body.Totals.CostUSD), fmt.Sprintf("%.4f", body.MTD.CostUSD)})
	tbl.AppendRow(table.Row{"tokens_billed", body.Totals.TokensBilled, body.MTD.TokensBilled})
	tbl.AppendRow(table.Row{"tokens_cached", body.Totals.TokensCached, body.MTD.TokensCached})
	tbl.AppendRow(table.Row{"tokens_input", body.Totals.TokensInput, body.MTD.TokensInput})
	tbl.AppendRow(table.Row{"tokens_output", body.Totals.TokensOutput, body.MTD.TokensOutput})
	tbl.AppendRow(table.Row{"tokens_cache_read", body.Totals.TokensCacheRead, body.MTD.TokensCacheRead})
	tbl.AppendRow(table.Row{"tokens_cache_creation", body.Totals.TokensCacheCreation, body.MTD.TokensCacheCreation})
	tbl.AppendRow(table.Row{"running", body.Running, ""})
	renderTable(tbl)
	return nil
}

func runRunsActivity(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req, err := request.BuildRunsActivity(c.String("range"), time.Now(), time.Local, c.String("agent"), workspaceFlag(c))
	if err != nil {
		return err
	}
	if c.Bool("json") {
		var raw map[string]any
		if err := client.DoHTTP(req, &raw); err != nil {
			return err
		}
		return encodeJSON(c, raw)
	}
	var body runs.Activity
	if err := client.DoHTTP(req, &body); err != nil {
		return err
	}
	tbl := newTable(outWriter(c), []string{"Agent", "Runs", "Failed", "Waiting", "Running", "Tokens", "Cost", "Due"})
	for _, row := range body.Rows {
		var total, failed, waiting, running, tokens, due int64
		var cost float64
		for _, b := range row.Bins {
			total += int64(b.Done + b.Failed + b.Waiting + b.Running + b.Skipped)
			failed += int64(b.Failed)
			waiting += int64(b.Waiting)
			running += int64(b.Running)
			tokens += b.Tokens
			cost += b.CostUSD
		}
		for _, d := range row.Due {
			due += int64(d.N)
		}
		tbl.AppendRow(table.Row{row.Key, total, failed, waiting, running, tokens, fmt.Sprintf("%.4f", cost), due})
	}
	renderTable(tbl)
	return nil
}

func runRunsSessions(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Sessions map[string]any `json:"sessions"`
	}
	if err := client.DoHTTP(request.BuildSessionState(), &body); err != nil {
		return err
	}
	if body.Sessions == nil {
		body.Sessions = map[string]any{}
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Run", "Session"})
	ids := make([]string, 0, len(body.Sessions))
	for id := range body.Sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		tbl.AppendRow(table.Row{id, colorStatus(w, fmt.Sprint(body.Sessions[id]))})
	}
	renderTable(tbl)
	if len(body.Sessions) == 0 {
		fmt.Fprintln(w, "in-memory snapshot of live runs only; empty on a fresh daemon")
	}
	return nil
}

// runRunsList implements runs list.
func runRunsList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws := workspaceFlag(c)
	kind := c.String("kind")
	q := request.RunsQuery{
		Status: c.String("status"), Job: c.String("job"), Owner: c.String("owner"), Workspace: ws, Kind: kind,
		Cursor: c.String("cursor"), Harness: c.String("harness"), Model: c.String("model"),
		EndReason: c.String("end-reason"), Q: c.String("search"),
		Since: c.String("since"), Until: c.String("until"), Limit: c.Int("limit"),
	}
	// Fetch a wider page when conversation filter needs post-filter.
	conv := c.String("conversation")
	if conv != "" && q.Limit > 0 {
		q.Limit *= 5
	}
	path := q.Path()
	var body struct {
		Runs       []map[string]any `json:"runs"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if conv != "" {
		filtered := body.Runs[:0]
		for _, r := range body.Runs {
			if request.MatchConversation(mapStr(r, "conversation_key"), conv) {
				filtered = append(filtered, r)
			}
		}
		body.Runs = filtered
		limit := c.Int("limit")
		if limit > 0 && len(body.Runs) > limit {
			body.Runs = body.Runs[:limit]
		}
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Job", "Kind", "Status", "Harness", "Started", "Reason", "Conversation", "Turns"})
	for _, r := range body.Runs {
		status := mapStr(r, "status")
		turns := dash(mapStr(r, "turn_count"))
		tbl.AppendRow(table.Row{
			mapStr(r, "id"),
			mapStr(r, "job"),
			dash(mapStr(r, "kind")),
			colorStatus(w, status),
			dash(mapStr(r, "harness")),
			dash(mapStr(r, "started_at")),
			dash(mapStr(r, "reason")),
			dash(mapStr(r, "conversation_key")),
			turns,
		})
	}
	renderTable(tbl)
	return nil
}

func runApprovalsList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := request.BuildApprovalsListPath(c.String("cursor"), c.Int("limit"), c.String("state"))
	var body struct {
		Approvals  []map[string]any `json:"approvals"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Run", "Status", "Decision", "Created"})
	for _, a := range body.Approvals {
		tbl.AppendRow(table.Row{
			mapStr(a, "id"), mapStr(a, "run_id"),
			colorStatus(w, mapStr(a, "status")),
			dash(mapStr(a, "decision")), dash(mapStr(a, "created_at")),
		})
	}
	renderTable(tbl)
	return nil
}

func runCommand() *cli.Command {
	return &cli.Command{
		Name:      "run",
		Usage:     "launch a job",
		ArgsUsage: "<job>",
		Flags: append([]cli.Flag{
			&cli.StringFlag{Name: "extra-prompt", Aliases: []string{"e"}, Usage: "append text to the job prompt"},
			&cli.StringFlag{Name: "prompt", Usage: "replace the job prompt"},
			&cli.StringSliceFlag{Name: "input", Aliases: []string{"i"}, Usage: "job input value, key=value (repeatable)"},
			&cli.StringFlag{Name: "event", Usage: "stored event id: run the job with this event, as a person"},
			&cli.IntFlag{Name: "trigger", Usage: "with --event: the index of the event trigger to use"},
			&cli.BoolFlag{Name: "force", Usage: "with --event: skip the match check of the trigger"},
			&cli.StringFlag{Name: "harness", Usage: "agent tool to run with"},
			&cli.BoolFlag{Name: "test", Aliases: []string{"dry-run"}, Usage: "dry_run: fill the inputs, evaluate gates, print plan, do not launch"},
			&cli.BoolFlag{Name: "attach", Aliases: []string{"a"}, Usage: "after launch: attach (local pane or remote WebSocket screen)"},
			&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		}, launchWaitFlags()...),
		Action: runLaunch,
	}
}

func runLaunch(ctx context.Context, c *cli.Command) error {
	if c.NArg() < 1 {
		return fmt.Errorf("usage: trebi job run <job>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	spec := request.RunSpec{Job: c.Args().Get(0), Workspace: workspaceFlag(c), Event: c.String("event"), Force: c.Bool("force")}
	if c.IsSet("trigger") {
		n := c.Int("trigger")
		spec.OnIndex = &n
	}
	return launchAndMaybeAttach(ctx, c, client, spec, "", request.RunSpec.Build)
}

// launchAndMaybeAttach is the shared job-run / agent-wake launch tail: fill
// RunSpec from flags, POST, print the id, optionally attach.
// --json is a job-only flag; agent wake never defines it.
func launchAndMaybeAttach(ctx context.Context, c *cli.Command, client *APIClient, spec request.RunSpec, kind string, build func(request.RunSpec) request.HTTPRequest) error {
	inputs, err := parseInputs(c)
	if err != nil {
		return err
	}
	spec.Prompt = c.String("prompt")
	spec.ExtraPrompt = c.String("extra-prompt")
	spec.Inputs = inputs
	spec.Harness = c.String("harness")
	spec.ConversationID = c.String("conversation")
	spec.DryRun = c.Bool("test")
	var out map[string]any
	if err := client.DoHTTP(build(spec), &out); err != nil {
		return err
	}
	if c.Bool("test") {
		return encodeJSON(c, out)
	}
	if queued, _ := out["queued"].(bool); queued {
		if c.Bool("json") {
			return encodeJSON(c, out)
		}
		fmt.Fprintf(outWriter(c), "queued %v: it starts when the current run finishes\n", out["queue_id"])
		if c.Bool("wait") || c.Bool("follow") {
			fmt.Fprintf(outWriter(c), "to watch it: trebi runs watch %s --next\n", spec.Job)
		}
		return nil
	}
	runID := mapStr(out, "id")
	if c.Bool("follow") && runID != "" {
		w := &runWatch{client: client, out: outWriter(c), json: c.Bool("json")}
		ctx, stop := w.withTimeout(ctx, c.Duration("timeout"))
		defer stop()
		return w.watch(ctx, runID)
	}
	if c.Bool("wait") && runID != "" {
		run, err := client.WaitRun(ctx, runID, c.Duration("timeout"))
		if err != nil {
			return err
		}
		if c.Bool("json") {
			if err := encodeJSON(c, run); err != nil {
				return err
			}
		} else {
			printRunEnd(outWriter(c), run)
		}
		return exitForRun(run)
	}
	if c.Bool("json") {
		if err := encodeJSON(c, out); err != nil {
			return err
		}
	} else if kind != "" {
		fmt.Fprintf(outWriter(c), "run %v status=%v kind=%s\n", runID, out["status"], kind)
	} else {
		fmt.Fprintf(outWriter(c), "run %v status=%v\n", runID, out["status"])
	}
	if c.Bool("attach") && runID != "" {
		return attachAfterLaunch(ctx, c, client, out)
	}
	return nil
}

// launchWaitFlags are the flags of job run and agent wake that keep the
// command open until the run ends.
func launchWaitFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: "wait", Usage: "after launch: wait until the run ends; exit codes as runs wait"},
		&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "after launch: print progress until the run ends (runs watch)"},
		&cli.DurationFlag{Name: "timeout", Usage: "with --wait or --follow: stop after this long, for example 9m (default: no limit)"},
	}
}

// printRunEnd prints the one-line result of a run.
func printRunEnd(w io.Writer, run map[string]any) {
	line := fmt.Sprintf("run %s status=%s", mapStr(run, "id"), mapStr(run, "status"))
	if r := mapStr(run, "end_reason"); r != "" {
		line += " end_reason=" + r
	}
	if d := formatDurationMS(run["duration_ms"]); d != "" {
		line += " duration=" + d
	}
	fmt.Fprintln(w, line)
}

func renderTimeline(w io.Writer, body map[string]any) error {
	tbl := newTable(w, []string{"When", "Type", "Layer", "State", "Duration", "Reason", "Source", "Detail"})
	// segments
	if segs, ok := body["segments"].([]any); ok {
		for _, raw := range segs {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			tbl.AppendRow(table.Row{
				dash(mapStr(m, "started_at", "when")),
				"segment",
				dash(mapStr(m, "layer")),
				colorStatus(w, mapStr(m, "state")),
				dash(formatDurationMS(m["duration_ms"])),
				dash(mapStr(m, "reason")),
				dash(mapStr(m, "source")),
				dash(mapStr(m, "detail", "detail_json")),
			})
		}
	}
	// events
	if evs, ok := body["events"].([]any); ok {
		for _, raw := range evs {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			tbl.AppendRow(table.Row{
				dash(mapStr(m, "at", "when", "created_at")),
				"event",
				"-",
				dash(mapStr(m, "type", "name", "event")),
				"-",
				dash(mapStr(m, "reason")),
				dash(mapStr(m, "source")),
				dash(mapStr(m, "detail", "detail_json")),
			})
		}
	}
	renderTable(tbl)
	return nil
}

func formatDurationMS(v any) string {
	if v == nil {
		return ""
	}
	switch n := v.(type) {
	case float64:
		if n <= 0 {
			return ""
		}
		ms := int64(n)
		if ms < 1000 {
			return fmt.Sprintf("%dms", ms)
		}
		sec := ms / 1000
		if sec < 60 {
			return fmt.Sprintf("%ds", sec)
		}
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	case json.Number:
		f, _ := n.Float64()
		return formatDurationMS(f)
	default:
		s := fmt.Sprint(v)
		if s == "0" || s == "<nil>" {
			return ""
		}
		return s
	}
}
