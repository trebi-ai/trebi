package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v3"

	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/requests"
)

// Exit codes of trebi request create --wait.
const (
	requestExitApproved = 0
	requestExitDeclined = 1
	requestExitClosed   = 2
	requestExitTimeout  = 3
)

// requestPollStep stays under the 30s client timeout.
const requestPollStep = 25 * time.Second

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
			tbl := newTable(outWriter(c), []string{"ID", "Job", "Type", "Status", "Title", "Created"})
			for _, r := range body.Requests {
				tbl.AppendRow(table.Row{r.ID, r.JobName, r.Type, r.Status, r.Title, r.CreatedAt.Local().Format("2006-01-02 15:04")})
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
		Usage:     "show one request and its answer",
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
			if err := client.DoHTTP(request.BuildRequestGet(c.Args().First(), ""), &r); err != nil {
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
		Usage: "ask a person (from a job run); end your turn after it",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "type", Usage: "confirm | review | choose | form", Required: true},
			&cli.StringFlag{Name: "title", Usage: "what the person decides", Required: true},
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
			&cli.DurationFlag{Name: "wait", Usage: "poll mode: block this long and print the answer JSON"},
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
			wait := c.Duration("wait")
			if wait <= 0 {
				if c.Bool("json") {
					return encodeJSON(c, out)
				}
				fmt.Fprintln(outWriter(c), out.Request.ID)
				if out.Message != "" {
					fmt.Fprintln(outWriter(c), out.Message)
				}
				return nil
			}
			r, err := waitForRequest(client, out.Request.ID, wait)
			if err != nil {
				return err
			}
			if err := encodeJSON(c, r); err != nil {
				return err
			}
			if code := requestExitCode(r); code != requestExitApproved {
				return cli.Exit("", code)
			}
			return nil
		},
	}
}

func requestCreateBody(c *cli.Command) (request.RequestCreateBody, error) {
	b := request.RequestCreateBody{
		Type: c.String("type"), Title: c.String("title"), Summary: c.String("summary"),
		Target: c.String("target"), Multi: c.Bool("multi"), Allow: c.StringSlice("allow"),
		Report: c.String("report"), ExpiresIn: c.String("expires-in"),
	}
	if c.Duration("wait") > 0 {
		b.Delivery = string(requests.DeliveryPoll)
	}
	draft, err := requestDraft(c)
	if err != nil {
		return b, err
	}
	if draft != nil {
		b.Draft = draft
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
		b.Options = opts
	}
	if f := c.String("schema-file"); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			return b, err
		}
		if !json.Valid(data) {
			return b, fmt.Errorf("--schema-file %s: not valid JSON", f)
		}
		b.Schema = data
	}
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

// waitForRequest long-polls until the request closes or wait ends.
func waitForRequest(client *APIClient, id string, wait time.Duration) (requests.Request, error) {
	deadline := time.Now().Add(wait)
	for {
		step := min(time.Until(deadline), requestPollStep)
		if step < time.Second {
			step = time.Second
		}
		var r requests.Request
		if err := client.DoHTTP(request.BuildRequestGet(id, step.String()), &r); err != nil {
			return r, err
		}
		if !r.Open() || !time.Now().Before(deadline) {
			return r, nil
		}
	}
}

// requestExitCode maps a request to the --wait exit code.
func requestExitCode(r requests.Request) int {
	switch {
	case r.Open():
		return requestExitTimeout
	case r.Status != requests.StatusAnswered || r.Answer == nil:
		return requestExitClosed
	}
	switch r.Answer.Decision {
	case requests.DecisionApprove, requests.DecisionEdit, requests.DecisionChoose, requests.DecisionSubmit:
		return requestExitApproved
	}
	return requestExitDeclined
}

func requestAnswerCommand() *cli.Command {
	return &cli.Command{
		Name:      "answer",
		ArgsUsage: "<id> approve|reject|edit|respond|choose|submit",
		Usage:     "answer a request as a person",
		Flags: []cli.Flag{
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
			if c.Args().Len() != 2 {
				return fmt.Errorf("usage: trebi request answer <id> approve|reject|edit|respond|choose|submit")
			}
			b := request.RequestAnswerBody{
				Decision: c.Args().Get(1), Text: c.String("text"), Message: c.String("message"),
				Choices: c.StringSlice("choice"), Note: c.String("note"), Via: requests.ViaCLI,
			}
			if f := c.String("text-file"); f != "" {
				data, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				b.Text = string(data)
			}
			if f := c.String("values-file"); f != "" {
				data, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				if !json.Valid(data) {
					return fmt.Errorf("--values-file %s: not valid JSON", f)
				}
				b.Values = data
			}
			client, err := clientFor(c)
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
			fmt.Fprintf(outWriter(c), "answered %s: %s\n", r.ID, b.Decision)
			return nil
		},
	}
}

func printRequest(c *cli.Command, r requests.Request) {
	w := outWriter(c)
	fmt.Fprintf(w, "%s  %s (%s)\n", r.ID, r.Title, r.Type)
	fmt.Fprintf(w, "Job: %s   Run: %s   Status: %s\n", r.JobName, r.RunID, r.Status)
	if r.Target != "" {
		fmt.Fprintf(w, "Target: %s\n", r.Target)
	}
	if r.ExpiresAt != nil && r.Open() {
		fmt.Fprintf(w, "Expires: %s\n", r.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}
	if r.Summary != "" {
		fmt.Fprintf(w, "\n%s\n", r.Summary)
	}
	if d := r.Payload.Draft; d != nil {
		fmt.Fprintf(w, "\nDraft (%s):\n%s\n", d.Format, d.Text)
	}
	for _, o := range r.Payload.Options {
		fmt.Fprintf(w, "  - %s: %s %s\n", o.ID, o.Label, o.Detail)
	}
	if len(r.Payload.Schema) > 0 {
		fmt.Fprintf(w, "\nForm schema: %s\n", r.Payload.Schema)
	}
	if r.ReportKind != "" {
		fmt.Fprintf(w, "Report: %s (GET /v1/requests/%s/report)\n", r.ReportKind, r.ID)
	}
	fmt.Fprintf(w, "Allowed: %v\n", r.Allow)
	if r.Answer != nil {
		printAnswer(w, *r.Answer)
	}
	if r.DeliveryState != requests.DeliveryNone && r.Delivery == requests.DeliveryResume {
		fmt.Fprintf(w, "Delivery: %s %s %s\n", r.DeliveryState, r.DeliveryPath, r.DeliveredRunID)
	}
}

func printAnswer(w io.Writer, a requests.Answer) {
	fmt.Fprintf(w, "\nAnswer: %s by %s via %s\n", a.Decision, a.By, a.Via)
	for _, f := range []struct{ k, v string }{{"Text", a.Text}, {"Message", a.Message}, {"Note", a.Note}} {
		if f.v != "" {
			fmt.Fprintf(w, "%s: %s\n", f.k, f.v)
		}
	}
	if len(a.Choices) > 0 {
		fmt.Fprintf(w, "Choices: %s\n", strings.Join(a.Choices, ", "))
	}
	if len(a.Values) > 0 {
		fmt.Fprintf(w, "Values: %s\n", a.Values)
	}
}
