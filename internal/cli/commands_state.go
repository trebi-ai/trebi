package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v3"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/state"
)

func stateCommand() *cli.Command {
	return &cli.Command{
		Name:  "state",
		Usage: "read and write the records of a job or an agent (default: summary)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "owner", Usage: "job or agent ID or name (default: self in a run)"},
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
		},
		Action: runStateSummary,
		Commands: []*cli.Command{
			stateQueryCommand(),
			stateKeysCommand("get", "read full rows by key"),
			stateKeysCommand("has", "check which keys exist"),
			statePutCommand(),
			stateDeleteCommand(),
			statePruneCommand(),
			stateDropCommand(),
		},
	}
}

// stateTarget is the owner and workspace of one state call.
type stateTarget struct {
	owner, workspace string
}

// stateTargetFor reads --owner. In a run, the default owner is self. A name
// carries the workspace; an ID or self does not need it.
func stateTargetFor(c *cli.Command) (stateTarget, error) {
	owner := strings.TrimSpace(c.String("owner"))
	if owner == "" {
		if os.Getenv("TREBI_RUN_ID") == "" {
			return stateTarget{}, errors.New("set --owner to a job or agent ID or name")
		}
		owner = "self"
	}
	t := stateTarget{owner: owner}
	if owner != "self" && !job.LooksLikeID(owner) {
		t.workspace = workspaceFlag(c)
	}
	return t, nil
}

// stateCollection reads the first argument.
func stateCollection(c *cli.Command, usage string) (string, error) {
	coll := c.Args().First()
	if coll == "" {
		return "", errors.New("usage: trebi state " + usage)
	}
	return coll, nil
}

func runStateSummary(_ context.Context, c *cli.Command) error {
	t, err := stateTargetFor(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		OwnerName   string                  `json:"owner_name"`
		Kind        string                  `json:"kind"`
		Collections []state.CollectionStats `json:"collections"`
	}
	var raw json.RawMessage
	if err := client.DoHTTP(request.BuildStateSummary(t.owner, t.workspace), &raw); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, raw)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "%s %s\n", body.Kind, body.OwnerName)
	tbl := newTable(outWriter(c), []string{"Collection", "Rows", "Bytes", "Newest", "Fields"})
	for _, s := range body.Collections {
		tbl.AppendRow(table.Row{s.Name, s.Rows, s.Bytes, s.NewestAt, strings.Join(s.Fields, ", ")})
	}
	renderTable(tbl)
	return nil
}

func stateQueryCommand() *cli.Command {
	return &cli.Command{
		Name:      "query",
		ArgsUsage: "<collection>",
		Usage:     "find rows",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "key", Usage: "exact key (repeat)"},
			&cli.StringFlag{Name: "prefix", Usage: "key prefix"},
			&cli.StringFlag{Name: "q", Usage: "full-text match"},
			&cli.StringFlag{Name: "where", Usage: "JavaScript on v, k, at, for example 'v.score >= 2'"},
			&cli.StringFlag{Name: "since", Usage: "start time, for example 7d or 2026-09-01"},
			&cli.StringFlag{Name: "until", Usage: "end time"},
			&cli.StringFlag{Name: "by", Usage: "at | updated | created"},
			&cli.StringFlag{Name: "sort", Usage: "at | -at | updated | created | key | -v.<field>"},
			&cli.StringSliceFlag{Name: "fields", Usage: "fields to return (repeat or comma list)"},
			&cli.IntFlag{Name: "limit", Usage: "rows per page (max 500)"},
			&cli.StringFlag{Name: "cursor", Usage: "the next value of the prior page"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			coll, err := stateCollection(c, "query <collection>")
			if err != nil {
				return err
			}
			t, err := stateTargetFor(c)
			if err != nil {
				return err
			}
			q := request.StateQuery{
				Keys: c.StringSlice("key"), Prefix: c.String("prefix"), Q: c.String("q"), Where: c.String("where"),
				Since: c.String("since"), Until: c.String("until"), By: c.String("by"), Sort: c.String("sort"),
				Fields: c.StringSlice("fields"), Limit: c.Int("limit"), Cursor: c.String("cursor"),
			}
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			var res state.QueryResult
			if err := client.DoHTTP(request.BuildStateQuery(t.owner, t.workspace, coll, q), &res); err != nil {
				return err
			}
			if c.Bool("json") {
				return encodeJSON(c, res)
			}
			printStateRows(c, res.Rows)
			if res.Next != "" {
				fmt.Fprintf(outWriter(c), "next: --cursor %s\n", res.Next)
			}
			return nil
		},
	}
}

