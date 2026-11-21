package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/memory"
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
		{
			Name: "mkdir", Usage: "make an empty folder", ArgsUsage: pathArgs,
			Action: fileMkdirAction(owner),
		},
		{
			Name: "mv", Aliases: []string{"move"}, Usage: "rename or move a file or a folder", ArgsUsage: ownerArgs(owner, "<from> <to>"),
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "overwrite", Usage: "replace a file at the new path"},
				&cli.StringFlag{Name: "if-match", Usage: "If-Match content hash of the file"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: fileMoveAction(owner),
		},
		{
			Name: "rmdir", Usage: "delete a folder and all files in it", ArgsUsage: pathArgs,
			Flags:  []cli.Flag{forceFlag("required; deletes the folder and its files")},
			Action: fileRmdirAction(owner),
		},
	}
}

// ownerArgs prefixes args with the owner name argument of a job or skill.
func ownerArgs(owner, args string) string {
	if owner == "" {
		return args
	}
	return "<" + owner + "> " + args
}

// fileOwnerArgs resolves the client and the file root of one owner: the
// workspace, or the job or skill that the first argument names. It returns
// the args after the owner name.
func fileOwnerArgs(c *cli.Command, owner string) (client *APIClient, root request.FileRoot, args []string, err error) {
	if owner == "" {
		var ws string
		client, ws, err = fileClient(c)
		return client, request.WorkspaceFiles(ws), c.Args().Slice(), err
	}
	args = c.Args().Slice()
	if len(args) == 0 || args[0] == "" {
		return nil, request.FileRoot{}, nil, fmt.Errorf("%s name required", owner)
	}
	client, err = clientFor(c)
	if err != nil {
		return nil, request.FileRoot{}, nil, err
	}
	if owner == "job" {
		return client, request.JobFiles(args[0], workspaceFor(c)), args[1:], nil
	}
	return client, request.SkillFiles(skillFileRef(c, args[0])), args[1:], nil
}

func fileMkdirAction(owner string) cli.ActionFunc {
	return func(_ context.Context, c *cli.Command) error {
		client, root, args, err := fileOwnerArgs(c, owner)
		if err != nil {
			return err
		}
		if len(args) == 0 || args[0] == "" {
			return fmt.Errorf("path required")
		}
		if err := client.DoHTTP(root.FolderCreate(args[0]), nil); err != nil {
			return err
		}
		fmt.Fprintf(outWriter(c), "made folder %s\n", args[0])
		return nil
	}
}

func fileMoveAction(owner string) cli.ActionFunc {
	return func(_ context.Context, c *cli.Command) error {
		client, root, args, err := fileOwnerArgs(c, owner)
		if err != nil {
			return err
		}
		if len(args) < 2 || args[0] == "" || args[1] == "" {
			return fmt.Errorf("from and to paths required")
		}
		m := request.FileMove{From: args[0], To: args[1], Overwrite: c.Bool("overwrite"), IfMatch: c.String("if-match")}
		var out map[string]any
		if err := client.DoHTTP(root.Move(m), &out); err != nil {
			return err
		}
		if c.Bool("json") {
			return encodeJSON(c, out)
		}
		fmt.Fprintf(outWriter(c), "moved %s to %s\n", args[0], mapStr(out, "path"))
		return nil
	}
}

func fileRmdirAction(owner string) cli.ActionFunc {
	return func(_ context.Context, c *cli.Command) error {
		client, root, args, err := fileOwnerArgs(c, owner)
		if err != nil {
			return err
		}
		if len(args) == 0 || args[0] == "" {
			return fmt.Errorf("path required")
		}
		confirm, err := confirmed(c, "deletes folder "+args[0]+" and all files in it")
		if err != nil {
			return err
		}
		var out struct {
			FilesRemoved int `json:"files_removed"`
		}
		if err := client.DoHTTP(root.FolderDelete(args[0], confirm), &out); err != nil {
			return err
		}
		fmt.Fprintf(outWriter(c), "deleted folder %s and %d files\n", args[0], out.FilesRemoved)
		return nil
	}
}

