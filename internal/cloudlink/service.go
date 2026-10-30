package cloudlink

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/store"
)

// Settings is the resolved cloud block handed to the service at wiring
// time. The config package owns parsing; this is what the service acts on.
type Settings struct {
	APIURLs   []string // config cloud.api_url, then cloud.fallback_urls
	Enabled   bool     // config cloud.enabled; a linked device with Enabled false stays offline
	RemoteMCP bool     // config cloud.remote_mcp
	Mobile    bool     // config cloud.mobile
}

// MailSink is the email channel. email.Service implements it.
type MailSink interface {
	Deliver(ctx context.Context, msg email.IngressMessage) error
}

// WebhookSink turns hook frames into events. webhooks.Service implements it.
type WebhookSink interface {
	Deliver(ctx context.Context, f WebhookIngress) error
}

// RelaySink owns the phone sessions. relay.Hub implements it. The bytes
// are opaque here: the socket decodes only the base64.
type RelaySink interface {
	OpenRelay(ctx context.Context, sid, clientID string)
	DeliverRelay(ctx context.Context, sid string, raw []byte)
	CloseRelay(sid, reason string)
	ClientRevoked(ctx context.Context, clientID string)
	Count() int
}

// McpSink serves one MCP request that the cloud forwards. api.Server implements it.
type McpSink interface {
	ServeRemoteMCP(ctx context.Context, req McpRequest) McpResponse
}

// CapabilitySource builds the capability summary of the machine.
type CapabilitySource interface {
	CloudCapabilities(ctx context.Context) (Capabilities, error)
}

// LoginSink ends an OAuth login that came back through the cloud return
// page. connectors.Service implements it through a daemon adapter.
type LoginSink interface {
	ReturnLogin(ctx context.Context, f LoginReturn) LoginResult
}

// Status is the cloud-link fact sheet the API serves verbatim.
type Status struct {
	Linked         bool     `json:"linked"`
	Status         string   `json:"status"` // not_linked | linked | connected | degraded | unauthorized
	AccountEmail   string   `json:"account_email"`
	DeviceName     string   `json:"device_name"`
	Plan           string   `json:"plan"`
	Features       []string `json:"features"`
	Connected      bool     `json:"connected"`
	RefreshedAt    string   `json:"refreshed_at"`    // RFC3339, "" when never refreshed
	RemoteMCP      bool     `json:"remote_mcp"`      // config cloud.remote_mcp
	McpURL         string   `json:"mcp_url"`         // from hello_ok, "" before the first connect
	DownSince      string   `json:"down_since"`      // RFC3339 start of the current outage; set only when degraded
	Mobile         bool     `json:"mobile"`          // config cloud.mobile
	MobileSessions int      `json:"mobile_sessions"` // open phone sessions
	Endpoint       string   `json:"endpoint"`        // host of the active cloud address
	Fallback       bool     `json:"fallback"`        // true while this network blocks the main address
}

// Service is the long-lived cloud-link worker: the cached link row, the
// hosted objects the worker mints, and the socket that receives pushed
// ingress. The row is a cache of cloud truth; the cloud re-checks the plan
// on every granted action.
type Service struct {
	st    *store.Store
	log   *slog.Logger
	cfg   Settings
	mail  MailSink
	hooks WebhookSink
	relay RelaySink
	mcp   McpSink
	login LoginSink
	caps  CapabilitySource

	onChange []func(ctx context.Context)

	mu     sync.Mutex
	link   *linkRow
	loaded bool
	ep     *endpoints // shared by the client and the socket; see endpointsLocked
	sock   *socket
	life   context.Context
	cancel context.CancelFunc
	done   chan struct{}
	mcpURL string
}

// linkRow is the single cloud_link row.
type linkRow struct {
	deviceID     string
	deviceName   string
	apiURL       string
	accountEmail string
	plan         string
	features     []string
	linkedAt     string
	refreshedAt  string
}

// The Status.Status values the API serves verbatim.
const (
	statusNotLinked    = "not_linked"
	statusLinked       = "linked"
	statusConnected    = "connected"
	statusDegraded     = "degraded"
	statusUnauthorized = "unauthorized"
)

var (
	// ErrNotLinked marks calls that need a device identity.
	ErrNotLinked = errors.New("cloud link is required")
	// ErrAlreadyLinked marks a Link against an existing cloud_link row.
	ErrAlreadyLinked = errors.New("cloud link already exists")
)

