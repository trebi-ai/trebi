package cli

import (
	"context"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func conversationCommand() *cli.Command {
	return &cli.Command{
		Name:  "conversation",
		Usage: "list and inspect conversations",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list conversations",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "channel", Usage: "origin or channel_type filter"},
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (conversation id)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runConversationList,
			},
			{
				Name:      "show",
				Usage:     "show a conversation",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runConversationShow,
			},
			{
				Name:      "messages",
				Usage:     "list messages in a conversation",
				ArgsUsage: "<id>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (message id)"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runConversationMessages,
			},
			{
				Name:    "create",
				Aliases: []string{"new"},
				Usage:   "create a conversation and point the cli channel at it",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "title", Usage: "optional title"},
					&cli.StringFlag{Name: "agent", Usage: "target an agent (short id)"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runConversationNew,
			},
			{
				Name:      "switch",
				Usage:     "point the cli channel at a conversation",
				ArgsUsage: "<id>",
				Action:    runConversationSwitch,
			},
			{
				Name:      "patch",
				Usage:     "update conversation title or summary",
				ArgsUsage: "<id>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "title", Usage: "new title"},
					&cli.StringFlag{Name: "summary", Usage: "new summary"},
					&cli.Int64Flag{Name: "revision", Usage: "expected revision"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runConversationPatch,
			},
			{
				Name:      "outbox",
				Usage:     "list the channel replies of a conversation",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runConversationOutbox,
				Commands: []*cli.Command{
					{
						Name:      "retry",
						Usage:     "retry one failed channel reply",
						ArgsUsage: "<id> <row>",
						Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
						Action:    runConversationOutboxRetry,
					},
				},
			},
			{
				Name:      "delete",
				Usage:     "delete a conversation with its messages (closes the assistant session)",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{forceFlag("required; deletes the conversation with its messages and events")},
				Action:    runConversationDelete,
			},
		},
	}
}

func runConversationList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws, _ := resolveWorkspaceID(c, client)
	path := request.BuildConversationsListPath(ws, c.String("channel"),
		request.AgentIndexName(c.String("agent")), c.String("cursor"), c.Int("limit"))
	var body struct {
		Conversations []map[string]any `json:"conversations"`
		NextCursor    string           `json:"next_cursor"`
	}
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Workspace", "Key", "Title", "Updated"})
	for _, conv := range body.Conversations {
		tbl.AppendRow(table.Row{
			mapStr(conv, "id"), mapStr(conv, "workspace"),
			dash(mapStr(conv, "key")), dash(mapStr(conv, "title")),
			dash(mapStr(conv, "updated_at")),
		})
	}
	renderTable(tbl)
	return nil
}

func runConversationShow(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi conversation show <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConversationGet(id), &body); err != nil {
		return err
	}
	return encodeJSON(c, body)
}

func runConversationMessages(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi conversation messages <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := request.BuildConversationMessagesPath(id, c.String("cursor"), c.Int("limit"))
	var body struct {
		Messages   []map[string]any `json:"messages"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Role", "Channel", "Text"})
	for _, m := range body.Messages {
		tbl.AppendRow(table.Row{
			mapStr(m, "id"), mapStr(m, "role"),
			dash(mapStr(m, "channel_type")), truncateDisplay(mapStr(m, "text"), 60),
		})
	}
	renderTable(tbl)
	return nil
}

func runConversationOutbox(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi conversation outbox <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := request.BuildConversationOutboxPath(id)
	var body struct {
		Outbox []map[string]any `json:"outbox"`
	}
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Status", "Attempts", "Message", "Error"})
	for _, row := range body.Outbox {
		tbl.AppendRow(table.Row{
			mapStr(row, "id"), mapStr(row, "status"), mapStr(row, "attempts"),
			mapStr(row, "message_id"), dash(mapStr(row, "error_message")),
		})
	}
	renderTable(tbl)
	return nil
}

func runConversationOutboxRetry(_ context.Context, c *cli.Command) error {
	id, row := c.Args().Get(0), c.Args().Get(1)
	if id == "" || row == "" {
		return fmt.Errorf("usage: trebi conversation outbox retry <id> <row>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildConversationOutboxRetry(id, row), &out); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	fmt.Fprintf(outWriter(c), "outbox %s %s\n", mapStr(out, "id"), mapStr(out, "status"))
	return nil
}

func runConversationNew(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildConversationCreate(wsID, c.String("title"), "cli", "cli", wsID,
		request.AgentIndexName(c.String("agent"))), &out); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	fmt.Fprintf(outWriter(c), "conversation %s\n", mapStr(out, "id"))
	return nil
}

func runConversationSwitch(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi conversation switch <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildPointerPut("cli", wsID, id), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "switched cli → %s\n", id)
	return nil
}

func runConversationPatch(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi conversation patch <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	req := request.BuildConversationPatch(id, setStr(c, "title"), setStr(c, "summary"), c.Int64("revision"))
	if err := client.DoHTTP(req, &out); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	fmt.Fprintf(outWriter(c), "conversation %s\n", mapStr(out, "id"))
	return nil
}

func runConversationDelete(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi conversation delete <id>")
	}
	if _, err := confirmed(c, "deletes conversation "+id+" with its messages"); err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildConversationDelete(id, true), nil)
}
