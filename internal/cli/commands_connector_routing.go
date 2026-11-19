package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/routing"
)

// routingView is the answer of GET .../connectors/{ref}/routing.
type routingView struct {
	Workspace  string `json:"workspace"`
	Connection struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Type string `json:"type"`
	} `json:"connection"`
	Path     string            `json:"path"`
	Exists   bool              `json:"exists"`
	Hash     string            `json:"hash"`
	Mode     string            `json:"mode"`
	Notes    string            `json:"notes"`
	Unsure   string            `json:"unsure"`
	NoFit    string            `json:"no_fit"`
	SureAt   float64           `json:"sure_at"`
	Routes   []routingRoute    `json:"routes"`
	Rules    []routingRule     `json:"rules"`
	Problems []routing.Problem `json:"problems"`
}

type routingRoute struct {
	ID     string `json:"id"`
	Target struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Exists bool   `json:"exists"`
	} `json:"target"`
	Gets    string         `json:"gets"`
	UseFor  string         `json:"use_for"`
	Filters map[string]any `json:"filters"`
}

type routingRule struct {
	Field string `json:"field"`
	Value string `json:"value"`
	To    string `json:"to"`
}

func connectorRoutingCommand() *cli.Command {
	ws := func() cli.Flag {
		return &cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id or name (default: the current workspace)"}
	}
	asJSON := func() cli.Flag { return &cli.BoolFlag{Name: "json", Usage: "emit JSON"} }
	return &cli.Command{
		Name:  "routing",
		Usage: "show and change which agent or job gets the messages of a connection",
		Commands: []*cli.Command{
			{
				Name:   "list",
				Usage:  "list the routing of each connection in a workspace",
				Flags:  []cli.Flag{ws(), asJSON()},
				Action: runRoutingList,
			},
			{
				Name:      "show",
				Usage:     "show the settings, routes, rules, and problems of one connection",
				ArgsUsage: "<connector>",
				Flags:     []cli.Flag{ws(), asJSON()},
				Action:    runRoutingShow,
			},
			{
				Name:      "set",
				Usage:     "change the settings of one connection, or the gets and use-for text of its routes",
				ArgsUsage: "<connector>",
				Flags: []cli.Flag{ws(), asJSON(),
					&cli.StringFlag{Name: "mode", Usage: "on or off"},
					&cli.StringFlag{Name: "notes", Usage: "free text that the router reads"},
					&cli.StringFlag{Name: "unsure", Usage: "ask or guess, for a choice below --sure-at"},
					&cli.StringFlag{Name: "no-fit", Usage: "drop or ask, for a message that no route fits"},
					&cli.FloatFlag{Name: "sure-at", Usage: "from 0 to 1"},
					&cli.StringSliceFlag{Name: "gets", Usage: "ID=VALUE: every_message, chosen, by_hand, or off (repeat)"},
					&cli.StringSliceFlag{Name: "use-for", Usage: "ID=TEXT: when the router picks the route (repeat)"},
					&cli.StringFlag{Name: "patch", Usage: "a file, - for stdin, or inline JSON: a patch with mode, notes, routes, remove_routes, and rules"},
					&cli.StringFlag{Name: "if-match", Usage: "fail when the file changed since this hash"},
				},
				Action: runRoutingSet,
			},
			{
				Name:      "add",
				Usage:     "add one route to a connection",
				ArgsUsage: "<connector>",
				Flags: []cli.Flag{ws(), asJSON(),
					&cli.StringFlag{Name: "agent", Usage: "the agent that gets the messages"},
					&cli.StringFlag{Name: "job", Usage: "the job (AGENT/JOB) that gets the messages"},
					&cli.BoolFlag{Name: "assistant", Usage: "the assistant gets the messages"},
					&cli.StringFlag{Name: "gets", Usage: "every_message, chosen, by_hand, or off (default: chosen when routing is on, else off)"},
					&cli.StringFlag{Name: "use-for", Usage: "when the router picks the route"},
					&cli.StringSliceFlag{Name: "room", Usage: "only this room (repeat)"},
					&cli.StringSliceFlag{Name: "allow", Usage: "only this sender (repeat)"},
					&cli.StringFlag{Name: "from", Usage: "people, bots, or anyone"},
					&cli.StringSliceFlag{Name: "contains", Usage: "only text with this word (repeat)"},
					&cli.StringFlag{Name: "match", Usage: "only events where this expression is true"},
					&cli.StringFlag{Name: "batch", Usage: "collect messages for this long, for example 5m"},
					&cli.StringFlag{Name: "chat", Usage: "listen or reply"},
					&cli.StringFlag{Name: "replies", Usage: "continue or off"},
					&cli.StringFlag{Name: "limit", Usage: "N/DURATION, for example 5/1h, or off"},
					&cli.StringSliceFlag{Name: "input", Usage: "NAME=VALUE for an input of the job (repeat)"},
				},
				Action: runRoutingAdd,
			},
			{
				Name:      "remove",
				Aliases:   []string{"rm"},
				Usage:     "remove routes from a connection",
				ArgsUsage: "<connector> <route-id>...",
				Flags: []cli.Flag{ws(), asJSON(),
					&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "do not ask"},
				},
				Action: runRoutingRemove,
			},
		},
	}
}

