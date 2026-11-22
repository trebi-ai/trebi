package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func skillCommand() *cli.Command {
	return &cli.Command{
		Name:  "skill",
		Usage: "manage skills",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "scope", Usage: "global, workspace, agent, or job; picks the skill when a name is in more than one scope"},
			&cli.StringFlag{Name: "owner", Usage: "workspace, agent, or job that holds the skill (name or ID)"},
		},
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list skills",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.StringFlag{Name: "effective-for", Usage: "job or agent; list the skills its runs get, narrow first"},
					&cli.BoolFlag{Name: "archived", Usage: "list only archived skills"},
					&cli.BoolFlag{Name: "include-archived", Usage: "list archived skills with the others"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runSkillList,
			},
			{
				Name:      "show",
				Usage:     "show skill",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON (always on)"}},
				Action: func(_ context.Context, c *cli.Command) error {
					return skillDo(c, func(r request.SkillRef) request.HTTPRequest {
						return request.HTTPRequest{Method: "GET", Path: request.BuildSkillGetPath(r)}
					})
				},
			},
			{
				Name:      "create",
				Usage:     "create skill from SKILL.md content, a git remote, or a folder",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "file", Usage: "path to SKILL.md"},
					&cli.StringFlag{Name: "source-git", Usage: "git remote to clone the skill bundle from"},
					&cli.StringFlag{Name: "source-folder", Usage: "local folder to copy the skill bundle from"},
					&cli.StringFlag{Name: "note", Usage: "short note for the person who reviews the skill (max 500 characters)"},
				},
				Action: runSkillCreate,
			},
			{
				Name:      "set",
				Aliases:   []string{"update"},
				Usage:     "replace SKILL.md content from --file or stdin",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "file", Usage: "path to SKILL.md"},
					&cli.StringSliceFlag{Name: "harness", Usage: "HARNESS=path override (repeatable)"},
					ifMatchFlag(),
				},
				Action: runSkillUpdate,
			},
			{
				Name:      "refresh",
				Usage:     "pull the skill source again",
				ArgsUsage: "<name>",
				Action: func(_ context.Context, c *cli.Command) error {
					return skillDo(c, request.BuildSkillRefresh)
				},
			},
			{
				Name:      "move",
				Usage:     "move a skill to another scope or owner",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "to-scope", Required: true, Usage: "global, workspace, agent, or job"},
					&cli.StringFlag{Name: "to-owner", Usage: "workspace, agent, or job that gets the skill"},
					&cli.StringFlag{Name: "to-workspace", Usage: "workspace of a --to-owner agent or job name"},
					&cli.BoolFlag{Name: "replace", Usage: "delete the skill of the same name at the target"},
					&cli.StringFlag{Name: "rename", Usage: "new name at the target"},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					m := request.SkillMove{
						ToScope: c.String("to-scope"), ToOwner: c.String("to-owner"), ToWorkspace: c.String("to-workspace"),
						Replace: c.Bool("replace"), Rename: c.String("rename"),
					}
					return skillDo(c, func(r request.SkillRef) request.HTTPRequest { return request.BuildSkillMove(r, m) })
				},
			},
			{
				Name:      "approve",
				Usage:     "approve a pending skill so that runs get it",
				ArgsUsage: "<name>",
				Action: func(_ context.Context, c *cli.Command) error {
					return skillDo(c, request.BuildSkillApprove)
				},
			},
			{
				Name:      "archive",
				Usage:     "archive a skill; it keeps its files but leaves every run",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{ifMatchFlag()},
				Action: func(_ context.Context, c *cli.Command) error {
					return skillDo(c, func(r request.SkillRef) request.HTTPRequest {
						return request.BuildSkillArchive(r, c.String("if-match"), true)
					})
				},
			},
			{
				Name:      "unarchive",
				Usage:     "restore an archived skill",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{ifMatchFlag()},
				Action: func(_ context.Context, c *cli.Command) error {
					return skillDo(c, func(r request.SkillRef) request.HTTPRequest {
						return request.BuildSkillArchive(r, c.String("if-match"), false)
					})
				},
			},
			{
				Name:      "delete",
				Usage:     "delete skill",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{forceFlag("required; deletes the skill folder")},
				Action: func(_ context.Context, c *cli.Command) error {
					confirm, err := confirmed(c, "deletes skill "+c.Args().Get(0)+" and its folder")
					if err != nil {
						return err
					}
					return skillDo(c, func(r request.SkillRef) request.HTTPRequest { return request.BuildSkillDelete(r, confirm) })
				},
			},
			skillFileSubcommand(),
		},
	}
}

// skillRefFor reads the skill name argument and the --scope, --owner, and
// --workspace flags. Only an explicit --workspace is sent.
func skillRefFor(c *cli.Command) (request.SkillRef, error) {
	name := c.Args().Get(0)
	if name == "" {
		return request.SkillRef{}, fmt.Errorf("skill name required")
	}
	return request.SkillRef{Name: name, Scope: c.String("scope"), Owner: c.String("owner"), Workspace: c.String("workspace")}, nil
}

