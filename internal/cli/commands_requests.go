package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v3"

	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/requests"
)

func requestCommand() *cli.Command {
	return &cli.Command{
		Name:  "request",
		Usage: "ask a person for a decision from a job run, and answer requests",
		Commands: []*cli.Command{
			requestCreateCommand(),
			requestListCommand(),
			requestShowCommand(),
			requestAnswerCommand(),
			requestDismissCommand(),
		},
	}
}

func requestListCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list requests, newest first",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "status", Usage: "pending | answered | expired | dismissed | cancelled"},
			&cli.StringFlag{Name: "job", Usage: "job name"},
			&cli.StringFlag{Name: "run", Usage: "run id"},
			&cli.StringFlag{Name: "workspace", Usage: "workspace id"},
			&cli.IntFlag{Name: "limit", Usage: "page size"},
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			var body struct {
				Requests   []requests.Request `json:"requests"`
				NextCursor string             `json:"next_cursor,omitempty"`
			}
			path := request.BuildRequestListPath(c.String("status"), c.String("run"), c.String("job"), c.String("workspace"), "", c.Int("limit"))
			if err := client.GetJSON(path, &body); err != nil {
				return err
			}
			if c.Bool("json") {
				return encodeJSON(c, body)
			}
			tbl := newTable(outWriter(c), []string{"ID", "Job", "Forms", "Status", "Title", "Created"})
			for _, r := range body.Requests {
				tbl.AppendRow(table.Row{r.ID, r.JobName, len(r.Forms), r.Status, r.Title, r.CreatedAt.Local().Format("2006-01-02 15:04")})
			}
			renderTable(tbl)
			return nil
		},
	}
}

func requestShowCommand() *cli.Command {
	return &cli.Command{
		Name:      "show",
		Aliases:   []string{"get"},
		ArgsUsage: "<id>",
		Usage:     "show one request, its forms, and its answers",
		Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
		Action: func(_ context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return fmt.Errorf("usage: trebi request show <id>")
			}
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			var r requests.Request
			if err := client.DoHTTP(request.BuildRequestGet(c.Args().First()), &r); err != nil {
				return err
			}
			if c.Bool("json") {
				return encodeJSON(c, r)
			}
			printRequest(c, r)
			return nil
		},
	}
}

func requestDismissCommand() *cli.Command {
	return &cli.Command{
		Name:      "dismiss",
		ArgsUsage: "<id>",
		Usage:     "close a request with no answer; the job does not continue",
		Action: func(_ context.Context, c *cli.Command) error {
			if os.Getenv("TREBI_RUN_TOKEN") != "" {
				return errors.New("a job run cannot dismiss a request; a person does that")
			}
			if c.Args().Len() != 1 {
				return fmt.Errorf("usage: trebi request dismiss <id>")
			}
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			var r requests.Request
			if err := client.DoHTTP(request.BuildRequestDismiss(c.Args().First()), &r); err != nil {
				return err
			}
			fmt.Fprintf(outWriter(c), "dismissed %s\n", r.ID)
			return nil
		},
	}
}

func requestCreateCommand() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "ask a person (from a job run); the calls of one turn join one request; end your turn after it",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "forms-file", Usage: "JSON array of forms; --title and --summary then describe the request"},
			&cli.StringFlag{Name: "type", Usage: "the one form: confirm | review | choose | form"},
			&cli.StringFlag{Name: "title", Usage: "what the person decides"},
			&cli.StringFlag{Name: "summary", Usage: "markdown context"},
			&cli.StringFlag{Name: "target", Usage: "what the action touches"},
			&cli.StringFlag{Name: "draft", Usage: "review draft text"},
			&cli.StringFlag{Name: "draft-file", Usage: "read the review draft from this file"},
			&cli.StringFlag{Name: "draft-format", Usage: "text | markdown (default: markdown for .md files, else text)"},
			&cli.StringSliceFlag{Name: "option", Usage: "choose option id=label (repeat)"},
			&cli.BoolFlag{Name: "multi", Usage: "choose: allow more than one option"},
			&cli.StringFlag{Name: "schema-file", Usage: "form: flat JSON Schema file"},
			&cli.StringSliceFlag{Name: "allow", Usage: "narrow the allowed decisions (repeat)"},
			&cli.StringFlag{Name: "report", Usage: ".html or .md report inside the working dir"},
			&cli.StringFlag{Name: "expires-in", Usage: "for example 24h (default requests.expire_after)"},
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			body, err := requestCreateBody(c)
			if err != nil {
				return err
			}
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			var out struct {
				Request requests.Request `json:"request"`
				Next    string           `json:"next,omitempty"`
				Message string           `json:"message,omitempty"`
			}
			if err := client.DoHTTP(request.BuildRequestCreate(body), &out); err != nil {
				return err
			}
			if c.Bool("json") {
				return encodeJSON(c, out)
			}
			fmt.Fprintf(outWriter(c), "%s (%d forms)\n", out.Request.ID, len(out.Request.Forms))
			if out.Message != "" {
				fmt.Fprintln(outWriter(c), out.Message)
			}
			return nil
		},
	}
}

