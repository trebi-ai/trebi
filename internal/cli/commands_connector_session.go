package cli

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"rsc.io/qr"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/conformance"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// connectorSessionCommands are the live side of a connector: login, rooms,
// and the workspace layer.
func connectorSessionCommands() []*cli.Command {
	cmds := []*cli.Command{
		{
			Name:      "login",
			Usage:     "log in to a connector account and follow the steps",
			ArgsUsage: "<connector>",
			Description: "A key is read with a hidden prompt, or from stdin when stdin is not a terminal.\n" +
				"Run login-methods to see the methods of a connector.",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "method", Usage: "id of the login method (default: the first available one)"},
				&cli.BoolFlag{Name: "no-browser", Usage: "print the login page address; do not open the browser"},
			},
			Action: runConnectorLogin,
		},
		connectorOAuthCommand(),
		{
			Name:      "login-methods",
			Usage:     "list the ways to log in to a connector account",
			ArgsUsage: "<connector>",
			Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
			Action:    runConnectorLoginMethods,
		},
		{
			Name:      "conformance",
			Usage:     "check a connector against its manifest and the trebi-connector/1 protocol",
			ArgsUsage: "[connector]",
			Description: "With a connector, the daemon checks the live connection and does not stop it.\n" +
				"With --manifest or --command, the check runs here with no daemon, in a sandbox with a clean env and temp folders.\n" +
				"The command exits 1 when a check fails. With --strict, a warn row also exits 1.\n" +
				"A message is sent only to the --room you name.",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "command", Usage: "adapter command to start here (default: events.command of --manifest)"},
				&cli.StringFlag{Name: "manifest", Usage: "trebi-connector.yaml, or its folder, that declares the features, events, and login"},
				&cli.StringFlag{Name: "room", Usage: "test room; with it, the check sends one message twice with one key"},
				&cli.StringSliceFlag{Name: "suite", Usage: "suites to run: manifest, install, inputs, actions, actions.cli, actions.mcp, events.lines, protocol, folder (default: each block of the manifest)"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				&cli.BoolFlag{Name: "strict", Usage: "a warn row also fails the command"},
			},
			Action: runConnectorConformance,
		},
		{
			Name:      "logout",
			Usage:     "log out of a connector account",
			ArgsUsage: "<connector>",
			Action:    runConnectorLogout,
		},
		{
			Name:      "rooms",
			Usage:     "list the rooms of a channel connector (chats, groups, mailboxes)",
			ArgsUsage: "<connector>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "query", Usage: "match on the room name"},
				&cli.StringFlag{Name: "kind", Usage: "keep rooms of this kind"},
				&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (next_cursor of the previous page)"},
				&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Usage: "max rows"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorRooms,
		},
		{
			Name:      "senders",
			Usage:     "list the recent senders of a connector, for a trigger allowlist",
			ArgsUsage: "<connector>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "query", Usage: "match on the sender id or name"},
				&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Usage: "max rows (default 50, at most 200)"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorSenders,
		},
		{
			Name:      "dependents",
			Usage:     "list the triggers and the room bindings that use a connector",
			ArgsUsage: "<connector>",
			Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
			Action:    runConnectorDependents,
		},
		{
			Name:      "threads",
			Usage:     "list the threads of one room",
			ArgsUsage: "<connector> <room>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (next_cursor of the previous page)"},
				&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Usage: "max rows"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorThreads,
		},
		{
			Name:      "bind",
			Usage:     "send the messages of a room to the assistant or an agent of a workspace",
			ArgsUsage: "<connector> <room>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Required: true, Usage: "workspace id or name"},
				&cli.StringFlag{Name: "operator", Value: "assistant", Usage: "assistant or an agent name"},
				&cli.StringFlag{Name: "thread", Usage: "bind one thread of the room only"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorBind,
		},
		connectorOverrideCommand(),
		connectorRoutingCommand(),
		connectorRouteTestCommand(),
	}
	return append(cmds, connectorWatchCommands()...)
}