// New creates the cloud-link service.
func New(st *store.Store) *Service {
	return &Service{st: st, log: slog.Default().With("source", "cloudlink")}
}

// Configure applies the resolved config block. Safe before Start.
// A RemoteMCP or Mobile change drops the socket, so the redial sends new caps.
func (s *Service) Configure(cfg Settings) {
	s.mu.Lock()
	changed := s.cfg.RemoteMCP != cfg.RemoteMCP || s.cfg.Mobile != cfg.Mobile
	s.cfg = cfg
	sk := s.sock
	s.mu.Unlock()
	if changed && sk != nil {
		sk.close()
	}
}

// SetRemoteMCP changes only the RemoteMCP setting.
func (s *Service) SetRemoteMCP(on bool) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	cfg.RemoteMCP = on
	s.Configure(cfg)
}

// SetMobile changes only the Mobile setting.
func (s *Service) SetMobile(on bool) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	cfg.Mobile = on
	s.Configure(cfg)
}

// MobileOn reports whether config cloud.mobile is on.
func (s *Service) MobileOn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Mobile
}

// SendRelay writes one relay_frame. It fails when the socket is down.
func (s *Service) SendRelay(sid string, raw []byte) error {
	return s.writeFrame(RelayFrame{V: ContractV, Type: "relay_frame", SID: sid, Bytes: base64.StdEncoding.EncodeToString(raw)})
}

// CloseRelay tells the cloud to close one phone socket.
func (s *Service) CloseRelay(sid, reason string) error {
	s.mu.Lock()
	sk := s.sock
	s.mu.Unlock()
	if sk != nil {
		sk.dropSid(sid)
	}
	return s.writeFrame(RelayClose{V: ContractV, Type: "relay_close", SID: sid, Reason: reason})
}

// SendAlert writes one alert frame. The cloud sends a push to the phones of the account.
func (s *Service) SendAlert(_ context.Context, a Alert) error {
	a.V, a.Type = ContractV, "alert"
	return s.writeFrame(a)
}

// DeviceID is the cloud id of this machine, or "" when not linked.
func (s *Service) DeviceID() string {
	s.ensure()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.link == nil {
		return ""
	}
	return s.link.deviceID
}

// DeviceName is the machine name of the link, or "" when not linked.
func (s *Service) DeviceName() string {
	s.ensure()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.link == nil {
		return ""
	}
	return s.link.deviceName
}

// APIHost is host[:port] of the cloud api, for the pairing link.
func (s *Service) APIHost() string {
	s.ensure()
	s.mu.Lock()
	defer s.mu.Unlock()
	ep := s.endpointsLocked()
	if ep == nil {
		return ""
	}
	return hostOf(ep.Primary()) // the phone maps it to its fallback itself
}

// MintClaim mints a one-time pairing claim for a phone.
func (s *Service) MintClaim(ctx context.Context, uses int, ttl time.Duration) (Claim, error) {
	c, err := s.client()
	if err != nil {
		return Claim{}, err
	}
	return c.MintClaim(ctx, uses, ttl)
}

// RevokeClient removes a phone from the account.
func (s *Service) RevokeClient(ctx context.Context, cloudClientID string) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	return c.RevokeClient(ctx, cloudClientID)
}

// WithMCP wires the remote MCP sink.
func (s *Service) WithMCP(m McpSink) *Service {
	s.mu.Lock()
	s.mcp = m
	s.mu.Unlock()
	return s
}

// McpURL is the URL from the last hello_ok, or "".
func (s *Service) McpURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mcpURL
}

// remoteMCP reports whether the socket may serve MCP requests.
func (s *Service) remoteMCP() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.RemoteMCP && s.mcp != nil
}

func (s *Service) setMcpURL(u string) {
	s.mu.Lock()
	s.mcpURL = u
	s.mu.Unlock()
}

// WithMail wires the email channel sink.
func (s *Service) WithMail(m MailSink) *Service {
	s.mail = m
	return s
}

// WithWebhooks wires the webhook ingress sink.
func (s *Service) WithWebhooks(w WebhookSink) *Service {
	s.hooks = w
	return s
}

