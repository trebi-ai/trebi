package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v3"
)

func fileCommand() *cli.Command {
	return &cli.Command{
		Name:     "file",
		Usage:    "workspace files (shared assets, assistant support files)",
		Commands: fileActionCommands("", runFileList, runFileGet, runFilePut, runFileRemove),
	}
}

func jobFileSubcommand() *cli.Command {
	return &cli.Command{
		Name:     "file",
		Usage:    "files next to a job (scripts, templates)",
		Commands: fileActionCommands("job", runJobFileList, runJobFileGet, runJobFilePut, runJobFileRemove),
	}
}

func skillFileSubcommand() *cli.Command {
	return &cli.Command{
		Name:     "file",
		Usage:    "files in a skill bundle (scripts)",
		Commands: fileActionCommands("skill", runSkillFileList, runSkillFileGet, runSkillFilePut, runSkillFileRemove),
	}
}

func fileActionCommands(owner string, list, get, put, remove cli.ActionFunc) []*cli.Command {
	listUsage, listArgs := "list files in the workspace", "[prefix]"
	noun, pathArgs := "a workspace file", "<path>"
	if owner != "" {
		listUsage, listArgs = "list files in the "+owner+" folder", "<"+owner+"> [prefix]"
		noun, pathArgs = "a "+owner+" file", "<"+owner+"> <path>"
	}
	return []*cli.Command{
		{
			Name: "list", Aliases: []string{"ls"}, Usage: listUsage, ArgsUsage: listArgs,
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				&cli.IntFlag{Name: "limit", Usage: "page size"},
				&cli.StringFlag{Name: "cursor", Usage: "path of the last row from the previous page"},
			},
			Action: list,
		},
		{
			Name: "get", Usage: "read " + noun, ArgsUsage: pathArgs,
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "out", Usage: "write bytes to a local file instead of stdout"},
				&cli.BoolFlag{Name: "stat", Usage: "JSON metadata only"},
			},
			Action: get,
		},
		{
			Name: "put", Usage: "write " + noun + " (--from or stdin)", ArgsUsage: pathArgs,
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "from", Usage: "local file to upload"},
				&cli.StringFlag{Name: "if-match", Usage: "If-Match content hash"},
			},
			Action: put,
		},
		{
			Name: "remove", Aliases: []string{"rm"}, Usage: "delete " + noun, ArgsUsage: pathArgs,
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "if-match", Usage: "If-Match content hash"},
			},
			Action: remove,
		},
	}
}

func runFileList(_ context.Context, c *cli.Command) error {
	client, wsID, err := fileClient(c)
	if err != nil {
		return err
	}
	return fileListAction(c, client, BuildFileListPath(wsID, c.Args().Get(0), c.String("cursor"), c.Int("limit")))
}

func runFileGet(_ context.Context, c *cli.Command) error {
	rel, err := fileArg(c)
	if err != nil {
		return err
	}
	client, wsID, err := fileClient(c)
	if err != nil {
		return err
	}
	return fileGetAction(c, client, BuildFileGetPath(wsID, rel, c.Bool("stat")))
}

func runFilePut(_ context.Context, c *cli.Command) error {
	rel, err := fileArg(c)
	if err != nil {
		return err
	}
	client, wsID, err := fileClient(c)
	if err != nil {
		return err
	}
	return filePutAction(c, client, BuildFilePutPath(wsID, rel))
}

func runFileRemove(_ context.Context, c *cli.Command) error {
	rel, err := fileArg(c)
	if err != nil {
		return err
	}
	client, wsID, err := fileClient(c)
	if err != nil {
		return err
	}
	return fileRemoveAction(c, client, BuildFileDeletePath(wsID, rel))
}