// connectorWatchCommands manage the subscriptions of a connection.
func connectorWatchCommands() []*cli.Command {
	set := func() cli.Flag {
		return &cli.StringSliceFlag{Name: "set", Usage: "field value as name=value; repeat it for a field with more than one value"}
	}
	events := func() cli.Flag {
		return &cli.StringSliceFlag{Name: "events", Usage: "event types to keep, comma separated (default: all)"}
	}
	return []*cli.Command{
		{
			Name:      "watching",
			Usage:     "list the things that a connector watches and their state",
			ArgsUsage: "<connector>",
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "sync", Usage: "ask the connector for a new state first"},
				&cli.BoolFlag{Name: "reveal", Usage: "show secret values in the steps"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorWatching,
		},
		{
			Name:      "watch",
			Usage:     "watch a new thing, such as a repository or a project",
			ArgsUsage: "<connector>",
			Flags:     []cli.Flag{set(), events(), &cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
			Action:    runConnectorWatch,
		},
		{
			Name:      "watch-options",
			Usage:     "list the values that a watch field accepts",
			ArgsUsage: "<connector> <field>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "query", Usage: "match on the value or its label"},
				set(),
				&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (next_cursor of the previous page)"},
				&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Usage: "max rows"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorWatchOptions,
		},
		{
			Name:      "watch-set",
			Usage:     "change the fields or the events of one watch",
			ArgsUsage: "<connector> <id>",
			Flags:     []cli.Flag{set(), events(), &cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
			Action:    runConnectorWatchSet,
		},
		{
			Name:      "unwatch",
			Usage:     "stop one watch",
			ArgsUsage: "<connector> <id>",
			Action:    runConnectorUnwatch,
		},
		{
			Name:      "watch-finish",
			Usage:     "send the values that a watch asks for to finish its setup",
			ArgsUsage: "<connector> <id>",
			Flags:     []cli.Flag{set(), &cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
			Action:    runConnectorWatchFinish,
		},
	}
}

func connectorOverrideCommand() *cli.Command {
	ws := func() cli.Flag {
		return &cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Required: true, Usage: "workspace id or name"}
	}
	return &cli.Command{
		Name:  "override",
		Usage: "show or change the connector layer of one workspace: turn a connection off, or patch its actions and skill",
		Commands: []*cli.Command{
			{
				Name:      "show",
				Usage:     "show the workspace layer of a connection",
				ArgsUsage: "<connector>",
				Flags:     []cli.Flag{ws()},
				Action:    runConnectorOverrideShow,
			},
			{
				Name:      "set",
				Usage:     "change the workspace layer of a connection (flags overlay the stored layer)",
				ArgsUsage: "<connector>",
				Flags: []cli.Flag{
					ws(),
					&cli.StringFlag{Name: "patch-file", Usage: "YAML or JSON merge patch on the actions and skill keys of the manifest; a file that holds null removes it"},
					&cli.BoolFlag{Name: "disabled", Usage: "turn the connector off in this workspace (--disabled=false turns it on)"},
				},
				Action: runConnectorOverrideSet,
			},
			{
				Name:      "delete",
				Aliases:   []string{"remove"},
				Usage:     "remove the workspace layer; the workspace uses the connection as it is again",
				ArgsUsage: "<connector>",
				Flags:     []cli.Flag{ws()},
				Action:    runConnectorOverrideDelete,
			},
		},
	}
}

func connectorOAuthCommand() *cli.Command {
	return &cli.Command{
		Name:  "oauth",
		Usage: "show or change the OAuth login of an online MCP server",
		Commands: []*cli.Command{
			{
				Name:      "show",
				Usage:     "show the OAuth settings and the login state",
				ArgsUsage: "<connector>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runConnectorOAuthShow,
			},
			{
				Name:      "set",
				Usage:     "change the OAuth settings (a flag that you do not give stays)",
				ArgsUsage: "<connector>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "mode", Usage: "proxy (runs call the server through Trebi) or header (the token goes in the run config)"},
					&cli.StringFlag{Name: "return", Usage: "the page that the browser comes back to: auto, local, or cloud"},
					&cli.StringFlag{Name: "registration", Usage: "how Trebi gets a client: auto, cimd, dcr, or own_app"},
					&cli.StringFlag{Name: "client-id", Usage: "client id of your own app; an empty value removes the app"},
					&cli.StringFlag{Name: "client-secret-name", Usage: "stored secret that holds the secret of your own app"},
					&cli.BoolFlag{Name: "client-secret-stdin", Usage: "read the secret of your own app from stdin"},
					&cli.StringSliceFlag{Name: "scope", Usage: "access to ask for at the next login; repeat it"},
				},
				Action: runConnectorOAuthSet,
			},
		},
	}
}

func runConnectorOAuthShow(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorOAuthGet(ref), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	set, _ := body["settings"].(map[string]any)
	st, _ := body["status"].(map[string]any)
	w := outWriter(c)
	fmt.Fprintf(w, "logged in:    %v\n", st["logged_in"])
	fmt.Fprintf(w, "server:       %s\n", dash(mapStr(st, "server")))
	fmt.Fprintf(w, "client:       %s %s\n", dash(mapStr(st, "client_kind")), mapStr(st, "client_id"))
	fmt.Fprintf(w, "expires:      %s\n", dash(mapStr(st, "expires_at")))
	fmt.Fprintf(w, "mode:         %s\n", cmp.Or(mapStr(set, "mode"), connectors.OAuthProxy))
	fmt.Fprintf(w, "return:       %s\n", cmp.Or(mapStr(set, "return"), connectors.ReturnAuto))
	fmt.Fprintf(w, "registration: %s\n", dash(mapStr(set, "registration")))
	return nil
}

func runConnectorOAuthSet(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	set := request.ConnectorOAuthSet{Ref: ref, ClientSecretName: c.String("client-secret-name")}
	for flag, dst := range map[string]**string{"mode": &set.Mode, "return": &set.Return, "registration": &set.Registration, "client-id": &set.ClientID} {
		if c.IsSet(flag) {
			v := c.String(flag)
			*dst = &v
		}
	}
	if c.IsSet("scope") {
		set.Scopes = c.StringSlice("scope")
	}
	if c.Bool("client-secret-stdin") {
		var in io.Reader = os.Stdin
		if c.Root().Reader != nil {
			in = c.Root().Reader
		}
		b, err := io.ReadAll(in)
		if err != nil {
			return fmt.Errorf("read the secret: %w", err)
		}
		set.ClientSecret = strings.TrimSpace(string(b))
	}
	if err := client.DoHTTP(set.Build(), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s: OAuth settings saved\n", ref.Ref)
	return nil
}

// openBrowser opens a page in the default browser of this computer.
func openBrowser(page string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", page)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", page)
	default:
		cmd = exec.Command("xdg-open", page)
	}
	return cmd.Start()
}

