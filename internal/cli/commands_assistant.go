package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

// assistantSetFields are the settings fields `assistant set` can write or
// unset. They mirror assistant.Override; email and triggers stay YAML-only in
// trebi.yaml for now.
var assistantSetFields = []string{"harness", "model", "effort", "mode", "skills", "context", "disabled"} //nolint:gochecknoglobals // fixed list, never changed

func assistantCommand() *cli.Command {
	return &cli.Command{
		Name:  "assistant",
		Usage: "workspace assistant sessions and settings",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "show effective assistant settings (add --defaults for the global defaults)",
				Flags: []cli.Flag{
					assistantWorkspaceFlag(),
					&cli.BoolFlag{Name: "defaults", Usage: "show the global defaults and the harness list"},
				},
				Action: runAssistantShow,
			},
			{
				Name:  "set",
				Usage: "write assistant settings (one field per flag; --unset removes a workspace override)",
				Flags: append(assistantSetFlags(),
					assistantWorkspaceFlag(),
					&cli.BoolFlag{Name: "defaults", Usage: "write the global defaults instead of the workspace override"},
				),
				Action: runAssistantSet,
			},
			{
				Name:      "say",
				Usage:     "send a chat message to the workspace assistant",
				ArgsUsage: "<message>",
				Flags: []cli.Flag{
					assistantWorkspaceFlag(),
					&cli.IntFlag{Name: "wait", Usage: "wait milliseconds for the reply (max 120000)"},
					&cli.StringSliceFlag{Name: "attach", Usage: "image file to send with the message (repeatable)"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runAssistantSay,
			},
			{
				Name:   "chat",
				Usage:  "interactive assistant chat with streamed replies",
				Flags:  []cli.Flag{assistantWorkspaceFlag()},
				Action: runAssistantChat,
			},
			{
				Name:  "sessions",
				Usage: "list assistant conversations with status and harness",
				Flags: []cli.Flag{
					assistantWorkspaceFlag(),
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runAssistantSessions,
			},
			{
				Name:      "close",
				Usage:     "close the assistant process of a conversation (the conversation stays)",
				ArgsUsage: "<conversation id>",
				Action:    runAssistantClose,
			},
			{
				Name:      "interrupt",
				Usage:     "interrupt the running assistant turn of a conversation",
				ArgsUsage: "<conversation id>",
				Action:    runAssistantInterrupt,
			},
			{
				Name:  "preview",
				Usage: "print the composed instructions of a new assistant session",
				Flags: []cli.Flag{
					assistantWorkspaceFlag(),
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runAssistantPreview,
			},
			memorySubcommands(assistantMemoryOwner()),
		},
	}
}

func assistantWorkspaceFlag() cli.Flag {
	return &cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"}
}

func assistantSetFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "harness", Usage: "harness name; empty means auto"},
		&cli.StringFlag{Name: "model", Usage: "model for the harness"},
		&cli.StringFlag{Name: "effort", Usage: "effort: low|medium|high"},
		&cli.StringFlag{Name: "mode", Usage: "permission mode: ask|auto_edit|auto"},
		&cli.StringSliceFlag{Name: "skill", Usage: "skill name (repeatable)"},
		&cli.StringFlag{Name: "context-file", Usage: "file with the context prompt text"},
		&cli.BoolFlag{Name: "disabled", Usage: "disable the assistant (--disabled=false enables it)"},
		&cli.StringSliceFlag{Name: "unset", Usage: "field to remove: " + strings.Join(assistantSetFields, "|")},
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
		root: func(_, ws string) request.MemoryRoot { return request.AssistantMemory(ws) },
	}
}

func runAssistantShow(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if c.Bool("defaults") {
		var body map[string]any
		if err := client.DoHTTP(request.BuildAssistantDefaultsGet(), &body); err != nil {
			return err
		}
		return encodeJSON(c, body)
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildAssistantGetPath(wsID), &body); err != nil {
		return err
	}
	return encodeJSON(c, body)
}

