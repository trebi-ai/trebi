package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v3"

	"github.com/trebi-ai/trebi/internal/chains"
	"github.com/trebi-ai/trebi/internal/request"
)

func chainCommand() *cli.Command {
	return &cli.Command{
		Name:  "chain",
		Usage: "read the work that agents ask of each other",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list chains, newest first",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "workspace id/name"},
					&cli.StringFlag{Name: "status", Usage: "open | held | stopped"},
					&cli.StringFlag{Name: "agent", Usage: "only chains where this agent asks or is asked"},
					&cli.BoolFlag{Name: "live", Usage: "only chains with a queued or running hop"},
					&cli.StringFlag{Name: "since", Usage: "RFC3339 time or a duration such as 24h"},
					&cli.IntFlag{Name: "limit", Usage: "page size"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runChainList,
			},
			{
				Name:      "show",
				ArgsUsage: "<trace-id or run-id>",
				Usage:     "show a chain as a tree; a run id shows the chain of its latest chain turn",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runChainShow,
			},
			{
				Name:      "hop",
				ArgsUsage: "<hop-id>",
				Usage:     "show one hop with its full text and both ends",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:    runChainHop,
			},
			{
				Name:      "stop",
				ArgsUsage: "<trace-id>",
				Usage:     "stop a chain: its live runs end and each waiting agent gets one turn to finish",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "reason", Usage: "why the chain stops"},
					&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "confirm the stop"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runChainStop,
			},
		},
	}
}

func runChainList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Chains     []chains.Chain `json:"chains"`
		NextCursor string         `json:"next_cursor,omitempty"`
	}
	path := request.BuildChainListPath(request.ChainFilter{
		Workspace: workspaceFlag(c), Status: c.String("status"), Agent: c.String("agent"),
		Since: c.String("since"), Live: c.Bool("live"), Limit: c.Int("limit"),
	})
	if err := client.GetJSON(path, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	tbl := newTable(outWriter(c), []string{"Trace", "Status", "Root", "Depth", "Steps", "Cost", "Updated"})
	for _, ch := range body.Chains {
		status := ch.Status
		if ch.Live {
			status += " (live)"
		}
		tbl.AppendRow(table.Row{ch.TraceID, status, cmp.Or(ch.RootAgent, ch.RootKind),
			fmt.Sprintf("%d/%d", ch.Usage.Depth, ch.Limits.MaxHops), fmt.Sprintf("%d/%d", ch.Usage.Turns, ch.Limits.MaxTurns),
			fmt.Sprintf("$%.2f", ch.Usage.CostUSD), ch.UpdatedAt.Local().Format("2006-01-02 15:04")})
	}
	renderTable(tbl)
	return nil
}

// chainView is the reply of GET /v1/chains/{trace_id}.
type chainView struct {
	Chain chains.Chain      `json:"chain"`
	Hops  []chains.Hop      `json:"hops"`
	Runs  []chains.ChainRun `json:"runs"`
}

func runChainShow(_ context.Context, c *cli.Command) error {
	if c.Args().Len() != 1 {
		return fmt.Errorf("usage: trebi chain show <trace-id or run-id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var v chainView
	if err := client.GetJSON(request.BuildChainGetPath(c.Args().First()), &v); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, v)
	}
	v.print(outWriter(c))
	return nil
}

// print writes the chain as a tree: each ask under the run that sent it,
// each reply under its ask.
func (v chainView) print(out io.Writer) {
	ch := v.Chain
	fmt.Fprintf(out, "chain %s  %s  depth %d of %d  steps %d of %d  $%.2f\n", ch.TraceID, ch.Status,
		ch.Usage.Depth, ch.Limits.MaxHops, ch.Usage.Turns, ch.Limits.MaxTurns, ch.Usage.CostUSD)
	turns := map[string][]int{}
	for _, r := range v.Runs {
		turns[r.ID] = r.Turns
	}
	root := cmp.Or(ch.RootAgent, ch.RootKind)
	if ch.RootRunID != "" {
		fmt.Fprintf(out, "%-8s run %s  turns %s  root\n", root, ch.RootRunID, joinInts(turns[ch.RootRunID]))
	} else {
		fmt.Fprintf(out, "%s  root\n", root)
	}
	targets := map[string]bool{}
	for _, h := range v.Hops {
		if h.Kind != chains.KindReply && h.To.RunID != "" {
			targets[h.To.RunID] = true
		}
	}
	var top []chains.Hop
	for _, h := range v.Hops {
		if h.Kind != chains.KindReply && (h.From.RunID == "" || h.From.RunID == ch.RootRunID || !targets[h.From.RunID]) {
			top = append(top, h)
		}
	}
	v.printHops(out, top, "", map[string]bool{})
}

