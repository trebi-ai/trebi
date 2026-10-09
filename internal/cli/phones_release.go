//go:build !relaytest

package cli

import (
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/urfave/cli/v3"
)

// phoneFlags is empty in a release build. The relaytest build adds --relay-loopback.
func phoneFlags() []cli.Flag { return nil }

// phoneCloud is nil in a release build: phones use the cloud link.
func phoneCloud(*cli.Command) (daemon.PhoneCloud, error) { return nil, nil }
