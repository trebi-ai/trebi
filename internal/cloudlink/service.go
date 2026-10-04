package cloudlink

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/store"
)

// Settings is the resolved cloud block handed to the service at wiring
// time. The config package owns parsing; this is what the service acts on.
type Settings struct {
	APIURL    string // from config cloud.api_url
	Enabled   bool   // config cloud.enabled; a linked device with Enabled false stays offline
	RemoteMCP bool   // config cloud.remote_mcp
}

// MailSink is the email channel. email.Service implements it.
type MailSink interface {
	Deliver(ctx context.Context, msg email.IngressMessage) error
}

// WebhookSink turns hook frames into events. webhooks.Service implements it.
type WebhookSink interface {
	Deliver(ctx context.Context, f WebhookIngress) error
}

// RelaySink receives opaque relay bytes, undecoded beyond base64.
type RelaySink interface {
	DeliverRelay(ctx context.Context, raw []byte) error
}

// McpSink serves one MCP request that the cloud forwards. api.Server implements it.
type McpSink interface {
	ServeRemoteMCP(ctx context.Context, req McpRequest) McpResponse
}

// Status is the cloud-link fact sheet the API serves verbatim.
type Status struct {
	Linked       bool     `json:"linked"`
	Status       string   `json:"status"` // not_linked | linked | connected | degraded | unauthorized
	AccountEmail string   `json:"account_email"`
	DeviceName   string   `json:"device_name"`
	Plan         string   `json:"plan"`
	Features     []string `json:"features"`
	Connected    bool     `json:"connected"`
	RefreshedAt  string   `json:"refreshed_at"` // RFC3339, "" when never refreshed
	RemoteMCP    bool     `json:"remote_mcp"`   // config cloud.remote_mcp
	McpURL       string   `json:"mcp_url"`      // from hello_ok, "" before the first connect
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

	onChange []func(ctx context.Context)

	mu     sync.Mutex
	link   *linkRow
	loaded bool
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
// A RemoteMCP change drops the socket, so the redial sends new caps.
func (s *Service) Configure(cfg Settings) {
	s.mu.Lock()
	changed := s.cfg.RemoteMCP != cfg.RemoteMCP
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

// WithRelay wires the opaque relay sink.
func (s *Service) WithRelay(r RelaySink) *Service {
	s.relay = r
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
		return Status{Status: statusNotLinked, Features: []string{}, RemoteMCP: s.cfg.RemoteMCP}
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
	base := s.cfg.APIURL
	s.mu.Unlock()
	if base == "" {
		return Status{}, errors.New("cloud api url is not configured")
	}

	id, err := MintIdentity()
	if err != nil {
		return Status{}, err
	}
	deviceID, err := NewClient(base, id).Register(ctx, enrollToken, name)
	if err != nil {
		_ = DeleteIdentity() // no half-link: drop the key minted above
		return Status{}, err
	}
	id = id.ForDevice(deviceID)
	now := time.Now().UTC().Format(time.RFC3339)
	row := &linkRow{
		deviceID:    deviceID,
		deviceName:  name,
		apiURL:      base,
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
func (s *Service) MintHook(ctx context.Context, label string) (Hook, error) {
	c, err := s.client()
	if err != nil {
		return Hook{}, err
	}
	return c.MintHook(ctx, label)
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
	return NewClient(s.apiBase(row), id), nil
}

// apiBase is the configured API URL, falling back to the URL saved at link
// time so an existing row keeps working before config loads.
func (s *Service) apiBase(row *linkRow) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.APIURL != "" {
		return s.cfg.APIURL
	}
	return row.apiURL
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
	}
}

// launchLocked starts the socket loop for the given device keypair when
// enabled. Callers hold s.mu and have the row loaded.
func (s *Service) launchLocked(id Identity) {
	s.ensureRunLocked()
	if s.sock != nil || s.link == nil || !s.cfg.Enabled {
		return
	}
	sk := &socket{svc: s, base: s.apiBaseLocked(), id: id, mcpSlots: make(chan struct{}, mcpSlotCount)}
	s.sock = sk
	s.done = make(chan struct{})
	// Stop condition: s.life is cancelled by Stop (directly or via Start).
	go func() {
		defer close(s.done)
		sk.run(s.life)
	}()
}

// apiBaseLocked resolves the API URL. Callers hold s.mu.
func (s *Service) apiBaseLocked() string {
	if s.cfg.APIURL != "" {
		return s.cfg.APIURL
	}
	return s.link.apiURL
}
