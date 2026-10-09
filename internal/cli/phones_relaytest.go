//go:build relaytest

package cli

import (
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/relay"
	"github.com/urfave/cli/v3"
)

func phoneFlags() []cli.Flag {
	return []cli.Flag{&cli.StringFlag{Name: "relay-loopback", Usage: "emulate the cloud relay on this address (test build only)"}}
}

// phoneCloud starts the loopback relay when --relay-loopback is set. The
// listener lives until the process exits.
func phoneCloud(c *cli.Command) (daemon.PhoneCloud, error) {
	addr := c.String("relay-loopback")
	if addr == "" {
		return nil, nil
	}
	return relay.NewLoopback(addr)
}
