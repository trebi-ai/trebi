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
		Name:  "file",
		Usage: "workspace files (scripts, templates, any path)",
		Subcommands: []*cli.Command{
			{
				Name:      "ls",
				Usage:     "list files in the workspace",
				ArgsUsage: "[prefix]",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
					&cli.IntFlag{Name: "limit", Usage: "page size"},
					&cli.StringFlag{Name: "cursor", Usage: "path of the last row from the previous page"},
				},
				Action: runFileList,
			},
			{
				Name:      "get",
				Usage:     "read a workspace file",
				ArgsUsage: "<path>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "out", Usage: "write bytes to a local file instead of stdout"},
					&cli.BoolFlag{Name: "stat", Usage: "JSON metadata only"},
				},
				Action: runFileGet,
			},
			{
				Name:      "put",
				Usage:     "write a workspace file (--from or stdin)",
				ArgsUsage: "<path>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "from", Usage: "local file to upload"},
					&cli.StringFlag{Name: "if-match", Usage: "If-Match content hash"},
				},
				Action: runFilePut,
			},
			{
				Name:      "rm",
				Usage:     "delete a workspace file",
				ArgsUsage: "<path>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "if-match", Usage: "If-Match content hash"},
				},
				Action: runFileRm,
			},
		},
	}
}

func runFileList(c *cli.Context) error {
	client, wsID, err := fileClient(c)
	if err != nil {
		return err
	}
	path := BuildFileListPath(wsID, c.Args().Get(0), c.String("cursor"), c.Int("limit"))
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

func runFileGet(c *cli.Context) error {
	rel, err := fileArg(c)
	if err != nil {
		return err
	}
	client, wsID, err := fileClient(c)
	if err != nil {
		return err
	}
	path := BuildFileGetPath(wsID, rel, c.Bool("stat"))
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

func runFilePut(c *cli.Context) error {
	rel, err := fileArg(c)
	if err != nil {
		return err
	}
	client, wsID, err := fileClient(c)
	if err != nil {
		return err
	}
	data, err := readBytesOrStdin(c.String("from"))
	if err != nil {
		return err
	}
	_, raw, err := client.PutRaw(BuildFilePutPath(wsID, rel), bytes.NewReader(data), "", c.String("if-match"))
	if err != nil {
		return err
	}
	fmt.Fprintln(outWriter(c), string(bytes.TrimSpace(raw)))
	return nil
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
	return client.DeleteIfMatch(BuildFileDeletePath(wsID, rel), c.String("if-match"))
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

func readBytesOrStdin(path string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	return io.ReadAll(os.Stdin)
}
