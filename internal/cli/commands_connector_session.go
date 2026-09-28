package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/procgroup"
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
			ArgsUsage: "<name>",
			Flags: append(connectorInstanceFlags(),
				&cli.StringFlag{Name: "kind", Usage: "login kind to ask for, such as qr (default: the connector chooses)"}),
			Action: runConnectorLogin,
		},
		{
			Name:      "conformance",
			Usage:     "check an adapter against the trebi-connector/1 protocol (Part H)",
			ArgsUsage: "[name]",
			Description: "With a name, the daemon checks the live instance and does not stop it.\n" +
				"With --command or --manifest, the check runs here with no daemon: it starts the adapter, checks it, and shuts it down.\n" +
				"A message is sent only to the --room you name.",
			Flags: append(connectorInstanceFlags(),
				&cli.StringFlag{Name: "command", Usage: "adapter command to start here (default: events.command of --manifest)"},
				&cli.StringFlag{Name: "manifest", Usage: "trebi-connector.yaml, or its folder, that declares the features, events, and login"},
				&cli.StringFlag{Name: "room", Usage: "test room; with it, the check sends one message twice with one key"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"}),
			Action: runConnectorConformance,
		},
		{
			Name:      "logout",
			Usage:     "log out of a connector account",
			ArgsUsage: "<name>",
			Flags:     connectorInstanceFlags(),
			Action:    runConnectorLogout,
		},
		{
			Name:      "rooms",
			Usage:     "list the rooms of a channel connector (chats, groups, mailboxes)",
			ArgsUsage: "<name>",
			Flags: append(connectorInstanceFlags(),
				&cli.StringFlag{Name: "query", Usage: "match on the room name"},
				&cli.StringFlag{Name: "kind", Usage: "keep rooms of this kind"},
				&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (next_cursor of the previous page)"},
				&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Usage: "max rows"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"}),
			Action: runConnectorRooms,
		},
		{
			Name:      "threads",
			Usage:     "list the threads of one room",
			ArgsUsage: "<name> <room>",
			Flags: append(connectorInstanceFlags(),
				&cli.StringFlag{Name: "cursor", Usage: "pagination cursor (next_cursor of the previous page)"},
				&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Usage: "max rows"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"}),
			Action: runConnectorThreads,
		},
		{
			Name:      "bind",
			Usage:     "send the messages of a room to the assistant or an agent of a workspace",
			ArgsUsage: "<name> <room>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Required: true, Usage: "workspace id or name"},
				&cli.StringFlag{Name: "operator", Value: "assistant", Usage: "assistant or agents/<name>"},
				&cli.StringFlag{Name: "thread", Usage: "bind one thread of the room only"},
				&cli.StringFlag{Name: "instance", Usage: "instance key: <name> or <name>@<workspace>"},
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
		Usage: "show or change the connector settings of one workspace",
		Commands: []*cli.Command{
			{
				Name:      "show",
				Usage:     "show the workspace layer of a connector",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{ws()},
				Action:    runConnectorOverrideShow,
			},
			{
				Name:      "set",
				Usage:     "change the workspace layer of a connector (flags overlay the stored layer)",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					ws(),
					&cli.StringFlag{Name: "version", Usage: "catalog version for this workspace"},
					&cli.StringFlag{Name: "track", Usage: "update track: exact|patch|minor|latest"},
					&cli.StringSliceFlag{Name: "input", Aliases: []string{"i"}, Usage: "KEY=VALUE setup input (repeatable)"},
					&cli.StringFlag{Name: "patch-file", Usage: "YAML or JSON merge patch on the manifest; a file that holds null removes it"},
					&cli.BoolFlag{Name: "disabled", Usage: "turn the connector off in this workspace (--disabled=false turns it on)"},
				},
				Action: runConnectorOverrideSet,
			},
			{
				Name:      "delete",
				Aliases:   []string{"remove"},
				Usage:     "remove the workspace layer; the workspace uses the global connector again",
				ArgsUsage: "<name>",
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
		FlowID   string `json:"flow_id"`
		Instance string `json:"instance"`
	}
	if err := client.DoHTTP(request.BuildConnectorLogin(ref, c.String("kind")), &start); err != nil {
		return err
	}
	var in io.Reader = os.Stdin
	if c.Root().Reader != nil {
		in = c.Root().Reader
	}
	l := &connectorLogin{client: client, name: ref.Name, flowID: start.FlowID, w: outWriter(c), in: bufio.NewReader(in)}
	fmt.Fprintf(l.w, "login to %s (flow %s)\n", start.Instance, start.FlowID)
	// The stream sends the current step first, so the start step is not
	// rendered here.
	err = client.StreamSSE(ctx, request.BuildConnectorLoginStreamPath(ref.Name, start.FlowID), l.frame)
	switch {
	case errors.Is(err, errLoginEnd):
		return l.err
	case ctx.Err() != nil:
		_ = client.DoHTTP(request.BuildConnectorLoginCancel(ref.Name, start.FlowID), nil) // best effort; the flow also expires
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
	fmt.Fprintf(outWriter(c), "connector %s logged out\n", dash(mapStr(body, "instance")))
	return nil
}

func runConnectorConformance(ctx context.Context, c *cli.Command) error {
	local := c.String("command") != "" || c.String("manifest") != ""
	var body map[string]any
	if local {
		if c.Args().Len() > 0 {
			return fmt.Errorf("give a name or --command/--manifest, not both")
		}
		res, err := localConformance(ctx, c)
		if err != nil {
			return err
		}
		ok := !slices.ContainsFunc(res, func(r protocol.CheckResult) bool { return r.Status == protocol.CheckFail })
		b, err := json.Marshal(map[string]any{"ok": ok, "results": res})
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &body); err != nil {
			return err
		}
	} else {
		ref, err := connectorRef(c)
		if err != nil {
			return fmt.Errorf("give a name, --command, or --manifest")
		}
		client, err := clientFor(c)
		if err != nil {
			return err
		}
		if err := client.DoHTTP(request.BuildConnectorConformance(ref, c.String("room")), &body); err != nil {
			return err
		}
	}
	if c.Bool("json") {
		if err := encodeJSON(c, body); err != nil {
			return err
		}
	} else {
		results, _ := body["results"].([]any)
		tbl := newTable(outWriter(c), []string{"Check", "Status", "Message"})
		for _, raw := range results {
			if m, ok := raw.(map[string]any); ok {
				tbl.AppendRow(table.Row{mapStr(m, "check"), mapStr(m, "status"), dash(mapStr(m, "message"))})
			}
		}
		renderTable(tbl)
	}
	if ok, _ := body["ok"].(bool); !ok {
		return fmt.Errorf("conformance: a check failed")
	}
	return nil
}

// localConformance starts an adapter here and runs every check on it,
// shutdown included. The manifest gives the promises to check.
func localConformance(ctx context.Context, c *cli.Command) ([]protocol.CheckResult, error) {
	chk := &protocol.Checker{Room: c.String("room")}
	script, dir := c.String("command"), ""
	if path := c.String("manifest"); path != "" {
		cmdline, err := conformanceManifest(chk, path)
		if err != nil {
			return nil, err
		}
		dir = filepath.Dir(path)
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			dir = path
		}
		if script == "" {
			script = cmdline
		}
	}
	if script == "" {
		return nil, fmt.Errorf("the manifest has no events.command; give --command")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer pw.Close() //nolint:errcheck // closed again below; the second close is a no-op
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	env := append(os.Environ(), "TREBI_PROTOCOL="+protocol.Version)
	cmd, stdout, stderr, err := procgroup.Command{Script: script, Dir: dir, Env: env, Stdin: pr}.Start(pctx)
	_ = pr.Close() // the child holds its own copy
	if err != nil {
		return nil, err
	}
	var ew io.Writer = os.Stderr
	if c.Root().ErrWriter != nil {
		ew = c.Root().ErrWriter
	}
	errDone := make(chan struct{})
	go func() { // ends when the adapter exits
		defer close(errDone)
		_, _ = io.Copy(ew, stderr)
	}()
	conn := protocol.NewConn(stdout, pw, chk.Notify)
	go func() { _ = conn.Run(context.Background()) }() // ends at stdout EOF
	res := chk.Run(ctx, conn)
	_ = pw.Close() // EOF on stdin asks the adapter to exit
	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
	}
	cancel() // kills an adapter that did not exit
	<-conn.Done()
	<-errDone
	_ = cmd.Wait() // the shutdown check reports how it exited
	return res, nil
}

// conformanceManifest reads the promises of a manifest file or folder into
// chk and returns its events.command.
func conformanceManifest(chk *protocol.Checker, path string) (string, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, manifest.FileName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m, err := manifest.Parse(data)
	if err == nil {
		err = m.Validate()
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	chk.Features = m.Features()
	if m.Setup != nil {
		chk.Login = m.Setup.Login
	}
	if chk.Schemas, err = m.EventSchemas(filepath.Dir(path)); err != nil {
		return "", err
	}
	if m.Events == nil {
		return "", nil
	}
	for _, t := range m.Events.Types {
		chk.EventTypes = append(chk.EventTypes, t.Type)
	}
	return m.Events.Command, nil
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
		return fmt.Errorf("usage: trebi connector threads <name> <room>")
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
		return fmt.Errorf("usage: trebi connector bind <name> <room> --workspace W [--operator assistant|agents/<name>] [--thread T]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildConnectorBind(ref, room, c.String("thread"), c.String("operator")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "room %s of %s now goes to %s\n", room, ref.Name, c.String("operator"))
	return nil
}

func runConnectorOverrideShow(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("usage: trebi connector override show <name> --workspace W")
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
		return fmt.Errorf("usage: trebi connector override set <name> --workspace W [flags]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	ws := c.String("workspace")
	// The stored layer shows inputs as ***, and the daemon keeps a *** value.
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
	for _, k := range []string{"version", "track"} {
		if c.IsSet(k) {
			layer[k] = c.String(k)
		}
	}
	if c.IsSet("disabled") {
		layer["disabled"] = c.Bool("disabled")
	}
	if c.IsSet("input") {
		kv, err := parseKeyValues(c.StringSlice("input"))
		if err != nil {
			return fmt.Errorf("input %w", err)
		}
		inputs, _ := layer["inputs"].(map[string]any)
		if inputs == nil {
			inputs = map[string]any{}
		}
		for k, v := range kv {
			inputs[k] = v
		}
		layer["inputs"] = inputs
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
		return fmt.Errorf("usage: trebi connector override delete <name> --workspace W")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildConnectorOverrideDelete(c.String("workspace"), name), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "connector %s uses the global settings in workspace %s\n", name, c.String("workspace"))
	return nil
}
