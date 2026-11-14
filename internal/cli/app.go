// Package cli is the trebi command surface (API client + local service commands).
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/ext"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/platform"
	"github.com/trebi-ai/trebi/internal/tray"
	"github.com/urfave/cli/v3"
)

const appStateKey = "trebi.cli.state"

// Options is the process wiring a wrapping main can inject.
type Options struct {
	Ext ext.Set
}

// NewApp builds the root CLI.
func NewApp(opts Options) *cli.Command {
	app := &cli.Command{
		Name:    "trebi",
		Usage:   "Trebi — LLM task runner (daemon + CLI)",
		Version: versionString(opts.Ext),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "workspace",
				Aliases: []string{"w"},
				Sources: cli.EnvVars("TREBI_WORKSPACE"),
				Usage:   "workspace id/name (sticky context if omitted; remote links route the API)",
			},
		},
		Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
			if c.Root().Metadata == nil {
				c.Root().Metadata = map[string]any{}
			}
			c.Root().Metadata[appStateKey] = newAppState(c.String("workspace"), opts.Ext)
			return ctx, nil
		},
		Action: runRoot,
		Commands: []*cli.Command{
			serviceCommand(),
			statusCommand(),
			workspaceCommand(),
			jobCommand(),
			runsCommand(),
			doctorCommand(),
			guideCommand(),
			busCommand(),
			configCommand(),
			assistantCommand(),
			agentCommand(),
			chainCommand(),
			artifactCommand(),
			decisionsCommand(),
			proposalsCommand(),
			requestCommand(),
			connectorCommand(),
			harnessCommand(),
			pauseCommand(),
			skillCommand(),
			secretCommand(),
			inboxCommand(),
			outboxCommand(),
			notifyCommand(),
			eventsCommand(),
			fileCommand(),
			memoryCommand(),
			conversationCommand(),
			alertsCommand(),
			agentHookCommand(),
			mcpCommand(),
			queueCommand(),
			rulesCommand(),
			stateCommand(),
			emailCommand(),
			cloudCommand(),
			setupCommand(),
			hooksCommand(),
			machineCommand(),
			visorCommand(),
			mobileCommand(),
		},
	}
	sortCommands(app.Commands)
	return app
}

func versionString(set ext.Set) string {
	v := api.Version
	if set.Edition != "" {
		return v + " (" + set.Edition + ")"
	}
	return v
}

// sortCommands orders help subcommands alphabetically at every nesting level.
func sortCommands(cmds []*cli.Command) {
	slices.SortFunc(cmds, func(a, b *cli.Command) int {
		an, bn := "", ""
		if a != nil {
			an = a.Name
		}
		if b != nil {
			bn = b.Name
		}
		return strings.Compare(an, bn)
	})
	for _, c := range cmds {
		if c != nil && len(c.Commands) > 0 {
			sortCommands(c.Commands)
		}
	}
}

// appState is the immutable startup context shared by all command actions.
type appState struct {
	workspace string
	sticky    *StickyContext
	stickyErr error
	linked    []LinkedWorkspace
	linkedErr error

	client         *APIClient
	clientErr      error
	localClient    *APIClient
	localClientErr error
	config         *config.Config
	configErr      error
	platform       *platform.Service
	log            *slog.Logger
	ext            ext.Set
}

func newAppState(workspaceHint string, extSet ext.Set) *appState {
	workspaceHint = strings.TrimSpace(workspaceHint)
	sticky, stickyErr := LoadStickyContext()
	linked, linkedErr := LoadLinkedWorkspaces()
	state := &appState{
		workspace: workspaceHint,
		sticky:    sticky,
		stickyErr: stickyErr,
		linked:    linked,
		linkedErr: linkedErr,
		platform:  platform.NewService(""),
		log:       slog.Default().With("source", "cli"),
		ext:       extSet,
	}
	if stickyErr != nil {
		state.clientErr = stickyErr
	} else {
		state.client, state.clientErr = loadAPIClientWithContext(
			workspaceHint, sticky, linked, os.Getenv("TREBI_URL"),
		)
	}
	url := os.Getenv("TREBI_URL")
	if url != "" {
		state.localClient = &APIClient{
			base:       strings.TrimRight(url, "/"),
			httpClient: &http.Client{Timeout: 30 * time.Second},
		}
	} else {
		state.localClient, state.localClientErr = apiJSONClient()
	}
	state.config, state.configErr = config.Load()
	state.workspace = effectiveWorkspaceFor(workspaceHint, sticky, linked)
	return state
}

