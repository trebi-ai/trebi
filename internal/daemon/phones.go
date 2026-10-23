package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
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
	c.phones = pc
	return nil
}

// directHost pins the direct listener; the relaytest loopback uses
// 127.0.0.1, which a release build never announces.
type directHost interface {
	DirectOptions(port int) relay.DirectOptions
}

// directWorker opens the direct listener for phones on the same network
// while the gate is open and mobile.direct.enabled is on, and closes it
// when either turns off. The port opens on all interfaces only for a
// machine that serves phones. Stop condition: ctx ends.
func (c *ControlPlane) directWorker(ctx context.Context) {
	every := cmp.Or(c.Opts.DirectInterval, 5*time.Second)
	retry := cmp.Or(c.Opts.DirectRetry, time.Minute)
	t := time.NewTicker(every)
	defer t.Stop()
	var (
		cancel  context.CancelFunc
		done    chan struct{}
		retryAt time.Time
	)
	stop := func() {
		if done != nil {
			cancel()
			<-done
			done = nil
		}
	}
	defer stop()
	for {
		opts, on := c.directOptions()
		switch {
		case on && done == nil && !time.Now().Before(retryAt):
			sub, cn := context.WithCancel(ctx)
			cancel, done = cn, make(chan struct{})
			go func(d chan struct{}) { // stop: sub ends
				defer close(d)
				_ = c.Relay.ServeDirect(sub, opts) // logs mobile.direct.bind_failed; doctor reads DirectError
			}(done)
		case !on:
			stop()
		}
		var ended <-chan struct{}
		if done != nil {
			ended = done
		}
		select {
		case <-ctx.Done():
			return
		case <-ended:
			cancel()
			done, retryAt = nil, time.Now().Add(retry)
		case <-t.C:
		}
	}
}

// directOptions reads mobile.direct and the gate.
func (c *ControlPlane) directOptions() (relay.DirectOptions, bool) {
	cfg, err := config.Load()
	if err != nil || !cfg.Mobile.Direct.On() || !c.phones.MobileAllowed() {
		return relay.DirectOptions{}, false
	}
	port := cfg.Mobile.Direct.PortOrDefault()
	if dh, ok := c.phones.(directHost); ok {
		return dh.DirectOptions(port), true
	}
	return relay.DirectOptions{Port: port}, true
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

// cloudLogin brings an OAuth login back through Trebi Cloud. It needs a
// connected link and remote access.
type cloudLogin struct {
	cl   *cloudlink.Service
	conn *connectors.Service
	url  string
}

func (c cloudLogin) LoginReturn() (string, string, error) {
	st := c.cl.Status()
	switch {
	case !st.Linked:
		return "", "", errors.New("this computer is not linked to Trebi Cloud")
	case !c.cl.Entitlement().Has(entitle.RemoteAccess):
		return "", "", errors.New("remote access is not active")
	case !st.Connected:
		return "", "", errors.New("the cloud link is down")
	}
	return c.url, c.cl.DeviceID(), nil
}

// ReturnLogin ends the login of one cloud login_return frame.
func (c cloudLogin) ReturnLogin(ctx context.Context, f cloudlink.LoginReturn) cloudlink.LoginResult {
	res, err := c.conn.FinishLogin(ctx, connectors.LoginReturn{
		State: f.State, Code: f.Code, Iss: f.Iss, Error: f.Error, ErrorDescription: f.ErrorDescription,
	})
	switch {
	case errors.Is(err, connectors.ErrLoginGone):
		return cloudlink.LoginResult{Status: cloudlink.LoginGone}
	case err != nil:
		slog.Default().Warn("connector.login_return", "return", "cloud", "connector", res.ID, "err", err)
		return cloudlink.LoginResult{Status: cloudlink.LoginFailed, Title: res.Title, Message: err.Error()}
	}
	return cloudlink.LoginResult{Status: cloudlink.LoginDone, Title: res.Title}
}
