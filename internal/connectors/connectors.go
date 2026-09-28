// Package connectors loads connector entries, resolves their manifests
// through the catalog, global, and workspace layers, and supervises one
// process per instance. A connector is one external system with up to three
// capabilities: actions, events, and a channel.
package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
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

// StatusHook is told about each adapter state change of an instance. The
// daemon wires it to the outbox, so held rows wake on connected.
type StatusHook func(ctx context.Context, instance, state string)

// Service stores connector entries and supervises their instances.
type Service struct {
	st    *store.Store
	ws    *workspaces.Service
	sk    *skills.Service
	queue *dispatch.Queue
	log   *slog.Logger

	mu        sync.Mutex
	procs     map[string]*proc // by instance
	entries   map[string]Entry
	overrides map[string]map[string]workspaces.ConnectorOverride // workspace → name
	flows     map[string]*Flow                                   // by instance and flow id
	loadErr   error                                              // set by a failed Load; blocks writes
	started   bool                                               // StartAll ran; reconcile starts new instances
	writeMu   sync.Mutex                                         // held across read, check, and save of a write
	runCtx    context.Context                                    // process lifetime; not the caller's (HTTP) context
	userHome  string                                             // empty: skip MCP discovery (tests)
	version   string                                             // daemon version for initialize
	onStatus  StatusHook
	wg        sync.WaitGroup

	catalog   Catalog
	relHTTP   *http.Client
	githubURL string
	updates   map[string]string    // name → newer catalog version on its track
	inspected map[string]inspected // resolved fingerprint → MCP inspection

	// now, sleep, and the session intervals are injectable for tests.
	now       func() time.Time
	sleep     func(context.Context, time.Duration) bool
	pingEvery time.Duration
	authEvery time.Duration

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
	Sender   string
	Type     string
}

// eventRecord is one persisted ingress event.
type eventRecord struct {
	id         string
	instance   string
	raw        string
	trace      string
	receivedAt string
	payload    any
	sourceID   string
	identity   string
	sender     string
	eventType  string
	room       string
	thread     string
	catchUp    bool
}

// EntryPatch updates a global entry. A present field replaces the stored
// value. A null patch or manifest removes it. A masked input ("***") keeps
// the stored value.
type EntryPatch struct {
	Version  *string            `json:"version"`
	Track    *string            `json:"track"`
	Inputs   *map[string]string `json:"inputs"`
	Patch    json.RawMessage    `json:"patch"`
	Manifest json.RawMessage    `json:"manifest"`
}

// New creates a connectors service.
func New(st *store.Store, ws *workspaces.Service, sk *skills.Service) *Service {
	return &Service{
		st: st, ws: ws, sk: sk,
		queue:     dispatch.NewQueue(st),
		log:       slog.Default().With("source", "connectors"),
		procs:     make(map[string]*proc),
		entries:   make(map[string]Entry),
		overrides: map[string]map[string]workspaces.ConnectorOverride{},
		flows:     map[string]*Flow{},
		updates:   map[string]string{},
		inspected: map[string]inspected{},
		runCtx:    context.Background(),
		now:       time.Now,
		sleep:     sleepCtx,
		pingEvery: 30 * time.Second,
		authEvery: 5 * time.Minute,
	}
}

// WithUserHome sets the home scanned for inherited MCP servers. Empty skips.
func (s *Service) WithUserHome(dir string) *Service {
	s.userHome = dir
	return s
}

// WithVersion sets the daemon version that initialize sends.
func (s *Service) WithVersion(v string) *Service {
	s.version = v
	return s
}

