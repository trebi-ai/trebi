// Package conformance checks one connector against its manifest (D15). It
// runs one suite for each manifest block. In the sandbox, the Runner starts
// the program itself with the clean env of D12 and its own temp folders. In
// live mode, it checks the running session of a connection and writes only
// to the room that the operator names. This package does not import
// internal/connectors; that package builds the live Target.
package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/procgroup"
)

// Result statuses.
const (
	StatusPass = "pass"
	StatusFail = "fail"
	StatusSkip = "skip"
	StatusWarn = "warn"
)

// Suite names. Run takes them in only; a prefix such as "actions" selects
// each suite under it.
const (
	SuiteManifest    = "manifest"
	SuiteInstall     = "install"
	SuiteInputs      = "inputs"
	SuiteActionsCLI  = "actions.cli"
	SuiteActionsMCP  = "actions.mcp"
	SuiteEventsLines = "events.lines"
	SuiteProtocol    = "protocol"
	SuiteFolder      = "folder"
)

// Timeouts of the processes that the runner starts.
const (
	stopWait   = 5 * time.Second
	stderrKeep = 2048
)

// Result is one row of a conformance run.
type Result struct {
	Suite   string `json:"suite"`
	Check   string `json:"check"`
	Status  string `json:"status"` // pass|fail|skip|warn
	Message string `json:"message,omitempty"`
}

// Target is the connector to check. It is live when Live or Inputs is set;
// otherwise the runner starts the programs in the sandbox.
type Target struct {
	Manifest manifest.Manifest
	Dir      string            // manifest folder; "" for a live connection
	Command  string            // sandbox command; default events.command + " --sandbox"
	Inputs   map[string]string // live inputs; nil in the sandbox
	Room     string
	Live     Live // set for a live connection
}

// Live is the running session of a connection. internal/connectors
// implements it; this package does not import internal/connectors.
type Live interface {
	Call(ctx context.Context, method string, params, result any) error
	Init() protocol.InitializeResult
}

// Runner runs the suites. It owns the processes and the temp folders of a
// run and removes them when the run ends.
type Runner struct {
	log     *slog.Logger
	now     func() time.Time
	tempDir string // parent of the temp folders; short, so socket paths fit
}

// NewRunner builds a Runner. A nil log uses the default logger.
func NewRunner(log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	tmp := os.TempDir()
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
			tmp = "/tmp"
		}
	}
	return &Runner{log: log.With("source", "conformance"), now: time.Now, tempDir: tmp}
}

// suite checks one manifest block.
type suite interface {
	name() string
	applies(m manifest.Manifest) bool
	run(ctx context.Context, r *run) []Result
}

// suites is the fixed order of a run.
func suites() []suite {
	return []suite{manifestSuite{}, installSuite{}, inputsSuite{}, actionsCLISuite{}, actionsMCPSuite{}, eventsSuite{}, protocolSuite{}, folderSuite{}}
}

// Run runs each suite whose block the manifest has. With only, it runs the
// named suites; a named suite with no block gives one skip row.
func (r *Runner) Run(ctx context.Context, t Target, only []string) []Result {
	all := suites()
	var out []Result
	for _, name := range only {
		if !slices.ContainsFunc(all, func(s suite) bool { return selects(name, s.name()) }) {
			out = append(out, Result{Suite: name, Check: "suite", Status: StatusFail, Message: "unknown suite"})
		}
	}
	rn, err := r.newRun(t)
	if err != nil {
		return append(out, Result{Suite: "runner", Check: "temp folder", Status: StatusFail, Message: err.Error()})
	}
	defer rn.close()
	for _, s := range all {
		if len(only) > 0 && !slices.ContainsFunc(only, func(n string) bool { return selects(n, s.name()) }) {
			continue
		}
		if !s.applies(t.Manifest) {
			if len(only) > 0 {
				out = append(out, Result{Suite: s.name(), Check: "suite", Status: StatusSkip, Message: "the manifest has no block for this suite"})
			}
			continue
		}
		rn.log.DebugContext(ctx, "conformance.suite", "suite", s.name())
		out = append(out, s.run(ctx, rn)...)
	}
	return out
}

// selects reports whether the name the operator gave picks a suite.
func selects(name, suite string) bool {
	return name == suite || strings.HasPrefix(suite, name+".")
}

