package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/api"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/platform"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tray"
	"github.com/urfave/cli/v2"
)

// errIfRemoteSticky blocks local-only service commands when sticky context is remote.
func errIfRemoteSticky(state *appState) error {
	if state.stickyErr != nil {
		return state.stickyErr
	}
	if state.sticky != nil && state.sticky.IsRemote() {
		return fmt.Errorf("service commands are local-only (sticky workspace is remote %s); trebi workspace use <local-id>", state.sticky.URL)
	}
	return nil
}

func serviceCommand() *cli.Command {
	return &cli.Command{
		Name:  "service",
		Usage: "daemon lifecycle",
		Subcommands: slices.Concat(
			serviceLifecycleSubcommands(),
			serviceDiagnosticsSubcommands(),
			serviceDaemonSubcommands(),
			serviceTokenSubcommands(),
		),
	}
}

// serviceLifecycleSubcommands returns the OS-supervisor commands: install, uninstall, start, stop, restart.
func serviceLifecycleSubcommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "install",
			Usage: "Install launchd LaunchAgent (macOS) for trebid",
			Flags: []cli.Flag{
				&cli.BoolFlag{
					Name:  "no-tray",
					Usage: "Headless agent (no menu-bar tray; KeepAlive always)",
				},
				&cli.StringFlag{
					Name:  "bin",
					Usage: "Absolute path to trebi/trebid (default: resolve TREBI_BIN, sibling trebid, or PATH)",
				},
			},
			Action: runServiceInstall,
		},
		{
			Name:  "uninstall",
			Usage: "Remove launchd LaunchAgent",
			Action: func(c *cli.Context) error {
				if err := errIfRemoteSticky(appStateFor(c)); err != nil {
					return err
				}
				return appStateFor(c).platform.Uninstall()
			},
		},
		{
			Name:  "start",
			Usage: "Start the OS-supervised daemon",
			Action: func(c *cli.Context) error {
				if err := errIfRemoteSticky(appStateFor(c)); err != nil {
					return err
				}
				if err := appStateFor(c).platform.Start(); err != nil {
					return err
				}
				fmt.Println("Service started.")
				return nil
			},
		},
		{
			Name:  "stop",
			Usage: "Stop the OS-supervised daemon",
			Action: func(c *cli.Context) error {
				if err := errIfRemoteSticky(appStateFor(c)); err != nil {
					return err
				}
				if err := appStateFor(c).platform.Stop(); err != nil {
					return err
				}
				fmt.Println("Service stopped.")
				return nil
			},
		},
		{
			Name:  "restart",
			Usage: "Restart the OS-supervised daemon",
			Action: func(c *cli.Context) error {
				if err := appStateFor(c).platform.Restart(); err != nil {
					return err
				}
				fmt.Println("Service restarted.")
				return nil
			},
		},
	}
}

// serviceDiagnosticsSubcommands returns status, logs, and migrate-db.
func serviceDiagnosticsSubcommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:   "status",
			Usage:  "Show OS service status and API health when available",
			Action: runServiceStatus,
		},
		{
			Name:    "logs",
			Aliases: []string{"log"},
			Usage:   "View the daemon log (launchd stdout/stderr → trebid.log)",
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "Follow log output"},
				&cli.IntFlag{Name: "lines", Aliases: []string{"n"}, Value: 50, Usage: "Number of recent lines"},
				&cli.StringFlag{Name: "level", Usage: "Only lines containing this level (debug|info|warn|error)"},
				&cli.StringFlag{Name: "grep", Usage: "Only lines containing this substring (case-insensitive)"},
				&cli.BoolFlag{Name: "json", Usage: "Print raw lines without filtering notes"},
			},
			Action: runServiceLogs,
		},
		{
			Name:   "migrate-db",
			Usage:  "create/migrate trebi.db schema and exit",
			Action: runServiceMigrateDB,
		},
	}
}

