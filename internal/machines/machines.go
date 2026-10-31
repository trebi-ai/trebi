// Package machines owns the machine roster. Each machine runs one Trebi
// daemon, and a workspace is on one machine. This daemon keeps one self row;
// the cloud roster fills the other rows.
package machines

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/store"
)

// SelfName is the name of the self row when the daemon has no cloud link.
const SelfName = "This machine"

// SelfRef is the id alias of the self row in /v1/machines/{id}.
const SelfRef = "self"

// ErrNotFound is a machine id with no row.
var ErrNotFound = errors.New("machine not found")

// errStale is a self row that another writer changed first.
var errStale = errors.New("machine row changed")

// Machine is the wire shape of one machine.
type Machine struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	OS       string  `json:"os"`
	Arch     string  `json:"arch"`
	DeviceID *string `json:"device_id"`
	IsSelf   bool    `json:"is_self"`
	// LastSeen is the time of the last sign of life. The self row is live
	// while it answers, so it is the read time.
	LastSeen   *string `json:"last_seen"`
	VisorBuild string  `json:"visor_build"`
	Revision   int64   `json:"revision"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
	// Capabilities is set on a single read of the self row only.
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	// Link is the peer link to the machine; nil on the self row.
	Link *Link `json:"link"`
}

// authStates reads the stored harness logins.
type authStates interface {
	States(ctx context.Context) (map[string]harness.State, error)
}

// connections reads the connections and resolves one for a workspace.
type connections interface {
	List(workspaceID string) []map[string]any
	Resolve(ref, workspaceID string) (connectors.Resolved, error)
}

// cloudLink gives the device of the cloud link; "" when not linked.
type cloudLink interface {
	DeviceID() string
	DeviceName() string
}

// Service reads and writes the machines table and builds the capabilities
// and the readiness of the self machine.
type Service struct {
	st      *store.Store
	auth    authStates
	loc     harness.Locator
	conns   connections
	link    cloudLink
	cloud   rosterCloud
	wsList  workspaceLister
	links   *Links
	version string
	log     *slog.Logger

	mu        sync.Mutex
	published string // device id and JSON of the last sent workspace index
}

// New builds the service. A nil source reports nothing for its part.
func New(st *store.Store, auth authStates, loc harness.Locator, conns connections, link cloudLink) *Service {
	return &Service{st: st, auth: auth, loc: loc, conns: conns, link: link, log: slog.Default().With("source", "machines")}
}

// WithVersion sets the Trebi version that the cloud summary carries.
func (s *Service) WithVersion(v string) *Service {
	s.version = v
	return s
}

// WithLinks sets the peer links that List and Get show.
func (s *Service) WithLinks(l *Links) *Service {
	s.links = l
	return s
}

const machineCols = `id, name, os, arch, device_id, is_self, last_seen, visor_build, revision, created_at, updated_at`

func scanMachine(sc interface{ Scan(...any) error }) (Machine, error) {
	var m Machine
	var device, seen sql.NullString
	var self int
	if err := sc.Scan(&m.ID, &m.Name, &m.OS, &m.Arch, &device, &self, &seen, &m.VisorBuild, &m.Revision, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return Machine{}, err
	}
	m.IsSelf = self == 1
	if device.Valid && device.String != "" {
		m.DeviceID = &device.String
	}
	if seen.Valid && seen.String != "" {
		m.LastSeen = &seen.String
	}
	if m.IsSelf {
		now := time.Now().UTC().Format(time.RFC3339)
		m.LastSeen = &now
	}
	return m, nil
}

// List returns every machine, the self row first. It makes the self row
// when it is missing.
func (s *Service) List(ctx context.Context) ([]Machine, error) {
	if _, err := s.Self(ctx); err != nil {
		return nil, err
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+machineCols+` FROM machines ORDER BY is_self DESC, name, id`)
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	defer rows.Close()
	out := []Machine{}
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if s.links != nil {
		if err := s.links.fill(ctx, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Get returns one machine by id, or the self row for SelfRef.
func (s *Service) Get(ctx context.Context, id string) (Machine, error) {
	self, err := s.Self(ctx)
	if err != nil {
		return Machine{}, err
	}
	if id == SelfRef || id == self.ID {
		return self, nil
	}
	m, err := scanMachine(s.st.DB().QueryRowContext(ctx, `SELECT `+machineCols+` FROM machines WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Machine{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Machine{}, err
	}
	if s.links != nil {
		one := []Machine{m}
		if err := s.links.fill(ctx, one); err != nil {
			return Machine{}, err
		}
		m = one[0]
	}
	return m, nil
}