func appStateFor(c *cli.Command) *appState {
	if c != nil {
		if state, ok := c.Root().Metadata[appStateKey].(*appState); ok && state != nil {
			return state
		}
		return newAppState(c.String("workspace"), ext.Set{})
	}
	return newAppState("", ext.Set{})
}

func clientFor(c *cli.Command) (*APIClient, error) {
	state := appStateFor(c)
	if state.clientErr != nil {
		return nil, state.clientErr
	}
	if state.client == nil {
		return nil, fmt.Errorf("API client is not configured")
	}
	return state.client, nil
}

func localClientFor(c *cli.Command) (*APIClient, error) {
	state := appStateFor(c)
	if state.localClientErr != nil {
		return nil, state.localClientErr
	}
	if state.localClient == nil {
		return nil, fmt.Errorf("local API client is not configured")
	}
	return state.localClient, nil
}

func configFor(c *cli.Command) (*config.Config, error) {
	state := appStateFor(c)
	if state.configErr != nil {
		return nil, state.configErr
	}
	if state.config == nil {
		return nil, fmt.Errorf("config is not loaded")
	}
	return state.config, nil
}

func workspaceFor(c *cli.Command) string {
	return resolveWorkspaceHint(c, appStateFor(c).workspace)
}

// workspaceFlag is the command --workspace / -w if set, else the global/sticky workspace.
func workspaceFlag(c *cli.Command) string {
	if w := strings.TrimSpace(c.String("workspace")); w != "" {
		return resolveWorkspaceHint(c, w)
	}
	return workspaceFor(c)
}

// resolveWorkspaceHint maps a name or id to a workspace id via sticky/linked
// context, then the daemon's workspace list.
func resolveWorkspaceHint(c *cli.Command, hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return ""
	}
	state := appStateFor(c)
	resolved := effectiveWorkspaceFor(hint, state.sticky, state.linked)
	client, err := clientFor(c)
	if err != nil {
		return resolved
	}
	if id := client.WorkspaceIDFor(resolved); id != "" {
		return id
	}
	return resolved
}

func stickyFor(c *cli.Command) (*StickyContext, error) {
	state := appStateFor(c)
	return state.sticky, state.stickyErr
}

func linkedFor(c *cli.Command) ([]LinkedWorkspace, error) {
	state := appStateFor(c)
	return state.linked, state.linkedErr
}

func loadAPIClientWithContext(hint string, sticky *StickyContext, linked []LinkedWorkspace, url string) (*APIClient, error) {
	if link, found := findLinked(linked, hint); found && hint != "" {
		return remoteClient(link.URL), nil
	}
	if sticky != nil && sticky.IsRemote() {
		if hint == "" || hint == sticky.WorkspaceID || equalFoldName(hint, sticky.Name) {
			return remoteClient(sticky.URL), nil
		}
		if link, found := findLinked(linked, hint); found {
			return remoteClient(link.URL), nil
		}
	}
	if url != "" {
		return &APIClient{
			base:       strings.TrimRight(url, "/"),
			httpClient: &http.Client{Timeout: 30 * time.Second},
		}, nil
	}
	return apiJSONClient()
}

func effectiveWorkspaceFor(hint string, sticky *StickyContext, linked []LinkedWorkspace) string {
	hint = strings.TrimSpace(hint)
	if hint != "" {
		if link, ok := findLinked(linked, hint); ok {
			return link.ID
		}
		if sticky != nil && equalFoldName(hint, sticky.Name) {
			return sticky.WorkspaceID
		}
		return hint
	}
	if sticky != nil {
		return sticky.WorkspaceID
	}
	return ""
}

func findLinked(all []LinkedWorkspace, idOrName string) (LinkedWorkspace, bool) {
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		return LinkedWorkspace{}, false
	}
	lower := strings.ToLower(idOrName)
	for _, w := range all {
		if w.ID == idOrName {
			return w, true
		}
		if w.Name != "" && strings.ToLower(w.Name) == lower {
			return w, true
		}
	}
	return LinkedWorkspace{}, false
}

// parseKeyValues turns repeated KEY=VALUE flags into a map.
func parseKeyValues(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("%q: want KEY=VALUE", p)
		}
		out[k] = v
	}
	return out, nil
}

