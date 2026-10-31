package machines

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// indexIDRe and indexNameMax follow the cloud limits of PUT /v1/workspaces.
var indexIDRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

const indexNameMax = 120

// rosterCloud reads the account roster and takes the workspace index.
type rosterCloud interface {
	Roster(ctx context.Context) (cloudlink.Roster, error)
	PublishWorkspaces(ctx context.Context, items []cloudlink.WorkspaceIndexItem) error
}

// workspaceLister lists the workspaces of this daemon.
type workspaceLister interface {
	List(ctx context.Context) ([]workspaces.Workspace, error)
}

// WithCloud sets the roster source and the workspaces of the index.
func (s *Service) WithCloud(c rosterCloud, ws workspaceLister) *Service {
	s.cloud = c
	s.wsList = ws
	return s
}

// SyncRoster copies the account roster into the rows that are not self,
// keyed by device id. Without a cloud link only the self row stays. A
// failed read keeps the cached rows.
func (s *Service) SyncRoster(ctx context.Context) error {
	self, err := s.Self(ctx)
	if err != nil {
		return err
	}
	if self.DeviceID == nil || s.cloud == nil {
		return s.keepRemote(ctx, nil)
	}
	r, err := s.cloud.Roster(ctx)
	if err != nil {
		return fmt.Errorf("read roster: %w", err)
	}
	keep := map[string]bool{}
	for _, m := range r.Machines {
		if m.DeviceID == "" || m.DeviceID == *self.DeviceID {
			continue
		}
		keep[m.DeviceID] = true
		if err := s.upsertRemote(ctx, m); err != nil {
			return err
		}
	}
	return s.keepRemote(ctx, keep)
}

// remoteFields are the values that a roster row must hold.
type remoteFields struct {
	name, os, arch, lastSeen string
}

func remoteOf(m cloudlink.RosterMachine) remoteFields {
	f := remoteFields{name: m.Name}
	if f.name == "" {
		f.name = m.DeviceID
	}
	if m.Capabilities != nil {
		f.os, f.arch = m.Capabilities.OS, m.Capabilities.Arch
	}
	if m.LastSeen != nil {
		f.lastSeen = *m.LastSeen
	}
	return f
}

func (f remoteFields) matches(m Machine) bool {
	seen := ""
	if m.LastSeen != nil {
		seen = *m.LastSeen
	}
	return m.Name == f.name && m.OS == f.os && m.Arch == f.arch && seen == f.lastSeen
}

func (s *Service) upsertRemote(ctx context.Context, rm cloudlink.RosterMachine) error {
	f := remoteOf(rm)
	now := time.Now().UTC().Format(time.RFC3339)
	m, err := scanMachine(s.st.DB().QueryRowContext(ctx,
		`SELECT `+machineCols+` FROM machines WHERE is_self=0 AND device_id=?`, rm.DeviceID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id := mintID()
		_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO machines (id, name, os, arch, device_id, is_self, last_seen, revision, created_at, updated_at)
				VALUES (?,?,?,?,?,0,?,1,?,?)`, id, f.name, f.os, f.arch, rm.DeviceID, store.NullStr(f.lastSeen), now, now)
			return err
		}, store.BusEvent{Topic: "machine.created", Entity: "machine", EntityID: id, Action: "created", Revision: 1})
	case err != nil:
		return fmt.Errorf("read machine %s: %w", rm.DeviceID, err)
	case f.matches(m):
		return nil
	default:
		rev := m.Revision + 1
		_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE machines SET name=?, os=?, arch=?, last_seen=?, revision=?, updated_at=? WHERE id=?`,
				f.name, f.os, f.arch, store.NullStr(f.lastSeen), rev, now, m.ID)
			return err
		}, store.BusEvent{Topic: "machine.updated", Entity: "machine", EntityID: m.ID, Action: "updated", Revision: rev})
	}
	if err != nil {
		return fmt.Errorf("write machine %s: %w", rm.DeviceID, err)
	}
	return nil
}

// keepRemote deletes each row that is not self and whose device id is not
// in keep.
func (s *Service) keepRemote(ctx context.Context, keep map[string]bool) error {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, COALESCE(device_id, '') FROM machines WHERE is_self=0`)
	if err != nil {
		return fmt.Errorf("list remote machines: %w", err)
	}
	var drop []string
	for rows.Next() {
		var id, device string
		if err := rows.Scan(&id, &device); err != nil {
			_ = rows.Close() // the scan error wins
			return err
		}
		if !keep[device] {
			drop = append(drop, id)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range drop {
		if s.links != nil { // the machine left the account: its link goes too
			if err := s.links.Disconnect(ctx, Machine{ID: id}); err != nil {
				s.log.Warn("machine.link", "machine_id", id, "err", err)
			}
		}
		_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM machines WHERE id=? AND is_self=0`, id)
			return err
		}, store.BusEvent{Topic: "machine.deleted", Entity: "machine", EntityID: id, Action: "deleted"})
		if err != nil {
			return fmt.Errorf("delete machine %s: %w", id, err)
		}
	}
	return nil
}

// PublishWorkspaces sends the workspace index of this machine to the
// cloud. It sends nothing when the index did not change since the last
// send, or when the daemon has no cloud link.
func (s *Service) PublishWorkspaces(ctx context.Context) error {
	if s.cloud == nil || s.wsList == nil || s.link == nil {
		return nil
	}
	device := s.link.DeviceID()
	if device == "" {
		s.setPublished("")
		return nil
	}
	list, err := s.wsList.List(ctx)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	items := indexItems(list)
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	key := device + "\n" + string(raw)
	s.mu.Lock()
	same := s.published == key
	s.mu.Unlock()
	if same {
		return nil
	}
	if err := s.cloud.PublishWorkspaces(ctx, items); err != nil {
		return fmt.Errorf("publish workspace index: %w", err)
	}
	s.setPublished(key)
	return nil
}

func (s *Service) setPublished(key string) {
	s.mu.Lock()
	s.published = key
	s.mu.Unlock()
}

// indexItems keeps the workspaces of the self machine in the form that
// the cloud accepts. A workspace with an id that the cloud refuses stays
// out.
func indexItems(list []workspaces.Workspace) []cloudlink.WorkspaceIndexItem {
	out := []cloudlink.WorkspaceIndexItem{}
	for _, w := range list {
		if (w.Machine != nil && !w.Machine.IsSelf) || !indexIDRe.MatchString(w.ID) {
			continue
		}
		name := strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, w.Name))
		if r := []rune(name); len(r) > indexNameMax {
			name = strings.TrimSpace(string(r[:indexNameMax]))
		}
		if name == "" {
			name = w.ID
		}
		handle := w.Handle
		if workspaces.ValidateHandle(handle) != nil {
			handle = ""
		}
		out = append(out, cloudlink.WorkspaceIndexItem{ID: w.ID, Name: name, Handle: handle})
	}
	return out
}