func (v chainView) printHops(out io.Writer, hops []chains.Hop, indent string, seen map[string]bool) {
	for i, h := range hops {
		if seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		branch, next := "├─ ", "│  "
		if i == len(hops)-1 {
			branch, next = "└─ ", "   "
		}
		to := cmp.Or(h.To.Agent, h.To.Kind)
		line := fmt.Sprintf("%s%s%-6s %s → %s", indent, branch, h.Kind, h.ID, to)
		if h.To.RunID != "" {
			line += fmt.Sprintf("  run %s turn %d", h.To.RunID, h.To.Turn)
		}
		if h.Kind != chains.KindReply {
			line += "  " + string(h.Status)
			if h.RefuseReason != "" {
				line += " (" + string(h.RefuseReason) + ")"
			}
		}
		fmt.Fprintln(out, line)
		var kids []chains.Hop
		for _, k := range v.Hops {
			switch {
			case k.Kind == chains.KindReply && k.ReplyOf == h.ID:
				kids = append(kids, k)
			case k.Kind != chains.KindReply && h.Kind != chains.KindReply && h.To.RunID != "" && k.From.RunID == h.To.RunID:
				kids = append(kids, k)
			}
		}
		v.printHops(out, kids, indent+next, seen)
	}
}

func runChainHop(_ context.Context, c *cli.Command) error {
	if c.Args().Len() != 1 {
		return fmt.Errorf("usage: trebi chain hop <hop-id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Hop chains.Hop `json:"hop"`
	}
	if err := client.GetJSON(request.BuildHopGetPath(c.Args().First()), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	h := body.Hop
	out := outWriter(c)
	fmt.Fprintf(out, "hop %s  %s  %s  chain %s  depth %d\n", h.ID, h.Kind, h.Status, h.TraceID, h.Depth)
	if h.RefuseReason != "" {
		fmt.Fprintf(out, "refused: %s\n", h.RefuseReason)
	}
	fmt.Fprintf(out, "from: %s\n", hopEnd(h.From))
	fmt.Fprintf(out, "to:   %s\n", hopEnd(h.To))
	if h.ReplyOf != "" {
		fmt.Fprintf(out, "reply of: %s\n", h.ReplyOf)
	}
	if h.RequestID != "" {
		fmt.Fprintf(out, "request: %s\n", h.RequestID)
	}
	fmt.Fprintf(out, "\n%s\n", h.Text)
	if h.Context != "" {
		fmt.Fprintf(out, "\nContext:\n%s\n", h.Context)
	}
	return nil
}

func runChainStop(_ context.Context, c *cli.Command) error {
	if c.Args().Len() != 1 {
		return fmt.Errorf("usage: trebi chain stop <trace-id>")
	}
	if os.Getenv("TREBI_RUN_TOKEN") != "" {
		return errors.New("an agent cannot stop a chain; reply or end the turn instead")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var res chains.StopResult
	if err := client.DoHTTP(request.BuildChainStop(c.Args().First(), c.String("reason"), c.Bool("yes")), &res); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, res)
	}
	out := outWriter(c)
	fmt.Fprintf(out, "Stopped chain %s.\n", c.Args().First())
	fmt.Fprintf(out, "Ended runs: %s\n", cmp.Or(strings.Join(res.KilledRuns, ", "), "none"))
	fmt.Fprintf(out, "Stopped hops: %s\n", cmp.Or(strings.Join(res.CancelledHops, ", "), "none"))
	fmt.Fprintf(out, "Agents that finish now: %s\n", cmp.Or(strings.Join(res.Unwinding, ", "), "none"))
	return nil
}

func hopEnd(e chains.End) string {
	s := cmp.Or(e.Agent, e.Kind)
	if e.RunID != "" {
		s += fmt.Sprintf("  run %s turn %d", e.RunID, e.Turn)
	}
	if e.ConversationID != "" {
		s += "  conversation " + e.ConversationID
	}
	return s
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}
