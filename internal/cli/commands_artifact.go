package cli

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v3"

	"github.com/trebi-ai/trebi/internal/artifacts"
	"github.com/trebi-ai/trebi/internal/request"
)

func artifactCommand() *cli.Command { //nolint:funlen // static command table
	ws := func() cli.Flag {
		return &cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Sources: cli.EnvVars("TREBI_WORKSPACE"), Usage: "workspace id or name"}
	}
	jsonFlag := func() cli.Flag { return &cli.BoolFlag{Name: "json", Usage: "emit JSON"} }
	return &cli.Command{
		Name:  "artifact",
		Usage: "save, read, and send the documents that agents and jobs make",
		Commands: []*cli.Command{
			{
				Name:    "ls",
				Aliases: []string{"list"},
				Usage:   "list artifacts, newest first",
				Flags: []cli.Flag{
					ws(),
					&cli.StringFlag{Name: "owner", Usage: "assistant | job:<name> | agent:<name>"},
					&cli.StringFlag{Name: "kind", Usage: "markdown | email | html | table | json | code | svg | image | pdf | file"},
					&cli.StringFlag{Name: "q", Usage: "text to find in the name or the title"},
					&cli.BoolFlag{Name: "live", Usage: "only live artifacts"},
					&cli.StringFlag{Name: "run", Usage: "only the versions that this run saved"},
					&cli.IntFlag{Name: "limit", Usage: "page size"},
					jsonFlag(),
				},
				Action: runArtifactList,
			},
			{
				Name:      "show",
				ArgsUsage: "<artifact>",
				Usage:     "show one artifact and its versions",
				Flags:     []cli.Flag{ws(), jsonFlag()},
				Action:    runArtifactShow,
			},
			{
				Name:      "get",
				ArgsUsage: "<artifact>[@n]",
				Usage:     "print the content of a version (the latest by default)",
				Flags:     []cli.Flag{ws(), &cli.StringFlag{Name: "out", Aliases: []string{"o"}, Usage: "write the content to this file"}},
				Action:    runArtifactGet,
			},
			{
				Name:      "put",
				ArgsUsage: "<name>",
				Usage:     "save a new version from a file or stdin",
				Flags: []cli.Flag{
					ws(),
					&cli.StringFlag{Name: "from", Aliases: []string{"f"}, Usage: "file to save; - or no flag reads stdin"},
					&cli.StringFlag{Name: "title", Usage: "the title that the user sees"},
					&cli.StringFlag{Name: "kind", Usage: "the kind; the default comes from the file name"},
					&cli.StringFlag{Name: "note", Usage: "what changed in this version"},
					&cli.IntFlag{Name: "base", Usage: "the version that this save starts from"},
					&cli.StringFlag{Name: "artifact", Usage: "save to this artifact id instead of a name"},
					&cli.StringFlag{Name: "owner", Usage: "assistant | job:<name> | agent:<name>, for a new artifact outside a run"},
					jsonFlag(),
				},
				Action: runArtifactPut,
			},
			{
				Name:      "versions",
				ArgsUsage: "<artifact>",
				Usage:     "list the versions of an artifact",
				Flags:     []cli.Flag{ws(), jsonFlag()},
				Action:    runArtifactVersions,
			},
			{
				Name:      "pin",
				ArgsUsage: "<artifact>@<n>",
				Usage:     "keep a version from the version limit",
				Flags:     []cli.Flag{ws(), &cli.BoolFlag{Name: "off", Usage: "remove the pin"}},
				Action:    runArtifactPin,
			},
			{
				Name:      "send",
				ArgsUsage: "<artifact>[@n]",
				Usage:     "send an email draft from its from: line",
				Flags:     []cli.Flag{ws(), &cli.BoolFlag{Name: "draft", Usage: "save it to the drafts of the mailbox; do not send"}, jsonFlag()},
				Action:    runArtifactSend,
			},
			{
				Name:      "deliveries",
				ArgsUsage: "<artifact>",
				Usage:     "list the mails that sent this artifact",
				Flags:     []cli.Flag{ws(), jsonFlag()},
				Action:    runArtifactDeliveries,
			},
			{
				Name:      "ask",
				ArgsUsage: "<artifact>[@n] <text>",
				Usage:     "ask the maker of an artifact for a change",
				Flags:     []cli.Flag{ws(), &cli.StringFlag{Name: "selection", Usage: "the part of the content that the request is about"}, jsonFlag()},
				Action:    runArtifactAsk,
			},
			{
				Name:      "rm",
				Aliases:   []string{"delete"},
				ArgsUsage: "<artifact>",
				Usage:     "delete an artifact and every version of it",
				Flags:     []cli.Flag{ws(), &cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "confirm the delete"}},
				Action:    runArtifactRemove,
			},
		},
	}
}