// routingWorkspace returns the workspace of a routing command.
func routingWorkspace(c *cli.Command) (string, error) {
	if ws := workspaceFlag(c); ws != "" {
		return ws, nil
	}
	return "", fmt.Errorf("use --workspace to name the workspace")
}

// typedRouting reads the typed view out of an answer.
func typedRouting(raw map[string]any) (v routingView, err error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(b, &v)
}

func getRoutingView(c *cli.Command, ws, connector string) (raw map[string]any, v routingView, err error) {
	client, err := clientFor(c)
	if err != nil {
		return nil, v, err
	}
	if err = client.DoHTTP(request.BuildRoutingGet(ws, connector), &raw); err != nil {
		return nil, v, err
	}
	v, err = typedRouting(raw)
	return raw, v, err
}

func runRoutingList(_ context.Context, c *cli.Command) error {
	ws, err := routingWorkspace(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildRoutingListPath(ws), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	items, _ := body["routing"].([]any)
	tbl := newTable(outWriter(c), []string{"Connection", "Type", "Routing", "Routes", "Rules", "Problems"})
	for _, item := range items {
		raw, _ := item.(map[string]any)
		v, err := typedRouting(raw)
		if err != nil {
			return err
		}
		mode := v.Mode
		if !v.Exists {
			mode = "no file"
		}
		tbl.AppendRow(table.Row{v.Connection.Slug, v.Connection.Type, mode, len(v.Routes), len(v.Rules), len(v.Problems)})
	}
	renderTable(tbl)
	return nil
}

func runRoutingShow(_ context.Context, c *cli.Command) error {
	conn := c.Args().Get(0)
	if conn == "" {
		return fmt.Errorf("usage: trebi connector routing show <connector> [--workspace W]")
	}
	ws, err := routingWorkspace(c)
	if err != nil {
		return err
	}
	raw, v, err := getRoutingView(c, ws, conn)
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, raw)
	}
	printRoutingView(outWriter(c), v, c.String("workspace"))
	return nil
}

func printRoutingView(w io.Writer, v routingView, workspace string) {
	if workspace == "" {
		workspace = v.Workspace
	}
	file := v.Path
	if !v.Exists {
		file += " (no file yet)"
	}
	fmt.Fprintf(w, "%s: routing %s  hash %s\nfile: %s\n", v.Connection.Slug, v.Mode, dash(v.Hash), file)
	fmt.Fprintf(w, "unsure: %s  no_fit: %s  sure_at: %v\n", dash(v.Unsure), dash(v.NoFit), v.SureAt)
	if v.Notes != "" {
		fmt.Fprintf(w, "notes: %s\n", v.Notes)
	}
	if len(v.Routes) > 0 {
		tbl := newTable(w, []string{"ID", "Target", "Gets", "Filters", "Use for"})
		for _, r := range v.Routes {
			target := r.Target.Name
			if !r.Target.Exists {
				target += " (missing)"
			}
			tbl.AppendRow(table.Row{dash(r.ID), target, r.Gets, dash(shortFilters(r.Filters)), dash(r.UseFor)})
		}
		renderTable(tbl)
	}
	for i, r := range v.Rules {
		fmt.Fprintf(w, "rule %d: %s %s -> %s\n", i, r.Field, r.Value, r.To)
	}
	for _, p := range v.Problems {
		fmt.Fprintf(w, "%s %s: %s\n", p.Severity, p.Code, p.Message)
		if p.Fix != nil {
			fmt.Fprintf(w, "  fix: %s\n", p.Fix.CLI(workspace))
		}
	}
}

