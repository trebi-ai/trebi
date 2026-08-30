package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runWorkspaceList,
			},
			{
				Name:      "add",
				Usage:     "register a local folder, or link a remote workspace (--url/--remote)",
				ArgsUsage: "[path]",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "name", Usage: "display name (default: basename of path)"},
					&cli.BoolFlag{Name: "create", Usage: "mkdir the folder if missing; path optional with --name"},
					&cli.StringFlag{Name: "url", Aliases: []string{"remote"}, Usage: "remote trebid base URL"},
					&cli.StringFlag{Name: "token", Usage: "remote bearer token"},
					&cli.BoolFlag{Name: "token-stdin", Usage: "read remote token from stdin"},
					&cli.StringFlag{Name: "id", Usage: "existing remote workspace id to link (with --url)"},
					&cli.BoolFlag{Name: "use", Usage: "set sticky context to this workspace after add"},
					&cli.BoolFlag{Name: "skip-check", Usage: "skip remote health check"},
				},
				Action: runWorkspaceAdd,
			},
			{
				Name:      "use",
				Usage:     "set sticky workspace context (local id/name or linked remote)",
				ArgsUsage: "<id|name>",
				Action:    runWorkspaceUse,
			},
			{
				Name:    "current",
				Aliases: []string{"show"},
				Usage:   "print sticky workspace context",
				Flags:   []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:  runWorkspaceCurrent,
			},
			{
				Name:      "remove",
				Usage:     "delete a workspace and its folder",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "force", Aliases: []string{"f"}, Usage: "required; deletes the workspace folder"}},
				Action:    runWorkspaceRemove,
			},
			{
				Name:      "rename",
				Usage:     "rename a workspace",
				ArgsUsage: "<id> <new-name>",
				Action:    runWorkspaceRename,
			},
			{
				Name:      "set",
				Usage:     "update workspace name and/or git_enabled",
				ArgsUsage: "<id>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "name", Usage: "display name"},
					&cli.BoolFlag{Name: "git-enabled", Usage: "write git_enabled to trebi.yaml"},
				},
				Action: runWorkspaceSet,
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

func runWorkspaceRemove(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi workspace remove <id>")
	}
	if !c.Bool("force") {
		return fmt.Errorf("workspace remove deletes the workspace folder for %s. Confirm with the user first, then re-run with --force", id)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildWorkspaceDelete(id), nil); err != nil {
		return err
	}
	if err := clearStickyIfWorkspace(c, id); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "removed %s\n", id)
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

func runWorkspaceSet(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi workspace set <id> [--name] [--git-enabled]")
	}
	if !c.IsSet("name") && !c.IsSet("git-enabled") {
		return fmt.Errorf("set --name and/or --git-enabled")
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
	if err := client.DoHTTP(request.BuildWorkspacePatch(id, name, git), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "updated %s\n", id)
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
	rows := localWorkspaceRows(sticky, local)
	rows = append(rows, linkedWorkspaceRows(sticky, linked)...)

	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"workspaces": rows})
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
func localWorkspaceRows(sticky *StickyContext, client *APIClient) []wsRow {
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
	if err := client.GetJSON(request.BuildWorkspacesListPath(), &body); err != nil {
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
		if path == "" && (!c.Bool("create") || c.String("name") == "") {
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
	if err := client.DoHTTP(request.BuildWorkspaceAdd(path, name, c.Bool("create")), &out); err != nil {
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
		if err := SaveStickyContext(StickyContext{
			WorkspaceID: id, Name: dispName, Path: path,
		}); err != nil {
			return err
		}
		fmt.Fprintln(outWriter(c), "sticky context → local "+id)
	}
	return nil
}

func workspaceAddRemote(c *cli.Command, remoteURL string) error {
	token, err := remoteAddToken(c)
	if err != nil {
		return err
	}
	remoteURL = strings.TrimRight(remoteURL, "/")
	client := remoteClient(remoteURL, token)
	if !c.Bool("skip-check") {
		var health map[string]any
		if err := client.GetJSON(request.BuildSystemHealthPath(), &health); err != nil {
			return fmt.Errorf("remote health check failed: %w", err)
		}
	}

	var id, name, path string
	switch {
	case c.String("id") != "":
		// Link existing remote workspace.
		id = c.String("id")
		name, path, err = findRemoteWorkspace(client, id, remoteURL)
		if err != nil {
			return err
		}
		if n := c.String("name"); n != "" {
			name = n
		}
	case c.NArg() >= 1 || (c.Bool("create") && c.String("name") != ""):
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
	link := LinkedWorkspace{ID: id, Name: name, Path: path, URL: remoteURL, Token: token}
	if err := SaveLinkedWorkspace(link); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "linked workspace %s @ %s\n", id, remoteURL)
	// Remote add always becomes sticky so later commands use that API endpoint.
	if err := SaveStickyContext(StickyFromLinked(link)); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "sticky context → %s (%s)\n", id, remoteURL)
	return nil
}

// remoteAddToken reads the remote token from --token or stdin.
func remoteAddToken(c *cli.Command) (string, error) {
	token := c.String("token")
	if c.Bool("token-stdin") {
		line, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading token from stdin: %w", err)
		}
		token = strings.TrimSpace(string(line))
	}
	if token == "" {
		return "", fmt.Errorf("remote token required (--token or --token-stdin); on host: trebi service token show")
	}
	return token, nil
}

// findRemoteWorkspace resolves name and path for an existing remote workspace id.
func findRemoteWorkspace(client *APIClient, id, remoteURL string) (name, path string, err error) {
	var body struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := client.GetJSON(request.BuildWorkspacesListPath(), &body); err != nil {
		return "", "", err
	}
	for _, ws := range body.Workspaces {
		if mapStr(ws, "id") == id {
			return mapStr(ws, "name"), mapStr(ws, "path"), nil
		}
	}
	return "", "", fmt.Errorf("workspace id %q not found on %s", id, remoteURL)
}

// registerRemoteWorkspace registers the path argument on the remote host.
func registerRemoteWorkspace(c *cli.Command, client *APIClient) (id, name, path string, err error) {
	path = c.Args().Get(0)
	name = c.String("name")
	create := c.Bool("create")
	if path == "" && (!create || name == "") {
		return "", "", "", fmt.Errorf("remote add needs a path (register on host) or --id (link existing)")
	}
	if name == "" && path != "" {
		name = filepath.Base(path)
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildWorkspaceAdd(path, name, create), &out); err != nil {
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
		if err := SaveStickyContext(StickyFromLinked(link)); err != nil {
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
	if err := client.GetJSON(request.BuildWorkspacesListPath(), &body); err != nil {
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
			if err := SaveStickyContext(st); err != nil {
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
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(st)
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

// resolveWorkspaceID picks sticky / -w workspace, else the first on the active daemon.
func resolveWorkspaceID(c *cli.Command, client *APIClient) (string, error) {
	if ws := workspaceFor(c); ws != "" {
		return ws, nil
	}
	var wss struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := client.GetJSON(request.BuildWorkspacesListPath(), &wss); err != nil {
		return "", err
	}
	if len(wss.Workspaces) == 0 {
		return "", fmt.Errorf("no workspaces")
	}
	return mapStr(wss.Workspaces[0], "id"), nil
}
