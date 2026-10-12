// Package mobile owns the paired phones, the pairing offers, and the
// daemon static key of the relay (plan 2026-10-09 B3). Offers live in
// memory only: a daemon restart cancels them.
package mobile

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/flynn/noise"
	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
	"golang.org/x/crypto/curve25519"
)

// Scopes and platforms.
const (
	ScopeFull       = "full"
	ScopeRead       = "read"
	PlatformIOS     = "ios"
	PlatformAndroid = "android"
)

// Offer limits (plan B3; H2 needs the maximum for App Review).
const (
	DefaultTTL  = 10 * time.Minute
	MaxTTL      = 30 * 24 * time.Hour
	MinTTL      = time.Minute
	DefaultUses = 1
	MaxUses     = 10
)

// PairBase is the pairing host. The secrets ride in the fragment.
const PairBase = "https://pair.trebi.ai/"

const staticKeySecret = "mobile_static_key"

var (
	// ErrNotLinked is a pairing on a machine with no cloud link.
	ErrNotLinked = errors.New("mobile: machine is not linked")
	// ErrRemoteAccess is a pairing without the remote_access grant.
	ErrRemoteAccess = errors.New("mobile: plan has no remote_access")
	// ErrMobileOff is a pairing while cloud.mobile is false.
	ErrMobileOff = errors.New("mobile: cloud.mobile is off")
	// ErrNotFound is an unknown phone or offer id.
	ErrNotFound = errors.New("mobile: not found")
	// ErrValidation is a bad input value.
	ErrValidation = errors.New("mobile: invalid input")
)