// shortFilters writes the filters of a route as key=value pairs.
func shortFilters(f map[string]any) string {
	var parts []string
	for _, k := range []string{"rooms", "from", "contains", "allowlist", "match", "extract", "types", "replies", "batch", "chat", "limit"} {
		switch v := f[k].(type) {
		case string:
			if v != "" {
				parts = append(parts, k+"="+v)
			}
		case []any:
			if len(v) > 0 {
				names := make([]string, len(v))
				for i, e := range v {
					names[i] = fmt.Sprint(e)
				}
				parts = append(parts, k+"="+strings.Join(names, ","))
			}
		}
	}
	return strings.Join(parts, " ")
}

func runRoutingSet(_ context.Context, c *cli.Command) error {
	conn := c.Args().Get(0)
	if conn == "" {
		return fmt.Errorf("usage: trebi connector routing set <connector> [--workspace W] [flags]")
	}
	ws, err := routingWorkspace(c)
	if err != nil {
		return err
	}
	body, err := routingSetBody(c)
	if err != nil {
		return err
	}
	return sendRouting(c, ws, conn, body)
}

// routingSetBody builds the patch of "routing set": the --patch input first,
// then the flags on top of it.
func routingSetBody(c *cli.Command) (map[string]any, error) {
	body := map[string]any{}
	if c.IsSet("patch") {
		raw, err := readPatchInput(c)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("--patch: want a JSON object: %w", err)
		}
	}
	p := request.RoutingPatch{
		Mode: setStr(c, "mode"), Notes: setStr(c, "notes"), Unsure: setStr(c, "unsure"), NoFit: setStr(c, "no-fit"),
	}
	if c.IsSet("sure-at") {
		v := c.Float("sure-at")
		p.SureAt = &v
	}
	var err error
	if p.Routes, err = routeEditsFromFlags(c); err != nil {
		return nil, err
	}
	flags, err := request.BuildRoutingPatch(p)
	if err != nil {
		return nil, err
	}
	for k, v := range flags {
		if k == "routes" {
			prev, _ := body["routes"].([]any)
			for _, r := range v.([]map[string]any) {
				prev = append(prev, r)
			}
			v = prev
		}
		body[k] = v
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("nothing to change: give a flag or --patch")
	}
	return body, nil
}

// readPatchInput reads --patch as stdin ("-"), inline JSON, or a file.
func readPatchInput(c *cli.Command) ([]byte, error) {
	arg := strings.TrimSpace(c.String("patch"))
	switch {
	case arg == "-":
		var in io.Reader = os.Stdin
		if c.Root().Reader != nil {
			in = c.Root().Reader
		}
		return io.ReadAll(in)
	case strings.HasPrefix(arg, "{"):
		return []byte(arg), nil
	}
	v, err := readDataFile(arg)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// sendRouting sends one patch and prints the result.
func sendRouting(c *cli.Command, ws, conn string, body map[string]any) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var raw map[string]any
	req := request.HTTPRequest{Method: "PATCH", Path: request.BuildRoutingPath(ws, conn), Body: body, IfMatch: c.String("if-match")}
	if err := client.DoHTTP(req, &raw); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, raw)
	}
	v, err := typedRouting(raw)
	if err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "routing %s: %s, %d routes, %d rules\n", v.Connection.Slug, v.Mode, len(v.Routes), len(v.Rules))
	return nil
}