// WithRelay wires the phone session hub.
func (s *Service) WithRelay(r RelaySink) *Service {
	s.mu.Lock()
	s.relay = r
	s.mu.Unlock()
	return s
}

// WithLogin wires the OAuth login return sink.
func (s *Service) WithLogin(l LoginSink) *Service {
	s.mu.Lock()
	s.login = l
	s.mu.Unlock()
	return s
}

// WithCapabilities wires the source of the heartbeat capability summary.
func (s *Service) WithCapabilities(c CapabilitySource) *Service {
	s.mu.Lock()
	s.caps = c
	s.mu.Unlock()
	return s
}

// WithOnChange registers fn, fired after Link, Unlink, and every
// plan/features write.
func (s *Service) WithOnChange(fn func(ctx context.Context)) *Service {
	s.onChange = append(s.onChange, fn)
	return s
}

// Start spawns the socket loop when linked and enabled, and returns at
// once. The loop stops on ctx.Done or Stop.
func (s *Service) Start(ctx context.Context) {
	s.ensure()
	s.mu.Lock()
	s.ensureRunLocked()
	life := s.life
	var (
		id     Identity
		idErr  error
		launch = s.link != nil && s.cfg.Enabled && s.sock == nil
	)
	if launch {
		id, idErr = LoadIdentity(s.link.deviceID)
		if idErr == nil {
			s.launchLocked(id)
		}
	}
	s.mu.Unlock()
	if idErr != nil {
		s.log.Error("cloudlink.identity", "err", idErr)
	}
	// Stop condition: the daemon's ctx ends (shutdown) or Stop is called.
	go func() {
		select {
		case <-ctx.Done():
			s.Stop()
		case <-life.Done():
		}
	}()
}

