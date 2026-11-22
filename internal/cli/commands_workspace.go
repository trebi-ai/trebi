package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

// wsRow is one row of workspace list output (JSON keys stay exported).
type wsRow struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Origin string `json:"origin"`
	Active string `json:"active"`
}

func workspaceCommand() *cli.Command {
	return &cli.Command{
		Name:  "workspace",
		Usage: "manage workspaces and sticky context",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list local daemon workspaces and linked remote workspaces",
				Flags: []cli.Flag{
					limitFlag(),
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runWorkspaceList,
			},
			{
				Name:      "create",
				Aliases:   []string{"add"},
				Usage:     "register a local folder, or link a remote workspace (--url/--remote)",
				ArgsUsage: "[path]",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "name", Usage: "display name (default: basename of path)"},
					&cli.StringFlag{Name: "handle", Usage: "fixed name in the email address, 2-20 of a-z 0-9 - (default: from the name)"},
					&cli.BoolFlag{Name: "mkdir", Aliases: []string{"create"}, Usage: "make the folder if missing; path optional with --name"},
					&cli.StringFlag{Name: "url", Aliases: []string{"remote"}, Usage: "remote trebid base URL"},
					&cli.StringFlag{Name: "id", Usage: "existing remote workspace id to link (with --url)"},
					&cli.BoolFlag{Name: "use", Usage: "set sticky context to this workspace after create"},
					&cli.BoolFlag{Name: "skip-check", Usage: "skip remote health check"},
				},
				Action: runWorkspaceAdd,
			},
			{
				Name:      "show",
				Usage:     "show one workspace",
				ArgsUsage: "<id|name>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON (always on)"}},
				Action:    runWorkspaceShow,
			},
			{
				Name:      "use",
				Usage:     "set sticky workspace context (local id/name or linked remote)",
				ArgsUsage: "<id|name>",
				Action:    runWorkspaceUse,
			},
			{
				Name:   "current",
				Usage:  "print sticky workspace context",
				Flags:  []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: runWorkspaceCurrent,
			},
			{
				Name:      "delete",
				Aliases:   []string{"remove"},
				Usage:     "remove a workspace from Trebi; the folder stays unless --delete-folder",
				ArgsUsage: "<id>",
				Flags: []cli.Flag{
					forceFlag("required; removes the workspace from Trebi"),
					&cli.BoolFlag{Name: "delete-folder", Usage: "also delete the workspace folder on disk"},
				},
				Action: runWorkspaceRemove,
			},
			{
				Name:      "rename",
				Usage:     "rename a workspace",
				ArgsUsage: "<id> <new-name>",
				Action:    runWorkspaceRename,
			},
			{
				Name:      "move",
				Usage:     "move a workspace folder and rewrite absolute paths inside it, or move the workspace to another machine",
				ArgsUsage: "<id|name> <new-path|machine>",
				Flags: []cli.Flag{
					forceFlag("required; moves the folder or the workspace"),
					&cli.BoolFlag{Name: "mkdir", Usage: "create the destination parent if missing"},
					&cli.BoolFlag{Name: "dry-run", Usage: "report rewrites without moving"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runWorkspaceMove,
			},
			{
				Name:      "set",
				Usage:     "update workspace name, handle, git_enabled, contact, notify route, or agent chain limits",
				ArgsUsage: "<id>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "name", Usage: "display name"},
					&cli.StringFlag{Name: "handle", Usage: "fixed name in the email address; only before the workspace has an address"},
					&cli.BoolFlag{Name: "chains", Usage: "agents can ask each other (--chains=false turns it off)"},
					&cli.IntFlag{Name: "chain-max-hops", Usage: "chain depth limit, 1 to 20"},
					&cli.IntFlag{Name: "chain-max-turns", Usage: "chain turn limit, 1 to 500"},
					&cli.FloatFlag{Name: "chain-max-usd", Usage: "chain cost limit in USD; 0 is no limit"},
					&cli.IntFlag{Name: "chain-pair-per-hour", Usage: "asks from one agent to another in one hour, 1 to 1000"},
					&cli.BoolFlag{Name: "git-enabled", Usage: "write git_enabled to trebi.yaml"},
					&cli.StringFlag{Name: "contact", Usage: `contact address, JSON list, or null to inherit (e.g. '["ops@acme.com","admins"]')`},
					&cli.StringFlag{Name: "notify", Usage: `notify route as JSON, or null to inherit (e.g. '[{"via":"email","to":"contact"}]')`},
					&cli.StringFlag{Name: "email-allowlist", Usage: `addresses that may send to and receive from the workspace, as a JSON list, or null to clear (e.g. '["ops@acme.com"]')`},
				},
				Action: runWorkspaceSet,
			},
			{
				Name:      "handle-check",
				Usage:     "check a handle for the workspace email address",
				ArgsUsage: "<handle>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Usage: "the workspace that would use the handle"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runWorkspaceHandleCheck,
			},
			{
				Name:  "email",
				Usage: "workspace mail address",
				Commands: []*cli.Command{
					{
						Name:      "show",
						Usage:     "show the workspace address (masked unless --reveal)",
						ArgsUsage: "[id|name]",
						Flags: []cli.Flag{
							&cli.BoolFlag{Name: "reveal", Usage: "print the full local part"},
							&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
						},
						Action: runWorkspaceEmailShow,
					},
					{
						Name:      "rotate",
						Usage:     "mint a new address; the old address retires",
						ArgsUsage: "[id|name]",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "handle", Usage: "new workspace handle for the address"},
							&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
							&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "confirm the rotate"},
						},
						Action: runWorkspaceEmailRotate,
					},
				},
			},
			{
				Name:      "unlink",
				Usage:     "remove a client-side remote workspace link (does not delete the workspace; see workspace remove)",
				ArgsUsage: "<id|name>",
				Action: func(_ context.Context, c *cli.Command) error {
					id := c.Args().Get(0)
					if id == "" {
						return fmt.Errorf("id or name required")
					}
					if err := RemoveLinkedWorkspace(id); err != nil {
						return err
					}
					// Clear sticky if it pointed at this link.
					st, err := stickyFor(c)
					if err != nil {
						return err
					}
					if st != nil {
						if st.WorkspaceID == id || equalFoldName(id, st.Name) {
							if err := ClearStickyContext(); err != nil {
								return err
							}
						}
					}
					fmt.Fprintf(outWriter(c), "unlinked %s\n", id)
					return nil
				},
			},
		},
	}
}