// OnStatus sets the hook for adapter state changes.
func (s *Service) OnStatus(h StatusHook) *Service {
	s.onStatus = h
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

// Load reads connectors.yaml and the workspace layers into memory. The file
// is strict: an unknown key fails the load with the file path, the connector
// name, and the key. A failed load keeps the last good entries and blocks
// writes until a load succeeds.
func (s *Service) Load() error {
	entries, err := readEntries()
	overrides := s.loadOverrides(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overrides = overrides
	s.loadErr = err
	if err != nil {
		return err
	}
	s.entries = entries
	return nil
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

// save writes the entries back to connectors.yaml with a temp-and-rename
// write. The file has mode 0600, because it can hold literal tokens. The
// caller holds writeMu.
func (s *Service) save(entries map[string]Entry) error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	b, err := yaml.Marshal(entries)
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
		_ = os.Remove(tmp) // best effort; the write already failed
		return fmt.Errorf("save connectors: %w", werr)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best effort; the rename already failed
		return fmt.Errorf("save connectors: %w", err)
	}
	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
	return nil
}

// snapshotEntries returns a deep copy of the entries.
func (s *Service) snapshotEntries() map[string]Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Entry, len(s.entries))
	for k, v := range s.entries {
		out[k] = v.clone()
	}
	return out
}

// Entry returns the stored global entry of a connector.
func (s *Service) Entry(name string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[name]
	if !ok {
		return Entry{}, false
	}
	return e.clone(), true
}

// emitWrite appends a connector.created/updated bus event with the
// capabilities of the resolved manifest.
func (s *Service) emitWrite(ctx context.Context, topic string, r Resolved) {
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: topic, Entity: "connector", EntityID: r.Name, Action: strings.TrimPrefix(topic, "connector."),
		Attrs: map[string]string{"capabilities": strings.Join(r.Manifest.Capabilities(), ",")},
	}); err != nil {
		s.log.Warn("bus append", "topic", topic, "error", err)
	}
}

// checkEntry validates an entry and resolves its global layer.
func (s *Service) checkEntry(e Entry) (Resolved, error) {
	if err := e.Validate(); err != nil {
		return Resolved{}, err
	}
	return resolveEntry(e.clone(), "", nil)
}

// Create registers a new connector entry. An existing name is ErrExists.
func (s *Service) Create(ctx context.Context, e Entry) error {
	if err := s.checkLoaded(); err != nil {
		return err
	}
	if err := s.GuardReserved(e.Name); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	entries := s.snapshotEntries()
	if _, ok := entries[e.Name]; ok {
		return fmt.Errorf("%w: %q", ErrExists, e.Name)
	}
	return s.commitEntry(ctx, entries, e, Entry{}, false)
}

// Replace writes the whole entry of a connector, creating it when missing.
// A masked input ("***") keeps the stored value.
func (s *Service) Replace(ctx context.Context, e Entry) error {
	if err := s.checkLoaded(); err != nil {
		return err
	}
	if err := s.GuardReserved(e.Name); err != nil {
		return err
	}
	if err := s.GuardMutate(e.Name); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	entries := s.snapshotEntries()
	old, existed := entries[e.Name]
	e.Inputs = maps.Clone(e.Inputs)
	if err := keepMasked(e.Name, e.Inputs, old.Inputs); err != nil {
		return err
	}
	if len(e.InstalledSecrets) == 0 {
		e.InstalledSecrets = old.InstalledSecrets
	}
	return s.commitEntry(ctx, entries, e, old, existed)
}

// Patch applies a field update to an existing entry.
func (s *Service) Patch(ctx context.Context, name string, p EntryPatch) error {
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
	entries := s.snapshotEntries()
	old, ok := entries[name]
	if !ok {
		return fmt.Errorf("connector %q not found: %w", name, ErrNotFound)
	}
	e := old.clone()
	if err := p.apply(&e); err != nil {
		return err
	}
	if err := keepMasked(name, e.Inputs, old.Inputs); err != nil {
		return err
	}
	return s.commitEntry(ctx, entries, e, old, true)
}

// apply writes the present fields of p into e.
func (p EntryPatch) apply(e *Entry) error {
	bad := func(field string, err error) error {
		return &ValidationError{Name: e.Name, Fields: []FieldError{{Field: field, Message: err.Error()}}}
	}
	if p.Version != nil {
		e.Version = *p.Version
	}
	if p.Track != nil {
		e.Track = manifest.Track(*p.Track)
	}
	if p.Inputs != nil {
		e.Inputs = maps.Clone(*p.Inputs)
	}
	if len(p.Patch) > 0 {
		e.Patch = nil
		if !isNull(p.Patch) {
			if err := json.Unmarshal(p.Patch, &e.Patch); err != nil {
				return bad("patch", err)
			}
		}
	}
	if len(p.Manifest) > 0 {
		e.Manifest = nil
		if !isNull(p.Manifest) {
			var m manifest.Manifest
			if err := decodeStrict(p.Manifest, &m); err != nil {
				return bad("manifest", err)
			}
			e.Manifest = &m
		}
	}
	return nil
}

func isNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// commitEntry validates, stores secret inputs, saves, emits, and reconciles.
// The caller holds writeMu.
func (s *Service) commitEntry(ctx context.Context, entries map[string]Entry, e, old Entry, existed bool) error {
	r, err := s.checkEntry(e)
	if err != nil {
		return err
	}
	owned := func(n string) bool {
		return slices.Contains(e.InstalledSecrets, n) || slices.Contains(old.InstalledSecrets, n)
	}
	created, err := storeSecretInputs(e.Name, r.Manifest, e.Inputs, owned)
	if err != nil {
		return err
	}
	for _, n := range created {
		if !slices.Contains(e.InstalledSecrets, n) {
			e.InstalledSecrets = append(e.InstalledSecrets, n)
		}
	}
	entries[e.Name] = e
	if err := s.save(entries); err != nil {
		return err
	}
	topic := "connector.updated"
	if !existed {
		topic = "connector.created"
	}
	s.emitWrite(ctx, topic, r)
	s.syncSkill(ctx, r)
	s.reconcile(ctx)
	return nil
}

// storeSecretInputs writes each literal value of a secret input to the
// secret <prefix>_<NAME> and puts the reference in inputs. It returns the
// names it wrote. It does not overwrite a secret that owned rejects: that
// secret belongs to the user.
func storeSecretInputs(prefix string, m manifest.Manifest, inputs map[string]string, owned func(string) bool) ([]string, error) {
	if m.Setup == nil {
		return nil, nil
	}
	var out []string
	for _, in := range m.Setup.Inputs {
		v, ok := inputs[in.Name]
		if !in.Secret || !ok || v == "" || strings.HasPrefix(v, "secret:") {
			continue
		}
		name := prefix + "_" + in.Name
		if err := secrets.CheckName(name); err != nil {
			return out, &ValidationError{Name: m.Name, Fields: []FieldError{{Field: "inputs", Message: err.Error()}}}
		}
		if _, err := secrets.Get(name); err == nil && !owned(name) {
			msg := fmt.Sprintf("secret %s exists; give secret:%s to use it, or delete it first", name, name)
			return out, &ValidationError{Name: m.Name, Fields: []FieldError{{Field: "inputs." + in.Name, Message: msg}}}
		}
		if err := secrets.Set(name, v); err != nil {
			return out, err
		}
		inputs[in.Name] = "secret:" + name
		out = append(out, name)
	}
	return out, nil
}

// syncSkill copies the skill folder of the global instance into the managed
// skills, so the harness homes project it (Part C.6).
func (s *Service) syncSkill(ctx context.Context, r Resolved) {
	dir := r.SkillDir()
	if s.sk == nil || dir == "" {
		return
	}
	if _, err := s.sk.SyncConnectorSkill(ctx, r.Name, dir, r.Name); err != nil {
		s.log.Warn("connector skill", "connector", r.Name, "error", err)
	}
}

// Delete removes a connector entry and stops its instances.
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	entries := s.snapshotEntries()
	e, ok := entries[name]
	if !ok {
		return fmt.Errorf("connector %q not found: %w", name, ErrNotFound)
	}
	delete(entries, name)
	if err := s.save(entries); err != nil {
		return err
	}
	s.reconcile(ctx)
	s.deleteContributedSkills(ctx, name)
	s.deleteOwnedSecrets(e, entries)
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "connector.deleted", Entity: "connector", EntityID: name, Action: "deleted",
	}); err != nil {
		s.log.Warn("bus append", "topic", "connector.deleted", "error", err)
	}
	return nil
}

// deleteOwnedSecrets removes the secrets that the install of e wrote, when no
// remaining entry refers to them. A failure is logged and does not undo the
// connector delete.
func (s *Service) deleteOwnedSecrets(e Entry, remaining map[string]Entry) {
	for _, n := range e.InstalledSecrets {
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
			s.log.Warn("delete owned secret", "connector", e.Name, "secret", n, "error", err)
		}
	}
}