func requestCreateBody(c *cli.Command) (request.RequestCreateBody, error) {
	b := request.RequestCreateBody{ExpiresIn: c.String("expires-in")}
	if f := c.String("forms-file"); f != "" {
		for _, name := range []string{"type", "target", "draft", "draft-file", "draft-format", "option", "multi", "schema-file", "allow", "report"} {
			if c.IsSet(name) {
				return b, fmt.Errorf("--%s describes one form; put it in --forms-file", name)
			}
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return b, err
		}
		if err := json.Unmarshal(data, &b.Forms); err != nil {
			return b, fmt.Errorf("--forms-file %s: want a JSON array of forms: %w", f, err)
		}
		b.Title, b.Summary = c.String("title"), c.String("summary")
		return b, nil
	}
	if c.String("type") == "" || c.String("title") == "" {
		return b, errors.New("give --type and --title, or --forms-file")
	}
	f := request.RequestFormBody{
		Type: c.String("type"), Title: c.String("title"), Summary: c.String("summary"),
		Target: c.String("target"), Multi: c.Bool("multi"), Allow: c.StringSlice("allow"), Report: c.String("report"),
	}
	draft, err := requestDraft(c)
	if err != nil {
		return b, err
	}
	if draft != nil {
		f.Draft = draft
	}
	var opts []request.RequestOption
	for _, o := range c.StringSlice("option") {
		id, label, ok := strings.Cut(o, "=")
		if !ok || id == "" || label == "" {
			return b, fmt.Errorf("--option %q: want id=label", o)
		}
		opts = append(opts, request.RequestOption{ID: id, Label: label})
	}
	if len(opts) > 0 {
		f.Options = opts
	}
	if sf := c.String("schema-file"); sf != "" {
		data, err := os.ReadFile(sf)
		if err != nil {
			return b, err
		}
		if !json.Valid(data) {
			return b, fmt.Errorf("--schema-file %s: not valid JSON", sf)
		}
		f.Schema = data
	}
	b.Forms = []request.RequestFormBody{f}
	return b, nil
}

// requestDraft reads --draft or --draft-file. A .md file defaults to markdown.
func requestDraft(c *cli.Command) (*request.RequestDraft, error) {
	text, file := c.String("draft"), c.String("draft-file")
	if text != "" && file != "" {
		return nil, errors.New("use --draft or --draft-file, not both")
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		text = string(data)
	}
	if text == "" && file == "" {
		return nil, nil
	}
	format := c.String("draft-format")
	if format == "" {
		format = "text"
		if strings.HasSuffix(strings.ToLower(file), ".md") {
			format = "markdown"
		}
	}
	return &request.RequestDraft{Format: format, Text: text}, nil
}

func requestAnswerCommand() *cli.Command {
	return &cli.Command{
		Name:      "answer",
		ArgsUsage: "<id> approve|reject|edit|respond|choose|submit",
		Usage:     "answer a request as a person; a request with more forms needs --answers-file",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "answers-file", Usage: "JSON array of form answers: [{idx, decision, ...}]"},
			&cli.StringFlag{Name: "text", Usage: "edit: the changed draft"},
			&cli.StringFlag{Name: "text-file", Usage: "edit: read the changed draft from this file"},
			&cli.StringFlag{Name: "message", Usage: "respond: the reply to the job"},
			&cli.StringSliceFlag{Name: "choice", Usage: "choose: option id (repeat)"},
			&cli.StringFlag{Name: "values-file", Usage: "submit: JSON object of form values"},
			&cli.StringFlag{Name: "note", Usage: "a note to the job"},
			&cli.StringFlag{Name: "if-match", Usage: "the request revision"},
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			if os.Getenv("TREBI_RUN_TOKEN") != "" {
				return errors.New("a job run cannot answer a request; a person does that")
			}
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			b, err := requestAnswerBody(c, client)
			if err != nil {
				return err
			}
			var r requests.Request
			if err := client.DoHTTP(request.BuildRequestAnswer(c.Args().First(), b, c.String("if-match")), &r); err != nil {
				return err
			}
			if c.Bool("json") {
				return encodeJSON(c, r)
			}
			fmt.Fprintf(outWriter(c), "answered %s: %d forms\n", r.ID, len(b.Forms))
			return nil
		},
	}
}

