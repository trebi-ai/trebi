package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"rsc.io/qr"

	"github.com/jedib0t/go-pretty/v6/table"
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
	return []*cli.Command{
		{
			Name:      "login",
			Usage:     "log in to a connector account and follow the steps",
			ArgsUsage: "<connector>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "kind", Usage: "login kind to ask for, such as qr (default: the connector chooses)"},
			},
			Action: runConnectorLogin,
		},
		{
			Name:      "conformance",
			Usage:     "check a connector against its manifest and the trebi-connector/1 protocol",
			ArgsUsage: "[connector]",
			Description: "With a connector, the daemon checks the live connection and does not stop it.\n" +
				"With --manifest or --command, the check runs here with no daemon, in a sandbox with a clean env and temp folders.\n" +
				"The command exits 1 when a check fails.\n" +
				"A message is sent only to the --room you name.",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "command", Usage: "adapter command to start here (default: events.command of --manifest)"},
				&cli.StringFlag{Name: "manifest", Usage: "trebi-connector.yaml, or its folder, that declares the features, events, and login"},
				&cli.StringFlag{Name: "room", Usage: "test room; with it, the check sends one message twice with one key"},
				&cli.StringSliceFlag{Name: "suite", Usage: "suites to run: manifest, install, inputs, actions, actions.cli, actions.mcp, events.lines, protocol, folder (default: each block of the manifest)"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
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
				&cli.StringFlag{Name: "operator", Value: "assistant", Usage: "assistant or agents/<name>"},
				&cli.StringFlag{Name: "thread", Usage: "bind one thread of the room only"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorBind,
		},
		connectorOverrideCommand(),
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
	}
	if err := client.DoHTTP(request.BuildConnectorLogin(ref, c.String("kind")), &start); err != nil {
		return err
	}
	var in io.Reader = os.Stdin
	if c.Root().Reader != nil {
		in = c.Root().Reader
	}
	l := &connectorLogin{client: client, name: ref.Ref, flowID: start.FlowID, w: outWriter(c), in: bufio.NewReader(in)}
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

// input asks for each field and submits the answers.
func (l *connectorLogin) input(fields []protocol.StepField) error {
	answers := make(map[string]string, len(fields))
	for _, f := range fields {
		label := f.Label
		if label == "" {
			label = f.Name
		}
		if f.Help != "" {
			fmt.Fprintf(l.w, "%s\n", f.Help)
		}
		fmt.Fprintf(l.w, "%s: ", label)
		if f.Secret && term.IsTerminal(int(os.Stdin.Fd())) {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(l.w)
			if err != nil {
				return err
			}
			answers[f.Name] = string(b)
			continue
		}
		line, err := l.in.ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read %s: %w", label, err)
		}
		answers[f.Name] = strings.TrimSpace(line)
	}
	return l.client.DoHTTP(request.BuildConnectorLoginSubmit(l.name, l.flowID, answers), nil)
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
		return fmt.Errorf("usage: trebi connector bind <connector> <room> --workspace W [--operator assistant|agents/<name>] [--thread T]")
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
	if err := client.DoHTTP(request.BuildConnectorOverrideGet(ws, name), &cur); err != nil && !strings.Contains(err.Error(), "404") {
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
