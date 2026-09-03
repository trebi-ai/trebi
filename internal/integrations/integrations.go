// Package integrations loads integrations.yaml and supervises stream processes.
package integrations

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
	"gopkg.in/yaml.v3"
)

// Definition is one integration from integrations.yaml.
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

// File is the top-level integrations.yaml shape.
// Accepts either a list or a map of name → def.
type File struct {
	Integrations []Definition `yaml:"integrations"`
}

// Status is live supervision state.
type Status struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // listening|stopped|crashed|running
	PID      int    `json:"pid,omitempty"`
	Restarts int    `json:"restarts"`
	LastErr  string `json:"last_err,omitempty"`
	Events   int64  `json:"events"`
}

// Service supervises integrations and dispatches matched events.
type Service struct {
	st    *store.Store
	ws    *workspaces.Service
	runs  *runs.Service
	pol   *dispatch.Policy
	queue *dispatch.Queue
	log   *slog.Logger

	mu    sync.Mutex
	procs map[string]*proc
	defs  map[string]Definition
}

type proc struct {
	cancel context.CancelFunc // set once at creation

	mu       sync.Mutex // guards the fields below (readLoop writes, List reads)
	cmd      *exec.Cmd  // nil until the launch completes
	restarts int
	events   int64
	lastErr  string
	status   string
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

func (p *proc) snapshot() (status string, restarts int, events int64, lastErr string, pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		pid = p.cmd.Process.Pid
	}
	return p.status, p.restarts, p.events, p.lastErr, pid
}

// New creates an integrations service.
func New(st *store.Store, ws *workspaces.Service, r *runs.Service, pol *dispatch.Policy) *Service {
	return &Service{
		st: st, ws: ws, runs: r, pol: pol,
		queue: dispatch.NewQueue(st),
		log:   slog.Default().With("source", "integrations"),
		procs: make(map[string]*proc),
		defs:  make(map[string]Definition),
	}
}

// Load reads integrations.yaml into memory.
func (s *Service) Load() error {
	data, err := os.ReadFile(home.IntegrationsPath())
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.defs = map[string]Definition{}
			s.mu.Unlock()
			return nil
		}
		return err
	}
	defs, err := parseIntegrationsYAML(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.defs = defs
	s.mu.Unlock()
	return nil
}

func parseIntegrationsYAML(data []byte) (map[string]Definition, error) {
	out := map[string]Definition{}
	// try list form
	var file File
	if err := yaml.Unmarshal(data, &file); err == nil && len(file.Integrations) > 0 {
		for _, d := range file.Integrations {
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
		if name == "integrations" {
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

// List returns definitions merged with live status.
func (s *Service) List() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for name, d := range s.defs {
		row := map[string]any{
			"name": name, "type": d.Type, "command": d.Command, "format": d.Format,
			"facets": d.Facets, "status": "stopped",
		}
		if p, ok := s.procs[name]; ok {
			st, restarts, events, lastErr, pid := p.snapshot()
			row["status"] = st
			row["restarts"] = restarts
			row["events"] = events
			row["last_err"] = lastErr
			if pid != 0 {
				row["pid"] = pid
			}
		}
		out = append(out, row)
	}
	return out
}

// Get returns one integration.
func (s *Service) Get(name string) (map[string]any, error) {
	for _, row := range s.List() {
		if row["name"] == name {
			return row, nil
		}
	}
	return nil, fmt.Errorf("integration %q not found", name)
}

// Save writes defs back to integrations.yaml.
func (s *Service) Save(defs map[string]Definition) error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	// write as map form
	b, err := yaml.Marshal(defs)
	if err != nil {
		return err
	}
	if err := os.WriteFile(home.IntegrationsPath(), b, 0o644); err != nil {
		return err
	}
	s.mu.Lock()
	s.defs = defs
	s.mu.Unlock()
	return nil
}

// Upsert registers or updates an integration definition.
func (s *Service) Upsert(ctx context.Context, d Definition) error {
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
		Topic: "integration.created", Entity: "integration", EntityID: d.Name, Action: "created",
	}); err != nil {
		s.log.Warn("bus append", "error", err)
	}
	return nil
}

// Delete removes an integration.
func (s *Service) Delete(ctx context.Context, name string) error {
	_ = s.Stop(name) // already-stopped is fine; delete proceeds either way
	s.mu.Lock()
	delete(s.defs, name)
	defs := copyDefs(s.defs)
	s.mu.Unlock()
	if err := s.Save(defs); err != nil {
		return err
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "integration.deleted", Entity: "integration", EntityID: name, Action: "deleted",
	}); err != nil {
		s.log.Warn("bus append", "error", err)
	}
	return nil
}