// serviceDaemonSubcommands returns daemon, drain, and undrain.
func serviceDaemonSubcommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "daemon",
			Usage: "run trebid (HTTP API + workers; menu-bar tray on macOS unless --no-tray)",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "listen", Value: "127.0.0.1:0", Usage: "HTTP listen address"},
				&cli.BoolFlag{Name: "shadow", Usage: "shadow mode: drain + no integrations/schedules"},
				&cli.BoolFlag{Name: "no-tray", Usage: "disable menu-bar tray (headless)"},
			},
			Action: runDaemonProcess,
		},
		{
			Name:  "drain",
			Usage: "enter drain mode",
			Action: func(c *cli.Context) error {
				client, err := clientFor(c)
				if err != nil {
					return err
				}
				return client.PostJSON("/v1/system/drain", nil, nil)
			},
		},
		{
			Name:  "undrain",
			Usage: "leave drain mode",
			Action: func(c *cli.Context) error {
				client, err := clientFor(c)
				if err != nil {
					return err
				}
				return client.PostJSON("/v1/system/undrain", nil, nil)
			},
		},
		{
			Name:  "shutdown",
			Usage: "ask the daemon to shut down",
			Action: func(c *cli.Context) error {
				client, err := clientFor(c)
				if err != nil {
					return err
				}
				return client.DoHTTP(BuildServiceShutdown(), nil)
			},
		},
	}
}

// serviceTokenSubcommands returns the token command group.
func serviceTokenSubcommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "token",
			Usage: "token management",
			Subcommands: []*cli.Command{
				{
					Name:  "show",
					Usage: "print current API token",
					Action: func(c *cli.Context) error {
						client, err := clientFor(c)
						if err != nil {
							return err
						}
						fmt.Println(client.Token)
						return nil
					},
				},
			},
		},
	}
}

func runServiceInstall(c *cli.Context) error {
	if err := errIfRemoteSticky(appStateFor(c)); err != nil {
		return err
	}
	return appStateFor(c).platform.Install(platform.InstallOpts{
		NoTray: c.Bool("no-tray"),
		Bin:    c.String("bin"),
	})
}

func runServiceStatus(c *cli.Context) error {
	osStatus, err := appStateFor(c).platform.Status()
	if err != nil {
		// Non-darwin: still try API.
		fmt.Fprintf(c.App.ErrWriter, "os: %v\n", err)
	} else {
		fmt.Println(osStatus)
	}
	client, err := clientFor(c)
	if err != nil {
		fmt.Printf("api: unreachable (%v)\n", err)
		return nil
	}
	var body map[string]any
	if err := client.GetJSON("/v1/system/health", &body); err != nil {
		fmt.Printf("api: unreachable (%v)\n", err)
		return nil
	}
	fmt.Printf("api: status=%v version=%v drain=%v\n", body["status"], body["version"], body["drain_state"])
	return nil
}

func runServiceLogs(c *cli.Context) error {
	logFile := home.DaemonLogPath()
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		return fmt.Errorf("log file not found: %s (start the service once)", logFile)
	}
	args := []string{"-n", fmt.Sprintf("%d", c.Int("lines"))}
	if c.Bool("follow") {
		args = append(args, "-f")
	}
	args = append(args, logFile)
	cmd := exec.Command("tail", args...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	streamServiceLog(c.App.Writer, stdout, c.String("level"), c.String("grep"), c.Bool("json"))
	return cmd.Wait()
}

func runServiceMigrateDB(_ *cli.Context) error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	v, err := st.SchemaVersion()
	if err != nil {
		return err
	}
	fmt.Printf("schema_version=%d path=%s\n", v, st.Path())
	return nil
}

// streamServiceLog filters daemon log lines (plain or slog JSON).
func streamServiceLog(w io.Writer, r io.Reader, level, grep string, raw bool) {
	level = strings.ToUpper(strings.TrimSpace(level))
	grep = strings.ToLower(grep)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if level != "" {
			if !lineHasLevel(line, level) {
				continue
			}
		}
		if grep != "" && !strings.Contains(strings.ToLower(line), grep) {
			continue
		}
		_ = raw // raw kept for flag parity; lines are already plain/JSON as written
		fmt.Fprintln(w, line)
	}
}