func runJobFileList(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("job name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return fileListAction(c, client, BuildJobFileListPath(name, workspaceFor(c), c.Args().Get(1), c.String("cursor"), c.Int("limit")))
}

func runJobFileGet(_ context.Context, c *cli.Command) error {
	name, rel, err := jobFileArgs(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return fileGetAction(c, client, BuildJobFileGetPath(name, workspaceFor(c), rel, c.Bool("stat")))
}

func runJobFilePut(_ context.Context, c *cli.Command) error {
	name, rel, err := jobFileArgs(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return filePutAction(c, client, BuildJobFilePutPath(name, workspaceFor(c), rel))
}

func runJobFileRemove(_ context.Context, c *cli.Command) error {
	name, rel, err := jobFileArgs(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return fileRemoveAction(c, client, BuildJobFileDeletePath(name, workspaceFor(c), rel))
}

func fileListAction(c *cli.Command, client *APIClient, path string) error {
	var body struct {
		Files      []map[string]any `json:"files"`
		NextCursor string           `json:"next_cursor"`
		Truncated  bool             `json:"truncated"`
	}
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	tbl := newTable(outWriter(c), []string{"Path", "Size", "Mtime"})
	for _, f := range body.Files {
		tbl.AppendRow(table.Row{mapStr(f, "path"), f["size_bytes"], mapStr(f, "mtime")})
	}
	renderTable(tbl)
	return nil
}

func fileGetAction(c *cli.Command, client *APIClient, path string) error {
	if c.Bool("stat") {
		var body map[string]any
		if err := client.GetJSON(path, &body); err != nil {
			return err
		}
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	raw, err := client.GetRaw(path)
	if err != nil {
		return err
	}
	if out := c.String("out"); out != "" {
		return os.WriteFile(out, raw, 0o644)
	}
	_, err = outWriter(c).Write(raw)
	return err
}

func filePutAction(c *cli.Command, client *APIClient, path string) error {
	data, err := readBytesOrStdin(c.String("from"))
	if err != nil {
		return err
	}
	_, raw, err := client.PutRaw(path, bytes.NewReader(data), "", c.String("if-match"))
	if err != nil {
		return err
	}
	fmt.Fprintln(outWriter(c), string(bytes.TrimSpace(raw)))
	return nil
}

func fileRemoveAction(c *cli.Command, client *APIClient, path string) error {
	return client.DeleteIfMatch(path, c.String("if-match"))
}

func fileClient(c *cli.Command) (*APIClient, string, error) {
	client, err := clientFor(c)
	if err != nil {
		return nil, "", err
	}
	wsID, err := resolveWorkspaceID(c, client)
	if err != nil {
		return nil, "", err
	}
	if wsID == "" {
		return nil, "", fmt.Errorf("workspace required (--workspace or trebi workspace use)")
	}
	return client, wsID, nil
}

func fileArg(c *cli.Command) (string, error) {
	rel := c.Args().Get(0)
	if rel == "" {
		return "", fmt.Errorf("path required")
	}
	return rel, nil
}

func jobFileArgs(c *cli.Command) (name, rel string, err error) {
	return scopedFileArgs(c, "job")
}

func skillFileArgs(c *cli.Command) (name, rel string, err error) {
	return scopedFileArgs(c, "skill")
}

func scopedFileArgs(c *cli.Command, owner string) (name, rel string, err error) {
	name = c.Args().Get(0)
	rel = c.Args().Get(1)
	if name == "" {
		return "", "", fmt.Errorf("%s name required", owner)
	}
	if rel == "" {
		return "", "", fmt.Errorf("path required")
	}
	return name, rel, nil
}

func runSkillFileList(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("skill name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return fileListAction(c, client, BuildSkillFileListPath(name, c.Args().Get(1), c.String("cursor"), c.Int("limit")))
}

func runSkillFileGet(_ context.Context, c *cli.Command) error {
	name, rel, err := skillFileArgs(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return fileGetAction(c, client, BuildSkillFileGetPath(name, rel, c.Bool("stat")))
}

func runSkillFilePut(_ context.Context, c *cli.Command) error {
	name, rel, err := skillFileArgs(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return filePutAction(c, client, BuildSkillFilePutPath(name, rel))
}

func runSkillFileRemove(_ context.Context, c *cli.Command) error {
	name, rel, err := skillFileArgs(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return fileRemoveAction(c, client, BuildSkillFileDeletePath(name, rel))
}

func readBytesOrStdin(path string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	return io.ReadAll(os.Stdin)
}