// Stop closes the socket and waits for the loop to exit. Idempotent.
func (s *Service) Stop() {
	s.mu.Lock()
	sk, done, cancel := s.sock, s.done, s.cancel
	s.sock, s.done, s.life, s.cancel = nil, nil, nil, nil
	s.mu.Unlock()
	if sk != nil {
		sk.close()
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Status reads the cached link row and socket state. It makes no calls.
func (s *Service) Status() Status {
	s.ensure()
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.link
	if row == nil {
		return Status{Status: statusNotLinked, Features: []string{}, RemoteMCP: s.cfg.RemoteMCP, Mobile: s.cfg.Mobile}
	}
	st := Status{
		Linked:       true,
		AccountEmail: row.accountEmail,
		DeviceName:   row.deviceName,
		Plan:         row.plan,
		Features:     row.features,
		RefreshedAt:  row.refreshedAt,
		RemoteMCP:    s.cfg.RemoteMCP,
		McpURL:       s.mcpURL,
		Mobile:       s.cfg.Mobile,
	}
	if s.relay != nil {
		st.MobileSessions = s.relay.Count()
	}
	if ep := s.endpointsLocked(); ep != nil {
		st.Endpoint, st.Fallback = hostOf(ep.Current()), ep.OnFallback()
	}
	if st.Features == nil {
		st.Features = []string{}
	}
	st.Connected = s.sock != nil && s.sock.isConnected()
	switch {
	case s.sock != nil && s.sock.isUnauthorized():
		st.Status = statusUnauthorized
	case !s.cfg.Enabled || s.sock == nil:
		st.Status = statusLinked
	case st.Connected:
		st.Status = statusConnected
	default:
		st.Status = statusDegraded
		if t := s.sock.downSinceAt(); !t.IsZero() {
			st.DownSince = t.UTC().Format(time.RFC3339)
		}
	}
	return st
}

// Entitlement is the grant cached in the cloud_link row.
func (s *Service) Entitlement() entitle.State {
	s.ensure()
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.link
	if row == nil {
		return entitle.State{}
	}
	st := entitle.State{Linked: true, Plan: row.plan, RefreshedAt: store.ParseTS(row.refreshedAt)}
	for _, f := range row.features {
		st.Features = append(st.Features, entitle.Feature(f))
	}
	return st
}

// Link mints the device keypair, registers it, saves the cloud_link row,
// and starts the socket when enabled. On any error after the key is
// minted the key is deleted: no half-link.
func (s *Service) Link(ctx context.Context, enrollToken, name string) (Status, error) {
	s.ensure()
	s.mu.Lock()
	if s.link != nil {
		s.mu.Unlock()
		return Status{}, ErrAlreadyLinked
	}
	ep := s.endpointsLocked()
	s.mu.Unlock()
	if ep == nil {
		return Status{}, errors.New("cloud api url is not configured")
	}

	id, err := MintIdentity()
	if err != nil {
		return Status{}, err
	}
	deviceID, err := NewClient(ep, id).Register(ctx, enrollToken, name)
	if err != nil {
		_ = DeleteIdentity() // no half-link: drop the key minted above
		return Status{}, err
	}
	id = id.ForDevice(deviceID)
	now := time.Now().UTC().Format(time.RFC3339)
	row := &linkRow{
		deviceID:    deviceID,
		deviceName:  name,
		apiURL:      ep.Primary(),
		features:    []string{},
		linkedAt:    now,
		refreshedAt: now,
	}
	if err := s.saveLink(ctx, row); err != nil {
		_ = DeleteIdentity()
		return Status{}, err
	}
	s.mu.Lock()
	s.link = row
	s.loaded = true
	s.launchLocked(id)
	s.mu.Unlock()
	s.fireOnChange(ctx)
	return s.Status(), nil
}

// Unlink stops the socket, deletes the row and the key, and publishes
// cloud.unlinked. No cloud call: a local unlink must work offline and the
// account page revokes the device.
func (s *Service) Unlink(ctx context.Context) error {
	s.Stop()
	s.ensure()
	s.mu.Lock()
	row := s.link
	s.link = nil
	s.loaded = true
	s.mu.Unlock()

	entityID := "cloud"
	if row != nil {
		entityID = row.deviceID
	}
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM cloud_link`)
		return err
	}, store.BusEvent{Topic: "cloud.unlinked", Entity: "cloud", EntityID: entityID, Action: "unlinked"})
	if err != nil {
		return fmt.Errorf("cloud unlink: %w", err)
	}
	if err := DeleteIdentity(); err != nil {
		return err
	}
	s.fireOnChange(ctx)
	return nil
}

// MintAddress mints a hosted address for a workspace and records the row.
func (s *Service) MintAddress(ctx context.Context, workspaceID, handle string) (Address, error) {
	c, err := s.client()
	if err != nil {
		return Address{}, err
	}
	addr, err := c.MintAddress(ctx, workspaceID, handle)
	if err != nil {
		return Address{}, err
	}
	addr.WorkspaceID = workspaceID
	addr.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	// One address per workspace: a re-mint replaces the previous row.
	_, err = s.st.DB().ExecContext(ctx,
		`INSERT INTO cloud_addresses (id, workspace_id, address, local_part, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id) DO UPDATE SET id = excluded.id, address = excluded.address,
			local_part = excluded.local_part, created_at = excluded.created_at`,
		addr.ID, workspaceID, addr.Address, addr.LocalPart, addr.CreatedAt)
	if err != nil {
		return Address{}, fmt.Errorf("cloud address %s: %w", addr.ID, err)
	}
	return addr, nil
}

// Addresses lists the daemon's hosted address rows.
func (s *Service) Addresses(ctx context.Context) ([]Address, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id, workspace_id, address, local_part, created_at FROM cloud_addresses ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("cloud addresses: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Address{}
	for rows.Next() {
		var a Address
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.Address, &a.LocalPart, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("cloud addresses: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAddress revokes one hosted address and drops its row.
func (s *Service) DeleteAddress(ctx context.Context, id string) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	if err := c.DeleteAddress(ctx, id); err != nil {
		return err
	}
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM cloud_addresses WHERE id = ?`, id); err != nil {
		return fmt.Errorf("cloud address %s: %w", id, err)
	}
	return nil
}

// MintHook mints one hosted webhook. The row belongs to the webhooks
// package; this is the signed call only.
func (s *Service) MintHook(ctx context.Context, req MintHookRequest) (Hook, error) {
	c, err := s.client()
	if err != nil {
		return Hook{}, err
	}
	return c.MintHook(ctx, req)
}

// DeleteHook revokes one hosted webhook (signed call only).
func (s *Service) DeleteHook(ctx context.Context, id string) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	return c.DeleteHook(ctx, id)
}

// Send posts one message through the hosted mail path.
func (s *Service) Send(ctx context.Context, req email.SendRequest) (email.SendResult, error) {
	c, err := s.client()
	if err != nil {
		return email.SendResult{}, err
	}
	return c.Send(ctx, req)
}