func lineHasLevel(line, want string) bool {
	// slog JSON: "level":"INFO" or plain text containing INFO
	if strings.Contains(strings.ToUpper(line), `"LEVEL":"`+want+`"`) {
		return true
	}
	if strings.Contains(strings.ToUpper(line), want) {
		return true
	}
	var raw map[string]any
	if json.Unmarshal([]byte(line), &raw) == nil {
		if lv, ok := raw["level"].(string); ok && strings.EqualFold(lv, want) {
			return true
		}
	}
	return false
}

// runDaemonProcess starts the HTTP control plane and, on macOS, the menu-bar tray.
// Tray icon means the service process is alive. Quit from the tray exits 0 and
// stops the whole process (daemon + tray).
func runDaemonProcess(c *cli.Context) error {
	if err := errIfRemoteSticky(appStateFor(c)); err != nil {
		return err
	}
	noTray := c.Bool("no-tray") || runtime.GOOS != "darwin" || !tray.Available
	if noTray {
		if !c.Bool("no-tray") && runtime.GOOS == "darwin" && !tray.Available {
			appStateFor(c).log.Warn("tray unavailable (need cgo on darwin); running headless")
		}
		return runDaemon(context.Background(), c)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		err := runDaemon(ctx, c)
		errCh <- err
		cancel() // daemon died → tear down tray
	}()

	// Menuet owns the main thread (like jobi).
	trayErr := tray.Run(ctx, cancel)
	cancel()

	// Give the daemon a moment to shut down after tray quit / cancel.
	select {
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			return err
		}
	case <-time.After(5 * time.Second):
	}
	return trayErr
}

func runDaemon(parent context.Context, c *cli.Context) error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	// First run seeds config.yaml so there is a real file to edit. Defaults
	// still apply when it is absent; this only removes the "missing" state.
	if created, err := config.Seed(); err != nil {
		appStateFor(c).log.Warn("config seed", "error", err)
	} else if created {
		fmt.Fprintf(os.Stderr, "wrote default config → %s\n", config.ConfigPath())
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	drainFlag, _, _ := st.GetFlag("drain")
	shadow := c.Bool("shadow")
	token := daemonToken()
	drain := drainFlag == "1" || shadow
	cp := daemon.New(st, daemon.Options{Shadow: shadow, Drain: drain})
	srv := api.New(st, api.Options{
		Token:  token,
		Shadow: shadow,
		Drain:  drain,
	})
	// Share control-plane services so API and workers see the same state.
	srv.BindControlPlane(cp.Runs, cp.Workspaces, cp.Policy, cp.Integrations, cp.Schedule, cp.Alerts)

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	cp.Start(ctx)

	addr, err := srv.ListenAndServe(c.String("listen"))
	if err != nil {
		cp.Stop()
		return err
	}
	if err := srv.WriteAPIJSON(addr); err != nil {
		cp.Stop()
		return err
	}
	appStateFor(c).appendBusEvent(st, "system.started")
	fmt.Fprintf(os.Stderr, "trebid listening on http://%s (home=%s shadow=%v tray=%v)\n",
		addr, home.Base(), shadow, !c.Bool("no-tray") && tray.Available && runtime.GOOS == "darwin")
	fmt.Fprintf(os.Stderr, "api.json → %s\n", home.APIJSONPath())

	_ = os.WriteFile(filepath.Join(home.Base(), "trebid.pid"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-ch:
	case <-ctx.Done():
	}
	appStateFor(c).appendBusEvent(st, "system.stopping")
	cp.Stop()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	return srv.Shutdown(shutCtx)
}

// daemonToken reuses the token from api.json when present, else mints one.
func daemonToken() string {
	token := ulid.Make().String()
	if data, err := os.ReadFile(home.APIJSONPath()); err == nil {
		var doc struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(data, &doc) == nil && doc.Token != "" {
			token = doc.Token
		}
	}
	return token
}

func (s *appState) appendBusEvent(st *store.Store, topic string) {
	if _, err := st.Append(context.Background(), store.BusEvent{Topic: topic}); err != nil {
		s.log.Warn("bus append", "error", err)
	}
}
