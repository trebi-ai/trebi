// Package connectors loads connectors.yaml and supervises connector
// processes. A connector is one external system with up to three optional
// capabilities: actions, events, and a channel.
package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
	"gopkg.in/yaml.v3"
)

var (
	// ErrNotFound is a missing connector name.
	ErrNotFound = errors.New("connector not found")
	// ErrInherited is a mutation against a read-only discovered MCP server.
	ErrInherited = errors.New("inherited from user MCP config")
	// ErrReserved is a write against a native connector name.
	ErrReserved = errors.New("connector name is reserved")
	// ErrExists is a create for a name that already exists.
	ErrExists = errors.New("connector already exists")
	// ErrNoEvents is a lifecycle action on a connector with no events block.
	ErrNoEvents = errors.New("connector has no events block")
	// ErrLoadFailed is a write while connectors.yaml does not load.
	ErrLoadFailed = errors.New("connectors.yaml did not load")
	// ErrDuplicate is an injected event whose source id is already stored.
	ErrDuplicate = errors.New("event source id already recorded")
	// ErrEventNotFound is a redispatch of a missing events row.
	ErrEventNotFound = errors.New("event not found")
)

// AmbiguousError is a connector name that matches more than one row.
type AmbiguousError struct {
	Name string
	IDs  []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("connector name %q is ambiguous; use id (%s)", e.Name, strings.Join(e.IDs, ", "))
}

// Service stores connector definitions and supervises their processes.
type Service struct {
	st    *store.Store
	ws    *workspaces.Service
	sk    *skills.Service
	queue *dispatch.Queue
	log   *slog.Logger

	mu       sync.Mutex
	procs    map[string]*proc
	defs     map[string]Definition
	loadErr  error           // set by a failed Load; blocks writes
	writeMu  sync.Mutex      // held across read, check, and save of a write
	runCtx   context.Context // process lifetime; not the caller's (HTTP) context
	userHome string          // empty: skip MCP discovery (tests)
	wg       sync.WaitGroup

	// now and sleep are injectable for tests.
	now   func() time.Time
	sleep func(context.Context, time.Duration) bool

	trigMu     sync.Mutex
	trigFP     string
	trigByConn map[string][]cachedTrig
}

type cachedTrig struct {
	row workspaces.JobRow
	j   *job.Job
	tr  job.Trigger
}

// NativeMeta carries the native identity of an injected event.
type NativeMeta struct {
	SourceID string
	Identity string
	Room     string
	Thread   string
}

// eventRecord is one persisted ingress event.
type eventRecord struct {
	id         string
	raw        string
	trace      string
	receivedAt string
	payload    any
	sourceID   string
	identity   string
	room       string
	thread     string
	catchUp    bool
}

// Patch is a block-level update: a present block replaces that block and an
// explicit null removes it. A masked value ("***") keeps the stored value.
type Patch struct {
	Env     *map[string]string `json:"env"`
	Cwd     *string            `json:"cwd"`
	Actions json.RawMessage    `json:"actions"`
	Events  json.RawMessage    `json:"events"`
	Channel json.RawMessage    `json:"channel"`
	// ActionsMerge edits actions field by field after the block values.
	ActionsMerge json.RawMessage `json:"actions_merge"`
}

// New creates a connectors service.
func New(st *store.Store, ws *workspaces.Service, sk *skills.Service) *Service {
	return &Service{
		st: st, ws: ws, sk: sk,
		queue:  dispatch.NewQueue(st),
		log:    slog.Default().With("source", "connectors"),
		procs:  make(map[string]*proc),
		defs:   make(map[string]Definition),
		runCtx: context.Background(),
		now:    time.Now,
		sleep:  sleepCtx,
	}
}

// WithUserHome sets the home scanned for inherited MCP servers. Empty skips.
func (s *Service) WithUserHome(dir string) *Service {
	s.userHome = dir
	return s
}

// sleepCtx waits for d or until ctx is done; it reports whether ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