// Attachment reads one attachment (key = "<messageID>/<attachmentID>").
func (s *Service) Attachment(ctx context.Context, key string) ([]byte, string, error) {
	c, err := s.client()
	if err != nil {
		return nil, "", err
	}
	return c.Attachment(ctx, key)
}

// client returns a signed client for the linked device.
func (s *Service) client() (*Client, error) {
	s.ensure()
	s.mu.Lock()
	row := s.link
	s.mu.Unlock()
	if row == nil {
		return nil, ErrNotLinked
	}
	id, err := LoadIdentity(row.deviceID)
	if err != nil {
		return nil, err
	}
	return NewClient(s.endpoints(), id), nil
}

func (s *Service) endpoints() *endpoints {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endpointsLocked()
}

// endpointsLocked returns the shared address list: the configured URLs, or
// the URL saved at link time before config loads. It keeps the instance
// while the list stays the same, so a failover survives a config reload.
// It returns nil when no URL is known. Callers hold s.mu.
func (s *Service) endpointsLocked() *endpoints {
	urls := s.cfg.APIURLs
	if len(urls) == 0 && s.link != nil {
		urls = []string{s.link.apiURL}
	}
	if len(urls) == 0 {
		return nil
	}
	if s.ep == nil || !slices.Equal(s.ep.urls, urls) {
		s.ep = newEndpoints(urls)
		s.ep.onSwitch = s.endpointSwitched
	}
	return s.ep
}

// endpointSwitched logs and publishes one address change. The bus frame
// carries hosts only, so the UI and the assistant can show the change.
func (s *Service) endpointSwitched(from, to string) {
	s.log.Warn("cloud.endpoint_switched", "from", hostOf(from), "to", hostOf(to))
	s.mu.Lock()
	entityID := "cloud"
	if s.link != nil {
		entityID = s.link.deviceID
	}
	s.mu.Unlock()
	if _, err := s.st.Append(context.Background(), store.BusEvent{
		Topic: "cloud.endpoint_switched", Entity: "cloud", EntityID: entityID, Action: "endpoint_switched",
		Attrs: map[string]string{"from": hostOf(from), "to": hostOf(to)},
	}); err != nil {
		s.log.Warn("bus append", "topic", "cloud.endpoint_switched", "err", err)
	}
}

// probeLoop moves back to the primary address when it answers again.
// Stop condition: ctx is the loop's process-lifetime context, which Stop
// cancels.
func (s *Service) probeLoop(ctx context.Context) {
	t := time.NewTicker(probeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ep := s.endpoints(); ep != nil {
				ep.ProbePrimary(ctx)
			}
		}
	}
}