func copyDefs(in map[string]Definition) map[string]Definition {
	out := make(map[string]Definition, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// StartAll launches all stream integrations.
func (s *Service) StartAll(ctx context.Context) {
	_ = s.Load() // start whatever is already in memory if disk reload fails
	s.mu.Lock()
	names := make([]string, 0, len(s.defs))
	for n, d := range s.defs {
		if d.Command != "" {
			names = append(names, n)
		}
	}
	s.mu.Unlock()
	for _, n := range names {
		if err := s.Start(ctx, n); err != nil {
			s.log.Warn("integration start", "name", n, "error", err)
		}
	}
}

// Start launches one integration process. It reserves the s.procs slot
// under s.mu (status "starting") so concurrent Starts launch one process.
func (s *Service) Start(parent context.Context, name string) error {
	s.mu.Lock()
	d, ok := s.defs[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("unknown integration %q", name)
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
	ctx, cancel := context.WithCancel(parent)
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
	if len(d.Env) > 0 {
		env := os.Environ()
		for k, v := range d.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		remove()
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		remove()
		return err
	}
	p.mu.Lock()
	p.cmd = cmd
	// A Stop during the launch already set "stopped"; keep it.
	if p.status == "starting" {
		p.status = "listening"
	}
	p.mu.Unlock()

	go s.readLoop(ctx, name, d, stdout, cmd, p)
	// integration.started is log-only; the bus rides integration.updated.
	s.log.Info("integration.started", "integration", name)
	if _, err := s.st.Append(parent, store.BusEvent{
		Topic: "integration.updated", Entity: "integration", EntityID: name, Action: "updated",
		Attrs: map[string]string{"status": "listening"},
	}); err != nil {
		s.log.Warn("bus append", "topic", "integration.updated", "error", err)
	}
	if wasCrashed {
		if _, err := s.st.Append(parent, store.BusEvent{
			Topic: "integration.recovered", Entity: "integration", EntityID: name, Action: "updated",
			Attrs: map[string]string{"status": "listening"},
		}); err != nil {
			s.log.Warn("bus append", "topic", "integration.recovered", "error", err)
		}
	}
	return nil
}

// Stop kills an integration process.
func (s *Service) Stop(name string) error {
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

// Cursor is one integration_cursors row.
type Cursor struct {
	Name        string
	LastEventID string
	UpdatedAt   string
}

// GetCursor returns the stored cursor, or a zero LastEventID when missing.
func (s *Service) GetCursor(ctx context.Context, name string) (Cursor, error) {
	c := Cursor{Name: name}
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT last_event_id, updated_at FROM integration_cursors WHERE name=?`, name).
		Scan(&c.LastEventID, &c.UpdatedAt)
	if err != nil {
		return c, nil
	}
	return c, nil
}

// PutCursor upserts the last-event cursor. Not a bus entity.
func (s *Service) PutCursor(ctx context.Context, name, lastEventID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.st.DB().ExecContext(ctx, `INSERT INTO integration_cursors (name, last_event_id, updated_at)
		VALUES (?,?,?) ON CONFLICT(name) DO UPDATE SET last_event_id=excluded.last_event_id, updated_at=excluded.updated_at`,
		name, lastEventID, now)
	return err
}

func (s *Service) readLoop(ctx context.Context, name string, d Definition, r io.Reader, cmd *exec.Cmd, p *proc) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	logPath := filepath.Join(home.IntegrationsLogDir(), name+".jsonl")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755) // log dir is best-effort
	logf, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	defer func() {
		if logf != nil {
			_ = logf.Close() // read-loop teardown; process is already exiting
		}
		p.markStopped()
		final := p.getStatus()
		s.log.Info("integration.exited", "integration", name, "status", final)
		// The loop ctx is canceled on Stop; use Background for the bus write.
		if _, err := s.st.Append(context.Background(), store.BusEvent{
			Topic: "integration.updated", Entity: "integration", EntityID: name, Action: "updated",
			Attrs: map[string]string{"status": final},
		}); err != nil {
			s.log.Warn("bus append", "topic", "integration.updated", "error", err)
		}
	}()
	for sc.Scan() {
		line := sc.Text()
		if logf != nil {
			_, _ = fmt.Fprintf(logf, `{"t":%q,"stream":"stdout","line":%q}`+"\n",
				time.Now().UTC().Format(time.RFC3339Nano), line) // disk log must not drop the event
		}
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
		Topic: "integration.unhealthy", Entity: "integration", EntityID: name, Action: "updated",
		Attrs: map[string]string{"integration": name, "status": status},
	}); err != nil {
		s.log.Warn("bus append", "topic", "integration.unhealthy", "error", err)
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
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO events (id, integration, received_at, summary, raw_json, trace_id)
		VALUES (?,?,?,?,?,?)`, ev.id, name, ev.receivedAt, summary, ev.raw, ev.trace); err != nil {
		s.log.Warn("event insert", "integration", name, "error", err)
	}
	return ev
}

// dispatchEvent matches one event against the triggers of all active jobs.
func (s *Service) dispatchEvent(ctx context.Context, name string, ev eventRecord) {
	if s.ws == nil {
		return
	}
	jobs, err := s.ws.ListJobs(ctx, "")
	if err != nil {
		return
	}
	for _, row := range jobs {
		if row.Kind != "job" || !row.Active {
			continue
		}
		j, err := job.ParseFile(row.Path)
		if err != nil || j == nil {
			continue
		}
		for _, tr := range j.Triggers {
			if tr.Family != "event" || tr.Event != name || tr.Disabled {
				continue
			}
			s.dispatchTrigger(ctx, name, ev, row, j, tr)
		}
	}
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
		Integration: name,
		WorkspaceID: row.WorkspaceID,
		Job:         j.Name,
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