// deleteContributedSkills removes the skills this connector installed. A
// failure is logged and does not undo the connector delete.
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

// Override returns the workspace layer of one connector.
func (s *Service) Override(ctx context.Context, workspaceID, name string) (workspaces.ConnectorOverride, bool, error) {
	if s.ws == nil {
		return workspaces.ConnectorOverride{}, false, nil
	}
	all, err := s.ws.ConnectorOverrides(ctx, workspaceID)
	if err != nil {
		return workspaces.ConnectorOverride{}, false, err
	}
	ov, ok := all[name]
	return ov, ok, nil
}

// SetOverride writes, or with nil removes, the workspace layer of a
// connector (Part C.2). A workspace cannot add a connector that is not
// installed globally. A literal secret input becomes the secret
// <name>@<ws>_<NAME>.
func (s *Service) SetOverride(ctx context.Context, workspaceID, name string, ov *workspaces.ConnectorOverride) error {
	if s.ws == nil {
		return errors.New("workspaces are not available")
	}
	if err := s.GuardReserved(name); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	e, ok := s.Entry(name)
	if !ok {
		return fmt.Errorf("connector %q not found: %w", name, ErrNotFound)
	}
	if ov != nil {
		prev, _, err := s.Override(ctx, workspaceID, name)
		if err != nil {
			return err
		}
		ov.Inputs = maps.Clone(ov.Inputs)
		if err := keepMasked(name, ov.Inputs, prev.Inputs); err != nil {
			return err
		}
		if ov.Track != "" && !manifest.Track(ov.Track).Valid() {
			return &ValidationError{Name: name, Layer: LayerWorkspace, Fields: []FieldError{{Field: "track", Message: "must be exact, patch, minor, or latest"}}}
		}
		for k := range ov.Inputs {
			if !envKeyRE.MatchString(k) {
				return &ValidationError{Name: name, Layer: LayerWorkspace, Fields: []FieldError{{Field: "inputs", Message: fmt.Sprintf("key %q must match %s", k, envKeyRE)}}}
			}
		}
		if !ov.Disabled {
			r, err := resolveEntry(e, workspaceID, ov)
			if err != nil {
				return err
			}
			all := func(string) bool { return true } // the override namespace belongs to Trebi
			if _, err := storeSecretInputs(name+"@"+workspaceID, r.Manifest, ov.Inputs, all); err != nil {
				return err
			}
		}
	}
	if err := s.ws.SetConnectorOverride(ctx, workspaceID, name, ov); err != nil {
		return err
	}
	overrides := s.loadOverrides(ctx)
	s.mu.Lock()
	s.overrides = overrides
	s.mu.Unlock()
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "connector.updated", Entity: "connector", EntityID: name, Action: "updated",
		Attrs: map[string]string{"workspace": workspaceID},
	}); err != nil {
		s.log.Warn("bus append", "topic", "connector.updated", "error", err)
	}
	s.reconcile(ctx)
	return nil
}

// List returns the resolved rows, with live status, unioned with inherited
// MCP servers. With a workspace, each row is resolved for that workspace.
func (s *Service) List(workspaceID string) []map[string]any {
	entries := s.snapshotEntries()
	positions, lastEvents := s.eventMeta()
	var out []map[string]any
	fpIndex := map[string]int{}
	for _, name := range sortedNames(entries) {
		row := s.managedRow(entries[name], workspaceID, positions, lastEvents)
		if fp, ok := row["mcp_fingerprint"].(string); ok && fp != "" {
			fpIndex[fp] = len(out)
		}
		delete(row, "mcp_fingerprint")
		out = append(out, row)
	}
	return s.unionInherited(out, fpIndex)
}

