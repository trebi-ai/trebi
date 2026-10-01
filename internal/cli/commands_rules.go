package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func rulesCommand() *cli.Command {
	return &cli.Command{
		Name:  "rules",
		Usage: "dry-run connector routing rules",
		Commands: []*cli.Command{
			{
				Name:  "test",
				Usage: "evaluate rooms, allowlist, extract, and match against a stored or sample event",
				Flags: append(ruleGateFlags(),
					&cli.StringFlag{Name: "event-id", Usage: "stored event id (instead of an event file)"},
					&cli.StringFlag{Name: "connector", Usage: "connector of a sample event"},
					&cli.StringFlag{Name: "event-file", Usage: "event JSON file (stdin if omitted)"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				),
				Action: runRulesTest,
			},
			{
				Name:      "test-batch",
				Usage:     "evaluate one trigger against up to 50 stored events",
				ArgsUsage: "<event-id>...",
				Flags: append(ruleGateFlags(),
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				),
				Action: runRulesTestBatch,
			},
			{
				Name:  "fire",
				Usage: "evaluate a rule and enqueue on match",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "connector", Usage: "connector name"},
					&cli.StringFlag{Name: "workspace", Usage: "workspace id"},
					&cli.StringFlag{Name: "job", Usage: "job name"},
					&cli.StringFlag{Name: "match", Usage: "match expression"},
					&cli.StringFlag{Name: "extract", Usage: "extract expression"},
					&cli.StringFlag{Name: "event-file", Usage: "event JSON file (stdin if omitted)"},
					&cli.IntFlag{Name: "on-index", Usage: "on[] index"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runRulesFire,
			},
		},
	}
}

func ruleGateFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "match", Usage: "match expression"},
		&cli.StringFlag{Name: "extract", Usage: "extract expression"},
		&cli.StringSliceFlag{Name: "room", Usage: "room id the trigger takes (repeatable)"},
		&cli.StringSliceFlag{Name: "allowlist", Usage: "sender id the trigger takes (repeatable)"},
		&cli.StringSliceFlag{Name: "contains", Usage: "word or phrase the message text must hold, any of them (repeatable)"},
		&cli.StringFlag{Name: "from", Usage: "senders the trigger takes: people, bots, or anyone (default)"},
		&cli.StringFlag{Name: "replies", Usage: "continue or off; continue shows the conversation key"},
		&cli.StringFlag{Name: "job", Usage: "job of the trigger; with --replies continue, a reply in an open conversation of this job is a follow-up"},
		&cli.StringFlag{Name: "workspace", Usage: "workspace of --job"},
	}
}

func ruleTestOf(c *cli.Command) request.RuleTest {
	t := request.RuleTest{
		Match: c.String("match"), Extract: c.String("extract"),
		Rooms: c.StringSlice("room"), Allowlist: c.StringSlice("allowlist"),
		Contains: c.StringSlice("contains"), From: c.String("from"), Replies: c.String("replies"),
		Job: c.String("job"),
	}
	if t.Job != "" {
		t.Workspace = workspaceFlag(c)
	}
	return t
}

func loadRuleEvent(c *cli.Command) (any, error) {
	raw, err := readFileOrStdin(c.String("event-file"))
	if err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("event JSON required (--event-file or stdin)")
	}
	var event any
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return nil, fmt.Errorf("event JSON: %w", err)
	}
	return event, nil
}

func runRulesTest(_ context.Context, c *cli.Command) error {
	var event any
	if c.String("event-id") == "" {
		var err error
		if event, err = loadRuleEvent(c); err != nil {
			return err
		}
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(ruleTestOf(c).Build(c.String("event-id"), c.String("connector"), event), &out); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "matched=%v\n", out["matched"])
	if stage := mapStr(out, "stage"); stage != "" {
		fmt.Fprintf(w, "stage=%s\n", stage)
	}
	fmt.Fprintf(w, "vars=%v\n", out["vars"])
	if errVal := out["error"]; errVal != nil && fmt.Sprint(errVal) != "" {
		fmt.Fprintf(w, "error=%v\n", errVal)
	}
	return nil
}

func runRulesTestBatch(_ context.Context, c *cli.Command) error {
	ids := c.Args().Slice()
	if len(ids) == 0 {
		return fmt.Errorf("usage: trebi rules test-batch <event-id>... --match EXPR")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(ruleTestOf(c).BuildBatch(ids), &out); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	results, _ := out["results"].([]any)
	tbl := newTable(outWriter(c), []string{"Event", "Matched", "Stage", "Error"})
	for _, raw := range results {
		m, _ := raw.(map[string]any)
		tbl.AppendRow(table.Row{mapStr(m, "event_id"), m["matched"], dash(mapStr(m, "stage")), dash(mapStr(m, "error"))})
	}
	renderTable(tbl)
	return nil
}

func runRulesFire(_ context.Context, c *cli.Command) error {
	event, err := loadRuleEvent(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws := c.String("workspace")
	if ws == "" {
		ws = workspaceFor(c)
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildRulesFire(
		c.String("connector"), ws, c.String("job"),
		c.String("match"), c.String("extract"), event, c.Int("on-index"),
	), &out); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	fmt.Fprintf(outWriter(c), "outcome=%v\n", out["outcome"])
	if qid := mapStr(out, "queue_id"); qid != "" {
		fmt.Fprintf(outWriter(c), "queue_id=%s\n", qid)
	}
	if errVal := out["error"]; errVal != nil && fmt.Sprint(errVal) != "" {
		fmt.Fprintf(outWriter(c), "error=%v\n", errVal)
	}
	return nil
}
