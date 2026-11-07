package connectors_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/runs"
)

type fakeKit map[string]runs.Access

func (k fakeKit) RunAccess(_ context.Context, runID string) runs.Access { return k[runID] }

type fakeAsker struct {
	approved map[string]bool
	asked    []string
}

func (a *fakeAsker) Approved(_ context.Context, _, target string) (bool, error) {
	return a.approved[target], nil
}

func (a *fakeAsker) Ask(_ context.Context, _, target, _, _ string) (string, error) {
	a.asked = append(a.asked, target)
	return "req_1", nil
}

func TestOperations(t *testing.T) {
	ctx := context.Background()
	_, _, svc := openStack(t)
	bin := filepath.Join(t.TempDir(), "fakeop")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"csv) printf 'region,total\\nnorth,3\\n' ;;\n" +
		"json) printf '{\"n\":\"%s\",\"token\":\"%s\"}' \"$2\" \"$TOKEN\" ;;\n" +
		"fail) echo boom >&2; exit 3 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test program
		t.Fatal(err)
	}
	str := &manifest.InputSchema{Type: "object", Properties: map[string]manifest.InputField{"n": {Type: "string"}}, Required: []string{"n"}}
	e := inline("warehouse", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{Operations: []manifest.Operation{
			{Name: "rows", Label: "Rows", ReadOnly: true, Default: manifest.OpOn, Command: []string{bin, "csv"}, Output: manifest.OutputCSV},
			{Name: "echo", Label: "Echo", ReadOnly: true, Default: manifest.OpOn, Command: []string{bin, "json", "{input.n}"},
				Output: manifest.OutputJSON, Input: str, Env: map[string]string{"TOKEN": "tok"}},
			{Name: "fail", Label: "Fail", ReadOnly: true, Default: manifest.OpOn, Command: []string{bin, "fail"}},
			{Name: "send", Label: "Send", Default: manifest.OpAsk, Command: []string{bin, "json", "x"}, Output: manifest.OutputJSON},
		}}
	})
	if _, err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	g, err := svc.GrantActions(ctx, "", "warehouse")
	if err != nil || g == nil || !g.ReadOnly || g.MCP != nil || g.Env != nil || g.BinDir != "" {
		t.Fatalf("grant: %+v %v", g, err)
	}
	if !strings.Contains(g.Hint, "started this run") || !strings.Contains(g.Hint, "- rows: Rows") || strings.Contains(g.Hint, "send") {
		t.Fatalf("grant hint = %q", g.Hint)
	}
	w := inline("writer", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{Operations: []manifest.Operation{{Name: "send", Label: "Send", Command: []string{bin, "json", "x"}}}}
	})
	if _, err := svc.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	if g, err := svc.GrantActions(ctx, "", "writer"); err != nil || g != nil {
		t.Fatalf("no read-only operation: %+v %v", g, err)
	}
	ask := &fakeAsker{approved: map[string]bool{}}
	ops := connectors.NewOperations(svc, fakeKit{"run_in": {Connectors: []string{"warehouse"}}, "run_ev": {Trigger: "warehouse"}}, ask)
	call := func(c connectors.CallOpts) (connectors.CallResult, error) {
		c.Connection = "warehouse"
		return ops.Call(ctx, c)
	}

	res, err := call(connectors.CallOpts{Operation: "rows"})
	if err != nil || res.Rows == nil || len(res.Rows.Columns) != 2 || len(res.Rows.Values) != 1 || res.Rows.Values[0][0] != "north" {
		t.Fatalf("csv: %+v %v", res, err)
	}
	res, err = call(connectors.CallOpts{Operation: "echo", Input: map[string]any{"n": "7"}})
	if err != nil || string(res.Output) != `{"n":"7","token":"tok"}` {
		t.Fatalf("json: %s %v", res.Output, err)
	}
	var in *manifest.InputError
	if _, err = call(connectors.CallOpts{Operation: "echo"}); !errors.As(err, &in) {
		t.Fatalf("missing input: %v", err)
	}
	var ce *connectors.CallError
	if _, err = call(connectors.CallOpts{Operation: "fail"}); !errors.As(err, &ce) || ce.ExitCode != 3 || ce.Stderr != "boom" {
		t.Fatalf("fail: %v", err)
	}
	if _, err = call(connectors.CallOpts{Operation: "nope"}); !errors.Is(err, connectors.ErrNoOperation) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err = call(connectors.CallOpts{Operation: "send", RequireReadOnly: true}); !errors.Is(err, connectors.ErrNotReadOnly) {
		t.Fatalf("read-only: %v", err)
	}

	// The kit of the run limits the connections.
	if _, err = call(connectors.CallOpts{Operation: "rows", RunID: "run_other"}); !errors.Is(err, connectors.ErrConnectorNotKit) {
		t.Fatalf("kit: %v", err)
	}

	// The trigger connection of an event run reads only.
	if _, err = call(connectors.CallOpts{Operation: "rows", RunID: "run_ev"}); err != nil {
		t.Fatalf("grant read: %v", err)
	}
	var ge *connectors.GrantError
	_, err = call(connectors.CallOpts{Operation: "send", RunID: "run_ev", Agent: true})
	if !errors.As(err, &ge) || !errors.Is(err, connectors.ErrConnectorNotKit) {
		t.Fatalf("grant write: %v", err)
	}
	if want := `warehouse started this run, so the run can only read it. "Send" changes it. To allow changes, add warehouse to the connectors of the job or agent.`; err.Error() != want {
		t.Fatalf("grant text = %q", err.Error())
	}

	// State ask: a person passes, an assistant waits, a run opens a request until it is approved.
	if _, err = call(connectors.CallOpts{Operation: "send"}); err != nil {
		t.Fatalf("person: %v", err)
	}
	var ae *connectors.ApprovalError
	if _, err = call(connectors.CallOpts{Operation: "send", Agent: true}); !errors.As(err, &ae) || ae.RequestID != "" {
		t.Fatalf("assistant: %v", err)
	}
	if _, err = call(connectors.CallOpts{Operation: "send", RunID: "run_in", Agent: true}); !errors.As(err, &ae) || ae.RequestID != "req_1" || len(ask.asked) != 1 {
		t.Fatalf("run ask: %v", err)
	}
	ask.approved[ask.asked[0]] = true
	if _, err = call(connectors.CallOpts{Operation: "send", RunID: "run_in", Agent: true}); err != nil {
		t.Fatalf("approved: %v", err)
	}

	// SetState writes an override; the default clears it.
	if err := ops.SetState(ctx, "warehouse", "send", manifest.OpOff); err != nil {
		t.Fatal(err)
	}
	if _, err = call(connectors.CallOpts{Operation: "send"}); !errors.Is(err, connectors.ErrOperationOff) {
		t.Fatalf("off: %v", err)
	}
	if err := ops.SetState(ctx, "warehouse", "rows", manifest.OpOff); err != nil {
		t.Fatal(err)
	}
	if _, err = call(connectors.CallOpts{Operation: "rows", RunID: "run_ev"}); !errors.Is(err, connectors.ErrOperationOff) {
		t.Fatalf("grant off: %v", err)
	}
	if err := ops.SetState(ctx, "warehouse", "rows", manifest.OpOn); err != nil {
		t.Fatal(err)
	}
	list, err := ops.List(ctx, "", "warehouse")
	if err != nil || len(list) != 4 || list[3].State != manifest.OpOff || list[3].Default != manifest.OpAsk {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := ops.SetState(ctx, "warehouse", "send", manifest.OpAsk); err != nil {
		t.Fatal(err)
	}
	if c, err := svc.Lookup("warehouse"); err != nil || c.Operations != nil {
		t.Fatalf("override kept: %+v %v", c.Operations, err)
	}
	if err := ops.SetState(ctx, "warehouse", "send", "maybe"); err == nil {
		t.Fatal("a bad state must fail")
	}
	if err := ops.SetState(ctx, "warehouse", "nope", manifest.OpOn); !errors.Is(err, connectors.ErrNoOperation) {
		t.Fatalf("unknown op: %v", err)
	}
}
