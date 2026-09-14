package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
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
			Name: "list", Usage: listUsage, ArgsUsage: listArgs,
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				&cli.IntFlag{Name: "limit", Usage: "page size"},
				&cli.StringFlag{Name: "cursor", Usage: "path of the last row from the previous page"},
				&cli.BoolFlag{Name: "recursive", Usage: "add a row per folder, so the tree shape is visible"},
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
			Name: "delete", Aliases: []string{"remove"}, Usage: "delete " + noun, ArgsUsage: pathArgs,
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "if-match", Usage: "If-Match content hash"},
				forceFlag("required for a workspace file; deletes it"),
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
	return fileListAction(c, client, request.BuildFileListPath(wsID, c.Args().Get(0), c.String("cursor"), c.Int("limit"), c.Bool("recursive")))
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
	return fileGetAction(c, client, request.BuildFileGetPath(wsID, rel, c.Bool("stat")))
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
	return filePutAction(c, client, request.BuildFilePut(wsID, rel, nil, "", c.String("if-match")))
}

func runFileRemove(_ context.Context, c *cli.Command) error {
	rel, err := fileArg(c)
	if err != nil {
		return err
	}
	confirm, err := confirmed(c, "deletes file "+rel)
	if err != nil {
		return err
	}
	client, wsID, err := fileClient(c)
	if err != nil {
		return err
	}
	return fileRemoveAction(c, client, request.BuildFileDelete(wsID, rel, c.String("if-match"), confirm))
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
	return fileListAction(c, client, request.BuildJobFileListPath(name, workspaceFor(c), c.Args().Get(1), c.String("cursor"), c.Int("limit"), c.Bool("recursive")))
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
	return fileGetAction(c, client, request.BuildJobFileGetPath(name, workspaceFor(c), rel, c.Bool("stat")))
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
	return filePutAction(c, client, request.BuildJobFilePut(name, workspaceFor(c), rel, nil, "", c.String("if-match")))
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
	return fileRemoveAction(c, client, request.BuildJobFileDelete(name, workspaceFor(c), rel, c.String("if-match")))
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
		return encodeJSON(c, body)
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
		return encodeJSON(c, body)
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

func filePutAction(c *cli.Command, client *APIClient, req request.HTTPRequest) error {
	data, err := readBytesOrStdin(c.String("from"))
	if err != nil {
		return err
	}
	req.Raw = data
	var out json.RawMessage
	if err := client.DoHTTP(req, &out); err != nil {
		return err
	}
	fmt.Fprintln(outWriter(c), string(bytes.TrimSpace(out)))
	return nil
}

func fileRemoveAction(_ *cli.Command, client *APIClient, req request.HTTPRequest) error {
	return client.DoHTTP(req, nil)
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
	return fileListAction(c, client, request.BuildSkillFileListPath(name, c.Args().Get(1), c.String("cursor"), c.Int("limit"), c.Bool("recursive")))
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
	return fileGetAction(c, client, request.BuildSkillFileGetPath(name, rel, c.Bool("stat")))
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
	return filePutAction(c, client, request.BuildSkillFilePut(name, rel, nil, "", c.String("if-match")))
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
	return fileRemoveAction(c, client, request.BuildSkillFileDelete(name, rel, c.String("if-match")))
}

func readBytesOrStdin(path string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	return io.ReadAll(os.Stdin)
}

// memoryOwner is one MEMORY.md ledger CLI (job, agent, or assistant).
// resolve and the Build* closures stay owner-specific so job keeps
// ?workspace= paths and agent/assistant keep /workspaces/{id} paths.
type memoryOwner struct {
	noun    string
	resolve func(c *cli.Command) (*APIClient, string, string, error)
	getPath func(name, ws string) string
	put     func(name, ws, content, ifMatch string) request.HTTPRequest
	add     func(name, ws, date, text, ifMatch string) request.HTTPRequest
	del     func(name, ws, entryID, ifMatch string) request.HTTPRequest
	clear   func(name, ws, ifMatch string, confirm bool) request.HTTPRequest
}

func (o memoryOwner) named() bool  { return o.noun != "assistant" }
func (o memoryOwner) wsFlag() bool { return o.noun == "agent" }

func (o memoryOwner) flags(extra ...cli.Flag) []cli.Flag {
	if o.wsFlag() {
		return append([]cli.Flag{&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"}}, extra...)
	}
	return extra
}

func memorySubcommands(o memoryOwner) *cli.Command {
	nameArgs, entryArgs := "", "<entry-id>"
	if o.named() {
		nameArgs = "<name>"
		entryArgs = "<name> <entry-id>"
	}
	return &cli.Command{
		Name:  "memory",
		Usage: o.noun + " MEMORY.md ledger",
		Commands: []*cli.Command{
			{
				Name: "show", Usage: "print MEMORY.md", ArgsUsage: nameArgs,
				Flags:  o.flags(&cli.BoolFlag{Name: "json", Usage: "emit JSON"}),
				Action: o.runShow,
			},
			{
				Name: "add", Usage: "append a ledger entry", ArgsUsage: nameArgs,
				Flags: o.flags(
					&cli.StringFlag{Name: "text", Usage: "entry text", Required: true},
					&cli.StringFlag{Name: "date", Usage: "YYYY-MM-DD (default today)"},
					ifMatchFlag(),
				),
				Action: o.runAdd,
			},
			{
				Name: "delete", Aliases: []string{"remove"}, Usage: "delete a ledger entry", ArgsUsage: entryArgs,
				Flags:  o.flags(ifMatchFlag()),
				Action: o.runRemove,
			},
			{
				Name: "set", Usage: "replace MEMORY.md (--file or stdin)", ArgsUsage: nameArgs,
				Flags:  o.flags(&cli.StringFlag{Name: "file", Usage: "path to MEMORY.md"}, ifMatchFlag()),
				Action: o.runSet,
			},
			{
				Name: "clear", Usage: "delete MEMORY.md (destroys the ledger)", ArgsUsage: nameArgs,
				Flags:  o.flags(forceFlag("required; destroys the ledger"), ifMatchFlag()),
				Action: o.runClear,
			},
		},
	}
}

func (o memoryOwner) runShow(_ context.Context, c *cli.Command) error {
	client, name, ws, err := o.resolve(c)
	if err != nil {
		return err
	}
	if err := o.requireName(name, fmt.Sprintf("usage: trebi %s memory show <name>", o.noun)); err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(o.getPath(name, ws), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprint(outWriter(c), mapStr(body, "content"))
	return nil
}

func (o memoryOwner) runAdd(_ context.Context, c *cli.Command) error {
	client, name, ws, err := o.resolve(c)
	if err != nil {
		return err
	}
	if err := o.requireName(name, fmt.Sprintf("usage: trebi %s memory add <name> --text <text>", o.noun)); err != nil {
		return err
	}
	return client.DoHTTP(o.add(name, ws, c.String("date"), c.String("text"), c.String("if-match")), nil)
}

func (o memoryOwner) runRemove(_ context.Context, c *cli.Command) error {
	client, name, ws, err := o.resolve(c)
	if err != nil {
		return err
	}
	eid := c.Args().Get(0)
	if o.named() {
		eid = c.Args().Get(1)
	}
	if eid == "" || (o.named() && name == "") {
		if !o.named() {
			return fmt.Errorf("usage: trebi assistant memory remove <entry-id>")
		}
		verb := "remove"
		if o.noun == "agent" {
			verb = "delete"
		}
		return fmt.Errorf("usage: trebi %s memory %s <name> <entry-id>", o.noun, verb)
	}
	return client.DoHTTP(o.del(name, ws, eid, c.String("if-match")), nil)
}

func (o memoryOwner) runSet(_ context.Context, c *cli.Command) error {
	client, name, ws, err := o.resolve(c)
	if err != nil {
		return err
	}
	if err := o.requireName(name, fmt.Sprintf("usage: trebi %s memory set <name> --file <path>", o.noun)); err != nil {
		return err
	}
	content, err := readFileOrStdin(c.String("file"))
	if err != nil {
		return err
	}
	return client.DoHTTP(o.put(name, ws, content, c.String("if-match")), nil)
}

func (o memoryOwner) runClear(_ context.Context, c *cli.Command) error {
	client, name, ws, err := o.resolve(c)
	if err != nil {
		return err
	}
	if err := o.requireName(name, fmt.Sprintf("usage: trebi %s memory clear <name>", o.noun)); err != nil {
		return err
	}
	confirm, err := confirmed(c, o.clearWhat(name))
	if err != nil {
		return err
	}
	if err := client.DoHTTP(o.clear(name, ws, c.String("if-match"), confirm), nil); err != nil {
		return err
	}
	fmt.Fprint(outWriter(c), o.clearedMsg(name, ws))
	return nil
}

func (o memoryOwner) requireName(name, usage string) error {
	if !o.named() || name != "" {
		return nil
	}
	if o.noun == "agent" {
		return fmt.Errorf("name required")
	}
	return fmt.Errorf("%s", usage)
}

func (o memoryOwner) clearWhat(name string) string {
	switch o.noun {
	case "agent":
		return "destroys the memory ledger of agent " + name
	case "assistant":
		return "destroys the assistant memory ledger"
	default:
		return "destroys the memory ledger for " + name
	}
}

func (o memoryOwner) clearedMsg(name, ws string) string {
	switch o.noun {
	case "agent":
		return fmt.Sprintf("cleared memory of %s\n", name)
	case "assistant":
		return fmt.Sprintf("cleared assistant memory for %s\n", ws)
	default:
		return fmt.Sprintf("cleared memory for %s\n", name)
	}
}