// Load reads connectors.yaml into memory. The file is strict: an unknown
// key, including the old top-level type, facets, command, and format, fails
// the load with the file path, the connector name, and the key. A failed load
// keeps the last good definitions and blocks writes until a load succeeds.
func (s *Service) Load() error {
	defs, err := readDefinitions()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadErr = err
	if err != nil {
		return err
	}
	s.defs = defs
	return nil
}

// readDefinitions reads and validates connectors.yaml. A missing file is an
// empty set.
func readDefinitions() (map[string]Definition, error) {
	path := home.ConnectorsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Definition{}, nil
		}
		return nil, err
	}
	defs, err := parseDefinitionData(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, d := range defs {
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defs[name] = d.withDefaults()
	}
	return defs, nil
}

// LoadErr returns the error of the last failed Load, or nil.
func (s *Service) LoadErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr
}

// checkLoaded refuses a write while connectors.yaml does not load, so a
// write cannot replace the file with a partial set.
func (s *Service) checkLoaded() error {
	if err := s.LoadErr(); err != nil {
		return fmt.Errorf("%w: %v", ErrLoadFailed, err)
	}
	return nil
}

// save writes defs back to connectors.yaml with a temp-and-rename write. The
// file has mode 0600, because it can hold literal tokens. The caller holds
// writeMu.
func (s *Service) save(defs map[string]Definition) error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	b, err := yaml.Marshal(defs)
	if err != nil {
		return err
	}
	path := home.ConnectorsPath()
	f, err := os.CreateTemp(filepath.Dir(path), ".connectors-*.yaml")
	if err != nil {
		return fmt.Errorf("save connectors: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("save connectors: %w", werr)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("save connectors: %w", err)
	}
	s.mu.Lock()
	s.defs = defs
	s.mu.Unlock()
	return nil
}

// snapshotDefs returns a deep copy of the definitions, so a caller can change
// it without a change to the live set.
func (s *Service) snapshotDefs() map[string]Definition {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Definition, len(s.defs))
	for k, v := range s.defs {
		out[k] = v.clone()
	}
	return out
}

// emitWrite appends a connector.created/updated bus event with the
// capabilities of the definition.
func (s *Service) emitWrite(ctx context.Context, topic string, d Definition) {
	capabilities := strings.Join(d.Capabilities(), ",")
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: topic, Entity: "connector", EntityID: d.Name, Action: strings.TrimPrefix(topic, "connector."),
		Attrs: map[string]string{"capabilities": capabilities},
	}); err != nil {
		s.log.Warn("bus append", "topic", topic, "error", err)
	}
}

// Create registers a new connector definition. An existing name is ErrExists.
func (s *Service) Create(ctx context.Context, d Definition) error {
	if err := s.checkLoaded(); err != nil {
		return err
	}
	if err := s.GuardReserved(d.Name); err != nil {
		return err
	}
	if err := d.Validate(); err != nil {
		return err
	}
	d = d.withDefaults()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	defs := s.snapshotDefs()
	if _, ok := defs[d.Name]; ok {
		return fmt.Errorf("%w: %q", ErrExists, d.Name)
	}
	defs[d.Name] = d
	if err := s.save(defs); err != nil {
		return err
	}
	s.emitWrite(ctx, "connector.created", d)
	return nil
}

// Replace writes the whole definition of a connector, creating it when
// missing. A masked value ("***") keeps the stored value.
func (s *Service) Replace(ctx context.Context, d Definition) error {
	if err := s.checkLoaded(); err != nil {
		return err
	}
	if err := s.GuardReserved(d.Name); err != nil {
		return err
	}
	if err := s.GuardMutate(d.Name); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	defs := s.snapshotDefs()
	old, existed := defs[d.Name]
	if err := d.keepMasked(old); err != nil {
		return err
	}
	if err := d.Validate(); err != nil {
		return err
	}
	d = d.withDefaults()
	defs[d.Name] = d
	if err := s.save(defs); err != nil {
		return err
	}
	topic := "connector.updated"
	if !existed {
		topic = "connector.created"
	}
	s.emitWrite(ctx, topic, d)
	s.maybeRestart(ctx, old, d, existed)
	return nil
}

