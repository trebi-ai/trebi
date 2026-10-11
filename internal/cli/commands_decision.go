package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v3"

	"github.com/trebi-ai/trebi/internal/request"
)

func decisionsCommand() *cli.Command {
	jsonFlag := func() cli.Flag { return &cli.BoolFlag{Name: "json", Usage: "emit JSON"} }
	ws := func() cli.Flag {
		return &cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id or name"}
	}
	return &cli.Command{
		Name:    "decisions",
		Aliases: []string{"decision"},
		Usage:   "the safety checks that Trebi makes, for example on email replies",
		Commands: []*cli.Command{
			{
				Name:    "ls",
				Aliases: []string{"list"},
				Usage:   "list decisions, newest first",
				Flags: []cli.Flag{
					ws(),
					&cli.StringFlag{Name: "kind", Usage: "decision kind, for example email.reply_risk"},
					&cli.StringFlag{Name: "outcome", Usage: "outcome, for example deliver or hold"},
					&cli.StringFlag{Name: "status", Usage: "ok | invalid | error | timeout | off"},
					&cli.StringFlag{Name: "subject", Usage: "<kind>:<id>, for example email:<message id>"},
					&cli.StringFlag{Name: "cursor", Usage: "page cursor from the last page"},
					limitFlag(),
					jsonFlag(),
				},
				Action: runDecisionList,
			},
			{
				Name:      "show",
				ArgsUsage: "<id>",
				Usage:     "show one decision with its questions and answers",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "raw", Usage: "print the raw output of the decider"}, jsonFlag()},
				Action:    runDecisionShow,
			},
			{
				Name:   "kinds",
				Usage:  "list the decision kinds and the decider that each uses",
				Flags:  []cli.Flag{jsonFlag()},
				Action: runDecisionKinds,
			},
			{
				Name:  "stats",
				Usage: "count the decisions and how often you agree",
				Flags: []cli.Flag{ws(), &cli.StringFlag{Name: "kind", Usage: "decision kind"},
					&cli.StringFlag{Name: "since", Usage: "RFC3339 time or a duration such as 168h (default 30 days)"}, jsonFlag()},
				Action: runDecisionStats,
			},
			{
				Name:      "test",
				ArgsUsage: "<kind>",
				Usage:     "run a check on a text and store nothing",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "text", Required: true, Usage: "file with the text; - reads stdin"},
					&cli.StringSliceFlag{Name: "fact", Usage: "key=value trusted fact (repeatable)"},
					jsonFlag(),
				},
				Action: runDecisionTest,
			},
			{
				Name:      "feedback",
				ArgsUsage: "<id> agree|disagree",
				Usage:     "tell Trebi if a decision was right",
				Flags:     []cli.Flag{jsonFlag()},
				Action:    runDecisionFeedback,
			},
		},
	}
}

func runDecisionList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	f := request.DecisionFilter{Workspace: c.String("workspace"), Kind: c.String("kind"), Outcome: c.String("outcome"),
		Status: c.String("status"), Cursor: c.String("cursor"), Limit: c.Int("limit")}
	if subj := c.String("subject"); subj != "" {
		var ok bool
		if f.SubjectKind, f.SubjectID, ok = strings.Cut(subj, ":"); !ok {
			return fmt.Errorf("--subject must be <kind>:<id>")
		}
	}
	var body struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := client.GetJSON(request.BuildDecisionListPath(f), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Kind", "Subject", "Outcome", "Status", "Took", "Created"})
	for _, d := range body.Items {
		tbl.AppendRow(table.Row{mapStr(d, "id"), mapStr(d, "kind"), mapStr(d, "subject_kind") + ":" + mapStr(d, "subject_id"),
			mapStr(d, "outcome"), mapStr(d, "status"), mapStr(d, "took_ms") + "ms", mapStr(d, "created_at")})
	}
	renderTable(tbl)
	if body.NextCursor != "" {
		fmt.Fprintf(w, "next cursor: %s\n", body.NextCursor)
	}
	return nil
}

func runDecisionShow(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi decisions show <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if c.Bool("raw") {
		var body map[string]any
		if err := client.GetJSON(request.BuildDecisionRawPath(id), &body); err != nil {
			return err
		}
		if c.Bool("json") {
			return encodeJSON(c, body)
		}
		fmt.Fprintln(outWriter(c), mapStr(body, "raw"))
		return nil
	}
	var d map[string]any
	if err := client.GetJSON(request.BuildDecisionPath(id), &d); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, d)
	}
	printDecision(c, d)
	return nil
}

