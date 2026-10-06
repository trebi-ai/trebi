package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/trebi-ai/trebi/internal/proposals"
	"github.com/urfave/cli/v3"
)

func proposalsCommand() *cli.Command {
	return &cli.Command{
		Name:  "proposals",
		Usage: "review and decide AGENT.md change proposals (human workflow)",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list proposals",
				Flags: []cli.Flag{&cli.BoolFlag{Name: "pending", Usage: "only pending proposals"}, &cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: func(_ context.Context, c *cli.Command) error {
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					state := ""
					if c.Bool("pending") {
						state = "pending"
					}
					var body struct {
						Proposals []proposals.Proposal `json:"proposals"`
					}
					if err := client.GetJSON("/v1/proposals?state="+state, &body); err != nil {
						return err
					}
					if c.Bool("json") {
						return encodeJSON(c, body)
					}
					tbl := newTable(outWriter(c), []string{"ID", "Agent", "Status", "Created"})
					for _, p := range body.Proposals {
						tbl.AppendRow(table.Row{p.ID, strings.TrimPrefix(p.AgentName, "agents/"), p.Status, p.CreatedAt.Format("2006-01-02 15:04")})
					}
					renderTable(tbl)
					return nil
				},
			},
			{
				Name:      "show",
				ArgsUsage: "<id>",
				Usage:     "show a proposal unified diff",
				Action: func(_ context.Context, c *cli.Command) error {
					if c.Args().Len() != 1 {
						return fmt.Errorf("usage: trebi proposals show <id>")
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var p proposals.Proposal
					if err := client.GetJSON("/v1/proposals/"+c.Args().First(), &p); err != nil {
						return err
					}
					if p.Note != "" {
						fmt.Fprintln(os.Stdout, p.Note)
					}
					if len(p.ProtectedKeys) > 0 {
						fmt.Fprintln(os.Stdout, "Protected keys changed: "+strings.Join(p.ProtectedKeys, ", "))
					}
					diff := difflib.UnifiedDiff{
						A: difflib.SplitLines(p.BaseContent), FromFile: "AGENT.md (reviewed)",
						B: difflib.SplitLines(p.Proposed), ToFile: "AGENT.md (proposed)", Context: 3,
					}
					text, _ := difflib.GetUnifiedDiffString(diff)
					fmt.Fprint(os.Stdout, text)
					return nil
				},
			},
			decisionCommand("approve", true), decisionCommand("deny", false),
		},
	}
}

func decisionCommand(name string, allow bool) *cli.Command {
	return &cli.Command{
		Name:      name,
		ArgsUsage: "<id>",
		Action: func(_ context.Context, c *cli.Command) error {
			if os.Getenv("TREBI_RUN_TOKEN") != "" {
				return errors.New("proposal decisions require a human; unset TREBI_RUN_TOKEN")
			}
			if c.Args().Len() != 1 {
				return fmt.Errorf("usage: trebi proposals %s <id>", name)
			}
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			decision := "deny"
			if allow {
				decision = "allow"
			}
			var p proposals.Proposal
			return client.PostJSON("/v1/proposals/"+c.Args().First()+"/decide", map[string]string{"decision": decision}, &p)
		},
	}
}
