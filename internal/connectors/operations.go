package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

var (
	ErrNoOperation      = errors.New("operation not found")
	ErrOperationOff     = errors.New("operation is off")
	ErrNotReadOnly      = errors.New("operation is not read-only")
	ErrConnectorNotKit  = errors.New("the run has no access to this connection")
	ErrOperationTooLong = errors.New("operation output is too large")
)

// ApprovalError is a call in state ask that waits for the user. RequestID
// is "" when no run can park.
type ApprovalError struct {
	Operation string
	RequestID string
}

func (e *ApprovalError) Error() string {
	if e.RequestID == "" {
		return fmt.Sprintf("operation %s needs the approval of the user; ask the user to run it", e.Operation)
	}
	return fmt.Sprintf("operation %s needs the approval of the user: request %s is open; call again after the user approves", e.Operation, e.RequestID)
}

// CallError is a call whose program failed. Stderr holds its first 2000
// characters.
type CallError struct {
	Operation string
	ExitCode  int
	Stderr    string
}

func (e *CallError) Error() string {
	return fmt.Sprintf("operation %s failed (exit %d): %s", e.Operation, e.ExitCode, e.Stderr)
}

// maxOpOutput caps the stdout of one call.
const maxOpOutput = 10 << 20

// maxOpStderr caps the stderr text in a CallError.
const maxOpStderr = 2000

// Asker makes and reads the confirm request of a call in state ask.
type Asker interface {
	Approved(ctx context.Context, runID, target string) (bool, error)
	Ask(ctx context.Context, runID, target, title, summary string) (string, error)
}

// RunKit reads the connections that a run launched with.
type RunKit interface {
	RunConnectors(ctx context.Context, runID string) []string
}

// Operations runs the named operations of a connection (D13).
type Operations struct {
	svc *Service
	kit RunKit
	ask Asker
	log *slog.Logger
}

func NewOperations(svc *Service, kit RunKit, ask Asker) *Operations {
	return &Operations{svc: svc, kit: kit, ask: ask, log: slog.Default().With("source", "operations")}
}

// OperationView is one operation with its effective state.
type OperationView struct {
	manifest.Operation
	State   manifest.OpState `json:"state"`
	Default manifest.OpState `json:"default_state"`
}

// List returns the operations of a connection.
func (o *Operations) List(_ context.Context, ws, ref string) ([]OperationView, error) {
	r, err := o.svc.Resolve(ref, ws)
	if err != nil {
		return nil, err
	}
	c, err := o.svc.Lookup(r.ID)
	if err != nil {
		return nil, err
	}
	out := []OperationView{}
	for _, op := range r.Manifest.Operations() {
		out = append(out, OperationView{Operation: op, State: c.opState(op), Default: op.DefaultState()})
	}
	return out, nil
}

func (c Connection) opState(op manifest.Operation) manifest.OpState {
	if st, ok := c.Operations[op.Name]; ok && st.Valid() {
		return st
	}
	return op.DefaultState()
}

// SetState writes the state of one operation to the connection file. The
// manifest default clears the override.
func (o *Operations) SetState(ctx context.Context, ref, name string, st manifest.OpState) error {
	if !st.Valid() {
		return &ValidationError{Name: ref, Fields: []FieldError{{Field: "state", Message: "must be on, ask, or off"}}}
	}
	if err := o.svc.GuardMutate(ref); err != nil {
		return err
	}
	o.svc.writeMu.Lock()
	defer o.svc.writeMu.Unlock()
	old, err := o.svc.Lookup(ref)
	if err != nil {
		return err
	}
	r, err := o.svc.Resolve(old.ID, "")
	if err != nil {
		return err
	}
	op, ok := r.Manifest.Operation(name)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoOperation, name)
	}
	c := old.clone()
	if st == op.DefaultState() {
		delete(c.Operations, name)
	} else {
		if c.Operations == nil {
			c.Operations = map[string]manifest.OpState{}
		}
		c.Operations[name] = st
	}
	if len(c.Operations) == 0 {
		c.Operations = nil
	}
	return o.svc.commit(ctx, c, &old, nil)
}

// CallOpts is one call. RunID is the run that calls; Agent marks a call of
// an agent with no run (an assistant session).
type CallOpts struct {
	Workspace       string
	Connection      string
	Operation       string
	Input           map[string]any
	RunID           string
	Agent           bool
	RequireReadOnly bool
}

// Rows is the parsed CSV output.
type Rows struct {
	Columns []string   `json:"columns"`
	Values  [][]string `json:"values"`
}

// CallResult is the output of one call.
type CallResult struct {
	Format string          `json:"format"`
	Output json.RawMessage `json:"output,omitempty"`
	Text   string          `json:"text,omitempty"`
	Rows   *Rows           `json:"rows,omitempty"`
	TookMS int64           `json:"took_ms"`
}

// Call checks the access and the state, then runs the program with an argv
// list and no shell.
func (o *Operations) Call(ctx context.Context, c CallOpts) (CallResult, error) {
	r, err := o.svc.Resolve(c.Connection, c.Workspace)
	if err != nil {
		return CallResult{}, err
	}
	if c.RunID != "" && o.kit != nil && !slices.ContainsFunc(o.kit.RunConnectors(ctx, c.RunID), func(n string) bool {
		return n == r.ID || n == r.Slug
	}) {
		return CallResult{}, fmt.Errorf("%w: add %s to the connectors of the job or agent", ErrConnectorNotKit, r.Slug)
	}
	op, ok := r.Manifest.Operation(c.Operation)
	if !ok {
		return CallResult{}, fmt.Errorf("%w: %s has no operation %s", ErrNoOperation, r.Slug, c.Operation)
	}
	if c.RequireReadOnly && !op.ReadOnly {
		return CallResult{}, fmt.Errorf("%w: %s", ErrNotReadOnly, op.Name)
	}
	argv, err := op.Argv(c.Input)
	if err != nil {
		return CallResult{}, err
	}
	stdin, err := op.StdinBytes(c.Input)
	if err != nil {
		return CallResult{}, err
	}
	conn, err := o.svc.Lookup(r.ID)
	if err != nil {
		return CallResult{}, err
	}
	if err := o.gate(ctx, c, r, conn.opState(op), argv); err != nil {
		return CallResult{}, err
	}
	return o.run(ctx, r, op, argv, stdin)
}