func printDecision(c *cli.Command, d map[string]any) {
	w := outWriter(c)
	if id := mapStr(d, "id"); id != "" {
		fmt.Fprintf(w, "ID:       %s\n", id)
	}
	fmt.Fprintf(w, "Kind:     %s\n", mapStr(d, "kind"))
	if s := mapStr(d, "subject_id"); s != "" {
		fmt.Fprintf(w, "Subject:  %s:%s\n", mapStr(d, "subject_kind"), s)
	}
	fmt.Fprintf(w, "Outcome:  %s (status %s)\n", mapStr(d, "outcome"), mapStr(d, "status"))
	fmt.Fprintf(w, "Decider:  %s, %s attempt(s), %sms\n", mapStr(d, "decider"), mapStr(d, "attempts"), mapStr(d, "took_ms"))
	if tok, _ := d["tokens"].(map[string]any); mapStr(tok, "billed") != "0" && mapStr(tok, "billed") != "" {
		fmt.Fprintf(w, "Tokens:   %s billed, %s cached\n", mapStr(tok, "billed"), mapStr(tok, "cached"))
	}
	if e := mapStr(d, "error"); e != "" {
		fmt.Fprintf(w, "Error:    %s\n", e)
	}
	if fb := mapStr(d, "feedback"); fb != "" {
		fmt.Fprintf(w, "Feedback: %s\n", fb)
	}
	tbl := newTable(w, []string{"Question", "Answer", "Confidence", "Method", "Reason"})
	answers, _ := d["answers"].([]any)
	for _, a := range answers {
		m, _ := a.(map[string]any)
		tbl.AppendRow(table.Row{mapStr(m, "id"), mapStr(m, "value"), mapStr(m, "confidence"), mapStr(m, "method"), mapStr(m, "reason")})
	}
	renderTable(tbl)
}

func runDecisionKinds(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := client.GetJSON(request.BuildDecisionKindsPath(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	tbl := newTable(outWriter(c), []string{"Kind", "Label", "Version", "Decider", "Safe outcome"})
	for _, k := range body.Items {
		tbl.AppendRow(table.Row{mapStr(k, "kind"), mapStr(k, "label"), mapStr(k, "version"), mapStr(k, "decider"), mapStr(k, "safe_outcome")})
	}
	renderTable(tbl)
	return nil
}

func runDecisionStats(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildDecisionStatsPath(c.String("workspace"), c.String("kind"), c.String("since")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "Since:      %s\n", mapStr(body, "since"))
	fmt.Fprintf(w, "Total:      %s\n", mapStr(body, "total"))
	for _, key := range []string{"outcomes", "statuses"} {
		if m, ok := body[key].(map[string]any); ok {
			parts := make([]string, 0, len(m))
			for k := range m {
				parts = append(parts, k+" "+mapStr(m, k))
			}
			fmt.Fprintf(w, "%-11s %s\n", strings.ToUpper(key[:1])+key[1:]+":", strings.Join(parts, ", "))
		}
	}
	fmt.Fprintf(w, "Feedback:   %s (agree rate %s)\n", mapStr(body, "feedback"), dash(mapStr(body, "agree_rate")))
	fmt.Fprintf(w, "Time:       p50 %sms, p95 %sms\n", mapStr(body, "p50_ms"), mapStr(body, "p95_ms"))
	return nil
}

func runDecisionTest(_ context.Context, c *cli.Command) error {
	kind := c.Args().Get(0)
	if kind == "" {
		return fmt.Errorf("usage: trebi decisions test <kind> --text <file>")
	}
	text, err := readFileOrStdin(strings.TrimPrefix(c.String("text"), "-"))
	if err != nil {
		return err
	}
	facts := map[string]string{}
	for _, kv := range c.StringSlice("fact") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("--fact must be key=value, got %q", kv)
		}
		facts[k] = v
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var d map[string]any
	if err := client.DoHTTP(request.BuildDecisionTest(kind, text, facts), &d); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, d)
	}
	printDecision(c, d)
	return nil
}

func runDecisionFeedback(_ context.Context, c *cli.Command) error {
	id, value := c.Args().Get(0), c.Args().Get(1)
	if id == "" || (value != "agree" && value != "disagree") {
		return fmt.Errorf("usage: trebi decisions feedback <id> agree|disagree")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var d map[string]any
	if err := client.DoHTTP(request.BuildDecisionFeedback(id, value), &d); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, d)
	}
	fmt.Fprintf(outWriter(c), "feedback %s: %s\n", value, id)
	return nil
}
