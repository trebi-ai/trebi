package cli

import (
	"encoding/json"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func skillCommand() *cli.Command {
	return &cli.Command{
		Name:  "skill",
		Usage: "manage skills",
		Subcommands: []*cli.Command{
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
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var body map[string]any
					if err := client.GetJSON("/v1/skills/"+c.Args().Get(0), &body); err != nil {
						return err
					}
					enc := json.NewEncoder(outWriter(c))
					enc.SetIndent("", "  ")
					return enc.Encode(body)
				},
			},
			{
				Name:      "create",
				Usage:     "create skill from SKILL.md content on stdin or --file",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "file", Usage: "path to SKILL.md"},
				},
				Action: runSkillCreate,
			},
			{
				Name:      "update",
				Usage:     "replace SKILL.md content from --file or stdin",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "file", Usage: "path to SKILL.md"},
				},
				Action: runSkillUpdate,
			},
			{
				Name:      "delete",
				Usage:     "delete skill",
				ArgsUsage: "<name>",
				Action: func(c *cli.Context) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.Delete("/v1/skills/" + c.Args().Get(0))
				},
			},
			skillFileSubcommand(),
		},
	}
}

func runSkillList(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := client.GetJSON(BuildSkillsListPath(c.Int("limit")), &body); err != nil {
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

func runSkillCreate(c *cli.Context) error {
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
	var out map[string]any
	return client.PostJSON("/v1/skills", map[string]any{"name": name, "skill_md": md}, &out)
}

func runSkillUpdate(c *cli.Context) error {
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
	return client.DoHTTP(BuildSkillPut(name, md), nil)
}