// Patch applies a block-level update to an existing connector.
func (s *Service) Patch(ctx context.Context, name string, p Patch) error {
	if err := s.checkLoaded(); err != nil {
		return err
	}
	if err := s.GuardReserved(name); err != nil {
		return err
	}
	if err := s.GuardMutate(name); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	defs := s.snapshotDefs()
	old, ok := defs[name]
	if !ok {
		return fmt.Errorf("connector %q not found: %w", name, ErrNotFound)
	}
	d := old.clone()
	if p.Env != nil {
		d.Env = *p.Env
	}
	if p.Cwd != nil {
		d.Cwd = *p.Cwd
	}
	if len(p.Actions) > 0 {
		block, err := decodeBlock[Actions](p.Actions)
		if err != nil {
			return &ValidationError{Name: name, Fields: []FieldError{{Field: "actions", Message: err.Error()}}}
		}
		d.Actions = block
	}
	if len(p.Events) > 0 {
		block, err := decodeBlock[Events](p.Events)
		if err != nil {
			return &ValidationError{Name: name, Fields: []FieldError{{Field: "events", Message: err.Error()}}}
		}
		d.Events = block
	}
	if len(p.Channel) > 0 {
		block, err := decodeBlock[Channel](p.Channel)
		if err != nil {
			return &ValidationError{Name: name, Fields: []FieldError{{Field: "channel", Message: err.Error()}}}
		}
		d.Channel = block
	}
	if len(p.ActionsMerge) > 0 {
		var m actionsMerge
		if err := decodeStrict(p.ActionsMerge, &m); err != nil {
			return &ValidationError{Name: name, Fields: []FieldError{{Field: "actions_merge", Message: err.Error()}}}
		}
		block, err := m.apply(name, d.Actions)
		if err != nil {
			return err
		}
		d.Actions = block
	}
	if err := d.keepMasked(old); err != nil {
		return err
	}
	if err := d.Validate(); err != nil {
		return err
	}
	d = d.withDefaults()
	defs[name] = d
	if err := s.save(defs); err != nil {
		return err
	}
	s.emitWrite(ctx, "connector.updated", d)
	s.maybeRestart(ctx, old, d, true)
	return nil
}

// decodeBlock parses a patch block and refuses unknown keys. A JSON null
// removes the block.
func decodeBlock[T any](raw json.RawMessage) (*T, error) {
	if isNull(raw) {
		return nil, nil
	}
	var out T
	if err := decodeStrict(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// maybeRestart restarts a live supervisor when the events, env, or cwd
// changed. An actions-only change does not restart it.
func (s *Service) maybeRestart(ctx context.Context, old, next Definition, existed bool) {
	if !existed {
		return
	}
	if reflect.DeepEqual(old.Events, next.Events) && reflect.DeepEqual(old.Env, next.Env) && old.Cwd == next.Cwd {
		return
	}
	if p := s.procByName(next.Name); p == nil || !p.alive() {
		return
	}
	if err := s.Restart(ctx, next.Name); err != nil {
		s.log.Warn("connector restart after update", "connector", next.Name, "error", err)
	}
}

// Delete removes a connector definition and stops its process.
func (s *Service) Delete(ctx context.Context, name string) error {
	if err := s.checkLoaded(); err != nil {
		return err
	}
	if err := s.GuardReserved(name); err != nil {
		return err
	}
	if err := s.GuardMutate(name); err != nil {
		return err
	}
	_ = s.Stop(name) // already-stopped is fine; delete proceeds either way
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	defs := s.snapshotDefs()
	d, ok := defs[name]
	if !ok {
		return fmt.Errorf("connector %q not found: %w", name, ErrNotFound)
	}
	delete(defs, name)
	if err := s.save(defs); err != nil {
		return err
	}
	s.deleteContributedSkills(ctx, name)
	s.deleteOwnedSecrets(d, defs)
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "connector.deleted", Entity: "connector", EntityID: name, Action: "deleted",
	}); err != nil {
		s.log.Warn("bus append", "topic", "connector.deleted", "error", err)
	}
	return nil
}