// errLoginEnd stops the login stream after the done frame.
var errLoginEnd = errors.New("login ended")

// connectorLogin follows one login flow on the terminal.
type connectorLogin struct {
	client *APIClient
	name   string
	flowID string
	w      io.Writer
	in     *bufio.Reader
	err    error // the result of the done frame
	// browser opens the page of a url step.
	browser bool
	pasting bool
}

func runConnectorLogin(ctx context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var start struct {
		FlowID string `json:"flow_id"`
		ID     string `json:"id"`
		Form   string `json:"form"`
	}
	if err := client.DoHTTP(request.BuildConnectorLogin(ref, c.String("method")), &start); err != nil {
		return err
	}
	if start.Form != "" {
		fmt.Fprintf(outWriter(c), "Ask the person to paste the key in Trebi: %s\n", start.Form)
		return nil
	}
	var in io.Reader = os.Stdin
	if c.Root().Reader != nil {
		in = c.Root().Reader
	}
	l := &connectorLogin{client: client, name: ref.Ref, flowID: start.FlowID, w: outWriter(c), in: bufio.NewReader(in)}
	l.browser = !c.Bool("no-browser") && c.Root().Reader == nil && term.IsTerminal(int(os.Stdout.Fd()))
	fmt.Fprintf(l.w, "login to %s (flow %s)\n", ref.Ref, start.FlowID)
	// The stream sends the current step first, so the start step is not
	// rendered here.
	err = client.StreamSSE(ctx, request.BuildConnectorLoginStreamPath(ref.Ref, start.FlowID), l.frame)
	switch {
	case errors.Is(err, errLoginEnd):
		return l.err
	case ctx.Err() != nil:
		_ = client.DoHTTP(request.BuildConnectorLoginCancel(ref.Ref, start.FlowID), nil) // best effort; the flow also expires
		return ctx.Err()
	case err != nil:
		return err
	}
	return fmt.Errorf("login stream ended before the login finished")
}

// frame handles one SSE frame of the flow.
func (l *connectorLogin) frame(event, data string) error {
	if event != "step" && event != "done" {
		return nil
	}
	var u struct {
		Step *protocol.Step           `json:"step"`
		Done *protocol.AuthDoneParams `json:"done"`
	}
	if err := json.Unmarshal([]byte(data), &u); err != nil {
		return fmt.Errorf("decode login frame: %w", err)
	}
	if u.Done != nil {
		l.done(*u.Done)
		return errLoginEnd
	}
	if u.Step != nil {
		return l.step(*u.Step)
	}
	return nil
}