func runFileList(_ context.Context, c *cli.Command) error {
	client, wsID, err := fileClient(c)
	if err != nil {
		return err
	}
	return fileListAction(c, client, request.WorkspaceFiles(wsID).ListPath(c.Args().Get(0), c.String("cursor"), c.Int("limit"), c.Bool("recursive")))
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
	return fileGetAction(c, client, request.WorkspaceFiles(wsID).GetPath(rel, c.Bool("stat")))
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
	return filePutAction(c, client, request.WorkspaceFiles(wsID).Put(rel, nil, "", c.String("if-match")))
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
	return fileRemoveAction(c, client, request.WorkspaceFiles(wsID).Delete(rel, c.String("if-match"), confirm))
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
	return fileListAction(c, client, request.JobFiles(name, workspaceFor(c)).ListPath(c.Args().Get(1), c.String("cursor"), c.Int("limit"), c.Bool("recursive")))
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
	return fileGetAction(c, client, request.JobFiles(name, workspaceFor(c)).GetPath(rel, c.Bool("stat")))
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
	return filePutAction(c, client, request.JobFiles(name, workspaceFor(c)).Put(rel, nil, "", c.String("if-match")))
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
	return fileRemoveAction(c, client, request.JobFiles(name, workspaceFor(c)).Delete(rel, c.String("if-match"), false))
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

// skillFileRef is the skill of a skill file command, with the --scope and
// --owner flags of the skill command.
func skillFileRef(c *cli.Command, name string) request.SkillRef {
	return request.SkillRef{Name: name, Scope: c.String("scope"), Owner: c.String("owner"), Workspace: c.String("workspace")}
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
	return fileListAction(c, client, request.SkillFiles(skillFileRef(c, name)).ListPath(c.Args().Get(1), c.String("cursor"), c.Int("limit"), c.Bool("recursive")))
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
	return fileGetAction(c, client, request.SkillFiles(skillFileRef(c, name)).GetPath(rel, c.Bool("stat")))
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
	return filePutAction(c, client, request.SkillFiles(skillFileRef(c, name)).Put(rel, nil, "", c.String("if-match")))
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
	return fileRemoveAction(c, client, request.SkillFiles(skillFileRef(c, name)).Delete(rel, c.String("if-match"), false))
}

func readBytesOrStdin(path string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	return io.ReadAll(os.Stdin)
}

// memoryOwner is one structured MEMORY.md CLI (job, agent, or assistant).
// resolve and root stay owner-specific so job keeps ?workspace= paths and
// agent and assistant keep /workspaces/{id} paths.
type memoryOwner struct {
	noun    string
	resolve func(c *cli.Command) (*APIClient, string, string, error)
	root    func(name, ws string) request.MemoryRoot
}

func (o memoryOwner) named() bool  { return o.noun != "assistant" }
func (o memoryOwner) wsFlag() bool { return o.noun == "agent" }

func (o memoryOwner) flags(extra ...cli.Flag) []cli.Flag {
	if o.wsFlag() {
		return append([]cli.Flag{&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"}}, extra...)
	}
	return extra
}

// entryFlags are the shared flags of lesson add and log add.
func (o memoryOwner) entryFlags() []cli.Flag {
	return o.flags(
		&cli.StringFlag{Name: "text", Usage: "entry text", Required: true},
		&cli.StringFlag{Name: "at", Usage: `"YYYY-MM-DD HH:MM" or "YYYY-MM-DD" (default now)`},
	)
}

func memorySubcommands(o memoryOwner) *cli.Command {
	nameArgs := ""
	if o.named() {
		nameArgs = "<name>"
	}
	return &cli.Command{
		Name:  "memory",
		Usage: o.noun + " structured MEMORY.md",
		Commands: []*cli.Command{
			{
				Name: "show", Usage: "print the memory summary (or the raw file)", ArgsUsage: nameArgs,
				Flags: o.flags(
					&cli.BoolFlag{Name: "json", Usage: "emit the full info payload"},
					&cli.BoolFlag{Name: "raw", Usage: "print MEMORY.md as stored"},
				),
				Action: o.runShow,
			},
			{
				Name: "validate", Usage: "check MEMORY.md against the rules", ArgsUsage: nameArgs,
				Flags:  o.flags(&cli.BoolFlag{Name: "json", Usage: "emit JSON"}),
				Action: o.runValidate,
			},
			{
				Name: "set-context", Usage: "replace the Context section (--body FILE or stdin)", ArgsUsage: nameArgs,
				Flags:  o.flags(bodyFlag()),
				Action: o.runSectionPut("context"),
			},
			{
				Name: "lesson", Usage: "lesson entries (never pruned by age)",
				Commands: []*cli.Command{
					{
						Name: "add", Usage: "append a lesson", ArgsUsage: nameArgs,
						Flags: o.entryFlags(), Action: o.runEntryAdd("lessons"),
					},
					{
						Name: "delete", Usage: "remove a lesson by id", ArgsUsage: o.entryArgs(),
						Flags: o.flags(), Action: o.runEntryDelete("lessons"),
					},
				},
			},
			{
				Name: "log", Usage: "log entries (capped; prune the oldest)",
				Commands: []*cli.Command{
					{
						Name: "add", Usage: "append a log entry", ArgsUsage: nameArgs,
						Flags: o.entryFlags(), Action: o.runEntryAdd("log"),
					},
					{
						Name: "delete", Usage: "remove a log entry by id", ArgsUsage: o.entryArgs(),
						Flags: o.flags(), Action: o.runEntryDelete("log"),
					},
				},
			},
			{
				Name: "set", Usage: "replace MEMORY.md (--file FILE or stdin)", ArgsUsage: nameArgs,
				Flags: o.flags(
					&cli.StringFlag{Name: "file", Usage: "path to the new MEMORY.md"},
					forceFlag("write even when the new file has validation issues"),
					ifMatchFlag(),
				),
				Action: o.runSet,
			},
			{
				Name: "clear", Usage: "rewrite MEMORY.md as an empty skeleton", ArgsUsage: nameArgs,
				Flags:  o.flags(forceFlag("required; wipes Context, Lessons, and Log"), ifMatchFlag()),
				Action: o.runClear,
			},
		},
	}
}

// bodyFlag names the section-body input file; "-" and empty read stdin.
func bodyFlag() cli.Flag {
	return &cli.StringFlag{Name: "body", Value: "-", Usage: "FILE with the section body, - for stdin"}
}

// bodyOrStdin maps the --body value onto readFileOrStdin's empty-means-stdin.
func bodyOrStdin(path string) (string, error) {
	if path == "-" {
		path = ""
	}
	return readFileOrStdin(path)
}

// entryArgs is the ArgsUsage of entry delete per owner shape.
func (o memoryOwner) entryArgs() string {
	if o.named() {
		return "<name> <entry-id>"
	}
	return "<entry-id>"
}

func (o memoryOwner) runShow(_ context.Context, c *cli.Command) error {
	client, name, ws, err := o.resolve(c)
	if err != nil {
		return err
	}
	if err := o.requireName(name, "show"); err != nil {
		return err
	}
	var info memory.Info
	if err := client.GetJSON(o.root(name, ws).Path(), &info); err != nil {
		return err
	}
	switch {
	case c.Bool("json"):
		return encodeJSON(c, info)
	case c.Bool("raw"):
		fmt.Fprint(outWriter(c), info.Content)
		return nil
	}
	o.printInfo(c, &info)
	for _, line := range memory.FormatIssues("MEMORY.md", info.Issues) {
		fmt.Fprintln(outWriter(c), line)
	}
	return nil
}

// printInfo renders the default two-column summary.
func (o memoryOwner) printInfo(c *cli.Command, info *memory.Info) {
	lessons, log := 0, 0
	if info.Doc != nil {
		lessons, log = len(info.Doc.Lessons), len(info.Doc.Log)
	}
	logCell := strconv.Itoa(log)
	if info.Options.MaxLogs > 0 {
		logCell = fmt.Sprintf("%d / %d", log, info.Options.MaxLogs)
	}
	updated := "-"
	if !info.UpdatedAt.IsZero() {
		updated = info.UpdatedAt.Format(time.RFC3339)
	}
	t := newTable(outWriter(c), []string{"Field", "Value"})
	t.AppendRow(table.Row{"bytes", info.SizeBytes})
	t.AppendRow(table.Row{"updated", updated})
	t.AppendRow(table.Row{"hash", dash(info.Hash)})
	t.AppendRow(table.Row{"lessons", lessons})
	t.AppendRow(table.Row{"log", logCell})
	t.AppendRow(table.Row{"inject_logs", info.Options.InjectLogs})
	t.AppendRow(table.Row{"require_update", yesNo(info.Options.RequireUpdate)})
	t.AppendRow(table.Row{"max_bytes", info.Options.MaxBytes})
	t.AppendRow(table.Row{"issues", len(info.Issues)})
	renderTable(t)
}

func (o memoryOwner) runValidate(_ context.Context, c *cli.Command) error {
	client, name, ws, err := o.resolve(c)
	if err != nil {
		return err
	}
	if err := o.requireName(name, "validate"); err != nil {
		return err
	}
	var res struct {
		Valid  bool           `json:"valid"`
		Issues []memory.Issue `json:"issues"`
	}
	if err := client.DoHTTP(o.root(name, ws).Validate(""), &res); err != nil {
		return err
	}
	if c.Bool("json") {
		if err := encodeJSON(c, res); err != nil {
			return err
		}
	} else if res.Valid {
		fmt.Fprintln(outWriter(c), "valid")
	}
	if res.Valid {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(memory.FormatIssues("MEMORY.md", res.Issues), "\n"))
}

// runSectionPut builds the action of set-context.
func (o memoryOwner) runSectionPut(section string) cli.ActionFunc {
	return func(_ context.Context, c *cli.Command) error {
		client, name, ws, err := o.resolve(c)
		if err != nil {
			return err
		}
		if err := o.requireName(name, "set-"+section); err != nil {
			return err
		}
		body, err := bodyOrStdin(c.String("body"))
		if err != nil {
			return err
		}
		return client.DoHTTP(o.root(name, ws).Section(section, body), nil)
	}
}

// runEntryAdd builds the action of lesson add and log add.
func (o memoryOwner) runEntryAdd(section string) cli.ActionFunc {
	return func(_ context.Context, c *cli.Command) error {
		client, name, ws, err := o.resolve(c)
		if err != nil {
			return err
		}
		if err := o.requireName(name, section+" add"); err != nil {
			return err
		}
		return client.DoHTTP(o.root(name, ws).EntryAdd(section, c.String("at"), c.String("text")), nil)
	}
}

// runEntryDelete builds the action of lesson delete and log delete.
func (o memoryOwner) runEntryDelete(section string) cli.ActionFunc {
	return func(_ context.Context, c *cli.Command) error {
		client, name, ws, err := o.resolve(c)
		if err != nil {
			return err
		}
		eid := c.Args().First()
		if o.named() {
			eid = c.Args().Get(1)
		}
		if eid == "" || (o.named() && name == "") {
			return fmt.Errorf("usage: trebi %s memory %s delete%s <entry-id>", o.noun, section, o.nameSuffix(name))
		}
		return client.DoHTTP(o.root(name, ws).EntryDelete(section, eid), nil)
	}
}

func (o memoryOwner) runSet(_ context.Context, c *cli.Command) error {
	client, name, ws, err := o.resolve(c)
	if err != nil {
		return err
	}
	if err := o.requireName(name, "set"); err != nil {
		return err
	}
	content, err := readFileOrStdin(c.String("file"))
	if err != nil {
		return err
	}
	return client.DoHTTP(o.root(name, ws).Put(content, c.String("if-match"), c.Bool("force")), nil)
}

func (o memoryOwner) runClear(_ context.Context, c *cli.Command) error {
	client, name, ws, err := o.resolve(c)
	if err != nil {
		return err
	}
	if err := o.requireName(name, "clear"); err != nil {
		return err
	}
	confirm, err := confirmed(c, o.clearWhat(name))
	if err != nil {
		return err
	}
	if err := client.DoHTTP(o.root(name, ws).Clear(c.String("if-match"), confirm), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "cleared%s; the file is the empty skeleton again\n", o.nameSuffix(name))
	return nil
}

// requireName enforces the <name> arg on named owners.
func (o memoryOwner) requireName(name, verb string) error {
	if !o.named() || name != "" {
		return nil
	}
	if o.noun == "agent" {
		return fmt.Errorf("name required")
	}
	return fmt.Errorf("usage: trebi %s memory %s <name>", o.noun, verb)
}

// nameSuffix renders the owner argument for hint lines.
func (o memoryOwner) nameSuffix(name string) string {
	if !o.named() {
		return ""
	}
	return " " + name
}

func (o memoryOwner) clearWhat(name string) string {
	switch o.noun {
	case "agent":
		return "wipes the memory of agent " + name
	case "assistant":
		return "wipes the assistant memory back to the skeleton"
	default:
		return "wipes the memory of job " + name + " back to the skeleton"
	}
}

// yesNo renders a boolean table cell.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// memoryCommand is the offline top-level: it checks and repairs MEMORY.md
// files with the daemon down.
func memoryCommand() *cli.Command {
	return &cli.Command{
		Name:  "memory",
		Usage: "offline MEMORY.md tools (no daemon)",
		Commands: []*cli.Command{
			{
				Name: "validate", Usage: "check a MEMORY.md file", ArgsUsage: "[FILE]",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
					&cli.IntFlag{Name: "max-logs", Value: 100, Usage: "log entry cap (0 = no cap)"},
					&cli.IntFlag{Name: "max-bytes", Value: config.DefaultMemoryMaxBytes, Usage: "cap on memory bytes in the prompt (0 = no cap)"},
					&cli.BoolFlag{Name: "fix", Usage: "seed the skeleton when the file is missing"},
				},
				Action: runMemoryValidateOffline,
			},
			{
				Name:   "skeleton",
				Usage:  "print the empty MEMORY.md skeleton",
				Action: runMemorySkeleton,
			},
		},
	}
}

func runMemoryValidateOffline(_ context.Context, c *cli.Command) error {
	path := c.Args().First()
	if path == "" {
		path = "MEMORY.md"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !c.Bool("fix") || !os.IsNotExist(err) {
			return err
		}
		if err := os.WriteFile(path, memory.Skeleton(), 0o644); err != nil {
			return err
		}
		data = memory.Skeleton()
		fmt.Fprintf(outWriter(c), "seeded %s\n", path)
	}
	return memoryIssues(c, data, path)
}

// memoryIssues reports the validation verdict; a file with issues exits 1.
func memoryIssues(c *cli.Command, data []byte, path string) error {
	opt := memory.Options{MaxLogs: c.Int("max-logs"), MaxBytes: c.Int("max-bytes"), InjectLogs: config.DefaultMemory().InjectLogs}
	issues := memory.Validate(data, opt)
	if c.Bool("json") {
		return encodeJSON(c, map[string]any{
			"valid": len(issues) == 0, "issues": issues,
		})
	}
	if len(issues) == 0 {
		fmt.Fprintln(outWriter(c), "valid")
		return nil
	}
	return fmt.Errorf("%s", strings.Join(memory.FormatIssues(path, issues), "\n"))
}

func runMemorySkeleton(_ context.Context, c *cli.Command) error {
	_, err := outWriter(c).Write(memory.Skeleton())
	return err
}
