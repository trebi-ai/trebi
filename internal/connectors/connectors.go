// Package connectors loads connectors.yaml and supervises stream processes.
package connectors

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
	"gopkg.in/yaml.v3"
)

var (
	// ErrNotFound is a missing connector name or id.
	ErrNotFound = errors.New("connector not found")
	// ErrInherited is a mutation against a read-only discovered MCP server.
	ErrInherited = errors.New("inherited from user MCP config")
)

// AmbiguousError is a connector name that matches more than one row.
type AmbiguousError struct {
	Name string
	IDs  []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("connector name %q is ambiguous; use id (%s)", e.Name, strings.Join(e.IDs, ", "))
}

// Definition is one connector from connectors.yaml.
type Definition struct {
	Name    string            `yaml:"name" json:"name"`
	Type    string            `yaml:"type" json:"type"` // stream|cli|mcp
	Command string            `yaml:"command" json:"command"`
	Format  string            `yaml:"format" json:"format"` // json|line
	Cwd     string            `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Facets  []string          `yaml:"facets,omitempty" json:"facets,omitempty"`
}

// MergeInto returns base with every non-empty field of d applied. It gives PUT
// upsert-merge semantics: a partial write keeps the fields it omits.
func (d Definition) MergeInto(base Definition) Definition {
	base.Name = d.Name
	for dst, src := range map[*string]string{
		&base.Type:    d.Type,
		&base.Command: d.Command,
		&base.Format:  d.Format,
		&base.Cwd:     d.Cwd,
	} {
		if src != "" {
			*dst = src
		}
	}
	if len(d.Env) > 0 {
		base.Env = d.Env
	}
	if len(d.Facets) > 0 {
		base.Facets = d.Facets
	}
	if base.Type == "" {
		base.Type = "stream"
	}
	if base.Format == "" {
		base.Format = "line"
	}
	return base
}

// File is the top-level connectors.yaml shape.
// Accepts either a list or a map of name → def.
type File struct {
	Connectors []Definition `yaml:"connectors"`
}

// Status is live supervision state.
type Status struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // stopped|starting|listening|crashed
	PID      int    `json:"pid,omitempty"`
	Restarts int    `json:"restarts"`
	LastErr  string `json:"last_err,omitempty"`
	Events   int64  `json:"events"`
}

// Service supervises connectors and dispatches matched events.
type Service struct {
	st    *store.Store
	ws    *workspaces.Service
	runs  *runs.Service
	pol   *dispatch.Policy
	queue *dispatch.Queue
	log   *slog.Logger

	mu       sync.Mutex
	procs    map[string]*proc
	defs     map[string]Definition
	runCtx   context.Context // process lifetime; not the caller's (HTTP) context
	userHome string          // empty: skip MCP discovery (tests)

	trigMu     sync.Mutex
	trigFP     string
	trigByConn map[string][]cachedTrig
}

type cachedTrig struct {
	row workspaces.JobRow
	j   *job.Job
	tr  job.Trigger
}

type proc struct {
	cancel context.CancelFunc // set once at creation

	mu        sync.Mutex // guards the fields below (readLoop writes, List reads)
	cmd       *exec.Cmd  // nil until the launch completes
	restarts  int
	events    int64
	lastErr   string
	status    string
	startedAt time.Time
}

// eventRecord is one persisted ingress event.
type eventRecord struct {
	id, raw, trace, receivedAt string
	payload                    any
}

func (p *proc) getStatus() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *proc) setStatus(st string) {
	p.mu.Lock()
	p.status = st
	p.mu.Unlock()
}

// markStopped keeps a crashed status; every other status becomes stopped.
func (p *proc) markStopped() {
	p.mu.Lock()
	if p.status != "crashed" {
		p.status = "stopped"
	}
	p.mu.Unlock()
}

func (p *proc) markCrashed(msg string) {
	p.mu.Lock()
	p.lastErr = msg
	p.status = "crashed"
	p.mu.Unlock()
}

func (p *proc) addEvent() {
	p.mu.Lock()
	p.events++
	p.mu.Unlock()
}

func (p *proc) addRestart() {
	p.mu.Lock()
	p.restarts++
	p.mu.Unlock()
}

func (p *proc) snapshot() (status string, restarts int, events int64, lastErr string, pid int, startedAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		pid = p.cmd.Process.Pid
	}
	return p.status, p.restarts, p.events, p.lastErr, pid, p.startedAt
}

// New creates a connectors service.
func New(st *store.Store, ws *workspaces.Service, r *runs.Service, pol *dispatch.Policy) *Service {
	return &Service{
		st: st, ws: ws, runs: r, pol: pol,
		queue:  dispatch.NewQueue(st),
		log:    slog.Default().With("source", "connectors"),
		procs:  make(map[string]*proc),
		defs:   make(map[string]Definition),
		runCtx: context.Background(),
	}
}

// WithUserHome sets the home scanned for inherited MCP servers. Empty skips.
func (s *Service) WithUserHome(dir string) *Service {
	s.userHome = dir
	return s
}

// Load reads connectors.yaml into memory.
func (s *Service) Load() error {
	data, err := os.ReadFile(home.ConnectorsPath())
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.defs = map[string]Definition{}
			s.mu.Unlock()
			return nil
		}
		return err
	}
	defs, err := parseConnectorsYAML(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.defs = defs
	s.mu.Unlock()
	return nil
}

func parseConnectorsYAML(data []byte) (map[string]Definition, error) {
	out := map[string]Definition{}
	// try list form
	var file File
	if err := yaml.Unmarshal(data, &file); err == nil && len(file.Connectors) > 0 {
		for _, d := range file.Connectors {
			if d.Name != "" {
				out[d.Name] = d
			}
		}
		return out, nil
	}
	// try map form: name: {command: ...}
	var m map[string]Definition
	if err := yaml.Unmarshal(data, &m); err == nil {
		for name, d := range m {
			d.Name = name
			out[name] = d
		}
		return out, nil
	}
	// try sources-like: top-level keys with command
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for name, v := range raw {
		if name == "connectors" {
			continue
		}
		b, _ := yaml.Marshal(v)
		var d Definition
		if yaml.Unmarshal(b, &d) == nil {
			d.Name = name
			if d.Command != "" {
				out[name] = d
			}
		}
	}
	return out, nil
}

// List returns definitions merged with live status, unioned with inherited MCP servers.
func (s *Service) List() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	fpIndex := map[string]int{}
	for name, d := range s.defs {
		row := map[string]any{
			"id": name, "name": name, "type": d.Type, "command": d.Command, "format": d.Format,
			"cwd": d.Cwd, "facets": d.Facets, "status": "stopped",
			"provenance": "managed", "managed": true,
		}
		if d.Env != nil {
			row["env"] = d.Env
		}
		if p, ok := s.procs[name]; ok {
			st, restarts, events, lastErr, pid, startedAt := p.snapshot()
			row["status"] = st
			row["restarts"] = restarts
			row["events"] = events
			row["last_err"] = lastErr
			if pid != 0 {
				row["pid"] = pid
			}
			if !startedAt.IsZero() {
				row["started_at"] = startedAt.UTC().Format(time.RFC3339Nano)
			}
		}
		if fp := managedFingerprint(d); fp != "" {
			fpIndex[fp] = len(out)
		}
		out = append(out, row)
	}
	return s.unionInherited(out, fpIndex)
}

func managedFingerprint(d Definition) string {
	cmd := strings.TrimSpace(d.Command)
	if d.Type != "mcp" || cmd == "" {
		return ""
	}
	if strings.HasPrefix(cmd, "http://") || strings.HasPrefix(cmd, "https://") {
		return "http:" + strings.TrimRight(cmd, "/")
	}
	return "stdio:" + strings.Join(strings.Fields(cmd), " ")
}

func (s *Service) unionInherited(out []map[string]any, fpIndex map[string]int) []map[string]any {
	discovered := harness.DiscoverMCP(s.userHome, nil)
	usedNames := map[string]int{}
	for _, row := range out {
		usedNames[fmt.Sprint(row["name"])]++
	}
	for _, srv := range discovered {
		usedNames[srv.Name]++
	}
	for _, srv := range discovered {
		if i, ok := fpIndex[srv.Fingerprint]; ok && srv.Fingerprint != "" {
			row := out[i]
			row["origins"] = srv.Origins
			row["harnesses"] = srv.Harnesses
			row["injected"] = srv.Injected
			row["transport"] = srv.Transport
			continue
		}
		id := "inherited:" + srv.Name
		if usedNames[srv.Name] > 1 {
			id = "inherited:" + srv.Name + "-" + harness.MCPFingerprintID(srv.Fingerprint)
		}
		row := map[string]any{
			"id": id, "name": srv.Name, "type": "mcp", "command": srv.Command,
			"facets": []string{"mcp"}, "status": "inherited",
			"provenance": "inherited", "managed": false,
			"transport": srv.Transport, "harnesses": srv.Harnesses,
			"injected": srv.Injected, "origins": srv.Origins,
		}
		if srv.URL != "" {
			row["url"] = srv.URL
		}
		out = append(out, row)
	}
	return out
}

// Get returns one connector by id or unique name.
func (s *Service) Get(name string) (map[string]any, error) {
	list := s.List()
	var names []map[string]any
	for _, row := range list {
		if fmt.Sprint(row["id"]) == name {
			return row, nil
		}
		if fmt.Sprint(row["name"]) == name {
			names = append(names, row)
		}
	}
	if len(names) == 1 {
		return names[0], nil
	}
	if len(names) > 1 {
		ids := make([]string, len(names))
		for i, r := range names {
			ids[i] = fmt.Sprint(r["id"])
		}
		return nil, &AmbiguousError{Name: name, IDs: ids}
	}
	return nil, fmt.Errorf("connector %q not found: %w", name, ErrNotFound)
}

// ErrReserved marks a connector name the daemon owns natively.
var ErrReserved = errors.New("connector name is reserved")

// reserved names belong to a native channel, not to a subprocess. The row
// for one is synthesized by the API and is invisible to Get, so without
// GuardReserved an Upsert would write a second row with the same name.
var reserved = map[string]bool{"email": true} //nolint:gochecknoglobals // fixed set of native channel names, never mutated

// GuardReserved refuses definition and lifecycle writes against a native
// channel's name. The native channel still owns its event and cursor rows,
// so InjectEvent and PutCursor do not call it.
func (s *Service) GuardReserved(name string) error {
	if reserved[name] {
		return fmt.Errorf("%w: %q is the native email channel", ErrReserved, name)
	}
	return nil
}

// GuardMutate refuses writes against inherited MCP rows. Missing names pass
// through so Start/Delete keep their existing missing-name behavior.
func (s *Service) GuardMutate(name string) error {
	row, err := s.Get(name)
	if err != nil {
		var amb *AmbiguousError
		if errors.As(err, &amb) {
			return err
		}
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	managed, _ := row["managed"].(bool)
	if managed {
		return nil
	}
	path := inheritedOriginPath(row)
	if path != "" {
		return fmt.Errorf("%w: inherited from %s; edit there", ErrInherited, path)
	}
	return fmt.Errorf("%w; edit the origin file", ErrInherited)
}

func inheritedOriginPath(row map[string]any) string {
	switch origins := row["origins"].(type) {
	case []harness.MCPOrigin:
		if len(origins) > 0 {
			return origins[0].Path
		}
	case []any:
		if len(origins) > 0 {
			if m, ok := origins[0].(map[string]any); ok {
				return fmt.Sprint(m["path"])
			}
		}
	}
	return ""
}

// Save writes defs back to connectors.yaml.
func (s *Service) Save(defs map[string]Definition) error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	// write as map form
	b, err := yaml.Marshal(defs)
	if err != nil {
		return err
	}
	if err := os.WriteFile(home.ConnectorsPath(), b, 0o644); err != nil {
		return err
	}
	s.mu.Lock()
	s.defs = defs
	s.mu.Unlock()
	return nil
}

// Upsert registers or updates a connector definition.
func (s *Service) Upsert(ctx context.Context, d Definition) error {
	if err := s.GuardReserved(d.Name); err != nil {
		return err
	}
	if err := s.GuardMutate(d.Name); err != nil {
		return err
	}
	s.mu.Lock()
	if s.defs == nil {
		s.defs = map[string]Definition{}
	}
	s.defs[d.Name] = d.MergeInto(s.defs[d.Name])
	defs := copyDefs(s.defs)
	s.mu.Unlock()
	if err := s.Save(defs); err != nil {
		return err
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "connector.created", Entity: "connector", EntityID: d.Name, Action: "created",
	}); err != nil {
		s.log.Warn("bus append", "error", err)
	}
	return nil
}

// Delete removes a connector.
func (s *Service) Delete(ctx context.Context, name string) error {
	if err := s.GuardReserved(name); err != nil {
		return err
	}
	if err := s.GuardMutate(name); err != nil {
		return err
	}
	_ = s.Stop(name) // already-stopped is fine; delete proceeds either way
	s.mu.Lock()
	delete(s.defs, name)
	defs := copyDefs(s.defs)
	s.mu.Unlock()
	if err := s.Save(defs); err != nil {
		return err
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "connector.deleted", Entity: "connector", EntityID: name, Action: "deleted",
	}); err != nil {
		s.log.Warn("bus append", "error", err)
	}
	return nil
}

// RewritePathPrefix updates connector cwd/command/env that point at old.
// dryRun reports the names without writing. Names are sorted.
func (s *Service) RewritePathPrefix(old, new string, dryRun bool) ([]string, error) {
	if s == nil || strings.TrimSpace(old) == "" || old == new {
		return nil, nil
	}
	if err := s.Load(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defs := copyDefs(s.defs)
	s.mu.Unlock()
	var names []string
	for name, d := range defs {
		changed := false
		if next, ok := workspaces.RewritePathPrefix(d.Cwd, old, new); ok {
			d.Cwd = next
			changed = true
		}
		if next, ok := workspaces.RewritePathPrefix(d.Command, old, new); ok {
			d.Command = next
			changed = true
		}
		if len(d.Env) > 0 {
			env := make(map[string]string, len(d.Env))
			for k, v := range d.Env {
				if next, ok := workspaces.RewritePathPrefix(v, old, new); ok {
					env[k] = next
					changed = true
				} else {
					env[k] = v
				}
			}
			d.Env = env
		}
		if changed {
			defs[name] = d
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if dryRun || len(names) == 0 {
		return names, nil
	}
	return names, s.Save(defs)
}

func copyDefs(in map[string]Definition) map[string]Definition {
	out := make(map[string]Definition, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// StartAll launches all stream connectors.
func (s *Service) StartAll(ctx context.Context) {
	_ = s.Load() // start whatever is already in memory if disk reload fails
	s.mu.Lock()
	s.runCtx = ctx
	names := make([]string, 0, len(s.defs))
	for n, d := range s.defs {
		if d.Command != "" {
			names = append(names, n)
		}
	}
	s.mu.Unlock()
	for _, n := range names {
		if err := s.Start(ctx, n); err != nil {
			s.log.Warn("connector start", "name", n, "error", err)
		}
	}
}

// Start launches one connector process. It reserves the s.procs slot
// under s.mu (status "starting") so concurrent Starts launch one process.
func (s *Service) Start(parent context.Context, name string) error {
	if err := s.GuardReserved(name); err != nil {
		return err
	}
	if err := s.GuardMutate(name); err != nil {
		return err
	}
	s.mu.Lock()
	d, ok := s.defs[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("unknown connector %q", name)
	}
	runCtx := s.runCtx
	if runCtx == nil {
		runCtx = context.Background()
	}
	wasCrashed := false
	if p, ok := s.procs[name]; ok {
		st := p.getStatus()
		if st == "listening" || st == "starting" {
			s.mu.Unlock()
			return nil
		}
		wasCrashed = st == "crashed"
	}
	// Process lifetime is Stop() / runCtx, not the caller's context. HTTP
	// connect/restart pass r.Context(), which cancels when the response ends.
	ctx, cancel := context.WithCancel(runCtx)
	p := &proc{cancel: cancel, status: "starting"}
	s.procs[name] = p
	s.mu.Unlock()

	// remove undoes the reservation on a launch error.
	remove := func() {
		cancel()
		s.mu.Lock()
		if s.procs[name] == p {
			delete(s.procs, name)
		}
		s.mu.Unlock()
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", d.Command)
	if d.Cwd != "" {
		cmd.Dir = d.Cwd
	}
	env := os.Environ()
	resolved, missing := secrets.ExpandRefs(d.Env)
	for _, secName := range missing {
		s.log.Info("secret.referenced_missing", "name", secName, "connector", d.Name)
	}
	for k, v := range resolved {
		env = append(env, k+"="+v)
	}
	cur, _ := s.GetCursor(parent, name)
	env = append(env, "TREBI_LAST_EVENT_ID="+cur.LastEventID)
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		remove()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		remove()
		return err
	}
	if err := cmd.Start(); err != nil {
		remove()
		return err
	}
	p.mu.Lock()
	p.cmd = cmd
	// A Stop during the launch already set "stopped"; keep it.
	if p.status == "starting" {
		p.status = "listening"
		p.startedAt = time.Now().UTC()
	}
	p.mu.Unlock()

	go s.readLoop(ctx, name, d, stdout, cmd, p)
	go s.readStderr(name, stderr)
	// connector.started is log-only; the bus rides connector.updated.
	s.log.Info("connector.started", "connector", name)
	if _, err := s.st.Append(parent, store.BusEvent{
		Topic: "connector.updated", Entity: "connector", EntityID: name, Action: "updated",
		Attrs: map[string]string{"status": "listening"},
	}); err != nil {
		s.log.Warn("bus append", "topic", "connector.updated", "error", err)
	}
	if wasCrashed {
		if _, err := s.st.Append(parent, store.BusEvent{
			Topic: "connector.recovered", Entity: "connector", EntityID: name, Action: "updated",
			Attrs: map[string]string{"status": "listening"},
		}); err != nil {
			s.log.Warn("bus append", "topic", "connector.recovered", "error", err)
		}
	}
	return nil
}

// Stop kills a connector process.
func (s *Service) Stop(name string) error {
	if err := s.GuardReserved(name); err != nil {
		return err
	}
	if err := s.GuardMutate(name); err != nil {
		return err
	}
	s.mu.Lock()
	p, ok := s.procs[name]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	p.cancel()
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill() // process may have already exited
	}
	p.setStatus("stopped")
	return nil
}

// Restart stops then starts.
func (s *Service) Restart(ctx context.Context, name string) error {
	if err := s.GuardReserved(name); err != nil {
		return err
	}
	if err := s.GuardMutate(name); err != nil {
		return err
	}
	_ = s.Stop(name) // not-running is the desired start state
	s.mu.Lock()
	p, ok := s.procs[name]
	s.mu.Unlock()
	if ok {
		p.addRestart()
	}
	return s.Start(ctx, name)
}

// InjectEvent handles a fake event (rules/fire) through the same path.
func (s *Service) InjectEvent(ctx context.Context, name string, event any) (string, error) {
	if err := s.GuardMutate(name); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(event)
	s.mu.Lock()
	d, ok := s.defs[name]
	s.mu.Unlock()
	if !ok {
		d = Definition{Name: name, Format: "json"}
	}
	line := string(raw)
	s.handleLine(ctx, name, d, line)
	return "ok", nil
}

// Cursor is one connector_cursors row.
type Cursor struct {
	Name        string
	LastEventID string
	UpdatedAt   string
}

// GetCursor returns the stored cursor, or a zero LastEventID when missing.
func (s *Service) GetCursor(ctx context.Context, name string) (Cursor, error) {
	c := Cursor{Name: name}
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT last_event_id, updated_at FROM connector_cursors WHERE name=?`, name).
		Scan(&c.LastEventID, &c.UpdatedAt)
	if err != nil {
		return c, nil
	}
	return c, nil
}

