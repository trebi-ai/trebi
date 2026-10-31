package machines

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/mobile"
	"github.com/trebi-ai/trebi/internal/relay"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
)

// Link states.
const (
	LinkNone   = "none"
	LinkLinked = "linked"
	LinkError  = "error"
)

var (
	// ErrBadLink is a pairing link that this machine cannot use.
	ErrBadLink = errors.New("bad pairing link")
	// ErrLinkUsed is a pairing link that the cloud refused: used or expired.
	ErrLinkUsed = errors.New("pairing link already used")
	// ErrNoLink is a request for a machine that this machine is not connected to.
	ErrNoLink = errors.New("not connected to this machine")
)

// Link is the peer link from this machine to a remote machine.
type Link struct {
	State    string  `json:"state"`
	Error    string  `json:"error,omitempty"`
	LinkedAt *string `json:"linked_at"`
}

// linkCloud enrolls this machine as a client of another machine and opens
// its relay sockets. cloudlink.Service implements it.
type linkCloud interface {
	APIHost() string
	DeviceID() string
	DeviceName() string
	EnrollPeer(ctx context.Context, claim, name string) (cloudlink.Enrollment, error)
	LeaveClient(ctx context.Context, token string) error
	DialRelay(ctx context.Context, token string) (*cloudlink.RelaySocket, error)
}

// Links owns the peer links of this machine: one row per link in the
// store, the keys in the secrets, and one relay.Peer per link in memory.
type Links struct {
	st      *store.Store
	cloud   linkCloud
	version string
	log     *slog.Logger

	mu    sync.Mutex
	peers map[string]*relay.Peer // by machine id
}

// NewLinks builds the links. version is the Trebi version in the hello.
func NewLinks(st *store.Store, cloud linkCloud, version string) *Links {
	return &Links{st: st, cloud: cloud, version: version, log: slog.Default().With("source", "machine_links"), peers: map[string]*relay.Peer{}}
}

// DialRelay adapts the cloud socket to relay.PeerDialer.
func (l *Links) DialRelay(ctx context.Context, token string) (relay.PeerConn, error) {
	s, err := l.cloud.DialRelay(ctx, token)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Connect pairs this machine with m through the pairing link that m made.
// A link that exists already is replaced.
func (l *Links) Connect(ctx context.Context, m Machine, raw string) error {
	if m.IsSelf || m.DeviceID == nil {
		return fmt.Errorf("%w: this machine cannot connect to itself", ErrBadLink)
	}
	if l.cloud.DeviceID() == "" {
		return cloudlink.ErrNotLinked
	}
	p, err := mobile.ParseLink(raw)
	if err != nil {
		return fmt.Errorf("%w: this is not a pairing link", ErrBadLink)
	}
	if p.DeviceID != *m.DeviceID {
		return fmt.Errorf("%w: this link is for another machine", ErrBadLink)
	}
	if p.APIHost != l.cloud.APIHost() {
		return fmt.Errorf("%w: this link is for another Trebi Cloud", ErrBadLink)
	}
	static := make([]byte, 32)
	if _, err := rand.Read(static); err != nil {
		return err
	}
	name := l.cloud.DeviceName()
	enr, err := l.cloud.EnrollPeer(ctx, p.Claim, name)
	var apiErr *cloudlink.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "bad_claim" {
		return fmt.Errorf("%w: the link was already used or has expired", ErrLinkUsed)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", m.Name, err)
	}
	creds := relay.PeerCreds{DeviceID: p.DeviceID, ClientID: enr.ClientID, Token: enr.Token, Static: static, DaemonKey: p.DaemonKey, PSK: p.PSK}
	creds, err = relay.PairPeer(ctx, l, creds, p.OfferID, name, l.version)
	if err != nil {
		l.leave(ctx, enr.Token)
		return fmt.Errorf("connect to %s: %w", m.Name, err)
	}
	old, _ := l.creds(ctx, m.ID)
	if err := l.save(ctx, m, creds); err != nil {
		l.leave(ctx, enr.Token)
		return err
	}
	if old.Token != "" {
		l.leave(ctx, old.Token)
	}
	l.mu.Lock()
	if prev := l.peers[m.ID]; prev != nil {
		prev.Close()
	}
	l.peers[m.ID] = relay.NewPeer(creds, l, name, l.version)
	l.mu.Unlock()
	l.log.Info("machine.link", "machine_id", m.ID, "state", LinkLinked)
	return nil
}

// Disconnect removes the link to a machine, here and in the cloud.
func (l *Links) Disconnect(ctx context.Context, m Machine) error {
	creds, err := l.creds(ctx, m.ID)
	if errors.Is(err, ErrNoLink) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = l.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM machine_links WHERE machine_id = ?`, m.ID)
		return err
	}, store.BusEvent{Topic: "machine.updated", Entity: "machine", EntityID: m.ID, Action: "updated", Revision: m.Revision,
		Attrs: map[string]string{"link": LinkNone}})
	if err != nil {
		return fmt.Errorf("disconnect %s: %w", m.ID, err)
	}
	if err := secrets.Delete(linkSecret(m.ID)); err != nil {
		l.log.Warn("machine.link", "machine_id", m.ID, "err", err)
	}
	l.mu.Lock()
	if p := l.peers[m.ID]; p != nil {
		p.Close()
		delete(l.peers, m.ID)
	}
	l.mu.Unlock()
	l.leave(ctx, creds.Token)
	l.log.Info("machine.link", "machine_id", m.ID, "state", LinkNone)
	return nil
}

// Peer returns the client of the link to a machine.
func (l *Links) Peer(ctx context.Context, machineID string) (*relay.Peer, error) {
	l.mu.Lock()
	p := l.peers[machineID]
	l.mu.Unlock()
	if p != nil {
		return p, nil
	}
	creds, err := l.creds(ctx, machineID)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if p := l.peers[machineID]; p != nil {
		return p, nil
	}
	p = relay.NewPeer(creds, l, l.cloud.DeviceName(), l.version)
	l.peers[machineID] = p
	return p, nil
}

// Close ends every session.
func (l *Links) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, p := range l.peers {
		p.Close()
		delete(l.peers, id)
	}
}

func (l *Links) save(ctx context.Context, m Machine, creds relay.PeerCreds) error {
	raw, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	if err := secrets.Set(linkSecret(m.ID), string(raw)); err != nil {
		return fmt.Errorf("save link %s: %w", m.ID, err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = l.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO machine_links (machine_id, device_id, client_id, linked_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (machine_id) DO UPDATE SET device_id = excluded.device_id, client_id = excluded.client_id, linked_at = excluded.linked_at`,
			m.ID, creds.DeviceID, creds.ClientID, now)
		return err
	}, store.BusEvent{Topic: "machine.updated", Entity: "machine", EntityID: m.ID, Action: "updated", Revision: m.Revision,
		Attrs: map[string]string{"link": LinkLinked}})
	if err != nil {
		return fmt.Errorf("save link %s: %w", m.ID, err)
	}
	return nil
}

