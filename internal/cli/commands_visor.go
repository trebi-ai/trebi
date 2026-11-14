package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	agentvisor "github.com/trebi-ai/agent-visor"
	visorcli "github.com/trebi-ai/agent-visor/cli"
	"github.com/trebi-ai/agent-visor/service"
	"github.com/urfave/cli/v3"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/platform"
	"github.com/trebi-ai/trebi/internal/visor"
)

// visorLabel is the service label of the visor of the default home.
const visorLabel = "ai.trebi.visor"

// visorCommand mounts the agent-visor commands under trebi, with the visor
// home under the trebi home.
func visorCommand() *cli.Command {
	return visorcli.Command(visorcli.Options{
		Name:  "visor",
		Usage: "keep agent sessions alive on this machine (advanced)",
		Home:  home.VisorDir(),
		Build: api.Version,
		Configure: func(_ context.Context, o *agentvisor.Options) error {
			o.Client, o.ClientVersion = "trebi", api.Version
			visor.Configure(o)
			return nil
		},
	})
}

// errNoVisorService means that the daemon runs the visor in its own process.
var errNoVisorService = errors.New("the visor runs in the daemon process")

// visorService describes the visor service for the binary exe.
func visorService(exe string) (service.Service, error) {
	unitDir, err := service.UserUnitDir(runtime.GOOS)
	if err != nil {
		return service.Service{}, err
	}
	label := visorLabel
	if os.Getenv("TREBI_HOME") != "" {
		sum := sha256.Sum256([]byte(home.Base()))
		label += "." + hex.EncodeToString(sum[:4])
	}
	userHome, _ := os.UserHomeDir()
	return service.Service{
		Label:   label,
		Command: []string{exe, "visor", "--home", home.VisorDir(), "--socket", home.VisorSocket(), "serve"},
		Socket:  home.VisorSocket(),
		UnitDir: unitDir,
		LogPath: filepath.Join(home.LogsDir(), "visor.log"),
		Env:     map[string]string{"PATH": platform.UserPath(userHome, os.Getenv("PATH"))},
	}, nil
}

// ensureVisor dials the visor service, and installs and starts it when it
// does not run. A home other than the default one runs the visor in the
// daemon process, unless TREBI_VISOR=service, so that a test daemon never
// replaces the service of the user. TREBI_VISOR=embedded always does.
func ensureVisor(ctx context.Context) (*agentvisor.Client, error) {
	mode := os.Getenv("TREBI_VISOR")
	if mode == "embedded" || (mode != "service" && os.Getenv("TREBI_HOME") != "") {
		return nil, errNoVisorService
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	svc, err := visorService(exe)
	if err != nil {
		return nil, err
	}
	c, err := service.Ensure(ctx, svc)
	if err != nil {
		return nil, err
	}
	if _, err := c.Hello(ctx, agentvisor.Hello{Client: "trebi", Build: api.Version, Protocol: agentvisor.Protocol, Binary: exe}); err != nil {
		_ = c.Close(ctx) // the hello error is the one to report
		return nil, err
	}
	return c, nil
}
