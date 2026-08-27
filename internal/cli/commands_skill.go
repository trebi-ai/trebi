package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func skillCommand() *cli.Command {
	return &cli.Command{
		Name:  "skill",
		Usage: "manage skills",
		Subcommands: []*cli.Command{
			{
				Name:   "list",
				Usage:  "list skills",
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
	if err := client.GetJSON("/v1/skills", &body); err != nil {
		return err
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
	md := ""
	if f := c.String("file"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		md = string(b)
	} else {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		md = string(b)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	return client.PostJSON("/v1/skills", map[string]any{"name": name, "skill_md": md}, &out)
}