func (l *connectorLogin) step(s protocol.Step) error {
	if s.Message != "" {
		fmt.Fprintln(l.w, s.Message)
	}
	switch s.Kind {
	case protocol.StepQR:
		code, err := qr.Encode(s.Data, qr.L)
		if err != nil {
			fmt.Fprintln(l.w, "Make a QR code from this text with a QR tool, then scan it with the app:")
			fmt.Fprintln(l.w, s.Data)
			break
		}
		fmt.Fprintln(l.w, "Scan this code with the app:")
		writeQR(l.w, code)
	case protocol.StepDeviceCode:
		fmt.Fprintf(l.w, "Open %s and enter the code %s\n", s.URL, s.Code)
	case protocol.StepURL:
		fmt.Fprintf(l.w, "Open %s\n", s.URL)
		if l.browser && openBrowser(s.URL) == nil {
			fmt.Fprintln(l.w, "The login page is open in the browser.")
		}
		if len(s.Fields) == 1 && !l.pasting {
			l.pasting = true
			// It stops after one line. A login that ends first leaves it
			// on the read until the command exits.
			go l.paste(s.Fields[0])
		}
	case protocol.StepInput:
		return l.input(s.Fields)
	case protocol.StepWait:
		if s.Message == "" {
			fmt.Fprintln(l.w, "waiting…")
		}
	}
	if s.ExpiresAt != "" {
		fmt.Fprintf(l.w, "(expires %s)\n", s.ExpiresAt)
	}
	return nil
}

// writeQR prints a QR code with half-block characters: one text line holds
// two module rows. A quiet zone of two modules surrounds the code, and dark
// modules print as spaces on a light cell, so the code scans on a dark
// terminal.
func writeQR(w io.Writer, code *qr.Code) {
	const quiet = 2
	dark := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		return x >= 0 && y >= 0 && x < code.Size && y < code.Size && code.Black(x, y)
	}
	n := code.Size + 2*quiet
	var b strings.Builder
	for y := 0; y < n; y += 2 {
		for x := 0; x < n; x++ {
			top, bottom := !dark(x, y), y+1 < n && !dark(x, y+1)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\n")
	}
	fmt.Fprint(w, b.String())
}

// input asks for each field and submits the answers. A field with choices
// takes a choice number, or numbers split by commas with multiple.
func (l *connectorLogin) input(fields []protocol.StepField) error {
	answers := make(map[string]any, len(fields))
	for _, f := range fields {
		label := cmp.Or(f.Label, f.Name)
		if f.Help != "" {
			fmt.Fprintf(l.w, "%s\n", f.Help)
		}
		if f.URL != "" {
			fmt.Fprintf(l.w, "Get it at %s\n", f.URL)
		}
		for i, ch := range f.Choices {
			fmt.Fprintf(l.w, "  %d. %s\n", i+1, cmp.Or(ch.Label, ch.Value))
		}
		fmt.Fprintf(l.w, "%s: ", label)
		line, err := l.read(f.Secret)
		if err != nil {
			return fmt.Errorf("read %s: %w", label, err)
		}
		if len(f.Choices) == 0 {
			answers[f.Name] = line
			continue
		}
		picked, err := pickChoices(f, line)
		if err != nil {
			return err
		}
		if f.Multiple {
			answers[f.Name] = picked
		} else if len(picked) > 0 {
			answers[f.Name] = picked[0]
		}
	}
	return l.client.DoHTTP(request.BuildConnectorLoginSubmit(l.name, l.flowID, answers), nil)
}

// read reads one answer. A secret on a terminal does not echo.
func (l *connectorLogin) read(secret bool) (string, error) {
	if secret && term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(l.w)
		return strings.TrimSpace(string(b)), err
	}
	line, err := l.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// paste reads the address from the browser, or a code, and submits it. An