// Client is one paired phone. PublicKey and PSK are never served.
type Client struct {
	ID            string     `json:"id"`
	CloudClientID string     `json:"-"`
	Name          string     `json:"name"`
	Platform      string     `json:"platform"`
	PublicKey     []byte     `json:"-"`
	PSK           []byte     `json:"-"`
	Scope         string     `json:"scope"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSeenAt    *time.Time `json:"last_seen_at"`
	Connected     bool       `json:"connected"`
}

// Enrollment is what a pairing handshake proves about a new phone.
type Enrollment struct {
	CloudClientID string
	Name          string
	Platform      string
	PublicKey     []byte
}

// Offer is one open pairing. PSK and Claim ride only in Link.
type Offer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Link      string    `json:"link"`
	ExpiresAt time.Time `json:"expires_at"`
	Uses      int       `json:"uses"`
	psk       []byte
}

// PairInput holds the optional pairing values. Zero means the default.
type PairInput struct {
	Name string
	TTL  time.Duration
	Uses int
}

// UpdateInput changes a phone. A nil field stays.
type UpdateInput struct {
	Name  *string
	Scope *string
}

// Target is the linked machine a pairing link names.
type Target struct {
	APIHost      string // host[:port] of the cloud api
	DeviceID     string
	Machine      string
	Linked       bool
	RemoteAccess bool
	MobileOn     bool
}

// Cloud is the cloud side of pairing. The daemon adapts cloudlink.Service.
type Cloud interface {
	PairTarget() Target
	MintClaim(ctx context.Context, uses int, ttl time.Duration) (string, time.Time, error)
	RevokeClient(ctx context.Context, cloudClientID string) error
}

// Sessions ends the live sessions of a phone. relay.Hub implements it.
type Sessions interface {
	CloseClient(clientID, code string)
	Connected(clientID string) bool
	DirectError() error
}

// Service owns the mobile_clients table and the offers.
type Service struct {
	st    *store.Store
	cloud Cloud
	log   *slog.Logger
	now   func() time.Time

	mu       sync.Mutex
	sessions Sessions
	offers   map[string]*Offer
	key      *noise.DHKey
	seen     map[string]time.Time // last write per phone, to limit writes
}

// New builds the service.
func New(st *store.Store, cloud Cloud) *Service {
	return &Service{st: st, cloud: cloud, log: slog.Default().With("source", "mobile"), now: time.Now,
		offers: map[string]*Offer{}, seen: map[string]time.Time{}}
}

// WithSessions wires the relay hub after it is built.
func (s *Service) WithSessions(h Sessions) *Service {
	s.mu.Lock()
	s.sessions = h
	s.mu.Unlock()
	return s
}

// StaticKey loads or mints the daemon X25519 key.
func (s *Service) StaticKey() (noise.DHKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil {
		return *s.key, nil
	}
	if v, err := secrets.Get(staticKeySecret); err == nil {
		priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(v)))
		if err == nil && len(priv) == 32 {
			pub, err := curve25519.X25519(priv, curve25519.Basepoint)
			if err == nil {
				s.key = &noise.DHKey{Private: priv, Public: pub}
				return *s.key, nil
			}
		}
		return noise.DHKey{}, errors.New("mobile: stored static key is not a 32-byte X25519 key")
	}
	k, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		return noise.DHKey{}, fmt.Errorf("mobile static key: %w", err)
	}
	if err := secrets.Set(staticKeySecret, base64.StdEncoding.EncodeToString(k.Private)); err != nil {
		return noise.DHKey{}, fmt.Errorf("mobile static key: %w", err)
	}
	s.key = &k
	return k, nil
}

// Pair opens an offer and returns its link.
func (s *Service) Pair(ctx context.Context, in PairInput) (Offer, error) {
	t := s.cloud.PairTarget()
	switch {
	case !t.Linked:
		return Offer{}, ErrNotLinked
	case !t.RemoteAccess:
		return Offer{}, ErrRemoteAccess
	case !t.MobileOn:
		return Offer{}, ErrMobileOff
	}
	if in.TTL == 0 {
		in.TTL = DefaultTTL
	}
	if in.Uses == 0 {
		in.Uses = DefaultUses
	}
	if in.TTL < MinTTL || in.TTL > MaxTTL {
		return Offer{}, fmt.Errorf("%w: ttl must be from 1m to 720h", ErrValidation)
	}
	if in.Uses < 1 || in.Uses > MaxUses {
		return Offer{}, fmt.Errorf("%w: uses must be from 1 to %d", ErrValidation, MaxUses)
	}
	key, err := s.StaticKey()
	if err != nil {
		return Offer{}, err
	}
	claim, expires, err := s.cloud.MintClaim(ctx, in.Uses, in.TTL)
	if err != nil {
		return Offer{}, err
	}
	psk := make([]byte, 32)
	if _, err := rand.Read(psk); err != nil {
		return Offer{}, err
	}
	o := &Offer{ID: ulid.Make().String(), Name: strings.TrimSpace(in.Name), ExpiresAt: expires.UTC(), Uses: in.Uses, psk: psk}
	if local := s.now().Add(in.TTL).UTC(); expires.IsZero() || local.Before(o.ExpiresAt) {
		o.ExpiresAt = local
	}
	o.Link = PairLink(LinkParts{APIHost: t.APIHost, DeviceID: t.DeviceID, DaemonKey: key.Public, PSK: psk, Claim: claim, OfferID: o.ID, Machine: t.Machine})
	s.mu.Lock()
	s.pruneLocked()
	s.offers[o.ID] = o
	s.mu.Unlock()
	return *o, nil
}

// CancelOffer drops an open offer.
func (s *Service) CancelOffer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.offers[id]; !ok {
		return ErrNotFound
	}
	delete(s.offers, id)
	return nil
}

// OfferPSK returns the psk of a live offer with uses left.
func (s *Service) OfferPSK(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	o, ok := s.offers[id]
	if !ok {
		return nil, false
	}
	return o.psk, true
}

// Redeem spends one use of an offer and saves the new phone with its psk.
func (s *Service) Redeem(ctx context.Context, offerID string, in Enrollment, psk []byte) (Client, error) {
	s.mu.Lock()
	s.pruneLocked()
	o, ok := s.offers[offerID]
	if ok {
		o.Uses--
		if o.Uses <= 0 {
			delete(s.offers, offerID)
		}
	}
	s.mu.Unlock()
	if !ok {
		return Client{}, ErrNotFound
	}
	name := strings.TrimSpace(in.Name)
	if o.Name != "" {
		name = o.Name
	}
	if name == "" {
		name = "Phone"
	}
	if in.Platform != PlatformIOS && in.Platform != PlatformAndroid {
		return Client{}, fmt.Errorf("%w: platform %q", ErrValidation, in.Platform)
	}
	c := Client{ID: ulid.Make().String(), CloudClientID: in.CloudClientID, Name: name, Platform: in.Platform,
		PublicKey: in.PublicKey, PSK: psk, Scope: ScopeFull, CreatedAt: s.now().UTC().Truncate(time.Second)}
	if err := secrets.Set(pskSecret(c.ID), base64.StdEncoding.EncodeToString(psk)); err != nil {
		return Client{}, err
	}
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		// A phone that pairs again with the same cloud id replaces its row.
		if _, err := tx.Exec(`DELETE FROM mobile_clients WHERE cloud_client_id = ? OR public_key = ?`, c.CloudClientID, c.PublicKey); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO mobile_clients (id, cloud_client_id, name, platform, public_key, scope, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, c.ID, c.CloudClientID, c.Name, c.Platform, c.PublicKey, c.Scope, c.CreatedAt.Format(time.RFC3339))
		return err
	}, store.BusEvent{Topic: "mobile.paired", Entity: "mobile_client", EntityID: c.ID, Action: "paired",
		Attrs: map[string]string{"client_id": c.ID, "name": c.Name, "platform": c.Platform, "offer_id": offerID}})
	if err != nil {
		_ = secrets.Delete(pskSecret(c.ID)) // no half pairing
		return Client{}, fmt.Errorf("mobile pair: %w", err)
	}
	s.log.Info("mobile.paired", "client_id", c.ID, "platform", c.Platform)
	return c, nil
}

// List returns the paired phones, newest first.
func (s *Service) List(ctx context.Context) ([]Client, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, cloud_client_id, name, platform, public_key, scope, created_at, last_seen_at
		FROM mobile_clients ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("mobile list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Client{}
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s.withLive(c))
	}
	return out, rows.Err()
}

// Get returns one phone.
func (s *Service) Get(ctx context.Context, id string) (Client, error) {
	row := s.st.DB().QueryRowContext(ctx, `SELECT id, cloud_client_id, name, platform, public_key, scope, created_at, last_seen_at
		FROM mobile_clients WHERE id = ?`, id)
	c, err := scanClient(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Client{}, ErrNotFound
	}
	if err != nil {
		return Client{}, err
	}
	return s.withLive(c), nil
}

// Update renames a phone or changes its scope. A scope change ends its
// sessions, so the next session carries the new scope.
func (s *Service) Update(ctx context.Context, id string, in UpdateInput) (Client, error) {
	c, err := s.Get(ctx, id)
	if err != nil {
		return Client{}, err
	}
	scopeChanged := false
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 64 {
			return Client{}, fmt.Errorf("%w: name must be 1 to 64 characters", ErrValidation)
		}
		c.Name = name
	}
	if in.Scope != nil {
		if *in.Scope != ScopeFull && *in.Scope != ScopeRead {
			return Client{}, fmt.Errorf("%w: scope must be full or read", ErrValidation)
		}
		scopeChanged = *in.Scope != c.Scope
		c.Scope = *in.Scope
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE mobile_clients SET name = ?, scope = ? WHERE id = ?`, c.Name, c.Scope, id)
		return err
	}, store.BusEvent{Topic: "mobile.updated", Entity: "mobile_client", EntityID: id, Action: "updated"})
	if err != nil {
		return Client{}, fmt.Errorf("mobile update %s: %w", id, err)
	}
	if scopeChanged {
		if h := s.hub(); h != nil {
			h.CloseClient(id, "scope_changed")
		}
	}
	return s.Get(ctx, id)
}

