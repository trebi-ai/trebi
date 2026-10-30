// Cloudlink orchestration (plan 2026-09-22): the hosted mail transport,
// the exclusive transport selection, and the link-state refresh hook. The
// daemon wires the cloudlink service; the service only decides.
package daemon

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/store"
)

// cloudTransport is the hosted mail transport (Part D). It satisfies
// email.Transport and email.HostedMint over the cloudlink service: send
// and fetch are signed calls, and minting moves to the cloud.
type cloudTransport struct{ cl *cloudlink.Service }

var _ email.Transport = cloudTransport{}
var _ email.HostedMint = cloudTransport{}

func (t cloudTransport) Send(ctx context.Context, req email.SendRequest) (email.SendResult, error) {
	return t.cl.Send(ctx, req)
}

func (t cloudTransport) Attachment(ctx context.Context, key string) ([]byte, string, error) {
	return t.cl.Attachment(ctx, key)
}

// Health reports the transport facts. The hosted transport has no spool;
// socket state lives in GET /v1/cloud/status.
func (t cloudTransport) Health(context.Context) (email.Health, error) {
	return email.Health{Version: "hosted"}, nil
}

// MintAddress mints one hosted address. A second mint for the same
// workspace replaces it in the cloud.
func (t cloudTransport) MintAddress(ctx context.Context, workspaceID, handle string) (string, error) {
	a, err := t.cl.MintAddress(ctx, workspaceID, handle)
	if err != nil {
		return "", err
	}
	return a.Address, nil
}

// emailMode names the one mail transport the channel runs on.
type emailMode int

const (
	emailOff emailMode = iota
	emailBearer
	emailHosted
)

// emailTransportMode picks the single mail transport (Part D): a Worker
// URL wins and keeps today's behaviour with its poll loop; a cloud link
// with hosted_ingress takes the socket; anything else leaves the channel
// off. Both configured reports the conflict and still returns emailBearer,
// so case 1 keeps running while doctor says why.
func emailTransportMode(cfg *config.Config, ent entitle.State) (emailMode, bool) {
	bearer := cfg != nil && cfg.Email.Enabled()
	hosted := cfg != nil && cfg.Cloud.Active() && ent.Linked && ent.Has(entitle.HostedIngress)
	switch {
	case bearer && hosted:
		return emailBearer, true
	case bearer:
		return emailBearer, false
	case hosted:
		return emailHosted, false
	default:
		return emailOff, false
	}
}

// configureCloud maps the cloud config block onto the link service. The
// device id is in the store and the private key is in the secrets
// directory; config holds only the endpoint and the offline switch.
func (c *ControlPlane) configureCloud(cfg *config.Config) {
	if cfg == nil {
		return
	}
	c.Cloud.Configure(cloudlink.Settings{
		APIURLs:   cfg.Cloud.APIURLs(),
		Enabled:   cfg.Cloud.Active(),
		RemoteMCP: cfg.Cloud.RemoteMCPOn(),
		Mobile:    cfg.Cloud.MobileOn(),
	})
}

// EmailConflict reports that both mail transports are configured at once.
// Doctor renders it; the bearer path keeps running (Part D).
func (c *ControlPlane) EmailConflict() bool {
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	_, conflict := emailTransportMode(cfg, c.Cloud.Entitlement())
	return conflict
}

// reselectEmail re-picks the mail transport after a link-state change:
// Link, Unlink, and every entitlement write can flip hosted mail without
// a restart.
func (c *ControlPlane) reselectEmail(ctx context.Context) {
	cfg, err := config.Load()
	if err != nil {
		c.log.Warn("cloud reconfigure", "error", err)
		return
	}
	c.configureCloud(cfg)
	c.configureEmail(cfg)
	c.syncEmailAddresses(ctx)
}

// syncMachine copies the link device id and name into the self machine
// row after a link change.
func (c *ControlPlane) syncMachine(ctx context.Context) {
	if _, err := c.Machines.Self(ctx); err != nil {
		c.log.Warn("machine.self", "err", err)
	}
}

// cloudHookTopics change which connections can get a hosted hook.
//
//nolint:gochecknoglobals // static table
var cloudHookTopics = []string{"cloud.linked", "cloud.entitlement", "cloud.unlinked"}

// cloudHooks follows the link events on the bus. A change reconciles the
// connection hooks; an unlink first drops the rows. It ends with ctx.
func (c *ControlPlane) cloudHooks(ctx context.Context) {
	after, err := c.Store.LatestSeq(ctx)
	if err != nil {
		c.log.Warn("cloud hooks", "err", err)
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		events, err := c.Store.ListBus(ctx, after, cloudHookTopics, 50)
		if err != nil || len(events) == 0 {
			continue
		}
		after = events[len(events)-1].Seq
		for _, ev := range events {
			if ev.Topic == "cloud.unlinked" {
				if err := c.Webhooks.ReleaseAll(ctx); err != nil {
					c.log.Warn("cloud hooks", "err", err)
				}
			}
		}
		c.Connectors.HooksChanged(ctx)
	}
}

// machineTopics change the cloud roster or the workspace index.
//
//nolint:gochecknoglobals // static table
var machineTopics = []string{"cloud.linked", "cloud.unlinked", "workspace.created", "workspace.updated", "workspace.deleted"}

// machineSync keeps the roster rows and the cloud workspace index current.
// It reads the roster at start, on a link change, and each RosterInterval,
// and it sends the index when a workspace changes. It ends with ctx.
func (c *ControlPlane) machineSync(ctx context.Context) {
	after, err := c.Store.LatestSeq(ctx)
	if err != nil {
		c.log.Warn("machine sync", "err", err)
	}
	every := c.Opts.RosterInterval
	if every <= 0 {
		every = 5 * time.Minute
	}
	c.syncRoster(ctx)
	c.publishWorkspaces(ctx)
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	roster := time.NewTicker(every)
	defer roster.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-roster.C:
			c.syncRoster(ctx)
			c.publishWorkspaces(ctx) // also retries a failed send
		case <-poll.C:
			events, err := c.Store.ListBus(ctx, after, machineTopics, 50)
			if err != nil || len(events) == 0 {
				continue
			}
			after = events[len(events)-1].Seq
			link := slices.ContainsFunc(events, func(ev store.StreamEvent) bool { return strings.HasPrefix(ev.Topic, "cloud.") })
			if link {
				c.syncRoster(ctx)
			}
			c.publishWorkspaces(ctx)
		}
	}
}

func (c *ControlPlane) syncRoster(ctx context.Context) {
	if err := c.Machines.SyncRoster(ctx); err != nil {
		c.log.Warn("machine.roster", "err", err)
	}
}

func (c *ControlPlane) publishWorkspaces(ctx context.Context) {
	if err := c.Machines.PublishWorkspaces(ctx); err != nil {
		c.log.Warn("machine.workspace_index", "err", err)
	}
}