// applyEntitlement writes plan and features to the cloud_link row,
// refreshes refreshed_at, and publishes cloud.entitlement. hello_ok
// overwrites the row on every connect, so a hand-edited row never
// survives a reconnect.
func (s *Service) applyEntitlement(ctx context.Context, plan string, features []string) {
	if features == nil {
		features = []string{}
	}
	raw, err := json.Marshal(features)
	if err != nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	s.ensure()
	s.mu.Lock()
	row := s.link
	s.mu.Unlock()
	entityID := "cloud"
	if row != nil {
		entityID = row.deviceID
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE cloud_link SET plan = ?, features = ?, refreshed_at = ?`,
			plan, string(raw), now)
		return err
	}, store.BusEvent{
		Topic: "cloud.entitlement", Entity: "cloud", EntityID: entityID, Action: "entitlement",
		Attrs: map[string]any{"plan": plan, "features": features},
	})
	if err != nil {
		s.log.Warn("bus append", "topic", "cloud.entitlement", "err", err)
	}
	s.mu.Lock()
	if s.link != nil {
		s.link.plan, s.link.features, s.link.refreshedAt = plan, features, now
	}
	s.mu.Unlock()
}

// saveLink writes the row and the cloud.linked frame in one transaction.
func (s *Service) saveLink(ctx context.Context, row *linkRow) error {
	raw, err := json.Marshal(row.features)
	if err != nil {
		return err
	}
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO cloud_link
			(id, device_id, device_name, api_url, account_email, plan, features, linked_at, refreshed_at)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.deviceID, row.deviceName, row.apiURL, row.accountEmail, row.plan,
			string(raw), row.linkedAt, row.refreshedAt)
		return err
	}, store.BusEvent{
		Topic: "cloud.linked", Entity: "cloud", EntityID: row.deviceID, Action: "linked",
		Attrs: map[string]string{"device_name": row.deviceName},
	})
	if err != nil {
		return fmt.Errorf("cloud link %s: %w", row.deviceID, err)
	}
	return nil
}

// publish appends one cloud.* frame that has no entity write of its own.
// EntityID is the device id, or "cloud" when no device exists.
func (s *Service) publish(ctx context.Context, action string) {
	s.mu.Lock()
	row := s.link
	s.mu.Unlock()
	entityID := "cloud"
	if row != nil {
		entityID = row.deviceID
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "cloud." + action, Entity: "cloud", EntityID: entityID, Action: action,
	}); err != nil {
		s.log.Warn("bus append", "topic", "cloud."+action, "err", err)
	}
}

// fireOnChange runs the registered listeners after a link-state change.
func (s *Service) fireOnChange(ctx context.Context) {
	s.mu.Lock()
	fns := append([]func(context.Context){}, s.onChange...)
	s.mu.Unlock()
	for _, fn := range fns {
		fn(ctx)
	}
}

// ensure loads the single cloud_link row into memory once. Status and
// Entitlement are I/O-free after this: every write path rewrites both the
// row and the cache.
func (s *Service) ensure() {
	s.mu.Lock()
	loaded := s.loaded
	s.mu.Unlock()
	if loaded {
		return
	}
	row := s.readLink()
	s.mu.Lock()
	if !s.loaded {
		s.link = row
		s.loaded = true
	}
	s.mu.Unlock()
}

// readLink scans the single cloud_link row, or nil when unlinked. It is
// one local SELECT (the shape store.GetFlag uses) and needs no context.
func (s *Service) readLink() *linkRow {
	var l linkRow
	var features string
	err := s.st.DB().QueryRow(`SELECT device_id, device_name, api_url, account_email, plan,
		features, linked_at, refreshed_at FROM cloud_link WHERE id = 1`).
		Scan(&l.deviceID, &l.deviceName, &l.apiURL, &l.accountEmail, &l.plan,
			&features, &l.linkedAt, &l.refreshedAt)
	if err != nil {
		return nil
	}
	_ = json.Unmarshal([]byte(features), &l.features) // "[]" at link time
	if l.features == nil {
		l.features = []string{}
	}
	return &l
}

// ensureRunLocked creates the loop's process-lifetime context. Callers
// hold s.mu.
func (s *Service) ensureRunLocked() {
	if s.life == nil {
		s.life, s.cancel = context.WithCancel(context.Background())
		go s.probeLoop(s.life) // stop: Stop cancels s.life
	}
}

// launchLocked starts the socket loop for the given device keypair when
// enabled. Callers hold s.mu and have the row loaded.
func (s *Service) launchLocked(id Identity) {
	s.ensureRunLocked()
	if s.sock != nil || s.link == nil || !s.cfg.Enabled {
		return
	}
	sk := &socket{svc: s, id: id, downSince: time.Now(), mcpSlots: make(chan struct{}, mcpSlotCount), sids: map[string]struct{}{}}
	s.sock = sk
	s.done = make(chan struct{})
	// Stop condition: s.life is cancelled by Stop (directly or via Start).
	go func() {
		defer close(s.done)
		sk.run(s.life)
	}()
}

// mobile reports whether the socket may carry phone sessions.
func (s *Service) mobile() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Mobile && s.relay != nil
}

func (s *Service) writeFrame(v any) error {
	s.mu.Lock()
	sk := s.sock
	s.mu.Unlock()
	if sk == nil {
		return errors.New("cloudlink: socket is not connected")
	}
	return sk.writeFrame(v)
}

// capabilities returns the summary and its JSON, or nil when there is no
// source or the build fails.
func (s *Service) capabilities(ctx context.Context) (*Capabilities, []byte) {
	s.mu.Lock()
	src := s.caps
	s.mu.Unlock()
	if src == nil {
		return nil, nil
	}
	c, err := src.CloudCapabilities(ctx)
	if err != nil {
		s.log.Warn("cloudlink.capabilities", "err", err)
		return nil, nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, nil
	}
	return &c, raw
}