// Remove ends the sessions, deletes the phone, and revokes it in the
// cloud. A failed cloud call does not undo the local remove: the next
// handshake already fails.
func (s *Service) Remove(ctx context.Context, id string) error {
	c, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if h := s.hub(); h != nil {
		h.CloseClient(id, "revoked")
	}
	if err := s.deleteLocal(ctx, c); err != nil {
		return err
	}
	if err := s.cloud.RevokeClient(ctx, c.CloudClientID); err != nil {
		s.log.Warn("mobile.revoke", "client_id", id, "err", err)
	}
	return nil
}

// Revoked removes a phone that the cloud removed. It makes no cloud call.
func (s *Service) Revoked(ctx context.Context, cloudClientID string) {
	row := s.st.DB().QueryRowContext(ctx, `SELECT id, cloud_client_id, name, platform, public_key, scope, created_at, last_seen_at
		FROM mobile_clients WHERE cloud_client_id = ?`, cloudClientID)
	c, err := scanClient(row)
	if err != nil {
		return
	}
	if h := s.hub(); h != nil {
		h.CloseClient(c.ID, "revoked")
	}
	if err := s.deleteLocal(ctx, c); err != nil {
		s.log.Warn("mobile.revoked", "client_id", c.ID, "err", err)
	}
}