// stateKeysCommand is get or has: both take a collection and keys.
func stateKeysCommand(verb, usage string) *cli.Command {
	return &cli.Command{
		Name:      verb,
		ArgsUsage: "<collection> <key>...",
		Usage:     usage,
		Action: func(_ context.Context, c *cli.Command) error {
			if c.Args().Len() < 2 {
				return fmt.Errorf("usage: trebi state %s <collection> <key> [<key>]", verb)
			}
			t, err := stateTargetFor(c)
			if err != nil {
				return err
			}
			coll, keys := c.Args().First(), c.Args().Tail()
			r := request.BuildStateGet(t.owner, t.workspace, coll, keys)
			if verb == "has" {
				r = request.BuildStateHas(t.owner, t.workspace, coll, keys)
			}
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			var body map[string]any
			if err := client.DoHTTP(r, &body); err != nil {
				return err
			}
			if c.Bool("json") || verb == "get" {
				return encodeJSON(c, body)
			}
			found, _ := body["found"].(map[string]any)
			for _, k := range keys {
				at, ok := found[k]
				switch {
				case !ok:
					fmt.Fprintf(outWriter(c), "%s\tmissing\n", k)
				case at == nil || at == "":
					fmt.Fprintf(outWriter(c), "%s\tfound\n", k)
				default:
					fmt.Fprintf(outWriter(c), "%s\tfound\t%v\n", k, at)
				}
			}
			return nil
		},
	}
}

func statePutCommand() *cli.Command {
	return &cli.Command{
		Name:      "put",
		ArgsUsage: "<collection> [<key> --value '<json>' | --file <path> | -]",
		Usage:     "write rows; all rows are written, or none",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "file", Usage: "JSON array or JSONL of rows {key, at, value, inc}; - reads stdin"},
			&cli.StringFlag{Name: "value", Usage: "JSON object for one row with <key>"},
			&cli.StringFlag{Name: "at", Usage: "time of the one row"},
			&cli.StringFlag{Name: "mode", Usage: "upsert (default) | insert | merge"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			coll, err := stateCollection(c, "put <collection> (--file <path> | - | <key> --value '<json>')")
			if err != nil {
				return err
			}
			t, err := stateTargetFor(c)
			if err != nil {
				return err
			}
			rows, err := statePutRows(c)
			if err != nil {
				return err
			}
			client, err := clientFor(c)
			if err != nil {
				return err
			}
			var res state.PutResult
			if err := client.DoHTTP(request.BuildStatePut(t.owner, t.workspace, coll, rows, c.String("mode")), &res); err != nil {
				return stateBatchErr(err)
			}
			if c.Bool("json") {
				return encodeJSON(c, res)
			}
			fmt.Fprintf(outWriter(c), "inserted %d, updated %d\n", res.Inserted, res.Updated)
			return nil
		},
	}
}

// statePutRows reads one row from <key> --value, or a batch from --file or -.
func statePutRows(c *cli.Command) ([]any, error) {
	file := c.String("file")
	if c.Args().Get(1) == "-" {
		file = "-"
	}
	if file == "" {
		key := c.Args().Get(1)
		if key == "" || !c.IsSet("value") {
			return nil, errors.New("give <key> --value '<json>', --file <path>, or - for stdin")
		}
		val := json.RawMessage(c.String("value"))
		if !json.Valid(val) {
			return nil, errors.New("--value is not valid JSON")
		}
		row := map[string]any{"key": key, "value": val}
		if at := c.String("at"); at != "" {
			row["at"] = at
		}
		return []any{row}, nil
	}
	var data []byte
	var err error
	if file == "-" {
		r := c.Root().Reader
		if r == nil {
			r = os.Stdin
		}
		data, err = io.ReadAll(r)
	} else {
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return nil, err
	}
	return parseStateRows(data)
}

// parseStateRows reads a JSON array or JSONL. Line numbers match the batch
// index, so a BatchError line points at the input line.
func parseStateRows(data []byte) ([]any, error) {
	data = bytes.TrimSpace(data)
	if bytes.HasPrefix(data, []byte("[")) {
		var rows []any
		if err := json.Unmarshal(data, &rows); err != nil {
			return nil, fmt.Errorf("rows: %w", err)
		}
		return rows, nil
	}
	var rows []any
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var row any
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("no rows in the input")
	}
	return rows, nil
}