// selfFields are the values that the self row must hold.
type selfFields struct {
	name, os, arch, deviceID string
}

func (f selfFields) matches(m Machine) bool {
	device := ""
	if m.DeviceID != nil {
		device = *m.DeviceID
	}
	return m.Name == f.name && m.OS == f.os && m.Arch == f.arch && device == f.deviceID
}

// Self returns the self row. It makes the row when it is missing, and it
// copies the device id and the device name of the cloud link into it.
func (s *Service) Self(ctx context.Context) (Machine, error) {
	want := s.wantSelf()
	var last error
	for range 3 {
		m, err := scanMachine(s.st.DB().QueryRowContext(ctx, `SELECT `+machineCols+` FROM machines WHERE is_self=1`))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			err = s.insertSelf(ctx, want)
		case err != nil:
			return Machine{}, fmt.Errorf("read self machine: %w", err)
		case want.matches(m):
			return m, nil
		default:
			err = s.updateSelf(ctx, m, want)
		}
		// On an error, a second writer can have made or changed the row; read again.
		if err != nil {
			last = err
		}
	}
	if last == nil {
		last = errStale
	}
	return Machine{}, fmt.Errorf("write self machine: %w", last)
}

func mintID() string {
	var b [6]byte
	_, _ = rand.Read(b[:]) // crypto/rand never fails on supported platforms
	return "mch_" + hex.EncodeToString(b[:])
}

// The states of a harness, a connector, or a secret.
const (
	StateReady        = "ready"
	StateMissing      = "missing"
	StateNotInstalled = "not_installed"
	StateOff          = "off"
	StateLoggedOut    = "logged_out"
	StateError        = "error"
	StateUnknown      = "unknown"
)

// Capabilities is what the self machine can run. It holds names and states
// only, never a secret.
type Capabilities struct {
	Harnesses  []HarnessCap   `json:"harnesses"`
	Connectors []ConnectorCap `json:"connectors"`
	Versions   Versions       `json:"versions"`
}

// HarnessCap is one harness program on the machine.
type HarnessCap struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Installed bool   `json:"installed"`
	Enabled   bool   `json:"enabled"`
	Version   string `json:"version"`
	State     string `json:"state"`
}

// ConnectorCap is one connection on the machine.
type ConnectorCap struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Title   string `json:"title"`
	Version string `json:"version"`
	State   string `json:"state"`
}

// Versions are the versions of the Trebi parts. An empty string is a part
// that the build does not have.
type Versions struct {
	Trebi   string `json:"trebi"`
	Edition string `json:"edition"`
	UI      string `json:"ui"`
}