// requestAnswerBody reads --answers-file, or builds the answer of a request
// with one form from the decision and the flags.
func requestAnswerBody(c *cli.Command, client *APIClient) (request.RequestAnswerBody, error) {
	b := request.RequestAnswerBody{Via: requests.ViaCLI}
	if f := c.String("answers-file"); f != "" {
		if c.Args().Len() != 1 {
			return b, errors.New("usage: trebi request answer <id> --answers-file <path>")
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return b, err
		}
		if err := json.Unmarshal(data, &b.Forms); err != nil {
			return b, fmt.Errorf("--answers-file %s: want a JSON array of form answers: %w", f, err)
		}
		return b, nil
	}
	if c.Args().Len() != 2 {
		return b, errors.New("usage: trebi request answer <id> approve|reject|edit|respond|choose|submit")
	}
	var r requests.Request
	if err := client.DoHTTP(request.BuildRequestGet(c.Args().First()), &r); err != nil {
		return b, err
	}
	if len(r.Forms) != 1 {
		return b, fmt.Errorf("request %s has %d forms; answer them all with --answers-file", r.ID, len(r.Forms))
	}
	a := request.RequestFormAnswer{
		Idx: r.Forms[0].Idx, Decision: c.Args().Get(1), Text: c.String("text"), Message: c.String("message"),
		Choices: c.StringSlice("choice"), Note: c.String("note"),
	}
	if f := c.String("text-file"); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			return b, err
		}
		a.Text = string(data)
	}
	if f := c.String("values-file"); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			return b, err
		}
		if !json.Valid(data) {
			return b, fmt.Errorf("--values-file %s: not valid JSON", f)
		}
		a.Values = data
	}
	b.Forms = []request.RequestFormAnswer{a}
	return b, nil
}

func printRequest(c *cli.Command, r requests.Request) {
	w := outWriter(c)
	fmt.Fprintf(w, "%s  %s (%d forms)\n", r.ID, r.Title, len(r.Forms))
	fmt.Fprintf(w, "Job: %s   Run: %s   Turn: %d   Status: %s\n", r.JobName, r.RunID, r.Turn, r.Status)
	if r.ExpiresAt != nil && r.Open() {
		fmt.Fprintf(w, "Expires: %s\n", r.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}
	if r.Summary != "" {
		fmt.Fprintf(w, "\n%s\n", r.Summary)
	}
	for _, f := range r.Forms {
		printForm(w, r.ID, f)
	}
	if a := r.Answered; a != nil {
		fmt.Fprintf(w, "\nClosed by %s via %s at %s\n", a.By, a.Via, a.At.Local().Format("2006-01-02 15:04"))
	}
	if r.DeliveryState != requests.DeliveryNone {
		fmt.Fprintf(w, "Delivery: %s %s %s\n", r.DeliveryState, r.DeliveryPath, r.DeliveredRunID)
	}
}

func printForm(w io.Writer, id string, f requests.Form) {
	fmt.Fprintf(w, "\nForm %d: %s (%s)\n", f.Idx, f.Title, f.Type)
	if f.Target != "" {
		fmt.Fprintf(w, "Target: %s\n", f.Target)
	}
	if f.Summary != "" {
		fmt.Fprintf(w, "%s\n", f.Summary)
	}
	if d := f.Payload.Draft; d != nil {
		fmt.Fprintf(w, "Draft (%s):\n%s\n", d.Format, d.Text)
	}
	for _, o := range f.Payload.Options {
		fmt.Fprintf(w, "  - %s: %s %s\n", o.ID, o.Label, o.Detail)
	}
	if len(f.Payload.Schema) > 0 {
		fmt.Fprintf(w, "Form schema: %s\n", f.Payload.Schema)
	}
	if f.ReportKind != "" {
		fmt.Fprintf(w, "Report: %s (GET /v1/requests/%s/forms/%d/report)\n", f.ReportKind, id, f.Idx)
	}
	fmt.Fprintf(w, "Allowed: %v\n", f.Allow)
	if a := f.Answer; a != nil {
		fmt.Fprintf(w, "Answer: %s\n", a.Decision)
		for _, kv := range []struct{ k, v string }{{"Text", a.Text}, {"Message", a.Message}, {"Note", a.Note}} {
			if kv.v != "" {
				fmt.Fprintf(w, "%s: %s\n", kv.k, kv.v)
			}
		}
		if len(a.Choices) > 0 {
			fmt.Fprintf(w, "Choices: %s\n", strings.Join(a.Choices, ", "))
		}
		if len(a.Values) > 0 {
			fmt.Fprintf(w, "Values: %s\n", a.Values)
		}
	}
}
