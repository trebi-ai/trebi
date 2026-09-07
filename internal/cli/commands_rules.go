package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
				Usage: "evaluate match/extract against a sample event",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "match", Usage: "match expression"},
					&cli.StringFlag{Name: "extract", Usage: "extract expression"},
					&cli.StringFlag{Name: "event-file", Usage: "event JSON file (stdin if omitted)"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runRulesTest,
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
	event, err := loadRuleEvent(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildRulesTest(c.String("match"), c.String("extract"), event), &out); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "matched=%v\n", out["matched"])
	fmt.Fprintf(w, "vars=%v\n", out["vars"])
	if errVal := out["error"]; errVal != nil && fmt.Sprint(errVal) != "" {
		fmt.Fprintf(w, "error=%v\n", errVal)
	}
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
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(out)
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
