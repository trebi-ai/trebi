package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/urfave/cli/v2"
)

func outWriter(c *cli.Context) io.Writer {
	if c != nil && c.App != nil && c.App.Writer != nil {
		return c.App.Writer
	}
	return os.Stdout
}

func scheduleCommand() *cli.Command {
	return &cli.Command{
		Name:  "schedule",
		Usage: "schedules",
		Subcommands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list schedules",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var body struct {
						Schedules []map[string]any `json:"schedules"`
					}
					if err := client.GetJSON(BuildSchedulesListPath(c.Int("limit")), &body); err != nil {
						return err
					}
					if c.Bool("json") {
						enc := json.NewEncoder(outWriter(c))
						enc.SetIndent("", "  ")
						return enc.Encode(body)
					}
					w := outWriter(c)
					tbl := newTable(w, []string{"Workspace", "Job", "Spec", "Timezone", "Next", "Last"})
					for _, s := range body.Schedules {
						tbl.AppendRow(table.Row{
							mapStr(s, "workspace"),
							mapStr(s, "job"),
							mapStr(s, "spec"),
							dash(mapStr(s, "timezone")),
							dash(mapStr(s, "next_due_at")),
							dash(mapStr(s, "last_fired_at")),
						})
					}
					renderTable(tbl)
					return nil
				},
			},
		},
	}
}

func doctorCommand() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "system doctor findings",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		},
		Action: func(c *cli.Context) error {
			client, err := clientFor(c)
			if err != nil {
				// offline checks
				fmt.Printf("home=%s\n", home.Base())
				if _, err := os.Stat(home.DBPath()); err != nil {
					fmt.Println("db: missing (run trebi service migrate-db)")
				} else {
					fmt.Println("db: present")
				}
				return nil
			}
			var body map[string]any
			if err := client.GetJSON("/v1/system/doctor", &body); err != nil {
				return err
			}
			if c.Bool("json") {
				enc := json.NewEncoder(outWriter(c))
				enc.SetIndent("", "  ")
				return enc.Encode(body)
			}
			return renderDoctor(outWriter(c), body)
		},
	}
}

func renderDoctor(w io.Writer, body map[string]any) error {
	// Prefer findings list if present; otherwise flatten top-level scalars.
	if findings, ok := body["findings"].([]any); ok && len(findings) > 0 {
		tbl := newTable(w, []string{"Severity", "Code", "Message"})
		for _, raw := range findings {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			tbl.AppendRow(table.Row{
				mapStr(m, "severity", "level", "status"),
				mapStr(m, "code", "id", "name"),
				mapStr(m, "message", "detail", "msg"),
			})
		}
		renderTable(tbl)
		return nil
	}
	tbl := newTable(w, []string{"Key", "Value"})
	for k, v := range body {
		if k == "findings" {
			continue
		}
		tbl.AppendRow(table.Row{k, fmt.Sprint(v)})
	}
	renderTable(tbl)
	return nil
}

func busCommand() *cli.Command {
	return &cli.Command{
		Name:  "bus",
		Usage: "tail the event bus (not trigger events)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "topics", Usage: "comma-separated topic filter"},
		},
		Action: func(c *cli.Context) error {
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			// one-shot catch-up via health + note
			var body map[string]any
			if err := client.GetJSON("/v1/system/health", &body); err != nil {
				return err
			}
			fmt.Println("bus live tail: connect to GET /v1/stream (SSE)")
			fmt.Printf("daemon ok version=%v\n", body["version"])
			return nil
		},
	}
}
