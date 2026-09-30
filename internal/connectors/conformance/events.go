package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/procgroup"
)

// Bounds of one lines run.
const (
	linesWait = 10 * time.Second
	pollWait  = 30 * time.Second
	linesMax  = 20
)

// eventsSuite runs a lines command once (or one poll) and parses its
// output with format, id, ts, and identity. It runs in live mode only,
// because the command reads the account.
type eventsSuite struct{}

func (eventsSuite) name() string { return SuiteEventsLines }

func (eventsSuite) applies(m manifest.Manifest) bool {
	return m.Protocol() == manifest.ProtocolLines
}

func (eventsSuite) run(ctx context.Context, r *run) []Result {
	if !r.live {
		return []Result{{Suite: SuiteEventsLines, Check: "run", Status: StatusSkip, Message: "checked on a live connection only"}}
	}
	ev := r.t.Manifest.WithDefaults().Events
	lines, err := r.lines(ctx, ev)
	out := []Result{row(SuiteEventsLines, "run", err)}
	if err != nil {
		return out
	}
	if len(lines) == 0 {
		return append(out, Result{Suite: SuiteEventsLines, Check: "parse", Status: StatusSkip, Message: "the command printed no line"})
	}
	return append(out, parseLines(ev, lines)...)
}

// lines runs the command with the clean env and the live inputs. A poll
// command must exit 0; a stream is read for linesWait.
func (r *run) lines(ctx context.Context, ev *manifest.Events) ([]string, error) {
	d, err := r.folders()
	if err != nil {
		return nil, err
	}
	wait := linesWait
	if ev.Poll > 0 {
		wait = pollWait
	}
	lctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	cmd, stdout, stderr, err := procgroup.Command{Script: ev.Command, Dir: r.t.Dir, Env: r.env(d, r.t.Inputs)}.Start(lctx)
	if err != nil {
		return nil, err
	}
	errTail := &tail{max: stderrKeep}
	errDone := make(chan struct{})
	go func() { // ends at stderr EOF, when the process group exits
		defer close(errDone)
		_, _ = bufio.NewReader(stderr).WriteTo(errTail) //nolint:errcheck // the tail is for messages only
	}()
	var out []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), protocol.MaxLine)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		out = append(out, sc.Text())
		if len(out) >= linesMax {
			cancel()
			break
		}
	}
	<-errDone
	waitErr := cmd.Wait()
	switch {
	case len(out) >= linesMax:
	case lctx.Err() != nil && ev.Poll > 0:
		return out, fmt.Errorf("the poll command did not exit in %s", wait)
	case lctx.Err() != nil:
		// A stream runs until the limit; that is normal.
	case waitErr != nil:
		return out, fmt.Errorf("%w: %s", waitErr, strings.TrimSpace(errTail.String()))
	}
	return out, nil
}

// parseLines checks each line against format, id, ts, and identity.
func parseLines(ev *manifest.Events, lines []string) []Result {
	if ev.Format != manifest.FormatJSON {
		return []Result{{Suite: SuiteEventsLines, Check: "parse", Status: StatusPass, Message: fmt.Sprintf("%d text lines", len(lines))}}
	}
	var bad, noID, noTS, noIdentity []string
	for i, l := range lines {
		var v any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			bad = append(bad, fmt.Sprintf("line %d: %v", i+1, err))
			continue
		}
		n := strconv.Itoa(i + 1)
		if ev.ID != "" && lookupPath(v, ev.ID) == "" {
			noID = append(noID, n)
		}
		if ev.TS != "" && lookupPath(v, ev.TS) == "" {
			noTS = append(noTS, n)
		}
		if ev.Identity != "" && lookupPath(v, ev.Identity) == "" {
			noIdentity = append(noIdentity, n)
		}
	}
	missing := func(field string, at []string) error {
		if len(at) == 0 {
			return nil
		}
		return fmt.Errorf("no %s on lines %s", field, strings.Join(at, ", "))
	}
	var parseErr error
	if len(bad) > 0 {
		parseErr = errors.New(strings.Join(bad, "; "))
	}
	out := []Result{row(SuiteEventsLines, "parse", parseErr)}
	for _, f := range []struct {
		check, path string
		at          []string
	}{{"id", ev.ID, noID}, {"ts", ev.TS, noTS}, {"identity", ev.Identity, noIdentity}} {
		if f.path == "" {
			out = append(out, Result{Suite: SuiteEventsLines, Check: f.check, Status: StatusSkip, Message: "events." + f.check + " is not set"})
			continue
		}
		out = append(out, row(SuiteEventsLines, f.check, missing("events."+f.check+" ("+f.path+")", f.at)))
	}
	return out
}

// lookupPath reads a dotted path (a.b.0.c) from a decoded JSON value and
// returns a scalar as text, or "".
func lookupPath(v any, path string) string {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return ""
			}
			cur = node[i]
		default:
			return ""
		}
	}
	switch x := cur.(type) {
	case string:
		return x
	case float64, bool:
		return fmt.Sprint(x)
	}
	return ""
}
