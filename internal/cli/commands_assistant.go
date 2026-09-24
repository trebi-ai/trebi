package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
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
					if err := client.GetJSON(request.BuildAssistantGetPath(id), &body); err != nil {
						return err
					}
					return encodeJSON(c, body)
				},
			},
			{
				Name:  "set",
				Usage: "replace TREBI.md (--file or stdin)",
				Flags: []cli.Flag{&cli.StringFlag{Name: "file", Usage: "path to TREBI.md"}, ifMatchFlag()},
				Action: func(_ context.Context, c *cli.Command) error {
					content, err := readFileOrStdin(c.String("file"))
					if err != nil {
						return err
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					id, err := resolveWorkspaceID(c, client)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildAssistantPut(id, content, c.String("if-match")), nil)
				},
			},
			{
				Name:    "delete",
				Aliases: []string{"remove"},
				Usage:   "delete TREBI.md of the current workspace",
				Flags:   []cli.Flag{forceFlag("required; deletes the assistant definition"), ifMatchFlag()},
				Action: func(_ context.Context, c *cli.Command) error {
					if _, err := confirmed(c, "deletes the workspace assistant definition"); err != nil {
						return err
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					id, err := resolveWorkspaceID(c, client)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildAssistantDelete(id, c.String("if-match")), nil)
				},
			},
			{
				Name:  "patch",
				Usage: "enable or disable the workspace assistant",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "disabled", Usage: "disable the assistant (--disabled=false enables it)"},
					ifMatchFlag(),
				},
				Action: runAssistantPatch,
			},
			{
				Name:  "wake",
				Usage: "run the workspace assistant (TREBI.md → kind=assistant run)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "extra-prompt", Aliases: []string{"e"}, Usage: "append text to the assistant prompt (server-side)"},
					&cli.StringFlag{Name: "prompt", Usage: "replace the assistant prompt"},
					&cli.StringSliceFlag{Name: "input", Aliases: []string{"i"}, Usage: "KEY=VALUE placeholder (repeatable)"},
					&cli.StringFlag{Name: "harness", Usage: "override the harness"},
					&cli.StringFlag{Name: "conversation", Usage: "attach the run to a conversation id"},
					&cli.BoolFlag{Name: "test", Usage: "dry_run: evaluate gates, print plan, do not launch"},
					&cli.BoolFlag{Name: "attach", Aliases: []string{"a"}, Usage: "after launch: attach (tmux local or WebSocket remote)"},
					&cli.IntFlag{Name: "wait", Usage: "wait milliseconds for the run to end (max 120000)"},
				},
				Action: runAssistantWake,
			},
			{
				Name:      "say",
				Usage:     "send a chat message to the workspace assistant",
				ArgsUsage: "<message>",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "wait", Usage: "wait milliseconds for the reply (max 120000)"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runAssistantSay,
			},
			{
				Name:   "chat",
				Usage:  "interactive assistant chat (POST + SSE refetch)",
				Action: runAssistantChat,
			},
			memorySubcommands(assistantMemoryOwner()),
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

func assistantMemoryOwner() memoryOwner {
	return memoryOwner{
		noun: "assistant",
		resolve: func(c *cli.Command) (*APIClient, string, string, error) {
			client, err := clientFor(c)
			if err != nil {
				return nil, "", "", err
			}
			wsID, err := resolveWorkspaceID(c, client)
			if err != nil {
				return nil, "", "", err
			}
			return client, "", wsID, nil
		},
		req: request.AssistantMemoryRequests(),
	}
}

func runAssistantPatch(_ context.Context, c *cli.Command) error {
	if !c.IsSet("disabled") {
		return fmt.Errorf("--disabled required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	id, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	disabled := c.Bool("disabled")
	return client.DoHTTP(request.BuildAssistantPatch(id, &disabled, c.String("if-match")), nil)
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
	return launchAndMaybeAttach(ctx, c, client, request.RunSpec{Workspace: wsID}, "assistant", request.RunSpec.BuildWake)
}

func runAssistantHistory(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws := workspaceFor(c)
	q := request.RunsQuery{
		Status: c.String("status"), Job: c.String("job"), Workspace: ws, Kind: "assistant",
		Cursor: c.String("cursor"), Harness: c.String("harness"), Model: c.String("model"),
		Limit: c.Int("limit"),
	}
	conv := c.String("conversation")
	if conv != "" && q.Limit > 0 {
		// The substring match runs client-side, so over-fetch before filtering.
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
	var out map[string]any
	spec := request.AskSpec{Workspace: wsID, Text: text, WaitMS: c.Int("wait")}
	if err := client.DoHTTP(spec.Build(), &out); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	fmt.Fprintf(outWriter(c), "delivery=%s conversation=%s\n", mapStr(out, "delivery"), mapStr(out, "conversation_id"))
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
	convID := client.CLIConversation(wsID)
	w := outWriter(c)
	fmt.Fprintf(w, "conversation %s  (/new /switch <id> /quit)\n", dash(convID))
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
		if err := client.DoHTTP(request.BuildConversationCreate(wsID, "", "cli", "cli", wsID, ""), &out); err != nil {
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
		if err := client.DoHTTP(request.BuildPointerPut("cli", wsID, id), nil); err != nil {
			return false, err
		}
		*convID = id
		fmt.Fprintf(outWriter(c), "switched → %s\n", id)
		return true, nil
	default:
		var out map[string]any
		req := request.AskSpec{Workspace: wsID, Text: line}.Build()
		if *convID != "" {
			req = request.BuildConversationSend(*convID, line, "cli", wsID, "")
		}
		if err := client.DoHTTP(req, &out); err != nil {
			return true, err
		}
		if *convID == "" {
			*convID = mapStr(out, "conversation_id")
		}
		fmt.Fprintf(outWriter(c), "… %s\n", mapStr(out, "delivery"))
		return true, waitAssistantReply(c, client, *convID)
	}
}

func waitAssistantReply(c *cli.Command, client *APIClient, convID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	seen := client.MessageIDSet(convID)
	err := client.StreamEvents(ctx, request.BuildStreamPath("conversation.updated,session.state", "0"), func(ev store.StreamEvent) error {
		if ev.Topic == "session.state" {
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

func printNewAssistant(c *cli.Command, client *APIClient, convID string, seen map[string]struct{}) bool {
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := client.GetJSON(request.BuildConversationMessagesPath(convID, "", 200), &body); err != nil {
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