// parseInputs turns repeated --input KEY=VALUE flags into job input values.
// The daemon converts each text to the type of its input.
func parseInputs(c *cli.Command) (map[string]any, error) {
	kv, err := parseKeyValues(c.StringSlice("input"))
	if kv == nil || err != nil {
		return nil, err
	}
	out := make(map[string]any, len(kv))
	for k, v := range kv {
		out[k] = v
	}
	return out, nil
}

// forceFlag is the confirm gate flag every destructive command carries.
func forceFlag(usage string) cli.Flag {
	return &cli.BoolFlag{Name: "force", Aliases: []string{"f"}, Usage: usage}
}

// ifMatchFlag guards a document write against a concurrent edit.
func ifMatchFlag() cli.Flag {
	return &cli.StringFlag{Name: "if-match", Usage: "write only when the current hash matches"}
}

// limitFlag caps the rows a list command asks for.
func limitFlag() cli.Flag {
	return &cli.IntFlag{Name: "limit", Usage: "maximum rows to return"}
}

// setStr returns a pointer to a string flag only when it was given, so a
// patch can tell "leave alone" from "clear to empty".
func setStr(c *cli.Command, name string) *string {
	if !c.IsSet(name) {
		return nil
	}
	v := c.String(name)
	return &v
}

// confirmed reports whether --force was given, and otherwise names what the
// call destroys. The API refuses the call without confirm=1.
func confirmed(c *cli.Command, what string) (bool, error) {
	if c.Bool("force") {
		return true, nil
	}
	return false, fmt.Errorf("this %s. Confirm with the user first, then re-run with --force", what)
}

// encodeJSON writes one payload as indented JSON.
func encodeJSON(c *cli.Command, body any) error {
	enc := json.NewEncoder(outWriter(c))
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

func outWriter(c *cli.Command) io.Writer {
	if c != nil {
		if w := c.Root().Writer; w != nil {
			return w
		}
	}
	return os.Stdout
}

// stdinPiped reports whether stdin carries input rather than a terminal.
func stdinPiped() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice == 0
}

func readFileOrStdin(path string) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// runRoot runs when no command is given. Opened as Trebi.app, it installs
// the service when needed and starts it with the tray. After a new install,
// the window shows the start page and then the steps. It opens the window
// when the tray already runs. Otherwise it shows help.
func runRoot(ctx context.Context, c *cli.Command) error {
	if c.NArg() > 0 {
		return fmt.Errorf("unknown command %q; run: trebi --help", c.Args().First())
	}
	exe, err := os.Executable()
	if err != nil || platform.AppBundle(exe) == "" {
		return cli.ShowAppHelp(c)
	}
	state := appStateFor(c)
	return appStart{exe: exe, svc: state.platform, shell: tray.NewShell(exe), url: tray.DaemonURL, log: state.log}.run()
}

type appService interface {
	Installed() bool
	TrayRunning() bool
	Install(platform.InstallOpts) error
	RefreshPath() (bool, error)
	Start() error
}

type appShell interface {
	Available() bool
	Open(base, path string) error
	Progress(tray.StartStep) error
}

// appStart is the start of Trebi.app: install, start, and show the window.
type appStart struct {
	exe   string
	svc   appService
	shell appShell
	url   func() string // the daemon URL
	log   *slog.Logger
}

func (a appStart) run() error {
	fresh := !a.svc.Installed()
	running := a.svc.TrayRunning()
	if !fresh {
		if _, err := a.svc.RefreshPath(); err != nil {
			a.log.Warn("app start: refresh the service path failed", "err", err)
		}
		if err := a.svc.Start(); err != nil {
			return err
		}
		if !running || !a.shell.Available() {
			return nil
		}
		return a.shell.Open(a.url(), "/")
	}
	// The start page opens first and names each step. The window is
	// optional, so a shell error never stops the install.
	show := a.shell.Available()
	if show {
		a.warn(a.shell.Open(a.url(), "/welcome"))
		a.warn(a.shell.Progress(tray.StepInstall))
	}
	if err := a.svc.Install(platform.InstallOpts{Bin: a.exe}); err != nil {
		return err
	}
	if show {
		a.warn(a.shell.Progress(tray.StepEngine))
	}
	if err := a.svc.Start(); err != nil {
		return err
	}
	if show {
		a.warn(a.shell.Progress(tray.StepSetup))
	}
	return nil
}

func (a appStart) warn(err error) {
	if err != nil {
		a.log.Warn("app start: window step failed", "err", err)
	}
}
