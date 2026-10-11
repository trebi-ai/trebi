// Package connectors loads connections, resolves their manifests through the
// package, connection, and workspace layers, and supervises one process per
// connection. A connection is one account of a connector type with up to
// three capabilities: actions, events, and a channel.
package connectors

import (
	"bytes"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

var (
	// ErrNotFound is a missing connection.
	ErrNotFound = errors.New("connector not found")
	// ErrInherited is a mutation against a read-only discovered MCP server.
	ErrInherited = errors.New("inherited from user MCP config")
	// ErrReserved is a write against a native connector name.
	ErrReserved = errors.New("connector name is reserved")
	// ErrExists is a create with a slug that another connection has.
	ErrExists = errors.New("connector already exists")
	// ErrNoEvents is a lifecycle action on a connector with no events block.
	ErrNoEvents = errors.New("connector has no events block")
	// ErrLoadFailed is a write to a connection whose folder does not load.
	ErrLoadFailed = errors.New("connection did not load")
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

// StatusHook is told about each adapter state change of a connection. The
// daemon wires it to the outbox, so held rows wake on connected.
type StatusHook func(ctx context.Context, id, state string)

// FloodHook is told about an event that Trebi dropped because the queue of
// the job is full. The daemon wires it to the trigger_flood alert.
type FloodHook func(ctx context.Context, workspaceID, job, connection string)

// Service stores connections and supervises their processes.
type Service struct {
	st    *store.Store
	ws    *workspaces.Service
	sk    *skills.Service
	queue *dispatch.Queue
	log   *slog.Logger

	mu        sync.Mutex
	procs     map[string]*proc                                   // by connection id
	conns     map[string]Connection                              // by connection id
	slugs     map[string]string                                  // slug → connection id
	loadErr   map[string]error                                   // folder name → load error; blocks writes to that connection
	overrides map[string]map[string]workspaces.ConnectorOverride // workspace → slug
	flows     map[string]*Flow                                   // by connection id and flow id
	started   bool                                               // StartAll ran; reconcile starts new connections
	writeMu   sync.Mutex                                         // held across read, check, and save of a write
	runCtx    context.Context                                    // process lifetime; not the caller's (HTTP) context
	userHome  string                                             // empty: skip MCP discovery (tests)
	version   string                                             // daemon version for initialize
	onStatus  StatusHook
	onFlood   FloodHook
	wg        sync.WaitGroup

	catalog   Catalog
	relHTTP   *http.Client
	githubURL string
	updates   map[string]string    // connection id → newer catalog version on its track
	inspected map[string]inspected // resolved fingerprint → MCP inspection

	// now, sleep, and the session intervals are injectable for tests.
	now       func() time.Time
	sleep     func(context.Context, time.Duration) bool
	pingEvery time.Duration
	authEvery time.Duration

	trigMu     sync.Mutex
	trigFP     string
	trigByConn map[string][]cachedTrig

	convs conversationProbe // nil: no conversation is open

	hooks        HookProvider  // nil: no connection gets a hosted hook
	webhookWake  chan struct{} // wakes the webhook queue
	webhookEvery time.Duration
}

// conversationProbe reports whether a job conversation is open (plan A.9).
type conversationProbe interface {
	// within is the limit after the last run; 0 is no limit.
	ConversationOpen(ctx context.Context, workspaceID, job, key string, within time.Duration) (bool, error)
}

// cachedTrig is one event trigger in the index. The index keeps inactive
// jobs and disabled triggers for the dependents list; dispatch skips them.
type cachedTrig struct {
	row      workspaces.JobRow
	j        *job.Job
	tr       job.Trigger
	active   bool
	disabled bool
}

// live reports whether dispatch fires the trigger.
func (c cachedTrig) live() bool { return c.active && !c.disabled }

// NativeMeta carries the native identity of an injected event.
type NativeMeta struct {
	SourceID string
	Identity string
	Room     string
	Thread   string
	Sender   string
	Type     string
	Text     string // what a person reads; contains tests it
}

// eventRecord is one persisted ingress event. connector is the type and
// instance the connection id.
type eventRecord struct {
	id         string
	connector  string
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
	text       string
	replyTo    string
	root       string // the conversation root (plan A.7)
	protocol   bool   // a trebi-connector/1 event; it gets the built-in vars
	bot        bool
	self       bool
	roomName   string
	roomKind   string
	ts         string
	slug       string // the connection slug at dispatch
}

// ConnectionPatch updates a connection. A present field replaces the stored
// value. A null patch or manifest removes it. A masked input ("***") keeps
// the stored value. A new slug rewrites the references to the old one.
type ConnectionPatch struct {
	Slug     *string            `json:"slug"`
	Title    *string            `json:"title"`
	Version  *string            `json:"version"`
	Track    *string            `json:"track"`
	Inputs   *map[string]string `json:"inputs"`
	Patch    json.RawMessage    `json:"patch"`
	Manifest json.RawMessage    `json:"manifest"`
	// Subscriptions replaces the list. JSON null clears it.
	Subscriptions json.RawMessage `json:"subscriptions"`
}

// New creates a connectors service.
func New(st *store.Store, ws *workspaces.Service, sk *skills.Service) *Service {
	return &Service{
		st: st, ws: ws, sk: sk,
		queue:     dispatch.NewQueue(st),
		log:       slog.Default().With("source", "connectors"),
		procs:     make(map[string]*proc),
		conns:     map[string]Connection{},
		slugs:     map[string]string{},
		loadErr:   map[string]error{},
		overrides: map[string]map[string]workspaces.ConnectorOverride{},
		flows:     map[string]*Flow{},
		updates:   map[string]string{},
		inspected: map[string]inspected{},
		runCtx:    context.Background(),
		now:       time.Now,
		sleep:     sleepCtx,
		pingEvery: 30 * time.Second,
		authEvery: 5 * time.Minute,

		webhookWake:  make(chan struct{}, 1),
		webhookEvery: webhookTick,
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

// WithConversations sets the probe that finds open job conversations, so a
// reply continues its run (replies: continue).
func (s *Service) WithConversations(p conversationProbe) *Service {
	s.convs = p
	return s
}

// OnStatus sets the hook for adapter state changes.
func (s *Service) OnStatus(h StatusHook) *Service {
	s.onStatus = h
	return s
}

// OnFlood sets the hook for dropped events.
func (s *Service) OnFlood(h FloodHook) *Service {
	s.onFlood = h
	return s
}

// Load scans the connection folders and the workspace layers into memory. A
// folder that does not load gets a load error; the last good copy of that
// connection stays, and writes to it fail until it loads again. The other
// connections are not affected.
func (s *Service) Load() error {
	conns, errs := readConnections()
	overrides := s.loadOverrides(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range errs {
		if old, ok := s.conns[id]; ok {
			conns[id] = old
		}
	}
	slugs := map[string]string{}
	for _, id := range slices.Sorted(maps.Keys(conns)) {
		c := conns[id]
		if other, dup := slugs[c.Slug]; dup {
			errs[id] = fmt.Errorf("slug %q is also used by %s", c.Slug, other)
			delete(conns, id)
			continue
		}
		slugs[c.Slug] = id
	}
	s.conns, s.slugs, s.loadErr, s.overrides = conns, slugs, errs, overrides
	var all []error
	for _, id := range slices.Sorted(maps.Keys(errs)) {
		all = append(all, fmt.Errorf("%s: %w", id, errs[id]))
	}
	return errors.Join(all...)
}

// LoadErr joins the load errors of every folder, or returns nil.
func (s *Service) LoadErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []error
	for _, id := range slices.Sorted(maps.Keys(s.loadErr)) {
		all = append(all, fmt.Errorf("%s: %w", id, s.loadErr[id]))
	}
	return errors.Join(all...)
}

// Lookup returns the connection with the slug or the id ref.
func (s *Service) Lookup(ref string) (Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.conns[ref]; ok {
		return c.clone(), nil
	}
	if id, ok := s.slugs[ref]; ok {
		return s.conns[id].clone(), nil
	}
	if err, ok := s.loadErr[ref]; ok {
		return Connection{}, fmt.Errorf("%w: %s: %v", ErrLoadFailed, ref, err)
	}
	return Connection{}, fmt.Errorf("connector %q not found: %w", ref, ErrNotFound)
}

// SlugOf returns the current slug of a connection id, or "" when the id is
// not a connection. A native id is its own slug.
func (s *Service) SlugOf(id string) string {
	if manifest.Reserved(id) {
		return id
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[id].Slug
}

// IDOf returns the id of a slug or id ref, or "" when no connection has it.
// A native name is its own id.
func (s *Service) IDOf(ref string) string {
	if manifest.Reserved(ref) {
		return ref
	}
	c, err := s.Lookup(ref)
	if err != nil {
		return ""
	}
	return c.ID
}

// ConnectionFor returns the id of a slug or id ref, or "" when no
// connection has it or the workspace turned it off.
func (s *Service) ConnectionFor(ref, workspaceID string) string {
	id := s.IDOf(ref)
	if id == "" || s.disabledIn(workspaceID, s.SlugOf(id)) {
		return ""
	}
	return id
}

// Create stores a new connection. It fills the id and, when empty, the
// default slug, and returns the stored value. A used slug is ErrExists.
func (s *Service) Create(ctx context.Context, c Connection) (Connection, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	c = c.clone()
	c.ID = NewConnectionID()
	c.Title = strings.TrimSpace(c.Title)
	if c.Slug == "" {
		c.Slug = s.nextSlug(c.Type)
	}
	if err := s.GuardReserved(c.Slug); err != nil {
		return Connection{}, err
	}
	if !s.slugFree(c.Slug, c.ID) {
		return Connection{}, fmt.Errorf("%w: slug %q", ErrExists, c.Slug)
	}
	if err := s.commit(ctx, c, nil, nil); err != nil {
		_ = NewFolder(c.ID).Remove() // best effort; the create already failed
		return Connection{}, err
	}
	return s.Lookup(c.ID)
}

// Patch applies a field update to a connection. It returns the config files
// that a slug change rewrote.
func (s *Service) Patch(ctx context.Context, ref string, p ConnectionPatch) ([]string, error) {
	if err := s.GuardReserved(ref); err != nil {
		return nil, err
	}
	if err := s.GuardMutate(ref); err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	old, err := s.Lookup(ref)
	if err != nil {
		return nil, err
	}
	if err := s.checkLoaded(old.ID); err != nil {
		return nil, err
	}
	c := old.clone()
	if err := p.apply(&c); err != nil {
		return nil, err
	}
	var keep map[string]bool
	if p.Inputs != nil {
		if keep, err = c.keepSecrets(old); err != nil {
			return nil, err
		}
	}
	rw, err := s.rewriteSlug(ctx, old, c)
	if err != nil {
		return nil, err
	}
	if err := s.commit(ctx, c, &old, keep); err != nil {
		if rw != nil {
			rw.rollback()
		}
		return nil, err
	}
	if rw == nil {
		return nil, nil
	}
	overrides := s.loadOverrides(ctx)
	s.mu.Lock()
	s.overrides = overrides
	s.mu.Unlock()
	return rw.changed(), nil
}

// Delete stops a connection and removes its folder. The skills of its type
// go when no other connection has the type. GC then removes each package
// version that no connection uses.
func (s *Service) Delete(ctx context.Context, ref string) error {
	if err := s.GuardReserved(ref); err != nil {
		return err
	}
	if err := s.GuardMutate(ref); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	c, err := s.Lookup(ref)
	if err != nil {
		if IsConnectionID(ref) && errors.Is(err, ErrLoadFailed) {
			return s.removeBroken(ctx, ref)
		}
		return err
	}
	s.unsyncBeforeDelete(ctx, c.ID)
	s.mu.Lock()
	delete(s.conns, c.ID)
	delete(s.slugs, c.Slug)
	delete(s.loadErr, c.ID)
	delete(s.updates, c.ID)
	s.mu.Unlock()
	s.reconcile(ctx)
	s.dropWebhooks(ctx, c.ID)
	s.dropStats(ctx, c.ID)
	if err := NewFolder(c.ID).Remove(); err != nil {
		return fmt.Errorf("remove connection folder: %w", err)
	}
	if !s.ofType(c.Type, c.ID) {
		s.deleteContributedSkills(ctx, c.Type)
	}
	if _, err := s.GC(ctx, false); err != nil {
		s.log.Warn("connector gc", "error", err)
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "connector.deleted", Entity: "connector", EntityID: c.ID, Action: "deleted",
		Attrs: map[string]string{"slug": c.Slug, "type": c.Type},
	}); err != nil {
		s.log.Warn("bus append", "topic", "connector.deleted", "error", err)
	}
	return nil
}

// Override returns the workspace layer of one connection.
func (s *Service) Override(ctx context.Context, workspaceID, ref string) (workspaces.ConnectorOverride, bool, error) {
	if s.ws == nil {
		return workspaces.ConnectorOverride{}, false, nil
	}
	c, err := s.Lookup(ref)
	if err != nil {
		return workspaces.ConnectorOverride{}, false, err
	}
	all, err := s.ws.ConnectorOverrides(ctx, workspaceID)
	if err != nil {
		return workspaces.ConnectorOverride{}, false, err
	}
	ov, ok := all[c.Slug]
	return ov, ok, nil
}

// SetOverride writes, or with nil removes, the workspace layer of a
// connection. The layer can only turn the connection off or patch its
// actions and skill.
func (s *Service) SetOverride(ctx context.Context, workspaceID, ref string, ov *workspaces.ConnectorOverride) error {
	if s.ws == nil {
		return errors.New("workspaces are not available")
	}
	if err := s.GuardReserved(ref); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	c, err := s.Lookup(ref)
	if err != nil {
		return err
	}
	if ov != nil {
		if err := ov.Validate(); err != nil {
			return &ValidationError{Name: c.Slug, Layer: LayerWorkspace, Fields: []FieldError{{Field: "patch", Message: err.Error()}}}
		}
		if !ov.Disabled {
			if _, err := resolveConnection(c, workspaceID, ov); err != nil {
				return err
			}
		}
	}
	if err := s.ws.SetConnectorOverride(ctx, workspaceID, c.Slug, ov); err != nil {
		return err
	}
	overrides := s.loadOverrides(ctx)
	s.mu.Lock()
	s.overrides = overrides
	s.mu.Unlock()
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "connector.updated", Entity: "connector", EntityID: c.ID, Action: "updated",
		Attrs: map[string]string{"workspace": workspaceID, "slug": c.Slug, "type": c.Type},
	}); err != nil {
		s.log.Warn("bus append", "topic", "connector.updated", "error", err)
	}
	return nil
}

// List returns the connection rows, with live status, unioned with inherited
// MCP servers. With a workspace, each row applies the workspace layer.
func (s *Service) List(workspaceID string) []map[string]any {
	stats := s.eventStats()
	var out []map[string]any
	fpIndex := map[string]int{}
	for _, c := range s.snapshot() {
		row := s.connectionRow(c, workspaceID, stats)
		if fp, ok := row["mcp_fingerprint"].(string); ok && fp != "" {
			fpIndex[fp] = len(out)
		}
		delete(row, "mcp_fingerprint")
		out = append(out, row)
	}
	s.mu.Lock()
	broken := map[string]error{}
	for id, err := range s.loadErr {
		if _, ok := s.conns[id]; !ok {
			broken[id] = err
		}
	}
	s.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(broken)) {
		out = append(out, map[string]any{
			"id": id, "type": "", "slug": "", "title": "", "provenance": "managed", "managed": true,
			"capabilities": []string{}, "load_error": broken[id].Error(),
		})
	}
	return s.unionInherited(out, fpIndex)
}

// Get returns one connection row by slug or id.
func (s *Service) Get(ref string) (map[string]any, error) {
	return s.GetFor(ref, "")
}

// GetFor returns one row by slug or id with the workspace layer applied. An
// inherited MCP server matches by its id, or by a name no other row has.
func (s *Service) GetFor(ref, workspaceID string) (map[string]any, error) {
	if c, err := s.Lookup(ref); err == nil {
		ref = c.ID
	}
	var byName []map[string]any
	for _, row := range s.List(workspaceID) {
		if fmt.Sprint(row["id"]) == ref {
			return row, nil
		}
		if fmt.Sprint(row["slug"]) == ref {
			byName = append(byName, row)
		}
	}
	switch len(byName) {
	case 0:
		return nil, fmt.Errorf("connector %q not found: %w", ref, ErrNotFound)
	case 1:
		return byName[0], nil
	}
	ids := make([]string, len(byName))
	for i, r := range byName {
		ids[i] = fmt.Sprint(r["id"])
	}
	return nil, &AmbiguousError{Name: ref, IDs: ids}
}

// HasEvents reports whether a connection has an events block. A missing ref
// is ErrNotFound.
func (s *Service) HasEvents(ref string) error {
	if _, err := s.Lookup(ref); err != nil {
		if _, gerr := s.Get(ref); gerr != nil {
			return err
		}
		return fmt.Errorf("%w: connector %q is not managed", ErrInherited, ref)
	}
	r, err := s.Resolve(ref, "")
	if err != nil {
		return err
	}
	if r.Manifest.Events == nil {
		return fmt.Errorf("%w: %q", ErrNoEvents, ref)
	}
	return nil
}

// IsChannel reports whether a connection is a channel that can hold a
// conversation.
func (s *Service) IsChannel(ref string) bool {
	r, err := s.Resolve(ref, "")
	return err == nil && slices.Contains(r.Manifest.Modes(), manifest.ModeReply)
}

// GuardReserved refuses record and lifecycle writes against a native
// connector name. The native service still owns its event and cursor rows,
// so InjectEvent and PutCursor do not call it.
func (s *Service) GuardReserved(ref string) error {
	if manifest.Reserved(ref) {
		return fmt.Errorf("%w: %q is a native connector", ErrReserved, ref)
	}
	return nil
}

// GuardMutate refuses writes against inherited MCP rows. A missing ref
// passes, so the caller reports its own not found.
func (s *Service) GuardMutate(ref string) error {
	if _, err := s.Lookup(ref); err == nil || errors.Is(err, ErrLoadFailed) {
		return nil
	}
	row, err := s.Get(ref)
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

// RewritePathPrefix updates the file sources, inline commands, and input
// values that point at old. dryRun reports the slugs without writing. Slugs
// are sorted.
func (s *Service) RewritePathPrefix(old, new string, dryRun bool) ([]string, error) {
	if s == nil || strings.TrimSpace(old) == "" || old == new {
		return nil, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.Load(); err != nil {
		s.log.Warn("connections", "error", err)
	}
	var changed []Connection
	for _, c := range s.snapshot() {
		if c.rewritePaths(old, new) {
			changed = append(changed, c)
		}
	}
	slugs := make([]string, 0, len(changed))
	for _, c := range changed {
		slugs = append(slugs, c.Slug)
	}
	if dryRun {
		return slugs, nil
	}
	for _, c := range changed {
		if err := NewFolder(c.ID).Save(c); err != nil {
			return slugs, err
		}
		s.mu.Lock()
		s.conns[c.ID] = c
		s.mu.Unlock()
	}
	return slugs, nil
}

// GetCursor returns the stored connector cursor of a connection. A missing
// cursor has an empty LastEventID.
func (s *Service) GetCursor(_ context.Context, ref string) (Cursor, error) {
	f, err := s.cursorFolder(ref)
	if err != nil {
		return Cursor{}, err
	}
	return f.Cursor()
}

// PutCursor writes the connector cursor of a connection. Not a bus entity.
func (s *Service) PutCursor(_ context.Context, ref, lastEventID string) error {
	if err := s.GuardMutate(ref); err != nil {
		return err
	}
	f, err := s.cursorFolder(ref)
	if err != nil {
		return err
	}
	return f.SetCursor(Cursor{LastEventID: lastEventID, UpdatedAt: s.now().UTC().Format(time.RFC3339)})
}

// LogPath is the stream log of a connection.
func (s *Service) LogPath(ref string) (string, error) {
	c, err := s.Lookup(ref)
	if err != nil {
		return "", err
	}
	return NewFolder(c.ID).LogPath(), nil
}

// LoadManifests resolves every connection without a Service, keyed by slug.
// The doctor uses it. A connection that does not resolve is left out; each
// folder that does not load is in the error map.
func LoadManifests() (map[string]manifest.Manifest, map[string]error) {
	conns, errs := readConnections()
	out := map[string]manifest.Manifest{}
	for _, c := range conns {
		if r, err := resolveConnection(c, "", nil); err == nil {
			out[c.Slug] = r.Manifest
		}
	}
	return out, errs
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

// readConnections loads every folder under the connections folder whose name
// is a connection id. A native cursor folder is not a connection.
func readConnections() (map[string]Connection, map[string]error) {
	conns := map[string]Connection{}
	errs := map[string]error{}
	ents, err := os.ReadDir(home.ConnectionsDir())
	if err != nil {
		return conns, errs
	}
	for _, e := range ents {
		if !e.IsDir() || !IsConnectionID(e.Name()) {
			continue
		}
		c, err := NewFolder(e.Name()).Load()
		if err != nil {
			errs[e.Name()] = err
			continue
		}
		conns[c.ID] = c
	}
	return conns, errs
}

// checkLoaded refuses a write to a connection whose folder does not load, so
// a write cannot replace a file that the user is still editing.
func (s *Service) checkLoaded(id string) error {
	s.mu.Lock()
	err := s.loadErr[id]
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrLoadFailed, id, err)
	}
	return nil
}

// snapshot returns a copy of every connection, sorted by slug.
func (s *Service) snapshot() []Connection {
	s.mu.Lock()
	out := make([]Connection, 0, len(s.conns))
	for _, c := range s.conns {
		out = append(out, c.clone())
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// ofType reports whether another connection than id has the type typ.
func (s *Service) ofType(typ, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		if c.Type == typ && c.ID != id {
			return true
		}
	}
	return false
}

// emitWrite appends a connector.created/updated bus event with the
// capabilities of the resolved manifest.
func (s *Service) emitWrite(ctx context.Context, topic string, r Resolved) {
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: topic, Entity: "connector", EntityID: r.ID, Action: strings.TrimPrefix(topic, "connector."),
		Attrs: map[string]string{"capabilities": strings.Join(r.Manifest.Capabilities(), ","), "slug": r.Slug, "type": r.Type},
	}); err != nil {
		s.log.Warn("bus append", "topic", topic, "error", err)
	}
}

// nextSlug returns the default slug of a new connection of a type: the type,
// then type-2, type-3, and so on. The caller holds writeMu.
func (s *Service) nextSlug(typ string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n := 1; ; n++ {
		slug := typ
		if n > 1 {
			slug = typ + "-" + strconv.Itoa(n)
		}
		if _, used := s.slugs[slug]; !used && CheckSlug(slug) == nil {
			return slug
		}
	}
}

// slugFree reports whether no connection other than id has slug.
func (s *Service) slugFree(slug, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	other, used := s.slugs[slug]
	return !used || other == id
}

// apply writes the present fields of p into c.
func (p ConnectionPatch) apply(c *Connection) error {
	bad := func(field string, err error) error {
		return &ValidationError{Name: c.Slug, Fields: []FieldError{{Field: field, Message: err.Error()}}}
	}
	if p.Slug != nil {
		c.Slug = strings.TrimSpace(*p.Slug)
	}
	if p.Title != nil {
		c.Title = strings.TrimSpace(*p.Title)
	}
	if p.Version != nil {
		c.Version = *p.Version
	}
	if p.Track != nil {
		c.Track = manifest.Track(*p.Track)
	}
	if p.Inputs != nil {
		c.Inputs = maps.Clone(*p.Inputs)
	}
	if len(p.Patch) > 0 {
		c.Patch = nil
		if !isNull(p.Patch) {
			if err := json.Unmarshal(p.Patch, &c.Patch); err != nil {
				return bad("patch", err)
			}
		}
	}
	if len(p.Manifest) > 0 {
		c.Manifest = nil
		if !isNull(p.Manifest) {
			var m manifest.Manifest
			if err := decodeStrict(p.Manifest, &m); err != nil {
				return bad("manifest", err)
			}
			c.Manifest = &m
		}
	}
	if len(p.Subscriptions) > 0 {
		c.Subscriptions = nil
		if !isNull(p.Subscriptions) {
			if err := json.Unmarshal(p.Subscriptions, &c.Subscriptions); err != nil {
				return bad("subscriptions", err)
			}
		}
		for i := range c.Subscriptions {
			if c.Subscriptions[i].ID == "" {
				c.Subscriptions[i].ID = NewSubscriptionID()
			}
		}
	}
	return nil
}

func isNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// commit validates c, moves its literal secret inputs to the folder, saves
// the record, emits, and reconciles. With keep set, the inputs were
// replaced: an owned secret that is not in keep and not written again is
// deleted. The caller holds writeMu.
func (s *Service) commit(ctx context.Context, c Connection, old *Connection, keep map[string]bool) error {
	if err := c.Validate(); err != nil {
		return err
	}
	r, err := resolveConnection(c, "", nil)
	if err != nil {
		return err
	}
	if err := c.checkSubscriptions(r.Manifest); err != nil {
		return err
	}
	f := NewFolder(c.ID)
	owned := c.takeSecrets(r.Manifest)
	if err := f.Create(); err != nil {
		return err
	}
	if err := f.syncSecrets(owned, c.Inputs, keep); err != nil {
		return err
	}
	if err := f.Save(c); err != nil {
		return err
	}
	s.mu.Lock()
	if old != nil {
		delete(s.slugs, old.Slug)
	}
	s.conns[c.ID] = c
	s.slugs[c.Slug] = c.ID
	delete(s.loadErr, c.ID)
	s.mu.Unlock()
	topic := "connector.updated"
	if old == nil {
		topic = "connector.created"
	}
	if r, err = resolveConnection(c, "", nil); err == nil {
		s.emitWrite(ctx, topic, r)
		s.syncSkill(ctx, r)
	}
	s.reconcile(ctx)
	if old != nil && !slices.EqualFunc(old.Subscriptions, c.Subscriptions, func(a, b Subscription) bool {
		return a.ID == b.ID && a.key() == b.key()
	}) {
		s.syncSubscriptionsAsync(c.ID)
	}
	return nil
}

// syncSkill copies the skill folder of the type into the managed skills, so
// the harness homes project it. Skills are per type.
func (s *Service) syncSkill(ctx context.Context, r Resolved) {
	dir := r.SkillDir()
	if s.sk == nil || dir == "" {
		return
	}
	if _, err := s.sk.SyncConnectorSkill(ctx, r.Type, dir, r.Type); err != nil {
		s.log.Warn("connector skill", "connector", r.Slug, "error", err)
	}
}

// removeBroken deletes the folder of a connection that does not load. The
// caller holds writeMu.
func (s *Service) removeBroken(ctx context.Context, id string) error {
	if err := NewFolder(id).Remove(); err != nil {
		return fmt.Errorf("remove connection folder: %w", err)
	}
	s.mu.Lock()
	delete(s.loadErr, id)
	s.mu.Unlock()
	s.dropStats(ctx, id)
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "connector.deleted", Entity: "connector", EntityID: id, Action: "deleted",
	}); err != nil {
		s.log.Warn("bus append", "topic", "connector.deleted", "error", err)
	}
	return nil
}

