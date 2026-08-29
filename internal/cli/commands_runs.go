package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func runsCommand() *cli.Command {
	return &cli.Command{
		Name:  "runs",
		Usage: "list and inspect runs (jobi history)",
		Subcommands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list runs",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "status", Aliases: []string{"s"}, Usage: "status filter (running, complete, idle, waiting, …)"},
					&cli.StringFlag{Name: "job", Usage: "job name filter"},
					&cli.StringFlag{Name: "kind", Usage: "kind filter (job|assistant)"},
					&cli.StringFlag{Name: "workspace", Usage: "workspace id filter"},
					&cli.StringFlag{Name: "harness", Usage: "harness filter (claude, grok, cursor, …)"},
					&cli.StringFlag{Name: "model", Aliases: []string{"m"}, Usage: "model filter (tokens_model exact match)"},
					&cli.StringFlag{Name: "conversation", Usage: "substring match on conversation_key (client-side)"},
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (run id)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 20, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runRunsList,
			},
			{
				Name:      "attach",
				Usage:     "attach to a run (local tmux or remote WebSocket screen)",
				ArgsUsage: "<id>",
				Action:    runRunsAttach,
			},
			{
				Name:      "show",
				Usage:     "show a run",
				ArgsUsage: "<id>",
				Action:    runRunsShow,
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
				Name:      "approve",
				Usage:     "decide an approval for a run",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{&cli.StringFlag{Name: "decision", Value: "allow"}},
				Action:    runRunsApprove,
			},
			{
				Name:      "message",
				Usage:     "send text to a live run",
				ArgsUsage: "<id> <text...>",
				Action:    runRunsMessage,
			},
			{
				Name:   "stats",
				Usage:  "run counts (total and currently running)",
				Flags:  []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: runRunsStats,
			},
			{
				Name:   "agents",
				Usage:  "in-memory agent state of live runs (empty on a fresh daemon; not persisted status)",
				Flags:  []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: runRunsAgents,
			},
			{
				Name:  "approvals",
				Usage: "list run approvals",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (approval id)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runApprovalsList,
			},
		},
	}
}

func runRunsAttach(c *cli.Context) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi runs attach <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return attachRun(c, client, id)
}

func runRunsShow(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON("/v1/runs/"+c.Args().Get(0), &body); err != nil {
		return err
	}
	enc := json.NewEncoder(outWriter(c))
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

func runRunsTimeline(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON("/v1/runs/"+c.Args().Get(0)+"/timeline", &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	return renderTimeline(outWriter(c), body)
}

func runRunsTranscript(c *cli.Context) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi runs transcript <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := BuildRunTranscript(id, c.Bool("raw"))
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
	enc := json.NewEncoder(outWriter(c))
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

func runRunsKill(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("usage: trebi runs kill <id> [id...]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	// Kill each independently so one bad ID does not abort the rest.
	var failed int
	for _, id := range c.Args().Slice() {
		req := BuildRunsKill(id)
		if err := client.DoHTTP(req, nil); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", id, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d run(s) could not be killed", failed, c.NArg())
	}
	return nil
}

func runRunsResume(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.PostJSON("/v1/runs/"+c.Args().Get(0)+"/resume", nil, &out); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "run %v status=%v\n", out["id"], out["status"])
	return nil
}

func runRunsReplay(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.PostJSON("/v1/runs/"+c.Args().Get(0)+"/replay", nil, &out); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "run %v status=%v\n", out["id"], out["status"])
	return nil
}

func runRunsRetry(c *cli.Context) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi runs retry <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := BuildRunsRetry(id)
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

func runRunsApprove(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.PostJSON("/v1/runs/"+c.Args().Get(0)+"/approve",
		map[string]string{"decision": c.String("decision")}, nil)
}

func runRunsMessage(c *cli.Context) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi runs message <id> <text...>")
	}
	argText := strings.Join(c.Args().Tail(), " ")
	stdinText := ""
	if strings.TrimSpace(argText) == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		stdinText = string(b)
	}
	text, err := resolveRunMessageText(argText, stdinText)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(BuildRunMessage(id, text), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "sent to %s\n", id)
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

