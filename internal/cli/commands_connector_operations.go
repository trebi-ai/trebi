package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v3"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/request"
)

func connectorOperationCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:      "operations",
			Usage:     "list the operations of a connection and their state",
			ArgsUsage: "<connector>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Usage: "resolve the connector for this workspace"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorOperations,
		},
		{
			Name:      "call",
			Usage:     "run one operation of a connection; Trebi gives it the secrets of the connection",
			ArgsUsage: "<connector> <operation>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "workspace", Aliases: []string{"w"}, Sources: cli.EnvVars("TREBI_WORKSPACE"), Usage: "workspace id or name"},
				&cli.StringSliceFlag{Name: "input", Aliases: []string{"i"}, Usage: "KEY=VALUE input field (repeatable; a repeated key makes a list)"},
				&cli.StringFlag{Name: "input-json", Usage: "read the input object from a JSON file; - reads stdin"},
				&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			},
			Action: runConnectorCall,
		},
		{
			Name:      "operation",
			Usage:     "set the state of one operation: on, ask (an agent needs your approval), or off",
			ArgsUsage: "<connector> <operation> <on|ask|off>",
			Action:    runConnectorOperationState,
		},
	}
}

func runConnectorOperations(_ context.Context, c *cli.Command) error {
	if c.Args().Len() != 1 {
		return fmt.Errorf("usage: trebi connector operations <connector>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Items []connectors.OperationView `json:"items"`
	}
	if err := client.GetJSON(request.BuildOperationsListPath(c.Args().First(), c.String("workspace")), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	printOperations(c, body.Items)
	return nil
}

func printOperations(c *cli.Command, ops []connectors.OperationView) {
	tbl := newTable(outWriter(c), []string{"Operation", "Label", "State", "Read-only", "Input"})
	for _, op := range ops {
		var keys []string
		if op.Input != nil {
			keys = slices.Sorted(maps.Keys(op.Input.Properties))
		}
		state := string(op.State)
		if op.State != op.Default {
			state += " (default " + string(op.Default) + ")"
		}
		tbl.AppendRow(table.Row{op.Name, op.Label, state, op.ReadOnly, strings.Join(keys, ", ")})
	}
	renderTable(tbl)
}

func runConnectorCall(_ context.Context, c *cli.Command) error {
	if c.Args().Len() != 2 {
		return fmt.Errorf("usage: trebi connector call <connector> <operation> [--input key=value]")
	}
	input, err := operationInput(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var res connectors.CallResult
	req := request.BuildOperationCall(c.Args().Get(0), c.Args().Get(1), c.String("workspace"), input)
	if err := client.DoHTTP(req, &res); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, res)
	}
	out := outWriter(c)
	switch {
	case res.Rows != nil:
		tbl := newTable(out, res.Rows.Columns)
		for _, row := range res.Rows.Values {
			r := make(table.Row, len(row))
			for i, v := range row {
				r[i] = v
			}
			tbl.AppendRow(r)
		}
		renderTable(tbl)
	case len(res.Output) > 0:
		return encodeJSON(c, res.Output)
	default:
		fmt.Fprint(out, res.Text)
	}
	return nil
}

func runConnectorOperationState(_ context.Context, c *cli.Command) error {
	if c.Args().Len() != 3 {
		return fmt.Errorf("usage: trebi connector operation <connector> <operation> <on|ask|off>")
	}
	if os.Getenv("TREBI_RUN_TOKEN") != "" {
		return fmt.Errorf("only the user can change the state of an operation")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Items []connectors.OperationView `json:"items"`
	}
	if err := client.DoHTTP(request.BuildOperationState(c.Args().Get(0), c.Args().Get(1), c.Args().Get(2)), &body); err != nil {
		return err
	}
	printOperations(c, body.Items)
	return nil
}

// operationInput merges --input-json and the repeated --input KEY=VALUE flags.
func operationInput(c *cli.Command) (map[string]any, error) {
	input := map[string]any{}
	if f := c.String("input-json"); f != "" {
		if f == "-" {
			f = ""
		}
		raw, err := readBytesOrStdin(f)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, fmt.Errorf("--input-json: %w", err)
		}
	}
	for _, kv := range c.StringSlice("input") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--input %q: want KEY=VALUE", kv)
		}
		switch prev := input[k].(type) {
		case nil:
			input[k] = v
		case []any:
			input[k] = append(prev, v)
		default:
			input[k] = []any{prev, v}
		}
	}
	return input, nil
}