// eventMeta reads every connector cursor and last event time in two queries,
// keyed by instance.
func (s *Service) eventMeta() (map[string]Cursor, map[string]string) {
	positions := map[string]Cursor{}
	lastEvents := map[string]string{}
	if s.st == nil {
		return positions, lastEvents
	}
	if rows, err := s.st.DB().Query(`SELECT instance, last_event_id, updated_at FROM connector_cursors`); err == nil {
		for rows.Next() {
			var c Cursor
			if rows.Scan(&c.Instance, &c.LastEventID, &c.UpdatedAt) == nil {
				positions[c.Instance] = c
			}
		}
		_ = rows.Close() // read-only query
	}
	if rows, err := s.st.DB().Query(`SELECT instance, MAX(received_at) FROM events GROUP BY instance`); err == nil {
		for rows.Next() {
			var name, at string
			if rows.Scan(&name, &at) == nil {
				lastEvents[name] = at
			}
		}
		_ = rows.Close() // read-only query
	}
	return positions, lastEvents
}

// managedRow builds the wire row of one entry, resolved for a workspace.
func (s *Service) managedRow(e Entry, workspaceID string, positions map[string]Cursor, lastEvents map[string]string) map[string]any {
	row := map[string]any{
		"id":                e.Name,
		"name":              e.Name,
		"provenance":        "managed",
		"managed":           true,
		"from":              e.From,
		"track":             string(e.track()),
		"inputs":            MaskInputs(e.Inputs),
		"installed_secrets": orEmptySlice(e.InstalledSecrets),
		"capabilities":      []string{},
		"instances":         []map[string]any{},
		"update":            nil,
	}
	if e.Version != "" {
		row["version"] = e.Version
	}
	if v := s.availableUpdate(e.Name); v != "" {
		row["update"] = map[string]any{"version": v}
	}
	r, err := s.Resolve(e.Name, workspaceID)
	if err != nil {
		row["resolve_error"] = err.Error()
		return row
	}
	m := r.Manifest
	row["title"] = m.Title
	row["description"] = m.Description
	row["icon"] = m.Icon
	row["version"] = r.Version
	row["track"] = string(r.Track)
	row["publisher"] = m.Publisher
	row["warning"] = m.Warning
	row["manifest"] = m
	row["inputs"] = MaskInputs(r.Inputs)
	row["capabilities"] = m.Capabilities()
	if workspaceID != "" {
		row["workspace"] = workspaceID
		row["instance"] = r.Instance
	}
	if m.Actions != nil {
		row["actions"] = actionsRow(r)
		if srv, _, err := r.MCPServer(); err == nil && srv != nil {
			transport := "stdio"
			if srv.URL != "" {
				transport = "http"
			}
			row["mcp_fingerprint"] = harness.MCPFingerprint(transport, srv.Command, srv.Args, srv.URL)
		}
	}
	instances := s.instanceRows(e.Name, workspaceID, r, positions, lastEvents)
	row["instances"] = instances
	if m.Events != nil {
		ev := eventsRow(*m.Events)
		if len(instances) > 0 {
			maps.Copy(ev, instances[0])
		}
		row["events"] = ev
	}
	if m.Channel != nil {
		row["channel"] = map[string]any{
			"features": orEmptySlice(m.Channel.Features),
			"limits":   m.Channel.Limits,
			"modes":    m.Modes(),
		}
	}
	if p := s.procByName(r.Instance); p != nil {
		snap := p.snapshot()
		row["auth"] = snap.authRow()
		row["features"] = orEmptySlice(snap.features())
		row["limits"] = snap.limits(m)
	} else {
		row["features"] = orEmptySlice(m.Features())
		if m.Channel != nil {
			row["limits"] = m.Channel.Limits
		}
	}
	return row
}

// instanceRows lists the instance that serves the workspace, or with no
// workspace every instance of the connector.
func (s *Service) instanceRows(name, workspaceID string, r Resolved, positions map[string]Cursor, lastEvents map[string]string) []map[string]any {
	if r.Manifest.Events == nil {
		return []map[string]any{}
	}
	keys := []string{r.Instance}
	if workspaceID == "" {
		s.mu.Lock()
		for key := range s.procs {
			if n, ws := splitInstance(key); n == name && ws != "" {
				keys = append(keys, key)
			}
		}
		s.mu.Unlock()
		slices.Sort(keys[1:])
	}
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		_, ws := splitInstance(key)
		row := map[string]any{
			"key": key, "workspace": ws, "status": statusStopped, "restarts": 0, "last_err": "",
			"last_event_id": positions[key].LastEventID, "last_event_at": lastEvents[key],
			"counters": map[string]any{"events": int64(0), "duplicates": int64(0), "parse_errors": int64(0)},
		}
		if p := s.procByName(key); p != nil {
			p.snapshot().fill(row)
		}
		out = append(out, row)
	}
	return out
}