// stateBatchErr turns a state_invalid error body into one line for each row.
// Other errors pass unchanged.
func stateBatchErr(err error) error {
	msg := err.Error()
	i := strings.Index(msg, `{"error"`)
	if i < 0 {
		return err
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Errors []state.RowError `json:"errors"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(msg[i:]), &env) != nil || env.Error.Code != "state_invalid" {
		return err
	}
	var b strings.Builder
	b.WriteString(env.Error.Message)
	for _, r := range env.Error.Details.Errors {
		fmt.Fprintf(&b, "\nline %d: %s: %s", r.Line, r.Path, r.Msg)
	}
	return errors.New(b.String())
}

func stateDeleteCommand() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		ArgsUsage: "<collection> <key>...",
		Usage:     "delete rows by key",
		Flags:     []cli.Flag{stateYesFlag()},
		Action: func(_ context.Context, c *cli.Command) error {
			if c.Args().Len() < 2 {
				return errors.New("usage: trebi state delete <collection> <key> [<key>] [--yes]")
			}
			t, err := stateTargetFor(c)
			if err != nil {
				return err
			}
			return stateDeleted(c, request.BuildStateDelete(t.owner, t.workspace, c.Args().First(), c.Args().Tail(), c.Bool("yes")))
		},
	}
}

func statePruneCommand() *cli.Command {
	return &cli.Command{
		Name:      "prune",
		ArgsUsage: "<collection>",
		Usage:     "delete the rows in a time range",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "since", Usage: "start time, inclusive"},
			&cli.StringFlag{Name: "until", Usage: "end time, exclusive, for example 180d"},
			&cli.StringFlag{Name: "by", Usage: "at (default) | updated | created"},
			stateYesFlag(),
		},
		Action: func(_ context.Context, c *cli.Command) error {
			coll, err := stateCollection(c, "prune <collection> [--since] [--until] [--yes]")
			if err != nil {
				return err
			}
			if c.String("since") == "" && c.String("until") == "" {
				return errors.New("set --since or --until")
			}
			t, err := stateTargetFor(c)
			if err != nil {
				return err
			}
			return stateDeleted(c, request.BuildStatePrune(t.owner, t.workspace, coll, c.String("since"), c.String("until"), c.String("by"), c.Bool("yes")))
		},
	}
}

func stateDropCommand() *cli.Command {
	return &cli.Command{
		Name:      "drop",
		ArgsUsage: "<collection>",
		Usage:     "delete all rows of a collection",
		Flags:     []cli.Flag{stateYesFlag()},
		Action: func(_ context.Context, c *cli.Command) error {
			coll, err := stateCollection(c, "drop <collection> [--yes]")
			if err != nil {
				return err
			}
			t, err := stateTargetFor(c)
			if err != nil {
				return err
			}
			return stateDeleted(c, request.BuildStateDrop(t.owner, t.workspace, coll, c.Bool("yes")))
		},
	}
}

func stateYesFlag() cli.Flag {
	return &cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "confirm the delete (not needed in a run)"}
}

// stateDeleted sends delete, prune, or drop and prints the count.
func stateDeleted(c *cli.Command, r request.HTTPRequest) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Deleted int `json:"deleted"`
	}
	// DoHTTP drops the body of a DELETE; the count is in it.
	if err := client.doJSON(r.Method, r.Path, r.Body, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "deleted %d\n", body.Deleted)
	return nil
}

// printStateRows prints query rows as a table: key, at, then the other fields.
func printStateRows(c *cli.Command, rows []map[string]any) {
	seen := map[string]bool{"key": true, "at": true}
	var extra []string
	for _, r := range rows {
		for k := range r {
			if !seen[k] {
				seen[k] = true
				extra = append(extra, k)
			}
		}
	}
	slices.Sort(extra)
	cols := append([]string{"key", "at"}, extra...)
	tbl := newTable(outWriter(c), cols)
	for _, r := range rows {
		row := make(table.Row, len(cols))
		for i, k := range cols {
			row[i] = stateCell(r[k])
		}
		tbl.AppendRow(row)
	}
	renderTable(tbl)
}

func stateCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case map[string]any, []any:
		b, _ := json.Marshal(t) // a decoded JSON value always encodes
		return string(b)
	default:
		return fmt.Sprint(t)
	}
}
