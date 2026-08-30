package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
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
				Action: func(_ context.Context, c *cli.Command) error {
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
				Action: func(_ context.Context, c *cli.Command) error {
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

func runSkillList(_ context.Context, c *cli.Command) error {
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

func runSkillCreate(_ context.Context, c *cli.Command) error {
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
	return client.DoHTTP(BuildSkillPut(name, md), nil)
}

func secretCommand() *cli.Command {
	return &cli.Command{
		Name:  "secret",
		Usage: "manage secrets",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list secret names",
				Flags: []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: func(_ context.Context, c *cli.Command) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var body struct {
						Secrets []string `json:"secrets"`
					}
					if err := client.GetJSON("/v1/secrets", &body); err != nil {
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
					return client.PutJSON("/v1/secrets/"+c.Args().Get(0), map[string]string{"value": value}, nil)
				},
			},
			{
				Name:      "delete",
				Usage:     "delete secret",
				ArgsUsage: "<name>",
				Action: func(_ context.Context, c *cli.Command) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.Delete("/v1/secrets/" + c.Args().Get(0))
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
	st, err := os.Stdin.Stat()
	if err == nil && st.Mode()&os.ModeCharDevice == 0 {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	return "", fmt.Errorf("value required (pass <value>, --from-file, or stdin)")
}
