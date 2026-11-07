package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/pauses"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

// pauseEndFlags set the end of a pause and what happens to the missed runs.
func pauseEndFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "until", Usage: `end time: RFC 3339, "2006-01-02 15:04", "15:04", or "reset" (a harness usage limit reset)`},
		&cli.StringFlag{Name: "for", Usage: `length, such as 90m, 3h, 2d, or 1w`},
		&cli.BoolFlag{Name: "no-end", Usage: "pause until you resume"},
		&cli.StringFlag{Name: "missed", Usage: "missed scheduled runs: skip, or once (run each job once at the end)"},
		&cli.StringFlag{Name: "note", Usage: "a note for the pause"},
		&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
	}
}

func pauseCommand() *cli.Command {
	scope := func(name, usage, args string) *cli.Command {
		return &cli.Command{
			Name:      name,
			Usage:     usage,
			ArgsUsage: args,
			Flags:     pauseEndFlags(),
			Action: func(_ context.Context, c *cli.Command) error {
				return runPauseSet(c, pauses.Scope(name))
			},
		}
	}
	return &cli.Command{
		Name:  "pause",
		Usage: "pause scheduled and event runs in bulk; each job keeps its own on or off switch",
		Commands: []*cli.Command{
			scope("all", "pause everything", ""),
			scope("workspace", "pause one workspace", "<workspace>"),
			scope("harness", "pause the jobs of one AI app", "<harness>"),
			scope("agent", "pause one agent and its jobs (workspace from -w)", "<agent>"),
			{
				Name:    "ls",
				Aliases: []string{"list"},
				Usage:   "list the open pauses",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "all", Usage: "also list the ended pauses"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runPauseList,
			},
			{
				Name:      "set",
				Usage:     "change the end, the missed runs, or the note of a pause",
				ArgsUsage: "<pause-id>",
				Flags:     pauseEndFlags(),
				Action:    runPauseUpdate,
			},
			{
				Name:      "resume",
				Usage:     "end a pause now",
				ArgsUsage: "<pause-id> | all | workspace <name> | harness <name> | agent <name>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runPauseResume,
			},
		},
	}
}

// parsePauseAtFlag reads harness set --pause-at: 1 to 100, or off (0).
func parsePauseAtFlag(v string) (int, error) {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "off") {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
	if err != nil || n < 1 || n > 100 {
		return 0, fmt.Errorf("--pause-at must be 1 to 100, or off")
	}
	return n, nil
}

// pauseUntil reads --until into RFC 3339. A local time without a date is
// the next such time.
func pauseUntil(v string, now time.Time) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "reset" {
		return v, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, now.Location()); err == nil {
			return t.UTC().Format(time.RFC3339), nil
		}
	}
	if t, err := time.ParseInLocation("15:04", v, now.Location()); err == nil {
		at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at.UTC().Format(time.RFC3339), nil
	}
	return "", fmt.Errorf(`--until must be RFC 3339, "2006-01-02 15:04", "15:04", or reset`)
}

func pauseSpecFrom(c *cli.Command) (request.PauseSpec, error) {
	until, err := pauseUntil(c.String("until"), time.Now())
	if err != nil {
		return request.PauseSpec{}, err
	}
	spec := request.PauseSpec{Until: until, For: c.String("for"), NoEnd: c.Bool("no-end"), Missed: c.String("missed")}
	if c.IsSet("note") {
		note := c.String("note")
		spec.Note = &note
	}
	n := 0
	for _, set := range []bool{spec.Until != "", spec.For != "", spec.NoEnd} {
		if set {
			n++
		}
	}
	if n > 1 {
		return spec, fmt.Errorf("set one of --until, --for, or --no-end")
	}
	return spec, nil
}

func runPauseSet(c *cli.Command, scope pauses.Scope) error {
	spec, err := pauseSpecFrom(c)
	if err != nil {
		return err
	}
	spec.Scope = string(scope)
	if scope != pauses.ScopeAll {
		spec.Target = c.Args().First()
		if spec.Target == "" {
			return fmt.Errorf("%s required", scope)
		}
	}
	if scope == pauses.ScopeAgent {
		spec.Workspace = workspaceFor(c)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var p pauses.Pause
	if err := client.DoHTTP(request.BuildPauseSet(spec), &p); err != nil {
		return err
	}
	return printPause(c, p)
}

func runPauseUpdate(_ context.Context, c *cli.Command) error {
	id := c.Args().First()
	if id == "" {
		return fmt.Errorf("pause id required")
	}
	spec, err := pauseSpecFrom(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var p pauses.Pause
	if err := client.DoHTTP(request.BuildPauseUpdate(id, spec), &p); err != nil {
		return err
	}
	return printPause(c, p)
}

func runPauseResume(_ context.Context, c *cli.Command) error {
	args := c.Args().Slice()
	if len(args) == 0 {
		return fmt.Errorf("pause id or scope required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	id := args[0]
	if !strings.HasPrefix(id, "pse_") {
		if id, err = findPause(c, client, args); err != nil {
			return err
		}
	}
	var p pauses.Pause
	if err := client.DoHTTP(request.BuildPauseResume(id), &p); err != nil {
		return err
	}
	return printPause(c, p)
}

// findPause finds the open pause of a scope and target, such as
// "harness claude". A workspace or agent target matches by name or ID.
func findPause(c *cli.Command, client *APIClient, args []string) (string, error) {
	scope, target := pauses.Scope(args[0]), ""
	if len(args) > 1 {
		target = args[1]
	}
	var body struct {
		Pauses []pauses.Pause `json:"pauses"`
	}
	if err := client.DoHTTP(request.BuildPauseList(false), &body); err != nil {
		return "", err
	}
	if scope == pauses.ScopeWorkspace {
		if id := resolveWorkspaceHint(c, target); id != "" {
			target = id
		}
	}
	for _, p := range body.Pauses {
		if p.Scope != scope {
			continue
		}
		if scope == pauses.ScopeAll || p.Target == target || strings.EqualFold(p.Label, target) {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("no open pause for %s", strings.Join(args, " "))
}

func runPauseList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Pauses []pauses.Pause `json:"pauses"`
	}
	if err := client.DoHTTP(request.BuildPauseList(c.Bool("all")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	if len(body.Pauses) == 0 {
		fmt.Fprintln(w, "Nothing is paused.")
		return nil
	}
	tbl := newTable(w, []string{"ID", "Paused", "Scope", "Ends", "Missed", "Skipped", "By", "Status"})
	for _, p := range body.Pauses {
		status := "active"
		if !p.Active {
			status = "ended"
		}
		tbl.AppendRow(table.Row{p.ID, p.Label, string(p.Scope), pauseEnd(p), string(p.Missed), p.SkippedFires, p.CreatedBy, status})
	}
	tbl.Render()
	return nil
}

func pauseEnd(p pauses.Pause) string {
	if p.Until == nil {
		return "until resumed"
	}
	if t, err := time.Parse(time.RFC3339, *p.Until); err == nil {
		return "until " + t.Local().Format("2006-01-02 15:04")
	}
	return "until " + *p.Until
}

func printPause(c *cli.Command, p pauses.Pause) error {
	if c.Bool("json") {
		return encodeJSON(c, p)
	}
	w := outWriter(c)
	if !p.Active {
		fmt.Fprintf(w, "%s resumed (%s). %d scheduled runs skipped.\n", p.Label, p.ID, p.SkippedFires)
		return nil
	}
	fmt.Fprintf(w, "%s paused %s (%s), missed runs: %s.\n", p.Label, pauseEnd(p), p.ID, p.Missed)
	return nil
}