// gate applies the state. A person passes ask; an agent needs an approved
// confirm request for the same call.
func (o *Operations) gate(ctx context.Context, c CallOpts, r Resolved, st manifest.OpState, argv []string) error {
	switch st {
	case manifest.OpOn:
		return nil
	case manifest.OpOff:
		return fmt.Errorf("%w: %s of %s; the user can turn it on in the connection settings", ErrOperationOff, c.Operation, r.Slug)
	}
	if c.RunID == "" && !c.Agent {
		return nil
	}
	if c.RunID == "" || o.ask == nil {
		return &ApprovalError{Operation: c.Operation}
	}
	sum := sha256.Sum256([]byte(strings.Join(argv, "\x00")))
	target := "operation:" + r.ID + "/" + c.Operation + "/" + hex.EncodeToString(sum[:8])
	ok, err := o.ask.Approved(ctx, c.RunID, target)
	if err != nil || ok {
		return err
	}
	id, err := o.ask.Ask(ctx, c.RunID, target, fmt.Sprintf("Allow %s: %s?", r.Title, c.Operation),
		fmt.Sprintf("The agent wants to run %s on %s with: %s", c.Operation, r.Title, strings.Join(argv[1:], " ")))
	if err != nil {
		return err
	}
	return &ApprovalError{Operation: c.Operation, RequestID: id}
}

func (o *Operations) run(ctx context.Context, r Resolved, op manifest.Operation, argv []string, stdin []byte) (CallResult, error) {
	env, missing := r.Env()
	o.svc.logMissingSecrets(r.Slug, missing)
	env["TREBI_STATE_DIR"] = r.StateDir()
	env["TREBI_CACHE_DIR"] = r.CacheDir()
	for k, v := range op.Env {
		env[k] = v
	}
	path := r.PathEnv(os.Getenv("PATH"))
	bin := argv[0]
	if dir := r.BinDir(); dir != "" && !strings.Contains(bin, "/") {
		if _, err := os.Stat(filepath.Join(dir, bin)); err == nil {
			bin = filepath.Join(dir, bin)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, op.EffectiveTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, argv[1:]...) //nolint:gosec // argv from a reviewed manifest; no shell
	cmd.Dir = r.WorkDir()
	cmd.Env = []string{"PATH=" + path, "HOME=" + os.Getenv("HOME"), "LANG=C.UTF-8", "TMPDIR=" + os.TempDir()}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout capped
	stdout.max = maxOpOutput
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := os.MkdirAll(r.StateDir(), 0o700); err != nil {
		return CallResult{}, err
	}
	start := time.Now()
	err := cmd.Run()
	took := time.Since(start).Milliseconds()
	if stdout.over {
		return CallResult{}, fmt.Errorf("%w: more than %d bytes", ErrOperationTooLong, maxOpOutput)
	}
	if err != nil {
		return CallResult{}, callError(ctx, r.ID, op, err, stderr.String())
	}
	res, err := decodeOutput(op, stdout.buf.Bytes())
	if err != nil {
		return CallResult{}, err
	}
	res.TookMS = took
	o.log.InfoContext(ctx, "connector.operation_called", "conn_id", r.ID, "operation", op.Name, "took_ms", took)
	return res, nil
}

// callError turns a failed operation process into a CallError and logs its
// stderr to the connection log.
func callError(ctx context.Context, connID string, op manifest.Operation, err error, stderr string) error {
	text := strings.TrimSpace(stderr)
	for _, line := range strings.Split(text, "\n") {
		appendConnectorLog(connID, "stderr", op.Name+": "+line)
	}
	if ctx.Err() != nil {
		text = fmt.Sprintf("timed out after %s", op.EffectiveTimeout())
	}
	if len(text) > maxOpStderr {
		text = text[:maxOpStderr]
	}
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	if text == "" {
		text = err.Error()
	}
	return &CallError{Operation: op.Name, ExitCode: code, Stderr: text}
}

// decodeOutput parses stdout in the output format of op.
func decodeOutput(op manifest.Operation, out []byte) (CallResult, error) {
	res := CallResult{Format: op.OutputFormat()}
	switch res.Format {
	case manifest.OutputCSV:
		cr := csv.NewReader(bytes.NewReader(out))
		cr.FieldsPerRecord = -1
		recs, err := cr.ReadAll()
		if err != nil {
			return CallResult{}, fmt.Errorf("operation %s: output is not CSV: %w", op.Name, err)
		}
		rows := &Rows{Columns: []string{}, Values: [][]string{}}
		if len(recs) > 0 {
			rows.Columns, rows.Values = recs[0], recs[1:]
		}
		res.Rows = rows
	case manifest.OutputJSON:
		if t := bytes.TrimSpace(out); len(t) > 0 {
			if !json.Valid(t) {
				return CallResult{}, fmt.Errorf("operation %s: output is not JSON", op.Name)
			}
			res.Output = t
		}
	default:
		res.Text = string(out)
	}
	return res, nil
}

// capped is a writer that keeps max bytes and reports more.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		c.over = true
		return 0, io.ErrShortWrite
	}
	return c.buf.Write(p)
}