// Lookup finds a phone by its static key, with its psk.
func (s *Service) Lookup(pub []byte) (Client, bool) {
	row := s.st.DB().QueryRow(`SELECT id, cloud_client_id, name, platform, public_key, scope, created_at, last_seen_at
		FROM mobile_clients WHERE public_key = ?`, pub)
	c, err := scanClient(row)
	if err != nil {
		return Client{}, false
	}
	v, err := secrets.Get(pskSecret(c.ID))
	if err != nil {
		return Client{}, false
	}
	psk, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(v)))
	if err != nil || len(psk) != 32 {
		return Client{}, false
	}
	c.PSK = psk
	return c, true
}

// Seen records a session start, at most once per minute per phone.
func (s *Service) Seen(id string, at time.Time) {
	s.mu.Lock()
	last := s.seen[id]
	if at.Sub(last) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.seen[id] = at
	s.mu.Unlock()
	if _, err := s.st.DB().Exec(`UPDATE mobile_clients SET last_seen_at = ? WHERE id = ?`, at.UTC().Format(time.RFC3339), id); err != nil {
		s.log.Warn("mobile.seen", "client_id", id, "err", err)
	}
}

// Connected publishes mobile.connected or mobile.disconnected.
func (s *Service) Connected(ctx context.Context, clientID string, on bool) {
	topic, action := "mobile.connected", "connected"
	if !on {
		topic, action = "mobile.disconnected", "disconnected"
	}
	if _, err := s.st.Append(ctx, store.BusEvent{Topic: topic, Entity: "mobile_client", EntityID: clientID, Action: action,
		Attrs: map[string]string{"client_id": clientID}}); err != nil {
		s.log.Warn("bus append", "topic", topic, "err", err)
	}
}

type scanner interface{ Scan(dest ...any) error }

// LinkParts are the values of a pairing link.
type LinkParts struct {
	APIHost   string
	DeviceID  string
	DaemonKey []byte
	PSK       []byte
	Claim     string
	OfferID   string
	Machine   string
}

// PairLink builds https://pair.trebi.ai/#v=1&a=…&d=…&k=…&p=…&c=…&o=…&n=….
// Keys are base64url without padding. A browser never sends the fragment.
func PairLink(p LinkParts) string {
	b64 := base64.RawURLEncoding
	q := []string{
		"v=1",
		"a=" + url.QueryEscape(p.APIHost),
		"d=" + url.QueryEscape(p.DeviceID),
		"k=" + b64.EncodeToString(p.DaemonKey),
		"p=" + b64.EncodeToString(p.PSK),
		"c=" + url.QueryEscape(p.Claim),
		"o=" + url.QueryEscape(p.OfferID),
		"n=" + url.QueryEscape(p.Machine),
	}
	return PairBase + "#" + strings.Join(q, "&")
}

func (s *Service) deleteLocal(ctx context.Context, c Client) error {
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM mobile_clients WHERE id = ?`, c.ID)
		return err
	}, store.BusEvent{Topic: "mobile.removed", Entity: "mobile_client", EntityID: c.ID, Action: "removed",
		Attrs: map[string]string{"client_id": c.ID}})
	if err != nil {
		return fmt.Errorf("mobile remove %s: %w", c.ID, err)
	}
	if err := secrets.Delete(pskSecret(c.ID)); err != nil {
		s.log.Warn("mobile.remove", "client_id", c.ID, "err", err)
	}
	s.log.Info("mobile.removed", "client_id", c.ID)
	return nil
}

func (s *Service) hub() Sessions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions
}

// DirectError is the last bind error of the direct listener for phones on
// the same network, or nil.
func (s *Service) DirectError() error {
	if h := s.hub(); h != nil {
		return h.DirectError()
	}
	return nil
}

func (s *Service) withLive(c Client) Client {
	if h := s.hub(); h != nil {
		c.Connected = h.Connected(c.ID)
	}
	return c
}

func (s *Service) pruneLocked() {
	now := s.now()
	for id, o := range s.offers {
		if !now.Before(o.ExpiresAt) || o.Uses <= 0 {
			delete(s.offers, id)
		}
	}
}

func pskSecret(id string) string { return "mobile_psk_" + id }

func scanClient(r scanner) (Client, error) {
	var (
		c       Client
		created string
		seen    sql.NullString
	)
	if err := r.Scan(&c.ID, &c.CloudClientID, &c.Name, &c.Platform, &c.PublicKey, &c.Scope, &created, &seen); err != nil {
		return Client{}, err
	}
	c.CreatedAt = store.ParseTS(created)
	if seen.Valid {
		t := store.ParseTS(seen.String)
		c.LastSeenAt = &t
	}
	return c, nil
}