// runWorkspaceShow prints one workspace as JSON. The id comes from the
// argument, else from -w <id|name>.
func runWorkspaceShow(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		id = workspaceFlag(c)
	}
	if id == "" {
		return fmt.Errorf("usage: trebi workspace show <id|name>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildWorkspaceGetPath(id), &body); err != nil {
		return err
	}
	return encodeJSON(c, body)
}

func runWorkspaceRemove(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi workspace remove <id>")
	}
	what := "removes workspace " + id + " from Trebi; its folder stays"
	if c.Bool("delete-folder") {
		what = "removes workspace " + id + " and deletes its folder"
	}
	confirm, err := confirmed(c, what)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildWorkspaceDelete(id, confirm, c.Bool("delete-folder")), nil); err != nil {
		return err
	}
	if err := clearStickyIfWorkspace(c, id); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "removed %s\n", id)
	return nil
}

func runWorkspaceMove(ctx context.Context, c *cli.Command) error { //nolint:gocyclo // path move and machine move share the argument checks
	id, dest := c.Args().Get(0), c.Args().Get(1)
	if id == "" || dest == "" {
		return fmt.Errorf("usage: trebi workspace move <id|name> <new-path|machine>")
	}
	if m, ok, err := machineArg(c, dest); err != nil {
		return err
	} else if ok {
		return runWorkspaceMoveToMachine(ctx, c, id, m)
	}
	dry := c.Bool("dry-run")
	confirm := dry
	if !dry {
		var err error
		confirm, err = confirmed(c, "moves the workspace folder for "+id+" and rewrites absolute paths in its files")
		if err != nil {
			return err
		}
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildWorkspaceMove(id, dest, c.Bool("mkdir"), dry, confirm), &out); err != nil {
		return err
	}
	if !dry {
		if err := updateStickyPath(c, id, mapStr(out, "name"), mapStr(out, "path")); err != nil {
			return err
		}
	}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	from := dest
	if p := mapStr(out, "path"); p != "" {
		from = p
	}
	verb, rewrote := "moved", "rewrote"
	if dry {
		verb, rewrote = "would move", "would rewrite"
	}
	fmt.Fprintf(outWriter(c), "%s %s → %s\n", verb, dash(id), from)
	if n := len(anySlice(out["rewritten"])); n > 0 {
		fmt.Fprintf(outWriter(c), "%s %d file(s)\n", rewrote, n)
	}
	if n := len(anySlice(out["connectors_rewritten"])); n > 0 {
		fmt.Fprintf(outWriter(c), "%s %d connector(s)\n", rewrote, n)
	}
	return nil
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func updateStickyPath(c *cli.Command, id, name, newPath string) error {
	if newPath == "" {
		return nil
	}
	st, err := stickyFor(c)
	if err != nil {
		return err
	}
	if st != nil && (st.WorkspaceID == id || equalFoldName(id, st.Name) || equalFoldName(name, st.Name) || st.WorkspaceID == name) {
		st.Path = newPath
		if err := st.Save(); err != nil {
			return err
		}
	}
	if link, ok := FindLinked(id); ok {
		link.Path = newPath
		return link.Save()
	}
	if name != "" {
		if link, ok := FindLinked(name); ok {
			link.Path = newPath
			return link.Save()
		}
	}
	return nil
}

func runWorkspaceRename(_ context.Context, c *cli.Command) error {
	id, name := c.Args().Get(0), c.Args().Get(1)
	if id == "" || name == "" {
		return fmt.Errorf("usage: trebi workspace rename <id> <new-name>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildWorkspaceRename(id, name), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "renamed %s → %s\n", id, name)
	return nil
}

func runWorkspaceHandleCheck(_ context.Context, c *cli.Command) error {
	handle := c.Args().Get(0)
	if handle == "" {
		return fmt.Errorf("usage: trebi workspace handle-check <handle> [--workspace <id>]")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	req := request.HTTPRequest{Method: "GET", Path: request.BuildWorkspaceHandleCheckPath(handle, c.String("workspace"))}
	if err := client.DoHTTP(req, &out); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	if out.OK {
		fmt.Fprintln(outWriter(c), "ok")
		return nil
	}
	fmt.Fprintln(outWriter(c), "not ok: "+out.Reason)
	return nil
}

func runWorkspaceSet(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		// -w <id|name> names the workspace, like the other workspace calls.
		id = workspaceFlag(c)
	}
	if id == "" {
		return fmt.Errorf("usage: trebi workspace set <id> [--name] [--git-enabled] [--contact] [--notify] [--email-allowlist]")
	}
	route := map[string]any{}
	for flag, key := range map[string]string{"contact": "contact", "notify": "notify", "email-allowlist": "email_allowlist"} {
		if c.IsSet(flag) {
			route[key] = patchValue(key, c.String(flag))
		}
	}
	if c.IsSet("handle") {
		route["handle"] = c.String("handle")
	}
	chains := map[string]any{}
	if c.IsSet("chains") {
		chains["enabled"] = c.Bool("chains")
	}
	for flag, key := range map[string]string{"chain-max-hops": "max_hops", "chain-max-turns": "max_turns", "chain-pair-per-hour": "pair_per_hour"} {
		if c.IsSet(flag) {
			chains[key] = c.Int(flag)
		}
	}
	if c.IsSet("chain-max-usd") {
		chains["max_usd"] = c.Float("chain-max-usd")
	}
	if len(chains) > 0 {
		route["chains"] = chains
	}
	if !c.IsSet("name") && !c.IsSet("git-enabled") && len(route) == 0 {
		return fmt.Errorf("set --name, --handle, --git-enabled, --contact, --notify, --email-allowlist, or a --chain flag")
	}
	var name *string
	var git *bool
	if c.IsSet("name") {
		n := c.String("name")
		name = &n
	}
	if c.IsSet("git-enabled") {
		g := c.Bool("git-enabled")
		git = &g
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildWorkspacePatch(id, name, git, route), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "updated %s\n", id)
	return nil
}

// workspaceEmailArg resolves the optional workspace argument: an explicit
// id/name, else the flag/sticky context.
func workspaceEmailArg(c *cli.Command) (string, error) {
	if id := c.Args().Get(0); id != "" {
		return id, nil
	}
	if id := workspaceFlag(c); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("workspace id/name required (or set sticky context: trebi workspace use)")
}

func runWorkspaceEmailShow(_ context.Context, c *cli.Command) error {
	id, err := workspaceEmailArg(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildWorkspaceEmailGetPath(id, c.Bool("reveal")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	fmt.Fprintf(w, "Address: %s\n", mapStr(body, "address"))
	fmt.Fprintf(w, "Masked:  %s\n", mapStr(body, "masked"))
	fmt.Fprintf(w, "Created: %s\n", mapStr(body, "created_at"))
	if r := mapStr(body, "rotated_at"); r != "" {
		fmt.Fprintf(w, "Rotated: %s\n", r)
	}
	return nil
}

func runWorkspaceEmailRotate(_ context.Context, c *cli.Command) error {
	id, err := workspaceEmailArg(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildWorkspaceEmailRotate(id, c.String("handle"), c.Bool("yes")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "rotated: %s\n", mapStr(body, "address"))
	return nil
}

func clearStickyIfWorkspace(c *cli.Command, id string) error {
	st, err := stickyFor(c)
	if err != nil {
		return err
	}
	if st == nil {
		return nil
	}
	if st.WorkspaceID == id || equalFoldName(id, st.Name) {
		return ClearStickyContext()
	}
	return nil
}

func runWorkspaceList(_ context.Context, c *cli.Command) error {
	sticky, err := stickyFor(c)
	if err != nil {
		return err
	}
	linked, err := linkedFor(c)
	if err != nil {
		return err
	}
	local, localErr := localClientFor(c)
	if localErr != nil {
		local = nil // the local daemon is optional; linked workspaces still render
	}
	rows := localWorkspaceRows(sticky, local, c.Int("limit"))
	rows = append(rows, linkedWorkspaceRows(sticky, linked)...)

	if c.Bool("json") {
		return encodeJSON(c, map[string]any{"workspaces": rows})
	}
	if len(rows) == 0 {
		fmt.Fprintln(outWriter(c), "(no workspaces; start local daemon or: trebi workspace add --url …)")
		return nil
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"*", "ID", "Name", "Path", "Origin"})
	for _, r := range rows {
		tbl.AppendRow(table.Row{r.Active, r.ID, dash(r.Name), dash(r.Path), r.Origin})
	}
	renderTable(tbl)
	return nil
}

// localWorkspaceRows lists daemon workspaces (empty when the daemon is offline).
func localWorkspaceRows(sticky *StickyContext, client *APIClient, limit int) []wsRow {
	activeID := ""
	if sticky != nil {
		activeID = sticky.WorkspaceID
	}
	if client == nil {
		return nil
	}
	var body struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := client.GetJSON(request.BuildWorkspacesListPath(limit), &body); err != nil {
		return nil
	}
	var rows []wsRow
	for _, ws := range body.Workspaces {
		id := mapStr(ws, "id")
		act := ""
		if id == activeID && (sticky == nil || !sticky.IsRemote()) {
			act = "*"
		}
		rows = append(rows, wsRow{
			ID: id, Name: mapStr(ws, "name"), Path: mapStr(ws, "path"),
			Origin: "local", Active: act,
		})
	}
	return rows
}

func linkedWorkspaceRows(sticky *StickyContext, linked []LinkedWorkspace) []wsRow {
	var rows []wsRow
	for _, w := range linked {
		act := ""
		if sticky != nil && sticky.IsRemote() && sticky.WorkspaceID == w.ID && sticky.URL == w.URL {
			act = "*"
		}
		rows = append(rows, wsRow{
			ID: w.ID, Name: w.Name, Path: w.Path,
			Origin: w.URL, Active: act,
		})
	}
	return rows
}

func runWorkspaceAdd(_ context.Context, c *cli.Command) error {
	remoteURL := strings.TrimSpace(c.String("url"))
	if remoteURL == "" {
		path := c.Args().Get(0)
		if path == "" && (!c.Bool("mkdir") || c.String("name") == "") {
			return fmt.Errorf("path required (or --create --name for pathless, or --url/--remote)")
		}
		return workspaceAddLocal(c, path)
	}
	return workspaceAddRemote(c, remoteURL)
}

func workspaceAddLocal(c *cli.Command, path string) error {
	client, err := localClientFor(c)
	if err != nil {
		return fmt.Errorf("local daemon: %w (is trebid running?)", err)
	}
	name := c.String("name")
	if name == "" && path != "" {
		name = filepath.Base(path)
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildWorkspaceAdd(path, name, c.String("handle"), c.Bool("mkdir")), &out); err != nil {
		return err
	}
	id := mapStr(out, "id")
	if id == "" {
		id = mapStr(out, "workspace_id")
	}
	dispName := mapStr(out, "name")
	if dispName == "" {
		dispName = name
	}
	if p := mapStr(out, "path"); p != "" {
		path = p
	}
	fmt.Fprintf(outWriter(c), "workspace %s path=%s\n", dash(id), path)
	if c.Bool("use") && id != "" {
		st := StickyContext{
			WorkspaceID: id, Name: dispName, Path: path,
		}
		if err := st.Save(); err != nil {
			return err
		}
		fmt.Fprintln(outWriter(c), "sticky context → local "+id)
	}
	return nil
}

func workspaceAddRemote(c *cli.Command, remoteURL string) error {
	remoteURL = strings.TrimRight(remoteURL, "/")
	client := remoteClient(remoteURL)
	if !c.Bool("skip-check") {
		var health map[string]any
		if err := client.GetJSON(request.BuildSystemHealthPath(), &health); err != nil {
			return fmt.Errorf("remote health check failed: %w", err)
		}
	}

	var id, name, path string
	var err error
	switch {
	case c.String("id") != "":
		// Link existing remote workspace.
		id = c.String("id")
		name, path, err = client.FindWorkspace(id, remoteURL)
		if err != nil {
			return err
		}
		if n := c.String("name"); n != "" {
			name = n
		}
	case c.NArg() >= 1 || (c.Bool("mkdir") && c.String("name") != ""):
		// Register path on remote host, then link (path optional with --create --name).
		id, name, path, err = registerRemoteWorkspace(c, client)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("remote add needs a path (register on host), --create --name, or --id (link existing)")
	}
	if id == "" {
		return fmt.Errorf("remote workspace id missing in response")
	}
	link := LinkedWorkspace{ID: id, Name: name, Path: path, URL: remoteURL}
	if err := link.Save(); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "linked workspace %s @ %s\n", id, remoteURL)
	// Remote add always becomes sticky so later commands use that API endpoint.
	st := link.Sticky()
	if err := st.Save(); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "sticky context → %s (%s)\n", id, remoteURL)
	return nil
}

// registerRemoteWorkspace registers the path argument on the remote host.
func registerRemoteWorkspace(c *cli.Command, client *APIClient) (id, name, path string, err error) {
	path = c.Args().Get(0)
	name = c.String("name")
	create := c.Bool("mkdir")
	if path == "" && (!create || name == "") {
		return "", "", "", fmt.Errorf("remote add needs a path (register on host) or --id (link existing)")
	}
	if name == "" && path != "" {
		name = filepath.Base(path)
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildWorkspaceAdd(path, name, c.String("handle"), create), &out); err != nil {
		return "", "", "", err
	}
	id = mapStr(out, "id")
	if id == "" {
		id = mapStr(out, "workspace_id")
	}
	if n := mapStr(out, "name"); n != "" {
		name = n
	}
	if p := mapStr(out, "path"); p != "" {
		path = p
	}
	return id, name, path, nil
}

func runWorkspaceUse(_ context.Context, c *cli.Command) error {
	key := c.Args().Get(0)
	if key == "" {
		return fmt.Errorf("usage: trebi workspace use <id|name>")
	}
	linked, err := linkedFor(c)
	if err != nil {
		return err
	}
	// Linked remote first.
	if link, ok := findLinked(linked, key); ok {
		st := link.Sticky()
		if err := st.Save(); err != nil {
			return err
		}
		fmt.Fprintf(outWriter(c), "sticky context → %s @ %s\n", link.ID, link.URL)
		return nil
	}
	// Local daemon workspace.
	client, err := localClientFor(c)
	if err != nil {
		return fmt.Errorf("workspace %q not linked and local daemon unavailable: %w", key, err)
	}
	var body struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := client.GetJSON(request.BuildWorkspacesListPath(0), &body); err != nil {
		return err
	}
	for _, ws := range body.Workspaces {
		id := mapStr(ws, "id")
		name := mapStr(ws, "name")
		if id == key || equalFoldName(key, name) {
			st := StickyContext{
				WorkspaceID: id,
				Name:        name,
				Path:        mapStr(ws, "path"),
			}
			if err := st.Save(); err != nil {
				return err
			}
			fmt.Fprintf(outWriter(c), "sticky context → local %s\n", id)
			return nil
		}
	}
	return fmt.Errorf("workspace %q not found (local or linked)", key)
}

func runWorkspaceCurrent(_ context.Context, c *cli.Command) error {
	st, err := stickyFor(c)
	if err != nil {
		return err
	}
	if st == nil {
		fmt.Fprintln(outWriter(c), "(no sticky workspace; trebi workspace use <id>)")
		return nil
	}
	if c.Bool("json") {
		return encodeJSON(c, st)
	}
	origin := "local"
	if st.IsRemote() {
		origin = st.URL
	}
	fmt.Fprintf(outWriter(c), "workspace_id: %s\n", st.WorkspaceID)
	if st.Name != "" {
		fmt.Fprintf(outWriter(c), "name: %s\n", st.Name)
	}
	if st.Path != "" {
		fmt.Fprintf(outWriter(c), "path: %s\n", st.Path)
	}
	fmt.Fprintf(outWriter(c), "origin: %s\n", origin)
	return nil
}

// resolveWorkspaceID picks the subcommand --workspace / sticky / global -w
// workspace, else the first on the active daemon.
func resolveWorkspaceID(c *cli.Command, client *APIClient) (string, error) {
	if ws := workspaceFlag(c); ws != "" {
		return ws, nil
	}
	var wss struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := client.GetJSON(request.BuildWorkspacesListPath(0), &wss); err != nil {
		return "", err
	}
	if len(wss.Workspaces) == 0 {
		return "", fmt.Errorf("no workspaces")
	}
	return mapStr(wss.Workspaces[0], "id"), nil
}

// machineArg reports whether arg names a machine of the account (id or
// name). A path wins when arg has a separator or starts with "." or "~".
func machineArg(c *cli.Command, arg string) (string, bool, error) {
	if strings.ContainsAny(arg, `/\`) || strings.HasPrefix(arg, ".") || strings.HasPrefix(arg, "~") {
		return "", false, nil
	}
	client, err := clientFor(c)
	if err != nil {
		return "", false, err
	}
	var body struct {
		Machines []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			IsSelf bool   `json:"is_self"`
		} `json:"machines"`
	}
	if err := client.GetJSON(request.BuildMachinesPath(), &body); err != nil {
		return "", false, err
	}
	for _, m := range body.Machines {
		if !m.IsSelf && (m.ID == arg || strings.EqualFold(m.Name, arg)) {
			return m.ID, true, nil
		}
	}
	return "", false, nil
}

// runWorkspaceMoveToMachine starts the move and follows it to the end.
func runWorkspaceMoveToMachine(ctx context.Context, c *cli.Command, id, machine string) error {
	confirm, err := confirmed(c, "moves workspace "+id+" with its history and secrets to machine "+machine+"; this machine keeps a read-only copy")
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var mv map[string]any
	if err := client.DoHTTP(request.BuildWorkspaceMoveToMachine(id, machine, confirm), &mv); err != nil {
		return err
	}
	ws, moveID := mapStr(mv, "workspace_id"), mapStr(mv, "id")
	phase := ""
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if p := mapStr(mv, "phase"); p != phase {
			phase = p
			if !c.Bool("json") {
				fmt.Fprintf(outWriter(c), "%s\n", p)
			}
		}
		if phase == "done" || phase == "failed" {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		if err := client.DoHTTP(request.BuildWorkspaceMoveGet(ws, moveID), &mv); err != nil {
			return err
		}
	}
	if c.Bool("json") {
		return encodeJSON(c, mv)
	}
	if phase == "failed" {
		return fmt.Errorf("move failed: %s", mapStr(mv, "error"))
	}
	fmt.Fprintf(outWriter(c), "moved %s to %s\n", id, mapStr(mv, "machine"))
	return nil
}