// deleteOwnedSecrets removes the secrets that the registry install of d wrote,
// when no remaining connector refers to them. A failure is logged and does not
// undo the connector delete.
func (s *Service) deleteOwnedSecrets(d Definition, remaining map[string]Definition) {
	if d.Source == nil {
		return
	}
	for _, n := range d.Source.Secrets {
		used := false
		for _, other := range remaining {
			if other.refersToSecret(n) {
				used = true
				break
			}
		}
		if used {
			continue
		}
		if err := secrets.Delete(n); err != nil {
			s.log.Warn("delete owned secret", "connector", d.Name, "secret", n, "error", err)
		}
	}
}

// deleteContributedSkills removes the skills this connector installed
// (Part H.4). A failure is logged and does not undo the connector delete.
func (s *Service) deleteContributedSkills(ctx context.Context, name string) {
	if s.sk == nil {
		return
	}
	removed, err := s.sk.RemoveConnectorSkills(ctx, name)
	if err != nil {
		s.log.Warn("delete contributed skills", "connector", name, "error", err)
		return
	}
	if len(removed) > 0 {
		s.log.Info("connector skills removed", "connector", name, "skills", strings.Join(removed, ","))
	}
}

// List returns definitions merged with live status, unioned with inherited
// MCP servers. One row shape covers managed, inherited, and native rows.
func (s *Service) List() []map[string]any {
	defs := s.snapshotDefs()
	positions, lastEvents := s.eventMeta()
	s.mu.Lock()
	procs := make(map[string]*proc, len(s.procs))
	for name, p := range s.procs {
		procs[name] = p
	}
	s.mu.Unlock()

	var out []map[string]any
	fpIndex := map[string]int{}
	for _, name := range sortedNames(defs) {
		d := defs[name]
		row := s.managedRow(name, d, procs[name], positions[name], lastEvents[name])
		if d.Actions != nil && d.Actions.MCP != nil {
			if fp := d.Actions.MCP.fingerprint(); fp != "" {
				fpIndex[fp] = len(out)
			}
		}
		out = append(out, row)
	}
	return s.unionInherited(out, fpIndex)
}

// eventMeta reads every connector cursor and last event time in two queries.
func (s *Service) eventMeta() (map[string]Cursor, map[string]string) {
	positions := map[string]Cursor{}
	lastEvents := map[string]string{}
	if s.st == nil {
		return positions, lastEvents
	}
	if rows, err := s.st.DB().Query(`SELECT instance, last_event_id, updated_at FROM connector_cursors`); err == nil {
		for rows.Next() {
			var c Cursor
			if rows.Scan(&c.Name, &c.LastEventID, &c.UpdatedAt) == nil {
				positions[c.Name] = c
			}
		}
		rows.Close()
	}
	if rows, err := s.st.DB().Query(`SELECT connector, MAX(received_at) FROM events GROUP BY connector`); err == nil {
		for rows.Next() {
			var name, at string
			if rows.Scan(&name, &at) == nil {
				lastEvents[name] = at
			}
		}
		rows.Close()
	}
	return positions, lastEvents
}

// managedRow builds the wire row of a managed definition.
func (s *Service) managedRow(name string, d Definition, p *proc, pos Cursor, lastEventAt string) map[string]any {
	row := map[string]any{
		"id":           name,
		"name":         name,
		"provenance":   "managed",
		"managed":      true,
		"capabilities": d.Capabilities(),
		"env":          maskValues(d.Env),
		"cwd":          d.Cwd,
		"source":       d.Source,
	}
	if d.Actions != nil {
		row["actions"] = s.actionsRow(d.Actions)
	}
	if d.Events != nil {
		row["events"] = eventsRow(*d.Events, p, pos, lastEventAt)
	}
	if d.Channel != nil {
		row["channel"] = channelRow(*d.Channel)
	}
	return row
}

