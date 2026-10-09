package daemon

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/mobile"
	"github.com/trebi-ai/trebi/internal/relay"
)

// PhoneCloud is the cloud side of paired phones: the cloud link in a
// release build, or the relaytest loopback in a test build.
type PhoneCloud interface {
	relay.Cloud
	relay.Gate
	mobile.Cloud
	BindHub(h *relay.Hub)
}

// StartPhones builds the phone service and the relay hub (plan B10). root
// is the handler of the loopback listener. A nil pc uses the cloud link.
// Call it before Start, so that the first hello names the mobile cap.
func (c *ControlPlane) StartPhones(root http.Handler, pc PhoneCloud) error {
	if pc == nil {
		pc = cloudPhones{cl: c.Cloud}
	}
	c.Mobile = mobile.New(c.Store, pc)
	key, err := c.Mobile.StaticKey()
	if err != nil {
		return fmt.Errorf("phones: %w", err)
	}
	c.Relay = relay.NewHub(key, c.Mobile, pc, pc, c.Mobile, root, c.Opts.Version)
	c.Mobile.WithSessions(c.Relay)
	pc.BindHub(c.Relay)
	return nil
}

// cloudPhones adapts the cloud link to PhoneCloud.
type cloudPhones struct{ cl *cloudlink.Service }

func (p cloudPhones) SendRelay(sid string, raw []byte) error { return p.cl.SendRelay(sid, raw) }
func (p cloudPhones) CloseRelay(sid, reason string) error    { return p.cl.CloseRelay(sid, reason) }
func (p cloudPhones) DeviceID() string                       { return p.cl.DeviceID() }
func (p cloudPhones) DeviceName() string                     { return p.cl.DeviceName() }
func (p cloudPhones) BindHub(h *relay.Hub)                   { p.cl.WithRelay(h) }

// MobileAllowed is true when cloud.mobile is on and the plan has remote_access.
func (p cloudPhones) MobileAllowed() bool {
	return p.cl.MobileOn() && p.cl.Entitlement().Has(entitle.RemoteAccess)
}

func (p cloudPhones) PairTarget() mobile.Target {
	ent := p.cl.Entitlement()
	return mobile.Target{
		APIHost:      p.cl.APIHost(),
		DeviceID:     p.cl.DeviceID(),
		Machine:      p.cl.DeviceName(),
		Linked:       ent.Linked,
		RemoteAccess: ent.Has(entitle.RemoteAccess),
		MobileOn:     p.cl.MobileOn(),
	}
}

func (p cloudPhones) MintClaim(ctx context.Context, uses int, ttl time.Duration) (string, time.Time, error) {
	claim, err := p.cl.MintClaim(ctx, uses, ttl)
	if err != nil {
		return "", time.Time{}, err
	}
	exp, _ := time.Parse(time.RFC3339, claim.ExpiresAt) // zero: the local TTL holds
	return claim.Claim, exp, nil
}

func (p cloudPhones) RevokeClient(ctx context.Context, cloudClientID string) error {
	return p.cl.RevokeClient(ctx, cloudClientID)
}
