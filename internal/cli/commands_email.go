package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"

	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func emailCommand() *cli.Command {
	return &cli.Command{
		Name:  "email",
		Usage: "email channel status, log, and inbound poll",
		Commands: []*cli.Command{
			{
				Name:   "status",
				Usage:  "email channel status (works with the block absent)",
				Flags:  []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: runEmailStatus,
			},
			{
				Name:   "test",
				Usage:  "send a fixed test message to the admins",
				Flags:  []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: runEmailTest,
			},
			{
				Name:  "send",
				Usage: "send one message from the workspace address (admins or allowlist only)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Required: true, Usage: "workspace id or name"},
					&cli.StringFlag{Name: "agent", Usage: "send from the agent subaddress"},
					&cli.StringSliceFlag{Name: "to", Required: true, Usage: "recipient address (repeatable)"},
					&cli.StringFlag{Name: "subject", Required: true, Usage: "subject line"},
					&cli.StringFlag{Name: "text", Required: true, Usage: "message body as plain text"},
					&cli.StringFlag{Name: "html", Usage: "message body as HTML"},
					&cli.StringFlag{Name: "html-file", Usage: "read the HTML body from a local file"},
					&cli.StringSliceFlag{Name: "attach", Usage: "attach a local file (repeatable)"},
					&cli.StringSliceFlag{Name: "workspace-file", Usage: "attach a workspace file by its workspace path (repeatable)"},
					&cli.StringFlag{Name: "in-reply-to", Usage: "message id this mail answers"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runEmailSend,
			},
			{
				Name:  "messages",
				Usage: "list the envelope log (newest first)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "direction", Usage: "in or out"},
					&cli.StringFlag{Name: "workspace", Usage: "filter by workspace id"},
					&cli.StringFlag{Name: "thread", Usage: "filter by thread root"},
					&cli.StringFlag{Name: "cursor", Usage: "page cursor from the last page"},
					limitFlag(),
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runEmailMessages,
			},
			{
				Name:      "show",
				Usage:     "show one envelope with verdicts and links",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runEmailShow,
			},
			{
				Name:      "attachment",
				Usage:     "fetch one attachment's bytes through the Worker",
				ArgsUsage: "<message-id> <attachment-id>",
				Flags:     []cli.Flag{&cli.StringFlag{Name: "out", Required: true, Usage: "output file path"}},
				Action:    runEmailAttachment,
			},
			{
				Name:   "poll",
				Usage:  "pull the Worker spool now, outside the tick",
				Flags:  []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: runEmailPoll,
			},
		},
	}
}

func licenseCommand() *cli.Command {
	return &cli.Command{
		Name:  "license",
		Usage: "license state and install",
		Commands: []*cli.Command{
			{
				Name:   "show",
				Usage:  "show license state",
				Flags:  []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: runLicenseShow,
			},
			{
				Name:      "install",
				Usage:     "install a license key",
				ArgsUsage: "<key>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runLicenseInstall,
			},
		},
	}
}

func runEmailStatus(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildEmailStatusPath(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "Configured:  %s\n", mapStr(body, "configured"))
	fmt.Fprintf(w, "Worker:      %s\n", dash(mapStr(body, "worker")))
	fmt.Fprintf(w, "Domain:      %s\n", dash(mapStr(body, "domain")))
	if ops, ok := body["admins"].([]any); ok && len(ops) > 0 {
		parts := make([]string, len(ops))
		for i, o := range ops {
			parts[i] = fmt.Sprint(o)
		}
		fmt.Fprintf(w, "Admins:      %s\n", strings.Join(parts, ", "))
	}
	if ms := mapStr(body, "poll_interval_ms"); ms != "" {
		fmt.Fprintf(w, "Poll:        %sms\n", ms)
	}
	fmt.Fprintf(w, "DMARC:       %s\n", dmarcPolicy(body))
	fmt.Fprintf(w, "Entitlement: %s\n", mapStr(body, "entitlement"))
	fmt.Fprintf(w, "Addresses:   %s\n", mapStr(body, "addresses"))
	if h, ok := body["worker_health"].(map[string]any); ok {
		fmt.Fprintf(w, "Spool:       %s deep, oldest %sms\n",
			mapStr(h, "spool_depth"), mapStr(h, "spool_oldest_age_ms"))
	} else if e := mapStr(body, "worker_error"); e != "" {
		fmt.Fprintf(w, "Worker err:  %s\n", e)
	}
	if lp := mapStr(body, "last_poll"); lp != "" {
		fmt.Fprintf(w, "Last poll:   %s (failures %s)\n", lp, mapStr(body, "failures"))
	}
	return nil
}

// dmarcPolicy renders require_dmarc as a word; absent means not configured.
func dmarcPolicy(body map[string]any) string {
	if body["require_dmarc"] == nil {
		return "-"
	}
	if req, ok := body["require_dmarc"].(bool); ok && req {
		return "required"
	}
	return "off"
}

func runEmailTest(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildEmailTest(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "sent %s\n", mapStr(body, "message_id"))
	return nil
}

// runEmailSend sends one explicit message (plan I.3). Recipients must be
// admins or on the workspace allowlist; the API answers 400 otherwise.
func runEmailSend(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	spec := request.EmailSendSpec{
		Workspace: c.String("workspace"), Agent: c.String("agent"), To: c.StringSlice("to"),
		Subject: c.String("subject"), Text: c.String("text"), HTML: c.String("html"),
		InReplyTo: c.String("in-reply-to"),
	}
	if f := c.String("html-file"); f != "" {
		if spec.HTML != "" {
			return errors.New("use --html or --html-file, not both")
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read html file: %w", err)
		}
		spec.HTML = string(raw)
	}
	for _, f := range c.StringSlice("attach") {
		raw, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read attachment: %w", err)
		}
		spec.Attachments = append(spec.Attachments, request.EmailFile{
			Name: filepath.Base(f), Content: base64.StdEncoding.EncodeToString(raw),
		})
	}
	for _, p := range c.StringSlice("workspace-file") {
		spec.Attachments = append(spec.Attachments, request.EmailFile{Path: p})
	}
	var body map[string]any
	req := spec.Build()
	if err := client.DoHTTP(req, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "sent %s\n", mapStr(body, "message_id"))
	return nil
}

