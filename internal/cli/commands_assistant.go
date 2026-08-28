package cli

import (
	"encoding/json"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func assistantCommand() *cli.Command {
	return &cli.Command{
		Name:  "assistant",
		Usage: "workspace assistant (singleton TREBI.md)",
		Subcommands: []*cli.Command{
			{
				Name:  "show",
				Usage: "show assistant for current/default workspace",
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					id, err := resolveWorkspaceID(c, client)
					if err != nil {
						return err
					}
					var body map[string]any
					if err := client.GetJSON("/v1/workspaces/"+id+"/assistant", &body); err != nil {
						return err
					}
					enc := json.NewEncoder(outWriter(c))
					enc.SetIndent("", "  ")
					return enc.Encode(body)
				},
			},
			{
				Name:  "wake",
				Usage: "run the workspace assistant (TREBI.md → kind=assistant run)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "extra-prompt", Aliases: []string{"e"}, Usage: "append text to the assistant prompt"},
					&cli.BoolFlag{Name: "test", Usage: "dry_run: evaluate gates, print plan, do not launch"},
					&cli.BoolFlag{Name: "attach", Aliases: []string{"a"}, Usage: "after launch: attach (tmux local or WebSocket remote)"},
				},
				Action: runAssistantWake,
			},
			{
				Name:  "memory",
				Usage: "assistant MEMORY.md ledger",
				Subcommands: []*cli.Command{
					{
						Name:   "show",
						Usage:  "print MEMORY.md",
						Flags:  []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
						Action: runAssistantMemory,
					},
					{
						Name:  "add",
						Usage: "append a ledger entry",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "text", Usage: "entry text", Required: true},
							&cli.StringFlag{Name: "date", Usage: "YYYY-MM-DD (default today)"},
						},
						Action: runAssistantMemoryAdd,
					},
					{
						Name:      "remove",
						Usage:     "delete a ledger entry",
						ArgsUsage: "<entry-id>",
						Action:    runAssistantMemoryRemove,
					},
				},
			},
			{
				Name:  "history",
				Usage: "list assistant runs (shortcut for runs list --kind assistant)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "status", Aliases: []string{"s"}, Usage: "status filter"},
					&cli.StringFlag{Name: "job", Usage: "assistant/job name filter"},
					&cli.StringFlag{Name: "harness", Usage: "harness filter"},
					&cli.StringFlag{Name: "model", Aliases: []string{"m"}, Usage: "model filter"},
					&cli.StringFlag{Name: "conversation", Usage: "substring match on conversation_key (client-side)"},
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (run id)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 20, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runAssistantHistory,
			},
		},
	}
}

func runAssistantMemory(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON("/v1/workspaces/"+wsID+"/assistant/memory", &body); err != nil {
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

func runAssistantMemoryAdd(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildAssistantMemoryEntryAdd(wsID, c.String("date"), c.String("text")), nil)
}

func runAssistantMemoryRemove(c *cli.Context) error {
	eid := c.Args().Get(0)
	if eid == "" {
		return fmt.Errorf("usage: trebi assistant memory remove <entry-id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildAssistantMemoryEntryDelete(wsID, eid), nil)
}

func runAssistantWake(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	promptOverride := ""
	if extra := c.String("extra-prompt"); extra != "" {
		var got map[string]any
		if err := client.GetJSON("/v1/workspaces/"+wsID+"/assistant", &got); err != nil {
			return err
		}
		base := mapStr(got, "prompt", "content")
		// content may be full markdown; prefer prompt field when present.
		if mapStr(got, "prompt") == "" {
			base = mapStr(got, "content")
		}
		promptOverride = AppendExtraPrompt(base, extra)
	}
	req := BuildAssistantWake(wsID, promptOverride, c.Bool("test"))
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
	fmt.Fprintf(outWriter(c), "run %v status=%v kind=assistant\n", runID, out["status"])
	if c.Bool("attach") && runID != "" {
		return attachAfterLaunch(c, client, out)
	}
	return nil
}

func runAssistantHistory(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws := workspaceFor(c)
	path := BuildRunsListPath(
		c.String("status"), c.String("job"), ws, "assistant",
		c.String("cursor"), c.String("harness"), c.String("model"), c.Int("limit"),
	)
	conv := c.String("conversation")
	if conv != "" && c.Int("limit") > 0 {
		path = BuildRunsListPath(
			c.String("status"), c.String("job"), ws, "assistant",
			c.String("cursor"), c.String("harness"), c.String("model"), c.Int("limit")*5,
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
		tbl.AppendRow(table.Row{
			mapStr(r, "id"),
			mapStr(r, "job"),
			dash(mapStr(r, "kind")),
			colorStatus(w, mapStr(r, "status")),
			dash(mapStr(r, "harness")),
			dash(mapStr(r, "started_at")),
			dash(mapStr(r, "reason")),
			dash(mapStr(r, "conversation_key")),
		})
	}
	renderTable(tbl)
	return nil
}