// run is the state of one Runner.Run: the target and the temp folders.
type run struct {
	*Runner
	t    Target
	live bool
	root string
	n    int
}

func (r *Runner) newRun(t Target) (*run, error) {
	root, err := os.MkdirTemp(r.tempDir, "trebi-cf-")
	if err != nil {
		return nil, err
	}
	return &run{Runner: r, t: t, live: t.Live != nil || t.Inputs != nil, root: root}, nil
}

func (r *run) close() {
	if err := os.RemoveAll(r.root); err != nil {
		r.log.Warn("conformance.cleanup", "dir", r.root, "err", err)
	}
}

// dirs are the folders of one process: HOME, TREBI_CACHE_DIR (also
// TMPDIR), and TREBI_STATE_DIR.
type dirs struct{ home, cache, state string }

// folders makes a new set of empty folders under the run root.
func (r *run) folders() (dirs, error) {
	r.n++
	n := fmt.Sprint(r.n)
	d := dirs{home: filepath.Join(r.root, "h"+n), cache: filepath.Join(r.root, "c"+n), state: filepath.Join(r.root, "s"+n)}
	for _, p := range []string{d.home, d.cache, d.state} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return d, err
		}
	}
	return d, nil
}

// pathEnv is the version PATH: <dir>/bin first when it exists.
func (r *run) pathEnv() string {
	path := os.Getenv("PATH")
	if r.t.Dir == "" {
		return path
	}
	bin := filepath.Join(r.t.Dir, "bin")
	if fi, err := os.Stat(bin); err != nil || !fi.IsDir() {
		return path
	}
	if abs, err := filepath.Abs(bin); err == nil {
		bin = abs
	}
	if path == "" {
		return bin
	}
	return bin + string(os.PathListSeparator) + path
}