// artifactArg reads <artifact>[@n] into a ref and a version.
func artifactArg(c *cli.Command, usage string) (request.ArtifactRef, string, error) {
	if c.Args().Len() < 1 {
		return request.ArtifactRef{}, "", fmt.Errorf("usage: trebi artifact %s", usage)
	}
	name, n := request.SplitArtifactRef(c.Args().First())
	return request.ArtifactRef{Ref: name, Workspace: c.String("workspace")}, n, nil
}

func runArtifactList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if run := c.String("run"); run != "" {
		var body map[string]any
		if err := client.GetJSON(request.BuildRunArtifactsPath(run), &body); err != nil {
			return err
		}
		return encodeJSON(c, body)
	}
	f := request.ArtifactFilter{Workspace: c.String("workspace"), Owner: c.String("owner"), Kind: c.String("kind"),
		Query: c.String("q"), Limit: c.Int("limit")}
	if c.IsSet("live") {
		live := c.Bool("live")
		f.Live = &live
	}
	var body struct {
		Items      []artifacts.Artifact `json:"items"`
		NextCursor string               `json:"next_cursor,omitempty"`
	}
	if err := client.GetJSON(request.BuildArtifactListPath(f), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	tbl := newTable(outWriter(c), []string{"Name", "Title", "Kind", "Owner", "Version", "Updated"})
	for _, a := range body.Items {
		owner := a.OwnerKind
		if a.OwnerID != "" {
			owner += ":" + a.OwnerID
		}
		tbl.AppendRow(table.Row{a.Name, a.Title, a.Kind, owner, a.LatestVersion, a.UpdatedAt.Local().Format("2006-01-02 15:04")})
	}
	renderTable(tbl)
	return nil
}

func runArtifactShow(_ context.Context, c *cli.Command) error {
	a, _, err := artifactArg(c, "show <artifact>")
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var art artifacts.Artifact
	if err := client.GetJSON(a.GetPath(), &art); err != nil {
		return err
	}
	var vs struct {
		Items []artifacts.Version `json:"items"`
	}
	if err := client.GetJSON(a.VersionsPath(), &vs); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, map[string]any{"artifact": art, "versions": vs.Items})
	}
	out := outWriter(c)
	fmt.Fprintf(out, "%s  %s  %s  %s\n", art.ID, art.Name, art.Kind, art.Title)
	fmt.Fprintf(out, "owner: %s %s  versions: %d  sent: %d\n", art.OwnerKind, art.OwnerID, art.VersionCount, art.DeliveryCount)
	printArtifactVersions(c, vs.Items)
	return nil
}

func printArtifactVersions(c *cli.Command, vs []artifacts.Version) {
	tbl := newTable(outWriter(c), []string{"N", "Size", "File", "Origin", "Pinned", "Sent", "Note", "Created"})
	for _, v := range vs {
		tbl.AppendRow(table.Row{v.N, v.SizeBytes, v.FileName, cmp.Or(v.RunID, v.OriginKind), v.Pinned, v.Delivered, v.Note,
			v.CreatedAt.Local().Format("2006-01-02 15:04")})
	}
	renderTable(tbl)
}

func runArtifactGet(_ context.Context, c *cli.Command) error {
	a, n, err := artifactArg(c, "get <artifact>[@n]")
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	raw, err := client.GetRaw(a.ContentPath(n, false))
	if err != nil {
		return err
	}
	if out := c.String("out"); out != "" {
		return os.WriteFile(out, raw, 0o600)
	}
	_, err = outWriter(c).Write(raw)
	return err
}