func runEmailMessages(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Messages   []map[string]any `json:"messages"`
		NextCursor string           `json:"next_cursor"`
	}
	path := request.BuildEmailMessagesPath(c.String("direction"), c.String("workspace"),
		c.String("thread"), c.String("cursor"), c.Int("limit"))
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"ID", "Dir", "From/To", "Subject", "Disposition", "Created"})
	for _, m := range body.Messages {
		party := mapStr(m, "sender")
		if party == "" {
			party = mapStr(m, "recipients")
		}
		tbl.AppendRow(table.Row{
			mapStr(m, "id"), mapStr(m, "direction"), party,
			mapStr(m, "subject"), mapStr(m, "disposition"), mapStr(m, "created_at"),
		})
	}
	renderTable(tbl)
	if body.NextCursor != "" {
		fmt.Fprintf(w, "next cursor: %s\n", body.NextCursor)
	}
	return nil
}

func runEmailShow(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi email show <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildEmailMessagePath(id), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "ID:       %s\n", mapStr(body, "id"))
	fmt.Fprintf(w, "Dir:      %s\n", mapStr(body, "direction"))
	if s := mapStr(body, "sender"); s != "" {
		fmt.Fprintf(w, "From:     %s\n", s)
	}
	if s := mapStr(body, "recipients"); s != "" {
		fmt.Fprintf(w, "To:       %s\n", s)
	}
	fmt.Fprintf(w, "Subject:  %s\n", mapStr(body, "subject"))
	if s := mapStr(body, "agent"); s != "" {
		fmt.Fprintf(w, "Agent:    %s\n", s)
	}
	if s := mapStr(body, "dmarc"); s != "" {
		fmt.Fprintf(w, "DMARC:    %s\n", s)
	}
	fmt.Fprintf(w, "Verdict:  %s\n", mapStr(body, "disposition"))
	for _, link := range [][2]string{{"run_id", "Run"}, {"conversation_id", "Conversation"}, {"inbox_id", "Inbox item"}} {
		if s := mapStr(body, link[0]); s != "" {
			fmt.Fprintf(w, "%s: %s\n", link[1], s)
		}
	}
	if atts, ok := body["attachments"].([]any); ok && len(atts) > 0 {
		fmt.Fprintf(w, "Attachments (%d; fetch with trebi email attachment):\n", len(atts))
		for _, a := range atts {
			if am, ok := a.(map[string]any); ok {
				fmt.Fprintf(w, "  %s  %s  %s bytes\n",
					mapStr(am, "id"), dash(mapStr(am, "name")), mapStr(am, "size"))
			}
		}
	}
	fmt.Fprintf(w, "Created:  %s\n", mapStr(body, "created_at"))
	return nil
}

// runEmailAttachment writes one attachment to a local file (plan A.11: the
// fetch is explicit; nothing auto-fetches). The Worker logs every read.
func runEmailAttachment(_ context.Context, c *cli.Command) error {
	msgID, attID := c.Args().Get(0), c.Args().Get(1)
	if msgID == "" || attID == "" {
		return fmt.Errorf("usage: trebi email attachment <message-id> <attachment-id> --out <path>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	raw, err := client.GetRaw(request.BuildEmailAttachmentPath(msgID, attID))
	if err != nil {
		return err
	}
	out := c.String("out")
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	fmt.Fprintf(outWriter(c), "wrote %s (%d bytes)\n", out, len(raw))
	return nil
}

func runEmailPoll(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildEmailPoll(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "pulled %s\n", mapStr(body, "pulled"))
	return nil
}

func runLicenseShow(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildSystemLicenseGetPath(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	if mapStr(body, "supported") == "false" {
		fmt.Fprintln(outWriter(c), "license: unsupported")
		return nil
	}
	w := outWriter(c)
	fmt.Fprintf(w, "Supported: %s\n", mapStr(body, "supported"))
	fmt.Fprintf(w, "Source:    %s\n", mapStr(body, "source"))
	if plan := mapStr(body, "plan"); plan != "" {
		fmt.Fprintf(w, "Plan:      %s\n", plan)
	}
	if sub := mapStr(body, "subject"); sub != "" {
		fmt.Fprintf(w, "Subject:   %s\n", sub)
	}
	if exp := mapStr(body, "expires_at"); exp != "" {
		fmt.Fprintf(w, "Expires:   %s\n", exp)
	}
	if feats, ok := body["features"].([]any); ok && len(feats) > 0 {
		parts := make([]string, len(feats))
		for i, f := range feats {
			parts[i] = fmt.Sprint(f)
		}
		fmt.Fprintf(w, "Features:  %s\n", strings.Join(parts, ", "))
	}
	return nil
}

func runLicenseInstall(_ context.Context, c *cli.Command) error {
	key := c.Args().Get(0)
	if key == "" {
		return fmt.Errorf("usage: trebi license install <key>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildSystemLicenseInstall(key), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "installed (source %s)\n", mapStr(body, "source"))
	return nil
}