func runRunsStats(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(BuildRunsStats(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Key", "Value"})
	tbl.AppendRow(table.Row{"total", fmt.Sprint(body["total"])})
	tbl.AppendRow(table.Row{"running", fmt.Sprint(body["running"])})
	renderTable(tbl)
	return nil
}

func runRunsAgents(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Agents map[string]any `json:"agents"`
	}
	if err := client.DoHTTP(BuildAgentsState(), &body); err != nil {
		return err
	}
	if body.Agents == nil {
		body.Agents = map[string]any{}
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Run", "AgentState"})
	ids := make([]string, 0, len(body.Agents))
	for id := range body.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		tbl.AppendRow(table.Row{id, colorStatus(w, fmt.Sprint(body.Agents[id]))})
	}
	renderTable(tbl)
	if len(body.Agents) == 0 {
		fmt.Fprintln(w, "in-memory snapshot of live runs only; empty on a fresh daemon")
	}
	return nil
}

// runRunsList implements runs list.
func runRunsList(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws := workspaceFor(c)
	kind := c.String("kind")
	path := BuildRunsListPath(
		c.String("status"), c.String("job"), ws,
		kind, c.String("cursor"),
		c.String("harness"), c.String("model"), c.Int("limit"),
	)
	// Fetch a wider page when conversation filter needs post-filter.
	conv := c.String("conversation")
	if conv != "" && c.Int("limit") > 0 {
		path = BuildRunsListPath(
			c.String("status"), c.String("job"), ws,
			kind, c.String("cursor"),
			c.String("harness"), c.String("model"), c.Int("limit")*5,
		)
	}
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
			if MatchConversation(mapStr(r, "conversation_key"), conv) {
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
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Job", "Kind", "Status", "Harness", "Started", "Reason", "Conversation"})
	for _, r := range body.Runs {
		status := mapStr(r, "status")
		tbl.AppendRow(table.Row{
			mapStr(r, "id"),
			mapStr(r, "job"),
			dash(mapStr(r, "kind")),
			colorStatus(w, status),
			dash(mapStr(r, "harness")),
			dash(mapStr(r, "started_at")),
			dash(mapStr(r, "reason")),
			dash(mapStr(r, "conversation_key")),
		})
	}
	renderTable(tbl)
	return nil
}

func runApprovalsList(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := BuildApprovalsListPath(c.String("cursor"), c.Int("limit"))
	var body struct {
		Approvals  []map[string]any `json:"approvals"`
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
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "extra-prompt", Aliases: []string{"e"}, Usage: "append text to the job prompt (fetched then prompt_override)"},
			&cli.BoolFlag{Name: "test", Usage: "dry_run: evaluate gates, print plan, do not launch"},
			&cli.BoolFlag{Name: "attach", Aliases: []string{"a"}, Usage: "after launch: attach (tmux local or WebSocket remote)"},
		},
		Action: runLaunch,
	}
}

func runLaunch(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("usage: trebi job run <job>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	jobName := c.Args().Get(0)
	ws := workspaceFor(c)
	promptOverride := ""
	if extra := c.String("extra-prompt"); extra != "" {
		// Append to job body so we match jobi -e (API prompt_override is full replace).
		var got map[string]any
		if err := client.GetJSON(withQuery("/v1/jobs/"+jobName, workspaceQ(ws)), &got); err != nil {
			return err
		}
		base := mapStr(got, "prompt")
		promptOverride = AppendExtraPrompt(base, extra)
	}
	req := BuildRunsCreate(jobName, ws, promptOverride, c.Bool("test"))
	var out map[string]any
	if err := client.DoHTTP(req, &out); err != nil {
		return err
	}
	if c.Bool("test") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	runID := mapStr(out, "id")
	fmt.Fprintf(outWriter(c), "run %v status=%v\n", runID, out["status"])
	if c.Bool("attach") && runID != "" {
		return attachAfterLaunch(c, client, out)
	}
	return nil
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