// actionsRow renders the actions block: the CLI commands with the missing
// ones, and the MCP server with masked values.
func actionsRow(r Resolved) map[string]any {
	a := r.Manifest.Actions
	row := map[string]any{"mcp": nil, "cli": nil}
	if a.MCP != nil {
		row["mcp"] = map[string]any{
			"registry": a.MCP.Registry, "name": a.MCP.Name, "version": a.MCP.Version,
			"command": a.MCP.Command, "args": orEmptySlice(a.MCP.Args),
			"env": MaskInputs(a.MCP.Env), "url": a.MCP.URL, "headers": MaskInputs(a.MCP.Headers),
		}
	}
	if a.CLI != nil {
		path := r.PathEnv(os.Getenv("PATH"))
		missing := []string{}
		for _, cmd := range a.CLI.Commands {
			if lookPath(cmd, path) == "" {
				missing = append(missing, cmd)
			}
		}
		row["cli"] = map[string]any{
			"commands": orEmptySlice(a.CLI.Commands),
			"skill":    r.Name,
			"missing":  missing,
		}
	}
	return row
}

// lookPath finds cmd on the given PATH, or returns "".
func lookPath(cmd, path string) string {
	if strings.Contains(cmd, "/") {
		if _, err := os.Stat(cmd); err == nil {
			return cmd
		}
		return ""
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, cmd)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	if p, err := exec.LookPath(cmd); err == nil && path == "" {
		return p
	}
	return ""
}

