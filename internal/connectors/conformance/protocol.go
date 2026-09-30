package conformance

import (
	"context"
	"errors"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
)

// protocolSuite runs the checks of Part H through protocol.Checker. In the
// sandbox it starts the program on empty folders; in live mode it uses the
// session of the connection and does not stop it.
type protocolSuite struct{}

func (protocolSuite) name() string { return SuiteProtocol }

func (protocolSuite) applies(m manifest.Manifest) bool {
	return m.Protocol() == manifest.ProtocolConnector
}

func (protocolSuite) run(ctx context.Context, r *run) []Result {
	chk := r.checker()
	var res []protocol.CheckResult
	if r.live {
		if r.t.Live == nil {
			return []Result{row(SuiteProtocol, "initialize", errors.New("the connection has no live session"))}
		}
		init := r.t.Live.Init()
		chk.Init = &init
		var st protocol.AuthStatusResult
		if err := r.t.Live.Call(ctx, protocol.MethodAuthStatus, protocol.Empty{}, &st); err != nil || st.State != protocol.StateAuthRequired {
			chk.SkipLogin = true
		}
		res = chk.Run(ctx, r.t.Live)
	} else {
		d, err := r.folders()
		if err != nil {
			return []Result{row(SuiteProtocol, "start", err)}
		}
		chk.Sandbox, chk.EmptyState, chk.Dirs = true, true, []string{d.state, d.cache}
		a, err := r.start(d, r.inputs(), chk.Notify, chk.BadLine)
		if err != nil {
			return []Result{row(SuiteProtocol, "start", err)}
		}
		res = chk.Run(ctx, a.conn)
		a.stop()
		if len(res) == 1 && res[0].Status == protocol.CheckFail && !a.alive() {
			res[0].Message += "; " + a.exitMessage()
		}
	}
	out := make([]Result, 0, len(res))
	for _, c := range res {
		out = append(out, Result{Suite: SuiteProtocol, Check: c.Check, Status: c.Status, Message: c.Message})
	}
	return out
}