func (l *Links) creds(ctx context.Context, machineID string) (relay.PeerCreds, error) {
	var device string
	err := l.st.DB().QueryRowContext(ctx, `SELECT device_id FROM machine_links WHERE machine_id = ?`, machineID).Scan(&device)
	if errors.Is(err, sql.ErrNoRows) {
		return relay.PeerCreds{}, ErrNoLink
	}
	if err != nil {
		return relay.PeerCreds{}, fmt.Errorf("read link %s: %w", machineID, err)
	}
	raw, err := secrets.Get(linkSecret(machineID))
	if err != nil {
		return relay.PeerCreds{}, fmt.Errorf("read link %s: %w", machineID, err)
	}
	var creds relay.PeerCreds
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		return relay.PeerCreds{}, fmt.Errorf("read link %s: %w", machineID, err)
	}
	return creds, nil
}

// leave removes a client token from the cloud. It is best effort: the
// target machine can also remove the client.
func (l *Links) leave(ctx context.Context, token string) {
	if err := l.cloud.LeaveClient(context.WithoutCancel(ctx), token); err != nil {
		l.log.Warn("machine.link", "action", "leave", "err", err)
	}
}

func linkSecret(machineID string) string { return "machine_link_" + machineID }

// fill sets the link state on each machine that is not self.
func (l *Links) fill(ctx context.Context, list []Machine) error {
	rows, err := l.st.DB().QueryContext(ctx, `SELECT machine_id, linked_at FROM machine_links`)
	if err != nil {
		return fmt.Errorf("list machine links: %w", err)
	}
	defer rows.Close()
	linked := map[string]string{}
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			return err
		}
		linked[id] = at
	}
	if err := rows.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range list {
		if list[i].IsSelf {
			continue
		}
		at, ok := linked[list[i].ID]
		if !ok {
			list[i].Link = &Link{State: LinkNone}
			continue
		}
		link := &Link{State: LinkLinked, LinkedAt: &at}
		if p := l.peers[list[i].ID]; p != nil && p.Err() != nil {
			link.State, link.Error = LinkError, p.Err().Error()
		}
		list[i].Link = link
	}
	return nil
}