// empty line or the end of stdin sends nothing.
func (l *connectorLogin) paste(f protocol.StepField) {
	if f.Help != "" {
		fmt.Fprintln(l.w, f.Help)
	}
	fmt.Fprintf(l.w, "%s (or wait): ", cmp.Or(f.Label, f.Name))
	line, err := l.read(false)
	if err != nil || line == "" {
		return
	}
	if err := l.client.DoHTTP(request.BuildConnectorLoginSubmit(l.name, l.flowID, map[string]any{f.Name: line}), nil); err != nil {
		fmt.Fprintf(l.w, "%v\n", err)
	}
}

// pickChoices maps an answer to choice values. An item is a choice number or
// a choice value. An empty answer keeps the default.
func pickChoices(f protocol.StepField, line string) ([]string, error) {
	if line == "" {
		return f.Default, nil
	}
	var out []string
	for item := range strings.SplitSeq(line, ",") {
		item = strings.TrimSpace(item)
		if n, err := strconv.Atoi(item); err == nil && n >= 1 && n <= len(f.Choices) {
			out = append(out, f.Choices[n-1].Value)
			continue
		}
		if !slices.ContainsFunc(f.Choices, func(ch protocol.Choice) bool { return ch.Value == item }) {
			return nil, fmt.Errorf("%q is not a choice of %s", item, cmp.Or(f.Label, f.Name))
		}
		out = append(out, item)
	}
	if len(out) > 1 && !f.Multiple {
		return nil, fmt.Errorf("%s takes one choice", cmp.Or(f.Label, f.Name))
	}
	return out, nil
}

func runConnectorLoginMethods(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		ID      string                   `json:"id"`
		Methods []connectors.LoginMethod `json:"methods"`
	}
	req := request.BuildConnectorLoginMethods(ref)
	if c.Bool("json") {
		var raw map[string]any
		if err := client.GetJSON(req.Path, &raw); err != nil {
			return err
		}
		return encodeJSON(c, raw)
	}
	if err := client.GetJSON(req.Path, &body); err != nil {
		return err
	}
	tbl := newTable(outWriter(c), []string{"Method", "Type", "Label", "Available"})
	for _, m := range body.Methods {
		avail := "yes"
		if !m.Available {
			avail = "no: " + m.Reason
		}
		tbl.AppendRow(table.Row{m.ID, m.Type, m.Label, avail})
	}
	renderTable(tbl)
	return nil
}

func (l *connectorLogin) done(d protocol.AuthDoneParams) {
	if !d.OK {
		l.err = fmt.Errorf("login failed: %s", dash(d.Error))
		return
	}
	who := ""
	if d.Account != nil {
		who = d.Account.Name
		if who == "" {
			who = d.Account.ID
		}
	}
	if who != "" {
		fmt.Fprintf(l.w, "logged in as %s\n", who)
		return
	}
	fmt.Fprintln(l.w, "logged in")
}

func runConnectorLogout(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorLogout(ref), &body); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s logged out\n", ref.Ref)
	return nil
}