// lookPath finds cmd on the version PATH, or returns "".
func (r *run) lookPath(cmd string) string {
	if strings.Contains(cmd, "/") {
		if _, err := os.Stat(cmd); err == nil {
			return cmd
		}
		return ""
	}
	for _, dir := range filepath.SplitList(r.pathEnv()) {
		p := filepath.Join(dir, cmd)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// env is the clean env of D12 for one process: the base env with HOME
// replaced, then TMPDIR, PATH, the folders, and the inputs.
func (r *run) env(d dirs, inputs map[string]string) []string {
	vals := map[string]string{}
	for _, kv := range protocol.BaseEnv(os.Environ()) {
		k, v, _ := strings.Cut(kv, "=")
		vals[k] = v
	}
	vals["HOME"] = d.home
	vals["TMPDIR"] = d.cache
	vals["PATH"] = r.pathEnv()
	vals[protocol.EnvStateDir] = d.state
	vals[protocol.EnvCacheDir] = d.cache
	maps.Copy(vals, inputs)
	out := make([]string, 0, len(vals))
	for _, k := range slices.Sorted(maps.Keys(vals)) {
		out = append(out, k+"="+vals[k])
	}
	return out
}

// inputs are the live inputs, or in the sandbox a fake value for each
// required input and the default of each other input.
func (r *run) inputs() map[string]string {
	if r.live {
		return r.t.Inputs
	}
	out := map[string]string{}
	if r.t.Manifest.Setup == nil {
		return out
	}
	for _, in := range r.t.Manifest.Setup.Inputs {
		switch {
		case in.Default != "":
			out[in.Name] = in.Default
		case len(in.Choices) > 0:
			out[in.Name] = in.Choices[0]
		case in.Required:
			out[in.Name] = "sandbox"
		}
	}
	return out
}

// command is the command line of the adapter in the sandbox.
func (r *run) command() string {
	if r.t.Command != "" {
		return r.t.Command
	}
	if r.t.Manifest.Events == nil || r.t.Manifest.Events.Command == "" {
		return ""
	}
	return r.t.Manifest.Events.Command + " --sandbox"
}

// checker copies the manifest promises into a protocol checker.
func (r *run) checker() *protocol.Checker {
	m := r.t.Manifest
	c := &protocol.Checker{Features: m.Features(), Room: r.t.Room}
	if m.Setup != nil {
		c.Login = m.Setup.Login
	}
	if m.Channel != nil {
		c.Limits = protocol.Limits{MaxText: m.Channel.Limits.MaxText, Formats: m.Channel.Limits.Formats}
	}
	if m.Events != nil {
		for _, t := range m.Events.Types {
			c.EventTypes = append(c.EventTypes, t.Type)
		}
	}
	if spec := m.SubscriptionSpec(); spec != nil {
		c.Watch = &protocol.WatchSpec{Modes: spec.Modes}
		for _, f := range spec.Fields {
			c.Watch.Fields = append(c.Watch.Fields, protocol.WatchField{
				Name: f.Name, Dynamic: f.Dynamic, Multiple: f.Multiple, Required: f.Required, Default: f.Default, Choices: f.Choices,
			})
		}
	}
	if r.t.Dir != "" {
		c.Schemas, _ = m.EventSchemas(r.t.Dir) //nolint:errcheck // the manifest suite reports a bad schema
	}
	return c
}

// row is a pass row, or a fail row with the error.
func row(suite, check string, err error) Result {
	if err != nil {
		return Result{Suite: suite, Check: check, Status: StatusFail, Message: err.Error()}
	}
	return Result{Suite: suite, Check: check, Status: StatusPass}
}

// adapter is one trebi-connector/1 program that the runner started.
type adapter struct {
	cmd    *exec.Cmd
	conn   *protocol.Conn
	stdin  *os.File
	cancel context.CancelFunc
	exited chan struct{}
	stderr *tail

	mu      sync.Mutex
	waitErr error
}

// start starts the adapter command with the clean env on the folders d.
// notify and bad get the notifications and the bad stdout lines.
func (r *run) start(d dirs, inputs map[string]string, notify protocol.NotifyFunc, bad func(error)) (*adapter, error) {
	script := r.command()
	if script == "" {
		return nil, errors.New("the manifest has no events.command; give a command")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	env := append(r.env(d, inputs), protocol.EnvProtocol+"="+protocol.Version)
	// The process lives until stop, not until the caller's ctx ends.
	pctx, cancel := context.WithCancel(context.Background())
	cmd, stdout, stderr, err := procgroup.Command{Script: script, Dir: r.t.Dir, Env: env, Stdin: pr, WaitDelay: stopWait}.Start(pctx)
	_ = pr.Close() // the child holds its own copy
	if err != nil {
		cancel()
		_ = pw.Close() // no process reads it
		return nil, err
	}
	a := &adapter{cmd: cmd, stdin: pw, cancel: cancel, exited: make(chan struct{}), stderr: &tail{max: stderrKeep}}
	a.conn = protocol.NewConn(stdout, pw, notify, protocol.WithBadLine(bad))
	errDone := make(chan struct{})
	go func() { // ends at stderr EOF, when the process group exits
		defer close(errDone)
		_, _ = io.Copy(a.stderr, stderr) //nolint:errcheck // the tail is for messages only
	}()
	go func() { // ends at stdout EOF
		_ = a.conn.Run(context.Background()) //nolint:errcheck // Done and the exit report the end
		<-errDone
		err := cmd.Wait()
		a.mu.Lock()
		a.waitErr = err
		a.mu.Unlock()
		close(a.exited)
	}()
	r.log.Debug("conformance.start", "command", script, "state", d.state)
	return a, nil
}

// alive reports whether the process still runs.
func (a *adapter) alive() bool {
	select {
	case <-a.exited:
		return false
	default:
		return true
	}
}

// waitExit waits up to d for the process to exit.
func (a *adapter) waitExit(d time.Duration) bool {
	select {
	case <-a.exited:
		return true
	case <-time.After(d):
		return false
	}
}

// exitMessage says how the process ended, with the end of its stderr.
func (a *adapter) exitMessage() string {
	a.mu.Lock()
	err := a.waitErr
	a.mu.Unlock()
	msg := "the program exited"
	if err != nil {
		msg += ": " + err.Error()
	}
	if s := strings.TrimSpace(a.stderr.String()); s != "" {
		msg += "; stderr: " + s
	}
	return msg
}

// stop closes stdin, waits for the exit, and kills the group after
// stopWait.
func (a *adapter) stop() {
	_ = a.stdin.Close() // EOF on stdin asks the adapter to exit
	if !a.waitExit(stopWait) {
		a.cancel()
		<-a.exited
	}
	a.cancel()
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = bytes.Clone(t.buf[over:])
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