// runAssistantSet writes one settings layer. Defaults scope reads the current
// settings, applies the flags, and PUTs the object back. Workspace scope
// applies the flags to the override object and PUTs it with the hash the read
// returned, so a concurrent edit fails with 412 instead of being lost.
func runAssistantSet(_ context.Context, c *cli.Command) error {
	changes, unset, err := assistantSetChanges(c)
	if err != nil {
		return err
	}
	if len(changes) == 0 && len(unset) == 0 {
		return fmt.Errorf("give at least one of --harness --model --effort --mode --skill --context-file --disabled --unset")
	}
	if c.Bool("defaults") {
		// The global layer has no disabled key; only a workspace can
		// disable the assistant.
		if _, ok := changes["disabled"]; ok {
			return fmt.Errorf("--disabled is a workspace override; it has no --defaults form")
		}
		for _, f := range unset {
			if f == "disabled" {
				return fmt.Errorf("--unset disabled is a workspace override; it has no --defaults form")
			}
		}
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if c.Bool("defaults") {
		return assistantSetDefaults(c, client, changes, unset)
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	return assistantSetWorkspace(client, wsID, changes, unset)
}

func assistantSetChanges(c *cli.Command) (changes map[string]any, unset []string, err error) {
	changes = map[string]any{}
	for _, f := range []string{"harness", "model", "effort", "mode"} {
		if c.IsSet(f) {
			changes[f] = c.String(f)
		}
	}
	if c.IsSet("skill") {
		changes["skills"] = c.StringSlice("skill")
	}
	if c.IsSet("context-file") {
		text, err := readFileOrStdin(c.String("context-file"))
		if err != nil {
			return nil, nil, err
		}
		changes["context"] = text
	}
	if c.IsSet("disabled") {
		changes["disabled"] = c.Bool("disabled")
	}
	unset = c.StringSlice("unset")
	known := map[string]bool{}
	for _, f := range assistantSetFields {
		known[f] = true
	}
	for _, f := range unset {
		if !known[f] {
			return nil, nil, fmt.Errorf("unknown --unset field %q (want %s)", f, strings.Join(assistantSetFields, "|"))
		}
		if _, clash := changes[f]; clash {
			return nil, nil, fmt.Errorf("--unset %s conflicts with the flag that sets it", f)
		}
	}
	return changes, unset, nil
}

// assistantSetDefaults resets an unset field to its empty value: the daemon
// then behaves as if config.yaml never carried it.
func assistantSetDefaults(c *cli.Command, client *APIClient, changes map[string]any, unset []string) error {
	var body struct {
		Settings map[string]any `json:"settings"`
	}
	if err := client.DoHTTP(request.BuildAssistantDefaultsGet(), &body); err != nil {
		return err
	}
	if body.Settings == nil {
		body.Settings = map[string]any{}
	}
	for _, f := range unset {
		switch f {
		case "skills":
			body.Settings[f] = []string{}
		default:
			body.Settings[f] = ""
		}
	}
	for f, v := range changes {
		body.Settings[f] = v
	}
	return client.DoHTTP(request.BuildAssistantDefaultsPut(body.Settings), nil)
}

func assistantSetWorkspace(client *APIClient, wsID string, changes map[string]any, unset []string) error {
	var body struct {
		Override map[string]any `json:"override"`
		Hash     string         `json:"hash"`
	}
	if err := client.GetJSON(request.BuildAssistantGetPath(wsID), &body); err != nil {
		return err
	}
	if body.Override == nil {
		body.Override = map[string]any{}
	}
	for _, f := range unset {
		delete(body.Override, f)
	}
	for f, v := range changes {
		body.Override[f] = v
	}
	return client.DoHTTP(request.BuildAssistantSettingsPut(wsID, body.Override, body.Hash), nil)
}

func runAssistantSay(_ context.Context, c *cli.Command) error {
	text := strings.TrimSpace(strings.Join(c.Args().Slice(), " "))
	attach := c.StringSlice("attach")
	if text == "" && len(attach) == 0 {
		return fmt.Errorf("usage: trebi assistant say <message> [--attach <image>]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	ids, err := client.UploadImages(wsID, attach)
	if err != nil {
		return err
	}
	var out map[string]any
	spec := request.AskSpec{Workspace: wsID, Text: text, WaitMS: c.Int("wait"), Attachments: ids}
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
	return assistantChatLoop(c, client, wsID, os.Stdin)
}

// assistantChatLoop is the interactive chat: one line per turn, replies
// streamed from the conversation event stream.
func assistantChatLoop(c *cli.Command, client *APIClient, wsID string, in io.Reader) error {
	convID := client.CLIConversation(wsID)
	reader := bufio.NewReader(in)
	w := outWriter(c)
	fmt.Fprintf(w, "conversation %s  (/new /switch <id> /quit)\n", dash(convID))
	for {
		fmt.Fprint(w, "> ")
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		next, err := chatLine(c, client, wsID, &convID, strings.TrimSpace(line), reader)
		if err != nil {
			return err
		}
		if !next {
			return nil
		}
	}
}

func chatLine(c *cli.Command, client *APIClient, wsID string, convID *string, line string, in *bufio.Reader) (bool, error) {
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
		if err := client.DoHTTP(request.BuildPointerPut("cli", wsID, "", id), nil); err != nil {
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
		return true, followAssistantTurn(c, client, *convID, in)
	}
}

// followAssistantTurn streams the conversation events until the turn ends.
// Text prints as it arrives; tool calls print as one line each; permission
// requests are answered from stdin.
func followAssistantTurn(c *cli.Command, client *APIClient, convID string, in *bufio.Reader) error {
	w := outWriter(c)
	after := lastEventSeq(client, convID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var printed string
	stream := func() error {
		return client.StreamConversationEvents(ctx,
			request.BuildConversationEventsPath(convID, after, 0, true),
			func(ev assistant.Event) error {
				switch ev.Type {
				case "user":
					printed = ""
				case "assistant":
					text := ev.Text
					switch {
					case ev.Delta:
						fmt.Fprint(w, text)
						printed += text
					case strings.HasPrefix(text, printed):
						fmt.Fprint(w, text[len(printed):])
						printed = text
					default:
						if printed != "" {
							fmt.Fprint(w, "\n")
						}
						fmt.Fprint(w, text)
						printed = text
					}
				case "tool":
					if ev.Tool == nil {
						return nil
					}
					switch ev.Tool.Status {
					case "started":
						fmt.Fprintf(w, "\n→ %s\n", assistantToolLabel(ev.Tool))
					case "failed":
						fmt.Fprintln(w, "  failed")
					}
				case "permission":
					if ev.Perm != nil {
						if err := answerAssistantPermission(c, client, convID, ev.Perm, in); err != nil {
							return err
						}
					}
				case "error":
					if ev.Error != nil {
						fmt.Fprintf(w, "\nerror: %s\n", ev.Error.Message)
					}
					cancel()
				case "result", "exit":
					cancel()
				}
				return nil
			})
	}
	err := stream()
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	if err != nil {
		return err
	}
	if printed != "" {
		fmt.Fprintln(w)
	}
	return nil
}

// lastEventSeq pages the stored transcript to the seq a follow resumes from.
func lastEventSeq(client *APIClient, convID string) int64 {
	after := int64(0)
	for page := 0; page < 200; page++ {
		var body struct {
			Events    []assistant.Event `json:"events"`
			NextAfter *int64            `json:"next_after"`
		}
		path := request.BuildConversationEventsPath(convID, after, 500, false)
		if err := client.GetJSON(path, &body); err != nil {
			return after
		}
		if len(body.Events) > 0 {
			after = body.Events[len(body.Events)-1].Seq
		}
		if body.NextAfter == nil || *body.NextAfter <= after {
			return after
		}
		after = *body.NextAfter
	}
	return after
}

// assistantToolLabel renders one tool call as `trebi.<tool> <action>`.
func assistantToolLabel(t *assistant.Tool) string {
	short := strings.TrimPrefix(t.Name, "mcp__trebi__")
	var action string
	if t.Input != "" {
		var m map[string]any
		if json.Unmarshal([]byte(t.Input), &m) == nil {
			action, _ = m["action"].(string)
		}
	}
	switch {
	case action != "":
		return "trebi." + short + " " + action
	case short != t.Name:
		return "trebi." + short
	default:
		return t.Name
	}
}

// answerAssistantPermission prompts once per open permission and posts the
// decision. An empty line or an unknown answer denies.
func answerAssistantPermission(c *cli.Command, client *APIClient, convID string, p *assistant.Permission, in *bufio.Reader) error {
	w := outWriter(c)
	fmt.Fprintf(w, "\npermission needed: %s\n", p.Tool)
	if p.Question != "" {
		fmt.Fprintln(w, p.Question)
	}
	fmt.Fprint(w, "[y]es once / [a]lways / [n]o: ")
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				line = "n\n"
			} else {
				return err
			}
		}
		decision := ""
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			decision = "allow_once"
		case "a", "always":
			decision = "allow_session"
		default:
			decision = "deny"
		}
		return client.DoHTTP(request.BuildConversationPermission(convID, p.ID, decision, ""), nil)
	}
}

func runAssistantSessions(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	path := request.BuildConversationsListPath(wsID, "", "", "", 200)
	var body struct {
		Conversations []map[string]any `json:"conversations"`
	}
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	sessions := make([]map[string]any, 0, len(body.Conversations))
	for _, conv := range body.Conversations {
		if s, ok := conv["session"].(map[string]any); ok && s != nil {
			sessions = append(sessions, conv)
		}
	}
	if c.Bool("json") {
		return encodeJSON(c, map[string]any{"conversations": sessions})
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Status", "Harness", "Turn", "Last message", "Updated"})
	for _, conv := range sessions {
		session := conv["session"].(map[string]any)
		tbl.AppendRow(table.Row{
			mapStr(conv, "id"),
			colorStatus(w, mapStr(session, "status")),
			dash(mapStr(session, "harness")),
			mapAnyInt(session["turn"]),
			dash(mapStr(session, "last_message")),
			dash(mapStr(session, "updated_at")),
		})
	}
	renderTable(tbl)
	return nil
}

func mapAnyInt(v any) string {
	n, ok := v.(float64)
	if !ok {
		return "0"
	}
	return fmt.Sprintf("%d", int(n))
}

func runAssistantClose(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi assistant close <conversation id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildConversationClose(id), nil)
}

func runAssistantInterrupt(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi assistant interrupt <conversation id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildConversationInterrupt(id), nil)
}

func runAssistantPreview(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return err
	}
	var body struct {
		Instructions string `json:"instructions"`
	}
	if err := client.GetJSON(request.BuildAssistantPreviewPath(wsID), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintln(outWriter(c), body.Instructions)
	return nil
}
