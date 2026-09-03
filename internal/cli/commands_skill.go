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
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list skills",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "limit", Aliases: []string{"l"}, Value: 50, Usage: "max rows"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runSkillList,
			},
			{
				Name:      "show",
				Usage:     "show skill",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON (always on)"}},
				Action:    showByName(request.BuildSkillGetPath),
			},
			{
				Name:      "create",
				Usage:     "create skill from SKILL.md content, a git remote, or a folder",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "file", Usage: "path to SKILL.md"},
					&cli.StringFlag{Name: "source-git", Usage: "git remote to clone the skill bundle from"},
					&cli.StringFlag{Name: "source-folder", Usage: "local folder to copy the skill bundle from"},
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
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildSkillRefresh(c.Args().Get(0)), nil)
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
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildSkillDelete(c.Args().Get(0), confirm), nil)
				},
			},
			skillFileSubcommand(),
		},
	}
}

func runSkillList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := client.GetJSON(request.BuildSkillsListPath(c.Int("limit")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Name", "Provenance", "Hash", "Used 30d"})
	for _, sk := range body.Skills {
		tbl.AppendRow(table.Row{
			mapStr(sk, "name"), mapStr(sk, "provenance"),
			mapStr(sk, "hash"), mapStr(sk, "used_30d"),
		})
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
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
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
	return client.DoHTTP(request.BuildSkillPut(name, md, harnesses, c.String("if-match")), nil)
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
					var body struct {
						Secrets []string `json:"secrets"`
					}
					if err := client.GetJSON(request.BuildSecretsListPath(c.Int("limit")), &body); err != nil {
						return err
					}
					if c.Bool("json") {
						enc := json.NewEncoder(outWriter(c))
						enc.SetIndent("", "  ")
						return enc.Encode(body)
					}
					w := outWriter(c)
					tbl := newTable(w, []string{"Name"})
					for _, n := range body.Secrets {
						tbl.AppendRow(table.Row{n})
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