// PutCursor upserts the last-event cursor. Not a bus entity.
func (s *Service) PutCursor(ctx context.Context, name, lastEventID string) error {
	if err := s.GuardMutate(name); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.st.DB().ExecContext(ctx, `INSERT INTO connector_cursors (name, last_event_id, updated_at)
		VALUES (?,?,?) ON CONFLICT(name) DO UPDATE SET last_event_id=excluded.last_event_id, updated_at=excluded.updated_at`,
		name, lastEventID, now)
	return err
}

func appendConnectorLog(name, stream, line string) {
	logPath := filepath.Join(home.ConnectorsLogDir(), name+".jsonl")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	logf, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(logf, `{"t":%q,"stream":%q,"line":%q}`+"\n",
		time.Now().UTC().Format(time.RFC3339Nano), stream, line)
	_ = logf.Close()
}

func (s *Service) readStderr(name string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		appendConnectorLog(name, "stderr", sc.Text())
	}
}

func (s *Service) readLoop(ctx context.Context, name string, d Definition, r io.Reader, cmd *exec.Cmd, p *proc) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	defer func() {
		p.markStopped()
		final := p.getStatus()
		s.log.Info("connector.exited", "connector", name, "status", final)
		// The loop ctx is canceled on Stop; use Background for the bus write.
		if _, err := s.st.Append(context.Background(), store.BusEvent{
			Topic: "connector.updated", Entity: "connector", EntityID: name, Action: "updated",
			Attrs: map[string]string{"status": final},
		}); err != nil {
			s.log.Warn("bus append", "topic", "connector.updated", "error", err)
		}
	}()
	for sc.Scan() {
		line := sc.Text()
		appendConnectorLog(name, "stdout", line)
		p.addEvent()
		s.handleLine(ctx, name, d, line)
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	// A canceled ctx means an intentional Stop, not a crash. A self-exit
	// with a zero code ends as "stopped"; a non-zero exit is "crashed".
	if ctx.Err() == nil {
		switch {
		case scanErr != nil:
			p.markCrashed(scanErr.Error())
			s.emitUnhealthy(name, "crashed")
		case waitErr != nil:
			p.markCrashed(waitErr.Error())
			s.emitUnhealthy(name, "crashed")
		}
	}
}