// actionsRow renders the actions block; missing CLI commands are computed on
// read, and MCP env and header values are masked (see maskValues).
func (s *Service) actionsRow(a *Actions) map[string]any {
	row := map[string]any{"mcp": nil, "cli": nil}
	if a.MCP != nil {
		m := map[string]any{
			"command": a.MCP.Command,
			"args":    orEmptySlice(a.MCP.Args),
			"env":     maskValues(a.MCP.Env),
			"url":     a.MCP.URL,
			"headers": maskValues(a.MCP.Headers),
		}
		row["mcp"] = m
	}
	if a.CLI != nil {
		missing := []string{}
		for _, cmd := range a.CLI.Commands {
			if _, err := exec.LookPath(cmd); err != nil {
				missing = append(missing, cmd)
			}
		}
		row["cli"] = map[string]any{
			"commands": orEmptySlice(a.CLI.Commands),
			"skill":    a.CLI.Skill,
			"missing":  missing,
		}
	}
	return row
}

func orEmptySlice(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// eventsRow renders the events block plus its live state.
func eventsRow(e Events, p *proc, pos Cursor, lastEventAt string) map[string]any {
	row := map[string]any{
		"command":       e.Command,
		"format":        e.Format,
		"restart":       e.Restart,
		"id":            e.ID,
		"ts":            e.TS,
		"identity":      e.Identity,
		"status":        "stopped",
		"restarts":      0,
		"last_err":      "",
		"last_event_id": pos.LastEventID,
		"last_event_at": lastEventAt,
		"counters":      map[string]any{"events": int64(0), "duplicates": int64(0), "parse_errors": int64(0)},
	}
	if e.Poll > 0 {
		row["poll_ms"] = int64(e.Poll.Std() / time.Millisecond)
	}
	if e.Catchup != nil {
		row["catchup"] = map[string]any{
			"command":    e.Catchup.Command,
			"max_events": e.Catchup.MaxEvents,
			"timeout_ms": int64(e.Catchup.Timeout.Std() / time.Millisecond),
		}
	}
	if p != nil {
		snap := p.snapshot()
		row["status"] = snap.status
		row["restarts"] = snap.restarts
		row["last_err"] = snap.lastErr
		row["counters"] = map[string]any{
			"events": snap.events, "duplicates": snap.duplicates, "parse_errors": snap.parseErrors,
		}
		if snap.pid != 0 {
			row["pid"] = snap.pid
		}
		if !snap.startedAt.IsZero() {
			row["started_at"] = snap.startedAt.UTC().Format(time.RFC3339Nano)
		}
		if !snap.lastEventAt.IsZero() {
			row["last_event_at"] = snap.lastEventAt.UTC().Format(time.RFC3339Nano)
		}
		if snap.lastCatchup != nil {
			row["last_catchup"] = map[string]any{
				"at":             snap.lastCatchup.At.UTC().Format(time.RFC3339Nano),
				"replayed":       snap.lastCatchup.Replayed,
				"skipped_reason": snap.lastCatchup.Reason,
			}
		}
	}
	return row
}

// channelRow renders the channel block.
func channelRow(c Channel) map[string]any {
	row := map[string]any{
		"ref": c.Ref, "send": c.Send, "typing": c.Typing, "ack": c.Ack, "modes": c.Modes(),
	}
	if c.TypingInterval > 0 {
		row["typing_interval_ms"] = int64(c.TypingInterval.Std() / time.Millisecond)
	}
	if c.TypingMax > 0 {
		row["typing_max_ms"] = int64(c.TypingMax.Std() / time.Millisecond)
	}
	return row
}

// unionInherited appends discovered MCP servers and folds the metadata of a
// managed connector that already declares the same server.
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
			"id": id, "name": srv.Name, "provenance": "inherited", "managed": false,
			"capabilities": []string{"actions"},
			"actions": map[string]any{"mcp": map[string]any{
				"command": srv.Command, "args": []string{},
				"url": srv.URL, "transport": srv.Transport,
				"env": map[string]string{}, "headers": map[string]string{},
			}, "cli": nil},
			"transport": srv.Transport, "harnesses": srv.Harnesses,
			"injected": srv.Injected, "origins": srv.Origins,
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

// Definition returns the stored definition of a managed connector.
func (s *Service) Definition(name string) (Definition, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.defs[name]
	return d, ok
}

// Channel returns the channel block of a managed connector, its expanded
// env, and whether the connector exists with a channel.
func (s *Service) Channel(name string) (Channel, map[string]string, bool) {
	d, ok := s.Definition(name)
	if !ok || d.Channel == nil {
		return Channel{}, nil, false
	}
	env, _ := secrets.ExpandRefs(d.Env)
	return *d.Channel, env, true
}

// Cwd returns the working directory of a managed connector, or "".
func (s *Service) Cwd(name string) string {
	d, ok := s.Definition(name)
	if !ok {
		return ""
	}
	return d.Cwd
}

// GuardReserved refuses definition and lifecycle writes against a native
// connector's name. The native service still owns its event and cursor rows,
// so InjectEvent and PutCursor do not call it.
func (s *Service) GuardReserved(name string) error {
	if reserved[name] {
		return fmt.Errorf("%w: %q is a native connector", ErrReserved, name)
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

// RewritePathPrefix updates connector cwd, commands, and env that point at
// old. dryRun reports the names without writing. Names are sorted.
func (s *Service) RewritePathPrefix(old, new string, dryRun bool) ([]string, error) {
	if s == nil || strings.TrimSpace(old) == "" || old == new {
		return nil, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.Load(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoadFailed, err)
	}
	defs := s.snapshotDefs()
	var names []string
	rewrite := func(v string) (string, bool) {
		next, ok := workspaces.RewritePathPrefix(v, old, new)
		return next, ok
	}
	for name, d := range defs {
		changed := false
		if next, ok := rewrite(d.Cwd); ok {
			d.Cwd = next
			changed = true
		}
		if len(d.Env) > 0 {
			env := make(map[string]string, len(d.Env))
			for k, v := range d.Env {
				if next, ok := rewrite(v); ok {
					env[k] = next
					changed = true
				} else {
					env[k] = v
				}
			}
			d.Env = env
		}
		if d.Events != nil {
			if next, ok := rewrite(d.Events.Command); ok {
				d.Events.Command = next
				changed = true
			}
			if d.Events.Catchup != nil {
				if next, ok := rewrite(d.Events.Catchup.Command); ok {
					d.Events.Catchup.Command = next
					changed = true
				}
			}
		}
		if d.Channel != nil {
			for dst, src := range map[*string]string{
				&d.Channel.Send:   d.Channel.Send,
				&d.Channel.Typing: d.Channel.Typing,
				&d.Channel.Ack:    d.Channel.Ack,
			} {
				if next, ok := rewrite(src); ok {
					*dst = next
					changed = true
				}
			}
		}
		if d.Actions != nil && d.Actions.MCP != nil {
			if next, ok := rewrite(d.Actions.MCP.Command); ok {
				d.Actions.MCP.Command = next
				changed = true
			}
			for i, arg := range d.Actions.MCP.Args {
				if next, ok := rewrite(arg); ok {
					d.Actions.MCP.Args[i] = next
					changed = true
				}
			}
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
	return names, s.save(defs)
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
		`SELECT last_event_id, updated_at FROM connector_cursors WHERE instance=?`, name).
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
	now := s.now().UTC().Format(time.RFC3339)
	_, err := s.st.DB().ExecContext(ctx, `INSERT INTO connector_cursors (instance, last_event_id, updated_at)
		VALUES (?,?,?) ON CONFLICT(instance) DO UPDATE SET last_event_id=excluded.last_event_id, updated_at=excluded.updated_at`,
		name, lastEventID, now)
	return err
}

// appendConnectorLog appends one line to the connector log file.
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
