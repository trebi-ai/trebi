package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/urfave/cli/v3"
)

func assistantCommand() *cli.Command {
	return &cli.Command{
		Name:  "assistant",
		Usage: "workspace assistant (singleton TREBI.md)",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "show assistant for current/default workspace",
				Action: func(_ context.Context, c *cli.Command) error {
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
					&cli.StringFlag{Name: "extra-prompt", Aliases: []string{"e"}, Usage: "append text to the assistant prompt (server-side)"},
					&cli.BoolFlag{Name: "test", Usage: "dry_run: evaluate gates, print plan, do not launch"},
					&cli.BoolFlag{Name: "attach", Aliases: []string{"a"}, Usage: "after launch: attach (tmux local or WebSocket remote)"},
				},
				Action: runAssistantWake,
			},
			{
				Name:      "say",
				Usage:     "send a chat message to the workspace assistant",
				ArgsUsage: "<message>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runAssistantSay,
			},
			{
				Name:   "chat",
				Usage:  "interactive assistant chat (POST + SSE refetch)",
				Action: runAssistantChat,
			},
			{
				Name:  "memory",
				Usage: "assistant MEMORY.md ledger",
				Commands: []*cli.Command{
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
					{
						Name:   "set",
						Usage:  "replace MEMORY.md (--file or stdin)",
						Flags:  []cli.Flag{&cli.StringFlag{Name: "file", Usage: "path to MEMORY.md"}},
						Action: runAssistantMemorySet,
					},
					{
						Name:   "clear",
						Usage:  "delete MEMORY.md (destroys the ledger)",
						Flags:  []cli.Flag{&cli.BoolFlag{Name: "force", Aliases: []string{"f"}, Usage: "required; destroys the ledger"}},
						Action: runAssistantMemoryClear,
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

func runAssistantMemory(_ context.Context, c *cli.Command) error {
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

func runAssistantMemoryAdd(_ context.Context, c *cli.Command) error {
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

func runAssistantMemoryRemove(_ context.Context, c *cli.Command) error {
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

func runAssistantMemorySet(_ context.Context, c *cli.Command) error {
	content, err := readFileOrStdin(c.String("file"))
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildAssistantMemoryPut(wsID, content), nil)
}

func runAssistantMemoryClear(_ context.Context, c *cli.Command) error {
	if !c.Bool("force") {
		return fmt.Errorf("refusing to clear assistant memory without --force")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(BuildAssistantMemoryClear(wsID), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "cleared assistant memory for %s\n", wsID)
	return nil
}

func runAssistantWake(ctx context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	req := BuildAssistantWake(wsID, c.String("extra-prompt"), c.Bool("test"))
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
		return attachAfterLaunch(ctx, c, client, out)
	}
	return nil
}

func runAssistantHistory(_ context.Context, c *cli.Command) error {
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

func runAssistantSay(_ context.Context, c *cli.Command) error {
	text := strings.TrimSpace(strings.Join(c.Args().Slice(), " "))
	if text == "" {
		return fmt.Errorf("usage: trebi assistant say <message>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	convID, err := ensureCLIConversation(client, wsID)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(BuildAssistantSay(convID, text, wsID), &out); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Fprintf(outWriter(c), "delivery=%s conversation=%s\n", mapStr(out, "delivery"), convID)
	return nil
}

func runAssistantChat(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	convID, err := ensureCLIConversation(client, wsID)
	if err != nil {
		return err
	}
	w := outWriter(c)
	fmt.Fprintf(w, "conversation %s  (/new /switch <id> /quit)\n", convID)
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Fprint(w, "> ")
		if !sc.Scan() {
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		next, err := chatLine(c, client, wsID, &convID, line)
		if err != nil {
			return err
		}
		if !next {
			return nil
		}
	}
}

func chatLine(c *cli.Command, client *APIClient, wsID string, convID *string, line string) (bool, error) {
	switch {
	case line == "":
		return true, nil
	case line == "/quit" || line == "/exit":
		return false, nil
	case line == "/new":
		var out map[string]any
		if err := client.DoHTTP(BuildConversationCreate(wsID, "", "cli", "cli", wsID), &out); err != nil {
			return false, err
		}
		*convID = mapStr(out, "id")
		fmt.Fprintf(outWriter(c), "conversation %s\n", *convID)
		return true, nil
	case strings.HasPrefix(line, "/switch"):
		id := strings.TrimSpace(strings.TrimPrefix(line, "/switch"))
		if id == "" {
			return true, fmt.Errorf("usage: /switch <id>")
		}
		if err := client.DoHTTP(BuildPointerPut("cli", wsID, id), nil); err != nil {
			return false, err
		}
		*convID = id
		fmt.Fprintf(outWriter(c), "switched → %s\n", id)
		return true, nil
	default:
		return true, chatSend(c, client, wsID, *convID, line)
	}
}

func chatSend(c *cli.Command, client *APIClient, wsID, convID, text string) error {
	var out map[string]any
	if err := client.DoHTTP(BuildAssistantSay(convID, text, wsID), &out); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "… %s\n", mapStr(out, "delivery"))
	return waitAssistantReply(c, client, convID)
}

func waitAssistantReply(c *cli.Command, client *APIClient, convID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	seen := messageIDs(client, convID)
	err := client.StreamEvents(ctx, BuildStreamPath("conversation.updated,agent.state", "0"), func(ev store.StreamEvent) error {
		if ev.Topic == "agent.state" {
			fmt.Fprint(outWriter(c), "thinking…\r")
			return nil
		}
		if ev.Topic != "conversation.updated" || ev.ID != convID {
			return nil
		}
		if printNewAssistant(c, client, convID, seen) {
			cancel()
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func messageIDs(client *APIClient, convID string) map[string]struct{} {
	out := map[string]struct{}{}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := client.GetJSON(BuildConversationMessagesPath(convID, "", 200), &body); err != nil {
		return out
	}
	for _, m := range body.Messages {
		out[mapStr(m, "id")] = struct{}{}
	}
	return out
}

func printNewAssistant(c *cli.Command, client *APIClient, convID string, seen map[string]struct{}) bool {
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := client.GetJSON(BuildConversationMessagesPath(convID, "", 200), &body); err != nil {
		return false
	}
	printed := false
	for _, m := range body.Messages {
		id := mapStr(m, "id")
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if mapStr(m, "role") != "assistant" {
			continue
		}
		fmt.Fprintf(outWriter(c), "assistant: %s\n", mapStr(m, "text"))
		printed = true
	}
	return printed
}

func ensureCLIConversation(client *APIClient, wsID string) (string, error) {
	var ptr map[string]any
	err := client.DoHTTP(BuildPointerGet("cli", wsID), &ptr)
	if err == nil {
		if id := mapStr(ptr, "conversation_id"); id != "" {
			return id, nil
		}
	}
	if err != nil && !strings.Contains(err.Error(), "404") {
		return "", err
	}
	var created map[string]any
	if err := client.DoHTTP(BuildConversationCreate(wsID, "", "cli", "cli", wsID), &created); err != nil {
		return "", err
	}
	return mapStr(created, "id"), nil
}
