package conformance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
)

// aliveWait is the time a program must stay alive after it reports a
// missing input.
const aliveWait = 500 * time.Millisecond

// inputsSuite checks setup.inputs: each required input has a label, and a
// sandbox with no inputs reports the missing input and stays alive (D13,
// D14).
type inputsSuite struct{}

func (inputsSuite) name() string { return SuiteInputs }

func (inputsSuite) applies(m manifest.Manifest) bool {
	return m.Setup != nil && len(m.Setup.Inputs) > 0
}

func (inputsSuite) run(ctx context.Context, r *run) []Result {
	var unlabeled, required []string
	for _, in := range r.t.Manifest.Setup.Inputs {
		if !in.Required {
			continue
		}
		required = append(required, in.Name)
		if strings.TrimSpace(in.Label) == "" {
			unlabeled = append(unlabeled, in.Name)
		}
	}
	var err error
	if len(unlabeled) > 0 {
		err = fmt.Errorf("required inputs with no label: %s", strings.Join(unlabeled, ", "))
	}
	out := []Result{row(SuiteInputs, "labels", err)}
	switch {
	case len(required) == 0:
		return append(out, Result{Suite: SuiteInputs, Check: "missing input", Status: StatusSkip, Message: "no required input"})
	case r.t.Manifest.Protocol() != manifest.ProtocolConnector:
		return append(out, Result{Suite: SuiteInputs, Check: "missing input", Status: StatusSkip, Message: "no " + protocol.Version + " program to start"})
	case r.live:
		return append(out, Result{Suite: SuiteInputs, Check: "missing input", Status: StatusSkip, Message: "checked in the sandbox only"})
	}
	return append(out, row(SuiteInputs, "missing input", r.missingInput(ctx)))
}

// missingInput starts the sandbox with no inputs. It must answer
// initialize, report auth_required with reason missing_input, stay alive,
// and answer ping.
func (r *run) missingInput(ctx context.Context) error {
	d, err := r.folders()
	if err != nil {
		return err
	}
	chk := &protocol.Checker{}
	a, err := r.start(d, nil, chk.Notify, chk.BadLine)
	if err != nil {
		return err
	}
	defer a.stop()
	if _, err := chk.Initialize(ctx, a.conn); err != nil {
		return fmt.Errorf("%w; %s", err, a.exitMessage())
	}
	st, ok := chk.FirstStatus(ctx)
	switch {
	case !ok && !a.alive():
		return fmt.Errorf("no status; %s", a.exitMessage())
	case !ok:
		return fmt.Errorf("no status after initialize")
	case st.State != protocol.StateAuthRequired || st.Reason != protocol.ReasonMissingInput:
		return fmt.Errorf("with no inputs, the status is %s/%s, want %s/%s", st.State, st.Reason, protocol.StateAuthRequired, protocol.ReasonMissingInput)
	}
	if a.waitExit(aliveWait) {
		return fmt.Errorf("the program exits on a missing input; %s", a.exitMessage())
	}
	pctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := a.conn.Call(pctx, protocol.MethodPing, protocol.Empty{}, nil); err != nil {
		return fmt.Errorf("ping with a missing input: %w", err)
	}
	return nil
}