// eventsRow renders the events block of the manifest.
func eventsRow(e manifest.Events) map[string]any {
	row := map[string]any{
		"protocol": e.Protocol,
		"command":  e.Command,
		"restart":  e.Restart,
		"types":    orEmptySlice(e.Types),
	}
	if e.Protocol == manifest.ProtocolLines {
		row["format"] = e.Format
		row["id"] = e.ID
		row["ts"] = e.TS
		row["identity"] = e.Identity
		if e.Poll > 0 {
			row["poll_ms"] = e.Poll.Std().Milliseconds()
		}
		if e.Catchup != nil {
			row["catchup"] = map[string]any{
				"command":    e.Catchup.Command,
				"max_events": e.Catchup.MaxEvents,
				"timeout_ms": e.Catchup.Timeout.Std().Milliseconds(),
			}
		}
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

// Get returns one connector by id or unique name, resolved for a workspace.
func (s *Service) Get(name string) (map[string]any, error) {
	return s.GetFor(name, "")
}

// GetFor returns one connector by id or unique name, resolved for a
// workspace. An empty workspace resolves the global layer.
func (s *Service) GetFor(name, workspaceID string) (map[string]any, error) {
	list := s.List(workspaceID)
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

// HasEvents reports whether the global layer of a managed connector has an
// events block. A missing name is ErrNotFound.
func (s *Service) HasEvents(name string) error {
	if _, ok := s.Entry(name); !ok {
		if _, err := s.Get(name); err != nil {
			return err
		}
		return fmt.Errorf("%w: connector %q is not managed", ErrInherited, name)
	}
	r, err := s.Resolve(name, "")
	if err != nil {
		return err
	}
	if r.Manifest.Events == nil {
		return fmt.Errorf("%w: %q", ErrNoEvents, name)
	}
	return nil
}

// IsChannel reports whether the global layer of a connector is a channel
// that can hold a conversation.
func (s *Service) IsChannel(name string) bool {
	r, err := s.Resolve(name, "")
	return err == nil && slices.Contains(r.Manifest.Modes(), manifest.ModeReply)
}

// GuardReserved refuses entry and lifecycle writes against a native
// connector's name. The native service still owns its event and cursor rows,
// so InjectEvent and PutCursor do not call it.
func (s *Service) GuardReserved(name string) error {
	if manifest.Reserved(name) {
		return fmt.Errorf("%w: %q is a native connector", ErrReserved, name)
	}
	return nil
}

// GuardMutate refuses writes against inherited MCP rows. Missing names pass
// through so Start/Delete keep their existing missing-name behavior.
func (s *Service) GuardMutate(name string) error {
	if _, ok := s.Entry(name); ok {
		return nil
	}
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
	if managed, _ := row["managed"].(bool); managed {
		return nil
	}
	if path := inheritedOriginPath(row); path != "" {
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

// RewritePathPrefix updates the file sources, inline commands, and input
// values that point at old. dryRun reports the names without writing. Names
// are sorted.
func (s *Service) RewritePathPrefix(old, new string, dryRun bool) ([]string, error) {
	if s == nil || strings.TrimSpace(old) == "" || old == new {
		return nil, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.Load(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoadFailed, err)
	}
	entries := s.snapshotEntries()
	var names []string
	for name, e := range entries {
		if e.rewritePaths(old, new) {
			entries[name] = e
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if dryRun || len(names) == 0 {
		return names, nil
	}
	return names, s.save(entries)
}

// rewritePaths rewrites every path field of e that starts with old.
func (e *Entry) rewritePaths(old, new string) bool {
	changed := false
	rewrite := func(p *string) {
		if next, ok := workspaces.RewritePathPrefix(*p, old, new); ok {
			*p = next
			changed = true
		}
	}
	if path := e.FilePath(); path != "" {
		if next, ok := workspaces.RewritePathPrefix(path, old, new); ok {
			e.From = fromFile + next
			changed = true
		}
	}
	for k, v := range e.Inputs {
		rewrite(&v)
		e.Inputs[k] = v
	}
	if m := e.Manifest; m != nil {
		if m.Events != nil {
			rewrite(&m.Events.Command)
			if m.Events.Catchup != nil {
				rewrite(&m.Events.Catchup.Command)
			}
		}
		if m.Actions != nil && m.Actions.MCP != nil {
			rewrite(&m.Actions.MCP.Command)
			for i := range m.Actions.MCP.Args {
				rewrite(&m.Actions.MCP.Args[i])
			}
		}
	}
	return changed
}

// Cursor is one connector_cursors row.
type Cursor struct {
	Instance    string
	LastEventID string
	UpdatedAt   string
}

// GetCursor returns the stored cursor of an instance, or a zero LastEventID
// when missing.
func (s *Service) GetCursor(ctx context.Context, instance string) (Cursor, error) {
	c := Cursor{Instance: instance}
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT last_event_id, updated_at FROM connector_cursors WHERE instance=?`, instance).
		Scan(&c.LastEventID, &c.UpdatedAt)
	if err != nil {
		return c, nil
	}
	return c, nil
}

// PutCursor upserts the connector cursor of an instance. Not a bus entity.
func (s *Service) PutCursor(ctx context.Context, instance, lastEventID string) error {
	name, _ := splitInstance(instance)
	if err := s.GuardMutate(name); err != nil {
		return err
	}
	now := s.now().UTC().Format(time.RFC3339)
	_, err := s.st.DB().ExecContext(ctx, `INSERT INTO connector_cursors (instance, last_event_id, updated_at)
		VALUES (?,?,?) ON CONFLICT(instance) DO UPDATE SET last_event_id=excluded.last_event_id, updated_at=excluded.updated_at`,
		instance, lastEventID, now)
	return err
}

// LogPath is the stream log of an instance.
func LogPath(instance string) string {
	return filepath.Join(home.ConnectorsLogDir(), instance+".jsonl")
}

// appendConnectorLog appends one line to the stream log of an instance.
func appendConnectorLog(instance, stream, line string) {
	logPath := LogPath(instance)
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755) // the open below reports the failure
	logf, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(logf, `{"t":%q,"stream":%q,"line":%q}`+"\n", //nolint:errcheck // a lost log line is not an error
		time.Now().UTC().Format(time.RFC3339Nano), stream, line)
	_ = logf.Close()
}