// Capabilities builds the harnesses and the connections of the self
// machine. The caller fills Versions.
func (s *Service) Capabilities(ctx context.Context) (Capabilities, error) {
	out := Capabilities{Harnesses: []HarnessCap{}, Connectors: []ConnectorCap{}}
	infos, states, err := s.harnessInfo(ctx)
	if err != nil {
		return out, err
	}
	for _, info := range infos {
		if info.Hidden && !info.Installed {
			continue
		}
		st, ok := states[info.Name]
		v, _ := harness.ReadVersionState(ctx, s.st.DB(), info.Name)
		out.Harnesses = append(out.Harnesses, HarnessCap{
			Name: info.Name, Label: info.Label, Installed: info.Installed, Enabled: info.Enabled,
			Version: v.Version, State: harnessState(info, st, ok),
		})
	}
	if s.conns == nil {
		return out, nil
	}
	for _, row := range s.conns.List("") {
		id, _ := row["id"].(string)
		if prov, _ := row["provenance"].(string); prov != "managed" || id == "" {
			continue
		}
		c := ConnectorCap{ID: id, State: StateError}
		c.Name, _ = row["slug"].(string)
		c.Type, _ = row["type"].(string)
		c.Title, _ = row["title"].(string)
		c.Version, _ = row["version"].(string)
		if _, broken := row["load_error"]; !broken {
			_, rerr := s.conns.Resolve(id, "")
			c.State = connectorState(rerr, loginState(row))
		}
		out.Connectors = append(out.Connectors, c)
	}
	slices.SortFunc(out.Connectors, func(a, b ConnectorCap) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// harnessState is the state of one harness: installed, on, then logged in.
// A harness that was never checked is unknown.
func harnessState(info harness.Info, st harness.State, ok bool) string {
	switch {
	case !info.Installed:
		return StateNotInstalled
	case !info.Enabled:
		return StateOff
	case !ok || !st.AuthOK.Valid:
		return StateUnknown
	case st.AuthOK.Int64 == 0:
		return StateLoggedOut
	}
	return StateReady
}

// connectorState maps the resolve error and the login state of a
// connection to a state.
func connectorState(resolveErr error, login string) string {
	switch {
	case errors.Is(resolveErr, connectors.ErrNotFound):
		return StateMissing
	case errors.Is(resolveErr, connectors.ErrNotInstalled):
		return StateNotInstalled
	case errors.Is(resolveErr, connectors.ErrDisabled), errors.Is(resolveErr, connectors.ErrNotInScope):
		return StateOff
	case resolveErr != nil:
		return StateError
	}
	switch login {
	case "auth_required", "auth_pending":
		return StateLoggedOut
	case "error":
		return StateError
	}
	return StateReady
}

// loginState reads login.state of a connection row.
func loginState(row map[string]any) string {
	login, _ := row["login"].(map[string]any)
	st, _ := login["state"].(string)
	return st
}

// CloudCapabilities is the heartbeat summary of the self machine.
func (s *Service) CloudCapabilities(ctx context.Context) (cloudlink.Capabilities, error) {
	c, err := s.Capabilities(ctx)
	if err != nil {
		return cloudlink.Capabilities{}, err
	}
	out := cloudlink.Capabilities{
		OS: runtime.GOOS, Arch: runtime.GOARCH, Version: s.version,
		Harnesses: []cloudlink.CapabilityItem{}, Connectors: []cloudlink.CapabilityItem{},
	}
	for _, h := range c.Harnesses {
		out.Harnesses = append(out.Harnesses, cloudlink.CapabilityItem{Name: h.Name, State: h.State, Version: h.Version})
	}
	for _, k := range c.Connectors {
		out.Connectors = append(out.Connectors, cloudlink.CapabilityItem{Name: k.Name, Type: k.Type, State: k.State, Version: k.Version})
	}
	return out, nil
}

func (s *Service) wantSelf() selfFields {
	f := selfFields{name: SelfName, os: runtime.GOOS, arch: runtime.GOARCH}
	if s.link != nil {
		if id := s.link.DeviceID(); id != "" {
			f.deviceID = id
			if n := s.link.DeviceName(); n != "" {
				f.name = n
			}
		}
	}
	return f
}

func (s *Service) insertSelf(ctx context.Context, f selfFields) error {
	id := mintID()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO machines (id, name, os, arch, device_id, is_self, revision, created_at, updated_at)
			VALUES (?,?,?,?,?,1,1,?,?)`, id, f.name, f.os, f.arch, store.NullStr(f.deviceID), now, now)
		return err
	}, store.BusEvent{Topic: "machine.created", Entity: "machine", EntityID: id, Action: "created", Revision: 1})
	return err
}

func (s *Service) updateSelf(ctx context.Context, m Machine, f selfFields) error {
	now := time.Now().UTC().Format(time.RFC3339)
	rev := m.Revision + 1
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE machines SET name=?, os=?, arch=?, device_id=?, revision=?, updated_at=?
			WHERE id=? AND revision=?`, f.name, f.os, f.arch, store.NullStr(f.deviceID), rev, now, m.ID, m.Revision)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errStale
		}
		return nil
	}, store.BusEvent{Topic: "machine.updated", Entity: "machine", EntityID: m.ID, Action: "updated", Revision: rev})
	return err
}

func (s *Service) harnessInfo(ctx context.Context) ([]harness.Info, map[string]harness.State, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	states := map[string]harness.State{}
	if s.auth != nil {
		if states, err = s.auth.States(ctx); err != nil {
			return nil, nil, fmt.Errorf("read harness logins: %w", err)
		}
	}
	return harness.List(cfg, s.loc), states, nil
}