// deleteContributedSkills removes the skills of a type. A failure is logged
// and does not undo the delete.
func (s *Service) deleteContributedSkills(ctx context.Context, typ string) {
	if s.sk == nil {
		return
	}
	removed, err := s.sk.RemoveConnectorSkills(ctx, typ)
	if err != nil {
		s.log.Warn("delete contributed skills", "connector", typ, "error", err)
		return
	}
	if len(removed) > 0 {
		s.log.Info("connector skills removed", "connector", typ, "skills", strings.Join(removed, ","))
	}
}

// eventStats is the lifetime event count of one connection.
type eventStats struct {
	total       int64
	first, last string
}

// eventStats reads the lifetime stats of each connection in one query.
func (s *Service) eventStats() map[string]eventStats {
	out := map[string]eventStats{}
	if s.st == nil {
		return out
	}
	rows, err := s.st.DB().Query(`SELECT instance, events_total, COALESCE(first_event_at,''), COALESCE(last_event_at,'') FROM connection_stats`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var st eventStats
		if rows.Scan(&id, &st.total, &st.first, &st.last) == nil {
			out[id] = st
		}
	}
	return out
}

// dropStats removes the lifetime stats of a deleted connection.
func (s *Service) dropStats(ctx context.Context, id string) {
	if s.st == nil {
		return
	}
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM connection_stats WHERE instance=?`, id); err != nil {
		s.log.Warn("drop connection stats", "connector", id, "error", err)
	}
}

// connectionRow builds the wire row of one connection. The live fields of
// its process are at the top of the row.
func (s *Service) connectionRow(c Connection, workspaceID string, stats map[string]eventStats) map[string]any {
	inputs := MaskInputs(c.Inputs)
	for _, name := range NewFolder(c.ID).Secrets() {
		if _, ok := inputs[name]; !ok {
			inputs[name] = maskValue
		}
	}
	row := map[string]any{
		"id":           c.ID,
		"type":         c.Type,
		"slug":         c.Slug,
		"title":        c.Title,
		"provenance":   "managed",
		"managed":      true,
		"from":         c.From,
		"track":        string(c.track()),
		"inputs":       inputs,
		"capabilities": []string{},
		"update":       nil,
		"folder":       NewFolder(c.ID).Dir(),
	}
	row["subscriptions"] = []Subscription{}
	if len(c.Subscriptions) > 0 {
		row["subscriptions"] = c.Subscriptions
	}
	if c.Version != "" {
		row["version"] = c.Version
	}
	if c.Digest != "" {
		row["digest"] = c.Digest
	}
	if err := s.loadErrOf(c.ID); err != nil {
		row["load_error"] = err.Error()
	}
	if v := s.availableUpdate(c.ID); v != "" {
		row["update"] = map[string]any{"version": v}
	}
	ws := workspaceID
	if s.disabledIn(workspaceID, c.Slug) {
		row["disabled"] = true
		ws = ""
	}
	if workspaceID != "" {
		row["workspace"] = workspaceID
	}
	r, err := s.Resolve(c.ID, ws)
	if err != nil {
		row["resolve_error"] = err.Error()
		return row
	}
	if _, missing := r.Env(); len(missing) > 0 {
		row["resolve_error"] = "missing secrets: " + strings.Join(missing, ", ")
	}
	m := r.Manifest
	row["title"] = r.Title
	row["description"] = m.Description
	row["icon"] = m.Icon
	row["version"] = r.Version
	row["track"] = string(r.Track)
	row["publisher"] = m.Publisher
	row["warning"] = m.Warning
	row["manifest"] = m
	row["capabilities"] = m.Capabilities()
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
	s.fillLive(row, c.ID, m, stats[c.ID])
	return row
}

// loadErrOf returns the load error of one folder, or nil.
func (s *Service) loadErrOf(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr[id]
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
			"skill":    r.Type,
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
// connection that already declares the same server.
func (s *Service) unionInherited(out []map[string]any, fpIndex map[string]int) []map[string]any {
	discovered := harness.DiscoverMCP(s.userHome, nil)
	usedNames := map[string]int{}
	for _, row := range out {
		usedNames[fmt.Sprint(row["slug"])]++
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
			"id": id, "type": "", "slug": srv.Name, "title": srv.Name, "provenance": "inherited", "managed": false,
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

// rewritePaths rewrites every path field of c that starts with old.
func (c *Connection) rewritePaths(old, new string) bool {
	changed := false
	rewrite := func(p *string) {
		if next, ok := workspaces.RewritePathPrefix(*p, old, new); ok {
			*p = next
			changed = true
		}
	}
	if path := c.FilePath(); path != "" {
		if next, ok := workspaces.RewritePathPrefix(path, old, new); ok {
			c.From = fromFile + next
			changed = true
		}
	}
	for k, v := range c.Inputs {
		rewrite(&v)
		c.Inputs[k] = v
	}
	if m := c.Manifest; m != nil {
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

// cursorFolder returns the folder that holds the cursor of ref. A native
// connector keeps its cursor in a folder named after it.
func (s *Service) cursorFolder(ref string) (Folder, error) {
	if manifest.Reserved(ref) {
		return Folder{dir: home.ConnectionDir(ref)}, nil
	}
	c, err := s.Lookup(ref)
	if err != nil {
		return Folder{}, err
	}
	return NewFolder(c.ID), nil
}

// appendConnectorLog appends one line to the stream log of a connection.
func appendConnectorLog(id, stream, line string) {
	logPath := NewFolder(id).LogPath()
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755) // the open below reports the failure
	logf, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(logf, `{"t":%q,"stream":%q,"line":%q}`+"\n", //nolint:errcheck // a lost log line is not an error
		time.Now().UTC().Format(time.RFC3339Nano), stream, line)
	_ = logf.Close()
}

// decodeStrict decodes JSON and rejects unknown fields.
func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// orEmptySlice returns a copy of in, or an empty slice for nil, so JSON
// shows [] and not null.
func orEmptySlice[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return slices.Clone(in)
}

// keepSecrets drops each input that comes back as the mask and has an owned
// secret, and returns those names. It puts back the stored value of each
// other masked input.
func (c *Connection) keepSecrets(old Connection) (map[string]bool, error) {
	keep := map[string]bool{}
	owned := NewFolder(c.ID).Secrets()
	for k, v := range c.Inputs {
		if v == maskValue && slices.Contains(owned, k) {
			delete(c.Inputs, k)
			keep[k] = true
		}
	}
	return keep, keepMasked(c.Slug, c.Inputs, old.Inputs)
}

// rewriteSlug rewrites the config files when the slug changes. It returns
// nil when the slug stays.
func (s *Service) rewriteSlug(ctx context.Context, old, c Connection) (*slugRewrite, error) {
	if c.Slug == old.Slug {
		return nil, nil
	}
	if err := CheckSlug(c.Slug); err != nil {
		return nil, &ValidationError{Name: old.Slug, Fields: []FieldError{{Field: "slug", Message: err.Error()}}}
	}
	if !s.slugFree(c.Slug, c.ID) {
		return nil, fmt.Errorf("%w: slug %q", ErrExists, c.Slug)
	}
	rw := &slugRewrite{old: old.Slug, new: c.Slug}
	if err := rw.apply(s.slugFiles(ctx)); err != nil {
		rw.rollback()
		return nil, err
	}
	return rw, nil
}

// takeSecrets moves each literal value of a secret input out of the inputs
// and returns them.
func (c *Connection) takeSecrets(m manifest.Manifest) map[string]string {
	owned := map[string]string{}
	if m.Setup == nil {
		return owned
	}
	for _, in := range m.Setup.Inputs {
		v, ok := c.Inputs[in.Name]
		if !in.Secret || !ok || v == "" || strings.HasPrefix(v, "secret:") {
			continue
		}
		owned[in.Name] = v
		delete(c.Inputs, in.Name)
	}
	return owned
}

// fillLive adds the events, channel, and auth fields of a connection row:
// the live session when the process runs, else the manifest.
func (s *Service) fillLive(row map[string]any, id string, m manifest.Manifest, stats eventStats) {
	p := s.procByName(id)
	if m.Events != nil {
		row["events"] = eventsRow(*m.Events)
		pos, _ := NewFolder(id).Cursor()
		row["status"] = statusStopped
		row["restarts"] = 0
		row["last_err"] = ""
		row["last_event_id"] = pos.LastEventID
		row["last_event_at"] = stats.last
		row["first_event_at"] = stats.first
		row["counters"] = map[string]any{"events": int64(0), "duplicates": int64(0), "parse_errors": int64(0)}
		if p != nil {
			p.snapshot().fill(row)
		}
		// events counts since process start; events_total survives restarts.
		row["counters"].(map[string]any)["events_total"] = stats.total
	}
	if m.Channel != nil {
		row["channel"] = map[string]any{
			"features": orEmptySlice(m.Channel.Features),
			"limits":   m.Channel.Limits,
			"modes":    m.Modes(),
		}
	}
	if p != nil {
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
}