func (s *Service) emitUnhealthy(name, status string) {
	if _, err := s.st.Append(context.Background(), store.BusEvent{
		Topic: "connector.unhealthy", Entity: "connector", EntityID: name, Action: "updated",
		Attrs: map[string]string{"connector": name, "status": status},
	}); err != nil {
		s.log.Warn("bus append", "topic", "connector.unhealthy", "error", err)
	}
}

func (s *Service) handleLine(ctx context.Context, name string, d Definition, line string) {
	ev := s.persistEvent(ctx, name, line, d.decodeLine(line))
	s.dispatchEvent(ctx, name, ev)
}

// persistEvent stores one durable events row and returns its record.
func (s *Service) persistEvent(ctx context.Context, name, line string, payload any) eventRecord {
	raw, _ := json.Marshal(payload)
	summary := line
	if len(summary) > 200 {
		summary = summary[:200]
	}
	ev := eventRecord{
		id:         ulid.Make().String(),
		raw:        string(raw),
		trace:      ulid.Make().String(),
		receivedAt: time.Now().UTC().Format(time.RFC3339Nano),
		payload:    payload,
	}
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO events (id, connector, received_at, summary, raw_json, trace_id)
		VALUES (?,?,?,?,?,?)`, ev.id, name, ev.receivedAt, summary, ev.raw, ev.trace); err != nil {
		s.log.Warn("event insert", "connector", name, "error", err)
		return ev
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "event.created", Entity: "event", EntityID: ev.id, Action: "created",
		Attrs: map[string]string{"connector": name},
	}); err != nil {
		s.log.Warn("bus append", "topic", "event.created", "error", err)
	}
	return ev
}

// dispatchEvent matches one event against the triggers of all active jobs.
func (s *Service) dispatchEvent(ctx context.Context, name string, ev eventRecord) {
	if s.ws == nil {
		return
	}
	for _, t := range s.triggersFor(ctx, name) {
		s.dispatchTrigger(ctx, name, ev, t.row, t.j, t.tr)
	}
	if s.pol != nil {
		s.pol.PruneIdle(time.Now(), 24*time.Hour)
	}
}

func (s *Service) triggersFor(ctx context.Context, connector string) []cachedTrig {
	jobs, err := s.ws.ListJobs(ctx, "")
	if err != nil {
		return nil
	}
	var b strings.Builder
	for _, row := range jobs {
		if !row.Active {
			continue
		}
		b.WriteString(row.WorkspaceID)
		b.WriteByte('/')
		b.WriteString(row.Name)
		b.WriteByte('@')
		b.WriteString(row.Hash)
		b.WriteByte(';')
	}
	fp := b.String()
	s.trigMu.Lock()
	defer s.trigMu.Unlock()
	if fp == s.trigFP && s.trigByConn != nil {
		return s.trigByConn[connector]
	}
	by := map[string][]cachedTrig{}
	for _, row := range jobs {
		if !row.Active {
			continue
		}
		j, err := job.ParseFile(row.Path)
		if err != nil || j == nil {
			continue
		}
		for _, tr := range j.Triggers {
			if tr.Family != "event" || tr.Disabled || tr.Event == "" {
				continue
			}
			by[tr.Event] = append(by[tr.Event], cachedTrig{row: row, j: j, tr: tr})
		}
	}
	s.trigFP = fp
	s.trigByConn = by
	return by[connector]
}

// dispatchTrigger gates one trigger (allowlist, match, policy) and enqueues.
func (s *Service) dispatchTrigger(ctx context.Context, name string, ev eventRecord, row workspaces.JobRow, j *job.Job, tr job.Trigger) {
	if s.st.IsDraining() {
		return
	}
	// Ingress allowlist (security layer 1): deterministic gate before JS match.
	if len(tr.Allowlist) > 0 && !allowlistPasses(tr.Allowlist, ev.payload) {
		s.insertOutcome(ctx, ev.id, row.WorkspaceID, j.Name, tr.Index, string(dispatch.OutcomeBlocked), "", ev.receivedAt)
		return
	}
	res := dispatch.EvalMatch(ctx, tr.Match, tr.Extract, ev.payload)
	if res.Error != "" || !res.Matched {
		outcome := string(dispatch.OutcomeNoMatch)
		if res.Error != "" {
			outcome = string(dispatch.OutcomeError)
		}
		s.insertOutcome(ctx, ev.id, row.WorkspaceID, j.Name, tr.Index, outcome, "", ev.receivedAt)
		return
	}
	// policy
	if s.pol != nil {
		key := row.WorkspaceID + "/" + j.Name + "/" + fmt.Sprint(tr.Index)
		if reason := s.pol.CheckAndRecord(key, tr.Debounce, parseRateLimit(tr.RateLimit), time.Now()); reason != "" {
			s.insertOutcome(ctx, ev.id, row.WorkspaceID, j.Name, tr.Index, reason, "", ev.receivedAt)
			return
		}
	}
	item := &dispatch.QueueItem{
		Connector:   name,
		WorkspaceID: row.WorkspaceID,
		Job:         j.Name,
		Kind:        row.Kind,
		OnIndex:     tr.Index,
		EventID:     ev.id,
		EventRaw:    ev.raw,
		Vars:        res.Vars,
		Status:      dispatch.QueuePending,
		TraceID:     ev.trace,
	}
	if ck, ok := res.Vars["conversation_key"]; ok {
		item.ConversationKey = ck
	}
	if err := s.queue.Enqueue(ctx, item); err != nil {
		s.log.Warn("enqueue", "error", err)
		return
	}
	s.insertOutcome(ctx, ev.id, row.WorkspaceID, j.Name, tr.Index, string(dispatch.OutcomeQueued), item.ID, ev.receivedAt)
}

// insertOutcome records one event_outcomes row (best-effort, logged on error).
func (s *Service) insertOutcome(ctx context.Context, eid, wsID, jobName string, onIndex int, outcome, queueID, now string) {
	var qid any
	if queueID != "" {
		qid = queueID
	}
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO event_outcomes
		(event_id, workspace_id, job, on_index, outcome, queue_id, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (event_id, workspace_id, job, on_index) DO NOTHING`, eid, wsID, jobName, onIndex, outcome, qid, now); err != nil {
		s.log.Warn("event outcome insert", "job", jobName, "outcome", outcome, "error", err)
	}
}