func runConnectorConformance(ctx context.Context, c *cli.Command) error {
	local := c.String("command") != "" || c.String("manifest") != ""
	var rows []conformance.Result
	var payload any
	if local {
		if c.Args().Len() > 0 {
			return fmt.Errorf("give a name or --command/--manifest, not both")
		}
		t, only, err := localConformance(c)
		if err != nil {
			return err
		}
		rows = conformance.NewRunner(slog.Default()).Run(ctx, t, only)
		payload = rows
	} else {
		ref, err := connectorRef(c)
		if err != nil {
			return fmt.Errorf("give a connector, --command, or --manifest")
		}
		client, err := clientFor(c)
		if err != nil {
			return err
		}
		req := request.BuildConnectorConformance(ref, c.String("room"), c.StringSlice("suite"))
		var raw json.RawMessage
		if err := client.DoHTTP(req, &raw); err != nil {
			return err
		}
		payload = raw
		var body struct {
			Results []conformance.Result `json:"results"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.Results == nil {
			if err := json.Unmarshal(raw, &body.Results); err != nil {
				return fmt.Errorf("conformance: read the result: %w", err)
			}
		}
		rows = body.Results
	}
	if c.Bool("json") {
		if err := encodeJSON(c, payload); err != nil {
			return err
		}
	} else {
		tbl := newTable(outWriter(c), []string{"Suite", "Check", "Status", "Message"})
		for _, r := range rows {
			tbl.AppendRow(table.Row{r.Suite, r.Check, r.Status, dash(r.Message)})
		}
		renderTable(tbl)
	}
	if slices.ContainsFunc(rows, func(r conformance.Result) bool { return r.Status == conformance.StatusFail }) {
		return errors.New("conformance: a check failed")
	}
	if c.Bool("strict") && slices.ContainsFunc(rows, func(r conformance.Result) bool { return r.Status == conformance.StatusWarn }) {
		return errors.New("conformance: a check warned (--strict)")
	}
	return nil
}

// localConformance builds the sandbox target from --manifest and
// --command. With --command only, the manifest is a stub and the default
// suites are protocol and folder.
func localConformance(c *cli.Command) (conformance.Target, []string, error) {
	t := conformance.Target{Command: c.String("command"), Room: c.String("room")}
	only := c.StringSlice("suite")
	if path := c.String("manifest"); path != "" {
		m, dir, err := conformanceManifest(path)
		if err != nil {
			return t, nil, err
		}
		t.Manifest, t.Dir = m, dir
		return t, only, nil
	}
	t.Manifest = manifest.Manifest{
		Schema: manifest.SchemaID, Name: "adapter", Title: "Adapter", Description: "adapter from --command", Version: "0.0.0",
		Events: &manifest.Events{Protocol: manifest.ProtocolConnector, Command: t.Command},
	}
	if len(only) == 0 {
		only = []string{conformance.SuiteProtocol, conformance.SuiteFolder}
	}
	return t, only, nil
}

// conformanceManifest reads a manifest file or folder. It does not
// validate; the manifest suite reports each error as a row.
func conformanceManifest(path string) (manifest.Manifest, string, error) {
	dir := filepath.Dir(path)
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		dir, path = path, filepath.Join(path, manifest.FileName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest.Manifest{}, "", err
	}
	m, err := manifest.Parse(data)
	if err != nil {
		return manifest.Manifest{}, "", fmt.Errorf("%s: %w", path, err)
	}
	return m, dir, nil
}

func runConnectorRooms(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	path := request.BuildConnectorRoomsPath(ref, c.String("query"), c.String("kind"), c.String("cursor"), c.Int("limit"))
	var body map[string]any
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	rooms, _ := body["rooms"].([]any)
	tbl := newTable(w, []string{"ID", "Name", "Kind", "Parent", "Last Activity"})
	for _, raw := range rooms {
		m, _ := raw.(map[string]any)
		tbl.AppendRow(table.Row{
			mapStr(m, "id"), dash(mapStr(m, "name")), dash(mapStr(m, "kind")),
			dash(mapStr(m, "parent")), dash(mapStr(m, "last_activity_at")),
		})
	}
	renderTable(tbl)
	printNextCursor(c, body)
	return nil
}

func runConnectorSenders(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildConnectorSendersPath(ref, c.String("query"), c.Int("limit")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	senders, _ := body["senders"].([]any)
	tbl := newTable(outWriter(c), []string{"Identity", "Name", "Events", "Last Event"})
	for _, raw := range senders {
		m, _ := raw.(map[string]any)
		tbl.AppendRow(table.Row{mapStr(m, "identity"), dash(mapStr(m, "name")), m["count"], dash(mapStr(m, "last_at"))})
	}
	renderTable(tbl)
	return nil
}

func runConnectorDependents(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildConnectorDependentsPath(ref), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	triggers, _ := body["triggers"].([]any)
	tbl := newTable(outWriter(c), []string{"Kind", "Workspace", "Name", "Route", "State", "Rooms", "Senders"})
	for _, raw := range triggers {
		m, _ := raw.(map[string]any)
		state := mapStr(m, "gets")
		switch {
		case mapStr(m, "broken") != "":
			state = "broken"
		case m["active"] != true:
			state = "inactive"
		}
		rooms := "any"
		if list, _ := m["rooms"].([]any); len(list) > 0 {
			rooms = strings.Trim(fmt.Sprint(list), "[]")
		}
		tbl.AppendRow(table.Row{mapStr(m, "kind"), mapStr(m, "workspace"), mapStr(m, "name"), mapStr(m, "route_id"), state, rooms, m["senders"]})
	}
	renderTable(tbl)
	bindings, _ := body["bindings"].([]any)
	if len(bindings) == 0 {
		return nil
	}
	tbl = newTable(outWriter(c), []string{"Workspace", "Operator", "Room", "Thread", "Conversation"})
	for _, raw := range bindings {
		m, _ := raw.(map[string]any)
		tbl.AppendRow(table.Row{mapStr(m, "workspace"), mapStr(m, "operator"), mapStr(m, "room"), dash(mapStr(m, "thread")), mapStr(m, "conversation_id")})
	}
	renderTable(tbl)
	return nil
}

func runConnectorThreads(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	room := c.Args().Get(1)
	if err != nil || room == "" {
		return fmt.Errorf("usage: trebi connector threads <connector> <room>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildConnectorThreadsPath(ref, room, c.String("cursor"), c.Int("limit")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	threads, _ := body["threads"].([]any)
	tbl := newTable(w, []string{"ID", "Title", "Participants", "Last Activity"})
	for _, raw := range threads {
		m, _ := raw.(map[string]any)
		tbl.AppendRow(table.Row{
			mapStr(m, "id"), dash(mapStr(m, "title")),
			dash(strings.Join(stringList(m["participants"]), ", ")), dash(mapStr(m, "last_activity_at")),
		})
	}
	renderTable(tbl)
	printNextCursor(c, body)
	return nil
}

// printNextCursor tells on stderr how to read the next page.
func printNextCursor(c *cli.Command, body map[string]any) {
	var ew io.Writer = os.Stderr
	if c.Root().ErrWriter != nil {
		ew = c.Root().ErrWriter
	}
	if next := mapStr(body, "next_cursor"); next != "" {
		fmt.Fprintf(ew, "more: --cursor %s\n", next)
	}
}

func runConnectorBind(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	room := c.Args().Get(1)
	if err != nil || room == "" {
		return fmt.Errorf("usage: trebi connector bind <connector> <room> --workspace W [--operator assistant|<agent>] [--thread T]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorBind(ref, c.String("workspace"), room, c.String("thread"), c.String("operator")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "room %s of %s now goes to %s\n", room, ref.Ref, c.String("operator"))
	return nil
}

func runConnectorOverrideShow(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector override show <connector> --workspace W")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorOverrideGet(c.String("workspace"), name), &body); err != nil {
		return err
	}
	return encodeJSON(c, body)
}

func runConnectorOverrideSet(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector override set <connector> --workspace W [flags]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws := c.String("workspace")
	var cur struct {
		Override map[string]any `json:"override"`
	}
	if err := client.DoHTTP(request.BuildConnectorOverrideGet(ws, name), &cur); err != nil && !isNotFound(err) {
		return err
	}
	layer := cur.Override
	if layer == nil {
		layer = map[string]any{}
	}
	if err := overlayOverrideFlags(c, layer); err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildConnectorOverrideSet(ws, name, layer), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s set for workspace %s\n", name, ws)
	return nil
}

// overlayOverrideFlags writes the flags given onto the stored layer.
func overlayOverrideFlags(c *cli.Command, layer map[string]any) error {
	if c.IsSet("disabled") {
		layer["disabled"] = c.Bool("disabled")
	}
	patch, err := readObjectFile(c.String("patch-file"))
	switch {
	case err != nil:
		return err
	case string(patch) == "null":
		delete(layer, "patch")
	case patch != nil:
		layer["patch"] = patch
	}
	return nil
}

func runConnectorOverrideDelete(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector override delete <connector> --workspace W")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildConnectorOverrideDelete(c.String("workspace"), name), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s has no layer in workspace %s\n", name, c.String("workspace"))
	return nil
}

// watchValues reads repeated --set name=value flags. A name given twice
// becomes a list.
func watchValues(c *cli.Command) (map[string]any, error) {
	if !c.IsSet("set") {
		return nil, nil
	}
	out := map[string]any{}
	for _, kv := range c.StringSlice("set") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--set %q: use name=value", kv)
		}
		switch cur := out[k].(type) {
		case nil:
			out[k] = v
		case string:
			out[k] = []string{cur, v}
		case []string:
			out[k] = append(cur, v)
		}
	}
	return out, nil
}

// watchEvents reads --events. An empty value clears the list.
func watchEvents(c *cli.Command) []string {
	if !c.IsSet("events") {
		return nil
	}
	out := []string{}
	for _, v := range c.StringSlice("events") {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

func runConnectorWatching(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var view connectors.SubscriptionView
	if c.Bool("sync") {
		err = client.DoHTTP(request.BuildConnectorSubscriptionsSync(ref), &view)
	} else {
		err = client.GetJSON(request.BuildConnectorSubscriptionsPath(ref), &view)
	}
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, view)
	}
	w := outWriter(c)
	switch view.Webhook.State {
	case connectors.WebhookReady:
		fmt.Fprintln(w, "Live updates: on")
	case connectors.WebhookUnavailable:
		fmt.Fprintln(w, "Live updates: off, link Trebi Cloud")
	}
	tbl := newTable(w, []string{"ID", "Title", "Events", "Mode", "State", "Expires"})
	for _, r := range view.Subscriptions {
		tbl.AppendRow(table.Row{r.ID, dash(r.Title), dashOr(strings.Join(r.Types, ","), "all"), dash(r.Mode), dash(r.State), dash(r.ExpiresAt)})
	}
	renderTable(tbl)
	for _, r := range view.Subscriptions {
		if r.Action != nil {
			printWatchAction(w, ref.Ref, r.ID, r.Action, c.Bool("reveal"))
		}
	}
	return nil
}

// printWatchAction prints the steps of one watch that needs a person.
func printWatchAction(w io.Writer, ref, id string, a *protocol.Action, reveal bool) {
	fmt.Fprintf(w, "\n%s: %s\n", id, a.Text)
	if a.URL != "" {
		fmt.Fprintf(w, "  open: %s\n", a.URL)
	}
	for _, v := range a.Show {
		val := v.Value
		if v.Secret && !reveal {
			val = "******** (--reveal shows it)"
		}
		fmt.Fprintf(w, "  %s: %s\n", v.Label, val)
	}
	if len(a.Ask) > 0 {
		args := make([]string, 0, len(a.Ask))
		for _, f := range a.Ask {
			args = append(args, "--set "+f.Name+"=...")
		}
		fmt.Fprintf(w, "  then: trebi connector watch-finish %s %s %s\n", ref, id, strings.Join(args, " "))
	}
}

func runConnectorWatch(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	if err != nil {
		return err
	}
	values, err := watchValues(c)
	if err != nil {
		return err
	}
	return doWatchRequest(c, request.BuildConnectorSubscribe(ref, values, watchEvents(c)), "watch added")
}

func runConnectorWatchSet(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	id := c.Args().Get(1)
	if err != nil || id == "" {
		return fmt.Errorf("usage: trebi connector watch-set <connector> <id> [--set name=value] [--events a,b]")
	}
	values, err := watchValues(c)
	if err != nil {
		return err
	}
	return doWatchRequest(c, request.BuildConnectorSubscriptionSet(ref, id, values, watchEvents(c)), "watch "+id+" changed")
}

func runConnectorUnwatch(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	id := c.Args().Get(1)
	if err != nil || id == "" {
		return fmt.Errorf("usage: trebi connector unwatch <connector> <id>")
	}
	return doWatchRequest(c, request.BuildConnectorUnsubscribe(ref, id), "watch "+id+" stopped")
}

func runConnectorWatchFinish(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	id := c.Args().Get(1)
	if err != nil || id == "" {
		return fmt.Errorf("usage: trebi connector watch-finish <connector> <id> --set name=value")
	}
	values, err := watchValues(c)
	if err != nil {
		return err
	}
	fields := map[string]string{}
	for k, v := range values {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("--set %s: give one value", k)
		}
		fields[k] = s
	}
	return doWatchRequest(c, request.BuildConnectorSubscriptionSubmit(ref, id, fields), "watch "+id+" sent")
}

// doWatchRequest sends one watch change and prints the result.
func doWatchRequest(c *cli.Command, req request.HTTPRequest, done string) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(req, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintln(outWriter(c), done)
	return nil
}

func runConnectorWatchOptions(_ context.Context, c *cli.Command) error {
	ref, err := connectorRef(c)
	field := c.Args().Get(1)
	if err != nil || field == "" {
		return fmt.Errorf("usage: trebi connector watch-options <connector> <field> [--query Q] [--set name=value]")
	}
	values, err := watchValues(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorSubscriptionOptions(ref, field, c.String("query"), values, c.String("cursor"), c.Int("limit")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	options, _ := body["options"].([]any)
	tbl := newTable(outWriter(c), []string{"Value", "Label", "Description"})
	for _, raw := range options {
		m, _ := raw.(map[string]any)
		tbl.AppendRow(table.Row{mapStr(m, "value"), dash(mapStr(m, "label")), dash(mapStr(m, "description"))})
	}
	renderTable(tbl)
	printNextCursor(c, body)
	return nil
}