// skillDo sends one skill request and prints the JSON reply.
func skillDo(c *cli.Command, build func(request.SkillRef) request.HTTPRequest) error {
	r, err := skillRefFor(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(build(r), &out); err != nil {
		return err
	}
	return encodeJSON(c, out)
}

func runSkillList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	archived, err := archivedFilter(c)
	if err != nil {
		return err
	}
	var body struct {
		Skills []map[string]any `json:"skills"`
	}
	l := request.SkillList{
		Scope: c.String("scope"), Owner: c.String("owner"), Workspace: c.String("workspace"),
		EffectiveFor: c.String("effective-for"), Archived: archived, Limit: c.Int("limit"),
	}
	if err := client.GetJSON(l.Path(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	headers := []string{"Name", "Scope", "Owner", "Status", "Provenance", "Overwritten by", "Used 30d"}
	if archived != "" {
		headers = append(headers, "Archived")
	}
	tbl := newTable(w, headers)
	for _, sk := range body.Skills {
		owner := ""
		if o, ok := sk["owner"].(map[string]any); ok {
			owner = mapStr(o, "name")
		}
		by := mapStr(sk, "overwritten_by")
		if list, ok := sk["overridden_by"].([]any); ok && by == "" {
			parts := make([]string, len(list))
			for i, v := range list {
				parts[i] = fmt.Sprint(v)
			}
			by = strings.Join(parts, ", ")
		}
		row := table.Row{
			mapStr(sk, "name"), mapStr(sk, "scope"), owner, mapStr(sk, "status"),
			mapStr(sk, "provenance"), by, mapStr(sk, "used_30d"),
		}
		if archived != "" {
			row = append(row, dash(archivedDate(sk)))
		}
		tbl.AppendRow(row)
	}
	renderTable(tbl)
	return nil
}

func runSkillCreate(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	spec := request.SkillSpec{
		Name:         name,
		SourceGit:    c.String("source-git"),
		SourceFolder: c.String("source-folder"),
		Scope:        c.String("scope"),
		Owner:        c.String("owner"),
		Workspace:    c.String("workspace"),
		Note:         c.String("note"),
	}
	if spec.SourceGit == "" && spec.SourceFolder == "" {
		md, err := readFileOrStdin(c.String("file"))
		if err != nil {
			return err
		}
		spec.SkillMD = md
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	return client.DoHTTP(spec.Build(), &out)
}

func runSkillUpdate(_ context.Context, c *cli.Command) error {
	r, err := skillRefFor(c)
	if err != nil {
		return err
	}
	md, err := readFileOrStdin(c.String("file"))
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	harnesses, err := parseKeyValues(c.StringSlice("harness"))
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildSkillPut(r, md, harnesses, c.String("if-match")), nil)
}

func secretCommand() *cli.Command {
	return &cli.Command{
		Name:  "secret",
		Usage: "manage secrets",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list secret names",
				Flags: []cli.Flag{limitFlag(), &cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: func(_ context.Context, c *cli.Command) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var raw json.RawMessage
					if err := client.GetJSON(request.BuildSecretsListPath(c.Int("limit")), &raw); err != nil {
						return err
					}
					if c.Bool("json") {
						return encodeJSON(c, raw)
					}
					var body struct {
						Secrets []string `json:"secrets"`
						Owners  map[string]struct {
							Label string `json:"label"`
						} `json:"owners"`
					}
					if err := json.Unmarshal(raw, &body); err != nil {
						return err
					}
					w := outWriter(c)
					tbl := newTable(w, []string{"Name", "Managed by"})
					for _, n := range body.Secrets {
						tbl.AppendRow(table.Row{n, body.Owners[n].Label})
					}
					renderTable(tbl)
					return nil
				},
			},
			{
				Name:      "set",
				Usage:     "set secret value",
				ArgsUsage: "<name> [<value>]",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "from-file", Usage: "read value from file (`-` for stdin)"},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					if c.NArg() < 1 {
						return fmt.Errorf("name required")
					}
					value, err := secretSetValue(c)
					if err != nil {
						return err
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildSecretPut(c.Args().Get(0), value), nil)
				},
			},
			{
				Name:      "delete",
				Usage:     "delete secret",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{forceFlag("required; jobs that read the secret will fail")},
				Action: func(_ context.Context, c *cli.Command) error {
					confirm, err := confirmed(c, "deletes secret "+c.Args().Get(0))
					if err != nil {
						return err
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildSecretDelete(c.Args().Get(0), confirm), nil)
				},
			},
		},
	}
}

func secretSetValue(c *cli.Command) (string, error) {
	if f := c.String("from-file"); f != "" {
		if f == "-" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return "", err
			}
			return strings.TrimRight(string(b), "\n"), nil
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	if c.NArg() >= 2 {
		return c.Args().Get(1), nil
	}
	if stdinPiped() {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	return "", fmt.Errorf("value required (pass <value>, --from-file, or stdin)")
}