// decodeLine parses one output line per the declared format.
func (d Definition) decodeLine(line string) any {
	if d.Format == "json" || (d.Format == "" && strings.HasPrefix(strings.TrimSpace(line), "{")) {
		var m any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			return m
		}
	}
	return map[string]any{"line": line}
}

// identityKeys returns the event fields the ingress allowlist checks.
func identityKeys() []string {
	return []string{"sender", "from", "chat_id", "chat", "user", "user_id", "author", "phone", "email", "channel"}
}

// allowlistPasses reports whether an identity field of the event matches an
// allowlist entry. An entry has two forms:
//   - "key:value" (key is an identity key): matches only that event field.
//   - bare "value": matches when ANY identity field equals it (flat OR
//     across all identity keys).
//
// Numeric identity values compare as decimal strings. The gate denies when
// no entry matches — the default is closed.
func allowlistPasses(allow []string, event any) bool {
	m, ok := event.(map[string]any)
	if !ok {
		return false
	}
	for _, a := range allow {
		if k, want, scoped := strings.Cut(a, ":"); scoped && isIdentityKey(k) {
			if want != "" && identityString(m[k]) == want {
				return true
			}
			continue
		}
		if a == "" {
			continue
		}
		for _, k := range identityKeys() {
			if identityString(m[k]) == a {
				return true
			}
		}
	}
	return false
}

func isIdentityKey(k string) bool {
	for _, ik := range identityKeys() {
		if k == ik {
			return true
		}
	}
	return false
}

// identityString renders an identity value for comparison; "" means absent.
func identityString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return ""
}

// parseRateLimit parses "N/duration" e.g. "10/1m".
func parseRateLimit(s string) *dispatch.RateLimit {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	var n int
	if _, err := fmt.Sscanf(parts[0], "%d", &n); err != nil || n <= 0 {
		return nil
	}
	d, err := time.ParseDuration(parts[1])
	if err != nil || d <= 0 {
		return nil
	}
	return &dispatch.RateLimit{Count: n, Per: d}
}