func runArtifactPut(_ context.Context, c *cli.Command) error {
	name := c.Args().First()
	spec := request.ArtifactPutSpec{Workspace: c.String("workspace"), Name: name, ArtifactID: c.String("artifact"),
		Title: c.String("title"), Kind: c.String("kind"), Note: c.String("note"), Owner: c.String("owner")}
	if spec.Name == "" && spec.ArtifactID == "" {
		return errors.New("usage: trebi artifact put <name> --from <file>")
	}
	if c.IsSet("base") {
		b := c.Int("base")
		spec.Base = &b
	}
	from := c.String("from")
	switch {
	case from != "" && from != "-" && os.Getenv("TREBI_RUN_TOKEN") != "":
		// The daemon reads the file from the working folder of the run.
		abs, err := filepath.Abs(from)
		if err != nil {
			return err
		}
		spec.FromPath = abs
	default:
		if from == "-" {
			from = ""
		}
		raw, err := readBytesOrStdin(from)
		if err != nil {
			return err
		}
		spec.ContentBase64 = base64.StdEncoding.EncodeToString(raw)
		if from != "" {
			spec.FileName = filepath.Base(from)
		}
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var res struct {
		Artifact artifacts.Artifact `json:"artifact"`
		Version  artifacts.Version  `json:"version"`
	}
	if err := client.DoHTTP(spec.Build(), &res); err != nil {
		if latest, ok := staleLatest(err); ok {
			ref := cmp.Or(name, spec.ArtifactID)
			return fmt.Errorf("version %d is newer. Read it with `trebi artifact get %s`, then save with `--base %d`", latest, ref, latest)
		}
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, res)
	}
	fmt.Fprintf(outWriter(c), "saved %s@%d (%s)\n", res.Artifact.Name, res.Version.N, res.Artifact.ID)
	return nil
}

// staleLatest reads details.latest from a 409 stale reply.
func staleLatest(err error) (int, bool) {
	msg := err.Error()
	i := strings.Index(msg, "{")
	if i < 0 {
		return 0, false
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Latest int `json:"latest"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(msg[i:]), &env) != nil || env.Error.Code != "stale" {
		return 0, false
	}
	return env.Error.Details.Latest, true
}

func runArtifactVersions(_ context.Context, c *cli.Command) error {
	a, _, err := artifactArg(c, "versions <artifact>")
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Items []artifacts.Version `json:"items"`
	}
	if err := client.GetJSON(a.VersionsPath(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	printArtifactVersions(c, body.Items)
	return nil
}

func runArtifactPin(_ context.Context, c *cli.Command) error {
	a, n, err := artifactArg(c, "pin <artifact>@<n>")
	if err != nil {
		return err
	}
	if _, err := strconv.Atoi(n); err != nil {
		return errors.New("usage: trebi artifact pin <artifact>@<n>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(a.Pin(n, !c.Bool("off")), nil); err != nil {
		return err
	}
	verb := "pinned"
	if c.Bool("off") {
		verb = "unpinned"
	}
	fmt.Fprintf(outWriter(c), "%s %s@%s\n", verb, a.Ref, n)
	return nil
}

func runArtifactSend(_ context.Context, c *cli.Command) error {
	a, n, err := artifactArg(c, "send <artifact>[@n]")
	if err != nil {
		return err
	}
	mode := "send"
	if c.Bool("draft") {
		mode = "draft"
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var res map[string]any
	if err := client.DoHTTP(a.Send(n, mode), &res); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, res)
	}
	if mode == "draft" {
		fmt.Fprintf(outWriter(c), "saved to the drafts of %s\n", mapStr(res, "connection"))
		return nil
	}
	fmt.Fprintf(outWriter(c), "sent %s\n", cmp.Or(mapStr(res, "message_id"), mapStr(res, "email_id")))
	return nil
}

func runArtifactDeliveries(_ context.Context, c *cli.Command) error {
	a, _, err := artifactArg(c, "deliveries <artifact>")
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.GetJSON(a.DeliveriesPath(), &body); err != nil {
		return err
	}
	return encodeJSON(c, body)
}

func runArtifactAsk(_ context.Context, c *cli.Command) error {
	if c.Args().Len() != 2 {
		return errors.New("usage: trebi artifact ask <artifact>[@n] <text>")
	}
	if os.Getenv("TREBI_RUN_TOKEN") != "" {
		return errors.New("a run cannot ask about an artifact; save a new version instead")
	}
	a, n, err := artifactArg(c, "ask <artifact>[@n] <text>")
	if err != nil {
		return err
	}
	v, _ := strconv.Atoi(n) // latest gives 0, the default
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var res map[string]any
	if err := client.DoHTTP(a.Ask(c.Args().Get(1), c.String("selection"), v), &res); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, res)
	}
	fmt.Fprintf(outWriter(c), "asked through the %s %s\n", mapStr(res, "via"), cmp.Or(mapStr(res, "conversation_id"), mapStr(res, "run_id")))
	return nil
}

func runArtifactRemove(_ context.Context, c *cli.Command) error {
	a, _, err := artifactArg(c, "rm <artifact>")
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(a.Delete(c.Bool("yes")), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "deleted %s\n", a.Ref)
	return nil
}
