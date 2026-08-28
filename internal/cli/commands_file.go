package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func fileCommand() *cli.Command {
	return &cli.Command{
		Name:        "file",
		Usage:       "workspace files (shared assets, assistant support files)",
		Subcommands: fileActionCommands("", runFileList, runFileGet, runFilePut, runFileRm),
	}
}

func jobFileSubcommand() *cli.Command {
	return &cli.Command{
		Name:        "file",
		Usage:       "files next to a job (scripts, templates)",
		Subcommands: fileActionCommands("job", runJobFileList, runJobFileGet, runJobFilePut, runJobFileRm),
	}
}

func skillFileSubcommand() *cli.Command {
	return &cli.Command{
		Name:        "file",
		Usage:       "files in a skill bundle (scripts)",
		Subcommands: fileActionCommands("skill", runSkillFileList, runSkillFileGet, runSkillFilePut, runSkillFileRm),
	}
}

func fileActionCommands(owner string, ls, get, put, rm cli.ActionFunc) []*cli.Command {
	listUsage, listArgs := "list files in the workspace", "[prefix]"
	noun, pathArgs := "a workspace file", "<path>"
	if owner != "" {
		listUsage, listArgs = "list files in the "+owner+" folder", "<"+owner+"> [prefix]"
		noun, pathArgs = "a "+owner+" file", "<"+owner+"> <path>"
	}
	return []*cli.Command{
		{
			Name: "ls", Usage: listUsage, ArgsUsage: listArgs,
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				&cli.IntFlag{Name: "limit", Usage: "page size"},
				&cli.StringFlag{Name: "cursor", Usage: "path of the last row from the previous page"},
			},
			Action: ls,
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
			Name: "rm", Usage: "delete " + noun, ArgsUsage: pathArgs,
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "if-match", Usage: "If-Match content hash"},
			},
			Action: rm,
		},
	}
}

func runFileList(c *cli.Context) error {
	client, wsID, err := fileClient(c)
	if err != nil {
		return err
	}
	return fileListAction(c, client, BuildFileListPath(wsID, c.Args().Get(0), c.String("cursor"), c.Int("limit")))
}

func runFileGet(c *cli.Context) error {
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

func runFilePut(c *cli.Context) error {
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

func runFileRm(c *cli.Context) error {
	rel, err := fileArg(c)
	if err != nil {
		return err
	}
	client, wsID, err := fileClient(c)
	if err != nil {
		return err
	}
	return fileRmAction(c, client, BuildFileDeletePath(wsID, rel))
}

func runJobFileList(c *cli.Context) error {
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

func runJobFileGet(c *cli.Context) error {
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

func runJobFilePut(c *cli.Context) error {
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

func runJobFileRm(c *cli.Context) error {
	name, rel, err := jobFileArgs(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return fileRmAction(c, client, BuildJobFileDeletePath(name, workspaceFor(c), rel))
}

func fileListAction(c *cli.Context, client *APIClient, path string) error {
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

func fileGetAction(c *cli.Context, client *APIClient, path string) error {
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

func filePutAction(c *cli.Context, client *APIClient, path string) error {
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

func fileRmAction(c *cli.Context, client *APIClient, path string) error {
	return client.DeleteIfMatch(path, c.String("if-match"))
}

func fileClient(c *cli.Context) (*APIClient, string, error) {
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

func fileArg(c *cli.Context) (string, error) {
	rel := c.Args().Get(0)
	if rel == "" {
		return "", fmt.Errorf("path required")
	}
	return rel, nil
}

func jobFileArgs(c *cli.Context) (name, rel string, err error) {
	return scopedFileArgs(c, "job")
}

func skillFileArgs(c *cli.Context) (name, rel string, err error) {
	return scopedFileArgs(c, "skill")
}

func scopedFileArgs(c *cli.Context, owner string) (name, rel string, err error) {
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

func runSkillFileList(c *cli.Context) error {
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

func runSkillFileGet(c *cli.Context) error {
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

func runSkillFilePut(c *cli.Context) error {
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

func runSkillFileRm(c *cli.Context) error {
	name, rel, err := skillFileArgs(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return fileRmAction(c, client, BuildSkillFileDeletePath(name, rel))
}

func readBytesOrStdin(path string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	return io.ReadAll(os.Stdin)
}