func runRoutingAdd(_ context.Context, c *cli.Command) error {
	conn := c.Args().Get(0)
	if conn == "" {
		return fmt.Errorf("usage: trebi connector routing add <connector> (--agent NAME | --job AGENT/JOB | --assistant) [flags]")
	}
	ws, err := routingWorkspace(c)
	if err != nil {
		return err
	}
	route, err := routeFromFlags(c)
	if err != nil {
		return err
	}
	_, before, err := getRoutingView(c, ws, conn)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := client.DoHTTP(request.BuildRoutingSet(ws, conn, request.RoutingPatch{Routes: []map[string]any{route}}, ""), &raw); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, raw)
	}
	v, err := typedRouting(raw)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, r := range before.Routes {
		known[r.ID] = true
	}
	for _, r := range v.Routes {
		if !known[r.ID] {
			fmt.Fprintf(outWriter(c), "route %s added: %s gets %s\n", r.ID, r.Target.Name, r.Gets)
		}
	}
	return nil
}

// routeFromFlags builds the new route of "routing add".
func routeFromFlags(c *cli.Command) (map[string]any, error) {
	route := map[string]any{}
	n := 0
	for _, t := range []struct{ flag, key string }{{"agent", "agent"}, {"job", "job"}} {
		if v := c.String(t.flag); v != "" {
			route[t.key] = v
			n++
		}
	}
	if c.Bool("assistant") {
		route["assistant"] = true
		n++
	}
	if n != 1 {
		return nil, fmt.Errorf("use one of --agent, --job, or --assistant")
	}
	for _, f := range []struct{ flag, key string }{
		{"gets", "gets"}, {"use-for", "use_for"}, {"from", "from"}, {"match", "match"},
		{"batch", "batch"}, {"chat", "chat"}, {"replies", "replies"}, {"limit", "limit"},
	} {
		if v := c.String(f.flag); v != "" {
			route[f.key] = v
		}
	}
	for _, f := range []struct{ flag, key string }{{"room", "rooms"}, {"allow", "allowlist"}, {"contains", "contains"}} {
		if v := c.StringSlice(f.flag); len(v) > 0 {
			route[f.key] = v
		}
	}
	if kvs := c.StringSlice("input"); len(kvs) > 0 {
		inputs := map[string]any{}
		for _, kv := range kvs {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return nil, fmt.Errorf("--input %q: use NAME=VALUE", kv)
			}
			inputs[k] = v
		}
		route["inputs"] = inputs
	}
	return route, nil
}

func runRoutingRemove(_ context.Context, c *cli.Command) error {
	conn, ids := c.Args().Get(0), c.Args().Slice()
	if len(ids) > 0 {
		ids = ids[1:]
	}
	if conn == "" || len(ids) == 0 {
		return fmt.Errorf("usage: trebi connector routing remove <connector> <route-id>... [--yes]")
	}
	ws, err := routingWorkspace(c)
	if err != nil {
		return err
	}
	if !c.Bool("yes") {
		if c.Root().Reader != nil || !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("usage: this removes %d route(s) from %s; add --yes to confirm", len(ids), conn)
		}
		fmt.Fprintf(os.Stderr, "Remove %d route(s) from %s? [y/N] ", len(ids), conn)
		var answer string
		_, _ = fmt.Fscanln(os.Stdin, &answer)
		if a := strings.ToLower(answer); a != "y" && a != "yes" {
			return fmt.Errorf("cancelled")
		}
	}
	return sendRouting(c, ws, conn, map[string]any{"remove_routes": ids})
}

// routeEditsFromFlags reads the --gets and --use-for pairs into one change
// for each route id.
func routeEditsFromFlags(c *cli.Command) ([]map[string]any, error) {
	routes := map[string]map[string]any{}
	for flag, key := range map[string]string{"gets": "gets", "use-for": "use_for"} {
		for _, kv := range c.StringSlice(flag) {
			id, v, ok := strings.Cut(kv, "=")
			if !ok || id == "" {
				return nil, fmt.Errorf("--%s %q: use ID=VALUE", flag, kv)
			}
			if routes[id] == nil {
				routes[id] = map[string]any{"id": id}
			}
			routes[id][key] = v
		}
	}
	var out []map[string]any
	for _, id := range slices.Sorted(maps.Keys(routes)) {
		out = append(out, routes[id])
	}
	return out, nil
}
